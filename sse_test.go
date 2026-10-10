package main

// SSE keepalive tests (Phase 6). The failure this defends against is
// invisible from the Go side: an idle server→client GET stream stays silent,
// and the CLIENT times it out — opencode's log shows "SSE stream disconnected:
// TimeoutError" exactly 180 s after every connect. A test therefore listens on
// a real stream and demands that SOMETHING arrives while nothing else happens.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// initSession performs the MCP handshake over raw HTTP and returns the
// session id, so a test can open the standalone SSE stream the way a client
// does — headers on a GET, no SDK in the way.
func initSession(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"keepalive-test","version":"0"},"capabilities":{}}}`
	req, err := http.NewRequest("POST", url, strings.NewReader(initReq))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize gave status %d, session id %q", resp.StatusCode, sid)
	}

	notify, err := http.NewRequest("POST", url, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatal(err)
	}
	notify.Header.Set("Content-Type", "application/json")
	notify.Header.Set("Accept", "application/json, text/event-stream")
	notify.Header.Set("Mcp-Session-Id", sid)
	nresp, err := c.Do(notify)
	if err != nil {
		t.Fatal(err)
	}
	defer nresp.Body.Close()
	if nresp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized gave status %d; want 202", nresp.StatusCode)
	}
	return sid
}

// decodeRPC reads a JSON-RPC reply that may arrive raw (application/json) or
// framed as one SSE event ("data: {...}") — the server picks the framing from
// the Accept header, and both shapes must parse.
func decodeRPC(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "{") {
		var out map[string]any
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			t.Fatalf("JSON-RPC reply is not valid JSON: %v\n%s", err, raw)
		}
		return out
	}
	for _, line := range strings.Split(s, "\n") {
		if data := strings.TrimPrefix(line, "data: "); data != line {
			var out map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &out); err != nil {
				t.Fatalf("SSE-framed reply is not valid JSON: %v\n%s", err, raw)
			}
			return out
		}
	}
	t.Fatalf("no JSON-RPC payload in reply:\n%s", raw)
	return nil
}

// The core of Phase 6: a client that opens the server→client stream and says
// NOTHING must still hear from the server. Each ping is an SSE comment, which
// conformant clients ignore — it exists only to reset their idle timer.
func TestIdleSSEStreamReceivesKeepalivePings(t *testing.T) {
	old := sseKeepaliveInterval
	sseKeepaliveInterval = 100 * time.Millisecond
	defer func() { sseKeepaliveInterval = old }()

	srv := mcp.NewServer(&mcp.Implementation{Name: "keepalive", Version: "test"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ping", Description: "test tool"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "PONG"}}}, nil, nil
		})

	ts := httptest.NewServer(keepaliveHandler(func(*http.Request) *mcp.Server { return srv }))
	defer ts.CloseClientConnections() // Close would wait on a correctly-blocked stream

	c := ts.Client()
	sid := initSession(t, c, ts.URL)

	req, err := http.NewRequest("GET", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("standalone SSE stream gave %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// Read the stream in a goroutine: if no ping ever arrives, Read blocks
	// forever, and a hanging test is less useful than a named failure.
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	gotOK, gotPing := false, false
	deadline := time.After(8 * time.Second)
	for !gotPing {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the stream closed after ok=%v, ping=%v — with no ping to keep it alive", gotOK, gotPing)
			}
			if line == ": ok" {
				gotOK = true
			}
			if line == ": ping" {
				gotPing = true
			}
		case <-deadline:
			t.Fatalf("no keepalive ping on an idle SSE stream within 8s (got ok=%v) — "+
				"a silent stream is timed out by the client: 'SSE stream disconnected: TimeoutError'", gotOK)
		}
	}
	if !gotOK {
		t.Error(`the stream never opened with its ": ok" comment`)
	}

	// The pings are comments: they must not corrupt the stream — a tool call
	// on the very same session, while the stream is still open, must work.
	call, err := http.NewRequest("POST", ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping","arguments":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Accept", "application/json, text/event-stream")
	call.Header.Set("Mcp-Session-Id", sid)
	cresp, err := c.Do(call)
	if err != nil {
		t.Fatal(err)
	}
	defer cresp.Body.Close()
	raw, err := io.ReadAll(cresp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRPC(t, raw)
	if !strings.Contains(fmt.Sprint(out), "PONG") {
		t.Fatalf("tool call on the keepalived session returned: %v", out)
	}
}

// Pings live only as long as the handler holds the stream. A write landing
// after ServeHTTP has returned is a use-after-return of the ResponseWriter —
// Go can panic on it. The tick racing the handler's return must land nowhere.
func TestKeepalivePingsStopWhenTheHandlerReturns(t *testing.T) {
	old := sseKeepaliveInterval
	sseKeepaliveInterval = 50 * time.Millisecond
	defer func() { sseKeepaliveInterval = old }()

	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k := newKeepaliveWriter(w, ctx)
	time.Sleep(250 * time.Millisecond)

	k.stop()
	// Quiet the ticker first: stop, then wait out a tick, so the baseline is
	// counted on a stream that cannot write any more. Reading the recorder
	// under the writer's lock is the happens-before edge against every ping.
	time.Sleep(200 * time.Millisecond)
	k.mu.Lock()
	n := strings.Count(w.Body.String(), ": ping")
	k.mu.Unlock()
	if n < 3 {
		t.Fatalf("expected several pings while the stream is open, got %d", n)
	}

	// Two racing pings, written directly: exactly the tick that fires at the
	// instant ServeHTTP returns. The guard must make them land nowhere.
	k.ping()
	k.ping()
	time.Sleep(200 * time.Millisecond)
	k.mu.Lock()
	got := w.Body.String()
	k.mu.Unlock()
	if strings.Count(got, ": ping") != n {
		t.Fatal("ping writes landed after stop — pings write on a ResponseWriter whose request is over")
	}
	// The stream must contain nothing but pings: no partial events, no stray bytes.
	if rest := strings.ReplaceAll(got, ": ping\n\n", ""); rest != "" {
		t.Errorf("the writer emitted something other than pings: %q", rest)
	}
}

// The proxy backends are the servers opencode actually connects to; their
// handler must serve a real MCP session through the keepalive wrapper.
func TestProxyHandlerServesMCPThroughKeepalive(t *testing.T) {
	e := newTestEngine(t, filepath.Join(t.TempDir(), "never.marker"))
	b := newBackend(e, "vision", InventoryServer{Transport: "stdio", Command: "none.exe", Why: "proxy"})

	ts := httptest.NewServer(b.handler)
	defer ts.CloseClientConnections()

	sid := initSession(t, ts.Client(), ts.URL)
	if sid == "" {
		t.Fatal("no session id from the proxy handler")
	}
	// The cached tool list is what a disconnected proxy advertises; an empty
	// cache means tools/list must answer with an (empty) list, not an error.
	list, err := http.NewRequest("POST", ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	list.Header.Set("Content-Type", "application/json")
	list.Header.Set("Accept", "application/json, text/event-stream")
	list.Header.Set("Mcp-Session-Id", sid)
	resp, err := ts.Client().Do(list)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("tools/list gave status %d on the keepalived proxy session", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out := decodeRPC(t, raw); out == nil {
		t.Fatal("tools/list reply decoded to nothing")
	}
}

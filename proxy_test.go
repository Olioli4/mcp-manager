package main

// The proxy tests: each one names the failure it prevents.
//
// They exist because the proxy is the load-bearing part of the tool and it was
// changed without a test of its own. Every failure below happened (or was about
// to): an idle timer cutting off a call in flight, a session dereferenced after
// it had been closed, a borrow leaked so that a server looked busy forever, and a
// preheating that started the service it was only supposed to reach.
//
// No test seams: the upstream is a REAL MCP server on a real local HTTP
// connection, the Engine is loaded from a REAL manifest, and a service start is
// observed by the file its start command leaves behind.
//
// Why the locking holds (read as an adversary, as asked for):
//
//   - The session is published and borrowed in ONE critical section
//     (ensureUpstream: b.session = sess; borrowLocked()). So there is no moment
//     between "a session exists" and "a call owns it" for the idle timer to slip
//     into. That is what makes the H3 crash unrepresentable, rather than checked
//     for after the fact.
//   - ensureUpstream returns a borrowed session and every caller answers with
//     leave(). borrowLocked increments only by one, leave decrements only when
//     inFlight > 0, so the counter cannot go negative, and it re-arms the timer
//     only when inFlight == 0 AND a session exists — it cannot arm a timer on a
//     session that is already gone.
//   - ensureUpstream arms the idle timer under b.mu; borrowLocked stops it under
//     the same b.mu. The two cannot interleave.
//   - warm() running beside a real forward() is safe because both funnel through
//     ensureUpstream: connect happens under connMu, the session and tool writes
//     under mu, and the server the HTTP layer reads is an atomic.Pointer —
//     rebuildServer publishes a whole server, never a half-built one.
//   - leave() runs as a deferred call, so every path out of forward — success,
//     CallTool error, dropSession — returns the borrow exactly once.
//
// The tests that read b.inFlight do so under b.mu; the stress test reads it from
// other goroutines for the same reason.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startUpstream runs a real MCP server on a local HTTP endpoint and returns its
// URL. It exposes three tools: "ping" answers, "boom" refuses (MCP reports that as
// a failed result), and "die" closes the upstream session mid-call — the only way
// to make CallTool itself fail, which is the path where the proxy must drop a
// session that is no longer usable.
func startUpstream(t *testing.T) (url string, stop func()) {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "test"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "ping", Description: "test tool"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "PONG"}},
			}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "boom", Description: "test tool that fails"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("upstream refused the call")
		})
	// "die" is how a test gets the upstream to disappear MID-CALL, which is the
	// only way to reach the path where CallTool itself fails: a tool handler that
	// returns an error is reported by MCP as a failed result, not as a broken
	// connection.
	mcp.AddTool(s, &mcp.Tool{Name: "die", Description: "closes the upstream session mid-call"},
		func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			if ss, ok := req.GetSession().(*mcp.ServerSession); ok {
				_ = ss.Close()
				time.Sleep(150 * time.Millisecond) // let the closure reach the wire
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "never delivered"}}}, nil, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	ts := httptest.NewServer(h)
	// CloseClientConnections, not Close: the streamable transport holds an open
	// stream for server-to-client messages, so Close would wait on a handler that
	// is correctly, permanently blocked.
	return ts.URL, ts.CloseClientConnections
}

// newTestEngine writes a manifest where service "starter" is STOPPED (nothing
// listens on its port) and its start command leaves MARKER on disk — so a test
// can tell, from the outside, whether something started it.
func newTestEngine(t *testing.T, marker string) *Engine {
	t.Helper()
	manifest := `{
  "settings": {"web_port": 8787, "default_probe_timeout_s": 10, "poll_seconds": 5},
  "services": [{
    "id": "starter",
    "label": "Starter",
    "why": "the marker file proves a start happened",
    "start": {"exe": "cmd.exe", "args": ["/c", "echo x > \"` + filepath.ToSlash(marker) + `\""]},
    "stop": {"exe": "cmd.exe", "args": ["/c", "del /q \"` + filepath.ToSlash(marker) + `\""]},
    "probe": {"port": 1}
  }]
}`
	path := filepath.Join(t.TempDir(), "services.json")
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := LoadEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func inflight(b *backend) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight
}

// The idle timer must not disconnect an upstream while a call is using it.
// Before the fix the timer ran on wall-clock inactivity alone: a slow render
// could be cut off mid-call by its own idle timeout.
//
// It also checks the other half: once the last call is gone, the timer MUST fire.
// A hub that holds every upstream open forever is the mirror of the same bug.
func TestIdleTimerDoesNotFireWhileCallInFlight(t *testing.T) {
	t.Setenv("MCPMGR_IDLE_SECONDS", "1")
	url, stop := startUpstream(t)
	defer stop()

	b := newBackend(newTestEngine(t, filepath.Join(t.TempDir(), "never.marker")),
		"idle-test", InventoryServer{Transport: "http", URL: url})

	sess, err := b.ensureUpstream(context.Background(), false)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// ensureUpstream hands back a BORROWED session.
	if n := inflight(b); n != 1 {
		t.Fatalf("the call that connect returned was not counted in flight: inFlight=%d", n)
	}

	// A second call on the live session takes the fast path, and must be counted
	// too: two clients, one upstream.
	sess2, err := b.ensureUpstream(context.Background(), false)
	if err != nil {
		t.Fatalf("fast path: %v", err)
	}
	if sess2 != sess {
		t.Fatal("the fast path reconnected instead of reusing the live session")
	}
	if n := inflight(b); n != 2 {
		t.Fatalf("two calls in flight, counter says %d", n)
	}

	// Longer than the idle timeout: with the bug, this is where the session dies.
	time.Sleep(1500 * time.Millisecond)
	b.mu.Lock()
	live := b.session
	b.mu.Unlock()
	if live != sess {
		t.Fatalf("the idle timer cut off calls in flight (inFlight=%d)", inflight(b))
	}

	// One borrow back: still one call in flight, so the timer must stay down.
	b.leave()
	time.Sleep(1500 * time.Millisecond)
	b.mu.Lock()
	live = b.session
	b.mu.Unlock()
	if live != sess {
		t.Fatal("the timer was re-armed while a call was still in flight")
	}

	b.leave()

	// With the last call gone, the timer is armed again and MUST fire.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		gone := b.session == nil
		b.mu.Unlock()
		if gone {
			if n := inflight(b); n != 0 {
				t.Fatalf("the idle disconnect left %d call(s) in flight", n)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the idle timer never disconnected the idle upstream")
}

// A call that arrives when there is no session must answer with a readable error
// naming the server. Before the fix it dereferenced a nil session: the tray build
// simply died, with no message.
func TestForwardReturnsReadableErrorWhenSessionGone(t *testing.T) {
	b := newBackend(newTestEngine(t, filepath.Join(t.TempDir(), "never.marker")),
		"gone-test", InventoryServer{Transport: "http", URL: "http://127.0.0.1:1/mcp"})

	res, err := b.forward(context.Background(), "ping", nil)
	if err != nil {
		t.Fatalf("forward must answer as a tool result, not as a transport failure: %v", err)
	}
	if !res.IsError {
		t.Fatal("a call to an unreachable server was reported as success")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("the error is not a text message: %#v", res.Content[0])
	}
	if !strings.Contains(text.Text, "gone-test") {
		t.Errorf("the error does not name the server: %q", text.Text)
	}
	// A refused call must not leave a borrow behind — that is how a server ends up
	// looking permanently busy.
	if n := inflight(b); n != 0 {
		t.Fatalf("a refused call leaked a borrow: inFlight=%d", n)
	}
}

// Every path out of forward owes exactly one leave(): success, CallTool failure,
// and dropSession. A leak here is invisible until the day the server refuses to
// disconnect and the next call is judged "in flight" forever.
func TestForwardNeverLeaksABorrow(t *testing.T) {
	url, stop := startUpstream(t)
	defer stop()

	b := newBackend(newTestEngine(t, filepath.Join(t.TempDir(), "never.marker")),
		"leak-test", InventoryServer{Transport: "http", URL: url})

	for i := 0; i < 40; i++ {
		if res, err := b.forward(context.Background(), "ping", nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		} else if res.IsError {
			t.Fatalf("call %d answered with an error: %q", i, res.Content[0])
		}
	}
	if n := inflight(b); n != 0 {
		t.Fatalf("%d call(s) in flight after 40 completed calls", n)
	}

	// Now a call that dies with the upstream gone: forward drops the session and
	// answers with a readable message. The deferred leave() must still run — this
	// is the path where a leak is easiest to forget.
	//
	// The deadline is not decoration. When the upstream closes the session
	// mid-call, the response for that call simply never arrives: an unbounded
	// CallTool sits in IO wait forever, and the test hangs (measured: 9 minutes,
	// then the suite's own timeout). A wedged stream must be bounded — recorded in
	// AUDIT.md as a known limitation of forward, which inherits its caller's
	// context and adds no timeout of its own.
	dieCtx, cancelDie := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDie()

	res, err := b.forward(dieCtx, "die", nil)
	if err != nil {
		t.Fatalf("the failed call must answer as a tool result: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a call that died with the upstream was reported as success: %q", res.Content[0])
	}
	if n := inflight(b); n != 0 {
		t.Fatalf("the failed call leaked a borrow: inFlight=%d", n)
	}
	b.mu.Lock()
	dropped := b.session == nil
	b.mu.Unlock()
	if !dropped {
		t.Fatal("a failed call left a dead session marked usable")
	}

	// And the next call must work: H1 is "the server stays unreachable forever".
	res, err = b.forward(context.Background(), "ping", nil)
	if err != nil {
		t.Fatalf("recovery call: %v", err)
	}
	if res.IsError {
		t.Fatalf("the proxy did not recover after a failed call: %q", res.Content[0])
	}
	if n := inflight(b); n != 0 {
		t.Fatalf("the recovery call leaked a borrow: inFlight=%d", n)
	}
}

// Preheating borrows on ensureUpstream's terms, so it must hand the borrow back.
// If it did not, a server added from the interface would look busy forever and
// never disconnect — the manager holding every upstream open, for no reason.
func TestWarmHandsTheBorrowBack(t *testing.T) {
	t.Setenv("MCPMGR_IDLE_SECONDS", "1")
	url, stop := startUpstream(t)
	defer stop()

	b := newBackend(newTestEngine(t, filepath.Join(t.TempDir(), "never.marker")),
		"warm-leak-test", InventoryServer{Transport: "http", URL: url})

	b.warm()
	if st := b.state(); !st.Connected {
		t.Fatalf("warm() did not connect: %+v", st)
	}
	if n := inflight(b); n != 0 {
		t.Fatalf("warm() left %d borrow(s) in flight", n)
	}

	// The idle timer must be free to do its work.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		gone := b.session == nil
		b.mu.Unlock()
		if gone {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("warm() left the upstream un-disconnectable")
}

// The stress the plan asks for: several clients on one server, with the idle
// disconnect churning underneath. Nothing may panic (the H3 crash class), every
// answer must be a well-formed tool result, and at the end the proxy must settle:
// no borrow left, session reclaimed, and calls that succeed.
func TestConcurrentCallsSurviveIdleDisconnects(t *testing.T) {
	t.Setenv("MCPMGR_IDLE_SECONDS", "1")
	url, stop := startUpstream(t)
	defer stop()

	b := newBackend(newTestEngine(t, filepath.Join(t.TempDir(), "never.marker")),
		"stress-test", InventoryServer{Transport: "http", URL: url})

	const goroutines, calls = 8, 25
	var mu sync.Mutex
	ok, failed, malformed := 0, 0, 0

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < calls; i++ {
				res, err := b.forward(context.Background(), "ping", nil)
				if err != nil {
					t.Errorf("forward must answer as a tool result: %v", err)
					return
				}
				if n := inflight(b); n < 0 {
					t.Errorf("inFlight went negative: %d", n)
					return
				}
				mu.Lock()
				switch {
				case len(res.Content) == 0:
					malformed++
				case !res.IsError:
					ok++
				default:
					if _, isText := res.Content[0].(*mcp.TextContent); !isText {
						malformed++
					}
					failed++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if ok == 0 {
		t.Fatal("no call ever reached the upstream — this test is not exercising a working proxy")
	}
	if malformed > 0 {
		t.Fatalf("%d answers were malformed (no content, or an error without a message)", malformed)
	}
	if n := inflight(b); n != 0 {
		t.Fatalf("%d borrow(s) left after %d calls", n, goroutines*calls)
	}

	// After the load: the idle timer must reclaim the session.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		n, live := b.inFlight, b.session
		b.mu.Unlock()
		if n == 0 && live == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("after the load, the proxy never settled: in-flight calls or a session left open")
}

// A server added from the interface is preheated so its tools appear. That
// preheating CONNECTS; it must never start the declared service. When this was
// violated, merely launching the manager opened ComfyUI (9 processes) and
// Chrome, with nobody asking for anything.
func TestNewServerWithEmptyCacheIsWarmed(t *testing.T) {
	url, stop := startUpstream(t)
	defer stop()

	marker := filepath.Join(t.TempDir(), "starter-started.marker")
	e := newTestEngine(t, marker)

	b := newBackend(e, "warm-test", InventoryServer{
		Transport: "http", URL: url, NeedsService: "starter"})

	if len(b.tools) != 0 {
		t.Fatal("this test needs a backend with an empty cache")
	}

	b.warm()

	if st := b.state(); !st.Connected || len(st.Tools) == 0 {
		t.Fatalf("warm() did not preheat the new server: %+v", st)
	}

	// Give a start command time to leave its marker. It must not appear.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("warm() started the declared service — only an incoming request may start one")
	}
}

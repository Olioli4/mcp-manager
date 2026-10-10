package main

// The web-side tests: the inventory file is written from HTTP handlers, so it is
// written CONCURRENTLY, and each write is a read-modify-write of the whole
// document. That is where configuration silently disappears.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Clicking two toggles quickly (or the page and an API client writing together)
// used to be a race: both handlers read the file, change one key in memory, and
// write the whole document back, so whichever finished last erased the other's
// change. The user sees a matrix that disagrees with the file.
//
// Here every client is switched on at once. Serialized, that ends as "all
// clients", which this code writes as an ABSENT field (a list only appears when
// it really restricts). A lost update leaves an explicit list behind, so the
// assertion has teeth: with the lock the field is gone, without it a subset
// survives.
func TestInventoryToggleWritesAreSerialised(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")
	doc := `{
  "$comment": {"what": "the format block must survive every write"},
  "servers": {"svc": {"transport": "http", "url": "http://127.0.0.1:9/mcp"}}
}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	const n = 8
	plugins := make([]*Plugin, 0, n)
	for i := 0; i < n; i++ {
		plugins = append(plugins, &Plugin{ID: fmt.Sprintf("client-%d", i)})
	}

	var wg sync.WaitGroup
	for _, p := range plugins {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := setServerClient(path, "svc", id, true, plugins); err != nil {
				t.Errorf("toggle %s on: %v", id, err)
			}
		}(p.ID)
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the inventory is not valid JSON: %v\n%s", err, raw)
	}
	if _, ok := got["$comment"]; !ok {
		t.Fatal("the $comment block did not survive the writes")
	}
	servers, ok := got["servers"].(map[string]any)
	if !ok {
		t.Fatalf("'servers' lost: %v", got["servers"])
	}
	svc, ok := servers["svc"].(map[string]any)
	if !ok {
		t.Fatalf("'svc' lost: %v", servers["svc"])
	}
	if v, present := svc["clients"]; present {
		arr, _ := v.([]any)
		t.Fatalf("all %d clients were switched on, so 'clients' must be absent; it is %v (%d entries)",
			n, v, len(arr))
	}

	// The atomic write leaves nothing behind: a crashed writer must not litter the
	// user's config directory with temp files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left in the config directory: %s", e.Name())
		}
	}
}

// M6: the UI form does not show env/headers (they live in the file, hand-edited
// with secrets-reference values). The old serverToMap copied a known list of
// fields and dropped everything else — so ANY save from the form erased them.
// The pair must survive the round trip through the API, unchanged, and stay
// absent when absent.
func TestServerToMapPreservesEnvHeaders(t *testing.T) {
	s := InventoryServer{
		Transport: "stdio", Command: "npx",
		Env:     map[string]string{"OPENAI_API_KEY": "${OPENAI_API_KEY}"},
		Headers: map[string]string{"Authorization": "Bearer ${MCP_TOKEN}"},
	}
	m := serverToMap(s)
	env, ok := m["env"].(map[string]string)
	if !ok || env["OPENAI_API_KEY"] != "${OPENAI_API_KEY}" {
		t.Fatalf("env lost through the UI save path: %v", m["env"])
	}
	hdr, ok := m["headers"].(map[string]string)
	if !ok || hdr["Authorization"] != "Bearer ${MCP_TOKEN}" {
		t.Fatalf("headers lost through the UI save path: %v", m["headers"])
	}

	m = serverToMap(InventoryServer{Transport: "stdio", Command: "x"})
	if _, has := m["env"]; has {
		t.Error(`"env" written for a server that has none`)
	}
	if _, has := m["headers"]; has {
		t.Error(`"headers" written for a server that has none`)
	}
}

// A broken pair is invisible until the server fails to start weeks later: an
// env key with a space is not a variable name, and a header value with a line
// break SPLITS the HTTP request — everything after the break becomes headers of
// its own (or a body). Refuse both at write time, where the user is standing.
func TestValidateServerSpecRejectsBrokenEnvHeaders(t *testing.T) {
	e := newTestEngine(t, filepath.Join(t.TempDir(), "never.marker"))
	plugins := []*Plugin{{ID: "claude-desktop", Label: "Claude Desktop"}}

	bad := InventoryServer{
		Transport: "stdio", Command: "npx",
		Env: map[string]string{"API KEY": "${X}"},
	}
	if err := validateServerSpec("s", bad, e, plugins); err == nil {
		t.Error(`env key "API KEY" (a space, not a variable name) was accepted`)
	} else if !strings.Contains(err.Error(), "API KEY") {
		t.Errorf("the error must name the offending key, got: %v", err)
	}

	bad = InventoryServer{Transport: "stdio", Command: "npx", Env: map[string]string{"1BAD": "x"}}
	if err := validateServerSpec("s", bad, e, plugins); err == nil {
		t.Error("env key starting with a digit was accepted")
	}

	bad = InventoryServer{
		Transport: "http", URL: "https://x/mcp",
		Headers: map[string]string{"Authorization": "Bearer t\r\nX-Evil: 1"},
	}
	if err := validateServerSpec("s", bad, e, plugins); err == nil {
		t.Fatal("a header value with CRLF (request splitting) was accepted")
	} else if !strings.Contains(err.Error(), "line break") {
		t.Errorf("the error must explain the split, got: %v", err)
	}

	good := InventoryServer{
		Transport: "http", URL: "https://x/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${MCP_TOKEN}"},
		Env:     map[string]string{"OK_KEY_1": "${V}"},
	}
	if err := validateServerSpec("s", good, e, plugins); err != nil {
		t.Errorf("the valid pair was refused: %v", err)
	}
}

package main

// Import tests. Every plugin here is a throwaway literal: no real client
// config is read, written or touched.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The plain case, and the exact inverse of Render's stdio branch: a
// string+args block becomes a command and an argument list.
func TestImportServerStdio(t *testing.T) {
	p := &Plugin{ID: "claude-code", Label: "Claude Code", RootKey: "mcpServers",
		TypeMap: map[string]string{"stdio": "stdio"}, CommandShape: "string_args", HTTPKey: "url"}
	block := map[string]any{
		"type":    "stdio",
		"command": "npx",
		"args":    []any{"-y", "@playwright/mcp@latest"},
	}
	s, ok := importServer(p, "playwright", block)
	if !ok {
		t.Fatal("a stdio block with a command must import")
	}
	if s.Transport != "stdio" {
		t.Errorf("transport = %q; want stdio", s.Transport)
	}
	if s.Command != "npx" {
		t.Errorf("command = %q; want npx", s.Command)
	}
	if want := []string{"-y", "@playwright/mcp@latest"}; !reflect.DeepEqual(s.Args, want) {
		t.Errorf("args = %#v; want %#v", s.Args, want)
	}
	// Round trip: Render must give back what we read.
	back := Render(p, "playwright", s)
	if back["command"] != "npx" {
		t.Errorf("Render after import wrote command=%v; want npx", back["command"])
	}
}

// opencode stores the whole command as ONE array, prefixed with the Windows
// shell shim ["cmd","/c"]. The shim is how opencode launches npx at all — it is
// NOT part of the server, and importing it as such would put "cmd" in the
// inventory's command field and break every later apply.
func TestImportServerStripsCommandPrefix(t *testing.T) {
	p := &Plugin{ID: "opencode", Label: "opencode", RootKey: "mcp",
		TypeMap: map[string]string{"stdio": "local"}, CommandShape: "array",
		HTTPKey: "url", CommandPrefix: []string{"cmd", "/c"}}

	s, ok := importServer(p, "playwright", map[string]any{
		"type":    "local",
		"command": []any{"cmd", "/c", "npx", "-y", "x"},
	})
	if !ok {
		t.Fatal("an array command must import")
	}
	if s.Command != "npx" {
		t.Errorf("command = %q; want npx (the cmd /c shim must be stripped)", s.Command)
	}
	if want := []string{"-y", "x"}; !reflect.DeepEqual(s.Args, want) {
		t.Errorf("args = %#v; want %#v", s.Args, want)
	}

	// An array that does NOT start with the prefix is left alone: a client
	// that spells it differently must not have its command eaten.
	s2, ok := importServer(p, "plain", map[string]any{
		"type":    "local",
		"command": []any{"node", "server.js"},
	})
	if !ok {
		t.Fatal("a non-prefixed array must import")
	}
	if s2.Command != "node" {
		t.Errorf("command = %q; want node (a non-matching prefix must not be stripped)", s2.Command)
	}
	if want := []string{"server.js"}; !reflect.DeepEqual(s2.Args, want) {
		t.Errorf("args = %#v; want %#v", s2.Args, want)
	}
}

// The trap that makes import pointless if it is got wrong: EVERY client with a
// proxy_base holds entries pointing at the manager itself. Importing those
// would create inventory servers that point at ourselves, and a later apply
// would then write "http://127.0.0.1:8787/mcp/manager" — the manager calling
// itself, once, per server.
func TestImportServerSkipsOurOwnProxy(t *testing.T) {
	p := &Plugin{ID: "claude-code", Label: "Claude Code", RootKey: "mcpServers",
		CommandShape: "string_args", HTTPKey: "url", ProxyBase: "http://127.0.0.1:8787/mcp"}

	for _, u := range []string{
		"http://127.0.0.1:8787/mcp",        // the manager itself
		"http://127.0.0.1:8787/mcp/vision", // a server we proxy
		"http://127.0.0.1:8787/mcp/",       // trailing slash
	} {
		if _, ok := importServer(p, "whatever", map[string]any{"type": "http", "url": u}); ok {
			t.Errorf("importServer accepted %q — that is our own proxy, not a foreign server", u)
		}
	}

	// A REAL remote server must still import, even next to ours.
	if _, ok := importServer(p, "remote", map[string]any{"url": "https://api.example.com/mcp"}); !ok {
		t.Error("a genuine remote http server must import")
	}
	// And a URL that merely shares a PREFIX of the proxy base is not ours:
	// ".../mcpEXTRA" is someone else's endpoint.
	if _, ok := importServer(p, "near", map[string]any{"url": "http://127.0.0.1:8787/mcpEXTRA"}); !ok {
		t.Error("a url that only shares a string prefix with the proxy base must import")
	}
}

// VS Code writes a stdio server with NO "type" field at all (omit_type). If
// import read "type" to decide the transport it would classify these wrongly;
// inferring from the presence of a url sidesteps type_map/omit_type entirely.
func TestImportServerInfersTransportWithoutType(t *testing.T) {
	p := &Plugin{ID: "vscode", Label: "VS Code", RootKey: "servers",
		TypeMap: map[string]string{"http": "http"}, OmitType: []string{"stdio"},
		CommandShape: "string_args", HTTPKey: "url", ProxyBase: "http://127.0.0.1:8787/mcp"}

	s, ok := importServer(p, "fs", map[string]any{
		"command": "npx",
		"args":    []any{"-y", "@modelcontextprotocol/server-filesystem"},
	})
	if !ok {
		t.Fatal("a vscode stdio block (no type field) must import")
	}
	if s.Transport != "stdio" {
		t.Errorf("transport = %q; want stdio — the block has no url, whatever it has for type", s.Transport)
	}
	if s.Command != "npx" {
		t.Errorf("command = %q; want npx", s.Command)
	}

	// Over http the type is present and must be recognised by the url alone.
	s2, ok := importServer(p, "remote", map[string]any{"type": "http", "url": "https://x.example/mcp"})
	if !ok {
		t.Fatal("a vscode http block must import")
	}
	if s2.Transport != "http" || s2.URL != "https://x.example/mcp" {
		t.Errorf("http import gave transport=%q url=%q", s2.Transport, s2.URL)
	}
}

// The merge must not be a rewrite. $comment is the inventory's own
// documentation, an existing server is the user's work, and a backup must be
// left behind so a wrong merge is recoverable.
func TestImportMergePreservesCommentAndSkipsConflicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.json")
	const original = `{
  "$comment": ["the inventory's own documentation"],
  "unrelated_top_level_key": {"keep": "me"},
  "servers": {
    "x": {"transport": "stdio", "command": "keep-me", "why": "the user's own"}
  }
}
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	add := map[string]InventoryServer{
		"x":    {Transport: "stdio", Command: "OVERWRITTEN", Notes: []string{"imported"}},
		"news": {Transport: "stdio", Command: "npx", Args: []string{"-y", "srv"}, Notes: []string{"imported from Claude Code"}},
	}
	if err := mergeIntoInventory(path, add); err != nil {
		t.Fatalf("mergeIntoInventory: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	if !strings.Contains(got, `"$comment"`) || !strings.Contains(got, "the inventory's own documentation") {
		t.Errorf("$comment was lost by the merge:\n%s", got)
	}
	if !strings.Contains(got, "unrelated_top_level_key") || !strings.Contains(got, "keep") {
		t.Errorf("an unrelated top-level key was lost by the merge:\n%s", got)
	}
	if !strings.Contains(got, "keep-me") {
		t.Errorf("the existing server \"x\" was overwritten — a conflict must be left alone:\n%s", got)
	}
	if strings.Contains(got, "OVERWRITTEN") {
		t.Errorf("the conflicting server was replaced; import must never overwrite:\n%s", got)
	}
	if !strings.Contains(got, `"news"`) {
		t.Errorf("the new server was not added:\n%s", got)
	}

	// The file must still parse, and a backup must exist beside it.
	inv, err := LoadInventory(path)
	if err != nil {
		t.Fatalf("the merged inventory no longer loads: %v\n%s", err, got)
	}
	if inv.Servers["x"].Command != "keep-me" {
		t.Errorf("server x command = %q; want keep-me", inv.Servers["x"].Command)
	}
	if inv.Servers["news"].Command != "npx" {
		t.Errorf("server news command = %q; want npx", inv.Servers["news"].Command)
	}
	backups, _ := filepath.Glob(path + ".bak-*")
	if len(backups) == 0 {
		t.Errorf("no backup was written beside %s — the merge is not recoverable", path)
	}
}

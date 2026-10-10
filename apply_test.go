package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests exist because of a specific audit finding. Each one names the
// failure it prevents, so that a later "simplification" of the writer has to
// argue with a test rather than with a memory.

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A file containing `null` used to decode to a nil map with a nil error, and
// writing the root key then panicked with "assignment to entry in nil map" —
// from an HTTP handler and from the CLI alike.
func TestReadDocNullDoesNotPanic(t *testing.T) {
	for _, body := range []string{"null", "", "   \n\t ", "\xef\xbb\xbf{}"} {
		doc, err := readDoc(writeTemp(t, body))
		if err != nil {
			t.Fatalf("readDoc(%q) returned an error: %v", body, err)
		}
		if doc == nil {
			t.Fatalf("readDoc(%q) returned a nil map — writing to it would panic", body)
		}
		doc["mcpServers"] = json.RawMessage(`{}`) // must not panic
	}
}

// A config that is not an object must be REFUSED, not silently replaced.
func TestReadDocRejectsNonObject(t *testing.T) {
	for _, body := range []string{"[]", `"hello"`, "42", "{bad"} {
		if _, err := readDoc(writeTemp(t, body)); err == nil {
			t.Errorf("readDoc(%q) accepted a non-object document", body)
		}
	}
}

// THE finding that matters most: we rewrite a whole file we do not own, so
// everything we do not touch must come back byte-identical.
//
// Decoding into map[string]any turned the numbers into float64 and destroyed
// them: 18446744073709551615 became 18446744073709552000, and the call still
// reported success.
func TestRoundTripPreservesUntouchedValues(t *testing.T) {
	const original = `{
  "big": 18446744073709551615,
  "precise": 9007199254740993,
  "float": 3.141592653589793238462643,
  "exp": 1e2,
  "trailing": 1.0,
  "other": { "nested": [1, 2, {"deep": 12345678901234567890}] },
  "mcpServers": { "old": { "command": "x" } }
}`

	path := writeTemp(t, original)
	doc, err := readDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := setBlock(doc, "mcpServers", map[string]any{
		"new": map[string]any{"command": "y"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeDoc(path, doc); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"18446744073709551615",
		"9007199254740993",
		"3.141592653589793238462643",
		"1e2",
		"1.0",
		"12345678901234567890",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("value %s did not survive the round-trip — the file we do not own was corrupted", want)
		}
	}
	if !strings.Contains(string(got), `"command": "y"`) {
		t.Error("our own block was not written")
	}
}

// A non-UTF-8 byte anywhere in the file must survive too: the old writer
// replaced it with U+FFFD and reported success.
func TestRoundTripPreservesInvalidUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	// 0xE9 alone is invalid UTF-8 (latin-1 "é"), inside a string value.
	body := []byte("{\n  \"latin\": \"caf\xe9\",\n  \"mcpServers\": {}\n}")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := readDoc(path)
	if err != nil {
		t.Skipf("decoder refused the byte rather than mangling it: %v", err)
	}
	if err := setBlock(doc, "mcpServers", map[string]any{"a": map[string]any{"command": "b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeDoc(path, doc); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "caf\xe9") {
		t.Errorf("the 0xE9 byte was rewritten (got %q) — silently mangling data we were only asked to pass through", got)
	}
}

// The write must be atomic: a reader must never observe a truncated file, and
// no temporary file may be left behind.
func TestWriteIsAtomicAndTidy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, _ := readDoc(path)
	if err := setBlock(doc, "mcpServers", map[string]any{"s": map[string]any{"url": "http://x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeDoc(path, doc); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	// The backup must exist and hold the ORIGINAL content.
	baks, _ := filepath.Glob(path + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("expected exactly one backup, found %d", len(baks))
	}
	bak, _ := os.ReadFile(baks[0])
	if string(bak) != `{"a":1}` {
		t.Errorf("the backup does not hold the pre-write content: %q", bak)
	}
}

// Two writes in the same second used to produce the SAME backup name, so the
// second overwrote the first and the pre-first-apply state was lost.
//
// The names are no longer separated by luck: O_EXCL makes the filesystem
// arbitrate. So this test asks the filesystem to refuse the collision in ONE
// frozen millisecond, and checks that each attempt took a NEW name with its own
// intact copy — for as many names as the scheme promises (the bound is tested by
// TestBackupGivesUpInsteadOfOverwriting).
func TestBackupNamesDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for i := 0; i < backupAttempts; i++ {
		name, err := createBackupAt(path, []byte(fmt.Sprintf("%d", i)), 0o644, at)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if seen[name] {
			t.Fatalf("backup name reused within the same second: %s", name)
		}
		seen[name] = true
		got, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if string(got) != fmt.Sprintf("%d", i) {
			t.Fatalf("attempt %d: the backup holds %q, not what we handed it", i, got)
		}
	}
}

// The counter is BOUNDED. When every candidate name is taken, createBackup must
// say so — never overwrite a backup that already exists, which is the whole
// reason the backup exists.
func TestBackupGivesUpInsteadOfOverwriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	stamp := at.Format("20060102-150405.000")
	// Occupy every name createBackup will propose in this millisecond.
	for n := 0; n < backupAttempts; n++ {
		name := path + ".bak-" + stamp
		if n > 0 {
			name = fmt.Sprintf("%s.bak-%s-%d", path, stamp, n)
		}
		if err := os.WriteFile(name, []byte("occupied"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := createBackupAt(path, []byte("mine"), 0o644, at); err == nil {
		t.Fatal("createBackup reported success with every candidate name taken")
	}
	first := path + ".bak-" + stamp
	got, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "occupied" {
		t.Errorf("an existing backup was overwritten: %q", got)
	}
}

// A file we may not write must FAIL, and must be left exactly as it was.
//
// This is the test that carries the weight on Windows, where os.Chmod only
// toggles the read-only attribute and Go reports a writable file as 0666
// regardless — so "the mode is unchanged" is not an assertion that can fail
// there. What CAN fail, and matters, is: we must not clobber a file we were not
// allowed to touch, we must not report success, and we must not leave litter.
func TestWriteToReadOnlyFileFailsCleanly(t *testing.T) {
	const original = `{"a":1}`
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Skipf("cannot make the file read-only here: %v", err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm()&0o200 != 0 {
		t.Skip("this platform does not make chmod 0444 read-only")
	}

	doc, err := readDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := setBlock(doc, "mcpServers", map[string]any{"s": map[string]any{"url": "http://x"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := writeDoc(path, doc); err == nil {
		t.Fatal("writing a read-only file reported SUCCESS")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("the read-only file was modified anyway: %q", got)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a failed write left a temporary file behind: %s", e.Name())
		}
	}
}

// A DRY RUN MUST NOT WRITE.
//
// This is not hypothetical. Refactoring the apply loop into applyToFile turned
// its early `continue`s into fall-throughs, and the preview went straight on
// into the write path — a "show me what would change" click would have changed
// it. The whole promise of `apply` without --write is that it touches nothing,
// so that promise gets a test.
func TestDryRunNeverWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	const original = `{"mcpServers":{"old":{"command":"x"}}}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)

	p := testPlugin(path)
	want := map[string]any{"new": map[string]any{"url": "http://x"}}

	var buf strings.Builder
	if err := applyToFile(&buf, p, &Inventory{}, want, nil, false); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("a dry run MODIFIED the file:\n  got  %s\n  want %s", got, original)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("a dry run touched the file's modification time")
	}
	if baks, _ := filepath.Glob(path + ".bak-*"); len(baks) != 0 {
		t.Errorf("a dry run created a backup: %v", baks)
	}
	if !strings.Contains(buf.String(), "dry-run") {
		t.Error("the report does not tell the user it was a dry run")
	}
}

// The other half of the same promise: with write, it must actually write.
func TestWriteActuallyWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"old":{"command":"x"}},"keep":42}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	if err := applyToFile(&buf, testPlugin(path), &Inventory{},
		map[string]any{"new": map[string]any{"url": "http://x"}}, nil, true); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `"http://x"`) {
		t.Errorf("the write did not happen:\n%s", got)
	}
	if !strings.Contains(string(got), "42") {
		t.Errorf("a key we do not own was lost:\n%s", got)
	}
	if baks, _ := filepath.Glob(path + ".bak-*"); len(baks) != 1 {
		t.Errorf("expected exactly one backup, got %v", baks)
	}
}

// And when there is nothing to change, the file must be left completely alone —
// not rewritten identically.
func TestNoOpWriteTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	body := `{"mcpServers":{"new":{"url":"http://x"}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)

	var buf strings.Builder
	if err := applyToFile(&buf, testPlugin(path), &Inventory{},
		map[string]any{"new": map[string]any{"url": "http://x"}}, nil, true); err != nil {
		t.Fatal(err)
	}

	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("an already-up-to-date file was rewritten anyway")
	}
	if baks, _ := filepath.Glob(path + ".bak-*"); len(baks) != 0 {
		t.Errorf("a no-op apply created a backup: %v", baks)
	}
	if !strings.Contains(buf.String(), "nothing written") {
		t.Errorf("the report does not say nothing was written:\n%s", buf.String())
	}
}

func testPlugin(path string) *Plugin {
	return &Plugin{
		ID: "t", Label: "Test", ConfigPath: path, RootKey: "mcpServers",
		TypeMap: map[string]string{"http": "http", "stdio": "stdio"},
		HTTPKey: "url", CommandShape: "string_args", Writer: "file",
	}
}

// M6: env and headers must reach the client's config for the servers that need
// them (direct writes) — and must NOT for the routed ones, where the manager
// itself carries the auth. And values travel VERBATIM: a resolved secret in a
// plaintext config file is exactly what the inventory's "names only" rule
// exists to prevent.
func TestRenderCarriesEnvAndHeaders(t *testing.T) {
	p := testPlugin(filepath.Join(t.TempDir(), "cfg.json"))

	stdio := InventoryServer{
		Transport: "stdio", Command: "npx", Args: []string{"-y", "@some/mcp"},
		Env: map[string]string{"OPENAI_API_KEY": "${OPENAI_API_KEY}"},
	}
	m := Render(p, "gen", stdio)
	env, ok := m["env"].(map[string]string)
	if !ok || env["OPENAI_API_KEY"] != "${OPENAI_API_KEY}" {
		t.Fatalf("stdio render lost env: %v (m=%v)", m["env"], m)
	}

	httpDirect := InventoryServer{
		Transport: "http", URL: "https://remote.example/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${MCP_TOKEN}"},
	}
	m = Render(p, "remote", httpDirect)
	hdr, ok := m["headers"].(map[string]string)
	if !ok || hdr["Authorization"] != "Bearer ${MCP_TOKEN}" {
		t.Fatalf("http render lost headers: %v (m=%v)", m["headers"], m)
	}

	// Absent fields must stay absent: an "env": {} line in every block of
	// every client file is noise a human reads.
	m = Render(p, "plain", InventoryServer{Transport: "stdio", Command: "x"})
	if _, has := m["env"]; has {
		t.Error(`a server without env got an "env" key in the client's config`)
	}
	if _, has := m["headers"]; has {
		t.Error(`a server without headers got a "headers" key in the client's config`)
	}

	// Routed through the manager: the client talks to localhost, the manager
	// forwards with the headers — writing them into the client file would put
	// the reference (and any literal) in a file the manager does not control.
	proxied := testPlugin(filepath.Join(t.TempDir(), "cfg2.json"))
	proxied.ProxyBase = "http://127.0.0.1:7812/mcp"
	m = Render(proxied, "remote", httpDirect)
	if _, has := m["headers"]; has {
		t.Error(`a routed server got "headers" in the client's config: the manager is the one who authenticates`)
	}
	m = Render(proxied, "gen", stdio)
	if _, has := m["env"]; has {
		t.Error(`a routed server got "env" in the client's config: the manager is the one who launches`)
	}
}

// diffBlock must converge: comparing a block with itself finds nothing.
func TestDiffBlockIsIdempotent(t *testing.T) {
	raw := json.RawMessage(`{"a":{"url":"http://x"},"b":{"url":"http://y"}}`)
	want := map[string]any{
		"a": map[string]any{"url": "http://x"},
		"b": map[string]any{"url": "http://y"},
	}
	added, changed, removed := diffBlock(raw, want)
	if len(added)+len(changed)+len(removed) != 0 {
		t.Errorf("an identical block reported changes: +%v ~%v -%v", added, changed, removed)
	}
}

// An absent block is not an error: every client starts with no servers.
func TestDiffBlockHandlesAbsentBlock(t *testing.T) {
	added, changed, removed := diffBlock(nil, map[string]any{"a": map[string]any{"url": "x"}})
	if len(added) != 1 || len(changed) != 0 || len(removed) != 0 {
		t.Errorf("absent block should read as one addition, got +%v ~%v -%v", added, changed, removed)
	}
}

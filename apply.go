package main

// The "plugin" layer: transforming the inventory of MCP servers into the
// config of each client.
//
// A plugin is a declarative file (plugins/*.json). The program knows how to
// RENDER a server according to the plugin's fields; the plugin says WHAT to
// render and WHERE to write it. Adding a client = adding a file. No code.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The inventory
// ---------------------------------------------------------------------------

type InventoryServer struct {
	Transport    string   `json:"transport"`
	Command      string   `json:"command"`
	Args         []string `json:"args"`
	URL          string   `json:"url"`
	Why          string   `json:"why"`
	Notes        []string `json:"notes"`
	NeedsService string   `json:"needs_service"`
	// Clients limits this server to certain clients (plugin ids).
	// Absent or empty = ALL clients. That is the deliberate default: a client
	// added tomorrow inherits everything, and we restrict afterwards if needed.
	Clients []string `json:"clients"`
	// Manager: this server IS the manager. It wires onto the proxy base,
	// not onto /mcp/<name> — otherwise we would be jumping to ourselves.
	Manager bool `json:"manager"`
	// Direct: do not go through the proxy, write the upstream as is. Escape hatch
	// to debug a server that the manager does not know how to relay.
	Direct bool `json:"direct"`
}

// AppliesTo says whether a server must be written for the client described by
// this plugin.
//
// nil = field absent = ALL clients. An empty list [] = NONE.
// The distinction matters: without it, "uncheck everything for this client"
// would be read back as "all clients", the exact opposite of what was just asked.
func (s InventoryServer) AppliesTo(pluginID string) bool {
	if s.Clients == nil {
		return true
	}
	for _, c := range s.Clients {
		if c == pluginID {
			return true
		}
	}
	return false
}

type Inventory struct {
	Servers map[string]InventoryServer `json:"servers"`
}

func LoadInventory(path string) (*Inventory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("inventory unreadable (%s): %w", path, err)
	}
	var inv Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("invalid inventory (%s): %w", path, err)
	}
	if len(inv.Servers) == 0 {
		return nil, fmt.Errorf("empty inventory: no server in %s", path)
	}
	return &inv, nil
}

// ---------------------------------------------------------------------------
// The plugin
// ---------------------------------------------------------------------------

type Plugin struct {
	ID           string            `json:"id"`
	Label        string            `json:"label"`
	ConfigPath   string            `json:"config_path"`
	RootKey      string            `json:"root_key"`
	TypeMap      map[string]string `json:"type_map"`
	CommandShape string            `json:"command_shape"`
	HTTPKey      string            `json:"http_key"`
	// CommandPrefix is prefixed to the command array when command_shape is
	// "array". On Windows, a Node client cannot launch "npx" (.cmd) without
	// going through a shell: opencode therefore needs ["cmd", "/c"].
	CommandPrefix []string `json:"command_prefix"`
	// ProxyBase: if non-empty, this client does NOT talk to the upstreams
	// directly but to the manager, which becomes the transport. Each server is
	// then written as http towards <proxy_base>/<name> (and <proxy_base> alone
	// for the manager itself). That is what makes "the request arrives,
	// the service is off, we start it" work.
	ProxyBase string `json:"proxy_base"`
	// OmitType: transports for which we write NO "type" field.
	//
	// Measured, not assumed: `code --add-mcp` writes a VS Code stdio server
	// as {"command":…,"args":…} — with no "type". Some clients infer the
	// transport from the presence of "command" or "url", and do not like
	// being told.
	OmitType []string `json:"omit_type"`

	// ---------------------------------------------------------------------
	// API writer — for clients that have no config FILE.
	// ---------------------------------------------------------------------
	//
	// Most clients keep their servers in a JSON file, and the writer above is
	// enough. AJEAN does not: it keeps them in a bbolt database and exposes
	// them only through its own HTTP API. So a plugin may declare
	// writer:"api" and we drive that API instead of writing a file.
	//
	// Everything else in the plugin — proxy_base, omit_type, targeting — works
	// exactly the same, because the body we POST is the very map Render()
	// produces, with the server name added.

	// Writer: "file" (the default) or "api".
	Writer string `json:"writer"`

	APIBase string `json:"api_base"`
	// Where the client lists its servers, and how to read the names out of the
	// answer: the JSON array to look into, and the field holding the name.
	// AJEAN answers {"ok":true,"servers":[{"name":"…"},…]}.
	APIListPath  string `json:"api_list_path"`
	APIListArray string `json:"api_list_array"`
	APINameField string `json:"api_name_field"`
	// Where we add/replace one server, and where we remove one.
	APISavePath   string `json:"api_save_path"`
	APIDeletePath string `json:"api_delete_path"`
	// Optional bearer auth. Empty = no Authorization header.
	APIAuthHeader string `json:"api_auth_header"`
	APIAuthValue  string `json:"api_auth_value"`
}

// IsAPI says whether this client is configured over HTTP rather than in a file.
func (p *Plugin) IsAPI() bool { return strings.EqualFold(p.Writer, "api") }

// Target is the human-readable destination, for the report.
func (p *Plugin) Target() string {
	if p.IsAPI() {
		return strings.TrimSuffix(p.APIBase, "/") + p.APIListPath
	}
	return expandPath(p.ConfigPath)
}

func LoadPlugin(path string) (*Plugin, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Plugin
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("invalid plugin (%s): %w", path, err)
	}
	if p.ID == "" {
		p.ID = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	if p.RootKey == "" {
		p.RootKey = "mcpServers"
	}
	if p.TypeMap == nil {
		p.TypeMap = map[string]string{"stdio": "stdio", "http": "http"}
	}
	if p.CommandShape == "" {
		p.CommandShape = "string_args"
	}
	if p.HTTPKey == "" {
		p.HTTPKey = "url"
	}
	if p.Writer == "" {
		p.Writer = "file"
	}
	if p.IsAPI() {
		// Defaults matching the shape AJEAN uses, so its plugin only has to
		// declare the base URL.
		if p.APIListArray == "" {
			p.APIListArray = "servers"
		}
		if p.APINameField == "" {
			p.APINameField = "name"
		}
		if p.APIListPath == "" {
			return nil, fmt.Errorf("plugin %s: writer \"api\" needs api_list_path", p.ID)
		}
		if p.APISavePath == "" || p.APIDeletePath == "" {
			return nil, fmt.Errorf("plugin %s: writer \"api\" needs api_save_path and api_delete_path", p.ID)
		}
	}
	return &p, nil
}

// LoadPlugins reads all the *.json of a folder (except those whose name starts
// with "_", reserved for disabled examples).
func LoadPlugins(dir string) ([]*Plugin, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("plugins folder unreadable (%s): %w", dir, err)
	}
	var out []*Plugin
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		p, err := LoadPlugin(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// Render translates a server of the inventory into the block specific to this
// client. This is where the only two real forms of "type" and "command" live —
// everything else is driven by the plugin's fields.
func Render(p *Plugin, name string, s InventoryServer) map[string]any {
	m := map[string]any{}

	// setType sets the "type" in the client's vocabulary ("stdio"/"http",
	// or "local"/"remote" for opencode) — unless the client does not want it.
	setType := func(kind string) {
		for _, t := range p.OmitType {
			if t == kind {
				return
			}
		}
		v := p.TypeMap[kind]
		if v == "" {
			v = kind
		}
		m["type"] = v
	}

	// Routing through the manager: takes priority over the upstream's transport,
	// because that is precisely the point — the client talks only to the manager,
	// which handles the prerequisite before forwarding.
	if p.ProxyBase != "" && !s.Direct {
		setType("http")
		if s.Manager {
			m[p.HTTPKey] = strings.TrimSuffix(p.ProxyBase, "/")
		} else {
			m[p.HTTPKey] = strings.TrimSuffix(p.ProxyBase, "/") + "/" + name
		}
		return m
	}

	if s.Transport == "http" {
		setType("http")
		m[p.HTTPKey] = s.URL
		return m
	}

	// stdio
	setType("stdio")
	if p.CommandShape == "array" {
		cmd := append([]string{}, p.CommandPrefix...)
		cmd = append(cmd, s.Command)
		cmd = append(cmd, s.Args...)
		m["command"] = cmd
	} else {
		m["command"] = s.Command
		if len(s.Args) > 0 {
			m["args"] = s.Args
		}
	}
	return m
}

// RenderAll renders the complete list for a client, ordered by server name
// for a stable diff. The servers not targeted at this client are rendered
// separately: they are not written, but we NAME them so that the omission is
// visible rather than silent.
func RenderAll(p *Plugin, inv *Inventory) (written map[string]any, skipped []string) {
	names := make([]string, 0, len(inv.Servers))
	for n := range inv.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	written = map[string]any{}
	for _, n := range names {
		s := inv.Servers[n]
		if !s.AppliesTo(p.ID) {
			skipped = append(skipped, n)
			continue
		}
		written[n] = Render(p, n, s)
	}
	return written, skipped
}

// ---------------------------------------------------------------------------
// The apply command
// ---------------------------------------------------------------------------

// expandPath replaces a possible leading "~" with the user profile.
func expandPath(p string) string {
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(p, "~\\") || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// applyDir is the plugins folder, resolved next to the executable like the rest
// (see assetPath).
func applyDir() string {
	if exe, err := os.Executable(); err == nil {
		d := filepath.Join(filepath.Dir(exe), "plugins")
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return "plugins"
}

// readDoc reads a JSON file, keeping EVERY top-level value as its original
// bytes.
//
// Why raw bytes and not a decoded map — this is the difference between a safe
// writer and a destructive one. Decoding into map[string]any turns every JSON
// number into a float64, so re-writing the file silently corrupts whatever we
// did not touch: 18446744073709551615 comes back as 18446744073709552000, and
// a non-UTF-8 byte comes back as U+FFFD — reported as success. Files like
// ~/.claude.json hold 62 top-level keys we have no business re-encoding.
//
// Everything except the one key we replace therefore survives byte-for-byte.
//
// A file that is missing, empty, whitespace-only, BOM-prefixed or `null` is
// treated as an empty document rather than an error. Each of those is a real
// case on Windows (Notepad and PowerShell 5.1 write BOMs), and refusing them
// would abort the whole apply for every other client too.
func readDoc(path string) (map[string]json.RawMessage, error) {
	empty := func() map[string]json.RawMessage { return map[string]json.RawMessage{} }

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return empty(), nil
		}
		return nil, err
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	trimmed := bytes.TrimSpace(raw)
	// An explicit `null` check: Go's decoder IGNORES a JSON null for a map
	// value, so without this the document would silently stay empty — correct,
	// but it would also skip the "not an object" rejection below, and a file
	// containing nothing but `null` deserves to be read as "no config yet".
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return empty(), nil
	}

	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("existing config unreadable (%s): %w", path, err)
	}
	// Defensive, and currently unreachable: the explicit `null` check above
	// already catches the only input that decodes to a nil map without an
	// error. Kept because the cost is one comparison and the failure it guards
	// is a panic inside a background service.
	if doc == nil {
		doc = empty()
	}
	return doc, nil
}

// serverNames returns the keys of a servers block, sorted.
func serverNames(v any) []string {
	m, _ := v.(map[string]any)
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// blockFor decodes the value stored under a root key, for COMPARISON only.
// Decoding here is safe: it decides whether to write, never what to write.
func blockFor(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// diffBlock compares a client's current block with the block we want to write.
// It returns three lists: added, changed, removed. This is what makes visible
// the fact that we REPLACE the block — therefore that what is not in the
// inventory disappears.
func diffBlock(cur json.RawMessage, want map[string]any) (added, changed, removed []string) {
	curM := blockFor(cur)
	for n, w := range want {
		c, ok := curM[n]
		if !ok {
			added = append(added, n)
			continue
		}
		a, _ := json.Marshal(c)
		b, _ := json.Marshal(w)
		if string(a) != string(b) {
			changed = append(changed, n)
		}
	}
	for n := range curM {
		if _, ok := want[n]; !ok {
			removed = append(removed, n)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return
}

// marshalDoc encodes a JSON document as a human must read it.
//
// SetEscapeHTML(false): without that, Go writes < for "<" and & for
// "&". It is perfectly valid JSON, but unreadable in a configuration
// file — and in a file we do not entirely own (VS Code's settings), it
// would dirty the diff of the whole file for nothing.
func marshalDoc(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// backupPath names a backup next to the file it copies.
//
// Milliseconds AND four random hex digits: the previous version had
// second resolution, so two applies in the same second produced the same name
// and the second silently overwrote the first — losing the state from before
// the first one.
func backupPath(path string) string {
	return fmt.Sprintf("%s.bak-%s-%04x", path, time.Now().Format("20060102-150405.000"), rand.Intn(0x10000))
}

// writeFileAtomic writes data to path without ever leaving a half-written file
// THERE.
//
// os.WriteFile truncates in place, so a crash, a full disk or a killed process
// mid-write leaves the file destroyed — and for a config file the user's whole
// client goes with it. Instead: write a temporary file in the SAME directory
// (so the rename cannot cross a filesystem), flush it, then replace the target
// in one step.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once the rename has succeeded

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeDoc backs up the existing file, then writes the document atomically.
// The backup is timestamped and placed NEXT TO the file, never elsewhere.
// The file's own permissions are preserved: we are a guest in it.
func writeDoc(path string, doc map[string]json.RawMessage) (string, error) {
	perm := os.FileMode(0o644)
	backup := ""
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		backup = backupPath(path)
		if err := os.WriteFile(backup, raw, perm); err != nil {
			return "", fmt.Errorf("backup impossible (%s): %w", backup, err)
		}
	}
	out, err := marshalDoc(doc)
	if err != nil {
		return backup, err
	}
	if err := writeFileAtomic(path, out, perm); err != nil {
		return backup, err
	}
	return backup, nil
}

// setBlock stores the rendered servers under the client's root key, leaving
// every other top-level value exactly as it was read.
func setBlock(doc map[string]json.RawMessage, key string, want map[string]any) error {
	enc, err := marshalDoc(want)
	if err != nil {
		return err
	}
	doc[key] = json.RawMessage(enc)
	return nil
}

// pluginIDs returns the plugins' ids, sorted — for the error messages.
func pluginIDs(plugins []*Plugin) []string {
	ids := make([]string, 0, len(plugins))
	for _, p := range plugins {
		ids = append(ids, p.ID)
	}
	sort.Strings(ids)
	return ids
}

// validateClients rejects a misspelled "clients": it would exclude the server
// everywhere, silently, and we would search for a long time.
func validateClients(inv *Inventory, plugins []*Plugin) error {
	known := map[string]bool{}
	for _, p := range plugins {
		known[p.ID] = true
	}
	for name, s := range inv.Servers {
		for _, c := range s.Clients {
			if !known[c] {
				return fmt.Errorf("inventory.json: server %q targets an unknown client %q (known plugins: %s)",
					name, c, strings.Join(pluginIDs(plugins), ", "))
			}
		}
	}
	return nil
}

// applyTo is the heart of the tool: for each plugin, it translates the inventory
// into the client's format and replaces ITS MCP block — nothing else in the
// file.
//
// It writes its report into w, which lets the CLI (os.Stdout) and the web
// interface (a buffer, returned as JSON) share exactly the same code path.
func applyTo(w io.Writer, inv *Inventory, write bool) error {
	plugins, err := LoadPlugins(applyDir())
	if err != nil {
		return err
	}
	if len(plugins) == 0 {
		return fmt.Errorf("no plugin in %s", applyDir())
	}
	if err := validateClients(inv, plugins); err != nil {
		return err
	}

	// A client that cannot be reached is NOT allowed to stop the others.
	//
	// The first version returned on the first error, which meant one closed
	// application made the whole tool unusable: with AJEAN shut down, you could
	// not even PREVIEW what would change for Claude Code, opencode and VS Code,
	// because the run died on the first plugin alphabetically. A dry run is
	// read-only; refusing to do it because a fourth application is closed is
	// absurd. Each failure is now reported in place and the run continues.
	var failures []string

	for _, p := range plugins {
		want, skipped := RenderAll(p, inv)

		var err error
		if p.IsAPI() {
			err = applyToAPI(w, p, inv, want, skipped, write)
		} else {
			err = applyToFile(w, p, inv, want, skipped, write)
		}
		if err != nil {
			fmt.Fprintf(w, "==== %s (%s) ====\n", p.Label, p.ID)
			fmt.Fprintf(w, "  FAILED — %v\n\n", err)
			failures = append(failures, fmt.Sprintf("%s: %v", p.Label, err))
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d client(s) could not be processed — everything above still applies:\n  - %s",
			len(failures), strings.Join(failures, "\n  - "))
	}
	return nil
}

// applyToFile is the file writer for one client: render, diff, show, and (only
// with write) replace the one root key.
func applyToFile(w io.Writer, p *Plugin, inv *Inventory, want map[string]any, skipped []string, write bool) error {
	path := expandPath(p.ConfigPath)
	doc, err := readDoc(path)
	if err != nil {
		return err
	}
	added, changed, removed := diffBlock(doc[p.RootKey], want)

	fmt.Fprintf(w, "==== %s (%s) ====\n", p.Label, p.ID)
	fmt.Fprintf(w, "target     : %s\n", path)
	fmt.Fprintf(w, "root key   : %q  (%d server(s) written)\n", p.RootKey, len(want))
	for _, n := range added {
		fmt.Fprintf(w, "  + %s\n", n)
	}
	for _, n := range changed {
		fmt.Fprintf(w, "  ~ %s\n", n)
	}
	for _, n := range removed {
		fmt.Fprintf(w, "  - %s   (present at the client, absent from the inventory)\n", n)
	}
	if len(added)+len(changed)+len(removed) == 0 {
		fmt.Fprintf(w, "  = already up to date\n")
	}
	for _, n := range skipped {
		c := inv.Servers[n].Clients
		fmt.Fprintf(w, "  · %s   (not targeted here: clients=%s)\n", n, strings.Join(c, ","))
	}

	if !write {
		// We show the EXACT BLOCK, not just the list of names. It is the
		// only way to see that one client receives "type: http" where
		// another does not want it — and to re-read the shape before
		// imposing it on a file we do not entirely own.
		block, err := marshalDoc(want)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "\n  block that would be written into %q:\n", p.RootKey)
		for _, line := range strings.Split(strings.TrimRight(string(block), "\n"), "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
		fmt.Fprintf(w, "\n(dry-run — nothing is written. Add --write to apply.)\n\n")
		return nil
	}

	// Nothing to change: do NOT rewrite.
	//
	// The old version wrote the file anyway, so every no-op apply still
	// re-encoded a document we do not own and left a backup to prove it.
	// An apply that changes nothing must touch nothing.
	if len(added)+len(changed)+len(removed) == 0 {
		fmt.Fprintf(w, "\n  nothing written — the file already matches.\n\n")
		return nil
	}

	if err := setBlock(doc, p.RootKey, want); err != nil {
		return err
	}
	backup, err := writeDoc(path, doc)
	if err != nil {
		return err
	}
	if backup != "" {
		fmt.Fprintf(w, "\n  written. backup: %s\n\n", backup)
	} else {
		fmt.Fprintf(w, "\n  written (the file did not exist).\n\n")
	}
	return nil
}

// ---------------------------------------------------------------------------
// The API writer — for a client that has no config file
// ---------------------------------------------------------------------------

// apiClient talks to one client's HTTP API.
type apiClient struct {
	p   *Plugin
	cli *http.Client
}

func newAPIClient(p *Plugin) *apiClient {
	// No timeout on the client itself: a client's API is local, but starting a
	// stdio server behind it can take a while. The per-call deadline is set on
	// the request instead, so the message names what actually hung.
	return &apiClient{p: p, cli: &http.Client{Timeout: 30 * time.Second}}
}

func (c *apiClient) url(path string) string {
	return strings.TrimSuffix(c.p.APIBase, "/") + path
}

func (c *apiClient) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := marshalDoc(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.url(path), rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.p.APIAuthHeader != "" && c.p.APIAuthValue != "" {
		req.Header.Set(c.p.APIAuthHeader, c.p.APIAuthValue)
	}
	resp, err := c.cli.Do(req)
	if err != nil {
		// The one thing that is different about an API client, and it deserves
		// to be said plainly: there is no file to write while it sleeps.
		return nil, fmt.Errorf("%s is not answering on %s — it must be RUNNING for us to configure it "+
			"(a file client can be configured while closed; an API client cannot): %w",
			c.p.Label, c.p.APIBase, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return raw, fmt.Errorf("%s %s returned HTTP %d: %s", method, path, resp.StatusCode,
			strings.TrimSpace(firstLine(string(raw))))
	}
	return raw, nil
}

// list reads the servers currently configured at the client, as a map keyed by
// name. The values are the client's own objects, so we can compare only the
// fields WE control.
func (c *apiClient) list() (map[string]map[string]any, error) {
	raw, err := c.do(http.MethodGet, c.p.APIListPath, nil)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %s did not return JSON: %w", c.p.Label, c.p.APIListPath, err)
	}
	arr, _ := doc[c.p.APIListArray].([]any)

	out := map[string]map[string]any{}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m[c.p.APINameField].(string)
		if name != "" {
			out[name] = m
		}
	}
	return out, nil
}

// sameAsWanted compares only the keys we are going to write. The client's own
// answer also carries computed fields (connected, tool count, transport) that
// differ on every call — comparing those would report a change every time.
func sameAsWanted(want map[string]any, got map[string]any) bool {
	for k, v := range want {
		a, _ := json.Marshal(v)
		b, _ := json.Marshal(got[k])
		if string(a) != string(b) {
			return false
		}
	}
	return true
}

// applyToAPI drives a client's HTTP API instead of a file.
func applyToAPI(w io.Writer, p *Plugin, inv *Inventory, want map[string]any, skipped []string, write bool) error {
	c := newAPIClient(p)
	current, err := c.list()
	if err != nil {
		return err
	}

	var added, changed, removed []string
	for n, body := range want {
		cur, ok := current[n]
		switch {
		case !ok:
			added = append(added, n)
		case !sameAsWanted(body.(map[string]any), cur):
			changed = append(changed, n)
		}
	}
	for n := range current {
		if _, ok := want[n]; !ok {
			removed = append(removed, n)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)

	fmt.Fprintf(w, "==== %s (%s) ====\n", p.Label, p.ID)
	fmt.Fprintf(w, "target     : %s   (HTTP API — the client must be running)\n", p.Target())
	fmt.Fprintf(w, "remote     : %d server(s) present, %d wanted\n", len(current), len(want))
	for _, n := range added {
		fmt.Fprintf(w, "  + %s\n", n)
	}
	for _, n := range changed {
		fmt.Fprintf(w, "  ~ %s\n", n)
	}
	for _, n := range removed {
		fmt.Fprintf(w, "  - %s   (present at the client, absent from the inventory)\n", n)
	}
	if len(added)+len(changed)+len(removed) == 0 {
		fmt.Fprintf(w, "  = already up to date\n")
	}
	for _, n := range skipped {
		fmt.Fprintf(w, "  · %s   (not targeted here: clients=%s)\n", n, strings.Join(inv.Servers[n].Clients, ","))
	}

	// The dry run shows the CALLS, not a JSON block: for an API client the
	// calls ARE the change. Same promise as the file writer — you see exactly
	// what would happen before it happens.
	if !write {
		fmt.Fprintf(w, "\n  calls that would be made against %s:\n", p.APIBase)
		for _, n := range removed {
			body, _ := marshalDoc(map[string]any{p.APINameField: n})
			fmt.Fprintf(w, "    DELETE %s\n%s", p.APIDeletePath, indent(string(body), "      "))
		}
		for _, n := range append(append([]string{}, added...), changed...) {
			body, _ := marshalDoc(withName(p, n, want[n].(map[string]any)))
			fmt.Fprintf(w, "    POST   %s\n%s", p.APISavePath, indent(string(body), "      "))
		}
		if len(added)+len(changed)+len(removed) == 0 {
			fmt.Fprintf(w, "    (none — nothing to do)")
		}
		fmt.Fprintf(w, "\n\n(dry-run — nothing was sent. Add --write to apply.)\n\n")
		return nil
	}

	// Remove first, then write: if a name is both removed and re-added the
	// order does not matter, but removing first means a client's stale entry
	// never coexists with its replacement.
	for _, n := range removed {
		if _, err := c.do(http.MethodPost, p.APIDeletePath, map[string]any{p.APINameField: n}); err != nil {
			return err
		}
		fmt.Fprintf(w, "  removed %s\n", n)
	}
	for _, n := range append(append([]string{}, added...), changed...) {
		if _, err := c.do(http.MethodPost, p.APISavePath, withName(p, n, want[n].(map[string]any))); err != nil {
			return err
		}
		fmt.Fprintf(w, "  saved %s\n", n)
	}
	fmt.Fprintf(w, "\n  applied over HTTP — no file was touched.\n\n")
	return nil
}

// withName adds the server name to the rendered body: the client's API takes
// one named server at a time, where a file takes them all under one key.
func withName(p *Plugin, name string, body map[string]any) map[string]any {
	out := map[string]any{p.APINameField: name}
	for k, v := range body {
		out[k] = v
	}
	return out
}

// indent pads every line of s, including the first, and ends with a newline.
func indent(s, pad string) string {
	return pad + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n"+pad) + "\n"
}

// cmdApply is the CLI entry point: report on standard output.
func cmdApply(inv *Inventory, write bool) error {
	return applyTo(os.Stdout, inv, write)
}

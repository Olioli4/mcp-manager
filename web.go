package main

// Local web interface.
//
// Why a web page and not a native window: the configuration we want to show is
// a MATRIX (clients x servers) plus an editable inventory — it does not fit in a
// notification-area menu, and a real native window would require an entire
// graphical toolkit for less comfort. A page embedded in the binary keeps
// everything in a single .exe, without build tools, and reads in the browser the
// user already has.
//
// The server listens ONLY on 127.0.0.1: nothing is exposed to the network.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

//go:embed web/index.html
var indexHTML []byte

// startWeb starts the server and returns the URL to open.
func startWeb(e *Engine) (string, error) {
	host := e.Manifest().Settings.WebHost
	port := e.Manifest().Settings.WebPort
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 8787
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// no-store: the page is EMBEDDED in the binary. Without this header, the
		// browser keeps it in cache and the user keeps seeing the old interface
		// after an update — an invisible lag between the program and what it
		// believes it is showing.
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) { handleState(w, e) })
	mux.HandleFunc("/api/service/", func(w http.ResponseWriter, r *http.Request) { handleService(w, r, e) })
	mux.HandleFunc("/api/toggle", func(w http.ResponseWriter, r *http.Request) { handleToggle(w, r, e) })
	mux.HandleFunc("/api/server", func(w http.ResponseWriter, r *http.Request) { handleServer(w, r, e) })
	mux.HandleFunc("/api/reload", func(w http.ResponseWriter, r *http.Request) { handleReload(w, r, e) })
	mux.HandleFunc("/api/apply", func(w http.ResponseWriter, r *http.Request) { handleApply(w, r) })

	// The MCP surface lives on the same port: / = the interface for the human,
	// /mcp = the same thing for an agent.
	mcpHandler := mcpHTTPHandler(e)
	mux.Handle("/mcp", mcpHandler)

	// One proxy per inventory server, but routed by a DISPATCHER and not by one
	// route per name. Why: adding a server from the interface must work without a
	// restart — a route registered at startup cannot appear later.
	//
	// We do NOT connect at startup: the visible tools come from the disk cache,
	// and the connection is made on the first request. Without that, starting the
	// manager would launch processes nobody asked for.
	mux.HandleFunc("/mcp/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/mcp/"), "/")
		if name == "" || name == "system" {
			mcpHandler.ServeHTTP(w, r)
			return
		}
		proxiesMu.RLock()
		b := proxiesReg[name]
		proxiesMu.RUnlock()
		if b == nil {
			http.Error(w, fmt.Sprintf("unknown server: %q", name), http.StatusNotFound)
			return
		}
		b.handler.ServeHTTP(w, r)
	})

	if err := ReloadProxies(e); err != nil {
		logf("proxy NOT mounted: %v", err)
	}

	addr := net.JoinHostPort(host, fmt.Sprint(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("port %d unavailable: %w", port, err)
	}
	// localOnly wraps EVERYTHING, the API and the MCP surface alike.
	go func() { _ = http.Serve(ln, localOnly(mux)) }()
	return "http://" + addr, nil
}

// loopbackHost reports whether a hostname is this machine's own loopback.
func loopbackHost(h string) bool {
	h = strings.Trim(strings.ToLower(h), "[]")
	if i := strings.LastIndex(h, "%"); i >= 0 { // IPv6 zone, e.g. fe80::1%eth0
		h = h[:i]
	}
	switch h {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	// The whole 127.0.0.0/8 block is loopback.
	if strings.HasPrefix(h, "127.") {
		return true
	}
	return false
}

// localOnly refuses any request that did not come from this machine.
//
// Two checks, because they close two DIFFERENT doors, and this server has no
// authentication — it listens on loopback and nothing else stands in the way.
//
//   - Host. A browser can be aimed at an attacker's domain whose DNS record
//     points at 127.0.0.1: a "DNS rebinding" attack. The request then arrives
//     here carrying Host: attacker.example. Accepting it makes every endpoint
//     on this server reachable from a web page.
//
//   - Origin. A cross-origin POST is made BY another site. The browser stamps
//     it with an Origin header; a request from our own page carries ours. Only
//     loopback origins are accepted.
//
// Why the Origin check cannot simply be dropped as "browsers won't do that":
// a POST with Content-Type: text/plain is a CORS *simple request* — no
// preflight is sent — and a bare HTML <form> can send one with no JavaScript at
// all. Nothing upstream stops it.
//
// What this closed, measured on 2026-10-09 before the fix: a cross-origin POST
// reached /api/server, replaced an inventory server with `cmd /c …`, and
// because the CACHED tool list for that name survived, the next tool call on
// /mcp/<name> executed the planted command. Arbitrary code execution from any
// page the user visited.
//
// Loopback origins are allowed on ANY port, not just ours: AJEAN drives us from
// its own web UI on :8090. That is safe — a page cannot forge the Origin header
// — and it keeps a local app able to talk to us.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !loopbackHost(host) {
			logf("refused: Host %q is not loopback (%s %s)", r.Host, r.Method, r.URL.Path)
			http.Error(w, "forbidden: this server only answers on loopback", http.StatusForbidden)
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !loopbackHost(u.Hostname()) {
				logf("refused: Origin %q (%s %s)", origin, r.Method, r.URL.Path)
				http.Error(w, "forbidden: cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

type apiService struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Why      string   `json:"why"`
	Notes    []string `json:"notes"`
	Requires []string `json:"requires"`
	Running  bool     `json:"running"`
	Detail   string   `json:"detail"`
}

type apiPlugin struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	ConfigPath string `json:"config_path"`
	RootKey    string `json:"root_key"`
}

type apiServer struct {
	Transport    string   `json:"transport"`
	Command      string   `json:"command"`
	Args         []string `json:"args"`
	URL          string   `json:"url"`
	Why          string   `json:"why"`
	Notes        []string `json:"notes"`
	NeedsService string   `json:"needs_service"`
	Clients      []string `json:"clients"`
	Explicit     bool     `json:"clients_explicit"` // false = "all"
	Manager      bool     `json:"manager"`
	Direct       bool     `json:"direct"`
}

// apiProxy: the state of a server's proxy — the half an MCP client cannot show.
// Its light says "the manager is answering"; THIS says whether the upstream is
// really connected and how many tools have been seen.
type apiProxy struct {
	Connected bool     `json:"connected"`
	Tools     []string `json:"tools"`
	LastErr   string   `json:"last_error"`
}

func handleState(w http.ResponseWriter, e *Engine) {
	// Services, probed live — that is the whole point of the page: seeing the
	// REAL state, not the one we think we left behind.
	m := e.Manifest()
	svcs := make([]apiService, 0, len(m.Services))
	for i := range m.Services {
		s := m.Services[i]
		running, detail := e.Probe(&s)
		svcs = append(svcs, apiService{
			ID: s.ID, Label: s.Label, Why: s.Why, Notes: s.Notes,
			Requires: s.Requires, Running: running, Detail: detail,
		})
	}

	// Plugins = the known clients.
	plugins, err := LoadPlugins(applyDir())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	apiPlugs := make([]apiPlugin, 0, len(plugins))
	for _, p := range plugins {
		apiPlugs = append(apiPlugs, apiPlugin{ID: p.ID, Label: p.Label, ConfigPath: p.ConfigPath, RootKey: p.RootKey})
	}

	// Servers + matrix.
	inv, err := LoadInventory(InventoryPath())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	servers := map[string]apiServer{}
	matrix := map[string]map[string]bool{}
	for _, p := range plugins {
		matrix[p.ID] = map[string]bool{}
	}
	for name, s := range inv.Servers {
		servers[name] = apiServer{
			Transport: s.Transport, Command: s.Command, Args: s.Args, URL: s.URL,
			Why: s.Why, Notes: s.Notes, NeedsService: s.NeedsService,
			Clients: s.Clients, Explicit: s.Clients != nil,
			Manager: s.Manager, Direct: s.Direct,
		}
		for _, p := range plugins {
			matrix[p.ID][name] = s.AppliesTo(p.ID)
		}
	}

	// The proxy states: what the client's light does NOT say.
	proxies := map[string]apiProxy{}
	for name, b := range getProxies() {
		st := b.state()
		proxies[name] = apiProxy{Connected: st.Connected, Tools: st.Tools, LastErr: st.LastErr}
	}

	writeJSON(w, map[string]any{
		"services":  svcs,
		"servers":   servers,
		"plugins":   apiPlugs,
		"matrix":    matrix,
		"proxies":   proxies,
		"config":    e.Path,
		"inventory": "inventory.json",
	})
}

// handleService: POST /api/service/{id}/{start|stop}
func handleService(w http.ResponseWriter, r *http.Request, e *Engine) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("method expected: POST"))
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/service/"), "/")
	if len(parts) != 2 {
		writeErr(w, 400, fmt.Errorf("path expected: /api/service/<id>/<start|stop>"))
		return
	}
	id, verb := parts[0], parts[1]

	var msg string
	var err error
	switch verb {
	case "start":
		msg, err = e.Start(id)
	case "stop":
		msg, err = e.Stop(id)
	default:
		err = fmt.Errorf("unknown verb %q (start or stop)", verb)
	}
	if err != nil {
		logf("web %s %s: FAILURE — %v", verb, id, err)
		writeErr(w, 500, err)
		return
	}
	logf("web %s: %s", verb, msg)
	writeJSON(w, map[string]string{"message": msg})
}

// handleToggle: POST /api/toggle {client, server, on}
//
// Writes into inventory.json — IT is the source of truth, not the client's
// config. So we do not touch the client's file here: the user sees the
// intention, then decides to apply.
func handleToggle(w http.ResponseWriter, r *http.Request, e *Engine) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("method expected: POST"))
		return
	}
	var req struct {
		Client string `json:"client"`
		Server string `json:"server"`
		On     bool   `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	plugins, err := LoadPlugins(applyDir())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := setServerClient(InventoryPath(), req.Server, req.Client, req.On, plugins); err != nil {
		writeErr(w, 500, err)
		return
	}
	logf("web toggle: %s / %s = %v", req.Server, req.Client, req.On)
	writeJSON(w, map[string]string{"message": "inventory updated — remember to Apply"})
}

// handleServer: POST /api/server
//
//	{ "key": "",       "name": "vision", "spec": {…} }   creation
//	{ "key": "vision", "name": "vision", "spec": {…} }   modification
//	{ "key": "vision", "name": "vision2", "spec": {…} }  rename
//	{ "key": "vision", "delete": true }                  deletion
//
// key = the CURRENT name (the key in inventory.json), name = the WANTED name.
// Separating them is what makes renaming possible without a special case: we
// delete the old key and write the new one.
//
// This is the "configuration" half of the tool: the inventory is the source of
// truth, and from here on a human can maintain all of it from the page.
func handleServer(w http.ResponseWriter, r *http.Request, e *Engine) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("method expected: POST"))
		return
	}
	var req struct {
		Key    string           `json:"key"`
		Name   string           `json:"name"`
		Delete bool             `json:"delete"`
		Spec   *InventoryServer `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	key := strings.TrimSpace(req.Key)
	name := strings.TrimSpace(req.Name)

	plugins, err := LoadPlugins(applyDir())
	if err != nil {
		writeErr(w, 500, err)
		return
	}

	// We re-read the RAW file (not the typed structure): the inventory contains
	// a $comment block that documents the format, and re-reading it into a
	// structure would make it disappear on the first write.
	invPath := InventoryPath()
	raw, err := os.ReadFile(invPath)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		writeErr(w, 500, fmt.Errorf("unreadable inventory.json: %w", err))
		return
	}
	servers, ok := doc["servers"].(map[string]any)
	if !ok {
		writeErr(w, 500, fmt.Errorf("inventory.json: 'servers' key missing or invalid"))
		return
	}

	var msg string
	if req.Delete {
		if key == "" {
			writeErr(w, 400, fmt.Errorf("deletion: the server name ('key') is required"))
			return
		}
		if key == "system" {
			writeErr(w, 400, fmt.Errorf("'system' is the manager itself: it does not delete itself. "+
				"Uncheck it in the matrix if you no longer want a client to see it"))
			return
		}
		if _, exists := servers[key]; !exists {
			writeErr(w, 404, fmt.Errorf("server unknown to the inventory: %q", key))
			return
		}
		delete(servers, key)
		msg = fmt.Sprintf("server %q deleted from the inventory", key)
	} else {
		if req.Spec == nil {
			writeErr(w, 400, fmt.Errorf("body without 'spec': nothing to write"))
			return
		}
		if err := validateServerSpec(name, *req.Spec, e, plugins); err != nil {
			writeErr(w, 400, err)
			return
		}
		if key != "" && key != name {
			if _, exists := servers[key]; !exists {
				writeErr(w, 404, fmt.Errorf("server to rename unknown: %q", key))
				return
			}
			delete(servers, key)
			msg = fmt.Sprintf("server %q renamed to %q", key, name)
		} else if key == "" {
			if _, exists := servers[name]; exists {
				msg = fmt.Sprintf("server %q updated", name)
			} else {
				msg = fmt.Sprintf("server %q added to the inventory", name)
			}
		} else {
			msg = fmt.Sprintf("server %q updated", name)
		}
		servers[name] = serverToMap(*req.Spec)
	}

	encoded, err := marshalDoc(doc)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := os.WriteFile(invPath, encoded, 0o644); err != nil {
		writeErr(w, 500, err)
		return
	}
	logf("web server: %s", msg)

	// The proxy follows immediately: a server just added is reachable at
	// /mcp/<name> without restarting the manager.
	if err := ReloadProxies(e); err != nil {
		writeJSON(w, map[string]string{
			"message": msg + " — but the proxies could not be rebuilt: " + err.Error(),
		})
		return
	}
	writeJSON(w, map[string]string{"message": msg + " — remember to Apply to write it to the clients"})
}

// handleReload: POST /api/reload — re-read services.json and inventory.json
// without restarting. This is what makes the manifest hand-editable: you write
// the JSON, you click, and the probe and the colors follow.
func handleReload(w http.ResponseWriter, r *http.Request, e *Engine) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("method expected: POST"))
		return
	}
	// We note the LIST of services before/after: the tray menu is built once at
	// startup and systray does not know how to remove an entry. A service added
	// is therefore usable right away (web, CLI, MCP) but will not appear in the
	// menu until the manager has restarted — better to say so than to let you
	// believe everything followed.
	before := map[string]bool{}
	for _, s := range e.Manifest().Services {
		before[s.ID] = true
	}

	if err := ReloadManifest(e); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := ReloadProxies(e); err != nil {
		writeErr(w, 500, err)
		return
	}
	logf("web reload: manifest and inventory re-read")

	added := []string{}
	for _, s := range e.Manifest().Services {
		if !before[s.ID] {
			added = append(added, s.ID)
		}
	}
	msg := "services.json and inventory.json re-read"
	if len(added) > 0 {
		msg += fmt.Sprintf(" — new service(s): %s. Usable right away here, "+
			"but the notification-area menu will only show them after a restart of the manager",
			strings.Join(added, ", "))
	}
	writeJSON(w, map[string]string{"message": msg})
}

// serverToMap translates the typed structure into a JSON object, OMITTING the
// empty fields.
//
// Why not json.Marshal directly: it would write "clients": null,
// "manager": false, "args": null into the file. inventory.json is read and
// edited by a human — it must stay as clean as what we put in it.
func serverToMap(s InventoryServer) map[string]any {
	m := map[string]any{"transport": s.Transport}
	if s.Command != "" {
		m["command"] = s.Command
	}
	if len(s.Args) > 0 {
		m["args"] = s.Args
	}
	if s.URL != "" {
		m["url"] = s.URL
	}
	if s.Why != "" {
		m["why"] = s.Why
	}
	if len(s.Notes) > 0 {
		m["notes"] = s.Notes
	}
	if s.NeedsService != "" {
		m["needs_service"] = s.NeedsService
	}
	// nil = absent = ALL. An empty list is a REAL value ("none") and must
	// therefore survive the write — that is the whole point of the distinction.
	if s.Clients != nil {
		m["clients"] = s.Clients
	}
	if s.Manager {
		m["manager"] = true
	}
	if s.Direct {
		m["direct"] = true
	}
	return m
}

// validateServerSpec refuses at write time what would break later in silence.
//
// Each check targets a failure already encountered: a misspelled prerequisite
// never starts, a misspelled client excludes the server everywhere, a name with
// a slash breaks the /mcp/<name> route.
func validateServerSpec(name string, s InventoryServer, e *Engine, plugins []*Plugin) error {
	if name == "" {
		return fmt.Errorf("the name is required")
	}
	for _, r := range name {
		ok := r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("the name %q contains a forbidden character: letters, digits, hyphen, dot and underscore only "+
				"(the name becomes a JSON key and a piece of URL)", name)
		}
	}

	switch s.Transport {
	case "stdio":
		if s.Command == "" {
			return fmt.Errorf("transport 'stdio': the command is required")
		}
	case "http":
		if s.URL == "" {
			return fmt.Errorf("transport 'http': the URL is required")
		}
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			return fmt.Errorf("the URL must start with http:// or https:// (got %q)", s.URL)
		}
	default:
		return fmt.Errorf("unknown transport %q — expected 'stdio' or 'http'", s.Transport)
	}

	known := map[string]bool{}
	for _, p := range plugins {
		known[p.ID] = true
	}
	for _, c := range s.Clients {
		if !known[c] {
			return fmt.Errorf("unknown client %q — known: %s", c, strings.Join(pluginIDs(plugins), ", "))
		}
	}

	if s.NeedsService != "" {
		if _, err := e.Find(s.NeedsService); err != nil {
			return fmt.Errorf("service unknown %q as prerequisite — a misspelled prerequisite would never start", s.NeedsService)
		}
	}
	return nil
}

// handleApply: POST /api/apply {"write": true|false}
func handleApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("method expected: POST"))
		return
	}
	var req struct {
		Write bool `json:"write"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	inv, err := LoadInventory(InventoryPath())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	var buf strings.Builder
	if err := applyTo(&buf, inv, req.Write); err != nil {
		writeErr(w, 500, err)
		return
	}
	logf("web apply (write=%v)", req.Write)
	writeJSON(w, map[string]string{"report": buf.String()})
}

// ---------------------------------------------------------------------------
// Writing the toggles into inventory.json
// ---------------------------------------------------------------------------

// setServerClient updates a server's "clients" list and rewrites only the
// inventory.json file — preserving the other root keys, including the $comment
// block that documents the format.
//
// Semantics: absent field = all clients; empty list = none. Unchecking a client
// when the server targeted "all" therefore materialises the full list MINUS that
// client — otherwise "none" would be re-read as "all".
func setServerClient(invPath, server, client string, on bool, plugins []*Plugin) error {
	raw, err := os.ReadFile(invPath)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	servers, ok := doc["servers"].(map[string]any)
	if !ok {
		return fmt.Errorf("inventory.json: 'servers' key missing or invalid")
	}
	entry, ok := servers[server].(map[string]any)
	if !ok {
		return fmt.Errorf("server unknown to the inventory: %q", server)
	}

	all := make([]string, 0, len(plugins))
	for _, p := range plugins {
		all = append(all, p.ID)
	}
	sort.Strings(all)

	// Current effective list.
	var cur []string
	if v, ok := entry["clients"]; ok && v != nil {
		arr, _ := v.([]any)
		for _, x := range arr {
			if s, ok := x.(string); ok {
				cur = append(cur, s)
			}
		}
	} else {
		cur = append(cur, all...) // absent = all
	}

	// Applying the toggle.
	set := map[string]bool{}
	for _, c := range cur {
		set[c] = true
	}
	if on {
		set[client] = true
	} else {
		delete(set, client)
	}
	out := make([]string, 0, len(set))
	for _, c := range all {
		if set[c] {
			out = append(out, c)
		}
	}
	// If the list already covers everyone, we revert to the default (absent field)
	// rather than writing an identical explicit list. That keeps inventory.json
	// readable: a list only appears when it really RESTRICTS.
	if len(out) == len(all) {
		delete(entry, "clients")
	} else {
		entry["clients"] = out
	}

	encoded, err := marshalDoc(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(invPath, encoded, 0o644)
}

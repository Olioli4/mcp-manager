package main

// The proxy: the manager becomes the TRANSPORT of the MCP servers.
//
// Why: a declared prerequisite must not depend on an agent's memory. The request
// arrives HERE — so it is HERE that we notice the service is off, that we start
// it, and that we forward. The agent has nothing to know about it.
//
// Each inventory server is exposed on /mcp/<name>:
//
//	client  ->  manager  ->  upstream (http or stdio)
//
// And the tool list is CACHED: it is therefore visible even when the upstream is
// off. That is the useful half of a hub — without a container.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type backend struct {
	name string
	spec InventoryServer
	eng  *Engine

	// srv is read by the HTTP manager on EVERY request and rewritten at each
	// refresh of the tool list: it is a concurrent access, therefore an atomic
	// pointer, not a bare field. (A bare field would be a race — the kind that
	// only shows up once in a thousand, in production.)
	srv atomic.Pointer[mcp.Server]

	connMu  sync.Mutex // held during connect/start to prevent concurrent connections
	mu      sync.Mutex // held only for fast state updates
	session *mcp.ClientSession
	tools   []*mcp.Tool // last known — the cache
	handler http.Handler
	lastErr string

	// Lifecycle: we do NOT keep the upstream connected permanently. Connection on
	// first request, disconnection after inactivity. Otherwise the manager would
	// run `npx @playwright/mcp` and `node aas-mcp.js` permanently — exactly the
	// "it runs even though I asked for nothing" that was refused.
	idleTimer *time.Timer
	inFlight  int
}

func newBackend(eng *Engine, name string, spec InventoryServer) *backend {
	b := &backend{name: name, spec: spec, eng: eng}
	// The disk cache: the tools are visible from startup, without a connection
	// and therefore without launching a single process.
	b.tools = loadToolCache(name)
	b.rebuildServer()
	// keepaliveHandler: the SDK alone would let an idle GET stream fall silent,
	// and clients time it out (Phase 6 — see sse.go).
	b.handler = keepaliveHandler(func(*http.Request) *mcp.Server { return b.srv.Load() })
	return b
}

// rebuildServer rebuilds the local server exposing the known tools.
// Called every time the list changes. A tool whose upstream is off stays
// exposed: that is the whole point of the cache.
func (b *backend) rebuildServer() {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    b.name,
		Version: "proxy",
	}, &mcp.ServerOptions{
		Instructions: b.spec.Why + notesText(b.spec.Notes),
	})
	for _, t := range b.tools {
		tool := t
		s.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return b.forward(ctx, req.Params.Name, req.Params.Arguments)
		})
	}
	b.srv.Store(s)
}

// ensureUpstream makes sure the upstream is reachable — and this is THE point
// the user wanted: if a service declared as a prerequisite is off, we start it
// HERE, at the moment a request arrives.
//
// On success it returns the session ALREADY BORROWED: the call is counted in
// flight in the same critical section that publishes the session, and the caller
// must answer with leave(). That borrowing is not a detail — it is the design.
// A function that only said "ok" would let the caller fetch b.session a moment
// later, and in that moment the idle timer closes it: dereferencing the result is
// the H3 crash. Here there is no moment, so there is nothing to check.
func (b *backend) ensureUpstream(ctx context.Context, mayStart bool) (*mcp.ClientSession, error) {
	b.mu.Lock()
	if b.session != nil {
		sess := b.session
		b.borrowLocked()
		b.mu.Unlock()
		return sess, nil
	}
	b.mu.Unlock()

	b.connMu.Lock()
	defer b.connMu.Unlock()

	b.mu.Lock()
	if b.session != nil {
		sess := b.session
		b.borrowLocked()
		b.mu.Unlock()
		return sess, nil
	}
	b.mu.Unlock()

	// 1. The declared prerequisite — only if a request demands it.
	if mayStart && b.spec.NeedsService != "" {
		svc, err := b.eng.Find(b.spec.NeedsService)
		if err == nil {
			if running, _ := b.eng.Probe(svc); !running {
				logf("proxy %s: request received, service %q off — starting", b.name, b.spec.NeedsService)
				if _, err := b.eng.Start(b.spec.NeedsService); err != nil {
					b.mu.Lock()
					b.lastErr = err.Error()
					b.mu.Unlock()
					return nil, fmt.Errorf("prerequisite %q: %w", b.spec.NeedsService, err)
				}
			}
		}
	}

	// 2. The connection to the upstream.
	sess, err := b.connect(ctx)
	if err != nil {
		b.mu.Lock()
		b.lastErr = err.Error()
		b.mu.Unlock()
		return nil, err
	}

	// Publish it AND borrow it in one move: from this line on, the idle timer
	// cannot close the session out from under the call that is about to use it.
	b.mu.Lock()
	b.session = sess
	b.borrowLocked()
	b.mu.Unlock()

	// 3. Refresh the tool cache.
	if lt, err := sess.ListTools(ctx, nil); err == nil && len(lt.Tools) > 0 {
		b.mu.Lock()
		b.tools = lt.Tools
		b.mu.Unlock()

		saveToolCache(b.name, lt.Tools)
		b.rebuildServer()
		logf("proxy %s: %d tool(s) cached", b.name, len(lt.Tools))
	}

	return sess, nil
}

func (b *backend) connect(ctx context.Context) (*mcp.ClientSession, error) {
	cli := mcp.NewClient(&mcp.Implementation{Name: "mcp-manager", Version: "1.0.0"}, nil)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch b.spec.Transport {
	case "http":
		tr := &mcp.StreamableClientTransport{
			Endpoint:   b.spec.URL,
			MaxRetries: 0, // we do not want 5 silent attempts: we want the error fast
		}
		if len(b.spec.Headers) > 0 {
			// StreamableClientTransport offers no header field, but it takes our
			// http.Client — and a client is exactly a RoundTripper with retries
			// around it. The RoundTripper stamps every request (POSTs, GET-SSE,
			// the standalone stream) with the inventory's headers, resolving
			// ${NAME} at the moment of use.
			tr.HTTPClient = &http.Client{
				Transport: &headerRoundTripper{
					base:    http.DefaultTransport,
					headers: b.spec.Headers,
				},
			}
		}
		return cli.Connect(cctx, tr, nil)
	case "stdio":
		args := b.spec.Args
		cmd := exec.Command(b.spec.Command, args...)
		// The tray is built with -H=windowsgui: it has no console, so every
		// stdio server we spawn (npx, node) gets a VISIBLE CONSOLE WINDOW each
		// time the idle timer releases it and a new request reconnects. On a
		// machine with playwright + aas + openai-agents proxied, that is three
		// black windows appearing at random while the user works.
		//
		// Same attribute engine.go uses: CREATE_NO_WINDOW gives the process a
		// console (stdio pipes work, measured) that is never shown; HideWindow
		// covers launchers like npx.cmd that create a window before their own
		// flags apply.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: 0x08000000, // CREATE_NO_WINDOW
		}
		if len(b.spec.Env) > 0 {
			// The inventory stores variable NAMES; the environment holds the
			// values. A missing reference refuses the launch with the name
			// named — "the server started and the API key silently defaulted to
			// empty" is the failure nobody debugs.
			pairs, err := resolveEnvPairs(b.spec.Env)
			if err != nil {
				return nil, fmt.Errorf("stdio %s: %w", b.name, err)
			}
			cmd.Env = append(os.Environ(), pairs...)
		}
		// jobbedTransport assigns the child to the kill-on-exit Job Object
		// (job_windows.go, M9) the instant its PID exists: when the manager
		// exits, the kernel kills npx, node, and everything they spawned.
		return cli.Connect(cctx, jobbedTransport{&mcp.CommandTransport{
			Command: cmd,
		}}, nil)
	default:
		return nil, fmt.Errorf("unknown transport %q", b.spec.Transport)
	}
}

// secretRef is the whole of the secrets grammar (M6): ${NAME} inside a value
// means "the value of the environment variable NAME, right now". Everything
// else is literal. The inventory therefore never contains a secret, a client's
// config file never receives one, and the only copy of the key stays where the
// user put it — in the environment.
var secretRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// resolveSecret expands every ${NAME} in v from the environment. A reference
// to an unset variable is an ERROR, never an empty string: a server that
// starts with an empty API key fails in ways that point at everything except
// the missing variable.
func resolveSecret(v string) (string, error) {
	var missing []string
	out := secretRef.ReplaceAllStringFunc(v, func(m string) string {
		name := secretRef.FindStringSubmatch(m)[1]
		val := os.Getenv(name)
		if val == "" {
			missing = append(missing, name)
			return m
		}
		return val
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("environment variable(s) not set: %s — the inventory stores variable names, the secret itself belongs in the manager's environment", strings.Join(missing, ", "))
	}
	return out, nil
}

// resolveEnvPairs turns the env map into "KEY=value" lines with every ${NAME}
// expanded. Keys are sorted so a repeated launch builds the same environment
// (a stable byte sequence is what makes a diff of two runs readable).
func resolveEnvPairs(env map[string]string) ([]string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v, err := resolveSecret(env[k])
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

// headerRoundTripper stamps the inventory's headers onto every HTTP request
// the SDK's transport makes. The RoundTripper contract forbids modifying the
// request handed to it, so each round trip works on a clone.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		resolved, err := resolveSecret(v)
		if err != nil {
			return nil, fmt.Errorf("header %q: %w", k, err)
		}
		clone.Header.Set(k, resolved)
	}
	return t.base.RoundTrip(clone)
}

// borrowLocked counts a call on the session and stops the idle timer. It must be
// called with b.mu held, in the same critical section that publishes the session:
// that is what makes "the timer closed it between the check and the call"
// unrepresentable. The caller owes exactly one leave().
func (b *backend) borrowLocked() {
	b.inFlight++
	if b.idleTimer != nil {
		b.idleTimer.Stop()
		b.idleTimer = nil
	}
}

// leave releases the session, decrementing the in-flight counter and re-arming
// the idle timer if no other calls are in flight.
func (b *backend) leave() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inFlight > 0 {
		b.inFlight--
	}
	if b.inFlight == 0 && b.session != nil {
		if b.idleTimer != nil {
			b.idleTimer.Stop()
		}
		b.idleTimer = time.AfterFunc(idleTimeout(), b.idleTimeoutFunc)
	}
}

func (b *backend) idleTimeoutFunc() {
	b.mu.Lock()
	s := b.session
	b.session = nil
	b.idleTimer = nil
	b.mu.Unlock()
	if s != nil {
		_ = s.Close()
		logf("proxy %s: idle — disconnected (the tools stay cached)", b.name)
	}
}

// forward routes a call. This is where "the request arrives and the service is
// off" gets resolved: we start, then we forward.
func (b *backend) forward(ctx context.Context, tool string, args json.RawMessage) (*mcp.CallToolResult, error) {
	// ensureUpstream hands back a session that is ALREADY BORROWED: this call is
	// counted in flight from the moment the session exists, so there is no window
	// where the idle timer closes it and the call dereferences a closed session.
	sess, err := b.ensureUpstream(ctx, true)
	if err != nil {
		// We return a READABLE error, not a transport failure: the caller must
		// understand what is missing.
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
				"server %q unreachable: %v", b.name, err)}},
		}, nil
	}
	defer b.leave()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		// The session may be dead: we drop it so that the next call retries a
		// clean connection, and we say so.
		b.dropSession()
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
				"call to %s on %q failed: %v — retry, the connection has been remade", tool, b.name, err)}},
		}, nil
	}
	return res, nil
}

// dropSession closes the current session (under lock).
func (b *backend) dropSession() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != nil {
		_ = b.session.Close()
		b.session = nil
	}
}

// idleTimeout: delay before disconnecting an unused upstream.
func idleTimeout() time.Duration {
	if v := os.Getenv("MCPMGR_IDLE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Minute
}

// warm attempts a connection in the background, to populate the cache without
// making the user wait.
//
// It is NOT called at startup: connecting to the stdio upstreams would launch
// `npx @playwright/mcp` and `node aas-mcp.js` permanently, when nobody asked for
// anything. The visibility of the tools comes from the disk cache, not from a
// permanent connection.
//
// mayStart = FALSE: preheating CONNECTS, it does not START anything.
func (b *backend) warm() {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	_, err := b.ensureUpstream(ctx, false)
	if err != nil {
		logf("proxy %s: not reachable yet (normal if its service is off)", b.name)
		return
	}
	// The preheating borrowed the session on ensureUpstream's terms; it is not a
	// call in flight, so hand it back and let the idle timer do its work.
	b.leave()
}

// ---------------------------------------------------------------------------
// Disk cache of the tool lists
// ---------------------------------------------------------------------------

func cacheDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "cache")
	}
	return "cache"
}

func cachePath(name string) string {
	return filepath.Join(cacheDir(), name+".tools.json")
}

func loadToolCache(name string) []*mcp.Tool {
	raw, err := os.ReadFile(cachePath(name))
	if err != nil {
		return nil
	}
	var tools []*mcp.Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	return tools
}

func saveToolCache(name string, tools []*mcp.Tool) {
	if err := os.MkdirAll(cacheDir(), 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(cachePath(name), raw, 0o644)
}

func notesText(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return "\n\nGood to know:\n  · " + strings.Join(notes, "\n  · ")
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// ReloadProxies re-reads the inventory and rebuilds the proxies. Called at
// startup, and after each edit of the inventory from the interface — this is
// what makes an added server reachable without a restart.
//
// mergeProxies decides what is rebuilt (see it for the rule); only the servers
// whose definition REALLY changed lose their upstream session. A save that
// changed nothing else must not disconnect the server the user is talking to:
// dropping every session on every reload used to cut the live connection, drop
// the tool cache, and force a reconnect (with a stdio relaunch of npx) for an
// edit of a completely unrelated server.
func ReloadProxies(e *Engine) error {
	inv, err := LoadInventory(InventoryPath())
	if err != nil {
		return err
	}
	old := getProxies()
	next, dropped := mergeProxies(old, buildProxies(e, inv))

	for _, b := range dropped {
		b.dropSession()
	}
	setProxies(next)

	// Warm up newly added servers that have an empty cache.
	for name, b := range next {
		if old[name] == nil {
			b.mu.Lock()
			noTools := len(b.tools) == 0
			b.mu.Unlock()
			if noTools {
				go b.warm()
			}
		}
	}

	logf("proxy: %d server(s) exposed", len(next))
	return nil
}

// mergeProxies is the registry diff, pure: it takes the old registry and the
// freshly built one, and returns the registry to install plus the backends whose
// sessions must be closed. No globals, no locks, no side effects — the caller
// performs every effect. That is what makes the rule testable without an
// inventory file, an engine, or a network: InventoryPath() is not injectable.
//
// The rule, one name at a time:
//
//   - new name            -> install the fresh backend (the caller warms it)
//   - same definition     -> KEEP THE OLD backend: its live session and its tool
//     cache survive. The definitions are compared as MARSHALLED BYTES of the
//     spec, so field order, formatting and key order in the file cannot matter,
//     and a reload after an unrelated edit is a no-op for this server.
//   - changed definition  -> install the fresh backend, drop the old one's
//     session: the URL may point elsewhere now, staying connected to the old
//     target would be answering from the wrong machine.
//   - name gone from file -> dropped: close its session and remove it.
func mergeProxies(old, fresh map[string]*backend) (next map[string]*backend, dropped []*backend) {
	next = make(map[string]*backend, len(fresh))
	for name, fb := range fresh {
		ob, existed := old[name]
		if existed && sameSpec(ob.spec, fb.spec) {
			next[name] = ob
			continue
		}
		next[name] = fb
	}
	// Everything the loop above did not keep — changed, or gone from the file.
	for name, ob := range old {
		if next[name] != ob {
			dropped = append(dropped, ob)
		}
	}
	return next, dropped
}

// sameSpec compares two server definitions by their marshalled bytes. Marshal
// sorts map keys, so the comparison is deterministic; a cosmetic change in
// inventory.json (key order, indentation) never counts as a change.
func sameSpec(a, b InventoryServer) bool {
	ja, ea := json.Marshal(a)
	jb, eb := json.Marshal(b)
	if ea != nil || eb != nil {
		return false // unreadable spec: treat as changed, rebuild
	}
	return bytes.Equal(ja, jb)
}

// buildProxies builds one proxy per inventory server (the manager itself
// excepted: it does not need to be proxied).
func buildProxies(e *Engine, inv *Inventory) map[string]*backend {
	out := map[string]*backend{}
	for name, spec := range inv.Servers {
		if name == "system" || spec.Manager {
			continue
		}
		out[name] = newBackend(e, name, spec)
	}
	return out
}

// Global registry: it lets the web interface show the state of each proxy
// (connected or not), and rebuild everything after an edit of the inventory —
// without restarting the manager.
var (
	proxiesMu  sync.RWMutex
	proxiesReg = map[string]*backend{}
)

func setProxies(m map[string]*backend) {
	proxiesMu.Lock()
	defer proxiesMu.Unlock()
	proxiesReg = m
}

func getProxies() map[string]*backend {
	proxiesMu.RLock()
	defer proxiesMu.RUnlock()
	return proxiesReg
}

// proxyState: what the interface needs to know about a proxy.
type proxyState struct {
	Connected bool     `json:"connected"`
	Tools     []string `json:"tools"`
	LastErr   string   `json:"last_error"`
}

func (b *backend) state() proxyState {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.tools))
	for _, t := range b.tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return proxyState{Connected: b.session != nil, Tools: names, LastErr: b.lastErr}
}

// CloseAll cleanly closes all the upstream sessions (manager shutdown).
func CloseAll() {
	for _, b := range getProxies() {
		b.dropSession()
	}
}

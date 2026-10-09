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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	mu      sync.Mutex
	session *mcp.ClientSession
	tools   []*mcp.Tool // last known — the cache
	handler *mcp.StreamableHTTPHandler
	lastErr string

	// Lifecycle: we do NOT keep the upstream connected permanently. Connection on
	// first request, disconnection after inactivity. Otherwise the manager would
	// run `npx @playwright/mcp` and `node aas-mcp.js` permanently — exactly the
	// "it runs even though I asked for nothing" that was refused.
	idleTimer *time.Timer
}

func newBackend(eng *Engine, name string, spec InventoryServer) *backend {
	b := &backend{name: name, spec: spec, eng: eng}
	// The disk cache: the tools are visible from startup, without a connection
	// and therefore without launching a single process.
	b.tools = loadToolCache(name)
	b.rebuildServer()
	b.handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return b.srv.Load() }, nil)
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
// We NEVER start outside this path. Preheating is content to connect: launching
// ComfyUI or Blender at manager startup would be exactly the automatic start the
// user refused.
func (b *backend) ensureUpstream(ctx context.Context, mayStart bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != nil {
		return nil
	}

	// 1. The declared prerequisite — only if a request demands it.
	if mayStart && b.spec.NeedsService != "" {
		svc, err := b.eng.Find(b.spec.NeedsService)
		if err == nil {
			if running, _ := b.eng.Probe(svc); !running {
				logf("proxy %s: request received, service %q off — starting", b.name, b.spec.NeedsService)
				if _, err := b.eng.Start(b.spec.NeedsService); err != nil {
					b.lastErr = err.Error()
					return fmt.Errorf("prerequisite %q: %w", b.spec.NeedsService, err)
				}
			}
		}
	}

	// 2. The connection to the upstream.
	sess, err := b.connect(ctx)
	if err != nil {
		b.lastErr = err.Error()
		return err
	}
	b.session = sess

	// 3. Refresh the tool cache.
	if lt, err := sess.ListTools(ctx, nil); err == nil && len(lt.Tools) > 0 {
		b.tools = lt.Tools
		saveToolCache(b.name, lt.Tools)
		b.rebuildServer()
		logf("proxy %s: %d tool(s) cached", b.name, len(lt.Tools))
	}
	return nil
}

func (b *backend) connect(ctx context.Context) (*mcp.ClientSession, error) {
	cli := mcp.NewClient(&mcp.Implementation{Name: "mcp-manager", Version: "1.0.0"}, nil)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch b.spec.Transport {
	case "http":
		return cli.Connect(cctx, &mcp.StreamableClientTransport{
			Endpoint:   b.spec.URL,
			MaxRetries: 0, // we do not want 5 silent attempts: we want the error fast
		}, nil)
	case "stdio":
		args := b.spec.Args
		return cli.Connect(cctx, &mcp.CommandTransport{
			Command: exec.Command(b.spec.Command, args...),
		}, nil)
	default:
		return nil, fmt.Errorf("unknown transport %q", b.spec.Transport)
	}
}

// forward routes a call. This is where "the request arrives and the service is
// off" gets resolved: we start, then we forward.
func (b *backend) forward(ctx context.Context, tool string, args json.RawMessage) (*mcp.CallToolResult, error) {
	if err := b.ensureUpstream(ctx, true); err != nil {
		// We return a READABLE error, not a transport failure: the caller must
		// understand what is missing.
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
				"server %q unreachable: %v", b.name, err)}},
		}, nil
	}

	b.mu.Lock()
	sess := b.session
	b.mu.Unlock()

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
	b.armIdle()
	return res, nil
}

// armIdle postpones the disconnection. Called after each successful call: as long
// as we are using the server, we keep it warm.
func (b *backend) armIdle() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.idleTimer != nil {
		b.idleTimer.Stop()
	}
	b.idleTimer = time.AfterFunc(idleTimeout(), func() {
		b.mu.Lock()
		s := b.session
		b.session = nil
		b.idleTimer = nil
		b.mu.Unlock()
		if s != nil {
			_ = s.Close()
			logf("proxy %s: idle — disconnected (the tools stay cached)", b.name)
		}
	})
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
	if err := b.ensureUpstream(ctx, false); err != nil {
		logf("proxy %s: not reachable yet (normal if its service is off)", b.name)
	}
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

// ReloadProxies re-reads the inventory and rebuilds all the proxies. Called at
// startup, and after each edit of the inventory from the interface — this is
// what makes an added server reachable without a restart.
//
// The upstream sessions in progress are closed: a server whose URL we have just
// changed must not stay connected to the old one.
func ReloadProxies(e *Engine) error {
	inv, err := LoadInventory(InventoryPath())
	if err != nil {
		return err
	}
	fresh := buildProxies(e, inv)

	old := getProxies()
	for _, b := range old {
		b.dropSession()
	}
	setProxies(fresh)
	logf("proxy: %d server(s) exposed", len(fresh))
	return nil
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

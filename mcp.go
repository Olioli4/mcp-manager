package main

// MCP surface of the manager.
//
// Why: without it, the concierge is usable only by a human who clicks. With it,
// an AGENT can see what exists, notice that a service is off, and start it —
// instead of receiving an opaque ECONNREFUSED.
//
// It is also where the KNOWLEDGE lives: each response carries the "why" and the
// pitfalls of the manifest. The information therefore travels with the response,
// instead of staying in a client's private memory.
//
// Served on the same port as the web interface, path /mcp.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newMCPServer(e *Engine) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "system",
		Version: "1.0.0",
	}, &mcp.ServerOptions{
		Instructions: "Manager of this machine's programs. " +
			"services_list says what is running and WHY each program exists; " +
			"service_start starts it and waits for it to answer. " +
			"Some MCP servers (Blender, ComfyUI) exist ONLY if their application is running: " +
			"start it first, the tools will appear afterwards.",
	})

	// --- services_list -----------------------------------------------------
	mcp.AddTool(s, &mcp.Tool{
		Name: "services_list",
		Description: "Lists the managed programs, their REAL state (probed right now), " +
			"the reason each one exists, its prerequisites and its pitfalls.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		var b strings.Builder
		up := 0
		man := e.Manifest()
		for i := range man.Services {
			svc := man.Services[i]
			running, detail := e.Probe(&svc)
			if running {
				up++
			}
			mark := "○ stopped"
			if running {
				mark = "● running"
			}
			fmt.Fprintf(&b, "%s  %-10s %-28s %s\n", mark, svc.ID, svc.Label, detail)
			fmt.Fprintf(&b, "    why      : %s\n", svc.Why)
			if len(svc.Requires) > 0 {
				fmt.Fprintf(&b, "    requires : %s\n", strings.Join(svc.Requires, ", "))
			}
			for _, n := range svc.Notes {
				fmt.Fprintf(&b, "    · %s\n", n)
			}
		}
		fmt.Fprintf(&b, "\n%d/%d running.\n", up, len(man.Services))
		return textResult(b.String()), nil, nil
	})

	// --- servers_list ------------------------------------------------------
	// What the inventory declares, independently of the state: an agent must be
	// able to know that a server EXISTS even when it is not reachable.
	mcp.AddTool(s, &mcp.Tool{
		Name: "servers_list",
		Description: "Lists the MCP servers declared in the inventory: transport, reason for being, " +
			"pitfalls, the service they depend on, and the clients that receive them. " +
			"Useful when a server is unreachable: it says whether it exists and what it is missing.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		inv, err := LoadInventory(InventoryPath())
		if err != nil {
			return errResult(err), nil, nil
		}
		plugins, _ := LoadPlugins(applyDir())
		var b strings.Builder
		names := make([]string, 0, len(inv.Servers))
		for n := range inv.Servers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			sv := inv.Servers[n]
			target := "all clients"
			if sv.Clients != nil {
				if len(sv.Clients) == 0 {
					target = "no client"
				} else {
					target = strings.Join(sv.Clients, ", ")
				}
			}
			fmt.Fprintf(&b, "%s  (%s)\n", n, sv.Transport)
			fmt.Fprintf(&b, "    why      : %s\n", sv.Why)
			fmt.Fprintf(&b, "    clients  : %s\n", target)
			if sv.NeedsService != "" {
				state := "?"
				if svc, err := e.Find(sv.NeedsService); err == nil {
					if running, detail := e.Probe(svc); running {
						state = "running — " + detail
					} else {
						state = "STOPPED — " + detail
					}
				}
				fmt.Fprintf(&b, "    requires service %q : %s\n", sv.NeedsService, state)
			}
			for _, note := range sv.Notes {
				fmt.Fprintf(&b, "    · %s\n", note)
			}
		}
		if len(plugins) > 0 {
			ids := pluginIDs(plugins)
			fmt.Fprintf(&b, "\nKnown clients: %s\n", strings.Join(ids, ", "))
		}
		return textResult(b.String()), nil, nil
	})

	// --- service_start / service_stop --------------------------------------
	type svcArg struct {
		ID string `json:"id" jsonschema:"identifier of the service, as returned by services_list"`
	}

	mcp.AddTool(s, &mcp.Tool{
		Name: "service_start",
		Description: "Starts a program and waits for its probe to answer (up to the manifest's timeout). " +
			"Checks its prerequisites first and says which are missing, rather than failing obscurely.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a svcArg) (*mcp.CallToolResult, any, error) {
		msg, err := e.Start(a.ID)
		if err != nil {
			return errResult(err), nil, nil
		}
		return textResult(msg), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_stop",
		Description: "Stops a program and waits for its probe to confirm it has gone away.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a svcArg) (*mcp.CallToolResult, any, error) {
		msg, err := e.Stop(a.ID)
		if err != nil {
			return errResult(err), nil, nil
		}
		return textResult(msg), nil, nil
	})

	return s
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// mcpHTTPHandler makes the manager reachable over MCP on the same port as the
// web interface, path /mcp. A local client sends Host: 127.0.0.1 — the SDK's
// anti-rebinding protection therefore lets it through, and nothing is exposed to
// the network since the server listens only on the loopback.
func mcpHTTPHandler(e *Engine) http.Handler {
	srv := newMCPServer(e)
	return keepaliveHandler(func(*http.Request) *mcp.Server { return srv })
}

// errResult returns an EXPLICIT error to the model, not a transport failure: the
// agent must be able to read "X is missing" and act.
func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

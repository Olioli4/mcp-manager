package main

// Import: the REVERSE of Render.
//
// The first install replaces a client's MCP block with the inventory, which
// means a user who already had servers configured in Claude Code, opencode or
// VS Code loses them on the first apply. Import reads what each client already
// has, recognises the entries that are OURS (the manager and everything routed
// through it) and offers the rest as inventory entries.
//
// Two promises this file keeps, and they are the whole point:
//
//   - Nothing is written without --write. The default is a preview.
//   - A name that already exists in the inventory is REPORTED, never
//     overwritten. The client's copy is a guess; the inventory is the truth.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// importNote is stamped on every imported server. A client's config stores no
// "why" — that field belongs to the inventory alone — so the origin is recorded
// where it can help: in the notes, which travel with the server to every agent.
func importNote(p *Plugin) string {
	return "imported from " + p.Label + " — why and notes live only in the inventory."
}

// importServer is Render in reverse: a client's config block becomes an
// inventory server. ok=false means "skip this entry" — it is ours, or its shape
// is not something we can read back.
//
// The three traps, each mirrored from a branch of Render:
//
//  1. Unwrap our own proxy. A client with proxy_base talks to the manager, so
//     its config holds http://127.0.0.1:8787/mcp (the manager itself) and
//     …/mcp/<name> (a server we proxy). Importing those would fill the
//     inventory with servers pointing at ourselves.
//  2. Strip the client's command prefix. opencode stores the whole command as
//     one array and needs ["cmd","/c"] on Windows; that prefix is the shell
//     shim, not part of the server.
//  3. Infer the transport from the block's SHAPE, never from "type". VS Code
//     writes no "type" for stdio, and some clients spell it differently.
func importServer(p *Plugin, name string, block map[string]any) (InventoryServer, bool) {
	s := InventoryServer{}

	// Trap 1: ours. Compare with the trailing "/" trimmed, and require the
	// separator for the sub-path form — otherwise a proxy_base of
	// http://host/mcp would also swallow an innocent http://host/mcpx.
	if p.ProxyBase != "" {
		if u, _ := block[p.HTTPKey].(string); u != "" {
			base := strings.TrimSuffix(p.ProxyBase, "/")
			if u == base || strings.HasPrefix(u, base+"/") {
				return s, false
			}
		}
	}

	s.Notes = []string{importNote(p)}

	// Trap 3: a URL means http, anything else means stdio. This is Render's
	// own decision read backwards, so it agrees with the writer by
	// construction and never has to know about type_map or omit_type.
	if u, _ := block[p.HTTPKey].(string); u != "" {
		s.Transport = "http"
		s.URL = u
		if h, ok := block["headers"].(map[string]any); ok {
			s.Headers = map[string]string{}
			for k, v := range h {
				if sv, ok := v.(string); ok {
					s.Headers[k] = sv
				}
			}
			if len(s.Headers) == 0 {
				s.Headers = nil
			}
		}
		return s, true
	}

	s.Transport = "stdio"

	// Trap 2: the shell shim comes off, but ONLY when it is really there. An
	// array that does not start with the prefix is a client that spells it
	// differently, and stripping blind would eat the command itself.
	if p.CommandShape == "array" {
		parts := toStrings(block["command"])
		parts = stripPrefix(parts, p.CommandPrefix)
		if len(parts) == 0 {
			return s, false // nothing left — unreadable, so skip rather than guess
		}
		s.Command, s.Args = parts[0], parts[1:]
	} else {
		s.Command, _ = block["command"].(string)
		if s.Command == "" {
			return s, false
		}
		s.Args = toStrings(block["args"])
	}

	if e, ok := block["env"].(map[string]any); ok {
		s.Env = map[string]string{}
		for k, v := range e {
			if sv, ok := v.(string); ok {
				s.Env[k] = sv
			}
		}
		if len(s.Env) == 0 {
			s.Env = nil
		}
	}
	return s, true
}

// toStrings reads a JSON array as strings, skipping anything that is not one.
//
// Skipping rather than failing is deliberate: an entry with a number in "args"
// is still a server we can describe, and losing the whole client over one odd
// element would be the worse trade.
func toStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// stripPrefix removes prefix from the front of parts, but only on an exact
// match of every element. A partial match is left alone.
func stripPrefix(parts, prefix []string) []string {
	if len(prefix) == 0 || len(parts) < len(prefix) {
		return parts
	}
	for i, p := range prefix {
		if parts[i] != p {
			return parts
		}
	}
	return parts[len(prefix):]
}

// ---------------------------------------------------------------------------
// Reading the clients
// ---------------------------------------------------------------------------

// clientServers is what one client currently holds, and where it came from.
type clientServers struct {
	plugin *Plugin
	source string
	// foreign maps a server name to the entry to import.
	foreign map[string]InventoryServer
	// ours counts the entries recognised as the manager's own — the proxy
	// base and everything routed through it. Reported so a user who sees
	// "nothing to import" learns they were recognised, not missed.
	ours int
}

// importFromFile reads a client's config file. A missing file is not an error:
// a client that has never been configured has nothing to import.
func importFromFile(p *Plugin) (clientServers, error) {
	path := expandPath(p.ConfigPath)
	doc, err := readDoc(path)
	if err != nil {
		return clientServers{}, err
	}
	// blockFor decodes the root key into name -> block. Each BLOCK is what
	// importServer reads; the outer map is the list of names.
	blocks := blockFor(doc[p.RootKey])
	if blocks == nil {
		return clientServers{}, nil
	}
	return collect(p, path, blocksOf(blocks)), nil
}

// importFromAPI reads a client that keeps its servers behind an HTTP API
// (AJEAN). It fails when the client is not running — which is reported for
// that client alone, never allowed to stop the others.
func importFromAPI(p *Plugin) (clientServers, error) {
	current, err := newAPIClient(p).list()
	if err != nil {
		return clientServers{}, err
	}
	return collect(p, p.Target(), current), nil
}

// blocksOf re-groups a decoded servers block as name -> the server's own map.
//
// The two readers hand the same data over in two shapes: blockFor (a file)
// yields name -> any, and apiClient.list (an API) already yields name -> map.
// This is the one place that difference is reconciled, so importServer itself
// sees a single type.
func blocksOf(raw map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(raw))
	for name, v := range raw {
		if m, ok := v.(map[string]any); ok {
			out[name] = m
		} else {
			// Present but not an object: keep the name with a nil block so the
			// entry is counted as "ours", never silently dropped from the count.
			out[name] = nil
		}
	}
	return out
}

// collect runs every block of one client through importServer.
func collect(p *Plugin, source string, blocks map[string]map[string]any) clientServers {
	cs := clientServers{plugin: p, source: source, foreign: map[string]InventoryServer{}}
	for name, block := range blocks {
		if block == nil {
			cs.ours++ // present but empty: ours, or unusable — either way not importable
			continue
		}
		if s, ok := importServer(p, name, block); ok {
			cs.foreign[name] = s
		} else {
			cs.ours++
		}
	}
	return cs
}

// ---------------------------------------------------------------------------
// The import command
// ---------------------------------------------------------------------------

// importAll reads every client and returns what each one holds. One client
// failing does not stop the run: the same rule the apply follows, because a
// closed AJEAN must not prevent previewing Claude Code.
func importAll() (clients []clientServers, failures []string) {
	plugins, err := LoadPlugins(applyDir())
	if err != nil {
		return nil, []string{err.Error()}
	}
	if len(plugins) == 0 {
		return nil, []string{fmt.Sprintf("no plugin in %s", applyDir())}
	}
	for _, p := range plugins {
		var cs clientServers
		if p.IsAPI() {
			cs, err = importFromAPI(p)
		} else {
			cs, err = importFromFile(p)
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", p.Label, err))
			continue
		}
		clients = append(clients, cs)
	}
	return clients, failures
}

// importTo reports what importing would do, and with write merges the NEW
// servers into inventory.json. Conflicts are reported and left untouched.
//
// It writes its report into w, so the CLI and any future caller share one code
// path.
func importTo(w io.Writer, write bool) error {
	clients, failures := importAll()

	totalNew, totalConflict := 0, 0
	// merged accumulates the new servers across all clients, keyed by name.
	merged := map[string]InventoryServer{}

	// Read the inventory ONCE. The first version called LoadInventory per
	// server, which meant a fresh disk read and a fresh parse for every name
	// — and, worse, a window in which the file could change under the run.
	inv, invErr := LoadInventory(InventoryPath())

	for _, cs := range clients {
		names := make([]string, 0, len(cs.foreign))
		for n := range cs.foreign {
			names = append(names, n)
		}
		sort.Strings(names)

		fmt.Fprintf(w, "==== %s (%s) ====\n", cs.plugin.Label, cs.plugin.ID)
		fmt.Fprintf(w, "source    : %s\n", cs.source)
		if cs.ours > 0 {
			fmt.Fprintf(w, "ours      : %d entr(ies) recognised as the manager's own (skipped)\n", cs.ours)
		}
		if len(names) == 0 {
			// Say WHICH nothing: "nothing here but our own entries" and "this
			// client has no servers at all" are different facts.
			if cs.ours > 0 {
				fmt.Fprintf(w, "  = no foreign server here — every entry is the manager's own\n\n")
			} else {
				fmt.Fprintf(w, "  = this client has no servers configured\n\n")
			}
			continue
		}
		for _, n := range names {
			if same, conflict := mergeStatus(inv, invErr, n, cs.foreign[n]); conflict {
				fmt.Fprintf(w, "  ~ %s   (already in the inventory with different content — left alone)\n", n)
				totalConflict++
			} else if same {
				fmt.Fprintf(w, "  = %s   (already in the inventory)\n", n)
			} else {
				fmt.Fprintf(w, "  + %s\n", n)
				totalNew++
				merged[n] = cs.foreign[n]
			}
		}
		fmt.Fprintln(w)
	}

	for _, f := range failures {
		fmt.Fprintf(w, "  FAILED — %s\n", f)
	}
	if len(failures) > 0 {
		fmt.Fprintln(w)
	}

	switch {
	case !write:
		if totalNew == 0 {
			fmt.Fprintf(w, "nothing to add — the clients hold no server the inventory does not already have.\n")
		} else {
			fmt.Fprintf(w, "%d new server(s), %d conflict(s) left untouched.\n", totalNew, totalConflict)
			fmt.Fprintf(w, "\n(dry-run — nothing is written. Add --write to import.)\n")
		}
		if len(failures) > 0 {
			return fmt.Errorf("%d client(s) could not be read — see above", len(failures))
		}
		return nil
	case totalNew == 0:
		fmt.Fprintf(w, "nothing written — no new server to add.\n")
	}

	if totalNew > 0 {
		path := InventoryPath()
		if err := mergeIntoInventory(path, merged); err != nil {
			return err
		}
		names := make([]string, 0, len(merged))
		for n := range merged {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(w, "added to %s:\n", path)
		for _, n := range names {
			fmt.Fprintf(w, "  + %s\n", n)
		}
		fmt.Fprintf(w, "\nFill in each server's \"why\" — it is the first thing an agent reads, and a\nclient's config has no place to keep it.\n")
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d client(s) could not be read — everything else still imported", len(failures))
	}
	return nil
}

// mergeStatus compares an incoming server with the inventory's own. It returns
// (same, conflict): same means the name exists and the content agrees,
// conflict means the name exists and differs — the only case that must never
// be resolved silently.
func mergeStatus(inv *Inventory, invErr error, name string, incoming InventoryServer) (same, conflict bool) {
	if invErr != nil || inv == nil {
		return false, false
	}
	cur, ok := inv.Servers[name]
	if !ok {
		return false, false
	}
	// Compare the fields an import can set, not the notes: the note is
	// added by the import and would make every server look like a conflict.
	a := comparable(incoming)
	b := comparable(cur)
	if a != b {
		return false, true
	}
	return true, false
}

// comparable renders the merge-relevant fields of a server as one string.
func comparable(s InventoryServer) string {
	return strings.Join([]string{
		s.Transport, s.Command, s.URL,
		strings.Join(s.Args, "\x00"),
		strings.Join(notesKey(s.Env), "\x00"),
		strings.Join(notesKey(s.Headers), "\x00"),
	}, "\x01")
}

func notesKey(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// mergeIntoInventory merges the new servers into inventory.json, preserving
// every other top-level key — $comment above all, which is the file's own
// documentation and is not ours to drop.
//
// The write goes through the apply's own machinery (readDoc, setBlock,
// writeDoc), so the merge gets the same backup-and-atomic-write guarantee a
// real apply has.
func mergeIntoInventory(path string, add map[string]InventoryServer) error {
	doc, err := readDoc(path)
	if err != nil {
		return err
	}
	merged := map[string]any{}
	current := map[string]InventoryServer{}
	if raw, ok := doc["servers"]; ok {
		if m := blockFor(raw); m != nil {
			for k, v := range m {
				merged[k] = v
				if sm, ok := v.(map[string]any); ok {
					var s InventoryServer
					if err := json.Unmarshal(mustMarshal(sm), &s); err == nil {
						current[k] = s
					}
				}
			}
		}
	}
	for name, s := range add {
		// The never-overwrite promise lives HERE, not in the caller: a name
		// already present keeps the inventory's version, whatever the caller
		// decided. An import that silently replaced a hand-written server
		// would be the worst possible failure of this command.
		if _, exists := current[name]; exists {
			continue
		}
		body, err := marshalDoc(s)
		if err != nil {
			return err
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return err
		}
		merged[name] = m
	}
	if err := setBlock(doc, "servers", merged); err != nil {
		return err
	}
	backup, err := writeDoc(path, doc)
	if err != nil {
		return err
	}
	if backup != "" {
		fmt.Printf("\n  backup: %s\n", backup)
	}
	return nil
}

// mustMarshal encodes a value we just decoded; the bytes are only fed straight
// back to json.Unmarshal, so a failure here would be a bug in encoding/json.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// cmdImport is the CLI entry point.
func cmdImport(write bool) error {
	return importTo(os.Stdout, write)
}

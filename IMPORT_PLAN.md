# MCP Manager — coding instructions: import what the clients already have (#5)

> For a coder coming in cold. Everything you need is here; you should not need the chat history.

## The problem

On a first install the manager replaces each client's config with an **empty** inventory. A user who
already has MCP servers configured in Claude Code, opencode, VS Code or AJEAN loses them. Import
reads what each client already has, recognises those servers, and **offers** to fold them into the
inventory. It never auto-applies — preview first, merge only on an explicit `--write`, and a name
that already exists is shown, never silently overwritten.

## What already exists — reuse, do not rewrite

All in `apply.go`:

| Function | What it does | Use it for |
|---|---|---|
| `readDoc(path)` | reads a JSON file keeping every top-level value as raw bytes | reading a file client's config |
| `blockFor(raw json.RawMessage) map[string]any` | decodes one root key's raw bytes (for reading, not writing) | extracting a client's `servers` block |
| `apiClient.list()` | already fetches an API client's servers as `map[string]map[string]any` (AJEAN) | reading the API client |
| `expandPath`, `applyDir`, `LoadPlugins` | plugin + path resolution | iterating clients |
| `LoadInventory`, `setBlock`, `writeDoc` | inventory load / replace-one-key / atomic write-with-backup | merging into inventory.json |

`Render(p *Plugin, name string, s InventoryServer) map[string]any` is the WRITE direction. Import is
`Render` in reverse. Read `Render` first — every rule below mirrors one of its branches.

## The reverse mapping — the heart, and its three traps

New file `import.go`. The core function:

```go
// importServer is Render in reverse: a client's config block → an inventory
// server. ok=false means "skip this entry" (it is ours, or unreadable).
func importServer(p *Plugin, name string, block map[string]any) (InventoryServer, bool)
```

The three traps (from `BRAINSTORM.md` #5), each a real failure mode:

1. **Unwrap our own proxy.** A client whose plugin has `proxy_base` talks to the manager, not to the
   upstreams — so its config contains entries like `http://127.0.0.1:8787/mcp` (the manager itself)
   and `http://127.0.0.1:8787/mcp/vision` (a server we proxy). Importing those naively would create
   inventory servers pointing at ourselves. **Skip any entry whose URL equals `p.ProxyBase` or starts
   with `p.ProxyBase + "/"`** (compare with the trailing `/` trimmed). Return `ok=false`.

2. **Strip client quirks.** `command_shape: "array"` (opencode) stores the whole command as one
   array: `["cmd","/c","npx","-y","@playwright/mcp@latest",…]`. The `p.CommandPrefix` (`["cmd","/c"]`)
   is the shell shim. Strip it — **only if the array actually starts with it** — to recover
   `["npx",…]`. Then command = first element, args = the rest. A prefix that does not match is left
   alone.

3. **Infer the transport.** VS Code omits `type` (`omit_type`); do not read it. Infer from the block's
   shape: it has `p.HTTPKey` (a non-empty `url`) → http; otherwise → stdio. This is exactly how
   `Render` decides, in reverse, and it sidesteps `type_map`/`omit_type` entirely.

Sketch:

```go
s := InventoryServer{}
if p.ProxyBase != "" {
    if u, _ := block[p.HTTPKey].(string); u != "" {
        base := strings.TrimSuffix(p.ProxyBase, "/")
        if u == base || strings.HasPrefix(u, base+"/") {
            return s, false // ours — the manager, or a server we already proxy
        }
    }
}
s.Notes = []string{"imported from " + p.Label + " — why and notes live only in the inventory."}
if u, _ := block[p.HTTPKey].(string); u != "" {
    s.Transport, s.URL = "http", u
    return s, true
}
s.Transport = "stdio"
if p.CommandShape == "array" {
    parts := toStrings(block["command"].([]any))
    parts = stripPrefix(parts, p.CommandPrefix)
    if len(parts) == 0 {
        return s, false // nothing left after the strip — unreadable, skip
    }
    s.Command, s.Args = parts[0], parts[1:]
} else {
    s.Command, _ = block["command"].(string)
    s.Args = toStrings(block["args"].([]any))
}
return s, true
```

`toStrings(v any) []string` (decode a JSON array to strings, skipping non-strings) and
`stripPrefix(parts, prefix []string) []string` are two small helpers to add in the same file.

## Reading each client

- **File clients** (`claude-code`, `opencode`, `vscode`): `doc, err := readDoc(expandPath(p.ConfigPath))`
  → `blockFor(doc[p.RootKey])` → iterate the names → `importServer`.
- **API client** (`ajean`): `newAPIClient(p).list()` already returns the servers → iterate →
  `importServer`. If it fails (AJEAN is not running), report the error for that client and continue
  with the others — the same "one client down does not stop the rest" rule the apply follows.

## The CLI command

In `main.go`, add a case to the switch:

```go
case "import":
    // same --write flag as apply
    write := false
    for _, a := range args[1:] {
        if a == "--write" || a == "-w" { write = true }
    }
    inv, err := LoadInventory(InventoryPath())
    if err != nil { fail(err.Error()) }
    if err := cmdImport(inv, write); err != nil { fail(err.Error()) }
```

`cmdImport(inv *Inventory, write bool) error` (in `import.go`):

1. For each plugin, gather the foreign servers (`ok=true`).
2. Compare each against the current inventory: **new** (name not present) vs **conflict** (name
   present with different content).
3. **Preview (default):** print, per client, the foreign servers and whether each is `+ new` or
   `~ conflict`. State the source path. Do **not** write. Note the skipped "ours" count so the user
   sees they were recognised, not missed.
4. **`--write`:** merge **only the new** servers into `inventory.json`, preserving everything else.
   Use the apply's own machinery so `$comment` survives and a backup is made:

   ```go
   doc, _ := readDoc(InventoryPath())           // preserves $comment and any other top-level key
   // merge new servers into the current servers map, then:
   setBlock(doc, "servers", mergedServersMap)   // map[string]any keyed by name
   writeDoc(InventoryPath(), doc)               // backup + atomic write
   ```

   Conflicts are reported and **skipped** — never overwritten.

`why` stays empty on an imported server (a client stores no `why`); the origin is in `notes` (see the
sketch above). The user fills `why` afterwards.

## Tests (`import_test.go`) — each named after the failure it prevents

Use throwaway plugins (`Plugin{...}` literals) — no real client config is touched.

- `TestImportServerStdio` — a `string_args` block `{command:"npx", args:[...]}` → command + args.
- `TestImportServerStripsCommandPrefix` — opencode's `{command:["cmd","/c","npx","-y","x"]}` →
  command `npx`, args `["-y","x"]`; a `["node","x"]` that does not start with the prefix stays
  `["node","x"]`.
- `TestImportServerHttp` — `{url:"http://1.2.3.4/mcp"}` → transport http.
- `TestImportServerSkipsOurOwnProxy` — url == `http://127.0.0.1:8787/mcp` and url ==
  `http://127.0.0.1:8787/mcp/vision` both return `ok=false` for a plugin with that `proxy_base`.
- `TestImportServerInfersTransportWithoutType` — a vscode plugin (no `type` in the block) with
  `{command:"npx", args:[...]}` → stdio.
- `TestImportMergePreservesCommentAndSkipsConflicts` — merge into a temp inventory.json that already
  has `$comment` and a server named `x`; assert `$comment` survives, `x` is untouched, and only the
  new server is added.

## Verification

1. `go build ./... && go vet ./... && gofmt -l .` — clean.
2. `go test -race -count=1 ./...` — clean, including the new import tests.
3. On the real machine: `manager import` must list the servers already in each client as **ours**
   (skipped, not imported) — today that is nearly everything, which is correct.
4. On a scratch copy of `inventory.json`: `manager import --write` adds a genuinely foreign server
   and leaves `$comment` intact; the file still parses and the backup is created next to it.

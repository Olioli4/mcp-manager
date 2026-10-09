# Plugins — teaching the manager a new client

A **plugin** is a JSON descriptor in `plugins/`. It is the only thing you write to support a new
client: no Go, no recompile, no rebuild. (Go's `plugin` package cannot load `.so` on Windows, so a
descriptor file is also the only honest option.)

```
plugins/
  claude-code.json     ~/.claude.json                     root key mcpServers
  opencode.json        ~/.config/opencode/limits.json      root key mcp
  vscode.json          %APPDATA%\Code\User\mcp.json        root key servers
  ajean.json           no file — drives an HTTP API
  _example.json        a leading _ DISABLES a file
```

## Discovery

The folder is re-read **on every request** — the web UI and `apply` both. Drop a file in and its
column appears immediately: no restart, no reload button. A file whose name starts with `_` is
skipped, which is how you park an example or disable a client without deleting it.

---

## The file writer (the default)

```jsonc
{
  "id": "vscode",                 // defaults to the filename without .json
  "label": "VS Code",             // the column header in the matrix
  "config_path": "~\\AppData\\Roaming\\Code\\User\\mcp.json",
  "root_key": "servers",          // the key holding the server list
  "type_map": { "http": "http" }, // our transport -> the client's vocabulary
  "omit_type": ["stdio"],         // transports for which we write NO "type" at all
  "command_shape": "string_args", // "string_args" | "array"
  "command_prefix": ["cmd","/c"], // only with command_shape "array"
  "http_key": "url",
  "proxy_base": "http://127.0.0.1:8787/mcp"
}
```

| Field | Meaning |
|---|---|
| `config_path` | where the client keeps its config. `~` expands to your profile. |
| `root_key` | the key whose value is the map of servers. **We replace only this key** — everything else in the file is left alone. |
| `type_map` | how our `stdio`/`http` is spelled there (`local`/`remote` for opencode). |
| `omit_type` | transports for which no `type` field is written at all. |
| `command_shape` | `string_args` = `command` string + `args[]`; `array` = one array. |
| `command_prefix` | prefixed to the array form — opencode needs `["cmd","/c"]` because Node cannot exec `npx` (a `.cmd`) without a shell. |
| `http_key` | the key carrying the URL (almost always `url`). |
| `proxy_base` | **if set, the client talks to the manager, not to the upstreams.** Servers are written as `<proxy_base>/<name>`, and the manager itself as `<proxy_base>`. This is what makes "a request arrives, the prerequisite is off, start it" work. |
| `omit_type`, `direct` | the escape hatches for a client the manager cannot relay. |

Writing is always: **timestamped backup next to the file**, then only `root_key` is replaced.
`apply` without `--write` prints the exact block and touches nothing.

---

## The API writer (a client with no config file)

**AJEAN does not keep its servers in a file.** It keeps them in a **bbolt database** (bucket
`state`, key `mcp`) and exposes them only through its own HTTP API. So a plugin may declare
`"writer": "api"`:

```jsonc
{
  "id": "ajean",
  "label": "AJEAN",
  "writer": "api",
  "api_base": "http://127.0.0.1:8090",
  "api_list_path": "/api/mcp",        // GET  -> {"ok":true,"servers":[{"name":…},…]}
  "api_list_array": "servers",        //   which array holds them     (default "servers")
  "api_name_field": "name",           //   which field is the name    (default "name")
  "api_save_path": "/api/mcp/save",   // POST one NAMED server
  "api_delete_path": "/api/mcp/delete",
  "api_auth_header": "", "api_auth_value": ""   // optional; empty = no Authorization header
}
```

Everything else works unchanged — `proxy_base`, `omit_type`, targeting, the matrix. That is not a
coincidence: **the body we POST is exactly the map `Render()` produces, with the name added.** The
client's API takes one named server at a time, where a file takes them all under one key.

Differences from the file writer, and why:

- **The dry run prints the CALLS, not a block.** For an API client the calls *are* the change:
  `DELETE /api/mcp/delete {"name":"MCP HUB"}` then one `POST /api/mcp/save` per server.
- **Removals run before writes**, so a client's stale entry never coexists with its replacement.
- **Diffing compares only the fields we control.** The client's answer also carries `connected` and
  a live tool count, which change on every call — comparing those would re-save everything forever.
- **There is no backup**, because there is no file we own. The client's API is the way back.

### Do not write a client's database directly

bbolt takes an **exclusive file lock**, and the client is normally running. At best you block; at
worst you corrupt a store that also holds its keys and tokens. If the client has an API, use it.

### The one consequence to know

**An API client must be RUNNING for an apply to reach it.** A file client can be configured while
closed; this one cannot. The manager says so in those words rather than failing obscurely:

```
error: AJEAN is not answering on http://127.0.0.1:8090 — it must be RUNNING for us to
configure it (a file client can be configured while closed; an API client cannot): …
```

---

## The clients, as measured on this machine

| Client | Where | Root key | stdio type | `command` shape | http key |
|---|---|---|---|---|---|
| Claude Code | `~/.claude.json` | `mcpServers` | `"stdio"` | string + `args[]` | `url` |
| opencode | `~/.config/opencode/limits.json` | `mcp` | `"local"` | **array**, needs `["cmd","/c"]` prefix | `url` |
| VS Code | `%APPDATA%\Code\User\mcp.json` | `servers` | **omitted** | string + `args[]` | `url` |
| AJEAN | *no file* — HTTP API on `:8090` | — | **omitted** | string + `args[]` | `url` |

The same server, three ways — this is the whole reason plugins exist:

```jsonc
// VS Code                          // Claude Code                     // opencode
{ "command": "npx",                { "command": "npx",                { "command": ["cmd","/c","npx",
  "args": ["-y","@playwright/mcp"] } "args": ["-y","@playwright/mcp"],    "-y","@playwright/mcp"] }
                                     "type": "stdio" }
```

---

## Adding a client — the recipe

1. **Find where it keeps its config.** Its docs, its source, or its own settings UI. If you cannot
   point at a file *or* an API, stop — do not guess.
2. **Determine four things:** the root key, the vocabulary for `type`, the shape of `command`,
   and whether `type` is written at all.
3. **Write `plugins/<id>.json`.**
4. **`mcp-manager.exe apply`** — read the printed block. It is the exact JSON that would be written.
5. **`mcp-manager.exe apply --write`**, then confirm in the client itself.

### Before you write anything, check for these three traps

**Is the config file JSONC?** If it contains `//` comments, `encoding/json` refuses it. Pointing a
plugin at it either fails or — worse — strips the user's comments from a file we do not own. This
is why VS Code's `settings.json` is *not* a target.

**Does the file hold more than MCP?** `~/.claude.json` is Claude Code's live state file;
VS Code's config is one key away from a 100 KB settings blob. We replace only `root_key`, and take
a timestamped backup — but the file is still the client's, and it may rewrite the block itself. If
it does, re-run `apply`.

**Does it merge two files?** opencode reads `opencode.json` **and** `limits.json`. A server removed
from one **survives** if it is in the other. Write into the one the client's other tools do not own.

### Terminology worth keeping straight

`needs_service` in the *inventory* is about an MCP server's upstream. A *plugin* describing an API
client is a different thing again: there the client must be running for the manager to reach it at
all. Both are declared, neither is guessed.

# MCP Manager

A **local, native, Docker-free** tool that owns three jobs which are really one:

1. **Declare an MCP server once** — in one inventory, not in four clients' configs.
2. **Write it into each client's own format.** Claude Code, opencode, VS Code, AJEAN … each is
   different. A *plugin* per client translates; adding a client means adding a file.
3. **Start what it needs, when it is needed.** The manager is the transport, so a request that
   finds ComfyUI or Chrome stopped starts it first, then forwards.

Everything runs on the host, in the tray. Nothing listens on the network — only `127.0.0.1`.

```
                 ┌──────────────┐
   inventory.json│   MANAGER    │  services.json
   (MCP servers) │  :8787 web   │  (host programs)
        │        │  /mcp proxy  │        │
        │        └──────┬───────┘        │
        │               │                │
   plugins/*.json ──────┤                └── probe / start / stop
   (one per client)     │
        │               │
   ┌────▼────┐  ┌───────▼──┐  ┌────────┐  ┌───────┐
   │ Claude  │  │ opencode │  │ VS Code│  │ AJEAN │
   └─────────┘  └──────────┘  └────────┘  └───────┘
```

---

## Quick start

```
go build -o manager.exe .
go build -o manager-gui.exe -ldflags "-H=windowsgui" .

manager-gui.exe            # tray icon; brings up the web UI on 127.0.0.1:8787
```

Then right-click the tray icon → **Open the configuration…**

### First run

There is no `services.json` or `inventory.json` in this repository, and that is deliberate: both
describe **one machine** — your ComfyUI path, your Blender, your browser profile. They are a map of
a computer, useless to anyone else.

Instead the repository ships `services.example.json` and `inventory.example.json`, and on first
launch the manager **copies them into place** and starts. You get a running program with three
placeholder services and four placeholder servers to edit, rather than an error about a file you
have never heard of.

```
services.json     ← copied from services.example.json   (yours; git-ignored)
inventory.json    ← copied from inventory.example.json  (yours; git-ignored)
```

Edit them, or edit the inventory from the web UI. Neither is ever committed.

**Windows only.** The manager starts and stops native programs, drives the tray, and speaks to the
Chrome DevTools Protocol — it is built for Windows and does not pretend otherwise.

```
manager.exe list           # the host programs and their live state
manager.exe start <id>     # start one, wait for its probe
manager.exe apply          # DRY RUN — shows exactly what would be written, changes nothing
manager.exe apply --write  # writes it, after a timestamped backup
```

`apply` without `--write` is the one to reach for first: it prints the exact JSON block per client
(or the exact HTTP calls, for a client configured over an API) and touches nothing.

---

## The two files that matter

Both live next to the executable — resolved **absolutely**, because a process started by a shortcut,
WMI or the Task Scheduler does not have the app folder as its working directory.

Both are **yours, not the repository's**. The `.example.json` twins are what gets committed; the
manager copies them on first run and you edit the real ones.

### `services.json` — the HOST programs

How to probe, start and stop each one, plus **why it exists** and its traps. The engine knows no
service in particular; it only reads this file.

```jsonc
{
  "id": "chrome-cdp",
  "label": "Chrome (debug port)",
  "why": "The VISIBLE browser that agents drive.",
  "notes": ["A dedicated --user-data-dir is MANDATORY: since Chrome 136 remote debugging is refused on the default profile."],
  "requires": [],
  "start": { "exe": "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe", "args": ["--remote-debugging-port=9222"] },
  "stop":  { "process": "chrome.exe", "only_if_cmdline_matches": "chrome-cdp" },
  "probe": { "http": "http://127.0.0.1:9222/json/version" }
}
```

`only_if_cmdline_matches` is why stopping "the debug Chrome" does not close your own browser.
Edit the file and press **Re-read files** in the UI — no restart.

### `inventory.json` — the MCP SERVERS

```jsonc
{
  "servers": {
    "vision": {
      "transport": "http",                 // "stdio" | "http"
      "url": "http://127.0.0.1:8761/mcp",
      "why": "Analyses a local image.",
      "notes": ["The first call after an idle period reloads the engine: 20–30 s."],
      "needs_service": "vision",           // tie-in to the concierge
      "clients": ["claude-code"]           // optional; ABSENT = all clients
    }
  }
}
```

`clients` is the whole point of the matrix: **absent = everyone, `[]` = nobody, a list = only
those.** The distinction is load-bearing — without it, "uncheck everything for this client" would
be read back as "everyone", exactly the opposite.

---

## The rule that governs everything

> **Only an incoming request may start a service. Never a timer, never a startup.**

Enforced in code, not by convention (`ensureUpstream(ctx, mayStart bool)` — a real tool call passes
`true`, the connection pre-heat passes `false`). An earlier version got this wrong, and merely
*launching the manager* started ComfyUI — nine processes — and Chrome.

---

## Where to read what

| File | For |
|---|---|
| **API.md** | the HTTP API, the MCP surface, the CLI, and the `/mcp/<name>` proxy |
| **PLUGINS.md** | the plugin format, every field, both writers, and how to add a client |
| **PLAN.md** | the plan and the running log — what was built, what broke, what was measured |
| `services.json` / `inventory.json` | both carry a `$comment` block documenting their own fields |

---

## Gotchas, learned the hard way

- **VS Code's `settings.json` is JSONC** — it contains `//` comments, so `encoding/json` refuses it.
  Never point a plugin at it. The live file is `%APPDATA%\Code\User\mcp.json`.
- **opencode reads two merged files.** `opencode.json` is owned by OpenChamber and gets rewritten;
  the durable config is `limits.json`. A server removed from `limits.json` **survives** if it is
  also in `opencode.json`.
- **`opencode mcp list` lies** — it reported "No MCP servers configured" while the running server
  demonstrably had three connected. Trust the log, not that subcommand.
- **An HTTP probe can call a half-booted app "stopped."** ComfyUI mid-launch has live processes and
  nothing listening yet. `stop` therefore has a process arm too, and says so when it uses it:
  *"stopped — 9 remaining processes eliminated (the probe already said it was "stopped")"*.
- **`~/.claude.json` is live state** Claude Code writes to itself. If it ever rewrites that block,
  re-run `manager apply --write`.
- **Never write a client's database directly.** bbolt locks exclusively; if the client is running,
  at best you block and at worst you corrupt it. Use its API — that is what `writer: "api"` is for.

---

## Not done yet

- **Secrets.** The inventory should never hold one — it should name an environment variable. Note
  that VS Code has a stronger mechanism than `${env}`: a top-level `inputs` array with `promptString`
  + `password: true`, referenced as `${input:id}`, so the value never reaches a file. We already
  preserve `inputs` by writing only the `servers` key. Not designed yet.
- **Importing servers back** from the clients. Today the direction is one-way: we tell them. This is
  real detection — reading what a client already has — as opposed to scanning the machine and
  guessing a config format, which I would argue against.
- **Plugins view** in the UI (see the descriptors; add a client from the page).
- **Launch at logon** via a scheduled task **in the user's session** — never a Windows service
  (session 0 has no desktop, so it could not open Chrome or ComfyUI).

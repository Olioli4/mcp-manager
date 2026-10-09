# The manager's interfaces

Three ways in, one engine behind them: **HTTP** (the web UI and scripts), **MCP** (agents), and the
**CLI**. Everything listens on `127.0.0.1` only — nothing is exposed to the network.

---

## CLI

```
manager                 the resident application (tray icon + web UI + MCP server)
manager list            services, with their live state and their reason for being
manager status <id>     one service in detail, with its notes
manager start <id>      start it and wait for its probe to answer
manager stop <id>       stop it and wait for it to be gone
manager apply           DRY RUN — prints the exact block (or the exact calls) for every client
manager apply --write   writes them, after a timestamped backup
manager reload          re-reads services.json live
```

`apply` is the one to know: without `--write` it changes nothing and shows you precisely what it
would do. With `--write` it backs up each file it touches as `<file>.bak-<YYYYMMDD-HHMMSS>`.

---

## HTTP API

Base: `http://127.0.0.1:8787`. Responses are JSON. Errors come back as `{"error": "…"}` with a
non-2xx status.

| Method | Path | Body | Effect |
|---|---|---|---|
| GET | `/` | — | the web UI |
| GET | `/api/state` | — | everything the page needs: services (probed **live**), servers, plugins, the matrix, and each proxy's connection state |
| POST | `/api/service/{id}/start` | — | start, wait for the probe to go green |
| POST | `/api/service/{id}/stop` | — | stop, wait for the probe to go red |
| POST | `/api/toggle` | `{client, server, on}` | write the `clients` list of one server |
| POST | `/api/server` | `{key, name, spec}` | create, modify or rename a server |
| POST | `/api/server` | `{key, delete: true}` | delete a server |
| POST | `/api/reload` | — | re-read `services.json` and `inventory.json` |
| POST | `/api/apply` | `{write: bool}` | render (and optionally write) every client; returns `{report}` |
| POST | `/mcp` | MCP | the manager's own MCP surface |
| POST | `/mcp/{name}` | MCP | proxy to the inventory server `name` |

### `POST /api/server` — the one with a subtlety

`key` is the name the server has **now** (its key in `inventory.json`); `name` is the name you
**want**. Separating them is what makes rename possible without a special case: leave `name` out of
it and you have an ordinary edit; give a different one and the old entry is deleted and the new one
written.

```jsonc
// add or modify — key "" means "this is new"
{"key": "", "name": "vision", "spec": {
  "transport": "http", "url": "http://127.0.0.1:8761/mcp",
  "why": "Analyses a local image.",
  "notes": ["The first call after an idle period reloads the engine: 20–30 s."],
  "needs_service": "vision", "clients": null
}}

// rename  demo  ->  demo2
{"key": "demo", "name": "demo2", "spec": { … }}

// remove
{"key": "demo", "delete": true}
```

`clients: null` means **all** clients; `[]` means **none**; a list means only those. The
distinction is load-bearing — without it, "uncheck everything for this client" would be read back
as "everyone", exactly the opposite.

**It refuses at write time what would otherwise fail silently later:**

| Refused | Because otherwise |
|---|---|
| unknown transport | nothing would ever connect |
| `stdio` with no command, `http` with no url | an empty server entry |
| a url not starting `http://` / `https://` | a silently unreachable server |
| a misspelled client id | the server would be excluded from every client, invisibly |
| a misspelled `needs_service` | the prerequisite would never start |
| a space or slash in the name | the name is a JSON key *and* a URL segment |

Deleting `system` is refused by name: that is the manager's own surface. Untick it in the matrix
instead if you do not want a client to see it.

After any successful change the **proxies are rebuilt immediately**, so a server you just added is
reachable on `/mcp/<name>` without restarting anything.

### `POST /api/reload`

Re-reads both source files. The manifest now sits behind an atomic pointer, so a probe already
running in the tray keeps reading the manifest it started with rather than racing the swap.

One caveat it reports honestly: the **tray menu is built once at startup** and systray cannot remove
items, so a newly added service is usable immediately in the web UI, the CLI and MCP, but will only
appear in the notification-area menu after a restart. The reload says so when it happens.

---

## The MCP surface

The manager registers **itself** in the inventory as the server `system`, so every client receives
it like any other server. Endpoint: `http://127.0.0.1:8787/mcp`.

| Tool | Answers |
|---|---|
| `services_list` | every program managed: its **real** state (probed at call time), why it exists, its prerequisites, its traps |
| `servers_list` | every declared MCP server: transport, why, notes, which clients receive it, **and the live state of its `needs_service`** |
| `service_start` | `{id}` — starts it and waits for the probe |
| `service_stop` | `{id}` — stops it and waits for it to be gone |

Sample of what an agent actually sees:

```
dcc  (http)
    why      : DCC gateway: ComfyUI and Blender tools behind a single endpoint.
    clients  : all clients
    requires the service "comfyui": STOPPED — no response on http://127.0.0.1:8188/system_stats
    · The gateway elects itself as soon as one adapter (ComfyUI or Blender) is running.
    · Its tools load on demand: call load_skill before calling a tool.
```

That is the honest half of "cache the tools": an agent learns a server **exists** and exactly what
is missing, without us pretending to own tools we cannot call. Errors are returned as readable
tool results (`isError` + a message naming the missing prerequisite), never as opaque transport
failures.

### `/mcp/{name}` — the proxy

Each inventory server is also exposed on its own path. The manager connects upstream (stdio **or**
http), **caches the tool list on disk** (`cache/<name>.tools.json`), and on an incoming call
ensures the declared prerequisite is running before forwarding.

**Note the semantic shift this creates.** A client's `Connected` light now means *"the manager is
up"*, not "the upstream is up". That is deliberate and more useful — the upstream's real state
lives in the web UI, in `servers_list`, and in the pills on the Inventory tab.

The connection is not held open: it is made on the first actual call and dropped after 5 minutes
idle (`MCPMGR_IDLE_SECONDS` overrides). Without that, starting the manager would keep
`npx @playwright/mcp` and `node aas-mcp.js` running forever — the exact "it runs when I didn't ask"
this project exists to avoid.

---

## The rule that governs all of it

> **Only an incoming request may start a service. Never a timer, never a startup.**

Enforced in code, not by convention: `ensureUpstream(ctx, mayStart bool)` — `forward()` (a real tool
call) passes `true`, `warm()` (the connection pre-heat) passes **`false`**.

An earlier version passed `true` everywhere, so merely *launching the manager* started ComfyUI
(nine processes) and Chrome. The startup log now contains **zero** start lines.

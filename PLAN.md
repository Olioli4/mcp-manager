# MCP Service Manager — plan & progress

A **local, native, Docker-free** tool that ends the chore of registering the same MCP servers in
every tool (Claude Code, opencode, VS Code, …).

## The idea (what we're building)

1. **One inventory** — declare each MCP server once, with its *why* and its *notes*.
2. **Plugin per client** — each client has its own config format; a plugin turns the inventory into
   that client's exact JSON shape. Adding Cline/Zed/whatever = dropping one plugin file, no code
   change.
3. **Concierge** — starts/stops the host programs an MCP server depends on (Chrome for playwright,
   ComfyUI, Blender, vision-go), on demand.

## Why not a Docker hub

We built MCPHub first. It worked, but every wound of that day came from the container boundary
(fwd.py, CDP through a gateway IP, Host-header filters, files landing in the container, knowledge
stuck in one client's memory). This tool removes the boundary entirely: everything is native and on
the host.

## Plugin model (the key constraint)

Go's `plugin` package (loading `.so` at runtime) **does not work on Windows**, so a "plugin" is a
**descriptor file** — one JSON per client in `plugins/` — that the main program renders. No
recompile to add a client.

## Files

```
services.json     source of truth: the HOST services (start/stop/probe), each with why + notes
engine.go         the engine — reads services.json, knows no service in particular
inventory.json    source of truth: the MCP SERVERS, declared once, with why + notes
apply.go          renders the inventory into each client's format, diffs, writes (with backup)
plugins/          one descriptor JSON per client — the extension point. Add a client = add a file
main.go           CLI (list / status / start / stop / apply) + resident entry point
resident.go       placeholder — will become the tray + MCP surface
go.mod            module mcphub-servicemanager, go 1.26
PLAN.md           this file
```

## Progress

- [x] **D1 — compile & fix.** `go build .` clean, `go vet .` clean.
  - Fixed: `Action` struct was missing the `Process` field used by `stop`.
  - Fixed: process probe failed on names with spaces (`Docker Desktop` vs `Docker Desktop.exe`)
    — added `procName()` normalisation.
  - Removed Docker/MCPHub from the manifest (dropped from the project per user).
  - `manager.exe list` reports the 4 services with correct live state.
- [x] **D2 — concierge end-to-end.** Verified on `vision`: `start` → probe green; `start` again on
  `chrome-cdp` → "already running" (no second window); `stop vision` → probe red, port free.
- [x] **D3 — dry-run writer + diff.** Plugin `plugins/claude-code.json` renders the exact Claude Code
  shape. `manager apply` shows `+ added / ~ changed / - removed` and touches nothing.
- [x] **D4 — real write, verified.** `manager apply --write` backed up
  `~/.claude.json` → `.bak-20261009-160424`, replaced only the `mcpServers` block.
  **Verified by Claude Code itself**: `claude mcp list` shows `aas, dcc, playwright, vision`
  (62 root keys and 17 `projects` entries untouched).
  - Removed as intended: `brainz` (dead) and `mcphub` (abandoned).
  - `vision` was ECONNREFUSED → `manager start vision` → **✔ Connected**. The loop closes:
    the manager writes the config *and* starts what the config needs.
- [x] **D5 — opencode plugin.** `plugins/opencode.json` — root key `mcp`, types `local`/`remote`,
  `command` as an **array**, and `command_prefix: ["cmd","/c"]` because opencode is a Node process
  and Node cannot exec `npx` (a `.cmd`) without a shell. Writes into `limits.json` (the file
  OpenChamber does **not** own).
  - **Verified by opencode's own log**: `mcp connected` for `vision` (1 tool), `aas` (9),
    `playwright` (25); `dcc` refused because the gateway is down — correct.
  - **The provider block survived**: `rewq`/`strata` intact, only `mcp` replaced.
  - ⚠️ `opencode mcp list` reports *"No MCP servers configured"* — that subcommand does not honour
    `OPENCODE_CONFIG` the way the running server does. It made the same false report earlier today
    while `playwright` was demonstrably connected. **Trust the log, not that subcommand.**
- [x] **Per-client scoping.** A server may carry `"clients": ["claude-code"]`; absent = all clients.
  Verified: restricting `aas` to `claude-code` made opencode write 3 servers and print
  `· aas (not targeted here: clients=claude-code)` — visible, not silent. A misspelled client id is
  **refused at startup** (`serveur "aas" cible un client inconnu "claude"`), not ignored.
  No server is restricted today — that's a policy call for the user.
- [x] **D6 — VS Code plugin.** `plugins/vscode.json`. Target: `%APPDATA%\Code\User\mcp.json`,
  root key `servers`.
  - **The path was verified, not assumed.** `code --add-mcp '{…}'` was used to let *VS Code itself*
    write a throwaway server; the file appeared exactly there, with that shape. Then it was removed.
    (The `.cmd` shim strips quotes from PowerShell arguments — the JSON has to be passed with
    `\"` escaping.)
  - **A trap the plan had wrong.** VS Code's `settings.json` is **JSONC** — it contains `//`
    comments, so `encoding/json` refuses it. Pointing a plugin there would have failed, or would
    have stripped the comments of a 100 KB file the user owns. The `mcp.servers` key inside it is
    legacy; **servers no longer live in settings.json**. We write neither.
  - **A real difference the plugin model had to absorb:** VS Code writes a stdio server with
    **no `type` field at all** (`{"command":…,"args":[…]}`). Added `omit_type` to the plugin
    format. Proven by rendering the same server three ways — VS Code without `type`, Claude Code
    with `type: stdio`, opencode as a `["cmd","/c",…]` array.
  - `inputs` (VS Code's own top-level block, used for prompted secrets) **survives**, because we
    replace only `doc["servers"]`.
  - Applied and read back. All three clients now report `= already up to date` — the writer is idempotent.
- [x] **W3 — inventory editing in the UI.** The Inventory tab is now the editable source of truth:
  add, modify, rename, delete, with per-client targeting, a service prerequisite picker, and the
  advanced flags. Written to `inventory.json`; `ReloadProxies` runs immediately, so a new server is
  reachable on `/mcp/<name>` without a restart.
  - **Verified through the page itself**, not through the API: filled the form, saved, read back
    `inventory.json`; opened the same server for edit (fields pre-filled correctly), renamed it,
    switched transport (the command/url rows swap, without losing what was typed), deleted it.
  - Argument input is shell-like — split on spaces, quotes protect — because
    `--cdp-endpoint http://…` must be two arguments, not one.
  - Refusals are specific and at write time: unknown transport, missing command/url, misspelled
    client, misspelled `needs_service`, illegal characters in the name. Six cases checked.
  - `$comment` (the format documentation at the top of the file) survives every write: we re-read
    the raw document rather than the typed struct.
- [x] **BUG FOUND BY CLICKING — the page rebuilt itself under the pointer.** A real click on
  "New server" timed out: every 5 s the poll re-rendered the whole tab, replacing the
  buttons. A click landing during the swap silently does nothing. A button that works one time in
  two is worse than a slow one.
  - Fixed by comparing the new state to the previous one and touching the DOM **only if it
    differs** — which also removes the pointless flicker. Measured after: the button node is
    replaced **0 times in 3 seconds**.
  - Same fix removes the same latent race from the matrix checkboxes.
- [x] **BUG FOUND BY LOOKING — a stale UI after every update.** The embedded page was served with
  no cache headers, so the browser kept the old interface and the user would go on seeing the
  previous version after the binary was replaced. Added `Cache-Control: no-store`.
- [x] **Manifest reload.** `/api/reload` re-reads `services.json` **and** `inventory.json` without
  restarting. The manifest now sits behind an `atomic.Pointer`: a probe running in the tray while
  the file is swapped keeps reading the manifest it started with, instead of racing a bare field.
- [x] **JSON writing made human-readable.** Go escapes `<` and `&` as `<` / `&` by
  default — valid, but it garbles a config file and would churn the entire diff of a file we do
  not own. All JSON writes now go through a single `marshalDoc()` that encodes with
  `SetEscapeHTML(false)`.
- [x] **Everything in English.** Asked for on 2026-10-09: *"why should any other LLM read
  french?"* The point is the same one the whole project rests on — knowledge only one reader can
  read is knowledge that dissolves. Translated: every Go comment and user-facing string, the four
  JSON files, the whole web UI, and the quotes of program output in this file.
  - The comments were **translated in full, not summarised** — they carry the reasoning the
    project paid for (why only an incoming request may start a service, why the server pointer
    needs to be atomic, why the tool list is cached on disk).
  - Identifiers were already English, and stayed as they were: this was a translation, not a
    refactor. Nothing in the behaviour changed — verified by rebuilding and re-running the same
    checks (apply dry-run, the API, an on-demand start through the proxy).
  - Quotes of program output in this file had to be updated too: they were quoting a French
    program that no longer exists.
- [x] **AJEAN — the fourth client, and the first with no config file.** `plugins/ajean.json`.
  - **The plugin model needed extending, and that was the finding.** Everyone else is configured by
    writing a JSON file. AJEAN is not: read from its own source
    (`E:\Coding\ajean\internal\ajean\mcp_config.go`), its servers live in a **bbolt database**
    (bucket `state`, key `mcp`) and are reachable only through its HTTP API
    (`/api/mcp`, `/api/mcp/save`, `/api/mcp/delete`). So a plugin can now declare
    `"writer": "api"`. The rule survives — adding a client is still just a file, it is only a
    different *kind* of file.
  - **Rejected: writing the database directly.** bbolt takes an exclusive file lock and AJEAN is
    normally running. Best case we block, worst case we corrupt a store that also holds the user's
    keys and tokens. If a client has an API, the API is the interface.
  - The adapter is thin because the client's payload already matches our own vocabulary
    (`command`/`args`/`url`): **the body we POST is exactly the map `Render()` produces, plus the
    name.** No per-client payload template was needed.
  - Diffing compares **only the fields we control**. AJEAN's answer also carries `connected` and a
    live tool count, which change on every call — comparing those would report a change forever and
    re-save every server on every apply.
  - Dry run prints the **calls**, not a block: for an API client the calls *are* the change.
  - **Verified end to end, through AJEAN's own API rather than our report:** 5 servers saved, all
    connected through the manager's proxy, and the stale `MCP HUB → localhost:3000` entry — the
    retired Docker hub, still feeding 25 tools into AJEAN — removed. A second apply says
    `= already up to date`.
  - **The new failure mode, named rather than discovered later:** an API client must be RUNNING for
    an apply to reach it. Tested by pointing a throwaway plugin at a dead port — the error reads
    *"… is not answering on http://127.0.0.1:59999 — it must be RUNNING for us to configure it
    (a file client can be configured while closed; an API client cannot)"*.
  - The matrix now shows four columns.
- [x] **The repository, and the docs split.** `git init` on `main`. The `.gitignore` keeps out the
  built binaries, `cache/` and `manager.log` — everything committed is either source or a *source
  of truth*, nothing derived.
  - **The documentation now has one job per file**, because one 250-line README doing four jobs is
    how documentation rots:
    **README.md** — what it is, quick start, the two files, the rule, the traps.
    **API.md** — the HTTP API, the MCP surface, the CLI, the `/mcp/<name>` proxy.
    **PLUGINS.md** — the plugin format, both writers, how to add a client, the three traps to check
    before writing one.
    **PLAN.md** — this log.
  - Both JSON sources keep their own `$comment` block documenting their fields, so the format is
    readable at the point of editing, not only in a doc elsewhere.
  - Swept the whole tree for French a second time, this time for accent-free French as well — the
    first pass only caught accented characters, and `"Chrome (port de debug)"` was sitting in a
    README sample the whole time.
- [x] **Adversarial audit** — `AUDIT.md`. Two independent reviewers under an adversarial brief
  (correctness/data-safety, and UI/usability) plus my own probing of the running system. Every
  load-bearing finding was re-verified by hand rather than relayed.
  - **The critical one was mine, and it was live:** a cross-origin POST reached `/api/server`
    because the server checked neither `Origin` nor `Host`, and parsed the body as JSON whatever
    the Content-Type — so a bare HTML `<form>` sufficed, no JavaScript needed. Replacing an
    inventory server with `cmd /c …` then executed it, because **the cached tool list for that
    name survives**, so the proxy still advertised tools and connecting upstream ran the command.
    Fixed in `2c04a38`; the replay is now 403 and produces no marker file.
- [x] **The writer, repaired.** Four findings, all in the same few functions:
  - **It re-encoded files it does not own.** `readDoc` decoded the whole document into
    `map[string]any`, so every JSON number became a `float64` and re-writing corrupted whatever we
    had not touched — `18446744073709551615` came back as `18446744073709552000`, a non-UTF-8 byte
    as U+FFFD, both reported as success. It now holds every top-level value as **raw bytes** and
    replaces only its own root key. (Measured before the fix: none of the three live configs would
    have changed *today* — the mechanism was armed, not firing.)
  - **A `null` config file panicked the app** — `assignment to entry in nil map`, from an HTTP
    handler and from the CLI.
  - **A partial apply threw away its own report.** Clients are written in order, so by the time one
    fails the earlier ones have already changed; the user was told only `HTTP 500`. The report now
    travels with the error and the UI displays it.
  - **Empty, whitespace and BOM files aborted the entire apply** — one bad client blocking all the
    others. The BOM case is the Windows one: Notepad and PowerShell 5.1 write them.
  - Plus: **atomic writes** (temp + `fsync` + rename, original permissions preserved — the old
    `os.WriteFile` truncated in place), **collision-proof backup names**, and a **no-op apply now
    touches nothing** (verified on the real files: mtimes unchanged, no new backup).
- [x] **The project has tests.** `apply_test.go`, each test named after the failure it prevents.
  Every one was **mutation-tested** — protection removed, test confirmed to fail, restored. That
  exercise found that my first permission test had no teeth on Windows (`os.Chmod` only toggles the
  read-only attribute, and Go reports any writable file as 0666), so it was replaced with the
  invariant Windows actually has: a file we may not write must fail, stay intact, and leave no
  litter.
- [x] **W1+W2 — local web UI.** `web.go` serves an embedded page (`web/index.html`, `go:embed`) on
  `127.0.0.1:8787` — loopback only, nothing on the network. No framework, no build step: one binary.
  Tray item **"Open the configuration…"** opens it.
  - **Services tab** — live probe per service, with *why*, *notes* and start/stop. Verified by
    rendering the page with headless Chrome and reading the screenshot: dots and button states match
    each service's real state.
  - **"MCP servers by client" tab** — the matrix (servers × clients), checkboxes, plus
    **Apply to clients** and **Preview (writes nothing)**. Hash routing (`#clients`) so a tab is directly
    addressable.
  - **Inventory tab** — read-only view of the source of truth (why/notes/target).
  - Toggling writes `inventory.json` (the source of truth), **not** the client's config — you see the
    intent, then choose to apply.
  - Verified: uncheck `vision`/opencode → `["claude-code"]`, matrix follows; re-check → back to
    *absent (= all)*, so the file stays clean. Re-toggling to "everyone" normalises to the default
    rather than writing an equivalent explicit list.
- [x] **Fixed — asset paths.** `services.json` resolved next to the exe, but `inventory.json` and
  `plugins/` used plain relative paths. A process started by WMI / Task Scheduler / a shortcut does
  **not** have the app folder as its working directory, so the web API returned empty. Added
  `assetPath()`; every asset now resolves the same way. This is the class of bug that only appears
  on a real launch, never in testing.
- [x] **S1 — the manager's own MCP surface.** `mcp.go` exposes the concierge to agents on
  `http://127.0.0.1:8787/mcp` (same port as the UI: `/` for the human, `/mcp` for an agent).
  Tools: `services_list`, `servers_list`, `service_start`, `service_stop`.
  - Registered in the inventory as the server **`system`**, so every client gets it through `apply`.
  - **`servers_list` answers the "cache the tools" idea as *knowledge*, not as a fake tool list.**
    For each declared server it reports transport, *why*, notes, which clients receive it, **and the
    live state of the service it depends on**:
    `dcc → requires the service "comfyui": STOPPED` / `playwright → requires "chrome-cdp": running`.
    An agent therefore knows a server EXISTS and precisely what is missing — even when it is
    unreachable — without us pretending to own tools we cannot call.
  - Verified end-to-end: `claude mcp list` → `system http://127.0.0.1:8787/mcp ✔ Connected`.
  - Errors are returned as explicit tool results (`isError` + a readable message naming the missing
    prerequisite), never as an opaque transport failure.
- [x] **S2 — the proxy: the app is now the transport.** `proxy.go`. Each inventory server is exposed
  on `/mcp/<name>`; the manager connects upstream (stdio **or** http), **caches the tool list**, and
  on an incoming call **ensures `needs_service` is running before forwarding**.
  - **The decisive test passed.** With vision stopped: the proxy still answered, `analyze_image`
    was still *visible* (from cache), and calling it produced
    `request received, service "vision" off — starting` → the image analysis returned
    `TEST_GGUF_OK` in 4.7 s, `:8761` now open. **No agent in that chain.**
  - Cache on disk: `cache/<name>.tools.json` — survives a manager restart (`aas` 9 tools,
    `playwright` 25 tools, `vision` 1 tool).
  - Errors are returned as readable tool results naming the missing prerequisite, never as opaque
    transport failures.
- [x] **BUG I INTRODUCED AND FIXED — warm-up was starting services.** The first cut had
  `ensureUpstream()` unconditionally starting the declared prerequisite, so merely *starting the
  manager* launched ComfyUI (9 processes) and Chrome. That is precisely the auto-start the user
  refused. Split into `ensureUpstream(ctx, mayStart bool)`: **`warm()` passes `false`** (connect
  only), **`forward()` passes `true`** (a real request may start). Verified after the fix: the
  startup log contains **zero** start lines.
  - **Design rule:** only an incoming request may start a service. Never a timer, never a startup.
- [x] **Wiring for S2 — done.** A plugin can declare `proxy_base`; servers are then written as
  `http://127.0.0.1:8787/mcp/<name>` (and the manager itself on `…/mcp`, marked `manager: true` in
  the inventory so we never hop to ourselves). `direct: true` on a server is the escape hatch.
  - **Proven end-to-end:** with `:8761` **and** `:9765` both closed, `claude mcp list` reports
    **all five servers ✔ Connected** — because the manager answers and holds the cached tool lists.
    Calling a tool then starts the upstream and forwards.
  - Semantic shift worth knowing: `Connected` now means **"the manager is up"**, not "the upstream
    is up". That is deliberate and more useful, but the *upstream's* real state lives in the UI and
    in `servers_list` — not in the client's connection light.
- [x] **Fixed — an HTTP probe could call a half-booted app "stopped".** ComfyUI launched by the bug
  above had 9 live processes with `:8188` not yet listening, so the HTTP probe read it as *stopped*
  and `stop` did nothing. `stop` now has a process arm: it kills by process name even when the
  probe says "stopped", waits for the processes to actually disappear, and says so —
  *"stopped — 9 remaining processes eliminated (the probe already said it was "stopped")"*.
- [ ] **W4 — plugins view** (see the descriptors; add a client from the page).
- [ ] **W5 — secrets.** The inventory should name an environment variable, never hold a value.
  VS Code has a stronger mechanism than `${env}`: a root-level `inputs` array with
  `promptString` + `password: true`, referenced as `${input:id}` — the secret never reaches a file.
  We already preserve `inputs` (we write only the `servers` key). Worth designing around.
- [ ] **D8 — launch at logon** via a scheduled task **in the user's session** (NOT a Windows service —
  session 0 has no desktop, so a service could never open Chrome or ComfyUI).

## Known wrinkle

`~/.claude.json` is a **live state file** Claude Code writes to itself. The write survived and was
read back, but if Claude Code ever rewrites that block, re-run `manager apply --write`. For this one
client the official CLI (`claude mcp add/remove`) is the safer writer — a future plugin option could
declare a *command* instead of a file, for clients that have one.

## Where to read what

`README.md` is the user-facing document: how to run it, the file formats, the plugin model, the
HTTP API, the MCP surface, and the traps. This file is the plan and the running log.

## Decisions to make (not yet settled)

- **Secrets.** `.vscode/mcp.json` holds API keys in cleartext today. Recommendation: the inventory
  never stores a secret — it references an env-var name; the client gets `"env": {"KEY": "${NAME}"}`.
- **Which servers to scope per client.** The `clients` field works; nothing is restricted yet.
- **Importing servers *back* from clients** — reading what a client already has and folding it into
  the inventory. Explicitly a later feature, not now (direction today is one-way: we tell clients).

## How to check

```
manager.exe list          list services + live state + why
manager.exe status <id>   detail + notes
manager.exe start <id>    start a service, wait for its probe to go green
manager.exe stop <id>     stop a service, wait for its probe to go red
```

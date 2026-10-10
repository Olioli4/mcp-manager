# MCP Manager — implementation plan

> For a coder coming in cold. Everything you need is here; you should not need the chat history or
> any prior session.

This is the task list produced from an independent security/correctness review (`project_review.md`)
and a UI note (`ui_audit_report.md`), both in this directory. The review was itself verified against
the code before this plan was written. Work top to bottom; each phase is independently committable.

---

## 0 · Orientation — what this project is

A local, native, Docker-free tool that:

1. Declares each MCP server **once** in `inventory.json`.
2. Writes it into each client's own config format (Claude Code, opencode, VS Code, AJEAN) via one
   **plugin** per client in `plugins/*.json`.
3. Starts the host programs a server depends on **only when a request arrives** — never on a timer,
   never at startup. This rule is load-bearing and must not be broken.

The interesting part is `proxy.go`: the manager is itself the transport. Each inventory server is
exposed on `/mcp/<name>`; a tool call arriving there checks the declared `needs_service`, starts it
if it is down, and forwards. Tool lists are cached on disk in `cache/<name>.tools.json` so they stay
visible while the upstream is off.

### Files that matter

| File | Role |
|---|---|
| `engine.go` | reads `services.json`; probe / start / stop host programs |
| `apply.go` | the inventory model + the config **writer** (renders one server per client, diffs, writes atomically with backup) |
| `proxy.go` | the `/mcp/<name>` proxy: lazy connect, tool cache, idle disconnect |
| `web.go` | the HTTP API (`/api/*`) and the `/mcp` MCP surface |
| `web/index.html` | the embedded UI (single file, no build step) |
| `main.go` | CLI entry + resident entry + first-run seeding |
| `tray.go` | systray icon, `-H=windowsgui` build |
| `apply_test.go` | writer tests (each named after the failure it prevents) |

### Build / test / format

```
go build ./...                                  # console + tray build together in CI
go build -o mcp-manager-tray.exe -ldflags "-H=windowsgui" .
go test -count=1 ./...                          # -count=1 defeats the Go test cache
go test -race -count=1 ./...
go vet ./...
gofmt -l .                                      # must print nothing
```

CI runs on **windows-latest** (this is a Windows-only program) on every push; the release workflow
fires on a tag. Line endings are pinned to LF in `.gitattributes` — do not "fix" the CRLF of a
checkout, and never let your editor rewrite `*.go` to CRLF.

---

## 1 · Read this first — history: an earlier agent had the fixes half-done

An earlier agent implemented part of the review and committed it as **`c8bb887` "Fix proxy
lifecycle, flaky backup test, and modernize Web UI"**. When this plan was written, that commit was
local and unpushed; it touched `apply.go`, `proxy.go` and `web/index.html`, and carried three
caveats: no tests of its own for the proxy changes, a Google-font `@import` in the UI, and an H1
backup naming fix that was a 32-bit lottery.

**Status 2026-10-10: pushed, reviewed, and built upon — all three caveats are closed.** Phase 0
replaced the proxy lifecycle with publish-and-borrow in one critical section under tests
(`proxy_test.go`, `-race` clean), removed the font `@import`, and made the backup names
`O_CREATE|O_EXCL` + retry. The history from `c8bb887` forward is the base; keep it, do not revert
it.

---

## 2 · Decisions (already resolved — follow these)

| Decision | Resolution | Why |
|---|---|---|
| The UI overhaul (glassmorphism, gradients, Inter font) | **Keep the polish, remove only the font fetch** | the polish is tasteful; the `@import` from Google is a network dependency that contradicts a tool whose identity is "local, nothing on the network" |
| H1 (flaky backup names) | **`O_CREATE|O_EXCL` + retry**, not the 32-bit randomness already in the tree | deterministic and provably collision-free; 32-bit is "practically fine" but still a lottery |
| Scope | **all HIGHs and MEDIUMs + doc corrections**; LOWs triaged at the end | a public repo with a wrong doc comment hurts more than a missing micro-animation |

---

## Phase 0 — verify and tighten the already-committed fixes

The four review findings below were already implemented in `c8bb887` (since pushed; see §1). They
were a draft verified by Phase 0, not done work:

- **H2** idle timer cut off in-flight calls → now an `inFlight` counter + `enter()`/`leave()`.
- **H3** nil-session deref in `forward` → `enter()` returns the session under the lock, `forward`
  checks for nil and returns a readable error.
- **H4** new server never shows tools → `ReloadProxies` calls `go b.warm()` for servers that are new
  *and* have an empty cache.
- **M1** the backend lock was held during `Engine.Start` (up to 90 s, freezing the UI) → a `connMu`
  (connect/start) is now split from `mu` (fast state).

**Read `enter()`/`leave()`/`idleTimeoutFunc()`/`ensureUpstream()` as an adversary.** Questions to
answer before trusting it:

- Can `ensureUpstream` re-arm the idle timer at the same moment `enter()` stops it, and in what
  order? (Both take `mu`, so order is defined — but state *why* that makes it correct.)
- Does `leave()` ever decrement `inFlight` below zero, or re-arm the timer while another call has
  already entered?
- Is `warm()`'s cache check (`len(b.tools) == 0`) safe under the lock, and does `warm()` running
  concurrently with a real `forward()` corrupt anything?

**Run `go test -race -count=1 ./...` and require it clean.** Then add tests (in a new
`proxy_test.go`) that would have failed *before* the change, each named after the failure it
prevents:

1. `TestIdleTimerDoesNotFireWhileCallInFlight` — enter a call, let the timer duration elapse, assert
   the session is still present; then leave, let it elapse again, assert it disconnects.
2. `TestForwardReturnsReadableErrorWhenSessionGone` — nil the session, call `forward`, assert no
   panic and an `IsError` result naming the server.
3. `TestNewServerWithEmptyCacheIsWarmed` — a fresh backend (empty cache) is warmed without the
   declared `needs_service` being started (`mayStart=false` must hold).

The third test is subtle: for a **stdio** server, "connect" spawns the process, which is allowed
(that is the MCP server itself, not the `needs_service`). Assert the *service* stays stopped, not
that nothing is spawned.

### 0.2 Remove the network dependency (`web/index.html`)

The overhaul added this as the very first CSS line:

```css
@import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&display=swap');
```

Delete that line. Keep `'Inter'` in the `font-family` stack (it is used only if locally installed —
harmless). The tool must never make a network request from the page.

**Verify:** `grep -c fonts.googleapis web/index.html` is `0`, and the page still renders (headless
Chrome screenshot at `#clients`, window 1180×560).

### 0.3 Settle H1 (`apply.go` — `backupPath`)

Replace the random suffix with a deterministic, collision-proof scheme. The current code:

```go
return fmt.Sprintf("%s.bak-%s-%08x", path, time.Now().Format("20060102-150405.000"), rand.Uint32())
```

Replace with: generate `path.bak-<timestamp>` and open it with `os.O_CREATE|os.O_EXCL|os.O_WRONLY`;
if the file exists (`errors.Is(err, os.ErrExist)`), append/increment a short counter and retry,
bounded (e.g. 10 attempts). This is provably collision-free because the filesystem arbitrates the
name instead of a random number.

You will need to split `backupPath`'s two jobs — naming and exclusive creation. A natural shape:
`createBackup(path string) (name string, err error)` that both picks the name and writes the file
exclusively, and have `writeDoc` use it directly.

**Verify:** `go test -count=50 -run TestBackupNamesDoNotCollide` green every run, and
`TestWriteIsAtomicAndTidy` still passes. Remove `math/rand` from `apply.go` if it is now unused.

### 0.4 Commit

Commit Phases 0.1–0.3 as **one commit** ("adopt and verify the proxy lifecycle fixes; drop the font
fetch; collision-proof backups"), then push. From here on each phase is its own commit.

---

## Phase 1 — remaining correctness (the MEDIUMs)

### 1.1 M3 — serialise and make inventory writes atomic (`web.go`)

`handleServer` and `setServerClient` each read `inventory.json`, mutate a `map[string]any`, and
`os.WriteFile` it — with no lock. Two rapid matrix clicks send two `/api/toggle` requests, and one
change is silently lost. The atomic writer exists but is not used here.

- Add a package-level `sync.Mutex` (call it `invMu`) held across each read→modify→write.
- Replace `os.WriteFile(invPath, …)` with the existing `writeFileAtomic(invPath, …)` (already in
  `apply.go`).

**Verify:** a test that runs N concurrent `setServerClient` calls on distinct servers and asserts the
resulting file parses and contains all N.

### 1.2 M2 — per-service start lock (`engine.go`)

Two servers needing the same service — or the tray and the proxy at the same moment — can both see
"stopped" and both launch it (ComfyUI twice → port conflict). Add a `map[string]*sync.Mutex` (or
`sync.Map` of mutexes) keyed by service id. `Start(id)` takes the lock for `id`, **re-probes inside
the lock**, and only launches if still stopped.

**Verify:** two goroutines calling `Start("comfyui")` at once leave exactly **one** process running.

### 1.3 M4 — only rebuild backends whose spec changed (`proxy.go`, `ReloadProxies`)

`ReloadProxies` builds a fresh map, then `dropSession()`s **every** old backend — so saving a note on
one server tears down the running session of every other server, including in-flight renders. (H3
means in-flight calls now survive on their held reference, but the session is still dropped and must
reconnect.)

Diff `old` vs `fresh` by spec (marshal each `InventoryServer` to JSON and compare). Keep a backend
whose spec is byte-identical; only `dropSession` backends that were **removed** or whose spec
**changed**. `warm()` already only targets new servers — keep that.

**Verify:** with server A's call in flight, save a change to server B; A's session must survive
(check the log or a live probe).

### 1.4 M5 — hide stdio console windows (`proxy.go`, `connect`)

The stdio branch runs `exec.Command(b.spec.Command, args...)` with no `SysProcAttr`, so a `-H=windowsgui`
tray binary spawns a **visible console** for `npx`/`node`. Add the same attribute `hidden()` in
`engine.go` already uses:

```go
cmd := exec.Command(b.spec.Command, args...)
cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
```

**Verify:** run the tray build, use a stdio server, confirm no console window flashes.

### 1.5 M8 — a `cmd:` start must not hang forever (`engine.go`, `launch`/`hidden`)

`launch` runs a `cmd:` entry through `hidden()` → `cmd.Output()`, which waits until **every** handle
to stdout closes. A launcher that leaves a long-lived child behind holds the pipe and blocks `Start`
with no timeout. Start `cmd:` entries detached (like `exe:` already is via
`DETACHED_PROCESS`/`Process.Release`), or give them a bounded timeout, and do not wait on inherited
pipes.

**Verify:** a `cmd:` entry whose child keeps stdout open still returns within the probe timeout.

### 1.6 M9 — children must not outlive the manager (`tray.go`, `proxy.go`)

`systray.Run(onReady, func(){})` has an **empty** `onExit`, and `CloseAll()` (in `proxy.go`) is never
called. Wire `CloseAll()` into `onExit`. For the stdio children spawned for servers, a Windows **Job
Object** with `KILL_ON_JOB_CLOSE` is the reliable way to ensure `npx`/`node` die when the manager
does (Windows does not kill a process's children on exit). This is Windows-specific syscall work;
scope it to the stdio `connect` path.

**Verify:** quit the tray app; `Get-Process node` (or `npx`) shows no orphans it spawned.

### 1.7 M7 — first-run seeding must not write into the CWD (`main.go`, `seedFromExample`)

`assetPath` falls back to a **relative** path when the file is not next to the exe, so a shortcut or
Task-Scheduler launch (CWD ≠ app dir) makes `seedFromExample` write `services.json`/`inventory.json`
into the working directory — exactly the case the README claims is handled. Resolve the target
directory from `os.Executable()` and write there, matching how `cache/` and the log already behave.

**Verify:** run the binary from a different CWD with no `services.json` present; the seeded file
appears next to the exe, not in the CWD.

### 1.8 M6 — `env` / `headers` support (`apply.go`, `web.go`, `proxy.go`, `web/index.html`)

`InventoryServer` cannot declare `env` (stdio API keys) or `headers` (HTTP auth), and `serverToMap`
in `web.go` **drops any hand-added field** on every UI save. This is the "secrets" work the README
lists as not done.

- Add `Env map[string]string` and `Headers map[string]string` to `InventoryServer`.
- `Render` (apply.go): emit `env` for stdio servers, `headers` for http servers.
- `serverToMap` (web.go): preserve both so a UI save does not erase them.
- `connect` (proxy.go): set `env` on the stdio command and send `headers` on the HTTP request.
- UI: add `env` and `headers` editors to the Inventory form (one `KEY=VALUE` per line is fine).

**Keep the rule:** the inventory stores a variable *name*, never a secret value. That rule is what
makes this safe to ship.

**Verify:** declare a server with `env`/`headers`, save from the UI, confirm both survive a
round-trip and appear in the client's rendered config.

---

## Phase 2 — documentation corrections (cheap, high-value)

- **"ABSENT or empty = ALL clients"** is wrong in `inventory.example.json` (line ~21) and `apply.go`
  (the `Clients` comment). The truth: **absent = all clients; `[]` = none.** Fix both.
- **"an unknown id is REFUSED at startup"** is wrong — it is refused at apply/save. Fix the wording.
- **The SECRETS RULE** in `inventory.json`/`inventory.example.json` says the client will receive
  `{"env": …}` — only true after 1.8 lands. Update the note to reflect reality.
- **The AJEAN CORS claim** in `PLUGINS.md` (":8090 drives us from its page") is likely wrong: the
  manager serves no CORS headers, so a browser `fetch` from :8090 cannot read responses, and a JSON
  POST would be preflighted and refused. Determine how AJEAN actually calls the manager (server-side?)
  and correct the sentence.
- `project_review.md` claims `.github/workflows/test.yml` is uncommitted — it is committed. Do not
  trust that line; the file is tracked.

---

## Phase 3 — the real UI fixes (not polish)

### 3.1 Narrow-width matrix (the review's HIGH, still open) — `web/index.html`

Still zero `@media` rules and no `overflow-x` wrapper. Add:

- an `overflow-x:auto` container around the clients matrix `<table>`, with a sticky first column;
- `@media (max-width: ~640px)` rules that stack the Services cards and collapse the form's
  two-column grid to one.

**Verify:** headless screenshot at 380px — every client column reachable, nothing clipped.

### 3.2 Accessibility (still open) — `web/index.html`

- `role="status" aria-live="polite"` on `#toast`; `aria-live="polite"` on `#report`.
- The status `.dot` must not be colour-only: add a text glyph or `aria-label`/`title` (a screen
  reader and a forced-colours render must distinguish "running" from "stopped").
- Wrap the Inventory editor in a real `<form>` with submit handling so Enter saves.
- Add `:focus-visible` styles so keyboard navigation is visible.

**Verify:** tab through the page; render with forced colours; the running/stopped distinction must
survive both.

### 3.3 Persistent error on first-load failure — `web/index.html`

The initial `load()` failure is still only a 5-second toast and a blank page. Render a persistent
panel inside `<main>` (endpoint tried + a Retry button) instead. The 5-second poll must not erase
it. Apply/report errors already go to `#report` (the right place) — just confirm they are shown and
never auto-dismissed.

---

## Phase 4 — LOW items, triaged

**Do** (each a few lines, low risk):

- rename-onto-existing-name warning: in `handleServer` (`web.go`), when `key != name` and `name`
  already exists, refuse or warn instead of silently overwriting.
- `probeProcess` (`engine.go`): guard `row[1]` with a `len(row) > 1` check.
- rune-safe `truncate` (`tray.go`): cut on rune boundaries, not bytes (a UTF-8 char can be split).
- `time.Tick` in `tray.go` reads `PollSeconds` once; make a reload change the poll rate.

**Defer** (note them as "known, not fixed" in `AUDIT.md` rather than implementing):

- symlink resolution before atomic replace (`filepath.EvalSymlinks`);
- backup rotation (keep last N);
- a hash of the previewed diff so "what you reviewed" is guaranteed to be "what gets written".

---

## Phase 5 — release

Once Phase 0–3 are committed and pushed:

1. `go build ./... && go vet ./... && gofmt -l .` — clean.
2. `go test -race -count=1 ./...` — clean.
3. `git tag v0.1.0 && git push --tags` — CI runs tests, GoReleaser builds both binaries for
   windows/amd64 + arm64 and opens a **draft** release. Read the draft before publishing it.

Do **not** cut the tag until Phases 0–3 are in.

---

## Phase 6 — SSE keepalive: the servers drop in opencode (found live, not in the review)

**Severity: HIGH** for reliability — it makes servers flake in and out, which is the opposite of
"declare once, always there". Confirm the severity during investigation: if opencode reconnects
transparently with no user-visible gap, downgrade to MEDIUM and treat as log noise.

**Symptom (measured, 2026-10-09, opencode's own log):**

```
mcp connected  server=vision tools=1
mcp connected  server=playwright tools=25
   … then, ~2 minutes later …
mcp transport error  server=vision|playwright|dcc|Agentic_Skills|system
error="SSE stream disconnected: TimeoutError: The operation timed out."
```

The pattern repeats every couple of minutes: connect → idle → SSE stream times out → reconnect. From
the user's side the servers are intermittently absent.

**Where to look.** The manager serves MCP over **Streamable HTTP** via the Go SDK's
`mcp.NewStreamableHTTPHandler` (`proxy.go`, `web.go`). That protocol keeps a long-lived SSE stream
open for server→client messages. If the handler sends **no keepalive/heartbeat**, an idle client —
opencode here — closes the stream on its own HTTP idle timeout. The 5-minute idle-disconnect in the
backend (`idleTimeout()`) is a *different* mechanism and does not match the ~2-minute cadence, so
the prime suspect is the SSE stream itself.

**Investigate, in this order:**

1. Read the Go SDK's `StreamableHTTPHandler` (and the `mcp.Server` options it takes) — does it have
   an SSE ping/keepalive interval, and is it off by default? (`go doc
   github.com/modelcontextprotocol/go-sdk/mcp.StreamableHTTPHandler` and its options.)
2. Confirm the two-minute figure is opencode's client idle timeout, not ours — grep the opencode log
   for the exact gap between `connected` and `transport error` across several cycles.
3. Decide whether the flapping is benign: does a tool call still succeed immediately after a
   "transport error", or is the server genuinely unreachable until the next reconnect? This is what
   sets HIGH vs MEDIUM.

**Likely fix.** Enable an SSE keepalive so the stream never idles out: either a handler option the
SDK already exposes, or — if it exposes none — a small periodic ping written into the handler's
underlying response writer, or a shorter server-side idle timer that recycles the stream before the
client gives up. Do **not** work around it by polling; the keepalive has to live at the transport.

**Verify:** start the manager, start a fresh opencode, and watch its log for **10+ minutes**: zero
`SSE stream disconnected` lines, the servers stay `mcp connected`, and a tool call succeeds after 10
minutes of sitting idle. Re-run for each of the four proxy servers.

---

## End-to-end verification checklist

1. `go build ./... && go vet ./... && gofmt -l .` — clean.
2. `go test -race -count=1 ./...` — clean, including the new proxy tests.
3. Fresh-clone: extract a `goreleaser --snapshot` archive into an empty dir; it seeds
   `services.json`/`inventory.json` and starts, with **no crash and no network fetch**.
4. The on-demand rule holds: with a service stopped, a tool call over `/mcp/<name>` starts it and
   forwards; the boot log contains **zero** start lines.
5. Two rapid matrix clicks lose nothing (M3); a reload of an unrelated server does not drop an
   in-flight session (M4); quitting the tray leaves no orphaned `node`/`npx` (M9).
6. Screenshots at 1400px and 380px: the matrix is reachable at both widths; the network panel shows
   no external font request.
7. opencode holds a connection for 10+ minutes with no `SSE stream disconnected` error (Phase 6).

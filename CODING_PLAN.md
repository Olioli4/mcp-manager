# MCP Manager — coding instructions: the tray + error batch

> For a coder coming in cold. Everything you need is here; you should not need the chat history or
> any prior session.

This is the task list for five small, independent fixes chosen from `BRAINSTORM.md` (items #1, #2,
#7, #3, #4). They are all **unblocked** — the Phase 6 / audit pass that preceded them is committed.
Work them in any order; each is independently committable.

---

## 0 · Orientation — what this project is

A local, native, Windows-only, Docker-free tool that:

1. Declares each MCP server **once** in `inventory.json`.
2. Writes it into each client's own config format (Claude Code, opencode, VS Code, AJEAN) via one
   **plugin** per client in `plugins/*.json`.
3. Starts the host programs a server depends on **only when a request arrives** — never on a timer,
   never at startup. This rule is load-bearing and must not be broken.

The resident mode is a **systray icon** (`tray.go`) plus a web UI and MCP surface. The tray menu is
built once at startup in `trayReady`; the `getlantern/systray` API cannot remove items afterwards, so
the menu is fixed from first launch until the manager restarts.

### Files that matter for this batch

| File | Role |
|---|---|
| `tray.go` | the systray menu (`trayReady`), the poll loop, the log helpers |
| `engine.go` | reads `services.json`; probe / start / stop host programs |
| `tray_test.go` | tests for the tray's **pure** parts (the menu itself is not unit-testable) |
| `engine_test.go` | engine tests on throwaway manifests in `t.TempDir()` |

### Build / test / format

```
go build ./...                                  # console + tray build together in CI
go build -o mcp-manager-tray.exe -ldflags "-H=windowsgui" .
go test -count=1 ./...                          # -count=1 defeats the Go test cache
go test -race -count=1 ./...
go vet ./...
gofmt -l .                                      # must print nothing
```

CI runs on **windows-latest** on every push. Line endings are pinned to LF in `.gitattributes` — do
not "fix" the CRLF of a checkout, and never let your editor rewrite `*.go` to CRLF.

Tests in this repo are **named after the failure they prevent** and are **mutation-tested** (remove the
fix, confirm the test fails, restore it). Follow that discipline for the new tests below.

---

## 1 · Short probe error — drop the `connectex` wall (`engine.go`)

**The defect.** `probeHTTP` (`engine.go`, around line 185) formats the raw error with `%v`, so a
stopped service produces a four-line message:

```
no response on http://127.0.0.1:9222/json/version (Get "http://127.0.0.1:9222/json/version": dial tcp 127.0.0.1:9222: connectex: No connection could be made because the target machine actively refused it.)
```

The `firstLine()` call on the error branch does nothing — the `*url.Error` is a single line, so the
whole wall passes through. That string then reaches the Services cards, the CLI, the tray, the web
API, the apply report, and the `services_list` MCP tool. Fixing it at the probe fixes all of them.

**The change.** In `probeHTTP`, the error branch currently reads:

```go
if err != nil {
    return false, fmt.Sprintf("no response on %s (%v)", p.HTTP, firstLine(err.Error()))
}
```

Replace it with:

```go
if err != nil {
    return false, fmt.Sprintf("no response on %s", p.HTTP)
}
```

This matches the style `probePort` already uses ("nothing is listening on …"). **Do not** add a
`logf` call in the probe: the tray polls every 5 seconds, so a stopped service would write ~17k log
lines a day, and the `connectex` text is not actionable.

**Test — `TestProbeHTTPReportsShortNotVerbose`** (in `engine_test.go`): call `probeHTTP` against a
closed loopback port (reuse `freePort(t)`), assert it returns `running == false` and that the detail
string is short and contains **neither** `dial tcp` **nor** `connectex`. A closed loopback port
refuses immediately, so the test is fast despite the 5 s client timeout.

---

## 2 · Collapse the five service submenus into one `Services` submenu (`tray.go`)

**The change.** Today `trayReady` gives each service its **own top-level** menu item (the loop around
line 58). Collapse to a single `Services` submenu with the per-service menus nested one level deeper:

```
before   Open the configuration… · Chrome › · vision › · ComfyUI › · Blender › · AJEAN › · Refresh · Log · Quit
after    Open the dashboard · Services › · Start with Windows · Quit
```

The mechanical change: add a top-level `Services` item, and make each service a **sub**-menu of it.
The current loop:

```go
menus := map[string]*svcMenu{}
man := e.Manifest()
for i := range man.Services {
    s := man.Services[i]
    parent := systray.AddMenuItem(s.Label, s.Why)
    state := parent.AddSubMenuItem("state: ?", "")
    state.Disable()
    start := parent.AddSubMenuItem("Start", "")
    stop := parent.AddSubMenuItem("Stop", "")
    menus[s.ID] = &svcMenu{parent: parent, state: state, start: start, stop: stop}
}
```

becomes:

```go
services := systray.AddMenuItem("Services", "start or stop the host programs")
menus := map[string]*svcMenu{}
man := e.Manifest()
for i := range man.Services {
    s := man.Services[i]
    parent := services.AddSubMenuItem(s.Label, s.Why)
    // … state / start / stop unchanged …
}
```

`getlantern/systray` supports nested submenus (`AddSubMenuItem` chains), so three levels — `Services`
→ service → `Start`/`Stop`/`state` — work. The `menus` map, the `svcMenu` struct, `refreshAll` and
`doAction` are **unchanged**: they act on `*svcMenu` and do not care how deep the item sits.

**Also remove `Refresh`.** The line `refresh := systray.AddMenuItem("Refresh", …)` (around line 72)
and its handler goroutine (`for range refresh.ClickedCh { refreshAll(e, menus) }`, around line 102)
go away. It is redundant: the periodic ticker already calls `refreshAll` every `poll_seconds`
(default 5 s), and re-reading `services.json` itself is the web UI's "Re-read files" button, not the
tray's job.

---

## 3 · Rename "Open the configuration…" → "Open the dashboard"

One string, three places. It is the whole dashboard, not just configuration.

- `tray.go` (~line 55): `systray.AddMenuItem("Open the configuration…", url)` → `"Open the dashboard"`.
- `README.md` (~line 45): `Then right-click the tray icon → **Open the configuration…**` → **Open the dashboard**.
- `PLAN.md` (~line 212): `Tray item **"Open the configuration…"** opens it.` → **"Open the dashboard"**.

(Do **not** touch `BRAINSTORM.md` — the brainstorm doc is updated by the orchestrator after this work
is verified, not by you.)

---

## 4 · Remove the log tray item (`tray.go`)

The tray item "Open the log" opens `mcp-manager.log` in Notepad — a crude way to expose a file. Remove
the item, keep the file.

- Remove `trayLog := systray.AddMenuItem("Open the log", …)` (~line 73).
- Remove its handler goroutine (`for range trayLog.ClickedCh { openLog() }`, ~line 107).
- Remove the now-dead `openLog()` function (~line 224).

**Keep:** `logf` and the `mcp-manager.log` writing — the tray is built `-H=windowsgui` (no console),
so that file is still the only record of what happened. **Keep** `exec_detached` — it is still used by
the "Open the dashboard" handler to launch the browser (`exec_detached("cmd", "/c", "start", "", url)`).

---

## 5 · "Start with Windows" — HKCU Run key

A new checkbox in the tray menu. Per-user, no admin, using the HKCU `Run` key (this is the item
BRAINSTORM calls #7).

**Where it lives.** New file `autostart_windows.go` (Windows-only, matching the existing
`job_windows.go` convention; the `_windows.go` suffix gives it the implicit build tag), plus
`autostart_windows_test.go`.

**What it writes.**

- Key: `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
- Value name: `MCP Manager`
- Value data: the **quoted** exe path — `"C:\path\mcp-manager-tray.exe"`. Quoting is mandatory: a
  path with spaces must arrive as one argument, not two.

No arguments in the value. `main.go` already treats "no args" as resident mode, and the tray binary
is built `-H=windowsgui`, so the Run entry boots a silent tray icon, nothing more.

**Use `golang.org/x/sys/windows/registry`.** It is already a direct dependency (`golang.org/x/sys
v0.41.0`), so there is **no `go.mod` change**. Do not shell out to PowerShell for this.

**Shape** (not final code — you write it):

```go
const runKeyPath  = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValueName = "MCP Manager"

// runValue quotes the exe path for the Run key.
func runValue(exe string) string { return `"` + exe + `"` }

// autostartEnabled reports whether the Run value is present.
func autostartEnabled() bool

// setAutostart adds or removes the Run value, pointing at os.Executable().
func setAutostart(on bool) error
```

For testability against a throwaway key rather than the real `Run` key, factor the read/write into
variants that take the key path (e.g. `autostartIn(path string) (bool, error)` and
`setAutostartIn(path string, on bool, value string) error`), and have the public functions call them
with `runKeyPath`. The HKCU `Run` key always exists on a real Windows profile, so `registry.OpenKey`
with `QUERY_VALUE` (read) / `SET_VALUE` (write + delete) is correct; do not `CreateKey` it.

**Wire into the menu** (`tray.go`, `trayReady`), in the collapsed layout from §2:

```go
systray.AddSeparator()
auto := systray.AddMenuItem("Start with Windows", "launch the manager when you sign in")
if autostartEnabled() {
    auto.Check()
}
systray.AddSeparator()
quit := systray.AddMenuItem("Quit", "stop the manager (does not touch the services)")
```

and a toggle handler in its own goroutine (same pattern as the existing handlers):

```go
go func() {
    for range auto.ClickedCh {
        if err := setAutostart(!autostartEnabled()); err != nil {
            logf("autostart toggle: %v", err)
            continue
        }
        if autostartEnabled() { auto.Check() } else { auto.Uncheck() }
    }
}()
```

**The invariant must hold.** Autostart-at-logon boots resident mode, which warms the proxy with
`mayStart=false` and starts **no service**. Adding the Run key must not change that — nothing in this
feature starts a service.

**Tests.**

- `TestRunValueQuotesTheExePath` — `runValue("C:\\Program Files\\MCP Manager\\mcp-manager-tray.exe")`
  must equal `"\"C:\\Program Files\\MCP Manager\\mcp-manager-tray.exe\""`. Mutation-check: an
  unquoted value would break autostart for any path with a space.
- `TestAutostartRoundTripAgainstATempKey` — create a throwaway key
  (`Software\mcp-manager-test-<random>`, via `registry.CreateKey(…, ALL_ACCESS)`), `setAutostartIn`
  on, read back `autostartIn` == true, `setAutostartIn` off, read back == false. Defer the key's
  deletion. **Never touch the real `Run` key in a test.**

---

## 6 · Doc truth pass (part of §3)

`README.md` line 45 and `PLAN.md` line 212 are the only two prose sites that name the old menu item
(verified by grep — `PLUGINS.md` does **not** mention it, despite an earlier note claiming it did).
Update exactly those two, nothing else.

---

## End-to-end verification checklist

1. `go build ./... && go vet ./... && gofmt -l .` — clean.
2. `go test -race -count=1 ./...` — clean, including the three new tests (§1, §5).
3. Build the tray binary, run it. Right-click → the menu reads
   `Open the dashboard · Services › · Start with Windows · Quit` — no `Refresh`, no `Open the log`.
   `Services ›` holds the per-service Start/Stop/state sub-submenus.
4. With a service stopped, its tray label reads `○ stopped — no response on http://…` (one line), and
   `mcp-manager.exe list` shows the same short detail — no `dial tcp`/`connectex`.
5. Toggle `Start with Windows` → the `Run` value appears under
   `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`; toggle off → it is gone.
6. Start resident with a service stopped → the boot log contains **zero** start lines (the on-demand
   rule holds through the new Run-key path).

## Commit shape

Two commits is the natural split, tests travelling with their change:

1. `engine: short probe error — drop the connectex wall` (`engine.go` + `engine_test.go`).
2. `tray: collapse to a Services submenu, rename the dashboard item, drop the log item, add start-with-Windows` (`tray.go` + `autostart_windows.go` + `autostart_windows_test.go` + the two doc strings).

# Adversarial audit — 2026-10-09

Done before making this public, on the assumption that publishing means other people's config files
and other people's machines.

**Method.** Not a read-through. Two independent reviewers worked the code and the running page
under an adversarial brief, and I probed the live system by hand. Every finding below is marked
with how it was established, and I re-verified the load-bearing ones myself rather than relaying
them — a report I have not checked is just a claim.

Reviewers ran `-race` builds, drove the real functions against throwaway files, and rendered the
real page at three widths. Nothing was fixed by them. The live manager was never killed, no service
was started or stopped, and the real config files were never written to during the audit.

---

## Status after repair

Fixed since this report was written. The findings below are kept as they were
found, with their evidence, because the reasoning is what stops them coming back.

| Finding | State |
|---|---|
| CRITICAL cross-origin → code execution | **fixed** `2c04a38`, replay now 403 |
| HIGH whole-file lossy round-trip | **fixed** — every untouched key is now held and written as raw bytes |
| HIGH panic on a `null` config file | **fixed** — two independent protections; test proven by mutation |
| HIGH partial apply discards its report | **fixed** — the report travels with the error, and the UI shows it |
| LOW-1 empty/whitespace/BOM aborts the whole apply | **fixed** — all read as "no config yet" |
| LOW-2 rewrites the file when already up to date | **fixed** — a no-op apply now touches nothing (verified: mtimes unchanged) |
| MEDIUM-4 no atomic write | **fixed** — temp file + fsync + rename, permissions preserved |
| MEDIUM-2 backup names collide | **fixed** — milliseconds plus random suffix |
| HIGH Apply has no confirmation | open — design agreed (render the diff, second click), not yet built |
| HIGH matrix unusable at narrow widths | open |
| MEDIUM-1 systray data race · MEDIUM-3 PowerShell interpolation · LOW-3…LOW-7 | open |

The project now has tests (`apply_test.go`), each named after the failure it
prevents. Every one was mutated — the protection removed, the test confirmed to
fail, then restored — because a test that cannot fail is worse than no test.

---

## CRITICAL — cross-origin request → code execution  ·  FIXED

**Found by me, by hand. Exploited end to end before the fix.**

The web server has no authentication. It listened on loopback and trusted that to be enough. Four
omissions chained:

```
no Origin check       a cross-origin POST reached the handlers
no Host validation    so DNS rebinding worked too
body parsed as JSON   regardless of Content-Type — a bare HTML <form> could send it,
                      so not even JavaScript was required
no auth               nothing else stood in the way
```

The chain, executed against the running server:

```
1. page POSTs /api/server   Content-Type: text/plain   Origin: evil.example
   -> 200  {"message":"server \"aas\" updated …"}
2. inventory.json: aas -> command "cmd", args ["/c","echo …> C:\Users\olivi\_pwned_marker.txt"]
3. any tool call on /mcp/aas — the CACHED tool list for that name survives, so the proxy
   still advertised its tools and connected upstream
4. marker file created
```

Step 3 is the part that is easy to miss when writing this: **replacing a server keeps its cached
tool list**, so the proxy still looks healthy and still hands out tool names — and calling one
launches whatever was planted.

The same request could also `POST /api/apply {write:true}` and rewrite all four clients' config
files, or start arbitrary declared programs on the desktop.

**Fixed** in `2c04a38`: `localOnly` wraps the whole mux. Host must be loopback; `Origin`, if
present, must be loopback. Loopback origins are accepted on *any* port, because AJEAN drives us
from its own page on `:8090` — safe, since a page cannot forge the `Origin` header.

Verified: all four vectors 403, the replay produces no marker, and the five legitimate paths still
return 200 (UI over `127.0.0.1` and `localhost`, plain API, loopback Origin, MCP client with no
Origin). Refusals are logged.

*Worth recording:* the Go MCP SDK ships exactly this protection, and `DisableLocalhostProtection`
is the flag we deliberately turned off so a container could reach vision-go. Our own server never
had it.

---

## HIGH — `apply` rewrites the whole file, and the round-trip is lossy

**Claimed by the correctness reviewer; verified by me against the real files.**

`readDoc` decodes the **entire** config into `map[string]any` — every JSON number becomes a
`float64` — and `writeDoc` re-marshals the whole document. So an apply does not "replace the MCP
block"; it round-trips a 77 KB file we do not own.

Measured, running the real functions:

```
18446744073709551615  ->  18446744073709552000
9007199254740993      ->  9007199254740992
3.141592653589793238462643 -> 3.141592653589793
non-UTF-8 byte 0xE9   ->  U+FFFD, and applyTo returned nil (success)
```

**The honest qualification:** I checked the three live targets. All are BOM-free, valid UTF-8, and
**0 of 544 number tokens** in `~/.claude.json` would change today. Nothing is being damaged right
now. The mechanism is armed and silent, not firing.

What *is* happening today: `~/.claude.json` has 62 top-level keys, not in sorted order, and every
apply rewrites them A-Z — a file we do not own, reordered on every write.

**Fix.** Never re-emit what we did not touch. Decode with `UseNumber()` and emit verbatim, or hold
the document as `map[string]json.RawMessage` and replace only `root_key`. Reject non-UTF-8 rather
than mangling it. Skip the write entirely when there is nothing to change (see LOW-2).

---

## HIGH — panic on a config file containing `null`

**Claimed by the reviewer; I reproduced it against the real `readDoc`.**

```
readDoc("<file containing null>")  ->  map[] (nil=true), err=<nil>
doc["mcpServers"] = …              ->  PANIC: assignment to entry in nil map
```

Reachable from `POST /api/apply` and from `manager apply --write`, where a background tray app
crashes on a file it was only asked to read. **Fix:** `if doc == nil { doc = map[string]any{} }`.

---

## HIGH — a partial apply discards the very report that says what happened

**Reviewer; I confirmed by reading `web.go`.**

`applyToAPI` deletes then saves; on the first failure it returns an error. The client is already
half-configured. `handleApply` then throws away the report buffer containing *which* operations
already ran, and answers `HTTP 500` alone. Same shape for the file writer: plugins apply in order,
so earlier clients are already written when a later one fails, and the web path hides that.

**Fix:** include the report in the error response. The user must learn what was already changed.

---

## HIGH — "Apply to clients" writes four real config files on one unguarded click

**UI reviewer; I verified the source myself.**

There is exactly one `confirm()` in the whole page, and it guards **Delete**, not Apply. Apply is
the primary-coloured button. Its blast radius is genuine deletion from all four clients — the hint
text says the block is *replaced*, but that is prose next to a big blue button.

**Fix — and I disagree with the reviewer here:** it proposed a `confirm()` dialog. I would rather
Apply **render the report first and require a second click to commit**. That reuses the Preview,
already built and honest, and shows *what* changes instead of asking a yes/no. Same protection,
less ceremony, and the diff is the thing that actually informs.

---

## HIGH — the matrix is unusable at narrow widths

**UI reviewer; I rendered it and looked.**

At 380 px the **VS Code column is entirely off-screen**, the hint clips mid-sentence, and the
header path is cut. Cause: zero `@media` rules in the stylesheet, and one unbreakable monospace
string in the header sets the document's minimum width wider than the viewport.

**Fix:** let the header wrap or ellipsize; put the matrix in an `overflow-x:auto` container with a
sticky first column; stack below ~640 px.

---

## MEDIUM

| # | Finding | How established |
|---|---|---|
| M1 | **Data race on `systray.MenuItem`.** The 5 s poll goroutine and the Refresh/Start/Stop goroutines all call `SetTitle`/`Enable`/`Disable`, which in systray v1.2.2 write fields with no lock. | `-race`, reproduced in isolation against the real `SetTitle` |
| M2 | **Backup filenames collide.** `…bak-20060102-150405` has second resolution; two applies in the same second overwrite each other, losing the earlier state. | Reproduced: two calls, one `.bak` on disk |
| M3 | **PowerShell interpolation in `killProcess`/`countProcesses`.** Manifest values are spliced into a script string. A process name containing `'` or `$` silently breaks the filter so `stop` no-ops; a crafted `only_if_cmdline_matches` could execute PowerShell. Local-config trust, not remote — but a silently broken stop is the likelier harm. | Constructed strings against the real builder |
| M4 | **No atomic write.** `writeDoc` ends in `os.WriteFile` = truncate in place. No temp-file + rename anywhere. A concurrent reader observed the target at size 0 and at partial sizes during a write. A crash or full disk destroys the file. | Proven window; the backup ordering is correct, which limits full loss |
| M5 | **If the initial page load fails, the page is blank** and the only clue self-destructs after 5 s. | UI reviewer, from the served code path (could not trigger live) |
| M6 | **Errors are unreadable.** The toast is the only error surface and auto-dismisses at 5 s. Real errors here are long, and the user must act on them. | UI reviewer |

---

## LOW

| # | Finding | How established |
|---|---|---|
| L1 | **Empty / whitespace / BOM file aborts the ENTIRE apply** — one bad client blocks all the others. The BOM case matters on Windows: Notepad and PowerShell 5.1 write them. | Reproduced against the real `readDoc` |
| L2 | **`apply --write` rewrites the file even when it prints "already up to date"** — a `.bak` proves it ran. This compounds HIGH-1. | Reviewer |
| L3 | **A malformed `services.json` makes the resident app exit instantly, code 1.** As a `-windowsgui` binary launched by double-click there is no console, so nothing appears at all — silent death from a typo in a file we tell you to hand-edit. | I ran it: process gone, exit 1 |
| L4 | **`CloseAll()` is never called** and the tray's `time.Tick` never stops. Upstream stdio children (`npx …`) outlive a quit. | Reviewer, grep |
| L5 | **No log rotation** — `manager.log` grows unbounded. | I checked |
| L6 | **Zero ARIA.** The toast announces nothing; status is colour-only and vanishes in forced-colours mode. | UI reviewer |
| L7 | **The Inventory form is not a `<form>`**, so Enter does nothing. | UI reviewer |

---

## Negative results — what held up under attack

Recorded because a negative result is a result, and because these are the parts I would otherwise
have to re-test after any change.

- **`diffBlock` converges and is idempotent.** Repeated applies report no changes. Key order does
  not matter. `args:[]` vs absent reads as "changed" once, then settles.
- **No unsynchronised shared-map race.** `proxiesReg` is swapped wholesale under a mutex and never
  mutated in place; `Engine.man` is an atomic pointer; the tray `menus` map is written once at
  setup. The only true race is M1.
- **The backup is written BEFORE the destructive write**, and a marshal failure after the backup
  leaves the original intact. The ordering is right.
- **Permission and path failures are honest.** Read-only file, missing parent, directory-as-file —
  all error, none reports false success. (Symlink behaviour unverified: Windows refused to create
  one.)
- **The Preview report is genuinely good** — target path, root key, per-server diff, the exact JSON
  block, and an explicit "(nothing is written)". It is the honest core of the tool and the thing
  the Apply flow should be built on.

---

## What I would fix, in order

1. **Whole-file round-trip** (HIGH-1) and **the nil-map panic** (HIGH-2). A tool whose entire job is
   rewriting config files must not re-emit bytes it did not touch, and must not panic on one.
2. **Partial-apply reporting** (HIGH-3) — never let a destructive operation fail silently about
   what it already did.
3. **Apply confirms through the diff**, not a dialog (HIGH-4), and **atomic writes** (M4). These
   are the same idea: make the destructive step reviewable and crash-safe.
4. **The security gate is already committed.** It was live and exploitable, so it did not wait for
   this report.

Everything else is real but survivable. Items L1–L2 are worth doing alongside (1) because they are
the same three lines of code.

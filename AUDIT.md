# Adversarial audit

Two rounds. **Round 2 (2026-10-10)** is at the end: it audits the four commits that landed after
Phase 6, and re-checks that round 1's fixes still hold.

## Round 1 — 2026-10-09

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
| HIGH Apply has no confirmation | **fixed** — the first click shows the diff and arms the button; only a second click writes |
| HIGH matrix unusable at narrow widths | **fixed** — Phase 3.1 (`ae669b2`): rebuilt as CSS Grid, sticky name column, stacking below 640 px; verified live at 380 px and 1280 px |
| MEDIUM-5 blank page on load failure · MEDIUM-6 unreadable errors | **fixed** — Phase 3.3 (`49454bd`): persistent panel in `<main>` with endpoint, reason, Retry; survives the failing polls; `#report` is `aria-live` and never auto-dismisses |
| LOW-3 silent death from a bad `services.json` | **fixed** — `fatalDialog` shows the parse failure in a modal box (`-windowsgui` has no console) |
| LOW-4 `CloseAll()` never called · orphaned children on quit | **fixed** — tray `onExit` calls `CloseAll()`, and the Job Object (KILL_ON_JOB_CLOSE) makes the kernel kill every child when the manager dies |
| LOW-6 zero ARIA · LOW-7 form not a `<form>` | **fixed** — Phase 3.2 (`6a1c49c`): live regions, labelled status dots that survive forced-colours, real `<form>` so Enter saves, visible focus ring |
| rename onto an existing name overwrote a third server | **fixed** — Phase 4: refused with 409 naming both servers; test proven by mutation |
| `probeProcess` panicked on a short tasklist row | **fixed** — Phase 4: `presentMsg` formats only what exists, `"pid ?"` for what it cannot know; test proven by mutation |
| tray label cut split UTF-8 characters · `time.Tick` ignored poll reload | **fixed** — Phase 4: rune-boundary `truncate`; ticker re-reads `poll_seconds` every tick (`tray_test.go`, mutation-proven) |
| MEDIUM-1 systray data race · MEDIUM-3 PowerShell interpolation · LOW-5 log rotation | **deferred — known, not fixed** (see below) |

The project now has tests (`apply_test.go`), each named after the failure it
prevents. Every one was mutated — the protection removed, the test confirmed to
fail, then restored — because a test that cannot fail is worse than no test.

---

## Deferred — known, not fixed

Deliberate scope decisions, recorded so they are not rediscovered as mysteries.
Each is real; none blocks v0.1.0.

| Finding | Why deferred |
|---|---|
| **Symlink resolution before atomic replace** — a temp-file rename replaces the *link*, not the target, if the config path is a symlink (`filepath.EvalSymlinks` would resolve it first) | Windows refused to create a test symlink during the audit; unverifiable behaviour on the dev machine. Config targets here are plain files. |
| **Backup rotation** — every apply writes a `.bak`, they accumulate, nothing prunes them | Disk cost is tiny, and deleting someone's backups by policy is worse than extra files. |
| **Preview-diff hash** — Apply re-computes the diff at write time; a file changed between Preview and Apply gets *the new* content written without you having reviewed it | Needs a content hash carried through the two-click flow; the second click already forces a human in the loop for the common case. |
| **`forward` has no call timeout of its own** — an MCP request blocks for as long as the caller's context lives (stdio upstream that never answers) | The proxy inherits the client's context; inventing our own timeout would kill slow legitimate calls (an `npx` cold start can take a minute). Clients set their own timeouts. |
| **`DETACHED_PROCESS` finding** — it silently kills console-app launches; every launch flag set is `CREATE_NO_WINDOW` + `HideWindow`, verified against real programs | Discovered and already applied everywhere; recorded because the natural first guess (DETACHED) looks right and is wrong. |
| **MEDIUM-1 systray data race** — poll goroutine and click goroutines call `SetTitle`/`Enable`/`Disable` with no lock (systray v1.2.2 has none) | A fix means serialising every tray mutation through one goroutine; the race is on label fields only, its observed cost is a garbled menu line, not a crash path. Carried forward. |
| **MEDIUM-3 PowerShell interpolation in `killProcess`/`countProcesses`** — manifest values are spliced into a script string | Local-config trust: whoever can edit `services.json` can already run anything the manager runs. A silently broken stop is the realistic harm; it is loud in the log. Carried forward. |
| **LOW-5 no log rotation** — `mcp-manager.log` grows unbounded | One line per action at human pace; gigabytes are years away. Carried forward. |

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
2. inventory.json: aas -> command "cmd", args ["/c","echo …> C:\Users\<you>\_pwned_marker.txt"]
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
present, must be loopback. Loopback origins are accepted on *any* port, so that a local web app can
drive the manager from its own page — safe, since a page cannot forge the `Origin` header.

*Correction (2026-10-10):* this paragraph claimed "AJEAN drives us from its own page on `:8090`".
The direction is the other way in the code that exists today: the manager calls **out** to AJEAN's
HTTP API (the `api` writer, `plugins/ajean.json`), and that outbound call is not governed by this
Origin check at all. A browser page on `:8090` driving *us* would in fact fail: the manager sends no
CORS headers, so a cross-origin `fetch` cannot read the responses, and a JSON POST is preflighted
and refused. The loopback-Origin allowance stays — it is harmless room for local apps — but it is
permission, not an integrated path.

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
| L5 | **No log rotation** — `mcp-manager.log` grows unbounded. | I checked |
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

---

# Round 2 — the four commits after Phase 6 · 2026-10-10

`9298b1f..ecc8ab7`. Four commits, and the shape of them is the story: **three fix defects the coder
introduced itself.**

| commit | what it fixed | introduced by |
|---|---|---|
| `2080f03` | every new session stalled **30 s** | its own Phase 6 keepalive |
| `53909f1` | a cold server could never be called (`-32602`) | the cache design |
| `ecc8ab7` | its error panel showed on **every** page load | Phase 3.3 |
| `f6dfba9` | first boot contacted the npm registry | the shipped example |

That is the right way round — it found these by *running* things (a checklist, screenshots, a real
client), not by re-reading its own code. But three self-inflicted regressions in one phase is a
statement about the phase.

**Method.** I read the entire diff myself rather than fanning out agents: 288 lines over six files,
and the value is one reader holding the whole picture. Everything below marked *executed* was run.

---

## Verified by execution

| Claim | How it was established | Result |
|---|---|---|
| the suite is green and silent | `gofmt -l` · `go vet` · `go build` · `go test -count=1 ./...` | clean; `ok 30.160s`, **no trailing warnings** |
| the wake gate is load-bearing | **mutation**: removed `AddReceivingMiddleware` | test fails — `calling "tools/call": unknown tool "ping"`. Reverted → passes 3.18 s |
| the `Flush` shape is load-bearing | **mutation**: restored `Flush() error` | test fails at **30.01 s** — `context deadline exceeded`. Reverted → passes 0.891 s |
| round 1's security fix survived | replayed the four vectors against the live server | cross-origin POST **403**, rebinding Host **403**, loopback GET **200**, loopback Origin **200**, no marker file |
| `[hidden]` beats `.card{display:flex}` | live page, 380/768/1280 | `#load-error` computed `display: none` at all three, `hidden` attribute intact |
| the matrix is usable at 380 px | live page | CSS-grid `div.matrix`, **6 sticky `div.cell-name` at `left: 0`**, inside a `.table-wrap` scrolling 496→348. No page-level overflow at any width |
| the editor is a real `<form>` | live page, editor opened | `<form id="server-form">`, 12 id'd fields, **all 12 labelled** (10 explicit `label[for]`, 2 checkboxes wrapped in their own `<label>`), 1 submit button |
| keyboard focus is visible | real `Tab` keypress on the live page | outline `solid 1.92px` on the focused button |
| ARIA survived | live page | 3 live regions: `#toast` (`role=status`, `aria-live`), `#report` (`aria-live=polite`), status dots (`role=img` + `aria-label`) |

The `Flush` diagnosis is exactly right, and I checked it against Go itself: `ResponseController.Flush`
matches `interface{ FlushError() error }`, then `http.Flusher` — `Flush()` with **no** error — and
only then `Unwrap`. `Flush() error` matched none of them, so the call became `ErrNotSupported`, the
SDK ignored it, and the stream's headers sat in `net/http`'s buffer until the first ping. The
*absence* of `Unwrap` is documented as the `-race`-proven hazard it is, not as a style choice.

---

## Findings

### F1 — the wake gate gives a hallucinated tool name a side effect

A `tools/call` naming something the instance does not know now runs
`ensureUpstream(ctx, mayStart=true)`. For a stdio upstream that **launches the process**; for a
`needs_service` upstream it **starts the service and waits for its probe**. If the name is then still
unknown, the SDK answers `-32602` exactly as before.

Before: an instant, free refusal. Now: a service-start attempt (up to the manifest timeout) and, for
stdio, a launch/kill cycle — on every call naming a tool that does not exist. Agent loops hallucinate
tool names; this turns a typo into work on the machine.

Not a security finding — the proxy is loopback-only and a *known* call already starts the same
service, so no new authority is granted. It is an ergonomics one. Cheapest fix: remember per instance
that a name was already woken for and still unknown, so the second bogus name is instant again.

### F2 — the first-boot fix closes the shipped case, not the class

`f6dfba9`'s own message is the evidence: a fresh extraction *seeded, warmed, and contacted the npm
registry*. The cure was to make the **example** inert. Warming is unchanged — `ReloadProxies` still
calls `warm()` for a new server with an empty cache, first load included, and warming a stdio server
means launching it. A user who replaces the placeholder with a real `npx -y @their/server@latest`
gets the same registry fetch on the next boot.

"First boot must not touch the network" is currently a property of the **sample file**, not of the
**program**. The wake gate now makes the alternative possible: let the first load skip warming and
let the first real call pay the connect. Worth deciding deliberately rather than by sample edit.

Related: `warm()`'s corrected comment now admits startup launches stdio upstreams when the cache is
empty. The rule — *only an incoming request may start a service, never a timer, never a startup* — is
the spine of this project, and it now has an exception recorded only in a comment in `proxy.go`. The
exception may well be right; it should be written down where the rule is written down.

---

## Gaps — claims that are not currently testable, or silently untested

- **G1 — two of the new tests can silently skip.** Both cold-cache tests `t.Skip` when the temp path
  contains a space (`fixture echo writes to a quoteless path`). On a profile like
  `C:\Users\John Smith\…` they **never run**, and `go test` still prints `ok`. A behaviour claim that
  quietly stops being tested depending on whose machine runs it is worse than an absent test. Fix by
  writing the marker from Go rather than through `cmd echo`, which removes the need for the skip.
- **G2 — the wake gate's failure path is untested.** The comment promises *"if waking fails, nothing
  is registered and the SDK's gate answers the name plainly"*. No test covers a service that refuses
  to start — the error path of a brand-new mechanism, where they most often break.
- **G3 — running the suite kills every `ping.exe` on the machine.** `newStartableEngine` probes and
  stops by process name, machine-wide (established by reading the fixture, not by execution), so
  `go test` will kill a `ping -t` the user happens to be running. The author documented it; it is
  still a side effect of the test suite on the real box.

---

## Not established — and three guesses of mine that measurement killed

Recorded because a negative result is a result, and because the next auditor will make the same
guesses.

- **A Playwright click on the nav timed out once** (30 s, "waiting for element to be visible, enabled
  and stable"), even though `elementFromPoint` returned the button itself with `pointer-events: auto`
  and nothing over it. **I could not reproduce a cause, and the evidence points away from the page:**
  the button's box was byte-identical across four samples over 2.7 s, `document.getAnimations()`
  showed nothing running, and the node survived 9 s — two poll ticks — as the same node, never
  replaced. Recorded as unexplained, not as a defect.
- **Three of my own first-pass UI numbers were false negatives**, each from an assumption rather than
  from the page: `tables: 0` (I assumed `<table>`; it is a CSS grid of divs), `stickyFirstCol: null`
  (I looked for `th`; the sticky cells are `div.cell-name`), `realForm: false` (the editor is not in
  the DOM until it is opened), and `outline-style: none` (I used programmatic `.focus()`, which does
  **not** set `:focus-visible` — only a real keypress does). Every one of these inverted on a proper
  measurement. Nothing in this round's findings survived on a single reading.

---

## What I would fix, in order

1. **G1** — the silent skip costs the suite its teeth on some machines, and the fix is a few lines
   (write the marker from Go).
2. **G2** — test the wake gate's failure path; it is the one branch of the new mechanism nobody has
   seen run.
3. **F1** — the bogus-name side effect. Small, and it is the difference between a typo being free and
   a typo starting a service.
4. **F2** — decide the first-boot invariant deliberately, and write the `warm()` exception down
   beside the rule it bends.
5. **G3** — test hygiene: scope the fixture's process name so the suite cannot kill a stranger's.

Round 1's deferred list is unchanged and still stands: the systray data race, PowerShell
interpolation in `killProcess`/`countProcesses`, log rotation, symlink resolution before atomic
replace, backup rotation, the preview-diff hash, and `forward`'s missing timeout of its own.

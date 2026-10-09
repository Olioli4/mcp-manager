# MCP Manager: full project review (2026-10-09)

**Scope.** Every Go source file, the web UI, the plugins, the example configs, CI/release config and the docs. I also ran `go vet` (clean), `gofmt -l` (clean), `go build` (OK) and `go test` (**fails intermittently**, see H1).

This review builds on [AUDIT.md](file:///e:/Coding/MCP_HUB/AUDIT.md). I don't repeat what it already covers. I list **new findings** first, then the current status of the audit's open items.

---

## Verdict

The writer (`apply.go`) is in good shape. The repairs from the audit are real and covered by tests, and the security gate (`localOnly`) plus HTML escaping in the UI hold up.

The weak spot is the **proxy lifecycle** (`proxy.go`). It has bugs that hurt the main promise ("the request arrives, the service starts, the call goes through"):
- In-flight calls can be cut off.
- A newly added server never shows any tools.
- The UI can freeze while a service starts.

---

## HIGH

### H1 · The test suite fails ~43% of the time, which also blocks releases
[backupPath](file:///e:/Coding/MCP_HUB/apply.go#L463-L465) uses milliseconds plus **16 random bits**. [TestBackupNamesDoNotCollide](file:///e:/Coding/MCP_HUB/apply_test.go#L164-L173) creates 200 names in the same millisecond. With only 16 random bits, a repeat is likely (birthday problem).

```
measured: go test -count=30 -run TestBackupNamesDoNotCollide → 13 / 30 FAIL
```

Both `release.yml` and the GoReleaser `before` hook run `go test`, so about 4 tags in 10 will fail to release.

**Fix:** open the backup with `O_CREATE|O_EXCL` and retry on `ErrExist`. This makes collisions impossible rather than unlikely. Alternatively, use 64 bits from `crypto/rand`.

### H2 · The idle timer can close a session while a call is still running
[armIdle](file:///e:/Coding/MCP_HUB/proxy.go#L189-L206) is armed only *after* a call succeeds, and nothing pauses it while a call is in progress. Example:
- Call A finishes at t=0, so the timer is set for t=5:00.
- Call B (say a 3-minute ComfyUI render) starts at t=4:00.
- At t=5:00 the timer fires and `Close()`s the session under call B.

**Fix:** keep a count of in-flight calls on the backend. Stop the timer when a call starts and re-arm it only when the count drops back to 0.

### H3 · `forward` can dereference a nil session
[forward](file:///e:/Coding/MCP_HUB/proxy.go#L157-L172) releases the lock between `ensureUpstream` and `sess := b.session`. In that gap, the idle timer, `dropSession`, or `ReloadProxies` (which acts on the *old* backend that in-flight requests still hold) can set the session to `nil`. Then `sess.CallTool` hits a nil pointer.

**Fix:** have `ensureUpstream` return the session it checked or created, still under the lock, and use that value.

### H4 · A newly added server never shows any tools
Tools are only discovered inside `ensureUpstream`, and that is only reached from:
- `forward`, which runs only when a client calls a tool;
- [warm](file:///e:/Coding/MCP_HUB/proxy.go#L237-L243), which is **never called anywhere**.

So a server added from the UI, with no `cache/<name>.tools.json` yet, offers **zero tools** at `/mcp/<name>`. No client can call a tool that isn't listed, so the cache is never filled. The UI then shows "not seen yet" forever. This contradicts the UI hint "reachable immediately".

**Fix:** after a server is saved, or via a "Discover tools" button, call `warm()` for that one server. It connects with `mayStart=false`, so it never starts a service, which keeps the "only a request starts a service" rule intact.

---

## MEDIUM

| # | Finding | Where |
|---|---|---|
| M1 | **The backend lock is held during `Engine.Start`**, up to 90 s of probe timeout plus 30 s of connect. [handleState](file:///e:/Coding/MCP_HUB/web.go#L276-L280) calls `b.state()`, which waits on the same lock. So the **whole page hangs** while a proxied request starts its service, and every other call to that server queues behind it. | [proxy.go#L95-L114](file:///e:/Coding/MCP_HUB/proxy.go#L95-L114) |
| M2 | **No per-service start lock.** Two servers that need the same service, or the tray and the proxy at the same moment, can both see "stopped" and both launch it. ComfyUI started twice means a port conflict. | [engine.go#L233-L265](file:///e:/Coding/MCP_HUB/engine.go#L233-L265) |
| M3 | **`inventory.json` writes are neither atomic nor serialized.** `handleServer` and `setServerClient` read, modify and then call `os.WriteFile`. Two quick matrix clicks send two concurrent `/api/toggle` requests, and **one change is silently lost**. Your atomic writer exists but isn't used here. | [web.go#L459-L467](file:///e:/Coding/MCP_HUB/web.go#L459-L467), [web.go#L663-L730](file:///e:/Coding/MCP_HUB/web.go#L663-L730) |
| M4 | **Every inventory save closes all upstream sessions**, including in-flight calls on *unrelated* servers. Editing a note on one server cuts off a running render on another. Only rebuild backends whose spec actually changed. | [proxy.go#L300-L314](file:///e:/Coding/MCP_HUB/proxy.go#L300-L314) |
| M5 | **stdio upstreams open console windows.** The tray binary is `-H=windowsgui`, and [connect](file:///e:/Coding/MCP_HUB/proxy.go#L145-L149) starts `npx` with no `SysProcAttr`. Windows gives a console child of a GUI process its own visible console window. `hidden()` already shows the fix: `CREATE_NO_WINDOW`. | proxy.go |
| M6 | **No `env` / `headers` in `InventoryServer`.** stdio servers that need API keys and HTTP servers that need auth can't be declared. Also, editing a server from the UI rewrites it through `serverToMap`, which **drops any field you added by hand**. | [apply.go#L28-L46](file:///e:/Coding/MCP_HUB/apply.go#L28-L46), [web.go#L530-L562](file:///e:/Coding/MCP_HUB/web.go#L530-L562) |
| M7 | **First-run seeding can write into the wrong folder.** If `services.json` doesn't exist next to the exe, [assetPath](file:///e:/Coding/MCP_HUB/engine.go#L478-L486) falls back to a *relative* path, and `seedFromExample` writes it into the **current working directory**. That is exactly the shortcut / Task Scheduler case the README says is handled. The cache and log already resolve to the exe folder, so the three behave inconsistently. | [main.go#L107-L131](file:///e:/Coding/MCP_HUB/main.go#L107-L131) |
| M8 | **A `cmd:` start can hang forever.** `launch` runs it through `hidden()` → `cmd.Output()`, which waits until *every* handle to stdout is closed. A launcher script that leaves a long-running child behind (holding the pipe) blocks `Start` with no timeout. | [engine.go#L357-L359](file:///e:/Coding/MCP_HUB/engine.go#L357-L359) |
| M9 | **stdio children outlive the manager.** The tray's `onExit` is `func(){}` and `CloseAll()` is never called (audit L4, still open). Windows doesn't kill a process's children when it exits, so a **Job Object** with `KILL_ON_JOB_CLOSE` is the reliable fix. | [tray.go#L21](file:///e:/Coding/MCP_HUB/tray.go#L21) |

---

## LOW

- **Renaming onto an existing name overwrites that server** with no warning. ([web.go#L440-L446](file:///e:/Coding/MCP_HUB/web.go#L440-L446))
- **What you review is not guaranteed to be what gets written.** "Write these changes" sends `write:true`, and the server *recomputes* the diff from the current inventory. If the file changed in between (CLI, another tab, an MCP call), the result differs from what was reviewed. Sending a hash of the previewed diff would close the gap.
- **The atomic rename replaces a symlink with a regular file.** Users who manage `~/.claude.json` as a dotfile symlink lose the link. Resolve it with `filepath.EvalSymlinks` first.
- **The "byte-for-byte" comment overstates what happens.** [readDoc](file:///e:/Coding/MCP_HUB/apply.go#L346) does keep values intact. But top-level keys still come out **sorted A–Z**, and whitespace is re-indented.
- **Backups pile up forever** next to the user's config files. Consider keeping only the last N.
- `probeProcess` reads `row[1]` without checking the row length. ([engine.go#L222](file:///e:/Coding/MCP_HUB/engine.go#L222))
- `truncate()` cuts by bytes and can split a UTF-8 character in the tray menu.
- `time.Tick` reads `PollSeconds` once, so a reload doesn't change the poll rate.
- `.github/workflows/test.yml` is **not committed** (it shows as `??` in git status).
- **Possible doc inaccuracy:** the claim that "AJEAN drives us from its page on :8090". The server sends no CORS headers, so a cross-origin `fetch` from :8090 can't read our responses, and a JSON POST triggers a preflight that gets a 405. This only works if AJEAN calls us server-side.

---

## Documentation contradicts the code

| Doc says | Code does |
|---|---|
| [inventory.example.json#L21](file:///e:/Coding/MCP_HUB/inventory.example.json#L21): "ABSENT **or empty** = ALL clients" | `[]` = **none** (`AppliesTo`, README). The same wrong comment is in [apply.go#L37](file:///e:/Coding/MCP_HUB/apply.go#L37). |
| "An unknown id is REFUSED **at startup**" | Refused only at **apply** / save time |
| "SECRETS RULE … the client will receive `{"env": …}`" | There is no `env` support at all (M6) |

---

## Status of AUDIT.md's open items

| Audit item | Now |
|---|---|
| HIGH matrix unusable at narrow widths | **still open**: there are still no `@media` rules and no `overflow-x` wrapper |
| M1 systray data race | **still open** |
| M3 PowerShell string interpolation | **still open**. Also, `[ ] * ?` in `only_if_cmdline_matches` act as `-like` wildcards. |
| M5 blank page if the first load fails | **still open** (only a 5 s toast) |
| M6 errors auto-dismiss after 5 s | **still open** |
| L3 silent exit on a bad `services.json` | **fixed** (`fatalDialog`) |
| L4 `CloseAll` never called | **still open** (see M9) |
| L5 no log rotation | **still open** |
| L6 no ARIA · L7 inventory form isn't a `<form>` | **still open** |

---

## Suggested order

1. **H1**, so CI and releases are reliable again. It's a few lines of code.
2. **H2 + H3 + M1** together: rework the backend's locking and lifecycle (in-flight counter, return the session, no lock held during `Start`).
3. **H4**: discover tools after a save.
4. **M3**: write the inventory under a mutex with `writeFileAtomic`.
5. **M5, M9**: hide the console windows and use a Job Object (both small and Windows-specific).
6. **M6**: design `env` / `headers` together with the secrets work the README lists as not done yet.

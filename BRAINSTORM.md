# Brainstorm

Ideas to discuss, before they become work. Append new ones at the bottom of **Open**; leave the
analysis empty — that is my half.

Each entry carries a **Verdict** so the list can be scanned without reading it: `do` · `discuss` ·
`defer` · `no`. Nothing here is a commitment. When something is decided it moves to **Settled** with
the decision written down; when it is refused it moves to **Dropped** *with the reason*, so it does
not come back every three months.

---

## Open

### 5 · Import what the clients already have

**The idea.** On first install, read each client's config, recognise the MCP servers already
configured (by name/command), and offer to import them.

**My read.** The strongest idea on this page, and it fixes the tool's **first impression** — today
a new user installs this and its first act is to *replace* their existing client config with an
empty inventory. Import turns that into "here is what you already have; shall I take it over?".

It is also **real detection**: reading what exists, rather than guessing a format. Most of the
plumbing is already there — each plugin knows its `config_path` and `root_key`.

**Cost.** The reading is easy. The work is the **reverse mapping**, and it has three traps:

```
unwrap our own proxy   an entry reading http://127.0.0.1:8787/mcp/vision is OURS.
                       Imported naively it becomes a server pointing at ourselves.
strip client quirks    opencode's ["cmd","/c","npx",…] must lose the shell prefix
infer the transport    VS Code omits "type" — infer from command-vs-url presence
```

**Traps.** Preview before importing; never auto-apply. Conflicts (same name, different content) must
be shown, not resolved silently.

**Verdict: do** — first, once the current fix pass lands.

---

### 6 · Per-tool enable/disable, to save context

**The idea.** A tab that enables/disables individual tools per service — even if a server is listed
in a client, not listing its tools saves context.

**My read.** Better than it looks, because the hard part is **already built**:

```
proxy.go    rebuildServer() exposes exactly the tools we choose to AddTool
web.go      apiProxy already ships each server's tool list to the UI
index.html  already renders "25 tool(s) cached"
```

So this is mostly wiring. The design call that matters: **filter in our proxy**, not per client.
The proxy is the chokepoint every client feeds from, so filtering `tools/list` there works for every
client uniformly — including clients whose config has no tool-filtering concept at all.

**Traps.** AJEAN has its own native `disabledTools`. Do **not** use it. Two mechanisms means two
places to change and guaranteed drift; one source of truth.

`direct: true` servers bypass the proxy, so they cannot be filtered — worth saying in the UI rather
than leaving a toggle that silently does nothing.

**Verdict: do** — after import.

---

### 8 · Windows installer

**The idea.** An installer with options: install as a service, path, etc.

**My read.** Defer the installer — and **remove "install as a service" outright**. Same reason as
#7: session 0 has no desktop, so a service could never start the programs this tool exists to
start. An installer offering it would be a trap that looks like it works.

Installers are also a maintenance tax: code signing, SmartScreen warnings, an upgrade path to
babysit. **Winget + Scoop via GoReleaser gets ~90% of the value for near-zero work** — it is already
written and commented in `.goreleaser.yaml`, waiting for a tag.

**Verdict: defer** — winget/scoop first, per-user only if ever, never as a service.

---

### 9 · Instrument the proxy: usage, errors, context cost, attribution

**The idea.** We stream every tool call. So: call counts, error counts — and can we tell *which agent*
made the call (claude opus, deepseek, qwen)?

**My read — what the protocol actually gives us** (verified in the SDK, v1.8.0):

```
harness    YES, automatic. ServerRequest.Session.InitializeParams().ClientInfo
           → {Name, Title, Version}: "claude-code", "opencode", "Antigravity".
           That is the CLIENT, not the model.

model      NO. MCP carries no model identity; opencode on qwen and opencode on
           deepseek are identical on the wire.

headers    Every request also carries Extra.Header — the raw HTTP headers. And the
           `headers` feature being built in M6 is exactly what lets a client
           DECLARE one. So `X-Model: qwen2.5-32b` is visible if the client sends
           it. Model attribution is available when declared, never inferred.
```

**The number we already hold.** `cache/<name>.tools.json` stores the full schemas — `name`,
`description`, `inputSchema`:

```
playwright   25 tools   29,504 bytes of schema   ≈ 7,000 tokens per conversation (estimate)
```

So the context cost of a server is computable **before any call**. That is the user's whole
motivation turned into a number, on the exact axis they care about — and no other tool can compute
it, because no other tool holds the schemas.

**Candidate features, ranked.**

```
A  context cost per server      the headline. Later: schema-size × never-called, so
                                filtering stops being guesswork.
B  usage counts                 per tool / server / client. Cheap; makes #6 actionable.
C  error + latency stats        which servers fail, how slow. Also exposes idle-unload
                                as data: ~25 s cold, ~200 ms warm.
D  cold-start attribution       we own the prerequisite, so we can attribute the wait to
                                "ComfyUI had to boot" rather than to the tool. Novel.
E  per-client attribution       who actually uses what; usually finds dead weight.
```

**Scope correction (2026-10-10):** this is **logging, not storage**. No event store, no aggregation
engine, no new subsystem — a line appended to the existing log. That drops it from "a phase" to
"small", and it should be read that way from here on.

**The sharp edge — decide before building.** We see tool **arguments**: file paths, code, document
contents, occasionally credentials. That is a privacy decision, not a technical one.

```
default   count and time only — never the argument values
opt-in    arguments only if explicitly asked for
never     ship argument logging on because it was easy
```

**Verdict: do** — small, once the current fix pass lands.

---

### 10 · Cut context cost — trim the text, or defer the list

**The idea.** We control what `tools/list` returns. Could we trim it — are the descriptions too
verbose? (Prompted by playwright's 30 KB of schema.) And a follow-up: are all tools passed at once,
or is there a lazy-loading mechanism — name and intent first, full schema only when the model wants
to use that tool?

**Are all tools passed? Yes.** Verified in our own code — `rebuildServer()` in `proxy.go` loops over
every cached tool and `AddTool`s it with its full schema. Nothing is deferred today.

**And lazy loading is real.** The MCP client best-practices docs recommend *"offer multiple detail
levels"* — name-only, then name + description, then full schema on demand. A formal extension does
exactly the second half of the idea: `tools/list` returns a one-sentence description with a **stub**
`inputSchema`, and the full schema is fetched from a `tool_descriptions` resource. It claims an
**80–90% token reduction**. Anthropic's Tool Search (`defer_loading: true` + `tool_search_tool`) is
the same mechanism.

**We already run one.** The DCC gateway this project fronts exposes 4 tools (`search`/`describe`/
`load_skill`/`call`) for an arbitrary number of upstream ones. That *is* lazy schema loading, working
on this machine today.

**The result that removes the accuracy worry.** An earlier version of this entry hedged that hiding
schemas might cost tool-selection accuracy. The evidence says the opposite: one implementation
reports a **~85% cut in tool-definition tokens AND an accuracy increase (Opus 79.5 → 88.1)**. Fewer,
better-scoped tools help the model choose.

**The trap that decides the design.** Most providers cache the prompt prefix *including the `tools`
array*. Adding or removing definitions mid-conversation invalidates that cache, and the miss can
cost more than the definitions removed. So incremental revelation ("now loading tool 12") can be
net-negative. Two mitigations, pointing the same way: append new tools **after** the cache
breakpoint without re-sorting, or expose **one stable meta-tool** (`call_tool({name, args})`) so the
array never changes at all. The second is what the DCC gateway does.

**My read — measured, and it contradicts the premise.** For playwright's 25 tools, on the wire
(compact — the only `MarshalIndent` in the SDK is in a test):

```
tool descriptions    1,785 octets    9%   <- what was suspected; almost nothing
inputSchema         14,270 octets   68%   <- the real weight
annotations          2,895 octets   14%   <- pure restatement, see below
names                  431 octets    2%
```

The verbose prose is not in the tool descriptions — it is in the **parameter** descriptions inside
the schema (6,532 of the schema's 14,270; the other 7,738 is structure: field names, types, enums,
which is the contract and cannot be trimmed).

**The free win.** All 25 tools carry all five annotation fields, and four of them are exactly MCP's
defaults — `destructiveHint: true`, `readOnlyHint: false`, `idempotentHint: false`,
`openWorldHint: true` — so they restate what the client already assumes. Only `title` says anything,
and it mostly prettifies the tool name ("Close browser" for `browser_close`). Dropping defaults is
near-zero risk by definition.

```
drop default annotations    -14%   near-zero risk
trim parameter descriptions -31%   accuracy risk — these teach the model to fill arguments
trim tool descriptions       -9%   not worth the risk to tool selection
structure                     0%   it is the contract
```

**The two levers, and they are not equal.**

```
trim the text     -14% near-zero risk (default annotations), -31% with accuracy risk
                  -> at best ~45% of 20 KB, and the risky half is real
defer the list    20 KB -> a few hundred bytes, ~85%, accuracy UP, and already proven
                  -> by the DCC gateway running on this machine
```

Trimming shrinks the *text*; deferring shrinks the *list*. For context economy the second wins by an
order of magnitude, and the proxy is the only place it can be done **once for every client** —
Claude Code, opencode, VS Code and AJEAN all feed from it, and none of them has to support anything.

Where trimming still earns a place is **per client**: full schemas to Claude Code, trimmed and
default-free to opencode on a small local model. Same server, two views — which only a proxy can do,
and is what #6 sets up.

**A first-hand objection, and it is the load-bearing one (2026-10-10).** BrainZ already tried this
and it failed — *"the agent then didn't know what to use, as the details had to be discovered"*.
That experience outweighs the benchmarks below, because it is the same problem on the same kind of
system.

It does not kill the idea; it kills **one shape** of it. Two designs were being conflated:

```
DCC-style        4 meta-tools, NOTHING visible. The agent must search blind — it cannot
                 know `playwright` exists, so it never searches for "browser".
                 <- this is the failure BrainZ hit.

schema-light     ALL tool names + a one-line intent stay VISIBLE; only the inputSchema is
                 deferred. The agent sees every capability; details arrive on demand.
```

The failure was **discoverability**, and it came from hiding the map — while the map is cheap:

```
names                  431    2%   keep — this IS the map
tool descriptions    1,785    9%   keep — this is the "intent"
inputSchema         14,270   68%   defer — nearly the whole payload, and unwanted until a
                                   tool is actually chosen
annotations          2,895   14%   drop — restated defaults
```

Hiding 11% to save 11% is what broke the agent; the 68% worth saving sits entirely in the schema.
The middle path keeps the map and defers the bulk.

**How to test it before believing it.** The proxy can serve two surfaces from the same servers, so
the comparison is natural: Claude Code on schema-light, opencode on full schemas, same tasks. Measure
four things per task — tokens, **whether the model reached for the right tool at all** (the BrainZ
failure, and the metric a token count would hide), whether the call succeeded, and the extra round
trips `search → describe → call` introduces.

**Test it on both model classes.** The honest split is cloud vs local; if a strong model navigates a
deferred surface and a small local one flounders, the answer is neither "defer" nor "don't" but
**per client** — which is what the matrix already exists to express.

**Verdict: discuss, and test before building** — the annotation drop is cheap and belongs with #6.
The deferred surface is the bigger prize but is genuinely unproven *on this machine and these
models*; the DCC gateway is the closest existing evidence, and it costs the agent its map.

**Sources:** [MCP client best practices](https://modelcontextprotocol.io/docs/2025-03-26/develop/clients/client-best-practices#programmatic-tool-calling-/-code-mode) ·
[progressive-disclosure extension](https://huggingface.co/spaces/MCP-1st-Birthday/mcp-extension-progressive-disclosure) ·
[two-stage tool surface with prompt-cache notes](https://raw.githubusercontent.com/caliban-ai/caliban/817bb09b06f75b7bcb63fc0a97ff923fad44dfe7/docs/superpowers/specs/2026-05-31-two-stage-tool-surface-design.md) ·
[deferral + accuracy result](https://github.com/anthony-chaudhary/fak/issues/3231)

---

## Settled

### 1 · Verbose, useless error messages
**Decided: do** — dropped the `connectex` wall in `probeHTTP`; the detail is now `no response on <url>`.
Done in `dc03efa` (2026-10-11). One deviation from the note above: the raw error is **not** logged,
because the 5 s poll would spam the log — the short message is the fix.

### 2 · The tray duplicates the start/stop in the web UI
**Decided: do** — collapsed the five per-service submenus into one `Services` submenu; `Refresh` was
dropped with them (the 5 s ticker already re-reads state). Done in `9c4661e`.

### 3 · "Open the configuration…" is misnamed
**Decided: do** — renamed to **Open the dashboard** in the menu, README, and PLAN. Done in `9c4661e`.

### 4 · The log should not be a tray item
**Decided: do the removal** — removed the "Open the log" item, its handler, and `openLog()`; kept
writing `mcp-manager.log` (the tray's only trace). A log view stays `discuss`. Done in `9c4661e`.

### 7 · Start with the operating system
**Decided: do** — "Start with Windows" checkbox via the HKCU `Run` key (per-user, no admin); value is
the quoted tray exe path, no args. Done in `9c4661e`. This is D8's idea, via the Run key rather than
a scheduled task.

### 11 · The comfyui / Blender separation
**Decided: split** — `dcc` became `dcc-comfyui` (needs comfyui) and `dcc-blender` (needs blender),
each auto-starting its own host app on first use. Done 2026-10-11: instruction `0668aad`; the split
itself is in `inventory.json` (machine-specific).

### 12 · Using a tool must visibly mean "start it"
**Decided: do** — rewrote the `system` instructions around "a tool call starts what it needs" and made
`servers_list` say "starts automatically on first use" on a stopped prerequisite. Done 2026-10-11
(`125b6c3`).

## Dropped

*(nothing yet — when something is refused, it moves here **with the reason**, so it does not come
back)*

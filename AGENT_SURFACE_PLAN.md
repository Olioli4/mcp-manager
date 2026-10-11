# MCP Manager — coding instructions: the agent-facing surface (#12)

> For a coder coming in cold. Everything you need is here; you should not need the chat history.

## The problem (observed live, not theoretical)

Agents see a service STOPPED and shy away from the tool, because nothing tells them that **using a
tool starts the service**. Observed twice in a parallel window: playwright was avoided because it
looked down; Blender went worse — the agent launched a *headless* Blender instead of the visible one
(the Blender tools live inside the visible Blender, so headless is exactly wrong).

The mechanism already works: the proxy's wake-gate starts a server's `needs_service` on the first
tool call. What is missing is the *words* on the `system` MCP surface.

## Files

| File | Role |
|---|---|
| `mcp.go` | the `system` MCP server — `Instructions` + `services_list` / `servers_list` / `service_start` / `service_stop` |

Build / test / format (unchanged from the repo norm):

```
go build ./... && go vet ./... && gofmt -l . && go test -race -count=1 ./...
```

## The changes (all in `mcp.go`)

### 1. Rewrite the `system` Instructions (`newMCPServer`, the `Instructions:` field, ~line 30)

Today it ends with *"Some MCP servers (Blender, ComfyUI) exist ONLY if their application is running:
start it first, the tools will appear afterwards."* That sentence actively teaches the agent to start
before using — the exact shyness this fixes.

Replace with the invariant, stated plainly. You own the wording; keep these three clauses:

- **a tool call starts what it needs** — a STOPPED service is not a reason to avoid the tool;
- **no manual pre-start** — the manager starts the service; use `service_start` only if a call fails;
- **Blender is the exception** — not auto-started (it opens a window): open the real Blender yourself,
  **never** a headless instance.

Suggested text:

```
A tool call starts what it needs. If a service is STOPPED, use the tool anyway — the manager starts
the service for you; "down" does not mean "unavailable". You do not need to start anything yourself;
use service_start only if a tool call actually fails. Exception: Blender is not auto-started (it opens
a window) — open the real Blender yourself, never a headless instance. services_list says what is
running and why; service_stop stops a program.
```

### 2. `servers_list`: the "requires service" line must say "using = starting" (~line 111)

Today a stopped prerequisite reads `requires service "chrome-cdp" : STOPPED — no response on …`.
Append the affordance when the state is STOPPED:

```go
} else {
    state = "STOPPED — " + detail + " (starts automatically when you call its tools)"
}
```

Every `needs_service` is auto-startable (the wake-gate starts it on a call), so the hint is uniform —
no per-service branching.

### 3. Keep the tool description honest

`servers_list`'s description already says "Useful when a server is unreachable: it says whether it
exists and what it is missing." Add one clause so the description itself carries the rule: a STOPPED
service means "call the tool and it starts", not "unavailable".

## Test

This is a wording change; the real proof is behavioural (an agent, shown STOPPED, must still reach
for the tool). For regression protection, extract the `Instructions` text to a package-level `const
systemInstructions` and add `TestSystemInstructionsStateUsingEqualsStarting` asserting it contains
both "starts" and "headless" — mutation-check: removing either phrase fails the test. The `servers_list`
hint is one line; a test there is optional unless it falls out naturally.

## Verification

```
go build ./... && go vet ./... && gofmt -l .   # clean
go test -race -count=1 ./...                   # clean, including the new test
```

Then run the manager and read the *actual* surface an agent receives: `tools/list` on `/mcp` returns
the new `Instructions`, and `servers_list` prints the "starts automatically" hint for a stopped
prerequisite.

The real proof is behavioural and lands after this ships: a real agent, shown a STOPPED server,
reaches for the tool instead of avoiding it — and for Blender, opens the real app, never headless.

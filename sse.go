package main

// SSE keepalive — why this file exists.
//
// The MCP Streamable HTTP protocol keeps a long-lived GET stream open per
// session for server→client messages. An idle client that receives NOTHING on
// that stream eventually closes it: opencode's log shows the exact cadence —
// connect, then 180 s later "SSE stream disconnected: TimeoutError", every
// cycle, for every server at the same instant (a client-side timer, not ours).
//
// The Go SDK v1.8.0 writes exactly one SSE comment when a stream opens
// (": ok") and nothing after it; StreamableHTTPOptions has no keepalive field
// (the client side has KeepAlive, the server side does not), and its own
// comment in streamable.go says clients should ping to stay live — which they
// do not. The spec's remedy is server-side: periodic SSE comments, which
// clients ignore per RFC. So the ping lives here, at the transport, on our own
// handler — no polling of services, no session tampering.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sseKeepaliveInterval: comfortably below the shortest known client idle
// timeout (opencode: 180 s), long enough that the traffic is noise.
// A variable so tests can run the mechanism on a short clock.
var sseKeepaliveInterval = 30 * time.Second

// keepaliveHandler wraps the SDK's StreamableHTTPHandler so every request
// gets a response writer that emits periodic SSE comments while the handler
// holds the stream open. When ServeHTTP returns, the writer is dead: pings
// stop before it, never after.
func keepaliveHandler(getServer func(*http.Request) *mcp.Server) http.Handler {
	inner := mcp.NewStreamableHTTPHandler(getServer, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ka := newKeepaliveWriter(w, r.Context())
		defer ka.stop()
		inner.ServeHTTP(ka, r)
	})
}

type keepaliveWriter struct {
	w       http.ResponseWriter
	ctx     context.Context
	mu      sync.Mutex // every write — SDK events and pings — is serialised here
	stopped bool       // checked under mu: no write ever lands after ServeHTTP returns
	done    chan struct{}
	stopOne sync.Once
}

func newKeepaliveWriter(w http.ResponseWriter, ctx context.Context) *keepaliveWriter {
	k := &keepaliveWriter{w: w, ctx: ctx, done: make(chan struct{})}
	go func() {
		t := time.NewTicker(sseKeepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-k.done:
				return
			case <-ctx.Done():
				return // the client is gone; there is nobody left to ping
			case <-t.C:
				k.ping()
			}
		}
	}()
	return k
}

// ping writes an SSE comment: ignored by every conformant client, and it
// resets the client's idle timer. Errors are swallowed — a broken stream is
// reaped when the handler itself returns; the ping must not invent a second
// failure path.
func (k *keepaliveWriter) ping() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return
	}
	_, _ = fmt.Fprint(k.w, ": ping\n\n")
	k.flushLocked()
}

func (k *keepaliveWriter) stop() {
	k.stopOne.Do(func() {
		k.mu.Lock()
		k.stopped = true
		k.mu.Unlock()
		close(k.done)
	})
}

// http.ResponseWriter forwarding. Unwrap is deliberately ABSENT, and Flush
// must have EXACTLY the http.Flusher shape — both facts are load-bearing,
// proven by TestStatefulClientConnectsWithoutWaitingForAPing plus -race.
// The SDK's streamable server releases the standalone SSE stream's headers
// with http.NewResponseController(w).Flush(). That walk matches http.Flusher
// (Flush(), NO error return) at our level, then stops. Give Flush an error
// return and it is invisible: ErrNotSupported, headers sit in net/http's
// buffer, every stateful client stalls in Connect until the first ping —
// 30 s per new session. Add Unwrap and the walk bypasses this mutex onto the
// raw response, whose Flush touches the connection buffer outside net/http's
// own write lock: a ping racing it is a DATA RACE (bufio slicecopy, caught by
// -race). Shaped as a Flusher, every flush the SDK asks for lands HERE,
// serialised with every write. One stream, one lock.

func (k *keepaliveWriter) Header() http.Header { return k.w.Header() }

func (k *keepaliveWriter) Write(b []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.w.Write(b)
}

func (k *keepaliveWriter) WriteHeader(code int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.w.WriteHeader(code)
}

func (k *keepaliveWriter) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	_ = k.flushLocked()
}

func (k *keepaliveWriter) flushLocked() error {
	return http.NewResponseController(k.w).Flush()
}

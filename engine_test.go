package main

// The engine-side tests.
//
// Everything here runs on throwaway manifests in t.TempDir() and throwaway
// programs: a TCP listener that closes itself after a few seconds. No real
// service is started, killed, or touched, and nothing writes into the user's
// configuration.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A manifest on disk, for an engine that never touches the user's real files.
func writeManifest(t *testing.T, dir, content string) *Engine {
	t.Helper()
	path := filepath.Join(dir, "services.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := LoadEngine(path)
	if err != nil {
		t.Fatalf("manifest rejected: %v", err)
	}
	return e
}

// listenerManifest starts a PowerShell TCP listener on the loopback port and
// appends one line to the log file every time the launched program runs, so the
// number of lines in that file IS the number of launches.
func listenerManifest(t *testing.T, dir string, port int, logPath string) *Engine {
	t.Helper()
	script := fmt.Sprintf(
		"Add-Content -LiteralPath '%s' -Value 'launch'; "+
			"$l=[System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback,%d); "+
			"try { $l.Start(); Start-Sleep -Seconds 3 } finally { $l.Stop() }",
		logPath, port)
	return writeManifest(t, dir, fmt.Sprintf(`{
  "settings": {"web_port": 8787, "default_probe_timeout_s": 20, "poll_seconds": 5},
  "services": [{
    "id": "starter",
    "label": "Starter",
    "why": "test fixture: listens on a port so the probe can see it",
    "start": {"exe": "powershell.exe", "args": ["-NoProfile", "-NonInteractive", "-Command", "%s"]},
    "stop": {"process": "no-such-process-for-this-test.exe"},
    "probe": {"port": %d}
  }]
}`, script, port))
}

// Start must report a launched program by what the probe sees, and that starts
// with the program ACTUALLY RUNNING. This is the regression test for the launch
// flags: under DETACHED_PROCESS, powershell.exe never runs at all — Start()
// returns no error, the tray says "started", and nothing is there. The engine
// asked for a program; the probe confirms it; the log proves it ran.
//
// Mutation-checked: with CreationFlags 0x00000008 (DETACHED_PROCESS) this test
// fails with "nobody launched at all" — the exact silent failure users would
// have lived with.
func TestStartLaunchesAConsoleProgramThroughExe(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.ToSlash(filepath.Join(dir, "launches.log"))

	e := listenerManifest(t, dir, freePort(t), logPath)

	msg, err := e.Start("starter")
	if err != nil {
		t.Fatalf("start failed: %v — the probe never saw the listener", err)
	}
	if !strings.Contains(msg, "started") {
		t.Fatalf("unexpected start report: %q", msg)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("nobody launched at all: %v\nstart said: %s", err, msg)
	}
	if n := strings.Count(string(raw), "launch"); n != 1 {
		t.Fatalf("%d launches; want exactly 1\nlog:\n%s", n, raw)
	}
}

// A port nothing is listening on right now, taken from the OS by binding :0.
// Every run gets its own: a fixture's listener lives a few seconds, and a fixed
// port would let one run's listener make the NEXT run's probe read green at
// t=0 — measured, under -count=3, where it produced "already running" with
// nobody launching. A test whose failure signature depends on how fast the
// previous run ended is not a test.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// The lock map is shared state created on demand: same id must give the SAME
// mutex (a fresh mutex per call would lock nobody), different ids must give
// different ones (starting Blender must not block starting ComfyUI), and the
// map must tolerate being filled from several goroutines at once.
func TestServiceLockIsPerService(t *testing.T) {
	e := writeManifest(t, t.TempDir(), `{
  "settings": {"web_port": 8787, "default_probe_timeout_s": 20, "poll_seconds": 5},
  "services": [{"id": "a", "label": "A", "start": {"exe": "x.exe"}, "stop": {"process": "x.exe"}, "probe": {"port": 1}}]
}`)

	if e.serviceLock("a") != e.serviceLock("a") {
		t.Fatal("serviceLock(\"a\") returned two different mutexes: the lock would lock nothing")
	}
	if e.serviceLock("a") == e.serviceLock("b") {
		t.Fatal("two services share one mutex: starting one would block starting the other")
	}

	// Fill the map from many goroutines at once. Under -race this also proves
	// startMu really guards it.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e.serviceLock(fmt.Sprintf("svc-%d", i%4))
		}(i)
	}
	wg.Wait()

	if e.serviceLock("svc-1") != e.serviceLock("svc-1") {
		t.Fatal("the map lost an entry under concurrent use")
	}
}

// The real thing, measured on the machine: two Start calls for one service,
// issued at the same instant. The launched program appends one line to a log
// file every time it runs, so the number of lines IS the number of launches.
//
// With the per-service lock: the first call launches, waits for its probe to
// turn green; the second call takes the lock after it, RE-PROBES inside the
// lock, sees the listener the first launch made, and answers "already running"
// without launching anything. One line in the log.
//
// Without the lock: both probe "off", both launch — two lines. Mutation-checked.
func TestStartLaunchesAServiceOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.ToSlash(filepath.Join(dir, "launches.log"))

	e := listenerManifest(t, dir, freePort(t), logPath)

	// Two callers, same instant: the tray button and the proxy request.
	var wg sync.WaitGroup
	results := make([]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg, err := e.Start("starter")
			results[i] = fmt.Sprintf("%s %v", msg, err)
		}(i)
	}
	wg.Wait()

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("nobody launched at all: %v\nrequests:\n%s", err, strings.Join(results, "\n"))
	}
	if n := strings.Count(string(raw), "launch"); n != 1 {
		t.Fatalf("%d launches of one service from two simultaneous requests; want exactly 1\nrequests:\n%s\nlog:\n%s",
			n, strings.Join(results, "\n"), raw)
	}

	// The second request must not fail: it must report the service as running,
	// because that is the truth by the time it gets its turn.
	joined := strings.Join(results, " | ")
	if !strings.Contains(joined, "already running") {
		t.Fatalf("neither caller saw the service as already running:\n%s", joined)
	}
}

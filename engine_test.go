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
	"time"
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

// M8: a cmd: start must return in the probe's time, not in the launcher's.
//
// hidden() used cmd.Output(), which blocks until EVERY handle to stdout
// closes. A launcher that boots a long-lived child keeps that handle open for
// the life of the child — Start hung with no timeout, the tray button frozen,
// the per-service lock held, every proxy request behind it frozen. This is the
// normal shape of a start script ("boot the dev server, return"), so the hang
// was not exotic.
//
// The fixture makes that exact shape: the cmd: entry logs its launch, binds a
// TCP port for the probe, spawns a child with -NoNewWindow (the child INHERITS
// the launcher's stdout), pauses two seconds for the handles to attach, and
// exits. The child then holds stdout open for 25 more seconds.
//
// With the detached launch: Start returns as soon as the probe sees the port —
// a few seconds. With cmd.Output(): Start cannot return before the child dies,
// ~25 seconds, by which time the listener is gone and the 20-second probe
// timeout has expired on top of it. The elapsed bound catches that;
// mutation-checked by putting hidden() back in launch.
//
// The child sleeps 25 seconds and exits by itself: nothing here needs a real
// service, and nothing survives the test beyond that.
func TestCmdStartReturnsWhileItsChildHoldsStdoutOpen(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.ToSlash(filepath.Join(dir, "launches.log"))
	port := freePort(t)

	script := fmt.Sprintf(
		"Add-Content -LiteralPath '%s' -Value 'launch'; "+
			"$l=[System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback,%d); "+
			"$l.Start(); "+
			"Start-Process -FilePath powershell.exe -ArgumentList '-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 25' -NoNewWindow; "+
			"Start-Sleep -Seconds 2; $l.Stop()",
		logPath, port)

	e := writeManifest(t, dir, fmt.Sprintf(`{
  "settings": {"web_port": 8787, "default_probe_timeout_s": 20, "poll_seconds": 5},
  "services": [{
    "id": "detached",
    "label": "Detached",
    "why": "test fixture: a cmd: launcher whose child inherits stdout",
    "start": {"cmd": "powershell.exe", "args": ["-NoProfile", "-NonInteractive", "-Command", "%s"]},
    "stop": {"process": "no-such-process-for-this-test.exe"},
    "probe": {"port": %d}
  }]
}`, script, port))

	start := time.Now()
	msg, err := e.Start("detached")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("start failed after %s: %v", elapsed, err)
	}
	if !strings.Contains(msg, "started") {
		t.Fatalf("unexpected start report: %q", msg)
	}
	// The probe answers in seconds. The child's stdout is open for ~25 s and
	// the old cmd.Output() waited for ALL of it (plus the 20 s probe timeout
	// after the listener closed): the bound sits well below that, above the
	// honest few seconds.
	if elapsed > 15*time.Second {
		t.Fatalf("Start took %s: it waited on the launcher's pipes, not on the probe\n%s", elapsed, msg)
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

// probeProcess reads row[1] — the pid column of tasklist's CSV — straight
// from the parsed row. A malformed row (a truncation, a locale surprise)
// panicked on the index and killed the whole tray. The formatting step
// must survive a short row: no panic, and "pid ?" says what is unknown.
func TestPresentMsgSurvivesShortTasklistRows(t *testing.T) {
	full := presentMsg("Docker Desktop", []string{"Docker Desktop.exe", "4242"})
	if !strings.Contains(full, "pid 4242") {
		t.Errorf("a complete row must report the pid, got: %s", full)
	}
	short := presentMsg("Docker Desktop", []string{"Docker Desktop.exe"})
	if !strings.Contains(short, "pid ?") {
		t.Errorf("a row without a pid column must say 'pid ?', got: %s", short)
	}
}

// The probe's error detail reaches EVERY surface — the Services cards, the
// CLI, the tray labels, the web API, the apply report, the services_list MCP
// tool — so a stopped service used to print the whole *url.Error wall
// ("dial tcp … connectex: No connection could be made …") in all of them.
// The detail must be the short, stable form the other probes use; the raw
// dial error is neither actionable nor worth re-printing every 5 s poll.
func TestProbeHTTPReportsShortNotVerbose(t *testing.T) {
	// A closed loopback port refuses immediately: fast, despite the 5 s
	// client timeout inside probeHTTP.
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", freePort(t))

	running, detail := probeHTTP(Probe{HTTP: url})
	if running {
		t.Fatalf("nothing listens on this port, yet probeHTTP says running (%q)", detail)
	}
	if want := "no response on " + url; detail != want {
		t.Errorf("detail = %q\nwant      = %q\n(the verbose dial error must not reach the UIs)", detail, want)
	}
	if strings.Contains(detail, "dial tcp") || strings.Contains(detail, "connectex") {
		t.Errorf("the connectex wall leaked into the probe detail: %q", detail)
	}
}

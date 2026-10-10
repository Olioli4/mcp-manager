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

// Start must report a launched program by what the probe sees, and that starts
// with the program ACTUALLY RUNNING. This is the regression test for the launch
// flags: under DETACHED_PROCESS, powershell.exe never runs at all — Start()
// returns no error, the tray says "started", and nothing is there. The engine
// asked for a program; the probe confirms it; the log proves it ran.
//
// Mutation-checked: with CreationFlags 0x00000008 (DETACHED_PROCESS) this test
// fails with "the probe never saw the listener" — the exact silent failure users
// would have lived with. Measured three runs each: DETACHED_PROCESS = never
// runs, CREATE_NO_WINDOW = runs, no flags = runs.
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

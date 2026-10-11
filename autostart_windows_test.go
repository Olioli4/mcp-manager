package main

// Autostart tests. The real HKCU Run key is NEVER touched: every registry
// test runs against a throwaway key created and deleted here.

import (
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// The Run key passes its value data verbatim to CreateProcess. An unquoted
// path breaks autostart for every install under "C:\Program Files\…" —
// Windows would split the value at the first space and look for an
// executable that does not exist. The quotes are the whole feature.
func TestRunValueQuotesTheExePath(t *testing.T) {
	exe := `C:\Program Files\MCP Manager\mcp-manager-tray.exe`
	want := `"C:\Program Files\MCP Manager\mcp-manager-tray.exe"`
	if got := runValue(exe); got != want {
		t.Fatalf("runValue(%q) = %q; want %q (an unquoted path with spaces never autostarts)", exe, got, want)
	}
	// A path without spaces must be quoted too: consistency means the value
	// always parses as exactly one argument.
	if got := runValue(`C:\mcp\mcp-manager-tray.exe`); got != `"C:\mcp\mcp-manager-tray.exe"` {
		t.Errorf("runValue of a spaceless path = %q; want it quoted as well", got)
	}
}

// The round trip the tray toggle performs: write → read back true →
// remove → read back false. Against a throwaway key — a test that poked
// the real Run key would autostart the TEST BINARY on the next logon.
func TestAutostartRoundTripAgainstATempKey(t *testing.T) {
	path := `Software\mcp-manager-test-` + time.Now().Format("150405.000000000")
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS)
	if err != nil {
		t.Fatalf("CreateKey(%s): %v", path, err)
	}
	k.Close()
	defer registry.DeleteKey(registry.CURRENT_USER, path)

	if on, err := autostartIn(path); err != nil || on {
		t.Fatalf("a fresh key must read as off, got on=%v err=%v", on, err)
	}

	value := runValue(`C:\Program Files\MCP Manager\mcp-manager-tray.exe`)
	if err := setAutostartIn(path, true, value); err != nil {
		t.Fatalf("setAutostartIn(on): %v", err)
	}
	if on, err := autostartIn(path); err != nil || !on {
		t.Fatalf("after setAutostartIn(on): on=%v err=%v; want true, nil", on, err)
	}

	if err := setAutostartIn(path, false, ""); err != nil {
		t.Fatalf("setAutostartIn(off): %v", err)
	}
	if on, err := autostartIn(path); err != nil || on {
		t.Fatalf("after setAutostartIn(off): on=%v err=%v; want false, nil", on, err)
	}

	// Toggling off twice must not error: clicking an already-off checkbox
	// is a no-op, not a failure to report to the log.
	if err := setAutostartIn(path, false, ""); err != nil {
		t.Fatalf("setAutostartIn(off) on a missing value = %v; want nil", err)
	}
}

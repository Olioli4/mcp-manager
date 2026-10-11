package main

// Start with Windows, per-user, no admin: a value in the HKCU `Run` key.
//
// Why the Run key and not a scheduled task (the D8 sketch): the Run key is
// per-user, needs no elevation and no service session — and a service runs
// in session 0 with no desktop, so it could never open the programs we
// drive here. The tray runs in YOUR session, exactly like a manual start.
//
// The invariant that must survive this feature: autostart boots RESIDENT
// mode, which warms the proxy with mayStart=false. The Run entry carries no
// arguments (main.go treats "no args" as resident mode), and nothing here
// starts a service — the boot log of an autostarted manager must contain
// zero start lines, same as any other boot.

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

const (
	runKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "MCP Manager"
)

// runValue quotes the exe path for the Run key. Quoting is mandatory: a
// path with spaces ("C:\Program Files\…") must arrive at CreateProcess as
// ONE argument — the Run key passes the value data verbatim, so without the
// quotes Windows would split it and find no such file.
func runValue(exe string) string {
	return `"` + exe + `"`
}

// autostartEnabled reports whether the Run value is present. Any error
// reads as "off": a menu checkbox must not fail to render because the
// registry was momentarily unavailable.
func autostartEnabled() bool {
	on, err := autostartIn(runKeyPath)
	return err == nil && on
}

// setAutostart adds or removes the Run value, pointing at our own exe.
func setAutostart(on bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return setAutostartIn(runKeyPath, on, runValue(exe))
}

// autostartIn is the testable core of autostartEnabled: same logic, but on
// a caller-chosen key path — so the test exercises a throwaway key instead
// of the real Run key.
func autostartIn(path string) (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	if _, _, err = k.GetStringValue(runValueName); err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// setAutostartIn is the testable core of setAutostart: same logic on a
// caller-chosen key path. The Run key always exists on a real Windows
// profile, so OpenKey (never CreateKey) is correct here — and the test
// creates its own throwaway key beforehand.
func setAutostartIn(path string, on bool, value string) error {
	// SET_VALUE covers both writes and the delete below.
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if on {
		return k.SetStringValue(runValueName, value)
	}
	// Toggling off when already off is not an error: the click is a no-op.
	if err := k.DeleteValue(runValueName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

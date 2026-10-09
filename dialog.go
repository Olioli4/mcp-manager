package main

// A startup failure has to be SEEN.
//
// The resident binary is built with -H=windowsgui, so it has no console: an
// error printed to stderr goes nowhere at all, and someone double-clicking the
// icon watches nothing happen. A typo in services.json is then indistinguishable
// from a broken program. This was a real finding in AUDIT.md (LOW-3) — the app
// exited with code 1 and said nothing anywhere the user could look.
//
// The CLI path still writes to stderr, because a terminal is exactly where a
// command-line user is looking.

import (
	"syscall"
	"unsafe"
)

var (
	user32      = syscall.NewLazyDLL("user32.dll")
	procMsgBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbOK            = 0x00000000
	mbIconError     = 0x00000010
	mbSetForeground = 0x00010000
)

// fatalDialog shows a modal error dialog. It is called only when the process is
// about to exit, so blocking until the user dismisses it is correct behaviour.
func fatalDialog(title, msg string) {
	t, err1 := syscall.UTF16PtrFromString(title)
	m, err2 := syscall.UTF16PtrFromString(msg)
	if err1 != nil || err2 != nil {
		return
	}
	procMsgBoxW.Call(
		0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		mbOK|mbIconError|mbSetForeground,
	)
}

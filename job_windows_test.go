package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// M9: the manager's stdio children must die with it.
//
// Windows kills nothing when a parent exits, so the promise "quit the tray, no
// orphan node is left serving your API keys" needs the kernel's Job Object:
// KILL_ON_JOB_CLOSE kills every member the instant the LAST handle to the job
// closes. The manager never closes its handle — the process exit itself does,
// crash included — so in production the trigger is invisible. This test pulls
// the same trigger by hand: it is the only way to watch the kernel do its job
// without quitting the tray from inside a test binary.
//
// The fixture is a PowerShell that writes a "born" marker, sleeps 60 seconds,
// and would write a "died" marker if it were allowed to finish. Born + no died
// + exit within seconds of closing the handle = killed by the job, which is
// exactly what a clean manager quit must do to an npx that ignores stdin EOF.
//
// Mutation-checked twice: dropping the KILL_ON_JOB_CLOSE flag, and dropping
// the assignToJob call (the child is then outside the job) — in both cases the
// child survives and the test fails.
//
// Closing the process-lifetime job handle here is safe for the rest of the
// suite: no other test connects a stdio server, and jobbedTransport degrades
// to a logged warning on a closed handle rather than breaking a session.
func TestJobObjectKillsItsChildWhenTheHandleCloses(t *testing.T) {
	dir := t.TempDir()
	born := filepath.Join(dir, "born")
	died := filepath.Join(dir, "died")

	script := "Set-Content -LiteralPath '" + born + "' -Value 'born'; " +
		"Start-Sleep -Seconds 60; " +
		"Set-Content -LiteralPath '" + died + "' -Value 'died'"

	h, err := stdioJob()
	if err != nil {
		t.Fatalf("no job object to test: %v", err)
	}

	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := assignToJob(cmd.Process.Pid); err != nil {
		t.Fatalf("assignToJob: %v", err)
	}

	// The child is inside the job only if it ran at all: wait for the marker.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(born); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never wrote its born marker: the launch did not work")
		}
		time.Sleep(250 * time.Millisecond)
	}

	// The trigger: close the last handle to the job. In production this is the
	// manager's exit; here it is this line.
	if err := windows.CloseHandle(h); err != nil {
		t.Fatalf("CloseHandle: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	select {
	case err := <-waited:
		_ = err // any exit is fine — the point is that it exits NOW
	case <-time.After(15 * time.Second):
		t.Fatal("the child survived the job's last handle closing: KILL_ON_JOB_CLOSE is not killing it")
	}

	if _, err := os.Stat(died); err == nil {
		t.Fatal("the child finished its work after the job closed: it was killed by nobody, it left by itself")
	}
}

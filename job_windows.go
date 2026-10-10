package main

// The stdio children must not outlive the manager (M9).
//
// Windows does not kill a process's children when it exits. So every npx/node
// the proxy spawns for a stdio server keeps serving on its pipes long after
// the tray is gone — invisible, unkillable from the UI, holding the ports and
// the API keys of a program the user believes is closed. A Job Object with
// KILL_ON_JOB_CLOSE is the reliable fix: the kernel kills every process in the
// job the instant the last handle to the job closes, and the OS closes the
// manager's handle when the manager exits — including a crash.
//
// Scope: the proxy's stdio connect path only. The services the user starts
// from the tray are meant to outlive the manager — the Quit item says "does
// not touch the services" and that promise is kept.

import (
	"context"
	"fmt"
	"sync"
	"unsafe"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/windows"
)

var (
	jobOnce   sync.Once
	jobHandle windows.Handle
	jobErr    error
)

// stdioJob creates the one job object the manager uses, exactly once.
// KILL_ON_JOB_CLOSE only fires when the LAST handle to the job closes: we keep
// this handle open for the whole life of the process and never close it
// ourselves — the exit itself is what closes it.
func stdioJob() (windows.Handle, error) {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			jobErr = fmt.Errorf("CreateJobObject: %w", err)
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(
			h,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		); err != nil {
			windows.CloseHandle(h)
			jobErr = fmt.Errorf("SetInformationJobObject: %w", err)
			return
		}
		jobHandle = h
	})
	return jobHandle, jobErr
}

// assignToJob puts a started process inside the kill-on-exit job. From that
// moment every process IT spawns joins the job too (job membership is
// inherited since Windows 8), so one assign covers node and all of its
// children. A child born in the millisecond window between Start and this
// call escapes — for a server process that window is before it spawns
// anything, and CloseAll on a clean quit is the polite path anyway.
func assignToJob(pid int) error {
	h, err := stdioJob()
	if err != nil {
		return err
	}
	// PROCESS_SET_QUOTA | PROCESS_TERMINATE are the access rights the job
	// assignment requires on the process handle.
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(p)
	if err := windows.AssignProcessToJobObject(h, p); err != nil {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", pid, err)
	}
	return nil
}

// jobbedTransport wraps the SDK's CommandTransport to get the one hook the
// SDK does not offer: the moment right after the command is started.
// CommandTransport.Connect calls cmd.Start() itself and keeps the *exec.Cmd
// private; wrapping its exported Transport interface lets us assign the
// child to the job as soon as its PID exists, before any request flows.
type jobbedTransport struct {
	*mcp.CommandTransport
}

func (t jobbedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.CommandTransport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if proc := t.Command.Process; proc != nil {
		if err := assignToJob(proc.Pid); err != nil {
			// Degraded guarantee, not a reason to drop a healthy session:
			// log it and keep the connection.
			logf("proxy: stdio child %d NOT in the kill-on-exit job: %v", proc.Pid, err)
		}
	}
	return conn, nil
}

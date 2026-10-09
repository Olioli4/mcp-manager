package main

// Resident mode: an icon in the notification area.
//
// Why a tray and not a Windows service: a service runs in session 0, isolated
// from the desktop — it could not open any of the graphical programs we drive
// here. The tray runs in YOUR session, so it can.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/getlantern/systray"
)

func runResident(e *Engine) {
	// systray.Run blocks and must hold the main thread: it is the one that owns
	// the Windows message loop.
	systray.Run(func() { trayReady(e) }, func() {})
}

// svcMenu groups a service's menu entries.
type svcMenu struct {
	parent      *systray.MenuItem
	state       *systray.MenuItem
	start, stop *systray.MenuItem
}

func trayReady(e *Engine) {
	systray.SetIcon(makeIcon())
	systray.SetTitle("")
	systray.SetTooltip("MCP Manager — starting…")

	// The configuration lives in a local web page: a clients x servers matrix and
	// an editable inventory do not fit in a menu.
	url, err := startWeb(e)
	if err != nil {
		logf("web interface NOT started: %v", err)
	}
	if url != "" {
		logf("web interface: %s", url)
	}
	open := systray.AddMenuItem("Open the configuration…", url)
	systray.AddSeparator()

	// One submenu per service: State / Start / Stop.
	menus := map[string]*svcMenu{}
	man := e.Manifest()
	for i := range man.Services {
		s := man.Services[i]
		parent := systray.AddMenuItem(s.Label, s.Why)
		state := parent.AddSubMenuItem("state: ?", "")
		state.Disable()
		start := parent.AddSubMenuItem("Start", "")
		stop := parent.AddSubMenuItem("Stop", "")
		menus[s.ID] = &svcMenu{parent: parent, state: state, start: start, stop: stop}
	}

	systray.AddSeparator()
	refresh := systray.AddMenuItem("Refresh", "re-read the state of the services")
	trayLog := systray.AddMenuItem("Open the log", "mcp-manager.log next to the executable")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "stop the manager (does not touch the services)")

	if url != "" {
		go func() {
			for range open.ClickedCh {
				_ = exec_detached("cmd", "/c", "start", "", url)
			}
		}()
	} else {
		open.Disable()
	}

	// Actions: each call to the engine goes in its own goroutine, otherwise we
	// would freeze the message loop while waiting for a probe.
	for id, m := range menus {
		id, m := id, m
		go func() {
			for range m.start.ClickedCh {
				go doAction(e, id, "start", m)
			}
		}()
		go func() {
			for range m.stop.ClickedCh {
				go doAction(e, id, "stop", m)
			}
		}()
	}
	go func() {
		for range refresh.ClickedCh {
			refreshAll(e, menus)
		}
	}()
	go func() {
		for range trayLog.ClickedCh {
			openLog()
		}
	}()
	go func() {
		<-quit.ClickedCh
		systray.Quit()
	}()

	refreshAll(e, menus)

	// Periodic refresh: the state can change without us (the user closes a
	// window, a program crashes).
	go func() {
		for range time.Tick(time.Duration(e.Manifest().Settings.PollSeconds) * time.Second) {
			refreshAll(e, menus)
		}
	}()
}

// doAction runs start/stop and writes the result to the log — a tray has no
// console, so the file is the only place to see what happened.
func doAction(e *Engine, id, verb string, m *svcMenu) {
	var msg string
	var err error
	if verb == "start" {
		msg, err = e.Start(id)
	} else {
		msg, err = e.Stop(id)
	}
	if err != nil {
		logf("%s %s: FAILURE — %v", verb, id, err)
		m.state.SetTitle("failure — see the log")
		return
	}
	logf("%s: %s", verb, msg)
}

// refreshAll re-reads the state of each service and updates the labels.
func refreshAll(e *Engine, menus map[string]*svcMenu) {
	up := 0
	man := e.Manifest()
	for i := range man.Services {
		s := man.Services[i]
		m := menus[s.ID]
		if m == nil {
			continue
		}
		running, detail := e.Probe(&s)
		if running {
			up++
			m.state.SetTitle("● running — " + truncate(detail, 60))
			m.start.Disable()
			m.stop.Enable()
		} else {
			m.state.SetTitle("○ stopped — " + truncate(detail, 60))
			m.start.Enable()
			m.stop.Disable()
		}
	}
	systray.SetTooltip(fmt.Sprintf("MCP Manager — %d/%d services running", up, len(man.Services)))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Log — the tray has no console
// ---------------------------------------------------------------------------

func logPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "mcp-manager.log")
	}
	return "mcp-manager.log"
}

func logf(format string, args ...any) {
	f, err := os.OpenFile(logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

func openLog() {
	p := logPath()
	// We do not want to depend on an external program: we try Notepad, which is
	// present everywhere on Windows.
	_ = exec_detached("notepad.exe", p)
}

// ---------------------------------------------------------------------------
// Icon — built in code, so as to have no binary file to ship
// ---------------------------------------------------------------------------

// makeIcon returns a 16x16 32-bit ICO: a solid disk with a light dot in the
// center. Readable on a light as well as a dark background, and it avoids
// embedding an .ico file in the repository.
func makeIcon() []byte {
	const size = 16
	const xorBytes = size * size * 4
	const andRowBytes = 4 // ((16 + 31) / 32) * 4
	const andBytes = andRowBytes * size
	const imgBytes = 40 + xorBytes + andBytes

	out := make([]byte, 0, 22+imgBytes)

	// ICONDIR
	out = append(out, 0, 0, 1, 0, 1, 0)
	// ICONDIRENTRY
	out = append(out, size, size, 0, 0, 1, 0, 32, 0)
	out = appendLE32(out, imgBytes)
	out = appendLE32(out, 22)

	// BITMAPINFOHEADER
	out = appendLE32(out, 40)
	out = appendLE32(out, size)
	out = appendLE32(out, size*2) // doubled height: XOR + AND mask
	out = appendLE16(out, 1)
	out = appendLE16(out, 32)
	out = appendLE32(out, 0) // BI_RGB
	out = appendLE32(out, 0)
	out = appendLE32(out, 0)
	out = appendLE32(out, 0)
	out = appendLE32(out, 0)
	out = appendLE32(out, 0)

	// XOR pixels, rows from BOTTOM to TOP (BMP convention).
	for row := 0; row < size; row++ {
		y := size - 1 - row
		for x := 0; x < size; x++ {
			r, g, b, a := iconPixel(x, y)
			out = append(out, b, g, r, a)
		}
	}
	// AND mask: all zero (the alpha does the work in 32 bits).
	for i := 0; i < andBytes; i++ {
		out = append(out, 0)
	}
	return out
}

// iconPixel draws a ring: colored border, light core, transparent outside.
func iconPixel(x, y int) (r, g, b, a uint8) {
	dx := float64(x) - 7.5
	dy := float64(y) - 7.5
	d2 := dx*dx + dy*dy
	switch {
	case d2 <= 12.25: // radius 3.5 — core
		return 0xF2, 0xF7, 0xF7, 0xFF
	case d2 <= 49: // radius 7 — ring
		return 0x1E, 0x88, 0xE5, 0xFF // blue
	default:
		return 0, 0, 0, 0 // transparent
	}
}

func appendLE32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func appendLE16(b []byte, v uint16) []byte {
	return append(b, byte(v), byte(v>>8))
}

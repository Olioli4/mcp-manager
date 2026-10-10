package main

// The engine: read the manifest, probe, start, stop.
//
// It knows NO service in particular. Everything it knows comes from
// services.json. That is the boundary that makes the tool extensible without
// recompiling it: adding a program is adding an entry to the file.

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// The manifest
// ---------------------------------------------------------------------------

type Probe struct {
	Process string `json:"process"`
	HTTP    string `json:"http"`
	Expect  []int  `json:"expect"`
	Port    int    `json:"port"`
}

type Action struct {
	Exe  string   `json:"exe"`
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
	// Process: stop by process name (start ignores it, stop uses it).
	Process string `json:"process"`
	// Stop only: kill only the processes whose command line
	// contains this string. Essential for Chrome — killing every
	// chrome.exe would close the user's personal browser.
	OnlyIfCmdlineMatches string `json:"only_if_cmdline_matches"`
}

type Service struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Why      string   `json:"why"`
	Notes    []string `json:"notes"`
	Requires []string `json:"requires"`
	Start    Action   `json:"start"`
	Stop     Action   `json:"stop"`
	Probe    Probe    `json:"probe"`
}

type Settings struct {
	WebHost              string `json:"web_host"`
	WebPort              int    `json:"web_port"`
	DefaultProbeTimeoutS int    `json:"default_probe_timeout_s"`
	PollSeconds          int    `json:"poll_seconds"`
}

type Manifest struct {
	Settings Settings  `json:"settings"`
	Services []Service `json:"services"`
}

// Engine holds the manifest behind an ATOMIC pointer, not in a bare field.
// Why: /api/reload replaces it while probes are reading it (the tray
// probes in a loop, the web page probes on every load). A bare field would be
// a race — writing it while another goroutine reads it is undefined behaviour,
// and that kind of bug only shows up one time in a thousand, in production.
type Engine struct {
	man  atomic.Pointer[Manifest]
	Path string
}

// Manifest returns the current manifest.
func (e *Engine) Manifest() *Manifest { return e.man.Load() }

// SetManifest replaces the manifest in one block. Readers already at work
// keep the old pointer, which stays allocated: they finish their
// probe on the state they had started reading, which is exactly what
// we want.
func (e *Engine) SetManifest(m *Manifest) { e.man.Store(m) }

func LoadEngine(path string) (*Engine, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest unreadable (%s): %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("invalid manifest (%s): %w", path, err)
	}
	if len(m.Services) == 0 {
		return nil, fmt.Errorf("empty manifest: no service in %s", path)
	}
	seen := map[string]bool{}
	for _, s := range m.Services {
		if s.ID == "" {
			return nil, fmt.Errorf("a service has no id in %s", path)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("duplicate id: %q", s.ID)
		}
		seen[s.ID] = true
	}
	if m.Settings.DefaultProbeTimeoutS <= 0 {
		m.Settings.DefaultProbeTimeoutS = 90
	}
	if m.Settings.PollSeconds <= 0 {
		m.Settings.PollSeconds = 5
	}
	if m.Settings.WebHost == "" {
		m.Settings.WebHost = "127.0.0.1"
	}
	if m.Settings.WebPort == 0 {
		m.Settings.WebPort = 8787
	}
	e := &Engine{Path: path}
	e.man.Store(&m)
	return e, nil
}

// ReloadManifest re-reads services.json and replaces the manifest IN PLACE.
//
// This is what makes the file editable by hand without restarting: you write the
// JSON, you reload, the probes and the colours follow. Without that, the only way
// to add a service would be to relaunch the application — and a tray has
// nobody to do it in its place.
func ReloadManifest(e *Engine) error {
	fresh, err := LoadEngine(e.Path)
	if err != nil {
		return err
	}
	e.man.Store(fresh.man.Load())
	return nil
}

func (e *Engine) Find(id string) (*Service, error) {
	m := e.Manifest()
	for i := range m.Services {
		if m.Services[i].ID == id {
			return &m.Services[i], nil
		}
	}
	var ids []string
	for _, s := range m.Services {
		ids = append(ids, s.ID)
	}
	return nil, fmt.Errorf("unknown service %q — known: %s", id, strings.Join(ids, ", "))
}

// ---------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------

// Probe says whether the service is running, and how we know.
//
// The detail ("http 200 on ...", "process chrome.exe present") is returned to the
// client: an agent that sees "stopped" must also see WHY we believe it.
func (e *Engine) Probe(s *Service) (bool, string) {
	switch {
	case s.Probe.HTTP != "":
		return probeHTTP(s.Probe)
	case s.Probe.Process != "":
		return probeProcess(s.Probe.Process)
	case s.Probe.Port != 0:
		return probePort(s.Probe.Port)
	default:
		return false, "no probe declared in the manifest"
	}
}

func probeHTTP(p Probe) (bool, string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(p.HTTP)
	if err != nil {
		return false, fmt.Sprintf("no response on %s (%v)", p.HTTP, firstLine(err.Error()))
	}
	defer resp.Body.Close()
	expect := p.Expect
	if len(expect) == 0 {
		return resp.StatusCode >= 200 && resp.StatusCode < 300,
			fmt.Sprintf("HTTP %d on %s", resp.StatusCode, p.HTTP)
	}
	for _, want := range expect {
		if resp.StatusCode == want {
			return true, fmt.Sprintf("HTTP %d on %s", resp.StatusCode, p.HTTP)
		}
	}
	return false, fmt.Sprintf("HTTP %d on %s (expected %v)", resp.StatusCode, p.HTTP, expect)
}

func probePort(port int) (bool, string) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		return false, fmt.Sprintf("nothing is listening on 127.0.0.1:%d", port)
	}
	c.Close()
	return true, fmt.Sprintf("127.0.0.1:%d accepts connections", port)
}

// probeProcess goes through tasklist as CSV: tasklist's headers are
// localised (your machine is in German), but /NH /FO CSV produces only
// data, so the parsing does not depend on the language.
//
// Normalisation: tasklist returns the image name WITH ".exe", but the manifest
// may declare "Docker Desktop" without an extension. So we compare on the
// normalised form (see procName), so that the manifest stays readable.
func probeProcess(name string) (bool, string) {
	out, err := hidden("tasklist", "/FI", "IMAGENAME eq "+procName(name), "/NH", "/FO", "CSV")
	if err != nil {
		return false, fmt.Sprintf("tasklist failed (%v)", firstLine(err.Error()))
	}
	for _, row := range csvRows(out) {
		if len(row) > 0 && strings.EqualFold(strings.TrimSpace(row[0]), procName(name)) {
			return true, fmt.Sprintf("process %s present (pid %s)", name, row[1])
		}
	}
	return false, fmt.Sprintf("no process %s", name)
}

// ---------------------------------------------------------------------------
// Start / stop
// ---------------------------------------------------------------------------

// Start launches the service and waits for its probe to turn green.
func (e *Engine) Start(id string) (string, error) {
	s, err := e.Find(id)
	if err != nil {
		return "", err
	}

	if running, detail := e.Probe(s); running {
		return fmt.Sprintf("%s is already running — %s", s.Label, detail), nil
	}

	// Prerequisites are checked, never started in cascade: launching Docker
	// then the container then three applications because someone asked for "blender"
	// would be an unpleasant surprise. We say what to do, we do not do it.
	var missing []string
	for _, req := range s.Requires {
		r, err := e.Find(req)
		if err != nil {
			missing = append(missing, req+" (unknown to the manifest)")
			continue
		}
		if running, _ := e.Probe(r); !running {
			missing = append(missing, fmt.Sprintf("%s [%s]", r.Label, r.ID))
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("%s requires first: %s — start them, then try again",
			s.Label, strings.Join(missing, ", "))
	}

	if err := e.launch(s.Start); err != nil {
		return "", fmt.Errorf("launching %s: %w", s.Label, err)
	}
	return e.waitFor(s, true)
}

// Stop stops the service and waits for its probe to turn red.
//
// It attempts the stop EVEN IF the probe already says "stopped", and that is
// essential: a half-started program has processes not listening yet. The HTTP
// probe does not see them, so the old version answered "already stopped" while
// leaving 9 ComfyUI alive — unstoppable from the application.
func (e *Engine) Stop(id string) (string, error) {
	s, err := e.Find(id)
	if err != nil {
		return "", err
	}

	running, detail := e.Probe(s)

	// No stop method declared: the probe is authoritative.
	if !running && s.Stop.Process == "" && s.Stop.Cmd == "" {
		return fmt.Sprintf("%s is already stopped — %s", s.Label, detail), nil
	}

	before := 0
	if s.Stop.Process != "" {
		before = countProcesses(s.Stop.Process, s.Stop.OnlyIfCmdlineMatches)
	}

	if err := e.terminate(s); err != nil {
		return "", fmt.Errorf("stopping %s: %w", s.Label, err)
	}

	if s.Stop.Process != "" {
		if !waitProcessesGone(s.Stop.Process, s.Stop.OnlyIfCmdlineMatches, 15*time.Second) {
			return "", fmt.Errorf("%s: some processes are still resisting (see the log)", s.Label)
		}
		// Case of the half-started program: the probe said "stopped" while
		// processes were alive. We SAY it, because it is the opposite of what
		// the probe was reporting.
		if !running && before > 0 {
			return fmt.Sprintf("%s stopped — %d remaining processes eliminated (the probe already said it was \"stopped\": %s)",
				s.Label, before, detail), nil
		}
	}

	if !running {
		return fmt.Sprintf("%s is already stopped — %s", s.Label, detail), nil
	}
	return e.waitFor(s, false)
}

// countProcesses counts the processes targeted by a stop, with the same filter
// as killProcess (therefore the same guard on the command line).
func countProcesses(name, match string) int {
	var script string
	if match == "" {
		script = fmt.Sprintf("@(Get-CimInstance Win32_Process -Filter \"Name='%s'\").Count", procName(name))
	} else {
		script = fmt.Sprintf("@(Get-CimInstance Win32_Process -Filter \"Name='%s'\" | Where-Object { $_.CommandLine -like '*%s*' }).Count",
			procName(name), match)
	}
	out, err := hidden("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// waitProcessesGone waits for the processes to have really disappeared. We do
// not assume: we observe.
func waitProcessesGone(name, match string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if countProcesses(name, match) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (e *Engine) launch(a Action) error {
	switch {
	case a.Exe != "":
		// Detached: the program must survive our own exit — but NOT via
		// DETACHED_PROCESS (0x8). Measured on this machine, three runs each:
		// powershell.exe launched with DETACHED_PROCESS does not run at all, and
		// fails SILENTLY — Start returns no error, the program is simply never
		// there. A console program given no console dies during its own startup,
		// so the tray reported "started" for a program that never existed.
		//
		// CREATE_NO_WINDOW is what we actually wanted in the first place: the
		// console exists (console programs accept it, measured ran=true) but is
		// never shown, and HideWindow covers the window cmd.exe-style launchers
		// create before their flag applies. The program still outlives us.
		cmd := exec.Command(a.Exe, a.Args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags: 0x08000000, // CREATE_NO_WINDOW
			HideWindow:    true,
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	case a.Cmd != "":
		_, err := hidden(a.Cmd, a.Args...)
		return err
	default:
		return fmt.Errorf("no launch method declared (neither exe, nor cmd)")
	}
}

func (e *Engine) terminate(s *Service) error {
	switch {
	case s.Stop.Cmd != "":
		_, err := hidden(s.Stop.Cmd, s.Stop.Args...)
		return err
	case s.Stop.Process != "":
		return killProcess(s.Stop.Process, s.Stop.OnlyIfCmdlineMatches)
	default:
		return fmt.Errorf("no stop method declared (neither process, nor cmd)")
	}
}

// killProcess kills by name. If match is non-empty, only the processes whose
// command line contains this string are killed — that is what protects the
// personal browser when we want to kill only the debug Chrome.
func killProcess(name, match string) error {
	var script string
	if match == "" {
		script = fmt.Sprintf(
			"Get-CimInstance Win32_Process -Filter \"Name='%s'\" | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }",
			procName(name))
	} else {
		script = fmt.Sprintf(
			"Get-CimInstance Win32_Process -Filter \"Name='%s'\" | Where-Object { $_.CommandLine -like '*%s*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }",
			procName(name), match)
	}
	_, err := hidden("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	return err
}

// waitFor waits for the probe to reach the wanted state, or hands back control
// once the timeout has elapsed.
func (e *Engine) waitFor(s *Service, want bool) (string, error) {
	deadline := time.Now().Add(time.Duration(e.Manifest().Settings.DefaultProbeTimeoutS) * time.Second)
	tick := 2 * time.Second
	var last string
	for {
		running, detail := e.Probe(s)
		last = detail
		if running == want {
			if want {
				return fmt.Sprintf("%s started — %s", s.Label, detail), nil
			}
			return fmt.Sprintf("%s stopped", s.Label), nil
		}
		if time.Now().After(deadline) {
			verb := "start"
			if !want {
				verb = "stop"
			}
			return "", fmt.Errorf("unable to %s %s within the timeout — last state: %s",
				verb, s.Label, last)
		}
		time.Sleep(tick)
	}
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

// hidden runs a command without flashing a console window.
// Without that, every probe would make a black rectangle appear on screen.
func hidden(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.Output()
	return string(out), err
}

// exec_detached launches a program without waiting for it or associating it with
// us. Used to open the log in a window: the manager must neither die with it,
// nor wait for it.
func exec_detached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}

func csvRows(s string) [][]string {
	r := csv.NewReader(strings.NewReader(s))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil
	}
	return rows
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// procName normalises a process name for tasklist / Get-CimInstance, which
// return the image name WITH ".exe" (e.g. "Docker Desktop.exe"). The manifest
// may declare "Docker Desktop" without an extension: we then add it. A name that
// already contains a dot (e.g. "chrome.exe") is left as is.
func procName(name string) string {
	if strings.Contains(name, ".") {
		return name
	}
	return name + ".exe"
}

// assetPath resolves a file shipped next to the executable.
//
// Essential: the working directory of a process launched by the
// Task Scheduler, by WMI or by a shortcut is NOT the application's
// folder. A simple relative path therefore fails depending on how we
// started — and the bug appears only on a "real" launch, never in a test.
// We fall back on the relative path only in development.
func assetPath(name string) string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

// ManifestPath: services.json (see assetPath).
func ManifestPath() string { return assetPath("services.json") }

// InventoryPath: inventory.json (see assetPath).
func InventoryPath() string { return assetPath("inventory.json") }

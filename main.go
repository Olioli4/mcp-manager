package main

// Service manager — the machine's concierge.
//
// Two ways to use it, a single engine:
//
//	without argument  the resident application: icon in the notification area
//	                  + MCP server, so that the hub and the agents can drive it
//	with an argument  the same thing on the command line, to test and debug
//
//	Everything it knows comes from services.json. No service is hard-coded.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const usage = `mcp-manager — the machine's concierge

  mcp-manager                 starts the resident application (tray icon + web UI + MCP server)
  mcp-manager list            the host programs, with their live state and their reason for being
  mcp-manager status <id>     one program in detail, with its notes
  mcp-manager start <id>      starts it and waits for its probe to answer
  mcp-manager stop <id>       stops it and waits for it to be gone
  mcp-manager apply           shows what would be written for each client (touches nothing)
  mcp-manager apply --write   writes it, after a timestamped backup
  mcp-manager reload          re-reads services.json live (resident mode)

  Extension happens in services.json, inventory.json and plugins/ — never here.
`

func main() {
	args := os.Args[1:]
	resident := len(args) == 0

	engine, err := loadEngine()
	if err != nil {
		// With no arguments this was launched from a shortcut or the Start menu,
		// and the binary is built with -H=windowsgui: no console, so stderr goes
		// nowhere. Without this dialog the app simply fails to appear.
		if resident {
			fatalDialog("MCP Manager cannot start", err.Error())
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if resident {
		runResident(engine)
		return
	}

	switch args[0] {
	case "list", "ls":
		cmdList(engine)
	case "status":
		if len(args) < 2 {
			fail("status: a service id is required")
		}
		cmdStatus(engine, args[1])
	case "start":
		if len(args) < 2 {
			fail("start: a service id is required")
		}
		cmdAction(engine, "start", args[1])
	case "stop":
		if len(args) < 2 {
			fail("stop: a service id is required")
		}
		cmdAction(engine, "stop", args[1])
	case "apply":
		inv, err := LoadInventory(InventoryPath())
		if err != nil {
			fail(err.Error())
		}
		write := false
		for _, a := range args[1:] {
			if a == "--write" || a == "-w" {
				write = true
			}
		}
		if err := cmdApply(inv, write); err != nil {
			fail(err.Error())
		}
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n%s", args[0], usage)
		os.Exit(1)
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}

// loadEngine seeds a missing configuration from the shipped example, then loads
// it.
//
// Why seed rather than refuse: a freshly cloned repository has no services.json
// — it is machine-specific and deliberately not committed. Refusing to start
// would mean the first experience of this tool is an error message about a file
// the user has never heard of. Copying the example gives them something that
// starts, shows the shape, and is theirs to edit.
func loadEngine() (*Engine, error) {
	for _, name := range []string{"services.json", "inventory.json"} {
		if _, err := os.Stat(assetPath(name)); os.IsNotExist(err) {
			if ex := seedFromExample(name); ex != "" {
				logf("no %s — created one from %s; edit it for this machine", name, ex)
			}
		}
	}
	return LoadEngine(ManifestPath())
}

// seedFromExample copies <name>.example.json to <name>. It returns the example's
// filename, or "" when there was nothing to copy — in which case the caller
// carries on and reports the real error.
//
// The copy lands NEXT TO THE EXAMPLE WE ACTUALLY READ, never in the working
// directory (M7): assetPath resolves the exe's own folder first, so a shortcut
// or Task-Scheduler launch (CWD = System32) seeds services.json/inventory.json
// beside the executable, where the README says they go and where cache/ and the
// log already live. Writing into the CWD seeded configs into whatever folder the
// launcher happened to set — and the next launch, with a different CWD, found
// neither and seeded again. In development (go run), where assetPath falls back
// to the relative path, the example is read from and written to the repository
// folder: seeding stays where the developer expects it.
func seedFromExample(name string) string {
	ex := strings.TrimSuffix(name, ".json") + ".example.json"
	srcPath := assetPath(ex)
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return ""
	}
	target := filepath.Join(filepath.Dir(srcPath), name)
	if err := os.WriteFile(target, src, 0o644); err != nil {
		return ""
	}
	return ex
}

func cmdList(e *Engine) {
	fmt.Printf("manifest: %s\n\n", e.Path)
	for _, s := range e.Manifest().Services {
		running, detail := e.Probe(&s)
		mark := "○"
		state := "stopped"
		if running {
			mark, state = "●", "running"
		}
		fmt.Printf("%s  %-10s %-26s %s\n", mark, s.ID, s.Label, state)
		fmt.Printf("              %s\n", detail)
		if len(s.Requires) > 0 {
			fmt.Printf("              requires: %s\n", strings.Join(s.Requires, ", "))
		}
	}
	fmt.Println("\n● running   ○ stopped")
}

func cmdStatus(e *Engine, id string) {
	s, err := e.Find(id)
	if err != nil {
		fail(err.Error())
	}
	running, detail := e.Probe(s)
	state := "stopped"
	if running {
		state = "running"
	}
	fmt.Printf("%s — %s\n\n", s.Label, state)
	fmt.Printf("state   : %s\n", detail)
	fmt.Printf("id      : %s\n", s.ID)
	if len(s.Requires) > 0 {
		fmt.Printf("requires: %s\n", strings.Join(s.Requires, ", "))
	}
	fmt.Printf("\nwhy      : %s\n", s.Why)
	if len(s.Notes) > 0 {
		fmt.Println("\nworth knowing:")
		for _, n := range s.Notes {
			fmt.Printf("  - %s\n", n)
		}
	}
}

func cmdAction(e *Engine, verb, id string) {
	var msg string
	var err error
	if verb == "start" {
		msg, err = e.Start(id)
	} else {
		msg, err = e.Stop(id)
	}
	if err != nil {
		fail(err.Error())
	}
	fmt.Println(msg)
}

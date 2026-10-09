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
	"strings"
)

const usage = `service manager — E:\Coding\MCP_HUB

  manager                 starts the resident application (icon + MCP server)
  manager list            lists the services, with their state and their reason for being
  manager status <id>     detailed state of a service
  manager start <id>      starts a service and waits for it to respond
  manager stop <id>       stops a service and waits for it to be gone
  manager apply           shows what would be written for each client (touches nothing)
  manager apply --write   actually writes, after a timestamped backup
  manager reload          re-reads services.json live (resident mode)

  Extension happens in services.json, inventory.json and plugins/ — never here.
`

func main() {
	args := os.Args[1:]

	engine, err := LoadEngine(ManifestPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if len(args) == 0 {
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

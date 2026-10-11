package main

import (
	"strings"
	"testing"
)

// The instructions are the first thing an agent reads on the manager's own MCP
// surface. Their load-bearing rule is "using a tool starts what it needs" —
// without it, an agent sees a service STOPPED and either avoids the tool
// (observed: playwright) or improvises a headless instance (observed: Blender,
// where the tools live inside the visible app). Two phrases carry the rule;
// removing either would silently reintroduce the shyness, so both are pinned.
func TestSystemInstructionsStateUsingEqualsStarting(t *testing.T) {
	for _, phrase := range []string{"use the tool anyway", "headless"} {
		if !strings.Contains(systemInstructions, phrase) {
			t.Errorf("systemInstructions must contain %q — the \"using = starting\" rule; got:\n%s",
				phrase, systemInstructions)
		}
	}
}

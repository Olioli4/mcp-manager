package main

import (
	"os"
	"path/filepath"
	"testing"
)

// M7: first-run seeding must land where the application lives, not where the
// launcher happened to set the working directory.
//
// A shortcut, a Task-Scheduler entry, a WMI start: their CWD is somebody
// else's folder (often System32). The old seedFromExample wrote through
// assetPath(name), which falls back to a RELATIVE path whenever nothing of
// that name sits next to the exe — so on a first run it happily wrote
// services.json into System32 (or into the launcher's folder, or nowhere when
// that folder is protected), and the next launch with a different CWD found
// neither the seed nor the error: it seeded again, elsewhere.
//
// The test uses what is already a real executable on disk — the test binary —
// as the fake installation, and t.Chdir to move the working directory away.

// "Installed": the example sits next to the exe, the CWD is another folder.
func TestSeedFromExampleWritesNextToTheExeNotTheCWD(t *testing.T) {
	exePath, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	exeDir := filepath.Dir(exePath)

	example := filepath.Join(exeDir, "services.example.json")
	seeded := filepath.Join(exeDir, "services.json")
	content := `{"services": []}`

	if err := os.WriteFile(example, []byte(content), 0o644); err != nil {
		t.Fatalf("placing the example beside the fake exe: %v", err)
	}
	t.Cleanup(func() {
		os.Remove(example)
		os.Remove(seeded)
	})

	cwd := t.TempDir() // the launcher's unrelated working directory
	t.Chdir(cwd)

	if got := seedFromExample("services.json"); got != "services.example.json" {
		t.Fatalf("seedFromExample returned %q, want the example's name", got)
	}

	raw, err := os.ReadFile(seeded)
	if err != nil {
		t.Fatalf("the seed did not appear next to the exe: %v", err)
	}
	if string(raw) != content {
		t.Fatalf("the seed beside the exe holds %q, want the example verbatim", raw)
	}

	if _, err := os.Stat(filepath.Join(cwd, "services.json")); err == nil {
		t.Fatal("the seed also landed in the working directory: that is exactly the M7 bug")
	}
}

// "Development": no example beside the exe, CWD is the repository folder.
// assetPath falls back to the relative path; the seed must land in the CWD —
// where the developer can see and edit it — and must not touch the exe's
// (temporary) folder.
func TestSeedFromExampleInDevelopmentLandsInTheCWD(t *testing.T) {
	exeDir := "."
	if exePath, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exePath)
		// Guard: the real test binary's folder must not carry our fixture.
		if _, err := os.Stat(filepath.Join(exeDir, "services.json")); err == nil {
			t.Fatalf("stub: %s already has services.json from a previous run", exeDir)
		}
	}

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "services.example.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	if got := seedFromExample("services.json"); got != "services.example.json" {
		t.Fatalf("seedFromExample returned %q, want the example's name", got)
	}

	if _, err := os.Stat(filepath.Join(cwd, "services.json")); err != nil {
		t.Fatalf("development seeding did not land in the working directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(exeDir, "services.json")); err == nil {
		t.Fatalf("development seeding wrote into the exe folder %s", exeDir)
	}
}

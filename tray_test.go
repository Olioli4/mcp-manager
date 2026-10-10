package main

// Tray-side tests. The tray itself runs inside systray.Run and is not
// testable on its own, but its two pure parts — the label cutter and the
// poll interval — each carried a cheap-to-prove bug.

import (
	"testing"
	"time"
	"unicode/utf8"
)

// Cutting a label by BYTES slices multi-byte characters in half (é is two
// bytes, an em dash three): the menu shows broken text at the end, and
// anything reading the label afterwards sees invalid UTF-8. The cut must
// land on a rune boundary.
func TestTruncateCutsOnRunesNotBytes(t *testing.T) {
	in := "aéb—x" // é = 2 bytes, — = 3 bytes
	got := truncate(in, 3)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated label is invalid UTF-8: %q", got)
	}
	if want := "aéb…"; got != want {
		t.Fatalf("truncate(%q, 3) = %q; want %q", in, got, want)
	}
	if s := truncate("short", 10); s != "short" {
		t.Errorf("a label that fits must pass through untouched, got %q", s)
	}
}

// The poll loop re-reads the interval at every tick — time.Tick, once taken,
// never changed its mind, so "Re-read files" had no effect on the rhythm.
// The interval must come from the CURRENT manifest, not from boot time, and
// a nonsensical poll_seconds falls back to 5 s.
func TestPollIntervalFollowsTheLiveManifest(t *testing.T) {
	e := &Engine{}
	e.SetManifest(&Manifest{Settings: Settings{PollSeconds: 7}})
	if d := pollInterval(e); d != 7*time.Second {
		t.Fatalf("poll_seconds 7 gives %v; want 7s", d)
	}
	// A reload replaces the manifest object wholesale: the interval follows.
	e.SetManifest(&Manifest{Settings: Settings{PollSeconds: 3}})
	if d := pollInterval(e); d != 3*time.Second {
		t.Fatalf("after a reload with poll_seconds 3 gives %v; want 3s", d)
	}
	e.SetManifest(&Manifest{Settings: Settings{PollSeconds: 0}})
	if d := pollInterval(e); d != 5*time.Second {
		t.Fatalf("poll_seconds 0 must fall back to 5s, got %v", d)
	}
}

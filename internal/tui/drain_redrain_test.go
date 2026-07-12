package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// DESIGN-handoff-lifecycle-hardening bug A, codex iter-2 [P1]: the TUI's
// auto-drain only fires on fsnotify queue events. A BACKGROUNDED resume
// (drain exits 0, queue file kept, drain child's resume goroutine dead with
// the process) changes nothing on disk, so without a scheduled re-drain the
// handoff sits silently stuck until an unrelated queue event lands. These
// tests pin the drainDoneMsg consumer side of that contract; the producer
// side (the marker on stdout) is pinned by
// TestDrain_BackgroundedResumeCountsBackgrounded in cmd/fleet.

// shrinkRedrainDelay drops the re-drain tick to 1ms so collectMsgs can
// resolve the tea.Tick synchronously. Restored via t.Cleanup.
func shrinkRedrainDelay(t *testing.T) {
	t.Helper()
	prev := redrainDelay
	redrainDelay = time.Millisecond
	t.Cleanup(func() { redrainDelay = prev })
}

// collectMsgs resolves a tea.Cmd tree (flattening tea.BatchMsg) into the
// messages it produces. Tick cmds block until they fire — redrainDelay is
// shrunk to 1ms first, so this is a bounded wait on an already-armed timer,
// not a timing assertion.
func collectMsgs(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, collectMsgs(t, c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestUpdate_DrainBackgrounded_FlashesAndSchedulesRedrain(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	shrinkRedrainDelay(t)
	m := New("test")

	out := "fleet drain: slowcord resume still running; handoff completing in the background (queue file kept; a later drain verifies or retries)\n" +
		"fleet drain: 0 processed, 1 backgrounded (still completing; a later drain retries), 0 failed\n"
	next, cmd := m.Update(drainDoneMsg{out: out})
	nm := next.(Model)

	if nm.flash == nil {
		t.Fatal("backgrounded drain must surface a flash (silent = the stuck-handoff bug)")
	}
	if nm.flash.isErr {
		t.Errorf("backgrounded resume is not an error; flash.isErr = true, text: %s", nm.flash.text)
	}
	if !strings.Contains(nm.flash.text, "re-draining") {
		t.Errorf("flash must say a re-drain is scheduled, got: %s", nm.flash.text)
	}

	redrained := false
	for _, msg := range collectMsgs(t, cmd) {
		if _, ok := msg.(queueEventMsg); ok {
			redrained = true
		}
	}
	if !redrained {
		t.Error("backgrounded drain must schedule a re-drain (queueEventMsg tick); none produced")
	}
}

func TestUpdate_DrainRoutineSuccess_NoFlashNoRedrain(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	shrinkRedrainDelay(t)
	m := New("test")

	next, cmd := m.Update(drainDoneMsg{out: "fleet drain: 1 processed, 0 failed\n"})
	nm := next.(Model)

	if nm.flash != nil {
		t.Errorf("routine drain success must stay silent, got flash: %s", nm.flash.text)
	}
	for _, msg := range collectMsgs(t, cmd) {
		if _, ok := msg.(queueEventMsg); ok {
			t.Error("routine drain success must not schedule a re-drain")
		}
	}
}

func TestUpdate_DrainError_FlashesErrorNoRedrain(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	shrinkRedrainDelay(t)
	m := New("test")

	next, cmd := m.Update(drainDoneMsg{
		out: "fleet drain: ghostbas: boom\nfleet drain: 0 processed, 1 failed\n",
		err: errors.New("exit status 1"),
	})
	nm := next.(Model)

	if nm.flash == nil || !nm.flash.isErr {
		t.Fatalf("drain error must surface an error flash, got %+v", nm.flash)
	}
	for _, msg := range collectMsgs(t, cmd) {
		if _, ok := msg.(queueEventMsg); ok {
			t.Error("drain error must not schedule a re-drain (the failed queue file is retried by the next real queue event / cron drain)")
		}
	}
}

// Row 13 (DESIGN-drain-nonforcing): a PENDING drain result flashes an
// informational banner but schedules NO re-drain. The pending marker means a
// live handoff is held waiting on a manual `fleet handoff`; re-arming a 30s
// re-drain tick (as the backgrounded path does) would recreate the forever-
// retry churn drain-nonforcing removes.
func TestUpdate_DrainPending_FlashesNoRedrain(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	shrinkRedrainDelay(t)
	m := New("test")

	out := "fleet drain: 3f2a pending — worker handoff, run 'fleet handoff 3f2a'\n" +
		"fleet drain: 0 processed, 0 failed, 1 pending\n"
	next, cmd := m.Update(drainDoneMsg{out: out})
	nm := next.(Model)

	if nm.flash == nil {
		t.Fatal("pending drain must surface a flash (silent = invisible held handoff)")
	}
	if nm.flash.isErr {
		t.Errorf("pending handoff is not an error; flash.isErr = true, text: %s", nm.flash.text)
	}
	for _, msg := range collectMsgs(t, cmd) {
		if _, ok := msg.(queueEventMsg); ok {
			t.Error("pending drain must NOT schedule a re-drain (a held handoff waits for a manual fleet handoff)")
		}
	}
}

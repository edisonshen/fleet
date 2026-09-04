//go:build integration

// Integration lane (real tmux, no Claude): the coord-run handoff-resume
// nudger drives the PRODUCTION spawn transport (WaitForReadyToPrompt /
// PromptPendingInInputBox / ResubmitPendingPrompt / SendPromptKeysVerified)
// against a fake "input box" pane that behaves like a TUI whose Enter is
// swallowed by paste detection. The unit tests in coord_handoff_resume_test.go
// stub the transport; only a real pane proves the input box ends up holding
// exactly ONE copy of the resume directive across repeated nudges (operator
// saw three concatenated copies in one message before the fix).
//
// The fake box is a bash loop in raw tty mode: it appends every byte to a
// buffer and redraws `> <buf>` on the bottom row. Enter is ignored until a
// flag file appears ("submit mode"), after which Enter moves the buffer to a
// transcript line at the TOP of the pane — outside the verifier's bottom band,
// exactly how Claude Code re-renders an accepted turn. A second flag file
// makes the pane "busy" (redraw every 50ms) so readiness never converges.
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/tmux"
)

// fakeInputBoxScript is the pane program. $1 = submit flag path, $2 = busy
// flag path. Rows: 1 header, 2.. transcript, last row the input box.
const fakeInputBoxScript = `#!/usr/bin/env bash
submit_flag="$1"
busy_flag="$2"
stty -echo -icanon min 1 time 0 2>/dev/null
buf=""
transcript=""
draw() {
  rows=$(tput lines 2>/dev/null || echo 24)
  printf '\033[2J\033[H[fake-claude]\n%s' "$transcript"
  if [ -e "$busy_flag" ]; then printf '\n[busy %s]' "$(date +%s%N)"; fi
  printf '\033[%d;1H> %s' "$rows" "$buf"
}
draw
while :; do
  # Poll with a timeout so the busy flag is noticed without input; an
  # idle (non-busy) timeout redraws nothing, keeping the pane stable.
  read -r -N1 -t 0.05 c
  rc=$?
  if [ "$rc" -ne 0 ]; then
    [ "$rc" -gt 128 ] || exit 0
    if [ -e "$busy_flag" ]; then draw; fi
    continue
  fi
  case "$c" in
    $'\r'|$'\n')
      if [ -e "$submit_flag" ]; then
        transcript="${transcript}[submitted] ${buf}"$'\n'
        buf=""
      fi ;;
    *) buf="${buf}${c}" ;;
  esac
  draw
done
`

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fakeInputBox struct {
	session    string
	submitFlag string
	busyFlag   string
}

// startFakeInputBox spawns the fake pane in an isolated tmux server and
// returns its flags. requireTmux pins the spawn timing knobs; the verify
// delay is raised so the capture runs after the box has echoed the typed
// bytes (a 0ms verify would read the pane before bash redraws and report a
// false "submitted").
func startFakeInputBox(t *testing.T, session string) fakeInputBox {
	t.Helper()
	requireTmux(t)
	t.Setenv("FLEET_POST_SEND_VERIFY_MS", "400")
	t.Setenv("FLEET_POST_SEND_RETRY_MS", "100")

	dir := t.TempDir()
	script := filepath.Join(dir, "fakebox.sh")
	if err := os.WriteFile(script, []byte(fakeInputBoxScript), 0o755); err != nil {
		t.Fatalf("write fake box script: %v", err)
	}
	box := fakeInputBox{
		session:    session,
		submitFlag: filepath.Join(dir, "submit"),
		busyFlag:   filepath.Join(dir, "busy"),
	}
	if err := tmux.Spawn(session, dir, []string{"bash", script, box.submitFlag, box.busyFlag}, nil); err != nil {
		t.Fatalf("spawn fake box: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(session) })
	waitFor(t, 5*time.Second, "fake box drew its header", func() bool {
		pane, err := tmux.CapturePane(session)
		return err == nil && bytes.Contains(pane, []byte("[fake-claude]"))
	})
	return box
}

func touchFlag(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("touch %s: %v", path, err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// promptCopiesInPane counts whitespace-insensitive occurrences of the
// directive anywhere in the pane (input box + transcript). tmux soft-wraps
// the long prompt, so byte-exact substring counting undercounts.
func promptCopiesInPane(t *testing.T, session string) int {
	t.Helper()
	pane, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("capture pane: %v", err)
	}
	flat := strings.Join(strings.Fields(string(pane)), "")
	return strings.Count(flat, "Readyourhandoffdocat")
}

func startNudgerForTest(t *testing.T, agentID, project string, maxAttempts int) (initialC, wakeC chan time.Time, stderr *lockedBuffer, stop func()) {
	t.Helper()
	initialC = make(chan time.Time)
	wakeC = make(chan time.Time)
	stderr = &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	stopNudger := startHandoffResumeNudger(ctx, coordRunOpts{
		agentID:                    agentID,
		project:                    project,
		handoffResumeInitialDelayC: initialC,
		handoffResumeWakeC:         wakeC,
		handoffResumeMaxAttempts:   maxAttempts,
	}, stderr)
	stop = func() {
		cancel()
		stopNudger()
	}
	t.Cleanup(stop)
	return initialC, wakeC, stderr, stop
}

func wake(t *testing.T, c chan time.Time) {
	t.Helper()
	select {
	case c <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("nudger did not consume the wake (loop exited early?)")
	}
}

func nudgeWarnings(stderr *lockedBuffer) int {
	return strings.Count(stderr.String(), "coord-run: warning: resume nudge")
}

// Enter-swallowing pane: nudge 1 types the prompt once, nudges 2..N press
// Enter only, so the box never holds a second copy. Flipping the pane to
// submit mode makes the pending Enter-only nudge land the turn, and a marker
// catch-up stops the loop without a diagnostic.
func TestHandoffResumeNudger_Integration_EnterSwallowed_TypesPromptOnce(t *testing.T) {
	const (
		agentID = "nudgeint1"
		project = "nudge-int-swallow"
		session = "fleet-test-fakebox-swallow"
	)
	box := startFakeInputBox(t, session)

	docPath := filepath.Join(t.TempDir(), "handoffs", "e509aead-20260904-061851-99be.md")
	setupHandoffResumeRecord(t, agentID, project, session, &docPath)
	writeTestCoordState(t, project, map[string]any{resumedHandoffPathCoordStateKey: "/stale/handoff.md"})
	prompt := handoff.ResumePrompt(docPath)

	initialC, wakeC, stderr, stop := startNudgerForTest(t, agentID, project, 4)

	// Nudge 1: box clear -> full prompt typed once; Enter swallowed.
	wake(t, initialC)
	waitFor(t, 15*time.Second, "nudge 1 to report unsubmitted", func() bool { return nudgeWarnings(stderr) == 1 })
	if got := promptCopiesInPane(t, session); got != 1 {
		t.Fatalf("after nudge 1: %d prompt copies in pane, want 1\n%s", got, stderr.String())
	}
	if !spawn.PromptPendingInInputBox(session, prompt) {
		t.Fatal("after nudge 1: production PromptPendingInInputBox should see the prompt in the box")
	}

	// Nudge 2: prompt pending -> Enter only, never retyped.
	wake(t, wakeC)
	waitFor(t, 15*time.Second, "nudge 2 to report unsubmitted", func() bool { return nudgeWarnings(stderr) == 2 })
	if got := promptCopiesInPane(t, session); got != 1 {
		t.Fatalf("after nudge 2: %d prompt copies in pane, want 1 (prompt was retyped)\n%s", got, stderr.String())
	}

	// Nudge 3: pane now accepts Enter -> the pending copy submits; the
	// transcript echo at the top is the ONLY copy left, and the box is clear.
	touchFlag(t, box.submitFlag)
	wake(t, wakeC)
	waitFor(t, 15*time.Second, "box to submit the pending prompt", func() bool {
		return !spawn.PromptPendingInInputBox(session, prompt)
	})
	if got := promptCopiesInPane(t, session); got != 1 {
		t.Fatalf("after submit: %d prompt copies in pane, want exactly 1 transcript echo\n%s", got, stderr.String())
	}

	// Marker catches up (handoff_resume.py's job). The loop is either still
	// verifying nudge 3 (it then sees the marker and exits on its own) or
	// already parked on wakeC (the wake below makes it re-read the marker and
	// exit). Either way nothing more may be typed and no diagnostic emitted.
	writeTestCoordState(t, project, map[string]any{resumedHandoffPathCoordStateKey: docPath})
	select {
	case wakeC <- time.Now():
	case <-time.After(3 * time.Second):
	}
	stop()
	if got := promptCopiesInPane(t, session); got != 1 {
		t.Fatalf("after marker catch-up: %d prompt copies, want 1\n%s", got, stderr.String())
	}
	if n := nudgeWarnings(stderr); n != 2 {
		t.Fatalf("nudge 3 should succeed silently; got %d warnings\n%s", n, stderr.String())
	}
	if strings.Contains(stderr.String(), "was not applied") {
		t.Fatalf("unexpected operator diagnostic after successful submit:\n%s", stderr.String())
	}
}

// Busy pane: readiness never converges, so the nudge types NOTHING and does
// not consume an attempt. Once the pane settles, the next wake types the
// prompt exactly once.
func TestHandoffResumeNudger_Integration_BusyPane_TypesNothingUntilSettled(t *testing.T) {
	const (
		agentID = "nudgeint2"
		project = "nudge-int-busy"
		session = "fleet-test-fakebox-busy"
	)
	box := startFakeInputBox(t, session)
	touchFlag(t, box.busyFlag)

	docPath := filepath.Join(t.TempDir(), "handoffs", "busy.md")
	setupHandoffResumeRecord(t, agentID, project, session, &docPath)
	writeTestCoordState(t, project, map[string]any{resumedHandoffPathCoordStateKey: ""})

	initialC, wakeC, stderr, _ := startNudgerForTest(t, agentID, project, 2)

	// Two busy wakes: readiness (FLEET_INITIAL_PROMPT_MAX_MS=1000) fails each
	// time, nothing is typed, no attempt is counted. maxAttempts=2 makes this
	// load-bearing: if busy wakes were counted the loop would already have
	// emitted the diagnostic and exited before the pane settles.
	wake(t, initialC)
	wake(t, wakeC)
	// The second wake is only consumed once the first nudge returned, so by
	// now the first busy readiness poll has finished. Give the second one its
	// own readiness window before asserting the pane is still untouched.
	time.Sleep(1500 * time.Millisecond)
	if got := promptCopiesInPane(t, session); got != 0 {
		t.Fatalf("busy pane received %d prompt copies, want 0\n%s", got, stderr.String())
	}
	if n := nudgeWarnings(stderr); n != 0 {
		t.Fatalf("busy wakes must not count as attempts; got %d warnings\n%s", n, stderr.String())
	}
	if strings.Contains(stderr.String(), "was not applied") {
		t.Fatalf("busy wakes consumed the attempt budget:\n%s", stderr.String())
	}

	// Pane settles -> exactly one copy typed on the next wake.
	if err := os.Remove(box.busyFlag); err != nil {
		t.Fatalf("clear busy flag: %v", err)
	}
	wake(t, wakeC)
	waitFor(t, 15*time.Second, "settled pane to receive the prompt", func() bool {
		return promptCopiesInPane(t, session) >= 1
	})
	waitFor(t, 15*time.Second, "nudge to report unsubmitted", func() bool { return nudgeWarnings(stderr) == 1 })
	if got := promptCopiesInPane(t, session); got != 1 {
		t.Fatalf("settled pane holds %d prompt copies, want 1\n%s", got, stderr.String())
	}
}

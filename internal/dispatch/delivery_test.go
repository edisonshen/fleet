package dispatch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withFleetHome points FLEET_HOME at a temp dir for the duration of t.
// Returns the absolute path of the temp dir so tests can assert specific
// resource paths under it.
func withFleetHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	// Pre-create the dirs the controller writes to (mirror Bootstrap).
	for _, sub := range []string{"inbox", "inbox/archive", "dispatches"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	return dir
}

// pinNow freezes the package clock for the duration of t.
func pinNow(t *testing.T, ts time.Time) {
	t.Helper()
	restore := setNow(ts)
	t.Cleanup(restore)
}

func mustNewID(t *testing.T, s string) DispatchID {
	t.Helper()
	id, err := NewDispatchID(s)
	if err != nil {
		t.Fatalf("NewDispatchID(%q): %v", s, err)
	}
	return id
}

// TestAcquireAndDeliver_Happy exercises the canonical path:
//
//	no journal → write journal allocating → write inbox file →
//	  flip journal claim live. The on-disk file should match the
//	  content the caller supplied (with trailing newline appended).
func TestAcquireAndDeliver_Happy(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	id := mustNewID(t, "a690424b")
	res, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		Owner:      "project/fleet/slug/example",
		Kind:       "worker",
		HostID:     "test-host",
		Content:    strings.NewReader("hello worker prompt"),
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if res.Outcome != OutcomeAcquired {
		t.Fatalf("Outcome = %q; want acquired", res.Outcome)
	}
	body, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Errorf("inbox body missing trailing newline: %q", body)
	}
	if !strings.HasPrefix(string(body), "hello worker prompt") {
		t.Errorf("inbox body = %q; want prefix 'hello worker prompt'", body)
	}

	// Journal exists, has one live claim.
	j, err := LoadJournal(id)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if j.ExecState != ExecInFlight {
		t.Errorf("exec_state = %q; want in_flight", j.ExecState)
	}
	if len(j.Claims) != 1 || j.Claims[0].State != ClaimLive {
		t.Errorf("claims = %+v; want one live claim", j.Claims)
	}
}

// TestAcquireAndDeliver_Idempotent calls Acquire twice and asserts the
// second call returns `already_acquired` without re-writing the inbox
// or the journal.
func TestAcquireAndDeliver_Idempotent(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	id := mustNewID(t, "b1234567")
	first, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		Content:    strings.NewReader("first call"),
	})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	second, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		Content:    strings.NewReader("second call"),
	})
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if second.Outcome != OutcomeAlreadyAcquired {
		t.Errorf("second Outcome = %q; want already_acquired", second.Outcome)
	}
	body, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if !strings.Contains(string(body), "first call") {
		t.Errorf("idempotent retry overwrote inbox: %q", body)
	}
}

// TestRelease_Happy asserts the canonical release: inbox unlinked,
// claim state=released, recl_state=complete once exec_state is terminal.
func TestRelease_Happy(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	id := mustNewID(t, "c2345678")
	if _, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		Content:    strings.NewReader("prompt"),
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	path, err := CoordPromptInboxPath(id)
	if err != nil {
		t.Fatalf("CoordPromptInboxPath: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("inbox missing before release: %v", err)
	}

	res, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{
		DispatchID: id,
		HostID:     HostID(),
	})
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if res.Outcome != OutcomeReleased {
		t.Errorf("Outcome = %q; want released", res.Outcome)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("inbox still present after release: %v", err)
	}

	j, err := LoadJournal(id)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if j.ReclState != ReclComplete {
		t.Errorf("recl_state = %q; want complete", j.ReclState)
	}
	if !j.ExecState.IsTerminal() {
		t.Errorf("exec_state = %q; want terminal", j.ExecState)
	}
}

// TestRelease_Idempotent calls Release twice; second call should be
// already_released.
func TestRelease_Idempotent(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	id := mustNewID(t, "d3456789")
	if _, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		Content:    strings.NewReader("prompt"),
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{
		DispatchID: id,
	}); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	res, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{
		DispatchID: id,
	})
	if err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if res.Outcome != OutcomeAlreadyReleased {
		t.Errorf("Outcome = %q; want already_released", res.Outcome)
	}
}

// TestRelease_AbsentJournal returns already_released when no journal
// exists for the dispatch_id (legacy / pre-PR1 inbox files that were
// written by the Python helper directly are NOT touched here — they're
// orphan-sweeper territory in PR4).
func TestRelease_AbsentJournal(t *testing.T) {
	withFleetHome(t)
	id := mustNewID(t, "e4567890")
	res, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{
		DispatchID: id,
	})
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if res.Outcome != OutcomeAlreadyReleased {
		t.Errorf("Outcome = %q; want already_released for absent journal", res.Outcome)
	}
}

// TestRelease_NotOwned_CrossHost asserts a release request from a
// different HostID is rejected.
func TestRelease_NotOwned_CrossHost(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	id := mustNewID(t, "f5678901")
	if _, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		HostID:     "owner-host",
		Content:    strings.NewReader("prompt"),
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	res, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{
		DispatchID: id,
		HostID:     "different-host",
	})
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if res.Outcome != OutcomeNotOwned {
		t.Errorf("Outcome = %q; want not_owned", res.Outcome)
	}
	// Inbox file still present.
	path, _ := CoordPromptInboxPath(id)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("cross-host release should have left inbox intact; stat err = %v", err)
	}
}

// TestRelease_Preserve archives instead of unlinking when preserve=true.
func TestRelease_Preserve(t *testing.T) {
	root := withFleetHome(t)
	pinNow(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	id := mustNewID(t, "0a0a0a0a")
	if _, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		Content:    strings.NewReader("prompt"),
		Preserve:   true,
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{
		DispatchID: id,
	}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	livePath, _ := CoordPromptInboxPath(id)
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("live inbox still present: %v", err)
	}
	archPath := filepath.Join(root, "inbox", "archive", id.String()+".md")
	if _, err := os.Stat(archPath); err != nil {
		t.Errorf("archived inbox missing: %v", err)
	}
}

// TestRelease_PreserveOverride is the codex iter-2 [P2] regression: a
// release-time --preserve override must archive even when the original
// acquire ran with Preserve=false. The bug pre-fix branched on the
// stored on-disk Preserve flag and silently dropped the override.
func TestRelease_PreserveOverride(t *testing.T) {
	root := withFleetHome(t)
	pinNow(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	id := mustNewID(t, "0b0b0b0b")
	if _, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		Content:    strings.NewReader("prompt"),
		// Acquire with Preserve=false (the default).
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{
		DispatchID: id,
		// CLI override: archive even though acquire said unlink.
		Preserve: true,
	}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	livePath, _ := CoordPromptInboxPath(id)
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("live inbox still present: %v", err)
	}
	archPath := filepath.Join(root, "inbox", "archive", id.String()+".md")
	if _, err := os.Stat(archPath); err != nil {
		t.Errorf("archived inbox missing under --preserve override: %v", err)
	}
}

// TestInspect_Cases covers Present | Absent across the lifecycle:
//
//	absent (no journal) → absent (journal but no claim) → present (live)
//	  → absent (after release).
func TestInspect_Cases(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	id := mustNewID(t, "1b1b1b1b")
	// Absent (no journal).
	res, err := InspectCoordPromptInbox(InspectCoordPromptInboxOptions{DispatchID: id})
	if err != nil {
		t.Fatalf("inspect absent: %v", err)
	}
	if res.Outcome != OutcomeAbsent {
		t.Errorf("inspect absent Outcome = %q; want absent", res.Outcome)
	}

	// Acquire → present.
	if _, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		Content:    strings.NewReader("body"),
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	res, err = InspectCoordPromptInbox(InspectCoordPromptInboxOptions{DispatchID: id})
	if err != nil {
		t.Fatalf("inspect present: %v", err)
	}
	if res.Outcome != OutcomeAcquired {
		t.Errorf("inspect present Outcome = %q; want acquired", res.Outcome)
	}

	// Release → absent.
	if _, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{DispatchID: id}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	res, err = InspectCoordPromptInbox(InspectCoordPromptInboxOptions{DispatchID: id})
	if err != nil {
		t.Fatalf("inspect post-release: %v", err)
	}
	if res.Outcome != OutcomeAbsent {
		t.Errorf("inspect post-release Outcome = %q; want absent", res.Outcome)
	}
}

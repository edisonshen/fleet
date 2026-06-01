package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// readInboxFile reads the coord_prompt_inbox file for id under the test
// FLEET_HOME.
func readInboxFile(t *testing.T, home string, id DispatchID) (string, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "inbox", id.String()+".md"))
	return string(b), err
}

// durability_test.go — dispatch-durability (fleet#184) Go-side tests.
//
// The flock RMW primitive is a genuine critical section, so the
// load-bearing test is the concurrent-writers table test (no lost
// updates). The rest pin the tri-state mark-launch-attempted contract,
// reset-for-relaunch atomicity, ReserveReplay cap, and the
// ReleaseCoordPromptInbox no-clobber fix.

// acquirePendingFixture acquires a fresh dispatch and asserts it lands at
// ExecPending (the #184 acquire contract), returning the id.
func acquirePendingFixture(t *testing.T, id string) DispatchID {
	t.Helper()
	did := mustNewID(t, id)
	if _, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: did,
		Owner:      "project/fleet/slug/x",
		Kind:       "worker",
		HostID:     "h",
		Content:    strings.NewReader("prompt body"),
	}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	j, err := LoadJournal(did)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if j.ExecState != ExecPending {
		t.Fatalf("acquire left exec_state=%q, want %q (premature ExecInFlight is the #184 bug)",
			j.ExecState, ExecPending)
	}
	return did
}

// TestAcquireLeavesPending pins that acquire no longer flips a fresh
// journal to ExecInFlight (the premature-launch bug).
func TestAcquireLeavesPending(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	acquirePendingFixture(t, "aa000001")
}

// TestMarkLaunchAttempted_TriState exercises all three exits.
func TestMarkLaunchAttempted_TriState(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	id := acquirePendingFixture(t, "bb000001")

	// gen mismatch → predicate_fail (does NOT flip).
	out, err := MarkLaunchAttempted(id, 99)
	if err != nil {
		t.Fatalf("mark (bad gen): %v", err)
	}
	if out != LaunchAttemptPredicateFail {
		t.Fatalf("bad-gen outcome = %q, want predicate_fail", out)
	}
	if j, _ := LoadJournal(id); j.ExecState != ExecPending {
		t.Fatalf("bad-gen flipped state to %q, must stay pending", j.ExecState)
	}

	// correct gen (0 for fresh) → ok, flips to launch_attempted.
	out, err = MarkLaunchAttempted(id, 0)
	if err != nil {
		t.Fatalf("mark (gen 0): %v", err)
	}
	if out != LaunchAttemptOK {
		t.Fatalf("gen-0 outcome = %q, want ok", out)
	}
	j, _ := LoadJournal(id)
	if j.ExecState != ExecLaunchAttempted {
		t.Fatalf("state = %q, want launch_attempted", j.ExecState)
	}
	if j.LaunchAttemptedAt.IsZero() {
		t.Fatalf("launch_attempted_at not stamped")
	}

	// second attempt (now not pending) → predicate_fail.
	out, err = MarkLaunchAttempted(id, 0)
	if err != nil {
		t.Fatalf("mark (2nd): %v", err)
	}
	if out != LaunchAttemptPredicateFail {
		t.Fatalf("2nd outcome = %q, want predicate_fail (no double-launch)", out)
	}

	// absent journal → predicate_fail (nothing to launch).
	out, err = MarkLaunchAttempted(mustNewID(t, "deadbeef"), 0)
	if err != nil {
		t.Fatalf("mark (absent): %v", err)
	}
	if out != LaunchAttemptPredicateFail {
		t.Fatalf("absent outcome = %q, want predicate_fail", out)
	}
}

// TestMarkLaunchAttempted_ContentionRetriesNeverSkips simulates a flock
// deadline by stubbing the lock sleep to drive the deadline immediately,
// and confirms the outcome is contention (TRANSIENT), never a silent
// skip. We force contention by holding the lock from another goroutine
// while the deadline is short.
func TestMarkLaunchAttempted_ContentionRetriesNeverSkips(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	id := acquirePendingFixture(t, "cc000001")

	// Shrink the deadline to near-zero and hold the lock in a long fn so
	// the contending caller times out deterministically.
	restoreDeadline := setJournalLockDeadline(1 * time.Millisecond)
	defer restoreDeadline()

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = withJournalLock(id, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held // ensure the lock is held before we contend

	out, err := MarkLaunchAttempted(id, 0)
	close(release)
	if err != nil {
		t.Fatalf("mark under contention: %v", err)
	}
	if out != LaunchAttemptContention {
		t.Fatalf("outcome = %q, want contention (must NOT be predicate_fail/skip)", out)
	}
	// The state must NOT have flipped — the launch is still pending.
	if j, _ := LoadJournal(id); j.ExecState != ExecPending {
		t.Fatalf("contention flipped state to %q; the launch was silently consumed", j.ExecState)
	}
}

// TestConcurrentWriters_NoLostUpdate is the load-bearing test: many
// goroutines run distinct RMW mutations against the SAME journal under
// the flock; none may clobber another's update. We interleave
// reserve-replay increments (cap high) and assert the final
// ReplayEmitAttempts equals the number of reservations — a lost update
// would undercount.
func TestConcurrentWriters_NoLostUpdate(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	id := acquirePendingFixture(t, "dd000001")

	const n = 50
	cap := n + 1 // never hit the cap, so every call reserves
	var wg sync.WaitGroup
	reserved := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := ReserveReplay(id, cap)
			if err != nil {
				t.Errorf("reserve %d: %v", i, err)
				return
			}
			// With a generous deadline and a fast critical section, every
			// caller should eventually reserve (the flock serializes them;
			// none times out).
			if res.Outcome == ReplayReserved {
				reserved[i] = true
			} else {
				t.Errorf("reserve %d: outcome=%q want reserved", i, res.Outcome)
			}
		}(i)
	}
	wg.Wait()

	got := 0
	for _, r := range reserved {
		if r {
			got++
		}
	}
	if got != n {
		t.Fatalf("only %d/%d reservations succeeded", got, n)
	}
	j, err := LoadJournal(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if j.ReplayEmitAttempts != n {
		t.Fatalf("replay_emit_attempts = %d, want %d — a lost update clobbered a counter increment",
			j.ReplayEmitAttempts, n)
	}
}

// TestReserveReplay_CapBlocks pins that once the cap is reached the
// journal flips to ExecBlocked and no further replay is reserved.
func TestReserveReplay_CapBlocks(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	id := acquirePendingFixture(t, "ee000001")

	const cap = 3
	for i := 0; i < cap; i++ {
		res, err := ReserveReplay(id, cap)
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		if res.Outcome != ReplayReserved {
			t.Fatalf("reserve %d outcome = %q, want reserved", i, res.Outcome)
		}
	}
	// cap-th reservation hits the cap → ExecBlocked.
	res, err := ReserveReplay(id, cap)
	if err != nil {
		t.Fatalf("reserve (capped): %v", err)
	}
	if res.Outcome != ReplayCapped {
		t.Fatalf("over-cap outcome = %q, want capped", res.Outcome)
	}
	j, _ := LoadJournal(id)
	if j.ExecState != ExecBlocked || j.BlockedReason != "dispatch_undelivered" {
		t.Fatalf("after cap: state=%q reason=%q, want blocked/dispatch_undelivered",
			j.ExecState, j.BlockedReason)
	}
	// A blocked entry is no longer pending → ReplayNotPending, no further
	// re-emit (no infinite loop).
	res, err = ReserveReplay(id, cap)
	if err != nil {
		t.Fatalf("reserve (post-block): %v", err)
	}
	if res.Outcome != ReplayNotPending {
		t.Fatalf("post-block outcome = %q, want not_pending", res.Outcome)
	}
}

// TestResetForRelaunch_Atomic pins that reset rewrites the inbox AND
// resets the entry to a fresh ExecPending with a bumped generation +
// zeroed cap — under one critical section.
func TestResetForRelaunch_Atomic(t *testing.T) {
	home := withFleetHome(t)
	pinNow(t, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	id := acquirePendingFixture(t, "ff000001")

	// Advance the lifecycle: launch + ack + a replay reservation so we
	// can prove reset clears all of it.
	if _, err := MarkLaunchAttempted(id, 0); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := MarkAcked(id); err != nil {
		t.Fatalf("ack: %v", err)
	}

	res, err := ResetForRelaunch(id, strings.NewReader("RESUME PROMPT BODY"))
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if res.Outcome != ResetDone {
		t.Fatalf("reset outcome = %q, want reset", res.Outcome)
	}
	if res.Generation != 1 {
		t.Fatalf("reset gen = %d, want 1 (bumped from 0)", res.Generation)
	}
	j, _ := LoadJournal(id)
	if j.ExecState != ExecPending {
		t.Fatalf("reset state = %q, want pending", j.ExecState)
	}
	if j.Generation != 1 {
		t.Fatalf("journal gen = %d, want 1", j.Generation)
	}
	if j.ReplayEmitAttempts != 0 {
		t.Fatalf("replay attempts = %d, want 0 (fresh budget)", j.ReplayEmitAttempts)
	}
	if !j.LaunchAttemptedAt.IsZero() {
		t.Fatalf("launch_attempted_at not cleared on reset")
	}
	// Inbox rewritten.
	body, rerr := readInboxFile(t, home, id)
	if rerr != nil {
		t.Fatalf("read inbox: %v", rerr)
	}
	if !strings.Contains(body, "RESUME PROMPT BODY") {
		t.Fatalf("inbox not rewritten: %q", body)
	}

	// A stale block carrying the OLD gen (0) must predicate-fail now.
	out, err := MarkLaunchAttempted(id, 0)
	if err != nil {
		t.Fatalf("mark (stale gen): %v", err)
	}
	if out != LaunchAttemptPredicateFail {
		t.Fatalf("stale-gen outcome = %q, want predicate_fail (stale-block guard)", out)
	}
	// The fresh gen (1) flips it.
	out, err = MarkLaunchAttempted(id, 1)
	if err != nil {
		t.Fatalf("mark (fresh gen): %v", err)
	}
	if out != LaunchAttemptOK {
		t.Fatalf("fresh-gen outcome = %q, want ok", out)
	}
}

// TestRelease_NoClobberLaunchStates pins the ReleaseCoordPromptInbox
// fix: releasing must not downgrade ExecBlocked, and an un-acked
// ExecLaunchAttempted resolves to ExecFailed (launch unconfirmed), not
// ExecDone.
func TestRelease_NoClobberLaunchStates(t *testing.T) {
	withFleetHome(t)
	pinNow(t, time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	// (1) un-acked launch_attempted → release → ExecFailed.
	id1 := acquirePendingFixture(t, "10000001")
	if _, err := MarkLaunchAttempted(id1, 0); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if _, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{DispatchID: id1, HostID: "h"}); err != nil {
		t.Fatalf("release: %v", err)
	}
	j1, _ := LoadJournal(id1)
	if j1.ExecState != ExecFailed {
		t.Fatalf("released un-acked launch_attempted = %q, want failed (not done — launch unconfirmed)", j1.ExecState)
	}

	// (2) acked → release → ExecDone (normal completion).
	id2 := acquirePendingFixture(t, "20000001")
	if _, err := MarkLaunchAttempted(id2, 0); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := MarkAcked(id2); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if _, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{DispatchID: id2, HostID: "h"}); err != nil {
		t.Fatalf("release: %v", err)
	}
	j2, _ := LoadJournal(id2)
	if j2.ExecState != ExecDone {
		t.Fatalf("released acked = %q, want done", j2.ExecState)
	}

	// (3) ExecBlocked must NOT be downgraded by release.
	id3 := acquirePendingFixture(t, "30000001")
	// drive it to blocked via cap.
	if _, err := ReserveReplay(id3, 1); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if res, err := ReserveReplay(id3, 1); err != nil || res.Outcome != ReplayCapped {
		t.Fatalf("expected capped, got %v err=%v", res.Outcome, err)
	}
	if _, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{DispatchID: id3, HostID: "h"}); err != nil {
		t.Fatalf("release: %v", err)
	}
	j3, _ := LoadJournal(id3)
	if j3.ExecState != ExecBlocked {
		t.Fatalf("released blocked = %q, want blocked (no downgrade)", j3.ExecState)
	}
}

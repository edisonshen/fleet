package dispatch

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
)

// TestE2E_CoordPromptInbox_FullCycle is the CRITICAL regression test
// from DESIGN-dispatch-lifecycle.md §"PR1 — CRITICAL tests
// (plan-eng-review)". It proves the 30-file inbox leak is closed:
//
//	register a coord_prompt_inbox claim via AcquireAndDeliver
//	 → assert inbox file + journal both exist
//	 → simulate dispatch reaching terminal state
//	 → call Release
//	 → assert inbox unlinked AND journal recl_state=complete
//
// The test follows the test-infra pattern from plan-eng-review A9:
// `tmuxtest.IsolateSocket(t)` for per-test FLEET_TMUX_SOCKET isolation
// (the canonical helper). PR1 doesn't reach `tmux.Spawn` itself — the
// Delivery flow is filesystem-only — so we use IsolateSocket (not
// RequireTmux) to keep the test running on tmux-less CI. The isolation
// is still load-bearing: it ensures any future PR2 expansion that adds
// tmux_session claims inside this regression test inherits the
// socket-isolation invariant automatically.
func TestE2E_CoordPromptInbox_FullCycle(t *testing.T) {
	// Per-test tmux server isolation (forward-compat with PR2's
	// Exclusive controller for tmux_session).
	tmuxtest.IsolateSocket(t)
	// Per-test ~/.fleet/ home.
	withFleetHome(t)
	pinNow(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	id := mustNewID(t, "a690424b")
	prompt := "## fleet worker prompt\n\nDo something useful.\n"

	// ---- 1. Acquire ----
	acq, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		Owner:      "project/fleet/slug/e2e-regression",
		Kind:       "worker",
		HostID:     "test-host",
		Content:    strings.NewReader(prompt),
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if acq.Outcome != OutcomeAcquired {
		t.Fatalf("Outcome = %q; want acquired", acq.Outcome)
	}

	// Inbox file present, journal present + ExecInFlight.
	if _, err := os.Stat(acq.Path); err != nil {
		t.Fatalf("inbox missing post-acquire: %v", err)
	}
	j, err := LoadJournal(id)
	if err != nil {
		t.Fatalf("LoadJournal post-acquire: %v", err)
	}
	if j.ExecState != ExecInFlight {
		t.Errorf("exec_state = %q post-acquire; want in_flight", j.ExecState)
	}
	if j.ReclState != ReclPending {
		t.Errorf("recl_state = %q post-acquire; want pending", j.ReclState)
	}
	if len(j.Claims) != 1 || j.Claims[0].State != ClaimLive {
		t.Errorf("claims = %+v; want one live", j.Claims)
	}

	// ---- 2. Simulate dispatch reaching terminal state via direct
	// journal write (the dispatch-aware caller will do this once
	// PR2/PR3 plumb exec_state through the spawn/handoff paths). PR1
	// surfaces only the file-based teardown, which is what the leak
	// fix needs. ----
	j.ExecState = ExecDone
	if err := SaveJournal(j); err != nil {
		t.Fatalf("SaveJournal terminal: %v", err)
	}

	// ---- 3. Release ----
	rel, err := ReleaseCoordPromptInbox(ReleaseCoordPromptInboxOptions{
		DispatchID: id,
		HostID:     "test-host",
	})
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if rel.Outcome != OutcomeReleased {
		t.Fatalf("Outcome = %q; want released", rel.Outcome)
	}

	// ---- 4. Assert inbox unlinked AND journal recl_state=complete ----
	if _, err := os.Stat(acq.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("inbox lingered post-release: %v", err)
	}
	j, err = LoadJournal(id)
	if err != nil {
		t.Fatalf("LoadJournal post-release: %v", err)
	}
	if j.ReclState != ReclComplete {
		t.Errorf("recl_state = %q post-release; want complete", j.ReclState)
	}
	if !j.ExecState.IsTerminal() {
		t.Errorf("exec_state = %q post-release; want terminal", j.ExecState)
	}
	if len(j.Claims) != 1 || j.Claims[0].State != ClaimReleased {
		t.Errorf("claims = %+v; want one released", j.Claims)
	}
}

// TestE2E_CrashRecovery_OrphanedAllocatingClaim is the T2 critical
// test from DESIGN-dispatch-lifecycle.md §"PR1 — CRITICAL tests": a
// dispatch crashes between AcquireAndDeliver's step-1 journal write
// and the closure invocation (the inbox file write). The journal is
// left with an `allocating` claim and no on-disk file.
//
// PR1 doesn't ship the sweeper; the recovery test mocks the sweeper
// behavior by inspecting the journal state directly and asserting:
//   - The claim is in state=allocating (the recoverable shape).
//   - Inspect returns Absent (the file isn't there).
//
// PR4 ships the actual sweeper that consumes this state. PR1 fixing
// the shape NOW means PR4 has a known target to operate against.
//
// We simulate the kill-9 by manually writing a journal with an
// allocating claim and asserting the controller's behavior under that
// orphaned state.
func TestE2E_CrashRecovery_OrphanedAllocatingClaim(t *testing.T) {
	tmuxtest.IsolateSocket(t)
	withFleetHome(t)
	pinNow(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	id := mustNewID(t, "b1234567")
	hostID := "test-host"
	path, err := CoordPromptInboxPath(id)
	if err != nil {
		t.Fatalf("CoordPromptInboxPath: %v", err)
	}

	// Construct a journal that simulates a crash after step-1
	// (allocating-claim written) but BEFORE step-2 (inbox file
	// written). This matches the disk shape AcquireAndDeliver leaves
	// behind if the process gets SIGKILL between the SaveJournal
	// (allocating) and the state.WriteAtomic of the inbox file.
	now := nowFunc().UTC()
	claim := DeliveryClaim{
		Kind:      KindCoordPromptInbox,
		ID:        id.String(),
		Path:      path,
		OwnerID:   id,
		HostID:    hostID,
		CreatedAt: now,
	}
	allocData, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	j := newJournal(id, "worker", "owner", hostID, "", now)
	j.ExecState = ExecInFlight
	j.Claims = []ClaimInline{{
		Class: ClassDelivery,
		Kind:  KindCoordPromptInbox,
		State: ClaimAllocating,
		Data:  allocData,
	}}
	if err := SaveJournal(j); err != nil {
		t.Fatalf("SaveJournal: %v", err)
	}

	// Sanity: inbox file is NOT on disk — we crashed before writing.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inbox should not exist after simulated crash: %v", err)
	}

	// ---- Sweeper-equivalent inspection: confirm the recovery target
	// shape. PR4's sweeper walks journals with claims in state=allocating
	// and either retries (file present, claim still allocating) or drops
	// (no file). PR1 asserts the shape is correct so PR4 has a target. ----
	loaded, err := LoadJournal(id)
	if err != nil {
		t.Fatalf("LoadJournal post-crash: %v", err)
	}
	if len(loaded.Claims) != 1 {
		t.Fatalf("claims = %+v; want exactly one entry", loaded.Claims)
	}
	if loaded.Claims[0].State != ClaimAllocating {
		t.Errorf("post-crash claim state = %q; want allocating (sweeper target)", loaded.Claims[0].State)
	}

	// ---- Controller behavior under the orphaned state ----
	controller := NewCoordPromptInboxController()
	status, claim2, err := controller.Inspect(loaded, KindCoordPromptInbox)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	// Inspect returns Absent because the claim is not state=live (it's
	// allocating). The controller doesn't auto-recover; sweeper does.
	if status != DeliveryAbsent {
		t.Errorf("Inspect status = %q; want Absent (orphaned allocating)", status)
	}
	if claim2 == nil {
		t.Errorf("Inspect should still return the claim metadata; got nil")
	}

	// ---- Recovery path 1: caller retries AcquireAndDeliver. The
	// controller's idempotency logic drops the prior allocating entry
	// and runs a fresh allocating → live cycle. ----
	res, err := AcquireCoordPromptInbox(AcquireCoordPromptInboxOptions{
		DispatchID: id,
		HostID:     hostID,
		Content:    strings.NewReader("re-delivered prompt"),
	})
	if err != nil {
		t.Fatalf("retry Acquire: %v", err)
	}
	if res.Outcome != OutcomeAcquired {
		t.Errorf("retry Outcome = %q; want acquired (allocating → live)", res.Outcome)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("inbox missing after retry: %v", err)
	}
	final, err := LoadJournal(id)
	if err != nil {
		t.Fatalf("LoadJournal post-retry: %v", err)
	}
	if len(final.Claims) != 1 || final.Claims[0].State != ClaimLive {
		t.Errorf("post-retry claims = %+v; want one live", final.Claims)
	}
}

//go:build linux || darwin

package handoffop

// atomic_coord_swap_unix_test.go — the AtomicCoordSwap/createHandoffJournal-
// Resolving tests that exercise genuinely linux||darwin-only coordlock
// primitives directly (coordlock.AcquireLease, coordlock.PidStartNanos —
// unlike CurrentOwner/LiveOwner/CreateHandoffJournal etc., these have NO
// cross-platform stub in lease_owner_other.go / handoff_journal_other.go).
// Split out from atomic_coord_swap_test.go (review iter-7: GOOS=freebsd
// `go vet` failed with "undefined: coordlock.PidStartNanos" /
// "undefined: coordlock.AcquireLease") — mirrors the same split the round-5
// reviewer already did for coordreconcile_unix_test.go.

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/state"
)

// TestAtomicCoordSwap_JournalCollision_LiveSuccessor_SoftGuard covers the T8
// O_EXCL collision "live-not-holding" arm + the soft-guarantee: a leftover
// journal naming a DIFFERENT successor whose process is ALIVE is NOT clobbered,
// and the resulting create refusal only WARNS — flock-exclusivity is the real
// no-duplicate guard, so the swap still completes (retire + archive OLD).
func TestAtomicCoordSwap_JournalCollision_LiveSuccessor_SoftGuard(t *testing.T) {
	in, newRec := seedCoordSwap(t, "rainier", "oldcoord", "newcoord")
	fake := &fakeSwap{
		postReadyAlive: true,  // NEW alive
		postKillAlive:  false, // OLD dead after kill
	}
	restore := fake.install(t, newRec)
	defer restore()

	// Seed a leftover journal naming a live-but-not-holding successor (this test
	// process' pid ⇒ HandoffSuccessorAlive true). createHandoffJournalResolving
	// classifies it live-not-holding and REFUSES to overwrite.
	selfStart, ok := coordlock.PidStartNanos(os.Getpid())
	if !ok {
		t.Fatalf("PidStartNanos(self) failed")
	}
	if err := coordlock.CreateHandoffJournal(coordlock.HandoffJournal{
		Project: in.Project, SuccessorID: "othersucc", BarrierID: "b-other",
		SuccessorPID: os.Getpid(), SuccessorPidStart: selfStart,
	}); err != nil {
		t.Fatalf("seed leftover journal: %v", err)
	}

	var stderr bytes.Buffer
	res, err := AtomicCoordSwap(in, &stderr)
	// Soft guard: the journal-create refusal must NOT abort the swap.
	if err != nil {
		t.Fatalf("journal-create refusal must NOT abort the swap; got err=%v (stderr=%s)", err, stderr.String())
	}
	// The leftover (a live in-flight successor) is PRESERVED, never clobbered.
	assertHandoffJournalNames(t, in.Project, "othersucc")
	// Retire proceeds despite the refusal — NEW kept, OLD killed + archived.
	if !fake.killCalled {
		t.Errorf("Kill OLD must still run after a soft journal-create refusal")
	}
	if !res.OldArchived {
		t.Errorf("OldArchived = false; the swap must complete despite the journal-create refusal")
	}
	livePath, _ := state.AgentPath("oldcoord")
	if _, statErr := os.Stat(livePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("OLD live record still exists at %s; retire must proceed", livePath)
	}
	// Surface-don't-silo: the refusal is logged, not swallowed.
	if !strings.Contains(stderr.String(), "create handoff journal") {
		t.Errorf("expected a stderr warning about the failed journal create; got %q", stderr.String())
	}
}

// TestCreateHandoffJournalResolving_CommittedStale_DeletesAndRetries covers
// the "committed-stale" O_EXCL collision arm of createHandoffJournalResolving
// directly (a /review testing-specialist gap: only the "live-not-holding"
// soft-guard arm above had a test). A leftover journal whose SuccessorID
// names the CURRENT live flock holder is a prior handoff that committed
// ownership but never cleared its own journal (e.g. the winner's post-acquire
// DeleteHandoffJournal never ran). createHandoffJournalResolving must delete
// it and create the new entry — never refuse a fresh handoff cycle because of
// a leftover that already resolved in the caller's favor.
func TestCreateHandoffJournalResolving_CommittedStale_DeletesAndRetries(t *testing.T) {
	setupFleetHome(t)
	const project = "chjr-committed-stale"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	// "winner" is the CURRENT live flock holder — simulates a coord that
	// already won a prior handoff cycle but raced/crashed before it could
	// commit (DeleteHandoffJournal) its own leftover journal.
	lease, acquired, err := coordlock.AcquireLease(project, "winner")
	if err != nil || !acquired || lease == nil {
		t.Fatalf("AcquireLease(winner): acquired=%v err=%v", acquired, err)
	}
	defer lease.Release()

	if err := coordlock.CreateHandoffJournal(coordlock.HandoffJournal{
		Project: project, SuccessorID: "winner", BarrierID: "b-stale",
		// Liveness is irrelevant here: committedStale short-circuits it.
		SuccessorPID: 999999, SuccessorPidStart: 424242,
	}); err != nil {
		t.Fatalf("seed leftover journal: %v", err)
	}

	next := coordlock.HandoffJournal{Project: project, SuccessorID: "nextsucc", BarrierID: "b-next"}
	if err := createHandoffJournalResolving(next); err != nil {
		t.Fatalf("createHandoffJournalResolving must delete the committed-stale leftover and succeed; got %v", err)
	}
	assertHandoffJournalNames(t, project, "nextsucc")
}

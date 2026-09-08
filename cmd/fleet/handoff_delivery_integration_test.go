//go:build integration

// Integration lane (real tmux, no Claude): the lock-owner resume delivery
// shared by `fleet handoff` (graceful route) and `fleet drain` (pending-doc
// recovery) drives the PRODUCTION spawn transport from
// handoffdelivery.DefaultDeps() against the fake Enter-swallowing input box.
// Only the lease lookup is stubbed — the flock is not what #297/#299 failed
// on; the pane was. The unit tests in internal/handoffdelivery stub the
// transport; only a real pane proves the retry loop leaves exactly ONE copy
// of the prompt in the box and lands it once Enter is accepted.
package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/handoffdelivery"
	"github.com/edisonshen/fleet/internal/spawn"
)

type deliveryResult struct {
	rec *agent.Record
	err error
}

func startLockOwnerDelivery(t *testing.T, session, project string, timeout time.Duration) (string, *lockedBuffer, <-chan deliveryResult) {
	t.Helper()
	docPath := filepath.Join(t.TempDir(), "handoffs", "delivery.md")
	prompt := handoff.ResumePrompt(docPath)
	owner := &agent.Record{ID: "winner1", Project: project, TmuxSession: session}

	deps := handoffdelivery.DefaultDeps()
	deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
		return coordlock.Owner{AgentID: owner.ID, PID: 4242, PidStart: 99}, true
	}
	deps.LeaseRecordActive = func(string) bool { return true }
	deps.LoadAgent = func(string) (*agent.Record, error) { return owner, nil }

	stderr := &lockedBuffer{}
	done := make(chan deliveryResult, 1)
	go func() {
		rec, err := handoffdelivery.DeliverToCurrentOwner(handoffdelivery.Options{
			Project: project,
			Prompt:  prompt,
			Timeout: timeout,
			Poll:    100 * time.Millisecond,
			Stderr:  stderr,
		}, deps)
		done <- deliveryResult{rec: rec, err: err}
	}()
	return prompt, stderr, done
}

func deliveryRetryWarnings(stderr *lockedBuffer) int {
	return strings.Count(stderr.String(), "not yet submitted; retrying")
}

// Enter-swallowing pane: the first attempt types the prompt once, every retry
// presses Enter only, and flipping the pane to submit mode lets the pending
// Enter-only retry land the turn. Delivery returns the owner record.
func TestHandoffDelivery_Integration_EnterSwallowed_RetriesThenSubmits(t *testing.T) {
	const (
		project = "delivery-int-swallow"
		session = "fleet-test-fakebox-delivery-swallow"
	)
	box := startFakeInputBox(t, session)
	prompt, stderr, done := startLockOwnerDelivery(t, session, project, 60*time.Second)

	waitFor(t, 20*time.Second, "two unsubmitted retries", func() bool { return deliveryRetryWarnings(stderr) >= 2 })
	if got := promptCopiesInPane(t, session); got != 1 {
		t.Fatalf("after retries: %d prompt copies in pane, want 1 (prompt was retyped)\n%s", got, stderr.String())
	}
	if !spawn.PromptPendingInInputBox(session, prompt) {
		t.Fatalf("production PromptPendingInInputBox should see the prompt in the box\n%s", stderr.String())
	}

	touchFlag(t, box.submitFlag)
	var res deliveryResult
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("delivery did not return after the pane started accepting Enter\n%s", stderr.String())
	}
	if res.err != nil {
		t.Fatalf("DeliverToCurrentOwner: %v\n%s", res.err, stderr.String())
	}
	if res.rec == nil || res.rec.TmuxSession != session {
		t.Fatalf("rec = %+v, want the lock owner", res.rec)
	}
	if spawn.PromptPendingInInputBox(session, prompt) {
		t.Fatalf("prompt still pending in the input box after a successful delivery\n%s", stderr.String())
	}
	if got := promptCopiesInPane(t, session); got != 1 {
		t.Fatalf("after submit: %d prompt copies in pane, want exactly 1 transcript echo\n%s", got, stderr.String())
	}
}

// Pane that never accepts Enter: delivery keeps retrying with Enter only
// until the deadline, then fails with "not submitted" (so the caller keeps the
// queue/doc pending) while the box still holds exactly one copy.
func TestHandoffDelivery_Integration_EnterNeverAccepted_FailsWithoutRetyping(t *testing.T) {
	const (
		project = "delivery-int-stuck"
		session = "fleet-test-fakebox-delivery-stuck"
	)
	startFakeInputBox(t, session)
	prompt, stderr, done := startLockOwnerDelivery(t, session, project, 6*time.Second)

	var res deliveryResult
	select {
	case res = <-done:
	case <-time.After(45 * time.Second):
		t.Fatalf("delivery did not give up after its deadline\n%s", stderr.String())
	}
	if res.err == nil {
		t.Fatalf("expected an error for a prompt that was never submitted\n%s", stderr.String())
	}
	if !strings.Contains(res.err.Error(), "not submitted") {
		t.Fatalf("err = %v, want 'not submitted'", res.err)
	}
	if n := deliveryRetryWarnings(stderr); n < 2 {
		t.Fatalf("expected at least 2 retry warnings before the deadline, got %d\n%s", n, stderr.String())
	}
	if !spawn.PromptPendingInInputBox(session, prompt) {
		t.Fatalf("the single typed copy should still be pending in the box\n%s", stderr.String())
	}
	if got := promptCopiesInPane(t, session); got != 1 {
		t.Fatalf("%d prompt copies in pane, want 1 (prompt was retyped across retries)\n%s", got, stderr.String())
	}
}

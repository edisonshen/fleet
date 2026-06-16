package tmuxfake

import (
	"errors"
	"testing"

	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
	"github.com/edisonshen/fleet/internal/tmux"
)

// TestParity_FakeMatchesReal asserts the Fake's observable behavior
// matches the real tmux backend for the ops Fleet routes. Each scenario
// runs the SAME sequence against both backends and compares the result
// shape (success / error-kind / bool). The real backend runs under
// RequireTmux (skipped on tmux-less CI); the fake always runs.
//
// We compare observable RESULTS, not byte-identical output: CapturePane's
// real bytes are shell-prompt-dependent, so parity there is "both return
// no error for a live session, both return ErrNoSession for a dead one".
//
// ENFORCEMENT GAP (/review testing finding): this contract is the load-
// bearing guarantee that the fake stays faithful, but it can ONLY run on a
// tmux-equipped host — PR-2's whole point is a tmux-less CI lane, where this
// test skips. So the parity contract is enforced by developer-machine runs +
// any tmux-enabled CI lane, NOT by the default no-tmux lane. Keep a tmux lane
// in CI (or run the suite locally on a tmux box) before trusting a fake
// change; the fake's own fake_test.go covers fake-only behavior, but the
// cross-check against real tmux lives here.
func TestParity_FakeMatchesReal(t *testing.T) {
	// Real backend needs an isolated tmux server; skips if tmux absent.
	tmuxtest.RequireTmux(t)

	real := tmux.RealBackend()
	fake := NewFake()

	const sess = "fleet-parity"
	cmd := []string{"sh", "-c", "sleep 30"}

	// --- HasSession / SessionAlive on a missing session ---
	if real.HasSession(sess) || fake.HasSession(sess) {
		t.Fatal("HasSession on missing session: both must be false")
	}
	rAlive, rErr := real.SessionAlive(sess)
	fAlive, fErr := fake.SessionAlive(sess)
	if rAlive != fAlive || rAlive {
		t.Fatalf("SessionAlive missing: real=%v fake=%v, want both false", rAlive, fAlive)
	}
	if (rErr == nil) != (fErr == nil) {
		t.Fatalf("SessionAlive missing err mismatch: real=%v fake=%v", rErr, fErr)
	}

	// --- Spawn empty command: both reject ---
	if (real.Spawn(sess, "", nil, nil) == nil) != (fake.Spawn(sess, "", nil, nil) == nil) {
		t.Fatal("Spawn empty command: real/fake disagree on rejection")
	}

	// --- Spawn live session ---
	if err := fake.Spawn(sess, "", cmd, nil); err != nil {
		t.Fatalf("fake Spawn: %v", err)
	}
	if err := real.Spawn(sess, "", cmd, nil); err != nil {
		t.Fatalf("real Spawn: %v", err)
	}
	t.Cleanup(func() { _ = real.Kill(sess) })

	// --- HasSession / SessionAlive on a live session ---
	if !real.HasSession(sess) || !fake.HasSession(sess) {
		t.Fatal("HasSession live: both must be true")
	}
	rAlive, rErr = real.SessionAlive(sess)
	fAlive, fErr = fake.SessionAlive(sess)
	if !rAlive || !fAlive || rErr != nil || fErr != nil {
		t.Fatalf("SessionAlive live: real=(%v,%v) fake=(%v,%v), want (true,nil)", rAlive, rErr, fAlive, fErr)
	}

	// --- Duplicate Spawn: both reject ---
	if (real.Spawn(sess, "", cmd, nil) == nil) != (fake.Spawn(sess, "", cmd, nil) == nil) {
		t.Fatal("duplicate Spawn: real/fake disagree on rejection")
	}

	// --- SendKeys to a live session: both succeed ---
	if (real.SendKeys(sess, "echo hi", "Enter") == nil) != (fake.SendKeys(sess, "echo hi", "Enter") == nil) {
		t.Fatal("SendKeys live: real/fake disagree")
	}

	// --- CapturePane live: both succeed (no error) ---
	_, rcErr := real.CapturePane(sess)
	_, fcErr := fake.CapturePane(sess)
	if (rcErr == nil) != (fcErr == nil) {
		t.Fatalf("CapturePane live err mismatch: real=%v fake=%v", rcErr, fcErr)
	}

	// --- ListSessions: both include the session ---
	rList, rlErr := real.ListSessions()
	fList, flErr := fake.ListSessions()
	if rlErr != nil || flErr != nil {
		t.Fatalf("ListSessions err: real=%v fake=%v", rlErr, flErr)
	}
	if !contains(rList, sess) || !contains(fList, sess) {
		t.Fatalf("ListSessions missing %q: real=%v fake=%v", sess, rList, fList)
	}

	// --- ListSessionsWithCreated: both include the session w/ created ---
	rInfos, _ := real.ListSessionsWithCreated()
	fInfos, _ := fake.ListSessionsWithCreated()
	if !containsInfo(rInfos, sess) || !containsInfo(fInfos, sess) {
		t.Fatalf("ListSessionsWithCreated missing %q", sess)
	}

	// --- Kill live: both succeed and remove ---
	if err := real.Kill(sess); err != nil {
		t.Fatalf("real Kill: %v", err)
	}
	if err := fake.Kill(sess); err != nil {
		t.Fatalf("fake Kill: %v", err)
	}
	if real.HasSession(sess) || fake.HasSession(sess) {
		t.Fatal("session must be gone after Kill")
	}

	// --- Kill missing: both idempotent (nil) ---
	if real.Kill(sess) != nil || fake.Kill(sess) != nil {
		t.Fatal("Kill on missing session must be nil for both")
	}

	// --- SendKeys to a dead session: both ErrNoSession ---
	rsk := real.SendKeys(sess, "x")
	fsk := fake.SendKeys(sess, "x")
	if !errors.Is(rsk, tmux.ErrNoSession) || !errors.Is(fsk, tmux.ErrNoSession) {
		t.Fatalf("SendKeys dead: real=%v fake=%v, want ErrNoSession both", rsk, fsk)
	}

	// --- CapturePane dead: both ErrNoSession ---
	_, rcd := real.CapturePane(sess)
	_, fcd := fake.CapturePane(sess)
	if !errors.Is(rcd, tmux.ErrNoSession) || !errors.Is(fcd, tmux.ErrNoSession) {
		t.Fatalf("CapturePane dead: real=%v fake=%v, want ErrNoSession both", rcd, fcd)
	}

	// --- Available: both nil on a machine with a new-enough tmux ---
	if (real.Available() == nil) != (fake.Available() == nil) {
		t.Fatalf("Available: real=%v fake=%v", real.Available(), fake.Available())
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func containsInfo(infos []tmux.SessionInfo, s string) bool {
	for _, i := range infos {
		if i.Name == s {
			return true
		}
	}
	return false
}

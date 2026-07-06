package coordlock

// Fence verdicts carry an epoch snapshot alongside the branch tag
// (DESIGN-coord-lease-false-fence-prevention piece 2). 5a90 gave every fence
// return a stable branch tag; this task appends a compact one-line JSON
// snapshot of the fence-time epoch record so `fleet lease-check`'s exit-3
// stderr — and loop.py's forwarded fleetlog event — say WHICH branch fenced
// AND what the record looked like. The 53-minute mystery becomes a one-line
// read.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Test 6: an own-expired lease with a live takeover rival fences with the
// own-expired-rival-fenced tag AND a parseable epoch-snapshot JSON blob
// (epoch + state at minimum).
func TestFenceVerdict_CarriesEpochSnapshot(t *testing.T) {
	setupHome(t)
	const project = "snap-test"
	clk := &fakeClock{}
	live := newFakeLiveness()
	owner := identity{Pid: 500, PidStart: 9001, AgentID: "coord", Project: project}
	live.set(owner.Pid, owner.PidStart)
	live.set(800, 6000) // live takeover candidate — the rival
	cfg := leaseCheckCfg(clk, live, map[int]int{510: 500, 500: 1})

	writeEpochRaw(t, project, epochRecord{
		Epoch: 5, State: stateFencing, Owner: owner,
		Candidate:     identity{Pid: 800, PidStart: 6000, AgentID: "cand", Project: project},
		BootID:        "test-boot-1",
		RenewedAtMono: clk.now(),
		RenewedAtWall: 1234567890,
	})

	err := leaseCheckByAncestorWithCfg(project, 510, cfg)
	if !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatalf("own-ancestor live-takeover must fence, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, fenceTagOwnExpiredRival) {
		t.Fatalf("fence must keep the branch tag %q, got: %s", fenceTagOwnExpiredRival, msg)
	}
	// The snapshot is appended as: ... [epoch-snapshot {json}]
	const marker = "[epoch-snapshot "
	i := strings.Index(msg, marker)
	if i < 0 {
		t.Fatalf("fence must carry an [epoch-snapshot ...] blob, got: %s", msg)
	}
	rest := msg[i+len(marker):]
	j := strings.IndexByte(rest, ']')
	if j < 0 {
		t.Fatalf("epoch-snapshot blob is not closed with ']', got: %s", msg)
	}
	var snap map[string]any
	if uerr := json.Unmarshal([]byte(rest[:j]), &snap); uerr != nil {
		t.Fatalf("epoch-snapshot is not valid JSON (%v): %s", uerr, rest[:j])
	}
	if snap["epoch"] != float64(5) {
		t.Errorf("snapshot epoch = %v, want 5", snap["epoch"])
	}
	if snap["state"] != stateFencing {
		t.Errorf("snapshot state = %v, want %q", snap["state"], stateFencing)
	}
}

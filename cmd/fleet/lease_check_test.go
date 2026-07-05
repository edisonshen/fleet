package main

import (
	"bytes"
	"strings"
	"testing"
)

// runLeaseCheck's --project is required.
func TestLeaseCheck_ProjectRequired(t *testing.T) {
	var out, errb bytes.Buffer
	if err := runLeaseCheck("", 0, false, &out, &errb); err == nil {
		t.Fatal("missing --project must error")
	}
}

// With FLEET_LEASE_FAILOVER explicitly off, lease-check is a no-op success
// (reversibility): the skill behaves exactly as pre-lease.
func TestLeaseCheck_FailoverDisabledNoOp(t *testing.T) {
	t.Setenv("FLEET_LEASE_FAILOVER", "0")
	var out, errb bytes.Buffer
	if err := runLeaseCheck("rainier", 4242, false, &out, &errb); err != nil {
		t.Fatalf("disabled failover must be a no-op success, got %v", err)
	}
	if !strings.Contains(out.String(), "failover disabled") {
		t.Fatalf("expected the disabled-no-op message, got %q", out.String())
	}
}

// codex iter-4 [P2]: --reacquire with an explicit --pid is a usage error —
// renewal is only valid for the caller's own ancestry (default ppid flow).
// Rejected even before the failover shortcut (pure argument validation).
func TestLeaseCheck_ReacquireRejectsExplicitPid(t *testing.T) {
	t.Setenv("FLEET_LEASE_FAILOVER", "0") // even disabled: still a usage error
	var out, errb bytes.Buffer
	err := runLeaseCheck("rainier", 4242, true, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "--reacquire cannot be combined with --pid") {
		t.Fatalf("want the reacquire+pid rejection, got %v", err)
	}
}

// The command wires up and reports --project required through cobra too.
func TestLeaseCheckCmd_Wiring(t *testing.T) {
	cmd := newLeaseCheckCmd()
	cmd.SetArgs([]string{})
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	if err := cmd.Execute(); err == nil {
		t.Fatal("lease-check with no --project must fail")
	}
}

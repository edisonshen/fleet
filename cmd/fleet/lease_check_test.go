package main

import (
	"bytes"
	"strings"
	"testing"
)

// runLeaseCheck's --project is required.
func TestLeaseCheck_ProjectRequired(t *testing.T) {
	var out, errb bytes.Buffer
	if err := runLeaseCheck("", 0, &out, &errb); err == nil {
		t.Fatal("missing --project must error")
	}
}

// With FLEET_LEASE_FAILOVER explicitly off, lease-check is a no-op success
// (reversibility): the skill behaves exactly as pre-lease.
func TestLeaseCheck_FailoverDisabledNoOp(t *testing.T) {
	t.Setenv("FLEET_LEASE_FAILOVER", "0")
	var out, errb bytes.Buffer
	if err := runLeaseCheck("rainier", 4242, &out, &errb); err != nil {
		t.Fatalf("disabled failover must be a no-op success, got %v", err)
	}
	if !strings.Contains(out.String(), "failover disabled") {
		t.Fatalf("expected the disabled-no-op message, got %q", out.String())
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

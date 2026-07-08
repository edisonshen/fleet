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

// codex iter-4 [P2]: --reacquire with an explicit --pid is a usage error —
// renewal is only valid for the caller's own ancestry (default ppid flow).
// Rejected before any ownership proof (pure argument validation).
func TestLeaseCheck_ReacquireRejectsExplicitPid(t *testing.T) {
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

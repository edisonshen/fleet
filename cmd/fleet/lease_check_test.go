package main

import (
	"bytes"
	"testing"
)

// runLeaseCheck's --project is required.
func TestLeaseCheck_ProjectRequired(t *testing.T) {
	var out, errb bytes.Buffer
	if err := runLeaseCheck("", 0, &out, &errb); err == nil {
		t.Fatal("missing --project must error")
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

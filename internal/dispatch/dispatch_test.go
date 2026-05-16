package dispatch

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

// TestNewDispatchID_Valid pins the canonical 8-hex shape. This is the
// load-bearing invariant from DESIGN-dispatch-lifecycle.md §TL;DR:
// DispatchID == agent_id (8-hex, same shape as
// skills/coordinator/dispatch.py::mint_agent_id).
func TestNewDispatchID_Valid(t *testing.T) {
	cases := []string{
		"00000000",
		"abcdef01",
		"deadbeef",
		"a690424b", // an actual agent_id used in DESIGN examples
	}
	for _, c := range cases {
		id, err := NewDispatchID(c)
		if err != nil {
			t.Errorf("NewDispatchID(%q) returned err %v, want nil", c, err)
			continue
		}
		if id.String() != c {
			t.Errorf("NewDispatchID(%q).String() = %q, want %q", c, id.String(), c)
		}
	}
}

// TestNewDispatchID_Invalid pins all the reject paths the Python skill
// also rejects (uppercase, length, non-hex). A bad ID should never end
// up in a journal path or inbox filename.
func TestNewDispatchID_Invalid(t *testing.T) {
	bad := []string{
		"",                  // empty
		"abcdef0",           // 7 chars
		"abcdef012",         // 9 chars
		"ABCDEF01",          // uppercase
		"abcdefgh",          // non-hex letters
		"abcd ef0",          // space
		"abc-def0",          // hyphen
		"agent_a690424b",    // legacy "agent_" prefix
		"a690424b\n",        // trailing whitespace
		"  a690424b",        // leading whitespace
		"a 690424b",         // middle space
		"00000000\x00",      // embedded NUL
		strings.Repeat("a", 64),
	}
	for _, c := range bad {
		if _, err := NewDispatchID(c); err == nil {
			t.Errorf("NewDispatchID(%q) returned nil err; want validation failure", c)
		}
	}
}

// TestNewDispatchID_ByteEqualWithPython pins cross-language byte
// compatibility with skills/coordinator/dispatch.py:mint_agent_id —
// which is `secrets.token_hex(4)`, 4 random bytes hex-encoded.
//
// We can't import Python here, so we exercise the Go side with a fresh
// hex.EncodeToString(4 random bytes) and assert NewDispatchID accepts
// every output. The fact that Go's `agent.NewID` uses the same recipe
// (encoding/hex.EncodeToString of 4 random bytes) is the second half
// of the proof; their byte equality is by definition (both are
// lowercase hex of 4 random bytes).
//
// A property-test loop catches mismatches if either side drifts (e.g.
// someone changes Go to base32 — the regex match fails on the Python-
// shaped output).
func TestNewDispatchID_ByteEqualWithPython(t *testing.T) {
	for i := 0; i < 200; i++ {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
		// Python's secrets.token_hex(4) emits lowercase hex.
		pythonShape := hex.EncodeToString(b[:])
		id, err := NewDispatchID(pythonShape)
		if err != nil {
			t.Fatalf("python-shaped %q rejected: %v", pythonShape, err)
		}
		if id.String() != pythonShape {
			t.Fatalf("roundtrip mismatch: got %q want %q", id.String(), pythonShape)
		}
	}
}

// TestExecStateIsTerminal is a quick sanity for the IsTerminal helper.
// All terminal states must return true; in-flight must not.
func TestExecStateIsTerminal(t *testing.T) {
	for _, s := range []ExecState{ExecDone, ExecBlocked, ExecFailed} {
		if !s.IsTerminal() {
			t.Errorf("%q.IsTerminal() = false; want true", s)
		}
	}
	for _, s := range []ExecState{ExecPending, ExecInFlight, ExecState("nonsense")} {
		if s.IsTerminal() {
			t.Errorf("%q.IsTerminal() = true; want false", s)
		}
	}
}

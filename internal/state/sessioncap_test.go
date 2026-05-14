package state

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestResolveMaxSessions(t *testing.T) {
	// Pin: unset / empty falls through to the default with no warning.
	// Operators on a default install must not see warning noise.
	t.Run("unset returns default silently", func(t *testing.T) {
		var warn bytes.Buffer
		got := resolveMaxSessions("", &warn)
		if got != DefaultMaxSessions {
			t.Fatalf("unset: got %d, want %d", got, DefaultMaxSessions)
		}
		if warn.Len() != 0 {
			t.Fatalf("unset: unexpected warning %q", warn.String())
		}
	})

	t.Run("whitespace-only returns default silently", func(t *testing.T) {
		var warn bytes.Buffer
		got := resolveMaxSessions("   \t\n", &warn)
		if got != DefaultMaxSessions {
			t.Fatalf("ws: got %d, want %d", got, DefaultMaxSessions)
		}
		if warn.Len() != 0 {
			t.Fatalf("ws: unexpected warning %q", warn.String())
		}
	})

	t.Run("valid positive value used as-is", func(t *testing.T) {
		var warn bytes.Buffer
		got := resolveMaxSessions("50", &warn)
		if got != 50 {
			t.Fatalf("50: got %d, want 50", got)
		}
		if warn.Len() != 0 {
			t.Fatalf("50: unexpected warning %q", warn.String())
		}
	})

	t.Run("valid value with surrounding whitespace trimmed", func(t *testing.T) {
		var warn bytes.Buffer
		got := resolveMaxSessions("  100  ", &warn)
		if got != 100 {
			t.Fatalf("trimmed: got %d, want 100", got)
		}
		if warn.Len() != 0 {
			t.Fatalf("trimmed: unexpected warning %q", warn.String())
		}
	})

	t.Run("non-integer falls back with warning", func(t *testing.T) {
		var warn bytes.Buffer
		got := resolveMaxSessions("thirty", &warn)
		if got != DefaultMaxSessions {
			t.Fatalf("thirty: got %d, want %d", got, DefaultMaxSessions)
		}
		if !strings.Contains(warn.String(), "is not an integer") {
			t.Fatalf("thirty: warning %q missing parse hint", warn.String())
		}
		if !strings.Contains(warn.String(), FleetMaxSessionsEnv) {
			t.Fatalf("thirty: warning %q missing env-var name", warn.String())
		}
	})

	t.Run("zero falls back with warning", func(t *testing.T) {
		var warn bytes.Buffer
		got := resolveMaxSessions("0", &warn)
		if got != DefaultMaxSessions {
			t.Fatalf("0: got %d, want %d", got, DefaultMaxSessions)
		}
		if !strings.Contains(warn.String(), "is not positive") {
			t.Fatalf("0: warning %q missing non-positive hint", warn.String())
		}
	})

	t.Run("negative falls back with warning", func(t *testing.T) {
		var warn bytes.Buffer
		got := resolveMaxSessions("-5", &warn)
		if got != DefaultMaxSessions {
			t.Fatalf("-5: got %d, want %d", got, DefaultMaxSessions)
		}
		if !strings.Contains(warn.String(), "is not positive") {
			t.Fatalf("-5: warning %q missing non-positive hint", warn.String())
		}
	})

	t.Run("nil warn writer tolerated", func(t *testing.T) {
		// Spawn paths may pass nil when no stderr is available. The
		// fallback still needs to return DefaultMaxSessions without
		// panicking.
		got := resolveMaxSessions("bogus", nil)
		if got != DefaultMaxSessions {
			t.Fatalf("nil-warn: got %d, want %d", got, DefaultMaxSessions)
		}
	})
}

func TestMaxSessions_ReadsEnv(t *testing.T) {
	t.Setenv(FleetMaxSessionsEnv, "42")
	var warn bytes.Buffer
	if got := MaxSessions(&warn); got != 42 {
		t.Fatalf("env=42: got %d, want 42", got)
	}
	if warn.Len() != 0 {
		t.Fatalf("env=42: unexpected warning %q", warn.String())
	}
}

func TestMaxSessions_ReadsEnv_Default(t *testing.T) {
	t.Setenv(FleetMaxSessionsEnv, "")
	var warn bytes.Buffer
	if got := MaxSessions(&warn); got != DefaultMaxSessions {
		t.Fatalf("env-empty: got %d, want %d", got, DefaultMaxSessions)
	}
}

func TestCountFleetSessions(t *testing.T) {
	t.Run("buckets live and orphan; skips non-fleet", func(t *testing.T) {
		sessions := []string{
			"fleet-aaaa1111",
			"fleet-bbbb2222",
			"fleet-cccc3333",
			"work",      // not fleet-prefixed, skip
			"misc-tmux", // skip
			"fleet-",    // bare prefix, skip
		}
		live := map[string]bool{
			"aaaa1111": true,
			"cccc3333": true,
		}
		got, err := CountFleetSessions(
			func() ([]string, error) { return sessions, nil },
			func(id string) bool { return live[id] },
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := SessionCounts{Live: 2, Orphan: 1}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("counts: got %+v, want %+v", got, want)
		}
		if got.Total() != 3 {
			t.Fatalf("Total: got %d, want 3", got.Total())
		}
	})

	t.Run("empty list returns zero counts", func(t *testing.T) {
		got, err := CountFleetSessions(
			func() ([]string, error) { return nil, nil },
			func(string) bool { return false },
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != (SessionCounts{}) {
			t.Fatalf("empty: got %+v, want zero", got)
		}
	})

	t.Run("listFn error surfaces", func(t *testing.T) {
		boom := errors.New("tmux not on PATH")
		_, err := CountFleetSessions(
			func() ([]string, error) { return nil, boom },
			func(string) bool { return true },
		)
		if !errors.Is(err, boom) {
			t.Fatalf("err: got %v, want wrapping %v", err, boom)
		}
	})

	t.Run("all live", func(t *testing.T) {
		sessions := []string{"fleet-a", "fleet-b", "fleet-c"}
		got, err := CountFleetSessions(
			func() ([]string, error) { return sessions, nil },
			func(string) bool { return true },
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != (SessionCounts{Live: 3, Orphan: 0}) {
			t.Fatalf("all-live: got %+v", got)
		}
	})

	t.Run("all orphan", func(t *testing.T) {
		sessions := []string{"fleet-a", "fleet-b", "fleet-c"}
		got, err := CountFleetSessions(
			func() ([]string, error) { return sessions, nil },
			func(string) bool { return false },
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != (SessionCounts{Live: 0, Orphan: 3}) {
			t.Fatalf("all-orphan: got %+v", got)
		}
	})
}

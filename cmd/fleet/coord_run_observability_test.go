package main

// supervisor.exit fleetlog event (DESIGN-coord-lease-false-fence-prevention
// piece 2). The coord-run supervisor emits its own exit from a defer so the
// event fires on EVERY exit path — clean, child-error, panic — carrying the
// reason and the child's exit code. When a coord dies, the log says how.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/fleetlog"
)

// readFleetlogEvents returns every event of type evt found under the fleetlog
// dir for the current FLEET_HOME.
func readFleetlogEvents(t *testing.T, evt string) []map[string]any {
	t.Helper()
	var out []map[string]any
	matches, _ := filepath.Glob(filepath.Join(fleetlog.Dir(), "*.jsonl"))
	for _, f := range matches {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if ln == "" {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(ln), &m) != nil {
				continue
			}
			if m["type"] == evt {
				out = append(out, m)
			}
		}
	}
	return out
}

// Test 4: a child exiting nonzero logs supervisor.exit with the child's rc.
// A clean exit logs supervisor.exit rc=0. Lease machinery is disabled so the
// test isolates the supervisor lifecycle emit.
func TestCoordRun_SupervisorExitLogged(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantRC  float64
		wantErr bool
	}{
		{name: "child exits nonzero", argv: []string{"sh", "-c", "exit 7"}, wantRC: 7, wantErr: true},
		{name: "clean exit", argv: []string{"true"}, wantRC: 0, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLEET_LEASE_FAILOVER", "0") // no lease: isolate supervisor.exit
			t.Setenv("XDG_STATE_HOME", "")        // logs under FLEET_HOME
			markerPath := coordRunTestHome(t, "obs11111", "obs-exit", "fleet-obs11111")
			_ = markerPath

			opts := coordRunOpts{
				agentID:  "obs11111",
				project:  "obs-exit",
				session:  "fleet-obs11111",
				argv:     tc.argv,
				killTmux: func(string) error { return nil },
			}
			err := runCoordRun(context.Background(), opts, os.Stdout, os.Stderr)
			if tc.wantErr && err == nil {
				t.Fatalf("want a child-error return, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want clean return, got %v", err)
			}

			evts := readFleetlogEvents(t, "supervisor.exit")
			if len(evts) != 1 {
				t.Fatalf("supervisor.exit count = %d, want exactly 1", len(evts))
			}
			data, _ := evts[0]["data"].(map[string]any)
			if data == nil {
				t.Fatalf("supervisor.exit has no data field: %v", evts[0])
			}
			if data["rc"] != tc.wantRC {
				t.Errorf("supervisor.exit rc = %v, want %v", data["rc"], tc.wantRC)
			}
			if tc.wantErr && data["child_rc"] != tc.wantRC {
				t.Errorf("supervisor.exit child_rc = %v, want %v", data["child_rc"], tc.wantRC)
			}
			if _, ok := data["reason"]; !ok {
				t.Errorf("supervisor.exit must carry a reason, got %v", data)
			}
		})
	}
}

// A panic between Start and Wait must be logged as a CRASH, not a clean exit
// (codex P2). The named `err` return is nil during panic unwinding, so the
// defer must recover() to record reason=panic:... rc=2 — and re-panic so
// cleanup still runs and the operator sees the stack.
func TestCoordRun_SupervisorExitLogsPanic(t *testing.T) {
	t.Setenv("FLEET_LEASE_FAILOVER", "0") // no lease: isolate supervisor.exit
	t.Setenv("XDG_STATE_HOME", "")        // logs under FLEET_HOME
	_ = coordRunTestHome(t, "obs22222", "obs-panic", "fleet-obs22222")

	opts := coordRunOpts{
		agentID:         "obs22222",
		project:         "obs-panic",
		session:         "fleet-obs22222",
		argv:            []string{"true"},
		killTmux:        func(string) error { return nil },
		panicAfterStart: true,
	}

	// The panic must propagate (re-raised after logging), never be swallowed.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("panic must propagate out of runCoordRun, got none")
			}
		}()
		_ = runCoordRun(context.Background(), opts, os.Stdout, os.Stderr)
	}()

	evts := readFleetlogEvents(t, "supervisor.exit")
	if len(evts) != 1 {
		t.Fatalf("supervisor.exit count = %d, want exactly 1", len(evts))
	}
	data, _ := evts[0]["data"].(map[string]any)
	if data == nil {
		t.Fatalf("supervisor.exit has no data field: %v", evts[0])
	}
	if data["rc"] != float64(2) {
		t.Errorf("panic supervisor.exit rc = %v, want 2", data["rc"])
	}
	reason, _ := data["reason"].(string)
	if !strings.HasPrefix(reason, "panic:") {
		t.Errorf("panic supervisor.exit reason = %q, want a panic: prefix", reason)
	}
}

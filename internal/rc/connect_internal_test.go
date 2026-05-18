package rc

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

// TestConnect_RefusesOnCorruptState (codex round-7 P2): corrupt
// rc-state.json must fail closed with an actionable reset/up
// message, not fall through to selectTarget + sendFn.
func TestConnect_RefusesOnCorruptState(t *testing.T) {
	root := withFleetHome(t)

	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	// Write malformed JSON directly.
	if err := os.MkdirAll(root+"/projects/demo", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(root+"/projects/demo/rc-state.json", []byte("{not json}"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	res, err := Connect("demo", ConnectOpts{})
	if err == nil {
		t.Fatalf("Connect must refuse on corrupt rc-state.json; got success res=%+v", res)
	}
	if res.Outcome != OutcomeNotEnabled {
		t.Fatalf("outcome=%q want %q", res.Outcome, OutcomeNotEnabled)
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error must surface 'malformed' for operator clarity; got %q", err)
	}
}

// TestConnect_RefusesWhenListenerPIDDead (codex round-5 P1): if the
// recorded listener PID is dead, Connect must refuse with an
// operator-actionable error instead of silently driving
// /remote-control into the coord pane (where it would attach to no
// daemon and report success).
func TestConnect_RefusesWhenListenerPIDDead(t *testing.T) {
	withFleetHome(t)

	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	host, _ := os.Hostname()
	// Pick a PID that's almost certainly not alive. PID 999999 is
	// well above the default kernel pid_max on most systems and
	// IsAlive will return false. Using something like 1 (init) or
	// os.Getpid() would defeat the test.
	deadPID := 999999
	if err := WriteState(RecordedState{
		Project:       "demo",
		PID:           deadPID,
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	res, err := Connect("demo", ConnectOpts{})
	if err == nil {
		t.Fatalf("Connect must refuse dead-listener attach; got success res=%+v", res)
	}
	if res.Outcome != OutcomeNotEnabled {
		t.Fatalf("outcome=%q want %q (listener-dead is operator-actionable)", res.Outcome, OutcomeNotEnabled)
	}
	if !strings.Contains(err.Error(), "not alive") {
		t.Fatalf("error must surface 'not alive' for operator clarity; got %q", err)
	}
}

// TestSelectTarget_FallbackFiltersCoordOnly (codex round-3 P2): when
// the coord-spawn-marker is absent or stale, selectTarget's fallback
// must NEVER pick a worker pane. Project has live worker but no
// live coord → refuse with an operator-actionable message.
func TestSelectTarget_FallbackFiltersCoordOnly(t *testing.T) {
	withFleetHome(t)

	worker := &agent.Record{
		ID:          "worker-aaaa",
		TmuxSession: "fleet-worker",
		Project:     "demo",
		TaskID:      "auth-token-refresh", // not "coord-demo"
	}

	restore := SetConnectFnsForTest(
		func() ([]*agent.Record, error) { return []*agent.Record{worker}, nil },
		func(session string) bool { return session == "fleet-worker" },
		nil, nil,
	)
	defer restore()

	_, err := selectTarget("demo", "")
	if err == nil {
		t.Fatalf("selectTarget must refuse to target a worker when no coord is alive")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no live coord") {
		t.Fatalf("error message must surface 'no live coord' for operator clarity; got %q", msg)
	}
	if !strings.Contains(msg, "1 live non-coord") {
		t.Fatalf("error must call out the non-coord agents found; got %q", msg)
	}
}

// TestSelectTarget_FallbackAcceptsSingleCoord (companion test):
// when a single live coord record exists for the project, the
// fallback accepts it even without a coord-spawn-marker.
func TestSelectTarget_FallbackAcceptsSingleCoord(t *testing.T) {
	withFleetHome(t)

	coord := &agent.Record{
		ID:          "coord-bbbb",
		TmuxSession: "fleet-coord",
		Project:     "demo",
		TaskID:      "coord-demo",
	}

	restore := SetConnectFnsForTest(
		func() ([]*agent.Record, error) { return []*agent.Record{coord}, nil },
		func(session string) bool { return session == "fleet-coord" },
		nil, nil,
	)
	defer restore()

	got, err := selectTarget("demo", "")
	if err != nil {
		t.Fatalf("selectTarget should accept the single live coord; err=%v", err)
	}
	if got == nil || got.ID != "coord-bbbb" {
		t.Fatalf("selectTarget returned wrong record: %+v", got)
	}
}

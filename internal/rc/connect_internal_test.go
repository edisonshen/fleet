package rc

import (
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
)

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

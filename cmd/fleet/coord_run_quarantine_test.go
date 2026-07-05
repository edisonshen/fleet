package main

// coord_run_quarantine_test.go — detection plumbing for the KP6 pre-fence
// gate (DESIGN-coord-no-auto-kill, task coord-no-auto-kill-ac54, plan
// test 6): standbyPollUntilAcquired consumes the typed live-holder
// result, KEEPS POLLING (no exit, no attempt-exhaustion error), owns the
// cross-round dedup state, and emits one stderr report + one fleetlog
// coord.quarantine{stale-competitor} event per holder on state CHANGE
// only — no log storm across repeat rounds.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// readQuarantineEvents reads every fleetlog JSONL line of type
// coord.quarantine written under the test's FLEET_HOME. Shared with the
// KP3 sweep tests (coord_lease_unix_test.go).
func readQuarantineEvents(t *testing.T, home string) []map[string]any {
	t.Helper()
	dir := filepath.Join(home, "logs")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read logs dir: %v", err)
	}
	var out []map[string]any
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read log %s: %v", e.Name(), rerr)
		}
		for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if ln == "" {
				continue
			}
			var m map[string]any
			if jerr := json.Unmarshal([]byte(ln), &m); jerr != nil {
				t.Fatalf("bad JSONL line %q: %v", ln, jerr)
			}
			if m["type"] == "coord.quarantine" {
				out = append(out, m)
			}
		}
	}
	return out
}

// quarantineReportCount counts detection report lines in the poll loop's
// stderr output (one line per newly-reported live holder).
func quarantineReportCount(s string) int {
	return strings.Count(s, "live stale lease holder")
}

// Plan test 6: N rounds against the SAME live-stale holder -> exactly one
// report; a state change (different holder) -> exactly one more; the loop
// keeps polling throughout and returns the lease once acquired.
func TestStandbyPoll_QuarantineDedupAcrossRounds(t *testing.T) {
	home := setupFleetHome(t)
	t.Setenv("XDG_STATE_HOME", "") // fleetlog under FLEET_HOME/logs

	h1 := liveHolderInfo{pid: 4242, pidStart: 111, agentID: "old-leader"}
	h2 := liveHolderInfo{pid: 5555, pidStart: 222, agentID: "other-cand"}
	lease := &fakeLease{}

	var calls int32
	acquire := func() (coordLease, bool, []liveHolderInfo, error) {
		switch atomic.AddInt32(&calls, 1) {
		case 1, 2, 3:
			return nil, false, []liveHolderInfo{h1}, nil // same holder, 3 rounds
		case 4:
			return nil, false, []liveHolderInfo{h2}, nil // state change
		default:
			return lease, true, nil, nil
		}
	}

	stderr := &bytes.Buffer{}
	opts := coordRunOpts{
		agentID:     "sbq1",
		project:     "quar-proj",
		standbyPoll: time.Millisecond,
	}
	got, err := standbyPollUntilAcquired(context.Background(), acquire, opts, nil, stderr)
	if err != nil {
		t.Fatalf("standbyPollUntilAcquired: %v", err)
	}
	if got != lease {
		t.Fatalf("poll loop did not return the acquired lease")
	}

	out := stderr.String()
	if n := quarantineReportCount(out); n != 2 {
		t.Fatalf("want exactly 2 detection reports (first detection + state change), got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "old-leader") || !strings.Contains(out, "other-cand") {
		t.Fatalf("reports must name the detected holders; got:\n%s", out)
	}

	events := readQuarantineEvents(t, home)
	if len(events) != 2 {
		t.Fatalf("want 2 coord.quarantine fleetlog events, got %d: %v", len(events), events)
	}
	for _, ev := range events {
		if ev["proj"] != "quar-proj" {
			t.Errorf("event proj = %v, want quar-proj", ev["proj"])
		}
		data, _ := ev["data"].(map[string]any)
		if data == nil || data["reason"] != "stale-competitor" {
			t.Errorf("event data.reason = %v, want stale-competitor", data)
		}
	}
}

// Detection that CLEARS (leader recovered -> plain busy rounds) and then
// re-appears is a state change each way: report on re-detection, no
// duplicate for the clear-then-same sequence being deduped as one state.
func TestStandbyPoll_QuarantineReportsAgainAfterClear(t *testing.T) {
	home := setupFleetHome(t)
	t.Setenv("XDG_STATE_HOME", "")

	h1 := liveHolderInfo{pid: 4242, pidStart: 111, agentID: "old-leader"}
	lease := &fakeLease{}

	var calls int32
	acquire := func() (coordLease, bool, []liveHolderInfo, error) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			return nil, false, []liveHolderInfo{h1}, nil // detected
		case 2:
			return nil, false, nil, nil // cleared (healthy-leader busy round)
		case 3:
			return nil, false, []liveHolderInfo{h1}, nil // re-detected
		default:
			return lease, true, nil, nil
		}
	}

	stderr := &bytes.Buffer{}
	opts := coordRunOpts{
		agentID:     "sbq2",
		project:     "quar-proj2",
		standbyPoll: time.Millisecond,
	}
	if _, err := standbyPollUntilAcquired(context.Background(), acquire, opts, nil, stderr); err != nil {
		t.Fatalf("standbyPollUntilAcquired: %v", err)
	}
	out := stderr.String()
	if n := quarantineReportCount(out); n != 2 {
		t.Fatalf("want 2 detection reports (detect, clear, re-detect), got %d:\n%s", n, out)
	}
	if len(readQuarantineEvents(t, home)) != 2 {
		t.Fatalf("want 2 fleetlog events across detect/clear/re-detect")
	}
}

// The FIRST acquire happens in runCoordRun before the poll loop starts;
// its detection is passed in as the initial state so the first report is
// not delayed a poll round — and not duplicated when round 1 repeats it.
func TestStandbyPoll_InitialDetectionReportedOnce(t *testing.T) {
	home := setupFleetHome(t)
	t.Setenv("XDG_STATE_HOME", "")

	h1 := liveHolderInfo{pid: 4242, pidStart: 111, agentID: "old-leader"}
	lease := &fakeLease{}

	var calls int32
	acquire := func() (coordLease, bool, []liveHolderInfo, error) {
		switch atomic.AddInt32(&calls, 1) {
		case 1, 2:
			return nil, false, []liveHolderInfo{h1}, nil // same as initial
		default:
			return lease, true, nil, nil
		}
	}

	stderr := &bytes.Buffer{}
	opts := coordRunOpts{
		agentID:     "sbq3",
		project:     "quar-proj3",
		standbyPoll: time.Millisecond,
	}
	if _, err := standbyPollUntilAcquired(context.Background(), acquire, opts,
		[]liveHolderInfo{h1}, stderr); err != nil {
		t.Fatalf("standbyPollUntilAcquired: %v", err)
	}
	out := stderr.String()
	if n := quarantineReportCount(out); n != 1 {
		t.Fatalf("want exactly 1 report (initial detection, deduped across rounds), got %d:\n%s", n, out)
	}
	if len(readQuarantineEvents(t, home)) != 1 {
		t.Fatalf("want exactly 1 fleetlog event for the initial detection")
	}
}

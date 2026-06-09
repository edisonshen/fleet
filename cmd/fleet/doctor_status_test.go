//go:build linux || darwin

package main

// doctor_status_test.go — the stuck-handoff surface wired into `fleet status`
// (DESIGN-handoff-drain-storm-leak PR6). A coordinator handoff that was
// requested but never completed (a pending spawn-fresh queue file with no
// live leader) must be SURFACED with the canonical plain-English line +
// the `fleet doctor` next step — never silent (surface-don't-silo).

import (
	"bytes"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/queue"
)

func TestStatus_StuckHandoff_Surfaced(t *testing.T) {
	// Inject deps: one pending coord handoff for "stuckproj" with NO live
	// leader -> stuck. A second project "liveproj" has a pending handoff but a
	// LIVE leader -> in-flight, NOT surfaced.
	d := doctorTestDeps()
	d.ListPendingQueue = func() ([]string, error) {
		return []string{"/q/spawn-fresh-a.json", "/q/spawn-fresh-b.json"}, nil
	}
	d.ReadQueue = func(p string) (queue.SpawnFresh, error) {
		if strings.Contains(p, "-a.json") {
			return queue.SpawnFresh{OldAgentID: "a", Project: "stuckproj", TaskID: CoordTaskIDPrefix + "stuckproj"}, nil
		}
		return queue.SpawnFresh{OldAgentID: "b", Project: "liveproj", TaskID: CoordTaskIDPrefix + "liveproj"}, nil
	}
	d.LeaderPresent = func(project string) bool { return project == "liveproj" }

	orig := stuckHandoffStatusFn
	stuckHandoffStatusFn = func() doctorDeps { return d }
	t.Cleanup(func() { stuckHandoffStatusFn = orig })

	var out, errOut bytes.Buffer
	emitStuckHandoffSection(&out, &errOut)
	s := out.String()

	if !strings.Contains(s, "the handoff to a fresh coordinator didn't complete") {
		t.Errorf("missing canonical stuck-handoff line:\n%s", s)
	}
	if !strings.Contains(s, "fleet doctor") {
		t.Errorf("missing the `fleet doctor` next step:\n%s", s)
	}
	if !strings.Contains(s, "stuckproj") {
		t.Errorf("stuck project not named:\n%s", s)
	}
	if strings.Contains(s, "liveproj") {
		t.Errorf("in-flight handoff (live leader) was wrongly surfaced:\n%s", s)
	}
	// No jargon in the status surface.
	for _, w := range []string{"tmux", "flock", "epoch", "STONITH", "lease", "fence", "wedged"} {
		if strings.Contains(strings.ToLower(s), strings.ToLower(w)) {
			t.Errorf("status stuck-handoff line leaked jargon %q:\n%s", w, s)
		}
	}
}

func TestStatus_NoStuckHandoff_Silent(t *testing.T) {
	d := doctorTestDeps() // ListPendingQueue returns nil -> nothing pending
	orig := stuckHandoffStatusFn
	stuckHandoffStatusFn = func() doctorDeps { return d }
	t.Cleanup(func() { stuckHandoffStatusFn = orig })

	var out, errOut bytes.Buffer
	emitStuckHandoffSection(&out, &errOut)
	if out.Len() != 0 {
		t.Errorf("healthy state should be silent, got:\n%s", out.String())
	}
}

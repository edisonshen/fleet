package main

// rc_session_project_test.go pins the contract that the remote-control
// session names injected into spawned claude argvs include the agent's
// project name as a suffix. Without the project suffix, the operator
// running coords for multiple projects (spark, fleet, rainier, ...)
// sees identical "fleet-coord-<8hex>" entries on phone / claude.ai
// with no way to tell them apart. The fix: extend the name format to
// `fleet-coord-<id>-<project>` (and `fleet-handoff-<id>-<project>`)
// so each registered session is self-describing.
//
// We pick the SUFFIX-extension shape rather than replacing the prefix
// to minimize churn: the existing `fleet-coord-<id>` substring is still
// present, so pidresolver disambiguator matching (Go side) and
// fleet-guard health.py argv matching (Python side) keep working
// without code change. The daemon's --remote-control-session-name-prefix
// stays "fleet-coord" / "fleet-handoff" (broad), so any session name
// starting with those prefixes still attaches.

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// TestCoordSpawn_RemoteControlSessionName_IncludesProject asserts the
// dispatch-side coord-spawn path constructs a remote-control session
// name of the form `fleet-coord-<id>-<project>` (NOT just
// `fleet-coord-<id>`). This is the operator-visible name on
// claude.ai mobile / web; without the project suffix, multiple coords
// across projects all show as `fleet-coord-...` and are
// indistinguishable.
//
// We exercise the build-site format directly. The build site is in
// runDispatch's coord-spawn branch; we mirror its construction here.
func TestCoordSpawn_RemoteControlSessionName_IncludesProject(t *testing.T) {
	const agentID = "abcd1234"
	const project = "spark"

	got := buildCoordRemoteControlSessionName(agentID, project)

	// Must START with the daemon prefix (so the daemon's
	// --remote-control-session-name-prefix filter still accepts).
	if !strings.HasPrefix(got, remoteControlSessionPrefix+"-") {
		t.Errorf("session name %q must start with %q+'-' so the "+
			"daemon's session-name-prefix filter accepts it",
			got, remoteControlSessionPrefix)
	}
	// Must contain the agent ID (preserves pidresolver disambiguator
	// substring `fleet-coord-<id>`).
	if !strings.Contains(got, agentID) {
		t.Errorf("session name %q must contain agent id %q "+
			"(pidresolver disambiguator depends on it)",
			got, agentID)
	}
	// Must contain the project (the operator-visible
	// distinguishing-suffix).
	if !strings.Contains(got, project) {
		t.Errorf("session name %q must contain project %q "+
			"(the operator-visible distinguishing suffix; without "+
			"it phone / claude.ai shows identical names across projects)",
			got, project)
	}
	// Pin the exact shape: `fleet-coord-<id>-<project>`. The
	// SUFFIX position is load-bearing — the pidresolver
	// disambiguator does substring containment on
	// `fleet-coord-<id>`, so the project must come AFTER the id
	// (not between prefix and id).
	want := remoteControlSessionPrefix + "-" + agentID + "-" + project
	if got != want {
		t.Errorf("session name = %q; want %q (suffix-extension shape)",
			got, want)
	}
}

// TestHandoff_RemoteControlSessionName_IncludesProject pins the
// equivalent contract for the handoff-replacement spawn path: the
// successor coord/agent's --remote-control session name must include
// the inherited project so the operator can distinguish handoff
// successors per project on phone / claude.ai.
//
// Order: project comes BEFORE id so the session name STARTS with
// the per-project daemon prefix `fleet-handoff-<project>` that
// internal/handoff.FirstAction renders into the bash bootstrap.
// The Claude remote-control daemon only attaches sessions whose name
// starts with its --remote-control-session-name-prefix value, so the
// project-first order is the contract that keeps mobile pairing alive
// across handoffs (codex review iter-2 [P1] regression bracket).
func TestHandoff_RemoteControlSessionName_IncludesProject(t *testing.T) {
	const newID = "deadbeef"
	const project = "rainier"

	got := buildHandoffRemoteControlSessionName(newID, project)

	if !strings.HasPrefix(got, handoffSessionPrefix+"-") {
		t.Errorf("handoff session name %q must start with %q+'-'",
			got, handoffSessionPrefix)
	}
	if !strings.Contains(got, newID) {
		t.Errorf("handoff session name %q must contain new id %q",
			got, newID)
	}
	if !strings.Contains(got, project) {
		t.Errorf("handoff session name %q must contain project %q",
			got, project)
	}
	want := handoffSessionPrefix + "-" + project + "-" + newID
	if got != want {
		t.Errorf("handoff session name = %q; want %q "+
			"(project-first order: name must START WITH the "+
			"per-project daemon prefix `fleet-handoff-<project>` "+
			"so the daemon attaches the session)",
			got, want)
	}
}

// TestHandoff_RemoteControlSessionName_StartsWithDaemonPrefix is the
// load-bearing contract codex iter-2 [P1] surfaced. The Claude
// remote-control daemon launched by internal/handoff.FirstAction(p)
// runs with `--remote-control-session-name-prefix "fleet-handoff-<p>"`,
// and its prefix filter only attaches sessions whose name starts
// with that value. If the session-name format ever drifts away from
// having `fleet-handoff-<project>` as its literal prefix, every
// per-project handoff silently breaks /remote-control attach.
func TestHandoff_RemoteControlSessionName_StartsWithDaemonPrefix(t *testing.T) {
	const newID = "abcd1234"
	for _, project := range []string{"spark", "rainier", "v2.1", "fleet"} {
		got := buildHandoffRemoteControlSessionName(newID, project)
		daemonPrefix := handoffSessionPrefix + "-" + project
		if !strings.HasPrefix(got, daemonPrefix) {
			t.Errorf("handoff session name %q must start with daemon "+
				"prefix %q (the FirstAction bash block launches the "+
				"daemon with `--remote-control-session-name-prefix "+
				"\"%s\"` and the daemon only attaches sessions "+
				"whose name STARTS WITH that prefix)",
				got, daemonPrefix, daemonPrefix)
		}
	}
}

// TestCoordSpawn_RemoteControlInjection_IncludesProjectInArgv pins
// end-to-end wiring: when the dispatch coord-spawn branch rewrites
// the default --command, the resulting argv body must contain
// `--remote-control "fleet-coord-<id>-<project>"`. Pre-fix this test
// would see only `fleet-coord-<id>` (no project suffix).
func TestCoordSpawn_RemoteControlInjection_IncludesProjectInArgv(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	slice := flag.Value.(pflag.SliceValue)
	defaultCmd := slice.GetSlice()

	const agentID = "1a2b3c4d"
	const project = "tatoosh"
	rcSessionName := buildCoordRemoteControlSessionName(agentID, project)
	rewritten := injectRemoteControlFlag(defaultCmd, rcSessionName)

	want := `--remote-control "fleet-coord-1a2b3c4d-tatoosh"`
	if !strings.Contains(rewritten[2], want) {
		t.Errorf("rewritten coord-spawn command should embed %q "+
			"(operator-visible distinguishing suffix); got %q",
			want, rewritten[2])
	}
}

// TestHandoff_RemoteControlInjection_IncludesProjectInArgv mirrors
// the above for the handoff replacement-spawn path. Order is
// project-first so the registered session name starts with the
// per-project daemon prefix (see
// TestHandoff_RemoteControlSessionName_StartsWithDaemonPrefix for
// the full rationale).
func TestHandoff_RemoteControlInjection_IncludesProjectInArgv(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	slice := flag.Value.(pflag.SliceValue)
	defaultCmd := slice.GetSlice()

	const newID = "feedface"
	const project = "fleet"
	rcSessionName := buildHandoffRemoteControlSessionName(newID, project)
	rewritten := injectRemoteControlFlag(defaultCmd, rcSessionName)

	want := `--remote-control "fleet-handoff-fleet-feedface"`
	if !strings.Contains(rewritten[2], want) {
		t.Errorf("rewritten handoff command should embed %q; got %q",
			want, rewritten[2])
	}
}

// TestCoordSpawn_RemoteControlSessionName_LegacyShapeStillRecognized
// is the upgrade-time graceful-degradation pin. Coords spawned BEFORE
// this fix have argv `--remote-control "fleet-coord-<id>"` (no project
// suffix). The pidresolver's disambiguator still matches because it
// does substring-containment on `fleet-coord-<id>` — the new shape
// `fleet-coord-<id>-<project>` is a strict superstring of the old.
//
// This test pins that the disambiguator builder yields a needle that
// matches BOTH the legacy and new shapes when scanning argv. Without
// this invariant, post-upgrade any in-flight legacy coord would lose
// its pid-resolver disambiguation and could latch onto a sibling
// claude.
func TestCoordSpawn_RemoteControlSessionName_LegacyShapeStillRecognized(t *testing.T) {
	const agentID = "cafef00d"

	// Legacy shape (pre-this-fix).
	legacyName := remoteControlSessionPrefix + "-" + agentID
	// New shape (post-this-fix).
	newName := buildCoordRemoteControlSessionName(agentID, "spark")

	// The pidresolver builds needle = "fleet-coord-" + agentID and
	// scans for substring containment. Both shapes must contain that
	// needle as a substring.
	needle := "fleet-coord-" + agentID
	if !strings.Contains(legacyName, needle) {
		t.Errorf("legacy shape %q lost the pidresolver needle %q "+
			"(graceful-degradation broken)", legacyName, needle)
	}
	if !strings.Contains(newName, needle) {
		t.Errorf("new shape %q lost the pidresolver needle %q "+
			"(disambiguator format incompatible — pidresolver.go's "+
			"pidResolveDisambiguator builds the needle from agentID alone)",
			newName, needle)
	}
}

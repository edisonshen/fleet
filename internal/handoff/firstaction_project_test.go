package handoff

// firstaction_project_test.go pins the contract that the handoff doc's
// FirstAction bash block carries the project name into the
// remote-control daemon prefix the resuming agent's bootstrap launches.
//
// Pre-this-fix the printed bash block hardcoded
// `--remote-control-session-name-prefix "fleet-handoff"` for every
// project, so the operator running multiple per-project handoff daemons
// saw identical entries. Post-fix the prefix becomes
// `fleet-handoff-<project>`, mirroring the per-session naming format
// `fleet-handoff-<id>-<project>` the spawned successor agent
// registers under via its --remote-control flag.
//
// The narrowed pgrep guard in the same bash block must also be
// project-scoped so per-project daemons coexist (otherwise a
// `pgrep -f "claude remote-control"` broad match would skip launching
// project B's daemon when project A's was already up).

import (
	"strings"
	"testing"
	"time"
)

// TestFirstAction_ProjectScopedDaemonPrefix asserts the daemon prefix
// flag value in the printed bash block carries the project name.
func TestFirstAction_ProjectScopedDaemonPrefix(t *testing.T) {
	const project = "spark"
	got := FirstAction(project)

	wantQuoted := `"fleet-handoff-` + project + `"`
	if !strings.Contains(got, wantQuoted) {
		t.Errorf("FirstAction(%q) should reference %q as the "+
			"--remote-control-session-name-prefix value (so the launched "+
			"daemon is project-scoped and the operator sees per-project "+
			"sessions on phone / claude.ai); got body:\n%s",
			project, wantQuoted, got)
	}
	// The legacy generic prefix (without project) must not appear as
	// a quoted standalone value — that would indicate the project
	// suffix wasn't appended.
	legacy := `"fleet-handoff"`
	if strings.Contains(got, legacy) {
		t.Errorf("FirstAction(%q) must NOT reference the legacy generic "+
			"prefix %q (drift means the project suffix wasn't applied); "+
			"got body:\n%s", project, legacy, got)
	}
}

// TestFirstAction_PgrepNarrowedToProject pins the second half of the
// per-project daemon: the pgrep guard at the start of the bash block
// must match ONLY this project's daemon, not any handoff daemon. With
// the broad guard, project B's daemon would be skipped when project
// A's was already up.
func TestFirstAction_PgrepNarrowedToProject(t *testing.T) {
	const project = "rainier"
	got := FirstAction(project)

	wantNeedle := "fleet-handoff-" + project
	// The pgrep -f pattern must reference the project-scoped prefix.
	// We don't pin the exact regex shape (anchors / word boundaries
	// are an implementation detail), only that the project literal
	// is in the pgrep argument.
	pgrepIdx := strings.Index(got, "pgrep -f")
	if pgrepIdx < 0 {
		t.Fatalf("FirstAction body lost the pgrep guard:\n%s", got)
	}
	// Look at the line containing pgrep — must include the project literal.
	line := got[pgrepIdx:]
	if nl := strings.Index(line, "\n"); nl > 0 {
		line = line[:nl]
	}
	if !strings.Contains(line, wantNeedle) {
		t.Errorf("pgrep guard line must contain %q so per-project "+
			"daemons don't mask each other's launch; got line:\n%s",
			wantNeedle, line)
	}
}

// TestFirstAction_DistinctPerProject asserts two different projects
// produce distinct FirstAction bodies — the regression bracket for
// "I refactored FirstAction to ignore the project arg".
func TestFirstAction_DistinctPerProject(t *testing.T) {
	a := FirstAction("spark")
	b := FirstAction("rainier")
	if a == b {
		t.Errorf("FirstAction must produce different output for different "+
			"projects (project-scoped daemon prefix); both = %q", a)
	}
}

// TestRender_FirstActionUsesDocProject pins the wiring: Render(d)
// must build its First Action body from d.Project, not from a
// hardcoded literal. Without this, the doc on disk would advertise
// the wrong daemon prefix for the project's handoff successor.
func TestRender_FirstActionUsesDocProject(t *testing.T) {
	const project = "tatoosh"
	d := NewManualStub("a1b2c3d4", "auth-fix", project, 1, nil, time.Now().UTC())
	got := string(Render(d))

	wantQuoted := `"fleet-handoff-` + project + `"`
	if !strings.Contains(got, wantQuoted) {
		t.Errorf("Render(d) should embed %q from d.Project=%q "+
			"into the First Action bash block; got:\n%s",
			wantQuoted, project, got)
	}
}

// TestFirstAction_PgrepEscapesProjectDot pins the regex-escape contract
// for project names containing `.`. ValidateProjectName allows `.` (e.g.
// `v2.1`); without escaping the bash block's pgrep pattern would be
// `^...fleet-handoff-v2.1( |$)` and `.` matches any char — a daemon
// process for a different project named `v2a1` would mask the launch
// of the v2.1 daemon, leaving /remote-control with no compatible
// daemon to attach to. Escaping `.` to `\\.` keeps the match strictly
// literal so daemons for project `v2.1` and `v2a1` coexist correctly.
//
// Codex review iter-1 [P2] regression bracket.
func TestFirstAction_PgrepEscapesProjectDot(t *testing.T) {
	const project = "v2.1"
	body := FirstAction(project)

	// The pgrep -f single-quoted regex must contain the LITERAL
	// `\.` escape (two chars: backslash then dot). The daemon prefix
	// flag value in `--remote-control-session-name-prefix "..."`
	// stays the unescaped literal because that's a shell-quoted
	// argument, not a regex.
	const wantEscaped = "fleet-handoff-v2\\.1( |$)"
	if !strings.Contains(body, wantEscaped) {
		t.Errorf("FirstAction(%q) pgrep guard must contain %q (escaped "+
			"`.` so a daemon for `v2a1` doesn't false-positive); got:\n%s",
			project, wantEscaped, body)
	}
	// And the daemon-prefix flag value (the non-regex arg) keeps the
	// literal `.` so the spawned daemon registers under the correct
	// project name. Drift here would mean the launched daemon's argv
	// no longer matches what /remote-control attaches to.
	const wantLiteralFlag = `--remote-control-session-name-prefix "fleet-handoff-v2.1"`
	if !strings.Contains(body, wantLiteralFlag) {
		t.Errorf("FirstAction(%q) daemon-prefix flag must contain %q "+
			"(literal `.` because the flag value is shell-quoted, not regex); "+
			"got:\n%s", project, wantLiteralFlag, body)
	}
}

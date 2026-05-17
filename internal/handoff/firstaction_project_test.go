package handoff

// firstaction_project_test.go pins the v0.12 FirstAction contract:
// the body is operator-instruction markdown (not bash bootstrap)
// that directs the resuming operator to run `fleet rc connect
// <project>` to re-attach mobile/web pairing. v0.12 retired the
// embedded bash bootstrap per DESIGN-rc-listener-lifecycle.md
// §"Handoff doc rewrite" — the new body must NOT contain any
// `claude remote-control` bash exec.

import (
	"strings"
	"testing"
	"time"
)

// TestFirstAction_OperatorInstructionShape asserts the new body's
// load-bearing surface: it tells the operator to run
// `fleet rc connect <project>` and references `fleet rc up <project>`
// as the opt-in command.
func TestFirstAction_OperatorInstructionShape(t *testing.T) {
	const project = "spark"
	got := FirstAction(project)

	for _, want := range []string{
		"fleet rc connect " + project,
		"fleet rc up " + project,
		"/remote-control",
		"/coordinator",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FirstAction(%q) body missing %q; got:\n%s",
				project, want, got)
		}
	}
}

// TestFirstAction_NoEmbeddedBashBootstrap asserts the bash block is
// gone. This is the regression bracket for v0.12's architectural
// fix: the test-pollution incident that pushed 5,620 mobile
// notifications stemmed from a handoff doc embedding bash that
// got exec'd inside tests.
func TestFirstAction_NoEmbeddedBashBootstrap(t *testing.T) {
	const project = "rainier"
	got := FirstAction(project)
	for _, forbidden := range []string{
		"nohup claude remote-control",
		"pgrep -f",
		"```bash",
		"--remote-control-session-name-prefix",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("FirstAction(%q) MUST NOT contain %q (v0.12 retired the bash bootstrap; operator-instruction text only); got:\n%s",
				project, forbidden, got)
		}
	}
}

// TestFirstAction_DistinctPerProject asserts two different projects
// produce distinct FirstAction bodies — the project name is the
// load-bearing substitution.
func TestFirstAction_DistinctPerProject(t *testing.T) {
	a := FirstAction("spark")
	b := FirstAction("rainier")
	if a == b {
		t.Errorf("FirstAction must produce different output for different "+
			"projects (project-name substitution); both = %q", a)
	}
}

// TestFirstAction_EmptyProjectFallback asserts the empty-project
// fallback uses a `<project>` placeholder text so the body stays
// well-formed for legacy records.
func TestFirstAction_EmptyProjectFallback(t *testing.T) {
	got := FirstAction("")
	if !strings.Contains(got, "fleet rc connect <project>") {
		t.Errorf("FirstAction(\"\") should emit `<project>` placeholder; got:\n%s", got)
	}
}

// TestRender_FirstActionUsesDocProject pins the wiring: Render(d)
// must build its First Action body from d.Project, not from a
// hardcoded literal.
func TestRender_FirstActionUsesDocProject(t *testing.T) {
	const project = "tatoosh"
	d := NewManualStub("a1b2c3d4", "auth-fix", project, 1, nil, time.Now().UTC())
	got := string(Render(d))

	want := "fleet rc connect " + project
	if !strings.Contains(got, want) {
		t.Errorf("Render(d) should embed %q from d.Project=%q; got:\n%s",
			want, project, got)
	}
}

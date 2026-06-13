package handoff

// firstaction_project_test.go pins the native-RC FirstAction contract
// (rc-default-native-startup): the body is a status note — pairing is
// native at coord spawn — plus the opt-out escape hatch (`fleet rc up
// <project>`), with NO bash bootstrap and NO retired `fleet rc
// connect` instruction. The /coordinator resume paragraph stays.

import (
	"strings"
	"testing"
	"time"
)

// TestFirstAction_OperatorInstructionShape asserts the native body's
// load-bearing surface: pairing is native (--remote-control mention),
// `fleet rc status <project>` is the diagnostic, `fleet rc up
// <project>` is the re-enable path, and /coordinator resumes the
// supervisor loop. The retired `fleet rc connect` must NOT appear.
func TestFirstAction_OperatorInstructionShape(t *testing.T) {
	const project = "spark"
	got := FirstAction(project)

	for _, want := range []string{
		"--remote-control",
		"fleet rc status " + project,
		"fleet rc up " + project,
		"/coordinator",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FirstAction(%q) body missing %q; got:\n%s",
				project, want, got)
		}
	}
	if strings.Contains(got, "fleet rc connect") {
		t.Errorf("FirstAction(%q) MUST NOT reference the retired `fleet rc connect`; got:\n%s",
			project, got)
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
		"fleet rc connect", // retired send-keys attach path
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
	if !strings.Contains(got, "fleet rc status <project>") {
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

	want := "fleet rc status " + project
	if !strings.Contains(got, want) {
		t.Errorf("Render(d) should embed %q from d.Project=%q; got:\n%s",
			want, project, got)
	}
}

package spawn

import (
	"os"
	"strings"
	"testing"
)

func withCoordRCInjector(t *testing.T, fn func(project, agentID string, argv []string) []string) {
	t.Helper()
	prev := CoordRCInjector
	CoordRCInjector = fn
	t.Cleanup(func() { CoordRCInjector = prev })
}

func testCoordRCInjector(project, agentID string, argv []string) []string {
	if os.Getenv("FLEET_RC_BOOTSTRAP_DISABLED") != "" || project == "disabled" {
		return argv
	}
	return InjectRemoteControlFlag(argv, CoordRemoteControlSessionName(agentID, project))
}

func TestCoordRCArgv_CoordDefaultInjects(t *testing.T) {
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
	withCoordRCInjector(t, testCoordRCInjector)

	in := []string{"sh", "-c", "claude --dangerously-skip-permissions"}
	got := coordRCArgv(in, "rainier", "abcd1234", true)

	want := `--remote-control "fleet-coord-abcd1234-rainier"`
	if !strings.Contains(strings.Join(got, " "), want) {
		t.Fatalf("coord argv missing %q: %v", want, got)
	}
	if strings.Contains(strings.Join(in, " "), "--remote-control") {
		t.Fatalf("coordRCArgv mutated input: %v", in)
	}
}

func TestCoordRCArgv_WorkerNeverInjects(t *testing.T) {
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
	withCoordRCInjector(t, func(project, agentID string, argv []string) []string {
		return InjectRemoteControlFlag(argv, CoordRemoteControlSessionName(agentID, project))
	})

	in := []string{"sh", "-c", "claude --dangerously-skip-permissions"}
	got := coordRCArgv(in, "rainier", "worker1", false)
	if !SameCommand(got, in) {
		t.Fatalf("worker argv must stay unchanged; got %v want %v", got, in)
	}
}

func TestCoordRCArgv_GatesAndCustomCommands(t *testing.T) {
	withCoordRCInjector(t, testCoordRCInjector)

	cases := []struct {
		name    string
		project string
		env     string
		argv    []string
		wantRC  bool
	}{
		{
			name:    "enabled default claude wrapper",
			project: "rainier",
			argv:    []string{"sh", "-c", "claude --print"},
			wantRC:  true,
		},
		{
			name:    "project opt-out",
			project: "disabled",
			argv:    []string{"sh", "-c", "claude --print"},
		},
		{
			name:    "env gate",
			project: "rainier",
			env:     "1",
			argv:    []string{"sh", "-c", "claude --print"},
		},
		{
			name:    "custom command",
			project: "rainier",
			argv:    []string{"sleep", "60"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", tc.env)
			got := coordRCArgv(tc.argv, tc.project, "abcd1234", true)
			hasRC := strings.Contains(strings.Join(got, " "), "--remote-control")
			if hasRC != tc.wantRC {
				t.Fatalf("remote-control presence = %v, want %v; argv=%v", hasRC, tc.wantRC, got)
			}
			if !tc.wantRC && !SameCommand(got, tc.argv) {
				t.Fatalf("suppressed/custom argv mutated: got %v want %v", got, tc.argv)
			}
		})
	}
}

func TestCoordRCArgv_NilSeamNoops(t *testing.T) {
	prev := CoordRCInjector
	CoordRCInjector = nil
	t.Cleanup(func() { CoordRCInjector = prev })

	in := []string{"sh", "-c", "claude --print"}
	got := coordRCArgv(in, "rainier", "abcd1234", true)
	if !SameCommand(got, in) {
		t.Fatalf("nil CoordRCInjector must no-op; got %v want %v", got, in)
	}
}

func TestCoordRCArgv_AlreadyCoordRunWrappedInjectsInnerOnce(t *testing.T) {
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
	withCoordRCInjector(t, testCoordRCInjector)

	in := []string{
		"/usr/local/bin/fleet", "coord-run", "--agent", "abcd1234",
		"--project", "rainier", "--", "sh", "-c", "claude --print",
	}
	got := coordRCArgv(in, "rainier", "abcd1234", true)

	if !alreadyCoordRunWrapped(got) {
		t.Fatalf("pre-wrapped argv lost coord-run head: %v", got)
	}
	joined := strings.Join(got, " ")
	if count := strings.Count(joined, "--remote-control"); count != 1 {
		t.Fatalf("pre-wrapped argv should have exactly one remote-control flag, got %d: %v", count, got)
	}
	if !strings.Contains(joined, `--remote-control "fleet-coord-abcd1234-rainier"`) {
		t.Fatalf("pre-wrapped argv missing coord session name: %v", got)
	}
}

func TestCoordRCArgv_AlreadyInjectedDoesNotDoubleInject(t *testing.T) {
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
	withCoordRCInjector(t, testCoordRCInjector)

	in := []string{"sh", "-c", `claude --remote-control "fleet-coord-old-rainier" --print`}
	got := coordRCArgv(in, "rainier", "abcd1234", true)
	if !SameCommand(got, in) {
		t.Fatalf("already injected argv must stay unchanged; got %v want %v", got, in)
	}
}

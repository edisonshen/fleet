package spawn_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/enginecfg"
	"github.com/edisonshen/fleet/internal/projects"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/testutil/tmuxfake"
)

// Coord handoff / recovery must run the engine the operator currently
// wants — the persisted `fleet -codex` / `fleet -claude` choice — not
// blindly the predecessor's. These tests drive spawn.Spawn against the
// in-process fake tmux backend so the stock engine wrappers are recorded
// rather than executed (neither claude nor codex needs to be installed).

func setupHandoffHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	t.Setenv("FLEET_ENGINE", "")
	t.Setenv("FLEET_ENGINE_EXPLICIT", "")
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return home
}

func oldCoord(t *testing.T, engine string) *agent.Record {
	t.Helper()
	old := agent.New("aaaa7777")
	old.TaskID = "coord-projects-rainier"
	old.Project = "projects-rainier"
	old.Engine = engine
	old.Cwd = t.TempDir()
	old.HandoffNumber = 1
	return old
}

func wrapper(t *testing.T, engine string) []string {
	t.Helper()
	argv, err := enginecfg.BuildWrapperCommand(engine)
	if err != nil {
		t.Fatalf("BuildWrapperCommand(%s): %v", engine, err)
	}
	return argv
}

func handoffCoord(t *testing.T, old *agent.Record, command []string) *agent.Record {
	t.Helper()
	rec, err := spawn.Spawn(spawn.Options{
		OldRecord:  old,
		NewDocPath: "/some/handoffs/aaaa7777-20260917-180000.md",
		Cwd:        old.Cwd,
		Command:    command,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return rec
}

func TestSpawn_CoordHandoffPersistedChoiceSwitchesEngine(t *testing.T) {
	for _, tc := range []struct{ from, to string }{
		{enginecfg.EngineClaudeCode, enginecfg.EngineCodex},
		{enginecfg.EngineCodex, enginecfg.EngineClaudeCode},
	} {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			home := setupHandoffHome(t)
			if err := projects.WriteGlobalCoordConfigEngine(home, tc.to); err != nil {
				t.Fatal(err)
			}
			f := tmuxfake.InstallFake(t)
			old := oldCoord(t, tc.from)
			rec := handoffCoord(t, old, wrapper(t, tc.from))

			if rec.Engine != tc.to {
				t.Errorf("successor rec.Engine = %q want %q", rec.Engine, tc.to)
			}
			if want := wrapper(t, tc.to); !reflect.DeepEqual(rec.Command, want) {
				t.Errorf("successor rec.Command = %q want %s wrapper", rec.Command, tc.to)
			}
			if got := f.SessionCommand(rec.TmuxSession); !reflect.DeepEqual(got, wrapper(t, tc.to)) {
				t.Errorf("tmux ran %q want %s wrapper", got, tc.to)
			}
			if env := f.SessionEnv(rec.TmuxSession); !slices.Contains(env, "FLEET_ENGINE="+tc.to) {
				t.Errorf("successor env %q lacks FLEET_ENGINE=%s", env, tc.to)
			}
			if got := spawn.ReadCoordConfigEngine(home, "projects-rainier"); got != tc.to {
				t.Errorf("project coord-config engine stamp = %q want %q", got, tc.to)
			}
			// The predecessor's record is untouched: changing the choice
			// never rewrites a running coord.
			if old.Engine != tc.from {
				t.Errorf("old record engine mutated to %q", old.Engine)
			}
		})
	}
}

func TestSpawn_CoordHandoffNoPersistedChoiceInheritsEngine(t *testing.T) {
	setupHandoffHome(t)
	f := tmuxfake.InstallFake(t)
	old := oldCoord(t, enginecfg.EngineCodex)
	rec := handoffCoord(t, old, wrapper(t, enginecfg.EngineCodex))

	if rec.Engine != enginecfg.EngineCodex {
		t.Errorf("rec.Engine = %q want codex (inherited)", rec.Engine)
	}
	if got := f.SessionCommand(rec.TmuxSession); !reflect.DeepEqual(got, wrapper(t, enginecfg.EngineCodex)) {
		t.Errorf("tmux ran %q want codex wrapper", got)
	}
}

func TestSpawn_CoordHandoffProjectStampUsedWhenNoGlobalChoice(t *testing.T) {
	home := setupHandoffHome(t)
	projDir := filepath.Join(home, "projects", "projects-rainier")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "coord-config.json"),
		[]byte(`{"engine": "codex"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Legacy predecessor (no engine field) under a project stamped codex.
	tmuxfake.InstallFake(t)
	old := oldCoord(t, "")
	rec := handoffCoord(t, old, wrapper(t, enginecfg.EngineClaudeCode))
	if rec.Engine != enginecfg.EngineCodex {
		t.Errorf("rec.Engine = %q want codex (project stamp)", rec.Engine)
	}
	if !reflect.DeepEqual(rec.Command, wrapper(t, enginecfg.EngineCodex)) {
		t.Errorf("rec.Command = %q want codex wrapper", rec.Command)
	}
}

func TestSpawn_CoordHandoffCustomCommandKeepsPredecessorEngine(t *testing.T) {
	home := setupHandoffHome(t)
	if err := projects.WriteGlobalCoordConfigEngine(home, enginecfg.EngineCodex); err != nil {
		t.Fatal(err)
	}
	custom := []string{"sh", "-c", "my-claude-wrapper --flag"}
	f := tmuxfake.InstallFake(t)
	old := oldCoord(t, enginecfg.EngineClaudeCode)
	rec := handoffCoord(t, old, custom)

	if rec.Engine != enginecfg.EngineClaudeCode {
		t.Errorf("rec.Engine = %q want claude-code (custom command can't be re-targeted)", rec.Engine)
	}
	if !reflect.DeepEqual(rec.Command, custom) {
		t.Errorf("rec.Command = %q want custom command preserved", rec.Command)
	}
	if got := f.SessionCommand(rec.TmuxSession); !reflect.DeepEqual(got, custom) {
		t.Errorf("tmux ran %q want custom command", got)
	}
	if env := f.SessionEnv(rec.TmuxSession); !slices.Contains(env, "FLEET_ENGINE=claude-code") {
		t.Errorf("env %q lacks FLEET_ENGINE=claude-code", env)
	}
}

func TestSpawn_CoordHandoffExplicitFlagBeatsPersistedChoice(t *testing.T) {
	home := setupHandoffHome(t)
	if err := projects.WriteGlobalCoordConfigEngine(home, enginecfg.EngineCodex); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENGINE", enginecfg.EngineClaudeCode)
	t.Setenv("FLEET_ENGINE_EXPLICIT", "1")
	tmuxfake.InstallFake(t)
	old := oldCoord(t, enginecfg.EngineCodex)
	rec := handoffCoord(t, old, wrapper(t, enginecfg.EngineCodex))
	if rec.Engine != enginecfg.EngineClaudeCode {
		t.Errorf("rec.Engine = %q want claude-code (explicit flag this invocation)", rec.Engine)
	}
	if !reflect.DeepEqual(rec.Command, wrapper(t, enginecfg.EngineClaudeCode)) {
		t.Errorf("rec.Command = %q want claude-code wrapper", rec.Command)
	}
}

func TestSpawn_WorkerHandoffIgnoresPersistedChoice(t *testing.T) {
	home := setupHandoffHome(t)
	if err := projects.WriteGlobalCoordConfigEngine(home, enginecfg.EngineCodex); err != nil {
		t.Fatal(err)
	}
	f := tmuxfake.InstallFake(t)
	old := agent.New("aaaa8888")
	old.TaskID = "worker-task"
	old.Project = "projects-rainier"
	old.Engine = enginecfg.EngineClaudeCode
	old.Role = "executor"
	rec, err := spawn.Spawn(spawn.Options{
		OldRecord: old,
		Command:   wrapper(t, enginecfg.EngineClaudeCode),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if rec.Engine != enginecfg.EngineClaudeCode {
		t.Errorf("worker rec.Engine = %q want claude-code (workers inherit their coord's engine)", rec.Engine)
	}
	if got := f.SessionCommand(rec.TmuxSession); !reflect.DeepEqual(got, wrapper(t, enginecfg.EngineClaudeCode)) {
		t.Errorf("tmux ran %q want claude-code wrapper", got)
	}
}

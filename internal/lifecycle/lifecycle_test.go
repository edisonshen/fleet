package lifecycle_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/lifecycle"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/workers"
)

func TestClassifyTask(t *testing.T) {
	cases := []struct {
		name string
		in   tasks.Status
		want lifecycle.State
	}{
		{"todo", tasks.StatusTodo, lifecycle.StatePrerun},
		{"ready", tasks.StatusReady, lifecycle.StatePrerun},
		{"in-progress", tasks.StatusInProgress, lifecycle.StateActive},
		{"in-review", tasks.StatusInReview, lifecycle.StateActive},
		{"blocked", tasks.StatusBlocked, lifecycle.StateWaiting},
		{"done", tasks.StatusDone, lifecycle.StateTerminalSuccess},
		{"abandoned", tasks.StatusAbandoned, lifecycle.StateTerminalFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lifecycle.ClassifyTask(&tasks.Task{Status: tc.in})
			if got != tc.want {
				t.Fatalf("ClassifyTask(%s) = %s; want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestClassifyTaskNil(t *testing.T) {
	if got := lifecycle.ClassifyTask(nil); got != lifecycle.StateUnknown {
		t.Fatalf("ClassifyTask(nil) = %s; want unknown", got)
	}
}

func TestClassifyTaskUnknownStatus(t *testing.T) {
	got := lifecycle.ClassifyTask(&tasks.Task{Status: tasks.Status("future")})
	if got != lifecycle.StateUnknown {
		t.Fatalf("ClassifyTask(future) = %s; want unknown", got)
	}
}

func TestClassifyWorker(t *testing.T) {
	cases := []struct {
		name string
		in   workers.Phase
		want lifecycle.State
	}{
		{"starting", workers.PhaseStarting, lifecycle.StatePrerun},
		{"branch", workers.PhaseBranch, lifecycle.StatePrerun},
		{"tdd-red", workers.PhaseTDDRed, lifecycle.StateActive},
		{"tdd-green", workers.PhaseTDDGreen, lifecycle.StateActive},
		{"tdd-refactor", workers.PhaseTDDRefactor, lifecycle.StateActive},
		{"review-claude", workers.PhaseReviewClaude, lifecycle.StateActive},
		{"review-codex", workers.PhaseReviewCodex, lifecycle.StateActive},
		// Three-stage flow handoff phases (reviewer-subagent-arch):
		// task is still in flight, classified Active.
		{"review-pending", workers.PhaseReviewPending, lifecycle.StateActive},
		{"review-done", workers.PhaseReviewDone, lifecycle.StateActive},
		{"push", workers.PhasePush, lifecycle.StateActive},
		{"done", workers.PhaseDone, lifecycle.StateTerminalSuccess},
		{"blocked", workers.PhaseBlocked, lifecycle.StateWaiting},
		{"failed", workers.PhaseFailed, lifecycle.StateTerminalFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lifecycle.ClassifyWorker(&workers.State{Phase: tc.in})
			if got != tc.want {
				t.Fatalf("ClassifyWorker(%s) = %s; want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestClassifyWorkerNil(t *testing.T) {
	if got := lifecycle.ClassifyWorker(nil); got != lifecycle.StateUnknown {
		t.Fatalf("ClassifyWorker(nil) = %s; want unknown", got)
	}
}

func TestClassifyAgent(t *testing.T) {
	t.Run("active when no flags", func(t *testing.T) {
		r := &agent.Record{NeedsInput: false, Blocked: false}
		if got := lifecycle.ClassifyAgent(r); got != lifecycle.StateActive {
			t.Fatalf("got %s; want active", got)
		}
	})
	t.Run("waiting when needs_input", func(t *testing.T) {
		r := &agent.Record{NeedsInput: true}
		if got := lifecycle.ClassifyAgent(r); got != lifecycle.StateWaiting {
			t.Fatalf("got %s; want waiting", got)
		}
	})
	t.Run("waiting when blocked", func(t *testing.T) {
		r := &agent.Record{Blocked: true}
		if got := lifecycle.ClassifyAgent(r); got != lifecycle.StateWaiting {
			t.Fatalf("got %s; want waiting", got)
		}
	})
	t.Run("nil → unknown", func(t *testing.T) {
		if got := lifecycle.ClassifyAgent(nil); got != lifecycle.StateUnknown {
			t.Fatalf("got %s; want unknown", got)
		}
	})
}

func TestStateIsTerminal(t *testing.T) {
	cases := []struct {
		s    lifecycle.State
		want bool
	}{
		{lifecycle.StateUnknown, false},
		{lifecycle.StatePrerun, false},
		{lifecycle.StateActive, false},
		{lifecycle.StateWaiting, false},
		{lifecycle.StateTerminalSuccess, true},
		{lifecycle.StateTerminalFailure, true},
	}
	for _, tc := range cases {
		t.Run(tc.s.String(), func(t *testing.T) {
			if got := tc.s.IsTerminal(); got != tc.want {
				t.Fatalf("IsTerminal() = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestOnTerminalTaskNoOp(t *testing.T) {
	// Tasks: OnTerminal must NOT touch disk regardless of status.
	// Verify by passing both terminal + non-terminal tasks and confirming
	// no error and no side-effect (we have nothing on disk to check;
	// this is a "doesn't crash" + "returns nil" test).
	cases := []tasks.Status{
		tasks.StatusTodo, tasks.StatusInProgress, tasks.StatusBlocked,
		tasks.StatusDone, tasks.StatusAbandoned,
	}
	for _, st := range cases {
		t.Run(string(st), func(t *testing.T) {
			tk := &tasks.Task{Slug: "x", Status: st}
			if err := lifecycle.OnTerminal(tk, "p"); err != nil {
				t.Fatalf("OnTerminal task: %v", err)
			}
		})
	}
}

func TestOnTerminalAgentNoOp(t *testing.T) {
	// Agents: OnTerminal must NOT touch the record regardless of state.
	r := &agent.Record{ID: "abcd1234"}
	if err := lifecycle.OnTerminal(r, ""); err != nil {
		t.Fatalf("OnTerminal agent: %v", err)
	}
	// Empty project is fine for agents — no disk path involved.
}

func TestOnTerminalWorkerDoneDeletes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)

	project := "demo"
	slug := "do-thing-1a2b"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("init project: %v", err)
	}
	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		t.Fatalf("WorkerDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Touch a state.json file so the dir is non-empty.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	w := &workers.State{
		Slug:    slug,
		Project: project,
		Phase:   workers.PhaseDone,
	}
	if err := lifecycle.OnTerminal(w, project); err != nil {
		t.Fatalf("OnTerminal: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("worker dir still exists after OnTerminal: %v", err)
	}
}

func TestOnTerminalWorkerFailedDeletes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)

	project := "demo"
	slug := "do-thing-cd34"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("init project: %v", err)
	}
	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		t.Fatalf("WorkerDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	w := &workers.State{Slug: slug, Project: project, Phase: workers.PhaseFailed}
	if err := lifecycle.OnTerminal(w, project); err != nil {
		t.Fatalf("OnTerminal: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("worker dir still exists after OnTerminal failed: %v", err)
	}
}

func TestOnTerminalWorkerBlockedDoesNotDelete(t *testing.T) {
	// Blocked is Waiting, not terminal — operator may unblock and
	// resume. OnTerminal must leave the dir alone.
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)

	project := "demo"
	slug := "blockedslug-1a2b"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("init project: %v", err)
	}
	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		t.Fatalf("WorkerDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	w := &workers.State{Slug: slug, Project: project, Phase: workers.PhaseBlocked}
	if err := lifecycle.OnTerminal(w, project); err != nil {
		t.Fatalf("OnTerminal: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("blocked worker dir was removed: %v", err)
	}
}

func TestOnTerminalWorkerActiveDoesNotDelete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)

	project := "demo"
	slug := "activeslug-1a2b"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("init project: %v", err)
	}
	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		t.Fatalf("WorkerDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	w := &workers.State{Slug: slug, Project: project, Phase: workers.PhaseTDDRed}
	if err := lifecycle.OnTerminal(w, project); err != nil {
		t.Fatalf("OnTerminal: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("active worker dir was removed: %v", err)
	}
}

func TestOnTerminalWorkerNilNoOp(t *testing.T) {
	if err := lifecycle.OnTerminal((*workers.State)(nil), "p"); err != nil {
		t.Fatalf("OnTerminal nil worker: %v", err)
	}
}

func TestOnTerminalWorkerEmptyProjectErrors(t *testing.T) {
	w := &workers.State{Slug: "x-1a2b", Phase: workers.PhaseDone}
	err := lifecycle.OnTerminal(w, "")
	if err == nil {
		t.Fatal("expected error for empty project")
	}
}

func TestOnTerminalWorkerEmptySlugErrors(t *testing.T) {
	w := &workers.State{Slug: "", Phase: workers.PhaseDone}
	err := lifecycle.OnTerminal(w, "p")
	if err == nil {
		t.Fatal("expected error for empty slug")
	}
}

func TestOnTerminalUnsupportedTypeErrors(t *testing.T) {
	type fake struct{}
	err := lifecycle.OnTerminal(&fake{}, "p")
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
}

func TestOnTerminalIdempotentOnMissingDir(t *testing.T) {
	// Calling OnTerminal twice for the same slug must not error on the
	// second call — Delete is ENOENT-tolerant.
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)

	project := "demo"
	slug := "idem-1a2b"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("init: %v", err)
	}
	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		t.Fatalf("WorkerDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	w := &workers.State{Slug: slug, Project: project, Phase: workers.PhaseDone}
	if err := lifecycle.OnTerminal(w, project); err != nil {
		t.Fatalf("first OnTerminal: %v", err)
	}
	if err := lifecycle.OnTerminal(w, project); err != nil {
		t.Fatalf("second OnTerminal: %v", err)
	}
}

// TestClassifyAgentTimeIndependent confirms ClassifyAgent doesn't read
// time.Now or LastActivityTS — the function must be deterministic for
// downstream lifecycle tests. We pin a very old LastActivityTS and
// expect Active (no flags = active) regardless.
func TestClassifyAgentTimeIndependent(t *testing.T) {
	r := &agent.Record{
		LastActivityTS: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if got := lifecycle.ClassifyAgent(r); got != lifecycle.StateActive {
		t.Fatalf("got %s; want active (no flags set, time should not factor)", got)
	}
}

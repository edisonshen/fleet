package planreview

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edisonshen/fleet/internal/projects"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
)

func TestLinkedDoc(t *testing.T) {
	cases := []struct {
		name string
		task tasks.Task
		want string
	}{
		{"spec bare", tasks.Task{Spec: "Task plan: docs/TASK-PLAN-a-0001.md"}, "docs/TASK-PLAN-a-0001.md"},
		{"backticks", tasks.Task{Spec: "see `docs/TASK-PLAN-a-0001.md`."}, "docs/TASK-PLAN-a-0001.md"},
		{"markdown link", tasks.Task{Spec: "[plan](docs/TASK-PLAN-x.md)"}, "docs/TASK-PLAN-x.md"},
		{"acceptance wins over notes", tasks.Task{Acceptance: "TASK-PLAN-b.md", Notes: "docs/TASK-PLAN-c.md"}, "TASK-PLAN-b.md"},
		{"absolute", tasks.Task{Notes: "plan /abs/docs/TASK-PLAN-z.md"}, "/abs/docs/TASK-PLAN-z.md"},
		{"html is not the plan", tasks.Task{Spec: "docs/TASK-PLAN-a.html"}, ""},
		{"design doc is not a task plan", tasks.Task{Spec: "docs/DESIGN-x.md"}, ""},
		{"none", tasks.Task{Spec: "embedded plan text"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LinkedDoc(&c.task); got != c.want {
				t.Errorf("LinkedDoc = %q, want %q", got, c.want)
			}
		})
	}
}

// Relative links resolve against the project's registered repo_path, not
// the caller's cwd (the TUI and the coord run from anywhere).
func TestEvaluate_ResolvesAgainstRepoPathAndTracksSHA(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := projects.Write("demo", projects.Meta{Schema: projects.SchemaVersion, RepoPath: repo}); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(repo, "docs", "TASK-PLAN-demo-1.md")
	if err := os.MkdirAll(filepath.Dir(doc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doc, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := &tasks.Task{Slug: "demo-1", Spec: "Task plan: docs/TASK-PLAN-demo-1.md"}

	st := Evaluate("demo", task)
	if st.State != StateUnreviewed || st.Doc != doc {
		t.Fatalf("Evaluate = %+v, want unreviewed at %s", st, doc)
	}
	sha := st.SHA
	if err := WriteApproval("demo", "demo-1", Approval{Doc: doc, DocSHA256: sha, ApprovedBy: "op"}); err != nil {
		t.Fatal(err)
	}
	// An approval without a review does not pass.
	if st := Evaluate("demo", task); st.State != StateUnreviewed {
		t.Fatalf("approval without review: %s", st.State)
	}

	if err := os.WriteFile(doc, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := Evaluate("demo", task); st.SHA == sha {
		t.Fatal("sha did not change with the doc")
	}

	if st := Evaluate("demo", &tasks.Task{Slug: "demo-1", Spec: "Task plan: docs/TASK-PLAN-missing.md"}); st.State != StateNoDoc || st.Err == nil {
		t.Fatalf("missing doc: %+v", st)
	}
	if _, err := ReadReview("demo", "../escape"); err == nil {
		t.Fatal("path-traversal slug accepted")
	}
}

package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/planreview"
	"github.com/edisonshen/fleet/internal/tasks"
	"github.com/edisonshen/fleet/internal/workers"
)

// seedPlanTask registers project "fleet" with a repo holding a linked
// task plan and returns the plan's sha256.
func seedPlanTask(t *testing.T, pdir string) string {
	t.Helper()
	repo := t.TempDir()
	doc := filepath.Join(repo, "docs", "TASK-PLAN-plan-aaaa.md")
	if err := os.MkdirAll(filepath.Dir(doc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doc, []byte("# plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedBlockedTask(t, pdir, "fleet", "plan-aaaa", tasks.StatusTodo, "Task plan: docs/TASK-PLAN-plan-aaaa.md")
	meta, _ := json.Marshal(map[string]string{"schema": "1", "repo_path": repo})
	if err := os.WriteFile(filepath.Join(pdir, "fleet", "meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := planreview.HashFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func writePlanReviewRecord(t *testing.T, pdir string, r planreview.Review) {
	t.Helper()
	dir := filepath.Join(pdir, "fleet", "plan-reviews")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(dir, "plan-aaaa.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func planRow(t *testing.T, pdir string) *taskRow {
	t.Helper()
	row, _ := scanProject(pdir, "fleet", time.Now(), 0)
	if row == nil || len(row.Tasks) != 1 {
		t.Fatalf("want one task row, got %+v", row)
	}
	return row.Tasks[0]
}

// TestPlanReview_BadgeAndFindingsPane walks a todo task through the gate
// as the operator sees it: no review, blocking findings grouped by
// severity/category, then clean + approved.
func TestPlanReview_BadgeAndFindingsPane(t *testing.T) {
	pdir := withFleetHome(t)
	stubReadTaskWorker(t, func(string, string) (*workers.State, error) { return nil, nil })
	sha := seedPlanTask(t, pdir)

	if got := planRow(t, pdir).Plan; got != string(planreview.StateUnreviewed) {
		t.Fatalf("unreviewed plan: badge state = %q", got)
	}

	writePlanReviewRecord(t, pdir, planreview.Review{
		Schema: 1, Project: "fleet", Slug: "plan-aaaa", DocSHA256: sha,
		ReviewedAt: "2026-10-05T18:00:00Z", Exit: 1,
		Slots: map[string]planreview.Slot{
			"alpha": {Engine: "codex", Model: "m1"},
			"beta":  {Engine: "claude", Model: "m2", Exit: 1},
		},
		Findings: []planreview.Finding{
			{Severity: "P2", Category: "security", Section: "Steps", Summary: "token logged", Slot: "alpha"},
			{Severity: "P1", Category: "bug", Section: "Files", Summary: "wrong function name", Slot: "beta"},
		},
	})
	tr := planRow(t, pdir)
	if tr.Plan != string(planreview.StateFindings) {
		t.Fatalf("blocking review: badge state = %q", tr.Plan)
	}
	if line := taskBlockLine(tr, 80, false); !strings.Contains(line, "plan findings") {
		t.Errorf("row should carry plan badge, got %q", line)
	}
	body, _, _ := readTaskDetail("fleet", "plan-aaaa")
	for _, want := range []string{"### Plan review", "gate:      findings", "P1 Bugs (1)", "[Files] wrong function name (beta)", "P2 Security (1)", "beta   claude/m2 exit 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "P1 Bugs") > strings.Index(body, "P2 Security") {
		t.Errorf("findings should be ordered most severe first:\n%s", body)
	}

	writePlanReviewRecord(t, pdir, planreview.Review{
		Schema: 1, Project: "fleet", Slug: "plan-aaaa", DocSHA256: sha,
		ReviewedAt: "2026-10-05T19:00:00Z", Clean: true,
	})
	if err := planreview.WriteApproval("fleet", "plan-aaaa", planreview.Approval{
		DocSHA256: sha, ApprovedBy: "op", ApprovedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := planRow(t, pdir).Plan; got != string(planreview.StateApproved) {
		t.Fatalf("approved plan: badge state = %q", got)
	}
	body, _, _ = readTaskDetail("fleet", "plan-aaaa")
	if !strings.Contains(body, "findings:  none") || !strings.Contains(body, "approved:") {
		t.Errorf("clean approved detail:\n%s", body)
	}
}

// TestPlanReview_NoBadgeOutsideTodo pins that the gate badge is only
// shown where promote would consult it.
func TestPlanReview_NoBadgeOutsideTodo(t *testing.T) {
	pdir := withFleetHome(t)
	seedBlockedTask(t, pdir, "fleet", "plan-aaaa", tasks.StatusInProgress, "docs/TASK-PLAN-plan-aaaa.md")
	if got := planRow(t, pdir).Plan; got != "" {
		t.Fatalf("in-progress task should carry no plan badge, got %q", got)
	}
}

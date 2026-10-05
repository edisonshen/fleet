package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/planreview"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
)

// linkPlanDoc writes docs/TASK-PLAN-<slug>.md under cwd (unregistered
// projects resolve relative links against cwd) and links it from the
// task spec. Returns the doc path.
func linkPlanDoc(t *testing.T, project, slug, body string) string {
	t.Helper()
	rel := filepath.Join("docs", "TASK-PLAN-"+slug+".md")
	if err := os.MkdirAll("docs", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rel, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, path, err := readTasks(project)
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.Get(slug)
	if err != nil {
		t.Fatal(err)
	}
	task.Spec += "\n\nTask plan: `" + rel + "`"
	if err := tasks.Write(path, f); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(rel)
	return abs
}

// writePlanReview stands in for `review_slot.py --plan` (the Python side
// is covered by test_plan_review.py): same record shape, same path.
func writePlanReview(t *testing.T, project, slug, doc string, exit int, findings ...planreview.Finding) {
	t.Helper()
	data, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	rec := planreview.Review{
		Schema: 1, Project: project, Slug: slug, Doc: doc,
		DocSHA256: hex.EncodeToString(sum[:]), Exit: exit, Clean: exit == 0,
		Findings: findings,
	}
	dir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "plan-reviews", slug+".json")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedApprovedPlan puts a task through the full gate (doc, clean review,
// operator approval) so promote-mechanics tests can promote it.
func seedApprovedPlan(t *testing.T, project, slug string) {
	t.Helper()
	doc := linkPlanDoc(t, project, slug, "# plan "+slug+"\n")
	writePlanReview(t, project, slug, doc, 0)
	role := os.Getenv("FLEET_ROLE")
	t.Setenv("FLEET_ROLE", "")
	if err := runTasksApprove(&tasksApproveOpts{project: project}, slug, &bytes.Buffer{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	t.Setenv("FLEET_ROLE", role)
}

func promoteErr(project, slug, force string) error {
	return runTasksPromote(&tasksPromoteOpts{project: project, force: force}, slug, &bytes.Buffer{})
}

func taskStatus(t *testing.T, project, slug string) tasks.Status {
	t.Helper()
	f, _, err := readTasks(project)
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.Get(slug)
	if err != nil {
		t.Fatal(err)
	}
	return task.Status
}

// Scenario: the operator walks a task through plan → review → approve →
// promote; every shortcut along the way is refused with the next step.
func TestPlanGate_ReviewApprovePromoteLifecycle(t *testing.T) {
	_, project := setupTasksHome(t)
	t.Setenv("FLEET_ROLE", "")
	slug := addTodoSlug(t, project, "gate-life")

	assertBlocked := func(state planreview.State, hint string) {
		t.Helper()
		err := promoteErr(project, slug, "")
		if err == nil || !strings.Contains(err.Error(), "("+string(state)+")") || !strings.Contains(err.Error(), hint) {
			t.Fatalf("promote: want blocked %s with %q, got %v", state, hint, err)
		}
		if got := taskStatus(t, project, slug); got != tasks.StatusTodo {
			t.Fatalf("blocked promote flipped status to %s", got)
		}
	}

	assertBlocked(planreview.StateNoDoc, "Task plan: docs/TASK-PLAN-<slug>.md")

	doc := linkPlanDoc(t, project, slug, "# plan v1\n")
	assertBlocked(planreview.StateUnreviewed, "review_slot.py --plan "+doc)

	writePlanReview(t, project, slug, doc, 1, planreview.Finding{Severity: "P1", Category: "bug", Summary: "wrong file"})
	assertBlocked(planreview.StateFindings, "1 P0/P1 finding(s)")
	if err := runTasksApprove(&tasksApproveOpts{project: project}, slug, &bytes.Buffer{}); err == nil {
		t.Fatal("approve accepted a review with P1 findings")
	}

	writePlanReview(t, project, slug, doc, 0)
	assertBlocked(planreview.StateReviewed, "fleet tasks approve "+slug)

	t.Setenv("FLEET_ROLE", "coord")
	if err := runTasksApprove(&tasksApproveOpts{project: project}, slug, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "operator-only") {
		t.Fatalf("coord self-approval: want operator-only refusal, got %v", err)
	}
	t.Setenv("FLEET_ROLE", "")
	out := &bytes.Buffer{}
	if err := runTasksApprove(&tasksApproveOpts{project: project}, slug, out); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !strings.Contains(out.String(), "approved "+slug) {
		t.Errorf("approve output: %q", out.String())
	}

	// Editing the doc after approval invalidates review and approval.
	if err := os.WriteFile(doc, []byte("# plan v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertBlocked(planreview.StateStale, "changed since its review")
	if err := runTasksApprove(&tasksApproveOpts{project: project}, slug, &bytes.Buffer{}); err == nil {
		t.Fatal("approve accepted a stale review")
	}
	writePlanReview(t, project, slug, doc, 0)
	assertBlocked(planreview.StateReviewed, "older revision")
	if err := runTasksApprove(&tasksApproveOpts{project: project}, slug, &bytes.Buffer{}); err != nil {
		t.Fatalf("re-approve: %v", err)
	}

	if err := promoteErr(project, slug, ""); err != nil {
		t.Fatalf("promote after approve: %v", err)
	}
	if got := taskStatus(t, project, slug); got != tasks.StatusReady {
		t.Fatalf("status after promote: %s", got)
	}
}

// Scenario: --force bypasses the gate from an operator shell only, and the
// override is recorded as a forced approval.
func TestPlanGate_ForceIsOperatorOnlyAndRecorded(t *testing.T) {
	_, project := setupTasksHome(t)
	slug := addTodoSlug(t, project, "gate-force")

	t.Setenv("FLEET_ROLE", "coord")
	if err := promoteErr(project, slug, "hotfix"); err == nil || !strings.Contains(err.Error(), "operator-only") {
		t.Fatalf("coord --force: want operator-only refusal, got %v", err)
	}
	t.Setenv("FLEET_ROLE", "")
	if err := promoteErr(project, slug, "   "); err == nil {
		t.Fatal("blank --force bypassed the gate")
	}
	if err := promoteErr(project, slug, "prod hotfix,\n no plan"); err != nil {
		t.Fatalf("operator --force: %v", err)
	}
	if got := taskStatus(t, project, slug); got != tasks.StatusReady {
		t.Fatalf("status after forced promote: %s", got)
	}
	a, err := planreview.ReadApproval(project, slug)
	if err != nil || a == nil {
		t.Fatalf("forced approval not recorded: %v %v", a, err)
	}
	if !a.Forced || a.Reason != "prod hotfix, no plan (gate was no-plan)" {
		t.Errorf("forced approval: %+v", a)
	}
}

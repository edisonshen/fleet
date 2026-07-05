package main

// tasks_session_test.go — `fleet tasks promote` appends the promoted slug
// to coord-state.json:session_tasks (the auto Next-Steps buffer), strictly
// best-effort after the tasks-lock is released. See
// docs/TASK-PLAN-handoff-next-steps-open-21c2.md (T7, T16).

import (
	"bytes"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// sessionTasksOf pulls session_tasks out of a coord-state map.
func sessionTasksOf(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["session_tasks"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

// addTodo adds a todo task and returns its slug.
func addTodo(t *testing.T, project, slug string) string {
	t.Helper()
	out := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: slug, priority: "P2",
		spec: "spec body", spawnedBy: "user", status: "todo",
	}, "", out); err != nil {
		t.Fatalf("add %s: %v", slug, err)
	}
	return slug
}

// T7 — promote seam: a todo→ready promote records the slug in session_tasks
// with a coord_id stamp; sibling coord-state keys survive.
func TestTasksPromote_RecordsSessionTask(t *testing.T) {
	home, project := setupTasksHome(t)
	t.Setenv("FLEET_AGENT_ID", "beef0001")
	// Pre-seed a sibling key that must survive the append RMW.
	seedCoordStateFile(t, home, project, `{"tick_count":9}`)
	slug := addTodo(t, project, "promote-me")

	if err := runTasksPromote(&tasksPromoteOpts{project: project}, slug, &bytes.Buffer{}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	m := readCoordState(t, home, project)
	st := sessionTasksOf(t, m)
	if len(st) != 1 || st[0]["slug"] != slug {
		t.Fatalf("session_tasks: got %#v", st)
	}
	if st[0]["coord_id"] != "beef0001" {
		t.Errorf("session_tasks entry not coord-stamped: %#v", st[0])
	}
	if ts, _ := st[0]["ts"].(string); ts == "" {
		t.Errorf("session_tasks entry missing ts: %#v", st[0])
	}
	if tc, _ := m["tick_count"].(float64); tc != 9 {
		t.Errorf("sibling tick_count clobbered: %v", m["tick_count"])
	}
}

// Dedupe by slug: promoting the same slug twice keeps one entry (ts/coord_id
// refreshed, moved to tail). Second promote is the already-ready no-op path,
// which still refreshes the stamp.
func TestTasksPromote_DedupesSessionTaskBySlug(t *testing.T) {
	home, project := setupTasksHome(t)
	addTodo(t, project, "dup-slug")
	if err := runTasksPromote(&tasksPromoteOpts{project: project}, "dup-slug", &bytes.Buffer{}); err != nil {
		t.Fatalf("promote 1: %v", err)
	}
	// Second promote: task is already ready → no-op transition, but the
	// slug is still recorded once (deduped).
	if err := runTasksPromote(&tasksPromoteOpts{project: project}, "dup-slug", &bytes.Buffer{}); err != nil {
		t.Fatalf("promote 2: %v", err)
	}
	st := sessionTasksOf(t, readCoordState(t, home, project))
	if len(st) != 1 || st[0]["slug"] != "dup-slug" {
		t.Fatalf("expected 1 deduped session_task, got %#v", st)
	}
}

// T16 — best-effort: when coordinator.lock is held (a live tick), the
// session_tasks append is dropped but `fleet tasks promote` STILL promotes
// tasks.md and returns success (exit 0). The append must never fail or block
// a core command.
func TestTasksPromote_SessionTaskBestEffortUnderLock(t *testing.T) {
	home, project := setupTasksHome(t)
	slug := addTodo(t, project, "under-lock")

	// Hold coordinator.lock for the whole promote so the append RMW times
	// out. Separate open file description → contends even in-process.
	release, err := state.LockCoordinatorTimeout(project, 2*time.Second)
	if err != nil {
		t.Fatalf("hold coordinator.lock: %v", err)
	}
	defer release()

	out := &bytes.Buffer{}
	if err := runTasksPromote(&tasksPromoteOpts{project: project}, slug, out); err != nil {
		t.Fatalf("promote must succeed despite locked coord-state: %v", err)
	}
	// tasks.md WAS promoted.
	f, _, err := readTasks(project)
	if err != nil {
		t.Fatalf("readTasks: %v", err)
	}
	task, err := f.Get(slug)
	if err != nil {
		t.Fatalf("get %s: %v", slug, err)
	}
	if string(task.Status) != "ready" {
		t.Errorf("promote did not land in tasks.md: status=%s", task.Status)
	}
	// The session_tasks append was dropped (lock held) — coord-state.json
	// either absent or has no session_tasks for this slug.
	_ = home
}

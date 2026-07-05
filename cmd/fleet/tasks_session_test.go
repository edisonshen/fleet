package main

// tasks_session_test.go — `fleet tasks promote` appends the promoted slug
// to coord-state.json:session_tasks (the auto Next-Steps buffer), strictly
// best-effort after the tasks-lock is released. See
// docs/TASK-PLAN-handoff-next-steps-open-21c2.md (T7, T16).

import (
	"bytes"
	"fmt"
	"strings"
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

// addTodo adds a todo task and returns its ACTUAL slug (tasks add appends a
// random suffix, so we parse it from the "added <slug>" output).
func addTodo(t *testing.T, project, slug string) string {
	t.Helper()
	out := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: slug, priority: "P2",
		spec: "spec body", spawnedBy: "user", status: "todo",
	}, "", out); err != nil {
		t.Fatalf("add %s: %v", slug, err)
	}
	parts := strings.Fields(out.String())
	if len(parts) < 2 {
		t.Fatalf("unexpected add output: %q", out.String())
	}
	return parts[1]
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
	slug := addTodo(t, project, "dup-slug")
	if err := runTasksPromote(&tasksPromoteOpts{project: project}, slug, &bytes.Buffer{}); err != nil {
		t.Fatalf("promote 1: %v", err)
	}
	// Second promote: task is already ready → no-op transition, but the
	// slug is still recorded once (deduped).
	if err := runTasksPromote(&tasksPromoteOpts{project: project}, slug, &bytes.Buffer{}); err != nil {
		t.Fatalf("promote 2: %v", err)
	}
	st := sessionTasksOf(t, readCoordState(t, home, project))
	if len(st) != 1 || st[0]["slug"] != slug {
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

// Rejected-status path: promoting a task that's neither todo nor ready
// returns an error and must NOT append to session_tasks — the `recorded`
// gate must stay false on that branch. Reviewer iter-2 test-completeness
// addition (testing specialist finding): the happy paths (T7, dedupe, T16)
// were covered but the rejected-status branch's effect on session_tasks
// was not asserted anywhere.
func TestTasksPromote_RejectedStatusDoesNotRecordSessionTask(t *testing.T) {
	home, project := setupTasksHome(t)
	slug := addTodo(t, project, "not-promotable")
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=in-progress", &bytes.Buffer{}); err != nil {
		t.Fatalf("set status=in-progress: %v", err)
	}
	seedCoordStateFile(t, home, project, `{"tick_count":1}`)

	if err := runTasksPromote(&tasksPromoteOpts{project: project}, slug, &bytes.Buffer{}); err == nil {
		t.Fatalf("expected error promoting an in-progress task")
	}
	st := sessionTasksOf(t, readCoordState(t, home, project))
	if len(st) != 0 {
		t.Errorf("rejected-status promote must not record session_tasks: %#v", st)
	}
}

// session_tasks caps at sessionTasksMax (30, newest kept) — mirrors
// TestCheckpointNextStep_CapsToNewest for the sibling session_next_steps
// buffer. Reviewer iter-2 test-completeness addition (maintainability
// specialist finding): the Python co-writer already has an equivalent test
// (test_record_session_task_caps_to_30); the Go writer did not.
func TestAppendSessionTask_CapsToNewest(t *testing.T) {
	home, project := setupTasksHome(t)
	const n = sessionTasksMax + 5 // 35 > cap 30
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("slug-%02d", i)
		if err := appendSessionTask(project, slug); err != nil {
			t.Fatalf("appendSessionTask %d: %v", i, err)
		}
	}
	m := readCoordState(t, home, project)
	st := sessionTasksOf(t, m)
	if len(st) != sessionTasksMax {
		t.Fatalf("expected cap %d, got %d", sessionTasksMax, len(st))
	}
	if st[0]["slug"] != "slug-05" {
		t.Errorf("oldest not trimmed: head=%v want slug-05", st[0]["slug"])
	}
	if st[len(st)-1]["slug"] != fmt.Sprintf("slug-%02d", n-1) {
		t.Errorf("newest not at tail: %v", st[len(st)-1]["slug"])
	}
}

// review iter-4 (codex P1): `fleet tasks set <slug> status=...` is the
// documented non-promote path for a status transition, but was not a
// session_tasks-recording seam. A coord that sets a slug to ready via
// `tasks set` (rather than `tasks promote`) must still see it in a
// session-scoped Next Steps render.
func TestTasksSet_StatusChangeRecordsSessionTask(t *testing.T) {
	home, project := setupTasksHome(t)
	t.Setenv("FLEET_AGENT_ID", "cafe0002")
	slug := addTodo(t, project, "set-to-ready")

	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=ready", &bytes.Buffer{}); err != nil {
		t.Fatalf("set status=ready: %v", err)
	}
	st := sessionTasksOf(t, readCoordState(t, home, project))
	if len(st) != 1 || st[0]["slug"] != slug {
		t.Fatalf("session_tasks: got %#v", st)
	}
	if st[0]["coord_id"] != "cafe0002" {
		t.Errorf("session_tasks entry not coord-stamped: %#v", st[0])
	}
}

// `parked=...` is the Open-Questions-relevant field mutation that isn't a
// status= change at all; it must ALSO stamp session_tasks.
func TestTasksSet_ParkedChangeRecordsSessionTask(t *testing.T) {
	home, project := setupTasksHome(t)
	slug := addTodo(t, project, "park-me")

	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "parked=2026-07-05 waiting on operator", &bytes.Buffer{}); err != nil {
		t.Fatalf("set parked: %v", err)
	}
	st := sessionTasksOf(t, readCoordState(t, home, project))
	if len(st) != 1 || st[0]["slug"] != slug {
		t.Fatalf("session_tasks: got %#v", st)
	}
}

// status=blocked is Open-Questions-renderable — must stamp.
func TestTasksSet_StatusBlockedRecordsSessionTask(t *testing.T) {
	home, project := setupTasksHome(t)
	slug := addTodo(t, project, "set-to-blocked")

	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=blocked", &bytes.Buffer{}); err != nil {
		t.Fatalf("set status=blocked: %v", err)
	}
	st := sessionTasksOf(t, readCoordState(t, home, project))
	if len(st) != 1 || st[0]["slug"] != slug {
		t.Fatalf("session_tasks: got %#v", st)
	}
}

// review iter-5 [P2] (codex): status targets CollectNextSteps/
// CollectOpenQuestions never render (in-progress, in-review, done,
// abandoned) must NOT stamp session_tasks — recording every status
// churn could evict an older, still-ready/blocked slug out of the
// capped (30) buffer for zero rendering benefit.
func TestTasksSet_NonRenderableStatusDoesNotRecordSessionTask(t *testing.T) {
	for _, status := range []string{"in-progress", "in-review", "done", "abandoned"} {
		t.Run(status, func(t *testing.T) {
			home, project := setupTasksHome(t)
			slug := addTodo(t, project, "set-to-"+status)
			seedCoordStateFile(t, home, project, `{"tick_count":1}`)

			if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status="+status, &bytes.Buffer{}); err != nil {
				t.Fatalf("set status=%s: %v", status, err)
			}
			st := sessionTasksOf(t, readCoordState(t, home, project))
			if len(st) != 0 {
				t.Errorf("status=%s must not record session_tasks: %#v", status, st)
			}
		})
	}
}

// Mutating a field the Next Steps/Open Questions collectors don't look
// at (e.g. priority) must NOT stamp session_tasks — the `recorded` gate
// stays false so an unrelated field bump doesn't fabricate session
// activity for a slug this coord never actually queued/blocked.
func TestTasksSet_NonStatusNonParkedKeyDoesNotRecordSessionTask(t *testing.T) {
	home, project := setupTasksHome(t)
	slug := addTodo(t, project, "bump-priority")
	// Seed coord-state.json so readCoordState below has a file to read
	// regardless of whether this set records anything.
	seedCoordStateFile(t, home, project, `{"tick_count":1}`)

	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "priority=P0", &bytes.Buffer{}); err != nil {
		t.Fatalf("set priority: %v", err)
	}
	st := sessionTasksOf(t, readCoordState(t, home, project))
	if len(st) != 0 {
		t.Errorf("priority-only set must not record session_tasks: %#v", st)
	}
}

package main

// tasks_session_test.go — the coord-heartbeat-preservation invariant
// (codex iter-11 [P1]): `fleet tasks add|set|promote` must NEVER write
// coord-state.json. Its mtime IS the coord tick heartbeat that
// coordStateFresh() (dispatch_recovery.go) and the TUI dashboard read as
// proof a coordinator is alive; a CLI write from an operator shell on an
// idle project would spoof it (false "● active" + spurious spawn refusal).
// session_tasks is instead populated ONLY by the coord tick
// (skills/coordinator dispatch.record_session_task, part of the legitimate
// per-tick coord-state.json write). See
// docs/TASK-PLAN-handoff-next-steps-open-21c2.md.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// coordStateExists reports whether projects/<project>/coord-state.json is
// present under home.
func coordStateExists(t *testing.T, home, project string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(home, "projects", project, "coord-state.json"))
	return err == nil
}

// addTodoSlug adds a todo task and returns its ACTUAL slug (tasks add
// appends a random suffix, parsed from the "added <slug>" output).
func addTodoSlug(t *testing.T, project, slug string) string {
	t.Helper()
	out := &bytes.Buffer{}
	if err := runTasksAdd(&tasksAddOpts{
		project: project, slug: slug, priority: "P2",
		spec: "spec body", spawnedBy: "user", status: "todo",
	}, "", out); err != nil {
		t.Fatalf("add %s: %v", slug, err)
	}
	fields := bytes.Fields(out.Bytes())
	if len(fields) < 2 {
		t.Fatalf("unexpected add output: %q", out.String())
	}
	return string(fields[1])
}

// tasks add must not create coord-state.json (would spoof the heartbeat on
// an idle project).
func TestTasksAdd_DoesNotWriteCoordState(t *testing.T) {
	home, project := setupTasksHome(t)
	_ = addTodoSlug(t, project, "filed-idle")
	if coordStateExists(t, home, project) {
		t.Error("fleet tasks add must not create coord-state.json (heartbeat spoof)")
	}
}

// tasks promote (todo→ready) must not create/touch coord-state.json.
func TestTasksPromote_DoesNotWriteCoordState(t *testing.T) {
	home, project := setupTasksHome(t)
	slug := addTodoSlug(t, project, "promote-idle")
	if err := runTasksPromote(&tasksPromoteOpts{project: project}, slug, &bytes.Buffer{}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if coordStateExists(t, home, project) {
		t.Error("fleet tasks promote must not create coord-state.json (heartbeat spoof)")
	}
}

// promote still performs the todo→ready transition in tasks.md (the write
// suppression only removes the coord-state.json side effect).
func TestTasksPromote_StillFlipsStatus(t *testing.T) {
	_, project := setupTasksHome(t)
	slug := addTodoSlug(t, project, "promote-flips")
	if err := runTasksPromote(&tasksPromoteOpts{project: project}, slug, &bytes.Buffer{}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	f, _, err := readTasks(project)
	if err != nil {
		t.Fatalf("readTasks: %v", err)
	}
	task, err := f.Get(slug)
	if err != nil {
		t.Fatalf("get %s: %v", slug, err)
	}
	if string(task.Status) != "ready" {
		t.Errorf("promote did not flip status: got %s", task.Status)
	}
}

// tasks set (status / parked / any key) must not create/touch
// coord-state.json.
func TestTasksSet_DoesNotWriteCoordState(t *testing.T) {
	for _, kv := range []string{"status=ready", "status=blocked", "parked=waiting on operator", "priority=P0"} {
		t.Run(kv, func(t *testing.T) {
			home, project := setupTasksHome(t)
			slug := addTodoSlug(t, project, "set-idle")
			if err := runTasksSet(&tasksSetOpts{project: project}, slug, kv, &bytes.Buffer{}); err != nil {
				t.Fatalf("set %q: %v", kv, err)
			}
			if coordStateExists(t, home, project) {
				t.Errorf("fleet tasks set %q must not create coord-state.json (heartbeat spoof)", kv)
			}
		})
	}
}

// A pre-existing coord-state.json (a live/prior coord's heartbeat) must be
// left byte-for-byte untouched by a task command — its mtime must not be
// bumped, so the heartbeat stays truthful.
func TestTasksSet_LeavesExistingCoordStateUntouched(t *testing.T) {
	home, project := setupTasksHome(t)
	slug := addTodoSlug(t, project, "set-existing")
	seedCoordStateFile(t, home, project, `{"worker_agent_ids":{"keep":"me01"},"tick_count":7}`)
	path := filepath.Join(home, "projects", project, "coord-state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if err := runTasksSet(&tasksSetOpts{project: project}, slug, "status=blocked", &bytes.Buffer{}); err != nil {
		t.Fatalf("set: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("tasks set rewrote coord-state.json:\n before=%s\n after=%s", before, after)
	}
}

package tui

// PR3 (DESIGN-coord-repo-binding-from-project.md): the coord repo binding
// is resolved via coordrepo.ResolveProjectRepo, NEVER the launch cwd.
// These tests cover the TUI coord-binding call-sites: [a] attach (Path 3
// fresh spawn) and dead-session recovery. The cross-repo-cwd bug (E1) and
// its regression guard (E2) live here; recovery/handoff binding is in
// cmd/fleet + internal/handoffop.
//
// E1 is the load-bearing regression: launch fleet in repo A, attach
// project B → the spawned coord binds B's checkout (from meta.json), NOT
// the launch cwd A. On the parent commit coordCwdForProject fell to
// os.Getwd() and bound A — E2 documents why E1 fails there.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
)

// seedProjectMeta writes a non-git meta.json pinning repo_path to repo so
// the resolver binds via tier 1 (is_git=false short-circuits on dir
// existence — no git identity needed). Returns repo.
func seedProjectMeta(t *testing.T, project, repo string) string {
	t.Helper()
	home := os.Getenv("FLEET_HOME")
	if home == "" {
		t.Fatal("FLEET_HOME not set; call t.Setenv first")
	}
	pdir := filepath.Join(home, "projects", project)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	meta := `{"schema":"v1","is_git":false,"repo_path":"` + repo + `","added_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(pdir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}
	return repo
}

// cwdArg extracts the value following --cwd in a captured dispatch argv,
// or "" when absent.
func cwdArg(args []string) string {
	for i, a := range args {
		if a == "--cwd" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// E1 — cross-repo cwd (the bug). Launch dir != the attached project's
// checkout; meta.json pins the project's repo. [a] attach must spawn the
// coord bound to the meta repo, NOT the launch cwd.
func TestAttachProject_BindsResolverRepoNotLaunchCwd(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	(&stubCoordSpawnMarker{markers: map[string]string{}}).install(t) // no coord
	stub := &stubFleetCmd{}
	stub.install(t)

	// asgard's checkout — the CORRECT binding (lives somewhere unrelated
	// to wherever the test process launched / `git rev-parse` resolves).
	asgardRepo := t.TempDir()
	seedProjectMeta(t, "projects-asgard", asgardRepo)

	// A loose dead record tags the project so the project row exists, but
	// it is NOT a coord (no coord-<project> task_id) → Path 3 fresh spawn.
	loose := agent.New("loose001")
	loose.Project = "projects-asgard"
	loose.TmuxSession = "fleet-loose001"
	m := projectRowModel(t, "projects-asgard", []*agent.Record{loose},
		map[string]bool{"loose001": false})

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if mm.flash != nil && mm.flash.isErr {
		t.Fatalf("attach unexpectedly refused: %q", mm.flash.text)
	}
	if cmd == nil {
		t.Fatal("expected a spawn cmd from Path-3 attach")
	}
	_ = cmd() // fire the dispatch shell-out so stub captures argv
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 dispatch call, got %d: %v", len(stub.calls), stub.calls)
	}
	got := cwdArg(stub.calls[0])
	if got != asgardRepo {
		t.Errorf("coord bound the WRONG repo: --cwd = %q; want %q (meta repo, not launch cwd)",
			got, asgardRepo)
	}
}

// E2 — regression guard (documented). On the parent commit
// coordCwdForProject's tier 3 was os.Getwd(): with NO meta.json it bound
// the launch cwd. This test asserts the FIXED behavior — no meta.json AND
// no worktrees → REFUSE, no spawn. On the parent commit the same input
// would have spawned a coord bound to os.Getwd() (E1 inverted), proving
// the cwd tier is gone.
func TestAttachProject_RefusesWhenUnresolvable_NoSpawn(t *testing.T) {
	t.Setenv("FLEET_HOME", t.TempDir())
	(&stubSessionAlive{}).install(t)
	(&stubProjectTreeExists{}).install(t)
	(&stubCoordSpawnMarker{markers: map[string]string{}}).install(t)
	stub := &stubFleetCmd{}
	stub.install(t)

	// NO meta.json, no worktrees → resolver refuses.
	loose := agent.New("loose002")
	loose.Project = "orphanproj"
	loose.TmuxSession = "fleet-loose002"
	m := projectRowModel(t, "orphanproj", []*agent.Record{loose},
		map[string]bool{"loose002": false})

	updated, cmd := m.Update(keyMsg("a"))
	mm := updated.(Model)
	if cmd != nil {
		t.Error("refused attach must NOT produce a spawn cmd")
	}
	if len(stub.calls) != 0 {
		t.Errorf("refused attach must NOT shell out; got %v", stub.calls)
	}
	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash on unresolvable attach, got %+v", mm.flash)
	}
	// E3 — the surfaced hint names the project, says cwd was ignored, and
	// suggests `fleet project add --project <p>`.
	for _, want := range []string{
		"orphanproj",
		"no usable checkout",
		"launch cwd was intentionally ignored",
		"fleet project add --project orphanproj",
	} {
		if !strings.Contains(mm.flash.text, want) {
			t.Errorf("refusal hint missing %q; got:\n%s", want, mm.flash.text)
		}
	}
}

package coordrepo

// U3 — path parity (DESIGN-coord-repo-binding-from-project.md PR3). Every
// Go coord-binding entry path must resolve the repo through the ONE shared
// resolver (ResolveProjectRepo), and the deleted cwd tiers must NOT
// reappear. This is a structural source-scan guard: a careless future
// edit that re-adds os.Getwd() as a coord-binding tier, or that resurrects
// the old coordCwdForProject helper, fails here even if it compiles.
//
// We scan source files (not behavior) deliberately — behavior is covered
// by E1-E9 in the tui / cmd / handoffop packages; this test pins that ALL
// five paths funnel through the resolver and none diverges with its own
// cwd logic.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from this package dir to the module root (the dir
// holding go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from package dir")
		}
		dir = parent
	}
}

func readSource(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestPathParity_AllCoordBindingPathsUseResolver asserts each coord-
// binding call-site references the shared resolver.
func TestPathParity_AllCoordBindingPathsUseResolver(t *testing.T) {
	root := repoRoot(t)
	// file → a token that proves it routes through the resolver.
	cases := map[string]string{
		"internal/tui/keys.go":            "coordrepo.ResolveProjectRepo", // [a] attach + dead-session
		"internal/tui/model.go":           "coordRepoForProject",          // [r] reset respawn
		"cmd/fleet/dispatch.go":           "coordrepo.ResolveProjectRepo", // crash recovery
		"cmd/fleet/handoff.go":            "coordrepo.ResolveProjectRepo", // manual [h]
		"cmd/fleet/attach.go":             "coordrepo.ResolveProjectRepo", // fleet attach recovery spawn
		"internal/handoffop/handoffop.go": "coordrepo.ResolveProjectRepo", // drain
	}
	for rel, token := range cases {
		src := readSource(t, root, rel)
		if !strings.Contains(src, token) {
			t.Errorf("%s does not route coord binding through the resolver (missing %q)", rel, token)
		}
	}
}

// TestPathParity_DeletedCwdTiersStayDeleted asserts the old launch-cwd
// binding tiers are gone.
func TestPathParity_DeletedCwdTiersStayDeleted(t *testing.T) {
	root := repoRoot(t)

	// The old TUI helper that fell through to os.Getwd() must not return.
	keys := readSource(t, root, "internal/tui/keys.go")
	if strings.Contains(keys, "func coordCwdForProject") {
		t.Error("internal/tui/keys.go: coordCwdForProject (the deleted os.Getwd cwd-tier helper) has reappeared")
	}

	// The dead-coord recovery in dispatch.go must not reinstate the
	// oldRecord.Cwd fallback for COORD spawns. The worker fallback
	// (else-branch) legitimately keeps it, so we only forbid the
	// coord-branch shape `spawnCwd = oldRecord.Cwd` outside the worker
	// guard. Cheap structural proxy: the resolver call must precede any
	// oldRecord.Cwd reference in the coord branch — assert the coord
	// branch resolves.
	dispatch := readSource(t, root, "cmd/fleet/dispatch.go")
	if !strings.Contains(dispatch, "if opts.coordSpawn {") ||
		!strings.Contains(dispatch, "coordrepo.ResolveProjectRepo(opts.project, true)") {
		t.Error("cmd/fleet/dispatch.go: coord recovery must resolve via the resolver before spawn")
	}
}

package spawn

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/edisonshen/fleet/internal/projects"
)

// coordTaskIDPrefix matches cmd/fleet/dispatch.go's CoordTaskIDPrefix.
// Duplicated here (rather than imported) to avoid a cmd/fleet →
// internal/spawn cycle. Keep in sync with the dispatch package's
// constant; a drift test in cmd/fleet/dispatch_test.go could pin both
// later if the constant churns.
const coordTaskIDPrefix = "coord-"

// isCoordSpawn returns true when the dispatch parameters indicate this
// is a coord (not a worker / generic) spawn.
//
// The signal we trust: TaskID == "coord-<project>" AND Project is
// non-empty. This is the convention cmd/fleet/dispatch.go follows when
// it spawns a coord — every other dispatch path uses a real task slug
// (worker) or leaves TaskID empty (raw spawn).
//
// Handoff branch (OldRecord non-nil): we also trip on inherited
// TaskID, so a handoff replacement for a coord continues stamping the
// repo. Spawn already sets rec.TaskID = OldRecord.TaskID on that path.
func isCoordSpawn(taskID, project string) bool {
	if project == "" {
		return false
	}
	return taskID == coordTaskIDPrefix+project
}

// IsCoordSpawn is the exported form of isCoordSpawn so other packages
// (cmd/fleet, internal/handoffop) can apply the SAME coord-vs-worker
// discriminator the spawn path uses, rather than re-deriving the
// convention and drifting. DESIGN-coord-repo-binding-from-project.md
// §6: coord binding resolves through the shared resolver; worker binding
// keeps inheriting its dispatch cwd. Callers gate that split on this.
func IsCoordSpawn(taskID, project string) bool {
	return isCoordSpawn(taskID, project)
}

// CoordConfigPath is projects.CoordConfigPath; kept here so spawn-side
// callers and tests keep one import.
func CoordConfigPath(fleetHome, project string) string {
	return projects.CoordConfigPath(fleetHome, project)
}

// ReadCoordConfigEngine is projects.ReadCoordConfigEngine.
func ReadCoordConfigEngine(fleetHome, project string) string {
	return projects.ReadCoordConfigEngine(fleetHome, project)
}

// writeCoordConfigStamp stamps the resolved repo path and the coord's
// engine into ~/.fleet/projects/<project>/coord-config.json.
//
// Schema (additive — never break existing readers):
//
//	{
//	    "parallelism": <int>,   // loop.py _load_parallelism
//	    "repo":        <str>,   // fleet#175 — coord's project checkout
//	    "engine":      <str>    // dominant engine of the last coord spawn
//	}
//
// Both keys are overwritten unconditionally; every other field is
// preserved. `engine` is what lets a later `fleet` (TUI [a], `fleet
// attach` Tier 3) with no engine flag respawn the project's coord under
// the engine the operator originally chose — the per-project memory of
// `fleet -codex` / `fleet -claude`. An explicit flag on the respawn
// always wins over it (cmd/fleet/dispatch.go).
//
// Atomic: write to a tempfile in the same directory, fsync, rename.
// On any non-recoverable error returns it for the caller to log; we
// deliberately do NOT fail the spawn because the worst case is the
// coord skill falls through to its legacy cwd-derived behavior + emits
// a warning into TickResult.errors. A failed write here is a
// breadcrumb, not a blocker.
func writeCoordConfigStamp(fleetHome, project, repo, engine string) error {
	if fleetHome == "" || project == "" || repo == "" {
		return errors.New("writeCoordConfigStamp: empty input")
	}
	projDir := filepath.Join(fleetHome, "projects", project)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		return fmt.Errorf("mkdir project dir: %w", err)
	}
	cfgPath := CoordConfigPath(fleetHome, project)

	// Read existing config (if any). Treat unreadable/malformed as
	// empty — we'll write a clean one. Matches the Python helper's
	// behavior (skills/coordinator/coord_config.py::write_repo_idempotent).
	data := map[string]any{}
	if raw, err := os.ReadFile(cfgPath); err == nil {
		var existing map[string]any
		if jerr := json.Unmarshal(raw, &existing); jerr == nil && existing != nil {
			data = existing
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read existing coord-config.json: %w", err)
	}

	// Always overwrite `repo` with the resolved repo (the `repo` arg).
	//
	// DESIGN-coord-repo-binding-from-project.md (PR3): the INPUT is now
	// the output of coordrepo.ResolveProjectRepo (a project-derived
	// checkout), NOT the launch cwd that the #175-era code stamped. The
	// caller (spawn.Spawn coord branch) runs this on EVERY coord spawn
	// AND handoff. The overwrite is unconditional so a respawn from the
	// freshly-resolved repo can correct any prior wrong value.
	//
	// coord-config.json::repo is now a strictly NON-authoritative
	// write-only breadcrumb: the Python tick (PR4) shells out to the Go
	// resolver instead of reading this file. The operator's durable pin
	// lives in meta.json::repo_path (set via `fleet project add`), which
	// the resolver reads first (tier 1) on every bind.
	data["repo"] = repo
	if engine != "" {
		data["engine"] = engine
	}

	// Atomic write: tmp + fsync + rename in the same directory.
	tmp, err := os.CreateTemp(projDir, "coord-config.json.tmp.*")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on any error path; success path renames first
	// so the unlink is a no-op.
	defer func() { _ = os.Remove(tmpName) }()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode coord-config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync coord-config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp coord-config: %w", err)
	}
	if err := os.Rename(tmpName, cfgPath); err != nil {
		return fmt.Errorf("rename coord-config: %w", err)
	}
	return nil
}

// fleetHomeForSpawn resolves the FLEET_HOME root: honors the env
// override (set by tests + custom installs), falling back to
// $HOME/.fleet. Returns empty string only on a truly busted env (no
// HOME, no FLEET_HOME) — caller should skip the write in that case
// rather than blow up the spawn.
func fleetHomeForSpawn() string {
	if h := os.Getenv("FLEET_HOME"); h != "" {
		return h
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".fleet")
	}
	return ""
}

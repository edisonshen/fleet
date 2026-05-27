package spawn

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// writeCoordConfigRepoIdempotent stamps the resolved repo path into
// ~/.fleet/projects/<project>/coord-config.json under the `repo` key.
//
// Schema (additive — never break existing readers):
//
//	{
//	    "parallelism": <int>,   // loop.py _load_parallelism
//	    "repo":        <str>    // fleet#175 — coord's project checkout
//	}
//
// Idempotency:
//   - file missing → create with {"repo": repo}
//   - file present, `repo` missing or whitespace-only → merge (preserve
//     parallelism + every other field)
//   - file present, `repo` non-empty → leave untouched (operator may
//     have set this to a fork/out-of-tree checkout)
//
// Atomic: write to a tempfile in the same directory, fsync, rename.
// On any non-recoverable error returns it for the caller to log; we
// deliberately do NOT fail the spawn because the worst case is the
// coord skill falls through to its legacy cwd-derived behavior + emits
// a warning into TickResult.errors. A failed write here is a
// breadcrumb, not a blocker.
func writeCoordConfigRepoIdempotent(fleetHome, project, repo string) error {
	if fleetHome == "" || project == "" || repo == "" {
		return errors.New("writeCoordConfigRepoIdempotent: empty input")
	}
	projDir := filepath.Join(fleetHome, "projects", project)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		return fmt.Errorf("mkdir project dir: %w", err)
	}
	cfgPath := filepath.Join(projDir, "coord-config.json")

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

	// Idempotent: preserve existing non-empty repo IFF it still
	// points at a live directory. Stale paths (deleted/moved
	// checkout) get overwritten by the respawn cwd — without this,
	// codex iter-5 P2 surfaces: an operator who hits the iter-5
	// stale-path refuse path follows the on-screen guidance to
	// "re-spawn the coord from inside the correct checkout," and
	// the respawn correctly writes the new cwd here... except
	// idempotency would silently preserve the stale value, leaving
	// the project permanently refusing dispatch. Stat-checking the
	// existing value lets respawn-as-recovery actually recover.
	if existing, ok := data["repo"].(string); ok && strings.TrimSpace(existing) != "" {
		if info, err := os.Stat(existing); err == nil && info.IsDir() {
			return nil
		}
		// Stat failed or not-a-dir → overwrite with the live cwd
		// (the respawn-as-recovery path).
	}
	data["repo"] = repo

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

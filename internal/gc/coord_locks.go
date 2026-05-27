package gc

// coord_locks.go — fifth classifier for `fleet gc`: scans
// `~/.fleet/projects/<project>/.locks/coordinator.lock` files across
// every project and flags three failure modes that the per-project
// coord lifecycle did NOT clean up:
//
//   1. Mismatch — lock body's agent record reports a DIFFERENT project
//      than the lock's parent dir. PRs #173/#174 (fleet#170/#171) now
//      prevent NEW cross-project hijacks at spawn + tick time; this
//      classifier sweeps historical state from BEFORE that fix landed.
//
//   2. Dead coord — lock body references an agent record file under
//      ~/.fleet/agents/<id>.json that's MISSING. The coord crashed or
//      was force-archived without releasing the lock body.
//
//   3. Stale tmux — agent record exists but the tmux session named on
//      the record is GONE. The agent JSON outlived its tmux session
//      (force-kill, machine reboot, etc.) and the lock body hasn't
//      been cleared.
//
// Default behavior: surface (Verb=surface). `--apply` upgrades to
// would-remove / removed (atomic unlink of the offending lock file).
// We never remove the agent record itself — that's orphan-agents'
// job; coord-locks ONLY touches `.locks/coordinator.lock` files.
//
// Out of scope (per task brief):
//   - Worktree cleanup (worktrees classifier).
//   - Inbox file cleanup (separate task).
//   - Auto-cleanup at coord spawn (separate task; PRs #173/#174 already
//     close the NEW-hijack window — this is the historical sweeper).
//   - Modifying the lock acquisition path (PRs #170/#171 cover that).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edisonshen/fleet/internal/state"
)

// KindCoordLocks is the fifth classifier — coordinator.lock sweep.
const KindCoordLocks Kind = "coord-locks"

// CoordLockInfo is the minimal per-lock-file shape the classifier
// needs. Path is the absolute path on disk (used as the Action.Target
// + as the unlink argument). Project is the lock's PARENT-DIR project
// name (the authoritative "what project is this lock for?" answer —
// compared against the loaded agent record's Project field for the
// mismatch detection). HolderID is the 8-hex-char agent ID parsed
// from the lock body's first line; empty when the body is empty,
// torn, or not a valid agent-ID shape.
type CoordLockInfo struct {
	Path     string
	Project  string
	HolderID string
}

// reconcileCoordLocks enumerates every coordinator.lock under
// ~/.fleet/projects/*/.locks/ and runs three checks per file. See
// the package-level comment for the failure modes.
//
// Per-classification flow:
//
//	for each lock file:
//	    if empty / unparseable holder         → skip (legacy zero-byte)
//	    LoadAgent(holder) →
//	        ErrNotFound                       → DEAD-COORD action
//	        other error                       → skip (transient, ambiguous)
//	        ok →
//	            record.Project != lockProject → MISMATCH action
//	            empty TmuxSession             → skip (legacy record)
//	            SessionAlive →
//	                err != nil                → skip (probe failure)
//	                alive=false               → STALE-TMUX action
//	                alive=true                → no action (healthy)
//
// Each action lands in r.Actions with Verb=surface by default; --apply
// upgrades to would-remove → removed.
//
// Project scope (opts.Project): when non-empty, only that project's
// .locks/ dir is scanned.
func reconcileCoordLocks(r *Report, opts Options, deps Deps) error {
	if deps.ListCoordLocks == nil {
		return errors.New("coord-locks: ListCoordLocks dep not wired")
	}
	if deps.LoadAgent == nil {
		return errors.New("coord-locks: LoadAgent dep not wired")
	}
	if deps.SessionAlive == nil {
		return errors.New("coord-locks: SessionAlive dep not wired")
	}
	locks, err := deps.ListCoordLocks()
	if err != nil {
		return err
	}
	// Stable per-classifier order for human-readable output. Reconcile's
	// outer sort re-sorts by Kind then Target, but this keeps the unit
	// tests deterministic regardless of map-iteration order in fakes.
	sort.SliceStable(locks, func(i, j int) bool { return locks[i].Path < locks[j].Path })
	for _, l := range locks {
		if opts.Project != "" && l.Project != opts.Project {
			continue
		}
		holder := strings.TrimSpace(l.HolderID)
		if holder == "" {
			// Empty / torn / unparseable body — surface-don't-silo.
			// A legacy zero-byte coordinator.lock from before issue #55
			// (when the body wasn't populated) is harmless on its own —
			// flock alone is the load-bearing signal. Don't touch.
			continue
		}
		rec, lerr := deps.LoadAgent(holder)
		if lerr != nil {
			if errors.Is(lerr, state.ErrNotFound) {
				appendCoordLockAction(r, opts, deps, l,
					fmt.Sprintf("dead coord (agent record %s missing)", holder))
				continue
			}
			// Transient / parse error — refuse to act on ambiguous state.
			continue
		}
		if rec == nil {
			// Defensive: a nil record without an error is unexpected;
			// treat the same as "transient" and skip.
			continue
		}
		if rec.Project != "" && rec.Project != l.Project {
			appendCoordLockAction(r, opts, deps, l,
				fmt.Sprintf("mismatch (lock dir=%s vs record.project=%s)", l.Project, rec.Project))
			continue
		}
		if strings.TrimSpace(rec.TmuxSession) == "" {
			// Legacy / partially populated record — empty TmuxSession is
			// unprobeable. Mirror reconcileOrphanAgents' conservative
			// guard (don't classify on a probe we can't run).
			continue
		}
		alive, perr := deps.SessionAlive(rec.TmuxSession)
		if perr != nil {
			// Probe failure is ambiguous; surface-don't-silo.
			continue
		}
		if alive {
			continue // healthy coord — leave alone
		}
		appendCoordLockAction(r, opts, deps, l,
			fmt.Sprintf("stale tmux (session %s gone for agent %s)", rec.TmuxSession, holder))
	}
	return nil
}

// appendCoordLockAction builds the per-lock Action (default Verb=surface)
// and, under --apply, calls RemoveCoordLock to unlink the file.
// Failure on remove flips Verb back to surface and the Reason carries
// the unlink error so the operator can re-run with elevated perms.
func appendCoordLockAction(r *Report, opts Options, deps Deps, l CoordLockInfo, reason string) {
	act := Action{
		Kind:   KindCoordLocks,
		Target: l.Path,
		Verb:   VerbSurface,
		Reason: reason + "; surface only — rerun with --apply to unlink",
	}
	if opts.Apply {
		act.Verb = VerbWouldRemove
		act.Reason = reason
		if deps.RemoveCoordLock == nil {
			act.Verb = VerbSurface
			act.Reason = reason + "; --apply requested but RemoveCoordLock dep not wired"
		} else if rerr := deps.RemoveCoordLock(l.Path); rerr != nil {
			act.Verb = VerbSurface
			act.Reason = fmt.Sprintf("%s; remove failed: %v", reason, rerr)
		} else {
			act.Verb = VerbRemoved
		}
	}
	r.Actions = append(r.Actions, act)
}

// listCoordLocksOnDisk is the production ListCoordLocks. Walks every
// project dir under ~/.fleet/projects/ and yields one CoordLockInfo
// per project that has a `.locks/coordinator.lock` file.
//
// Project enumeration mirrors listProjectsOnDisk: directories under
// ~/.fleet/projects/, skipping the reserved `.locks` sibling + any
// names that fail state.ValidateProjectName (legacy / hand-edited
// trees stay out of the sweep).
//
// Lock-body parsing: read up to the first newline; trim CR/LF +
// whitespace; validate 8-hex-char agent-ID shape (Python coord skill's
// _try_lock writes `<id>\n`, see skills/coordinator/loop.py:1828).
// Anything else → HolderID="" and the classifier skips the file
// (legacy zero-byte / hand-edit / torn read).
func listCoordLocksOnDisk() ([]CoordLockInfo, error) {
	root, err := state.Root()
	if err != nil {
		return nil, err
	}
	pdir := filepath.Join(root, "projects")
	entries, err := os.ReadDir(pdir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", pdir, err)
	}
	var out []CoordLockInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == ".locks" {
			continue
		}
		if err := state.ValidateProjectName(name); err != nil {
			continue
		}
		lockPath := filepath.Join(pdir, name, ".locks", "coordinator.lock")
		st, serr := os.Stat(lockPath)
		if serr != nil || st.IsDir() {
			continue
		}
		holder := parseCoordLockHolder(lockPath)
		out = append(out, CoordLockInfo{
			Path:     lockPath,
			Project:  name,
			HolderID: holder,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// parseCoordLockHolder reads coordinator.lock's first line and returns
// the holder agent ID when it shape-matches an 8-lowercase-hex token,
// or "" otherwise. Tolerant of CRLF + trailing whitespace, same as the
// TUI's readCoordHolder (internal/tui/dashboard.go).
//
// Read budget: 64 bytes is more than enough — the body is `<8hex>\n`.
func parseCoordLockHolder(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	var buf [64]byte
	n, _ := f.Read(buf[:])
	if n <= 0 {
		return ""
	}
	line := string(buf[:n])
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if i := strings.IndexByte(line, '\r'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if !isCoordHolderShape(line) {
		return ""
	}
	return line
}

// isCoordHolderShape mirrors internal/tui/dashboard.go:isAgentIDShape —
// duplicated here rather than imported because internal/gc cannot
// depend on internal/tui (gc sits below tui in the import graph; tui
// reads gc.Report for the dashboard's reconciliation summary).
func isCoordHolderShape(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// removeCoordLockFile is the production RemoveCoordLock. Best-effort
// unlink — ENOENT collapses to nil so two operators racing
// `fleet gc --apply` don't see spurious errors.
func removeCoordLockFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

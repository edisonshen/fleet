//go:build linux || darwin

package coordlock

// handoff_journal.go — the write-once handoff journal that replaces the
// epoch's `ReserveHandoff` reservation (DESIGN-coord-lease-flock-only
// Decision 7). It is EPOCH-INDEPENDENT: a small JSON file, created O_EXCL +
// fsynced, that carries "a successor is coming" across the window where the
// OLD coord has released the flock but the successor has not yet acquired it.
//
// It is NOT a lease, NOT a TTL, NOT a heartbeat — nothing here can lapse under
// a live process. The mid-boot / abandoned decision is made by the successor
// PROCESS's liveness (pid + pid_start + boot_id — the same kernel signal the
// flock body uses), never by elapsed time.
//
//	CREATE (strict order, flock release LAST — the producer's job):
//	  1. spawn the successor (a polling standby)
//	  2. O_EXCL-create + fsync coordinator.handoff.json
//	     (successor identity + process handle + barrier_id + respawn spec)
//	  3. only THEN release the flock
//	COMMIT: the winner (whoever acquires the freed flock) deletes the journal
//	  + its barrier on acquire — idempotent; a leftover naming the current
//	  live flock holder is committed-stale ⇒ deletable.
//	ABANDON (a spawn-decision path, flock FREE): journal + successor
//	  process-ALIVE ⇒ hold off; journal + successor process-DEAD ⇒ clear the
//	  stale journal+barrier THEN respawn (clear-before-spawn).
//
// Flock-exclusivity (two processes can never both hold coordinator.flock) is
// the real no-duplicate-COORDINATOR guarantee; the journal only makes the
// common handoff window spawn-free without a timer.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// handoffJournalName is the fixed filename under the project's .locks/ dir.
// A fixed path (not a per-cycle name) is why Create is O_EXCL: a leftover from
// an abandoned cycle must be resolved (committed-stale ⇒ delete; dead ⇒ clear;
// live-not-holding ⇒ wait) before a fresh cycle can Create, never silently
// clobbered.
const handoffJournalName = "coordinator.handoff.json"

// HandoffJournal is the on-disk write-once handoff record. AgentID fields name
// the intended successor; the process handle (SuccessorPID/PidStart/BootID)
// drives the mid-boot-vs-abandoned liveness decision; BarrierID names this
// cycle's drain barrier (rotated every cycle, never reused). The respawn spec
// (primitive fields — coordlock cannot import spawn/agent) lets an Abandon
// recovery re-spawn from the journal alone when the caller has no other record.
type HandoffJournal struct {
	Project     string `json:"project"`
	OldAgentID  string `json:"old_agent_id"`
	SuccessorID string `json:"successor_id"`
	// BarrierID names this handoff cycle's completion barrier
	// (handoff-complete-<BarrierID>.json). Allocated once at Create, cleaned
	// up with the journal on commit/abandon.
	BarrierID string `json:"barrier_id"`
	// Successor process handle — the kernel liveness signal (pid + pid_start
	// + boot_id) that decides mid-boot (alive) vs abandoned (dead). Never a TTL.
	SuccessorPID      int    `json:"successor_pid"`
	SuccessorPidStart int64  `json:"successor_pid_start"`
	SuccessorBootID   string `json:"successor_boot_id"`
	SuccessorSession  string `json:"successor_session,omitempty"`
	// Respawn spec (primitive; optional) — enough to re-spawn on Abandon when
	// the caller has only the journal.
	RespawnCommand []string `json:"respawn_command,omitempty"`
	RespawnCwd     string   `json:"respawn_cwd,omitempty"`
	RespawnDocPath string   `json:"respawn_doc_path,omitempty"`
	StampWall      int64    `json:"stamp_wall"`
}

// ErrHandoffJournalExists is returned by CreateHandoffJournal when a leftover
// journal is already present. The caller MUST resolve the collision (per
// Decision 7 Create: committed-stale ⇒ delete; dead successor ⇒ clear per
// Abandon; live-not-holding ⇒ wait) and retry — never blind-overwrite.
var ErrHandoffJournalExists = errors.New("coordlock: handoff journal already exists")

// HandoffDisposition is a spawn-decision path's verdict for a FLOCK-FREE
// project, derived from the journal by successor PROCESS liveness (never time).
type HandoffDisposition int

const (
	// HandoffNone: no journal — the handoff (if any) is abandoned/clean ⇒
	// the caller Spawns a fresh coord.
	HandoffNone HandoffDisposition = iota
	// HandoffInFlight: a journal names a successor whose process is ALIVE
	// (booting, not yet holding the flock) ⇒ hold off and poll; do NOT spawn
	// beside it.
	HandoffInFlight
	// HandoffAbandoned: a journal names a successor whose process is DEAD
	// (died mid-boot) ⇒ clear the stale journal+barrier FIRST, then respawn.
	HandoffAbandoned
)

func (d HandoffDisposition) String() string {
	switch d {
	case HandoffNone:
		return "none"
	case HandoffInFlight:
		return "in-flight"
	case HandoffAbandoned:
		return "abandoned"
	default:
		return fmt.Sprintf("handoff-disposition(%d)", int(d))
	}
}

func handoffJournalPath(project string) (string, error) {
	paths, err := resolvePaths(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(paths.flock), handoffJournalName), nil
}

// HandoffBarrierPath returns the completion-barrier path for a handoff cycle,
// named by the journal's BarrierID: <project>/.locks/handoff-complete-<id>.json.
// Both the producer (graceful_unix) and the drain verifier resolve it through
// this one helper so they name the SAME file (the epoch used to give them that
// via CurrentEpoch; the persisted BarrierID replaces it). A prior-cycle
// leftover (a different id) is excluded because its filename differs.
func HandoffBarrierPath(project, barrierID string) (string, error) {
	if barrierID == "" {
		return "", fmt.Errorf("coordlock.HandoffBarrierPath: empty barrier id")
	}
	paths, err := resolvePaths(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(paths.flock),
		fmt.Sprintf("handoff-complete-%s.json", barrierID)), nil
}

// NewBarrierID mints a fresh, per-cycle unique barrier id (never reused). The
// pid+nanos shape is unique enough that two handoff cycles never collide.
func NewBarrierID() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

// CreateHandoffJournal writes coordinator.handoff.json O_EXCL and fsyncs it
// durable (Decision 7 Create step 2). It fills StampWall if unset. On a
// pre-existing journal it returns ErrHandoffJournalExists WITHOUT touching the
// file — the caller resolves the collision and retries. The parent dir is
// fsynced so the create survives a crash.
func CreateHandoffJournal(j HandoffJournal) error {
	if j.Project == "" {
		return fmt.Errorf("coordlock.CreateHandoffJournal: project required")
	}
	if j.SuccessorID == "" {
		return fmt.Errorf("coordlock.CreateHandoffJournal: successor id required")
	}
	if j.BarrierID == "" {
		return fmt.Errorf("coordlock.CreateHandoffJournal: barrier id required")
	}
	if j.StampWall == 0 {
		j.StampWall = time.Now().UnixNano()
	}
	path, err := handoffJournalPath(j.Project)
	if err != nil {
		return err
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return fmt.Errorf("coordlock.CreateHandoffJournal: mkdir: %w", mkErr)
	}
	b, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("coordlock.CreateHandoffJournal: marshal: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrHandoffJournalExists
		}
		return fmt.Errorf("coordlock.CreateHandoffJournal: open: %w", err)
	}
	if _, werr := f.Write(b); werr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("coordlock.CreateHandoffJournal: write: %w", werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("coordlock.CreateHandoffJournal: fsync: %w", serr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("coordlock.CreateHandoffJournal: close: %w", cerr)
	}
	// Parent-dir fsync so the O_EXCL create survives a crash/power loss.
	if dir, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// ReadHandoffJournal reads coordinator.handoff.json. ok=false (nil error) when
// the file is absent; a torn/unreadable body is a real error the caller must
// treat conservatively (never delete the queue on ambiguity — D9b).
func ReadHandoffJournal(project string) (HandoffJournal, bool, error) {
	path, err := handoffJournalPath(project)
	if err != nil {
		return HandoffJournal{}, false, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HandoffJournal{}, false, nil
		}
		return HandoffJournal{}, false, fmt.Errorf("coordlock.ReadHandoffJournal: read: %w", err)
	}
	var j HandoffJournal
	if uerr := json.Unmarshal(b, &j); uerr != nil {
		return HandoffJournal{}, false, fmt.Errorf("coordlock.ReadHandoffJournal: unmarshal: %w", uerr)
	}
	return j, true, nil
}

// DeleteHandoffJournal removes the journal AND its barrier (idempotent — a
// missing file is not an error). Used by both Commit (the winner clears it on
// acquire) and Abandon (clear-before-spawn). Best-effort barrier removal: the
// journal is the record of record, the barrier is derived from it.
func DeleteHandoffJournal(project string) error {
	path, err := handoffJournalPath(project)
	if err != nil {
		return err
	}
	// Clean the barrier first, resolved from the journal's own barrier_id, so a
	// journal-then-barrier partial delete never orphans a barrier we can no
	// longer name.
	if j, ok, rerr := ReadHandoffJournal(project); rerr == nil && ok && j.BarrierID != "" {
		if bp, berr := HandoffBarrierPath(project, j.BarrierID); berr == nil {
			_ = os.Remove(bp)
		}
	}
	if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return fmt.Errorf("coordlock.DeleteHandoffJournal: remove: %w", rmErr)
	}
	return nil
}

// HandoffSuccessorAlive reports whether the journal's recorded successor
// process is still the SAME live process (pid + pid_start), same boot. A
// cross-boot journal (BootID mismatch) reads as DEAD (a reboot kills every
// process; pid_start is only comparable within a boot). This is the ONLY
// discriminator for the mid-boot-vs-abandoned decision — never elapsed time.
func HandoffSuccessorAlive(j HandoffJournal) bool {
	return handoffSuccessorAliveWithCfg(j, defaultLeaseConfig())
}

func handoffSuccessorAliveWithCfg(j HandoffJournal, cfg leaseConfig) bool {
	if j.SuccessorPID <= 0 {
		return false
	}
	if j.SuccessorBootID != "" && j.SuccessorBootID != cfg.boot() {
		return false // previous boot -> provably dead
	}
	st, ok := cfg.pidStart(j.SuccessorPID)
	if !ok {
		return false // pid gone
	}
	return st == j.SuccessorPidStart // recycled pid -> start mismatch -> dead
}

// ClassifyFreeFlockHandoff is the spawn-decision helper for a FLOCK-FREE
// project (Decision 7 Abandon). The caller MUST have already established the
// flock is free (a busy flock ⇒ Attach to the live holder, decided by the
// caller before calling this). It reads the journal and decides by successor
// PROCESS liveness:
//
//	no journal            -> HandoffNone      (spawn a fresh coord)
//	journal + succ ALIVE  -> HandoffInFlight  (hold off; poll — no deadline)
//	journal + succ DEAD   -> HandoffAbandoned (clear journal+barrier THEN respawn)
//
// A torn/unreadable journal returns the read error; the caller treats it
// conservatively (do not spawn beside a possibly-in-flight successor).
func ClassifyFreeFlockHandoff(project string) (HandoffDisposition, HandoffJournal, error) {
	return classifyFreeFlockHandoffWithCfg(project, defaultLeaseConfig())
}

func classifyFreeFlockHandoffWithCfg(project string, cfg leaseConfig) (HandoffDisposition, HandoffJournal, error) {
	j, ok, err := ReadHandoffJournal(project)
	if err != nil {
		return HandoffNone, HandoffJournal{}, err
	}
	if !ok {
		return HandoffNone, HandoffJournal{}, nil
	}
	if handoffSuccessorAliveWithCfg(j, cfg) {
		return HandoffInFlight, j, nil
	}
	return HandoffAbandoned, j, nil
}

// CurrentHandoffSuccessor reports the in-flight successor id named by the
// journal when the successor's process is still ALIVE (the flock-only
// replacement for the epoch's CurrentHandoff reservation). ok=false when there
// is no journal, the successor is dead, or the read is torn. Read-only.
func CurrentHandoffSuccessor(project string) (string, bool) {
	j, ok, err := ReadHandoffJournal(project)
	if err != nil || !ok {
		return "", false
	}
	if !HandoffSuccessorAlive(j) {
		return "", false
	}
	return j.SuccessorID, true
}

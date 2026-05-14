package handoffop

// AtomicCoordSwap is the single transactional entry point for replacing
// a project's coordinator. It enforces one invariant: at any observable
// moment, state.ReadCoordSpawnMarker(project) resolves to exactly one
// agent ID, AND that agent's tmux session is alive. OLD tmux sessions
// that survive past the marker rename are *orphan processes*, not
// coords; they are reapable by `fleet maintenance prune-orphan-tmux`.
//
// The marker rename in step 4 is the atomic commit point. Steps 1–3
// (preconditions + spawn + readiness probe) run BEFORE the commit and
// leave no observable change on rollback. Steps 5–6 (kill OLD + archive
// OLD record) run AFTER the commit and are best-effort retire mechanics:
// on failure the marker is still at NEW, the invariant holds, and the
// helper degrades into FAILURE MODE 5 (archive OLD record + inbox alert
// + ErrOrphanSurvived) so the existing prune-orphan-tmux sweeper can
// reap the leaked session on its next pass.
//
// The four covered cases:
//   1. Auto-handoff 50% (Yellow) — fleet-guard MILESTONE.
//   2. Auto-handoff 70% (Red) — fleet-guard hard threshold.
//   3. Manual `fleet handoff <id>` — operator CLI.
//   4. Dead-coord resume via TUI [a] — oldIsDead=true.
//   5. Engine swap — implicit, identical to case 3 plus new Engine.
//
// Worker swap is OUT OF SCOPE for this helper. Worker swap needs task-row
// CompareAndSwap under LockProjectState, not a marker-file rename.
//
// ASCII flow:
//
//                ┌─ preconditions ───────────────────────────┐
//   step 1.a ───►│ flock(swap.lock)                          │
//   step 1.b ───►│ ReadCoordSpawnMarker == oldRec.ID         │
//                └────────┬──────────────────────────────────┘
//   step 2 ─────►  spawn.Spawn(newRecSpec) → NEW alive
//   step 3.a ───►  WaitForReadyToPrompt(NEW)
//   step 3.b ───►  SessionAlive(NEW)  ◄─ ambiguous-error: preserve
//                ┌─ *** ATOMIC COMMIT *** ──────────────────┐
//   step 4 ─────►│ WriteCoordSpawnMarker(project, NEW.ID)   │
//                │   (WriteAtomic w/ parent-dir-fsync)      │
//                └────────┬──────────────────────────────────┘
//   step 5.a ───►  SendKeys(OLD, "/exit"); sleep grace        (skipped if oldIsDead)
//   step 5.b ───►  tmux.Kill(OLD)                             (skipped if oldIsDead)
//   step 5.c ───►  SessionAlive(OLD)  ──── alive=true → FAILURE MODE 5
//   step 6 ─────►  oldRec.Archive()                           (skipped if oldIsDead)
//   step 7 ─────►  release swap.lock; return success

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// DefaultSwapGraceWindow is the time between SendKeys("/exit") and Kill
// on the OLD session. 10s matches the operator-visible grace expectation
// documented in DESIGN-subagent-lifecycle.md.
const DefaultSwapGraceWindow = 10 * time.Second

// ErrConcurrentSwap is returned when step 1.b's marker probe reads a
// value other than oldRec.ID — another swap raced us and already
// committed. The wrapping caller's correct response is "back off; the
// other swap is the source of truth now."
var ErrConcurrentSwap = errors.New("atomic coord swap: another swap raced us; marker already moved")

// ErrSwapInProgress is reserved for non-blocking flock acquisition. The
// current implementation uses a blocking flock so callers don't see
// this; the constant is here so a future LOCK_NB path can return it
// without an API break.
var ErrSwapInProgress = errors.New("atomic coord swap: another swap holds the lock")

// ErrOrphanSurvived is returned when step 5.c reports OLD's tmux
// session is STILL alive after the kill (FAILURE MODE 5). The marker
// commit has already succeeded, so the invariant holds — NEW owns
// per the marker. OLD's tmux session is now an orphan process,
// archived from the records side, and reapable by `fleet maintenance
// prune-orphan-tmux`. The wrapping caller surfaces this to the operator
// via a red banner / inbox alert.
//
// The string includes "orphan tmux" so operator log greps key on it the
// same way they grep for "still alive" in the pre-AtomicCoordSwap world.
type ErrOrphanSurvived struct {
	OldSession string
	NewAgentID string
	Project    string
	Inner      error
}

func (e *ErrOrphanSurvived) Error() string {
	return fmt.Sprintf(
		"atomic coord swap: marker committed to %s but OLD tmux session %s for project %q survived kill (orphan tmux session; prune-orphan-tmux will reap on next sweep): %v",
		e.NewAgentID, e.OldSession, e.Project, e.Inner)
}

// Is allows errors.Is(err, ErrOrphanSurvivedSentinel) without callers
// having to type-assert the struct. The sentinel is a separate package-
// level value so callers can match on the class.
func (e *ErrOrphanSurvived) Is(target error) bool {
	return target == ErrOrphanSurvivedSentinel
}

// ErrOrphanSurvivedSentinel is the sentinel for errors.Is matching
// against any *ErrOrphanSurvived value.
var ErrOrphanSurvivedSentinel = errors.New("atomic coord swap: orphan survived")

// AtomicCoordSwapInputs collects the parameters for one swap. Mirrors
// the plan's INPUTS block.
type AtomicCoordSwapInputs struct {
	// Project — used to locate coord-spawn marker + swap.lock.
	Project string

	// OldRec — current coord record (may be alive or proven-dead).
	OldRec *agent.Record

	// NewRecSpec — spawn.Options for the replacement. The helper does
	// not mutate this; PreAllocatedID, OldRecord, NewDocPath, etc. are
	// the caller's responsibility.
	//
	// Ignored when AlreadySpawnedNewRec is non-nil.
	NewRecSpec spawn.Options

	// AlreadySpawnedNewRec, when non-nil, instructs the helper to
	// SKIP steps 2-3 (spawn + readiness probe) and proceed directly
	// to step 4 (marker commit) using this record. This is the entry
	// point for callers that already wrote a crash-recovery queue
	// journal and ran their own spawn before invoking the helper
	// (e.g. `fleet handoff` and the auto-drain path, which pre-allocate
	// IDs and journal them BEFORE spawning).
	//
	// When set, the caller is responsible for having already:
	//   - verified the NEW tmux session is alive (step 3.b equivalent),
	//   - written the NEW agent record to disk,
	//   - waited for the pane to stabilize if applicable.
	// The helper still runs step 1 (precondition + flock) before the
	// commit so the marker invariant is preserved.
	AlreadySpawnedNewRec *agent.Record

	// OldIsDead — caller asserts oldRec.session is gone (case 4 only).
	// When true, steps 5 + 6 are SKIPPED entirely — no kill, no
	// archive. Operator clears OLD via TUI [x]. This matches
	// cmd/fleet/dispatch.go's existing dead-coord-resume semantics
	// (see commentary at line 709 onward).
	OldIsDead bool

	// GraceWindow — duration between SendKeys("/exit") and Kill on
	// OLD. Default DefaultSwapGraceWindow (10s) if zero. Ignored when
	// OldIsDead=true.
	GraceWindow time.Duration
}

// AtomicCoordSwapResult carries what the caller needs after a successful
// swap: the new record (spawn-resolved), whether OLD was archived (false
// for dead-old case 4), and any non-fatal warnings emitted during step
// 5/6 (so the caller's UI can render them as banners).
type AtomicCoordSwapResult struct {
	NewRec       *agent.Record
	OldArchived  bool
	OrphanedKill bool // FAILURE MODE 5 hit but invariant held; ErrOrphanSurvived also returned
}

// AtomicCoordSwap executes the 7-step contract above. The marker rename
// is the single atomic commit point.
//
// Returns:
//
//   - on success: result populated, err == nil. Marker is at NEW, NEW is
//     alive. If OldIsDead=false, OLD is killed and archived. If
//     OldIsDead=true, OLD's record is preserved (operator clears via [x]).
//
//   - on rollback (steps 1–3 fail): err describes the failure. Marker
//     unchanged, OLD remains the coord. NEW (if spawned in step 2) is
//     killed + record removed before return.
//
//   - on FAILURE MODE 5 (step 5.c reports OLD still alive after kill):
//     returns *ErrOrphanSurvived with the OLD session embedded. Marker
//     is at NEW (committed). OLD's record IS archived (so prune-orphan-tmux
//     reaps the session on its next sweep). An inbox alert is dropped at
//     ~/.fleet/inbox/<NEW.ID>.md.
//
// Caller is responsible for higher-level concerns: handoff doc, queue
// journal, prompt delivery, retire-aware status output. The helper owns
// only the atomic owner-flip + retire bookkeeping.
//
// stderr is for the [P0] FAILURE MODE 5 log line. nil-tolerant.
func AtomicCoordSwap(in AtomicCoordSwapInputs, stderr io.Writer) (AtomicCoordSwapResult, error) {
	res := AtomicCoordSwapResult{}

	if in.Project == "" {
		return res, errors.New("atomic coord swap: Project is required")
	}
	if in.OldRec == nil {
		return res, errors.New("atomic coord swap: OldRec is required")
	}

	graceWindow := in.GraceWindow
	if graceWindow <= 0 {
		graceWindow = DefaultSwapGraceWindow
	}

	// Step 1.a — acquire swap.lock on this project.
	release, lerr := acquireSwapLock(in.Project)
	if lerr != nil {
		return res, fmt.Errorf("atomic coord swap: acquire swap.lock: %w", lerr)
	}
	defer release()

	// Step 1.b — verify the marker still points at OLD. If something
	// else moved it (concurrent swap, manual operator override), abort
	// without spawning. The lock makes this race the narrow window
	// between OUR acquire and OUR read; in practice another swap that
	// completed before us will have re-pointed the marker.
	cur := state.ReadCoordSpawnMarker(in.Project)
	if cur != in.OldRec.ID {
		return res, fmt.Errorf("%w (project=%q expected=%s observed=%s)",
			ErrConcurrentSwap, in.Project, in.OldRec.ID, cur)
	}

	// Steps 2-3 — spawn the replacement + readiness probe. Skipped
	// when the caller pre-spawned (AlreadySpawnedNewRec set), in which
	// case the caller is responsible for asserting the same liveness
	// invariant before invoking the helper.
	var newRec *agent.Record
	if in.AlreadySpawnedNewRec != nil {
		newRec = in.AlreadySpawnedNewRec
	} else {
		// Step 2 — spawn. spawn.Spawn rolls back its own state.json
		// write failure (kills the orphan tmux session before
		// returning). On error here the marker is still at OLD and no
		// observable change has happened.
		spawned, serr := spawnFn(in.NewRecSpec)
		if serr != nil {
			return res, fmt.Errorf("atomic coord swap: spawn replacement: %w", serr)
		}
		newRec = spawned

		// Step 3.a — wait for NEW's pane to stabilize.
		// spawn.WaitForReadyToPrompt is best-effort: convergence
		// failure (didn't stabilize in MaxWait) returns an error but
		// the pane may still be usable. We don't fail the swap on
		// convergence failure alone — Step 3.b's tristate
		// SessionAlive probe is the authoritative liveness gate.
		if werr := waitForReadyToPromptFn(newRec.TmuxSession); werr != nil {
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: atomic coord swap: readiness poll for %s did not converge: %v (probe-checking liveness)\n",
					newRec.TmuxSession, werr)
			}
		}

		// Step 3.b — re-probe NEW after the readiness wait. Catches
		// the "NEW dies during boot" window. The tristate is critical:
		//
		//   - alive=true,  err=nil → continue.
		//   - alive=false, err=nil → definitively dead → roll back NEW.
		//   - alive=false, err!=nil → probe ambiguous → preserve NEW
		//     record + tmux for operator inspection (no rollback).
		switch alive, perr := sessionAliveFn(newRec.TmuxSession); {
		case perr != nil:
			return res, fmt.Errorf(
				"atomic coord swap: probe replacement %s session %s failed: %w (NEW record preserved for operator inspection; marker unchanged at %s)",
				newRec.ID, newRec.TmuxSession, perr, in.OldRec.ID)
		case !alive:
			if dropErr := DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
				return res, fmt.Errorf(
					"atomic coord swap: replacement %s tmux session %s exited during readiness wait AND cleanup failed: %w (marker unchanged at %s; investigate)",
					newRec.ID, newRec.TmuxSession, dropErr, in.OldRec.ID)
			}
			return res, fmt.Errorf(
				"atomic coord swap: replacement %s tmux session %s exited during readiness wait; marker unchanged at %s",
				newRec.ID, newRec.TmuxSession, in.OldRec.ID)
		}
	}
	res.NewRec = newRec

	// Step 4 — *** ATOMIC COMMIT POINT ***
	// WriteCoordSpawnMarker uses WriteAtomic, which (after commit 1)
	// fsyncs the parent directory after rename. Crash safety: a kernel
	// panic that lands here either leaves the marker at OLD (rename
	// not durable yet — operator's `fleet rm <NEW>` resolves) or at
	// NEW (rename durable — crash window B/C, queued-vs-not split per
	// the plan).
	if werr := writeCoordSpawnMarkerFn(in.Project, newRec.ID); werr != nil {
		// Marker write failed. NEW is alive but not declared as coord.
		// Roll back NEW — kill + remove record — and surface error.
		if dropErr := DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
			return res, fmt.Errorf(
				"atomic coord swap: marker write failed (%w) AND replacement cleanup failed (%w); marker unchanged at %s but NEW session %s may be live — investigate",
				werr, dropErr, in.OldRec.ID, newRec.TmuxSession)
		}
		return res, fmt.Errorf(
			"atomic coord swap: marker write failed: %w (replacement rolled back; marker unchanged at %s)",
			werr, in.OldRec.ID)
	}

	// Step 5 — retire OLD. Asymmetric on OldIsDead.
	//
	// OldIsDead=true (case 4 — dead-coord resume): SKIP kill entirely.
	// Cross-socket probe ambiguity means we can't be 100% sure OLD is
	// gone, and operator's already-told-us-it's-dead has to be enough.
	// SKIPPING kill avoids racing a possibly-alive OLD on another socket
	// and yanking a healthy coord. Marker commit in step 4 is the
	// authoritative source of truth either way.
	//
	// OldIsDead=false: normal /exit + grace + Kill + post-probe.
	//
	// On FAILURE MODE 5 the helper returns *ErrOrphanSurvived directly
	// from inside the switch, so we don't need an "orphaned" guard to
	// suppress step 6 — the early-return handles it.
	if !in.OldIsDead {
		// 5.a — graceful exit hint. ErrNoSession means OLD already
		// died between our marker write and now; not fatal.
		if serr := tmuxSendKeysFn(in.OldRec.TmuxSession, "/exit", "Enter"); serr != nil &&
			!errors.Is(serr, tmux.ErrNoSession) {
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: atomic coord swap: send-keys /exit to %s: %v\n",
					in.OldRec.TmuxSession, serr)
			}
		}
		if graceWindow > 0 {
			sleepFn(graceWindow)
		}
		// 5.b — Kill is idempotent (tmux.Kill returns nil for already-
		// dead sessions). On error we drop into 5.c to disambiguate.
		killErr := tmuxKillFn(in.OldRec.TmuxSession)

		// 5.c — confirm dead.
		alive, perr := sessionAliveFn(in.OldRec.TmuxSession)
		switch {
		case perr != nil:
			// Probe ambiguous. Marker is already committed; going
			// forward is safer than backward. Log a warning and
			// proceed to archive (step 6). The plan calls this "[P1]
			// warning; treat as probably dead but uncertain."
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"[P1] atomic coord swap: post-kill probe for OLD %s ambiguous: %v (marker already committed to %s; proceeding with archive)\n",
					in.OldRec.TmuxSession, perr, newRec.ID)
			}
		case alive:
			// FAILURE MODE 5. Marker is at NEW (committed in step 4).
			// OLD's tmux session is an orphan. Archive OLD's record so
			// prune-orphan-tmux can reap; drop inbox alert; log [P0];
			// return ErrOrphanSurvived. Invariant still holds.
			if arcErr := in.OldRec.Archive(); arcErr != nil {
				// Archive failed too. Fall back to deleting the live
				// record so prune-orphan-tmux's "session exists, no
				// live record" check can still find the orphan. If
				// even Remove fails, log it — operator triage from here.
				path, perr := state.AgentPath(in.OldRec.ID)
				if perr == nil {
					if rmErr := os.Remove(path); rmErr != nil {
						if stderr != nil {
							_, _ = fmt.Fprintf(stderr,
								"[P0] atomic coord swap: FAILURE MODE 5 cleanup: archive %s failed (%v) AND remove failed (%v); orphan tmux %s for project %s — operator: manual cleanup required\n",
								in.OldRec.ID, arcErr, rmErr, in.OldRec.TmuxSession, in.Project)
						}
					}
				}
			}
			if alertErr := dropOrphanInboxAlert(newRec.ID, in.OldRec.TmuxSession, in.Project); alertErr != nil {
				if stderr != nil {
					_, _ = fmt.Fprintf(stderr,
						"warning: atomic coord swap: drop orphan inbox alert at %s: %v\n",
						newRec.ID, alertErr)
				}
			}
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"[P0] atomic coord swap: FAILURE MODE 5: marker committed to %s for project %s but OLD tmux session %s survived kill (kill_err=%v); OLD record archived; orphan reapable by `fleet maintenance prune-orphan-tmux`\n",
					newRec.ID, in.Project, in.OldRec.TmuxSession, killErr)
			}
			res.OrphanedKill = true
			return res, &ErrOrphanSurvived{
				OldSession: in.OldRec.TmuxSession,
				NewAgentID: newRec.ID,
				Project:    in.Project,
				Inner:      killErr,
			}
		default:
			// alive=false, err=nil — OLD is genuinely dead. Continue
			// to step 6 (archive). If killErr was non-nil, it was a
			// race (operator killed manually, OS reaped) — note to
			// stderr for log correlation.
			if killErr != nil && stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"note: atomic coord swap: kill %s reported error but session is gone: %v\n",
					in.OldRec.TmuxSession, killErr)
			}
		}
	}

	// Step 6 — archive OLD record. SKIPPED on OldIsDead=true (operator
	// clears via TUI [x]; cross-socket ambiguity means we can't be sure
	// archiving is safe). FAILURE MODE 5 already returned earlier and
	// archived OLD there.
	if !in.OldIsDead {
		if arcErr := in.OldRec.Archive(); arcErr != nil {
			// Kill is confirmed dead in step 5. Falling back to
			// os.Remove on the live record keeps `fleet status` from
			// showing a stale entry. If even Remove fails, return
			// error — the live record is stuck and a retry would
			// load it as the new "coord OLD."
			path, perr := state.AgentPath(in.OldRec.ID)
			if perr != nil {
				return res, fmt.Errorf(
					"atomic coord swap: archive OLD %s failed (%w) AND could not resolve live path (%w); marker at NEW=%s",
					in.OldRec.ID, arcErr, perr, newRec.ID)
			}
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return res, fmt.Errorf(
					"atomic coord swap: archive OLD %s failed (%w) AND remove failed (%w); marker at NEW=%s but OLD record stuck — clean up agents/%s.json manually",
					in.OldRec.ID, arcErr, rmErr, newRec.ID, in.OldRec.ID)
			}
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: atomic coord swap: archive %s: %v (live record removed instead, archive copy lost)\n",
					in.OldRec.ID, arcErr)
			}
		}
		// res.OldArchived reflects "OLD record is no longer at the
		// live agents/ path", true whether Archive succeeded or the
		// os.Remove fallback ran. Callers use this to decide whether
		// `fleet status` will still show OLD.
		res.OldArchived = true
	}

	// Step 7 — defer'd release() will run. Return.
	return res, nil
}

// acquireSwapLock takes a project-scoped flock at
// ~/.fleet/projects/<project>/.locks/swap.lock and returns a release
// function. The .locks/ dir is lazily MkdirAll'd to support the
// fresh-project case (matches LockProjectState's pattern).
//
// Held for the lifetime of one swap. Other swap attempts for the same
// project block until release. Different projects proceed in parallel.
//
// Process-scoped flock; goroutines in the same process do NOT serialize
// via this. Matches the v0.1 flock semantics in state.LockProject.
func acquireSwapLock(project string) (func(), error) {
	path, err := swapLockPath(project)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir swap.lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open swap.lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock swap.lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// swapLockPath returns ~/.fleet/projects/<project>/.locks/swap.lock.
//
// Distinct from LockProject's lock path (~/.fleet/projects/.locks/<name>.lock)
// and from LockProjectState's path (~/.fleet/projects/<name>/.locks/state.lock).
// AtomicCoordSwap intentionally uses its own lock so concurrent
// `fleet handoff <coord>` and `fleet tasks set` operations on the same
// project don't serialize against each other — only same-project SWAP
// attempts serialize.
func swapLockPath(project string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(dir), ".locks", "swap.lock"), nil
}

// dropOrphanInboxAlert writes a structured alert at ~/.fleet/inbox/<newID>.md
// so the operator's fleet-guard inbox-relay path surfaces the orphan
// banner. Best-effort — the FAILURE MODE 5 stderr log is the load-bearing
// alert channel; this is for the TUI side.
func dropOrphanInboxAlert(newID, oldSession, project string) error {
	root, err := state.Root()
	if err != nil {
		return err
	}
	path := filepath.Join(root, "inbox", newID+".md")
	body := fmt.Sprintf(
		"# coord-swap orphan tmux alert\n\n"+
			"coord-swap left orphan tmux session `%s` for project `%s`.\n"+
			"`fleet maintenance prune-orphan-tmux` will reap on its next sweep.\n"+
			"You can also reap manually via `tmux kill-session -t %s` if you have access to the right socket.\n",
		oldSession, project, oldSession)
	return state.WriteAtomic(path, []byte(body))
}

// Package-level seams for unit-testable dependency injection. Production
// values point at the real subsystems; tests override per-case to
// simulate failure modes without spinning up real tmux / spawn.
//
// These are vars (not interface arguments) so the caller's signature
// stays single-method. The trade-off: tests must save+restore the
// pre-test value in t.Cleanup. The handoffop package's existing
// tmuxKillFn / tmuxSessionAliveFn pattern (in replacement_cleanup.go)
// uses the same seam approach.
var (
	spawnFn                 = spawn.Spawn
	waitForReadyToPromptFn  = spawn.WaitForReadyToPrompt
	sessionAliveFn          = tmux.SessionAlive
	writeCoordSpawnMarkerFn = state.WriteCoordSpawnMarker
	tmuxSendKeysFn          = tmux.SendKeys
	sleepFn                 = time.Sleep
	// tmuxKillFn is declared in replacement_cleanup.go; we reuse it.
)

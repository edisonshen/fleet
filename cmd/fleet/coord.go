// fleet coord-run is the Go-level supervisor wrapper for a coord
// session. It owns the three coord-exit paths the design calls out:
// signal, clean exit, and panic. Whatever happens to the child, the
// top-level `defer coord.Cleanup(...)` runs.
//
// See docs/DESIGN-cleanup-fleet-owns-resources.md §PR-C for design.
//
// Usage:
//
//	fleet coord-run --agent <id> --project <p> -- <child-cmd> [args...]
//
// Production wiring: on lease-capable platforms dispatch wraps the engine
// argv in this supervisor:
// ["fleet","coord-run","--agent",<id>,"--project",<p>,"--",
// "sh","-c","claude ..."]. The supervisor then ACQUIRES + HEARTBEATS the
// coordinator lease for the coord's whole life, stands down (exit 0) if a
// healthy leader already holds it, releases the lease on EVERY exit path
// (alongside coord.Cleanup), and — on a contested acquire — reaps the stale
// holder via the authenticated internal/coord.KillCoordIfIdentityMatches
// STONITH.
//
// PR-C originally introduced this subcommand without wiring it in; PR2
// closed that gap so the lease has a real lifetime holder.
//
// Exit-path matrix (per task plan acceptance criteria):
//
//	Child exits cleanly (status 0)       → runCoordRun returns nil
//	Child exits non-zero                  → returns *exec.ExitError
//	Parent receives SIGTERM/SIGINT        → ctx.Done() fires, child gets
//	                                        SIGTERM via CommandContext,
//	                                        wait returns, function returns
//	Internal panic (e.g. opts validation) → propagated AFTER cleanup
//	                                        runs via defer/recover
//
// In ALL paths above, coord.Cleanup fires exactly once. The top-level
// defer is panic-safe because Cleanup itself runs each step inside its
// own recover (see internal/coord/cleanup.go).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	fleet "github.com/edisonshen/fleet"
	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coord"
	"github.com/edisonshen/fleet/internal/fleetlog"
	"github.com/edisonshen/fleet/internal/install"
	"github.com/edisonshen/fleet/internal/tmux"
)

// coordRunOpts holds the resolved flags + injection hooks for one
// invocation of `fleet coord-run`. Exported via the unexported struct
// shape so cmd/fleet/coord_run_test.go can build instances directly
// without going through cobra; production callers go through
// newCoordRunCmd which resolves the cobra flags into this struct.
type coordRunOpts struct {
	agentID string
	project string
	// session is the tmux session name to kill on exit. Production
	// passes the canonical tmux.SessionName(agentID); tests inject
	// arbitrary strings to verify the killer dep is called with the
	// expected value.
	session string
	// argv is the child command to spawn. Production passes the
	// engine argv (e.g. ["sh", "-c", "claude ..."]); tests pass
	// ["sleep","30"] / ["true"].
	argv []string
	// killTmux overrides the production tmux.Kill so tests don't shell
	// out to real tmux + can panic to exercise the per-step defer
	// recover in coord.Cleanup. nil = production (tmux.Kill).
	killTmux func(string) error
	// panicAfterStart, when true, makes runCoordRun panic immediately
	// after the child has been launched but before Wait. Test-only
	// hook used by TestCoordRun_PanicViaDefer to exercise the
	// top-level defer-cleanup contract. Has no production toggle.
	panicAfterStart bool
	// notifyReady is invoked AFTER signal.NotifyContext has installed
	// the SIGTERM/SIGINT handler in notifyCoordRun. Test-only —
	// production callers leave it nil. Used by the real-OS-signal test
	// to synchronize "send the kill" with "handler is wired up".
	notifyReady func()
	// standby marks this supervisor as a WARM-STANDBY coord
	// (DESIGN-handoff-drain-storm-leak §3(A)/§3(B), PR3). A normal coord
	// stands down + exits 0 when a healthy leader already holds the lease;
	// a standby instead POLLS — re-acquiring (LOCK_NB, which internally
	// runs TakeOver on a hung leader) every standbyPoll until it acquires
	// the lease (the old leader exited / was taken over) or ctx is
	// canceled. Only AFTER acquiring does it start the engine child. This
	// is the receiving half of a graceful handoff: the old coord spawns
	// one standby in the background, then retires; the standby's next poll
	// acquires the kernel-released flock and becomes leader.
	standby bool
	// standbyPoll is the poll interval the standby loop waits between
	// re-acquire attempts on a busy lease. 0 = production default
	// (defaultStandbyPoll). Test-only override so the poll loop is
	// exercised without a wall-clock wait.
	standbyPoll time.Duration
	// standbyTimeout bounds how long a standby polls behind a healthy
	// leader before self-exiting cleanly. 0 = production default
	// (defaultStandbyTimeout).
	standbyTimeout time.Duration
	// standbyTimeoutC is a test-only timeout channel override so timeout
	// behavior can be exercised deterministically without sleeping.
	standbyTimeoutC <-chan time.Time
	// standbyOnAcquired, when non-nil, is invoked once the standby poll
	// loop acquires the lease (after a busy period). Test-only seam to
	// assert the loop converged without scraping stderr.
	standbyOnAcquired func()

	// --- lease wiring (DESIGN-handoff-drain-storm-leak PR2) ---
	//
	// acquireLease is the lease-acquire seam. nil = production
	// (productionAcquireLease, which calls coordlock.AcquireLease + stamps the
	// supervisor identity + clears the handoff journal + runs the new-leader
	// sweep). Tests inject a stub that returns a fakeLease + an acquired flag
	// without touching a real flock.
	//
	// Contract (mirrors coordlock.AcquireLease):
	//   acquired=true,  lease!=nil  -> we hold the flock; we ARE the coordinator.
	//       runCoordRun registers lease.Release() in the cleanup defer stack,
	//       then starts the child (no Activate flip, no Heartbeat — D1).
	//   acquired=false              -> a live holder owns the flock; runCoordRun
	//       stands down (prints, returns nil, NEVER starts the child) — a standby
	//       instead polls.
	//   err!=nil with unsupported-platform sentinel -> runCoordRun skips all
	//       lease behavior and runs the legacy bare-child path.
	//
	// The []liveHolderInfo return is retained (always nil post-PR-2 — the
	// takeover/KP6 detection machinery is gone with the epoch write path) so the
	// standby poll loop's signature is stable; reportDetection(nil) is a no-op.
	acquireLease func() (coordLease, bool, []liveHolderInfo, error)

	// onStandDown, when non-nil, is invoked instead of the default
	// stderr print when acquired==false. Test-only seam so a stand-down
	// test can assert the path was taken without scraping stderr.
	onStandDown func()
}

// coordLease is the minimal lease surface runCoordRun needs. The real
// *coordlock.Lease satisfies it; tests inject a fake so the lifetime
// (acquire -> release on every exit path) is exercised without a real kernel
// flock. PR-2 (D1) collapsed it to Release() only: acquiring the flock IS
// becoming the coordinator — there is no starting->active flip (Activate) and
// no TTL to renew (Heartbeat). A live process holding the flock is the coord;
// the kernel releases the flock the instant it dies.
type coordLease interface {
	Release()
}

// liveHolderInfo is the platform-neutral projection of one LIVE stale
// lease holder detected — and deliberately NOT killed — by the takeover's
// KP6 pre-fence gate (DESIGN-coord-no-auto-kill). coord.go compiles on
// every GOOS, so this local type (not coordlock.LiveHolder, which is
// linux||darwin-gated) is what the acquire seam and the drain TakeOver
// seam carry; the unix wiring converts. Detection is REPORT-ONLY: the
// standby poll loop quarantines (stderr + fleetlog) and keeps polling.
type liveHolderInfo struct {
	pid      int
	pidStart int64
	agentID  string
}

// withReady is a tiny helper for the real-OS-signal test that returns
// a copy of opts with notifyReady set. Inline helper rather than a
// method on the struct so the field doesn't need test-only export.
func (o coordRunOpts) withReady(f func()) coordRunOpts {
	o.notifyReady = f
	return o
}

// defaultStandbyPoll is how long a warm-standby coord waits between
// re-acquire attempts while the lease is held by a healthy leader
// (DESIGN-handoff-drain-storm-leak §3(A): "~2-3s"). Var (not const) so a
// test can shrink it; the cobra flag does not expose it (an operator never
// tunes the poll cadence).
var defaultStandbyPoll = 2 * time.Second

var defaultStandbyTimeout = 10 * time.Minute

var errStandbyTimedOut = errors.New("coord-run: standby timed out")

// newCoordRunCmd builds the cobra subcommand for `fleet coord-run`.
// Registered from cmd/fleet/main.go's newRootCmd.
func newCoordRunCmd() *cobra.Command {
	var (
		agentID        string
		project        string
		standby        bool
		standbyTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "coord-run -- <cmd> [args...]",
		Short: "Run a coord child process with guaranteed cleanup on exit",
		Long: `Supervise a coord child (claude in production). On every exit
path — clean exit, signal-killed, internal panic — the supervisor
reaps the coord's own tmux session, archives its agent record to
~/.fleet/agents/archive/<id>-<UTC>.json, and releases the project's
coordinator lease if this coord still holds it.

Required: --agent <id>, --project <name>, and a child argv after --.`,
		// Use Args=ArbitraryArgs so anything after the flags is passed to
		// the child verbatim. cobra normally treats unknown flags after
		// the first positional as errors; the `--` separator + ArbitraryArgs
		// is the established pattern for "the rest is for the child."
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if agentID == "" {
				return errors.New("coord-run: --agent is required")
			}
			if project == "" {
				return errors.New("coord-run: --project is required")
			}
			if len(args) == 0 {
				return errors.New("coord-run: child argv required (pass after `--`)")
			}
			opts := coordRunOpts{
				agentID:        agentID,
				project:        project,
				session:        tmux.SessionName(agentID),
				argv:           args,
				standby:        standby,
				standbyTimeout: standbyTimeout,
			}
			return notifyCoordRun(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&agentID, "agent", "",
		"agent ID this coord belongs to (REQUIRED; matches ~/.fleet/agents/<id>.json)")
	cmd.Flags().StringVar(&project, "project", "",
		"project name (REQUIRED; the project whose coordinator lease we own)")
	cmd.Flags().BoolVar(&standby, "standby", false,
		"run as a WARM-STANDBY coord: on a busy lease, POLL until the leader exits "+
			"(graceful handoff) instead of standing down + exiting")
	cmd.Flags().DurationVar(&standbyTimeout, "standby-timeout", defaultStandbyTimeout,
		"maximum time a standby coord polls behind a healthy leader before exiting cleanly")
	return cmd
}

// notifyCoordRun wraps runCoordRun with signal.NotifyContext for
// SIGTERM + SIGINT. This is the production entry point — the cobra
// RunE forwards to it directly. Split out so tests can exercise
// runCoordRun in isolation (with their own context) AND the real
// signal-handler path end-to-end.
//
// SIGTERM + SIGINT + SIGHUP are the three operator-issued kill signals
// fleet cares about:
//   - SIGHUP is what tmux sends to the pane's foreground process when
//     the pane / window / session is torn down (`tmux kill-window`,
//     `tmux kill-session`, `tmux kill-server`, or the operator detaches
//     a session whose `remain-on-exit` is off and the last process is
//     reaped). Without trapping SIGHUP, the Go default action is to
//     terminate WITHOUT running defers — exactly the bug the cleanup
//     defer is supposed to fix. Trap it explicitly. (Codex iter-1 P1.)
//   - SIGTERM is what `kill <pid>` / `tmux kill-session -t <name>`
//     (when the session has no tty client) sends to the foreground
//     process; standard graceful-kill signal.
//   - SIGINT is Ctrl-C inside an attached session.
//
// All three cancel the child's context, which exec.CommandContext
// translates to SIGKILL on the child after a short grace period
// (cancel-the-cmd, not the process directly — that gives the child a
// moment to flush before the kernel reaps it).
func notifyCoordRun(opts coordRunOpts, stdout, stderr io.Writer) error {
	// Surface-dont-silo: a coord running on a stale skill COPY is exactly
	// how a merged P0 fix gets silently neutered (#182). Shout to stderr
	// with a remediation command BEFORE the child starts — best-effort, it
	// never blocks the spawn. Resolves ~/.claude itself; on resolution
	// failure WarnIfStale's Status call simply finds nothing and stays
	// quiet.
	if home, herr := os.UserHomeDir(); herr == nil {
		_ = install.WarnIfStale(stderr,
			filepath.Join(home, ".claude"), fleet.SkillFS())
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer cancel()
	// Synchronization hook for tests that need to know the handler is
	// installed before they send a signal. Production leaves nil.
	if opts.notifyReady != nil {
		opts.notifyReady()
	}
	return runCoordRun(ctx, opts, stdout, stderr)
}

// runCoordRun is the inner exec + wait loop. The top-level
// `defer coord.Cleanup(...)` is the load-bearing contract: regardless
// of how the function returns (normal, error, panic), cleanup runs.
//
// We don't recover() the panic ourselves — re-panic AFTER cleanup so
// the operator sees the original stack. coord.Cleanup is panic-safe
// internally (per-step recover) so a buggy dep can't take cleanup
// down; we propagate panics from runCoordRun's own logic (option
// validation, exec.Start failures the caller didn't expect) so they
// surface as crashes rather than getting silently swallowed.
//
// ASCII flow:
//
//	┌──────────────────────────────┐
//	│ defer coord.Cleanup(...)     │ ← runs LAST regardless of exit path
//	└──────────────────────────────┘
//	           │
//	           ▼
//	┌──────────────────────────────┐
//	│ exec.CommandContext(ctx)     │ ← ctx cancellation = SIGTERM to child
//	└──────────────────────────────┘
//	           │
//	           ▼
//	┌──────────────────────────────┐
//	│ cmd.Start() → cmd.Wait()     │ ← blocks; returns when child exits
//	└──────────────────────────────┘
//	           │
//	           ▼  (returns the child's exit error verbatim)
//	┌──────────────────────────────┐
//	│ defer cleanup fires HERE     │
//	└──────────────────────────────┘
func runCoordRun(ctx context.Context, opts coordRunOpts, stdout, stderr io.Writer) (err error) {
	deps := coord.Deps{
		KillTmux: opts.killTmux,
		Stderr:   stderr,
	}
	// The single, mandatory cleanup hook. Runs LAST on every exit path
	// — normal return, child-error return, panic. coord.Cleanup is
	// internally panic-safe so this defer never re-panics.
	defer coord.Cleanup(opts.agentID, opts.project, deps) //nolint:errcheck // best-effort

	// exitNote refines the supervisor.exit reason on the nil-return paths
	// where the supervisor never became the coordinator / never started the
	// child (stand-down, standby timeout, standby cancel-before-acquire). Left
	// empty those exits are indistinguishable from a normal clean child exit,
	// so duplicate-start and failed-standby incidents get mislabeled in the
	// very signal this feature adds (codex P3). Empty => a real
	// coordinator/child lifecycle exit.
	var exitNote string

	// supervisor.exit (DESIGN-coord-lease-false-fence-prevention piece 2):
	// emit the supervisor's own exit from a defer so it fires on EVERY exit
	// path (clean, child-error, acquire fault, PANIC, stand-down) with the
	// reason + exit code. child_rc is the claude child's exit code when the
	// return is an *exec.ExitError (-1 when unknown: a non-child fault, a
	// clean/stand-down exit, or a panic). A panic unwind leaves the named
	// `err` nil, so recover() here is the ONLY way to record the crash as a
	// crash (rc=2, reason=panic:<v>) rather than a spurious clean exit — the
	// exact incident this feature exists to catch. We re-panic after logging
	// so cleanup (the earlier defer) still runs on the continued unwind and
	// the operator sees the original stack. Best-effort fleetlog; never
	// affects the return.
	defer func() {
		p := recover()
		reason, rc, childRC := "clean", 0, -1
		switch {
		case p != nil:
			reason, rc = fmt.Sprintf("panic: %v", p), 2
		case err != nil:
			reason = err.Error()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				childRC = ee.ExitCode()
				rc = childRC
			} else {
				rc = 1
			}
		case exitNote != "":
			// A nil-return non-coordinator exit (stand-down / standby). rc
			// stays 0 (not a failure) but the reason names WHY we exited.
			reason = exitNote
		}
		fleetlog.Log(fleetlog.CompCoord, "supervisor.exit", "info", fleetlog.Fields{
			Proj:  opts.project,
			Agent: opts.agentID,
			Data:  map[string]any{"reason": reason, "rc": rc, "child_rc": childRC},
		})
		if p != nil {
			panic(p) // re-raise: cleanup still runs, operator sees the stack
		}
	}()

	if len(opts.argv) == 0 {
		return errors.New("coord-run: empty child argv")
	}

	// --- lease acquire / stand-down / heartbeat (PR2) ---
	//
	// BEFORE child.Start(): acquire the coordinator lease so the
	// supervisor is the single lease-holder for the coord's lifetime.
	//   - acquired=false -> a healthy live leader exists; STAND DOWN
	//     (print + return nil, NEVER start the child). Stand-down is not
	//     a failure: a normal coord-spawn that loses the race exits 0.
	//   - acquired=true  -> register lease.Release() in this defer stack
	//     (runs on EVERY exit path alongside coord.Cleanup —
	//     fleet-owns-its-resources), start the heartbeat, then start the
	//     child.
	//   - unsupported platform -> skip all lease behavior; run the legacy
	//     bare-child path (byte-identical to today).
	acquire := opts.acquireLease
	if acquire == nil {
		acquire = defaultAcquireLease(opts, stderr)
	}
	lease, acquired, liveHolders, lerr := acquire()
	switch {
	case leaseDisabledOrUnsupported(lerr):
		// Platform without the lease primitive — no lease, legacy path.
		// Fall through to child start.
	case lerr != nil:
		// A real acquire/takeover fault. Surface-don't-silo: refuse to
		// start a child that would have no lease holder rather than
		// silently spawning an unsupervised coord.
		return fmt.Errorf("coord-run: acquire lease for project %q: %w", opts.project, lerr)
	case !acquired:
		// A healthy live leader holds the lease. What happens next depends
		// on whether we are a normal coord or a warm standby (PR3):
		//
		//   normal  -> STAND DOWN. Do NOT start the child, exit 0. A plain
		//              coord-spawn that loses the race is not a failure.
		//   standby -> POLL. Re-acquire (LOCK_NB, which internally runs
		//              TakeOver on a hung leader) every standbyPoll until we
		//              acquire the lease (the old leader exited / was taken
		//              over) or ctx is canceled. Only then start the child.
		//              This is the receiving half of a graceful handoff.
		if !opts.standby {
			if opts.onStandDown != nil {
				opts.onStandDown()
			} else {
				_, _ = fmt.Fprintf(stderr, "a coord is already running for %s\n", opts.project)
			}
			exitNote = "stand-down" // a healthy leader exists; we never led
			return nil
		}
		polled, perr := standbyPollUntilAcquired(ctx, acquire, opts, liveHolders, stderr)
		switch {
		case errors.Is(perr, errStandbyTimedOut):
			exitNote = "standby-timeout" // never acquired within the budget
			return nil
		case perr != nil:
			return fmt.Errorf("coord-run: standby acquire for project %q: %w", opts.project, perr)
		case polled == nil:
			// ctx canceled before we ever acquired — a clean shutdown of a
			// standby that never led. coord.Cleanup still runs (the deferred
			// archive of our own record); nothing else to release.
			exitNote = "standby-canceled"
			return nil
		default:
			lease = polled
			if opts.standbyOnAcquired != nil {
				opts.standbyOnAcquired()
			}
			defer lease.Release()
			// Holding the flock IS being the coordinator (D1) — no Activate
			// flip, no Heartbeat. Release() (cleanup defer) frees the flock on
			// every exit path.
		}
	default:
		// acquired==true: we are the leader. Release joins the cleanup
		// defer stack (LIFO: this defer runs BEFORE coord.Cleanup, so the
		// flock frees first, then the record is archived). lease.Release
		// also stops the heartbeat goroutine, so heartbeat-stop is on the
		// same every-exit-path guarantee.
		//
		// SCOPE BOUNDARY: a WEDGED-but-alive engine that stays a live process
		// keeps holding the flock (the supervisor lives while child.Wait
		// blocks), so it is NOT detected by this layer — a hung-but-alive coord
		// is cleared by `fleet rm` (Decision 3), never auto-killed. A DEAD engine
		// process -> child.Wait returns -> Release() frees the flock -> a standby
		// acquires. Holding the flock IS being the coordinator (D1): no Activate,
		// no Heartbeat.
		defer lease.Release()
	}

	// Build the child with the cancellable context — when the parent's
	// signal handler cancels ctx, exec.CommandContext sends SIGTERM to
	// the child after Wait returns from the io copy unblocking. This
	// is the canonical "pass signals through to child" pattern.
	child := exec.CommandContext(ctx, opts.argv[0], opts.argv[1:]...)
	child.Stdout = stdout
	child.Stderr = stderr
	// Inherit stdin from the parent so the operator can interact with
	// the claude session via the tmux pane (typed input goes to claude).
	child.Stdin = os.Stdin

	if err := child.Start(); err != nil {
		return fmt.Errorf("coord-run: start child: %w", err)
	}

	// PR-2 (D1): acquiring the flock WAS becoming the coordinator (above) —
	// there is no starting->active flip and no heartbeat to start. The engine
	// child is now running; the deferred lease.Release() frees the flock on
	// every exit path.

	// WARM STANDBY engine-pid stamp (codex PR3 iter-14 [P1]): a standby spawn
	// SKIPPED engine-pid resolution at spawn time (the engine wasn't launched
	// until we acquired the lease just now), so the on-disk record still carries
	// the short-lived spawning CLI's pid as Record.PID. Stamp the REAL engine
	// child pid now so TUI / status liveness checks (which read Record.PID) see a
	// live process instead of misclassifying this live successor as dead. Only
	// the standby path needs this — a normal coord spawn resolved the engine pid
	// in spawn.Spawn before the record went live. Best-effort: a stamp failure is
	// surfaced but does not abort the now-running coord (fleet-guard's heartbeat
	// re-stamps on the first turn).
	if opts.standby && child.Process != nil {
		if serr := agent.StampEnginePID(opts.agentID, child.Process.Pid); serr != nil {
			_, _ = fmt.Fprintf(stderr,
				"coord-run: warning: stamp engine pid %d for standby %s failed: %v "+
					"(status/TUI may briefly show it dead until the next fleet-guard heartbeat)\n",
				child.Process.Pid, opts.agentID, serr)
		}
	}

	// Test-only panic hook. Production leaves panicAfterStart false; the
	// PanicViaDefer test sets it to true to prove the top-level defer
	// cleanup fires even when runCoordRun blows up between Start and
	// Wait. The defer above runs BEFORE the panic unwinds past this
	// function, so cleanup fires before main sees the panic.
	if opts.panicAfterStart {
		// Best-effort kill the child so the panic test doesn't leak a
		// `true` process (it exits in microseconds anyway, but be tidy).
		_ = child.Process.Kill()
		panic("runCoordRun: panicAfterStart test hook fired")
	}

	if err := child.Wait(); err != nil {
		// Wait returns the child's exit status verbatim. Don't wrap —
		// production callers (the future dispatch wiring) may want to
		// inspect the *exec.ExitError to forward the child's exit code.
		// Context-cancellation also surfaces here as a Wait error
		// (SIGTERM'd child); the defer still runs.
		return err
	}
	return nil
}

// standbyPollUntilAcquired runs the warm-standby poll loop. It re-calls
// acquire() every standbyPoll until one of:
//
//	acquired==true  -> returns (lease, nil): we are now the leader.
//	ctx canceled    -> returns (nil, nil): clean shutdown, never led.
//	timeout         -> returns errStandbyTimedOut: clean self-exit, never led.
//	real fault      -> returns (nil, err): acquire/takeover error, surface.
//
// A busy lease (acquired==false, err==nil) is the normal "leader still
// alive" case — sleep and retry. The acquire seam internally runs the
// takeover state machine (fence -> kill -> acquire) against a HUNG leader,
// so a standby polling a wedged old coord recovers within one TTL window
// without any lock held across slow work.
//
//	┌──────────────────────────────────────────────────────────────┐
//	│ for {                                                          │
//	│   lease, acquired, live, err := acquire() ── internally        │
//	│   acquired -> return lease            ── become leader         │
//	│   err      -> return err              ── surface fault         │
//	│   live     -> report on STATE CHANGE  ── quarantine, keep poll │
//	│   else     -> select { <-ctx.Done(): return nil;               │
//	│                        <-after(poll): continue }  ── retry     │
//	│ }                                                              │
//	└──────────────────────────────────────────────────────────────┘
//
// Quarantine reporting (KP6, DESIGN-coord-no-auto-kill): a busy round may
// carry live-holder DETECTION — the takeover found a live-but-stale
// leader/candidate and refused to fence or kill it. This loop owns the
// cross-round dedup state and emits one stderr line + one fleetlog
// coord.quarantine{stale-competitor} event per holder on state CHANGE
// only (first detection, a different holder set, or re-detection after a
// clear) — never a per-round log storm. initialDetect is the detection
// from runCoordRun's FIRST acquire (the one that routed us here), so the
// first report is not delayed a poll round and not double-emitted.
func standbyPollUntilAcquired(ctx context.Context, acquire func() (coordLease, bool, []liveHolderInfo, error),
	opts coordRunOpts, initialDetect []liveHolderInfo, stderr io.Writer) (coordLease, error) {

	poll := opts.standbyPoll
	if poll <= 0 {
		poll = defaultStandbyPoll
	}
	timeout := opts.standbyTimeout
	if timeout <= 0 {
		timeout = defaultStandbyTimeout
	}
	_, _ = fmt.Fprintf(stderr,
		"coord-run: standby for %s — a coord is already running; polling for the lease every %s for up to %s\n",
		opts.project, poll, timeout)

	// Cross-round quarantine dedup: report only when the detected set
	// changes (including clear -> re-detect).
	lastQuarantineKey := ""
	reportDetection := func(holders []liveHolderInfo) {
		key := quarantineKey(holders)
		if key == lastQuarantineKey {
			return
		}
		if key == "" {
			_, _ = fmt.Fprintf(stderr,
				"coord-run: standby for %s: previously detected live stale holder no longer detected (leader recovered or exited)\n",
				opts.project)
			lastQuarantineKey = ""
			return
		}
		for _, h := range holders {
			emitQuarantine(opts.project, h, stderr)
		}
		lastQuarantineKey = key
	}
	reportDetection(initialDetect)

	// First attempt already happened in runCoordRun (acquired==false led us
	// here); start by waiting one interval before re-trying so we don't
	// hot-loop on a freshly-observed busy lease.
	timer := time.NewTimer(poll)
	defer timer.Stop()
	timeoutC := opts.standbyTimeoutC
	var timeoutTimer *time.Timer
	if timeoutC == nil {
		timeoutTimer = time.NewTimer(timeout)
		timeoutC = timeoutTimer.C
	}
	if timeoutTimer != nil {
		defer timeoutTimer.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintf(stderr,
				"coord-run: standby for %s canceled before acquiring the lease (clean shutdown)\n",
				opts.project)
			return nil, nil
		case <-timeoutC:
			_, _ = fmt.Fprintf(stderr,
				"coord-run: standby for %s gave up after %s — exiting (not the coord)\n",
				opts.project, timeout)
			return nil, errStandbyTimedOut
		case <-timer.C:
		}
		lease, acquired, live, err := acquire()
		switch {
		case err != nil:
			// A real acquire/takeover fault. Surface-don't-silo: do not keep
			// silently looping on a structural error (e.g. unreadable lease
			// dir) — bubble it so the operator sees a diagnostic.
			return nil, err
		case acquired:
			_, _ = fmt.Fprintf(stderr,
				"coord-run: standby for %s acquired the lease — taking over as leader\n",
				opts.project)
			return lease, nil
		default:
			// Still busy: a healthy leader is alive, OR the KP6 gate stood
			// down against a LIVE stale holder (live non-empty). Either way
			// KEEP POLLING — never exit, never kill; report on state change.
			reportDetection(live)
			timer.Reset(poll)
		}
	}
}

// quarantineKey canonicalizes a detected holder set for dedup.
func quarantineKey(holders []liveHolderInfo) string {
	if len(holders) == 0 {
		return ""
	}
	parts := make([]string, 0, len(holders))
	for _, h := range holders {
		parts = append(parts, fmt.Sprintf("%d/%d/%s", h.pid, h.pidStart, h.agentID))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// emitQuarantine reports one live stale lease holder: a stderr line the
// operator sees in the standby's pane plus a fleetlog coord.quarantine
// event (reason=stale-competitor; msg matches the report string exactly).
// Report-only by design — reaping a LIVE coord takes an explicit
// per-target operator command (`fleet rm <id>` / `fleet handoff <id>`);
// `fleet gc` lists it and reaps only corpses.
func emitQuarantine(project string, h liveHolderInfo, stderr io.Writer) {
	msg := fmt.Sprintf(
		"coord-run: standby for %s: live stale lease holder detected (pid=%d agent=%s) — quarantined, NOT killed; reap manually with `fleet rm %s` or `fleet handoff %s` if it is truly wedged",
		project, h.pid, h.agentID, h.agentID, h.agentID)
	_, _ = fmt.Fprintln(stderr, msg)
	fleetlog.Log(fleetlog.CompCoord, "coord.quarantine", "warn", fleetlog.Fields{
		Proj:  project,
		Agent: h.agentID,
		Msg:   msg,
		Data: map[string]any{
			"reason":    "stale-competitor",
			"pid":       h.pid,
			"pid_start": h.pidStart,
		},
	})
}

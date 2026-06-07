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
// Production wiring:
//
//   - FLEET_LEASE_FAILOVER OFF (default): the dispatch path
//     (cmd/fleet/dispatch.go) builds the default --command argv as
//     ["sh","-c","claude --dangerously-skip-permissions; ..."] — a bare
//     engine, NOT routed through this supervisor (byte-identical to
//     pre-PR2 behavior).
//
//   - FLEET_LEASE_FAILOVER ON (DESIGN-handoff-drain-storm-leak PR2):
//     dispatch wraps that engine argv in this supervisor:
//     ["fleet","coord-run","--agent",<id>,"--project",<p>,"--",
//     "sh","-c","claude ..."]. The supervisor then ACQUIRES + HEARTBEATS
//     the coordinator lease for the coord's whole life, stands down
//     (exit 0) if a healthy leader already holds it, releases the lease
//     on EVERY exit path (alongside coord.Cleanup), and — on a contested
//     acquire — reaps the stale holder via the authenticated
//     internal/coord.KillCoordIfIdentityMatches STONITH.
//
// PR-C originally introduced this subcommand without wiring it in; PR2
// closes that gap behind the failover flag so the lease has a real
// lifetime holder.
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
	"syscall"

	"github.com/spf13/cobra"

	fleet "github.com/edisonshen/fleet"
	"github.com/edisonshen/fleet/internal/coord"
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

	// --- lease wiring (DESIGN-handoff-drain-storm-leak PR2) ---
	//
	// acquireLease is the lease-acquire seam. nil = production
	// (productionAcquireLease, which calls coordlock.AcquireLeaseWithKill
	// + stamps the supervisor identity + runs the new-leader sweep, all
	// gated on FLEET_LEASE_FAILOVER). Tests inject a stub that returns a
	// fakeLease + an acquired flag without touching a real flock.
	//
	// Contract (mirrors coordlock.AcquireLease):
	//   acquired=true,  lease!=nil  -> we are the leader; runCoordRun
	//       registers lease.Release() in the cleanup defer stack, starts
	//       lease.Heartbeat(), then starts the child.
	//   acquired=false              -> a healthy live leader exists;
	//       runCoordRun stands down (prints, returns nil, NEVER starts
	//       the child).
	//   err!=nil with failover-disabled sentinel -> off-flag: runCoordRun
	//       skips all lease behavior and runs the legacy bare-child path.
	acquireLease func() (coordLease, bool, error)

	// onStandDown, when non-nil, is invoked instead of the default
	// stderr print when acquired==false. Test-only seam so a stand-down
	// test can assert the path was taken without scraping stderr.
	onStandDown func()
}

// coordLease is the minimal lease surface runCoordRun needs. The real
// *coordlock.Lease satisfies it; tests inject a fake so the lifetime
// (acquire -> heartbeat -> release on every exit path) is exercised
// without a real kernel flock.
type coordLease interface {
	Heartbeat()
	Release()
}

// withReady is a tiny helper for the real-OS-signal test that returns
// a copy of opts with notifyReady set. Inline helper rather than a
// method on the struct so the field doesn't need test-only export.
func (o coordRunOpts) withReady(f func()) coordRunOpts {
	o.notifyReady = f
	return o
}

// newCoordRunCmd builds the cobra subcommand for `fleet coord-run`.
// Registered from cmd/fleet/main.go's newRootCmd.
func newCoordRunCmd() *cobra.Command {
	var (
		agentID string
		project string
	)
	cmd := &cobra.Command{
		Use:   "coord-run -- <cmd> [args...]",
		Short: "Run a coord child process with guaranteed cleanup on exit",
		Long: `Supervise a coord child (claude in production). On every exit
path — clean exit, signal-killed, internal panic — the supervisor
reaps the coord's own tmux session, archives its agent record to
~/.fleet/agents/archive/<id>-<UTC>.json, and clears the project's
coord-spawn-marker iff its body matches <id>.

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
				agentID: agentID,
				project: project,
				session: tmux.SessionName(agentID),
				argv:    args,
			}
			return notifyCoordRun(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&agentID, "agent", "",
		"agent ID this coord belongs to (REQUIRED; matches ~/.fleet/agents/<id>.json)")
	cmd.Flags().StringVar(&project, "project", "",
		"project name (REQUIRED; the project whose coord-spawn-marker we own)")
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
	//   - failover OFF/unsupported -> skip all lease behavior; run the
	//     legacy bare-child path (byte-identical to today).
	acquire := opts.acquireLease
	if acquire == nil {
		acquire = defaultAcquireLease(opts, stderr)
	}
	lease, acquired, lerr := acquire()
	switch {
	case leaseDisabledOrUnsupported(lerr):
		// Flag OFF (or platform without the lease primitive) — no lease,
		// legacy path. Fall through to child start.
	case lerr != nil:
		// A real acquire/takeover fault. Surface-don't-silo: refuse to
		// start a child that would have no lease holder rather than
		// silently spawning an unsupervised coord.
		return fmt.Errorf("coord-run: acquire lease for project %q: %w", opts.project, lerr)
	case !acquired:
		// Healthy live leader exists -> stand down. Do NOT start the child.
		if opts.onStandDown != nil {
			opts.onStandDown()
		} else {
			_, _ = fmt.Fprintf(stderr, "a coord is already running for %s\n", opts.project)
		}
		return nil
	default:
		// acquired==true: we are the leader. Release joins the cleanup
		// defer stack (LIFO: this defer runs BEFORE coord.Cleanup, so the
		// flock frees first, then the record is archived). lease.Release
		// also stops the heartbeat goroutine, so heartbeat-stop is on the
		// same every-exit-path guarantee.
		defer lease.Release()
		lease.Heartbeat()
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

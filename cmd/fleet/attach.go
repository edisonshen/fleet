package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// attach plumbing — fleet attach <token>.
//
// v0.13 (handoff-identity-cont-3f1d) turns this into a chain-following
// resolver: when the token names an archived agent that was archived
// with cause="handoff", attach walks the successor_id pointer to the
// live tail and attaches there. Operators holding a stale predecessor
// id stop dead-ending. The traversal is observable on stderr —
// "rotated through N hop(s)" so the operator sees the resolution
// path. Cycle guard inside agent.ResolveChain.
//
// Out of scope here (deferred to docs/DESIGN-handoff-identity-continuity.md
// v2 Tier 3 — separate task): project recovery (derive project from
// cwd / picker → reap+respawn). This change keeps the existing
// non-zero exit on unresolvable tokens; a follow-up task wires the
// "never exits" failover.
func newAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <agent-id>",
		Short: "Attach to a running agent's tmux session",
		Long: `attach replaces the fleet process with ` + "`tmux attach`" + ` against
the agent's session. On exit (Ctrl-b d to detach, or exit inside the
session) you return to your shell, not to fleet.

If the named agent has been archived as a handoff, attach walks the
successor pointer chain transitively (with a cycle guard) and lands
on the live tail. The resolution path is printed to stderr.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttach(args[0], cmd.ErrOrStderr(), tmux.Attach)
		},
	}
}

// runAttach is the testable core of newAttachCmd. The attach func is
// injected so tests can verify resolver behavior without exec'ing
// tmux. Production wires tmux.Attach (which exec-replaces this
// process and returns only on error).
//
// Resolution order (per dispatch task brief handoff-identity-cont-3f1d):
//   - agent.ResolveChain walks live → archive → successor chain
//   - on a successful resolution with hops > 0, stderr gets a
//     one-line "rotated through N hop(s); attaching to fleet-<tail>"
//     diagnostic so the operator sees the chain walk happened
//   - on a dead chain (cause != handoff OR successor missing),
//     surface "archived (cause=X); no live successor" so the
//     operator knows why they hit a wall
//   - on a cycle, surface the cycle error verbatim so the operator
//     knows the archive is corrupt
//   - on an unknown token, surface the existing "no agent record"
//     message (preserves UX continuity)
func runAttach(token string, stderr io.Writer, attachFn func(session string) error) error {
	rec, hops, err := agent.ResolveChain(token)
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrChainCycle):
			return err
		case errors.Is(err, agent.ErrNoLiveSuccessor):
			// Dead-chain surface — message already carries the cause
			// embedded by ResolveChain.
			return err
		case errors.Is(err, state.ErrNotFound):
			// Unknown token (no live, no archive). Preserve the
			// historical "no agent record for X" wording for the
			// common case so existing operator muscle memory still
			// reads cleanly. Suggest fleet status as before.
			return fmt.Errorf("no agent record for %q (try `fleet status` to list)", token)
		default:
			return err
		}
	}

	session := rec.TmuxSession
	if session == "" {
		session = tmux.SessionName(rec.ID)
	}

	// Emit the resolution diagnostic when the operator's input id
	// was NOT the live tail — that's the user-visible signal that
	// the chain-following resolver kicked in.
	if hops > 0 {
		hopWord := "hops"
		if hops == 1 {
			hopWord = "hop"
		}
		_, _ = fmt.Fprintf(stderr,
			"%s handed off → %s (rotated through %d %s); attaching to %s\n",
			token, rec.ID, hops, hopWord, session)
	}

	return attachFn(session)
}

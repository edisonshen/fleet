package handoffdelivery

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// ErrNoOwnerObserved is returned when the timeout elapses without ever seeing
// an active lock owner for the project (e.g. a legacy/bare coord from before
// the lease wrapper, which never stamps a lease record). Callers that still
// hold a live successor handle (rec.TmuxSession) use this sentinel to fall back
// to a direct send rather than retrying the same doomed owner-poll. It is
// distinct from a delivery error where an owner WAS observed but its send
// failed — that one must stay pending so a real owner can pick it up.
var ErrNoOwnerObserved = errors.New("handoff delivery: no active lock owner observed before timeout")

// Deps are the delivery seams. Tests inject these to avoid real lease/tmux
// polling; production callers use DefaultDeps.
type Deps struct {
	CurrentOwner func(project string) (coordlock.Owner, bool)
	LoadAgent    func(id string) (*agent.Record, error)
	WaitReady    func(session string) error
	SessionAlive func(session string) (bool, error)
	SendVerified func(session, prompt string) (bool, error)
	WriteMarker  func(project, id string) error
	Now          func() time.Time
	Sleep        func(time.Duration)
}

// DefaultDeps returns the production delivery dependencies.
func DefaultDeps() Deps {
	return Deps{
		CurrentOwner: coordlock.CurrentOwner,
		LoadAgent:    agent.Load,
		WaitReady:    spawn.WaitForReadyToPrompt,
		SessionAlive: tmux.SessionAlive,
		SendVerified: spawn.SendPromptKeysVerified,
		WriteMarker:  state.WriteCoordSpawnMarker,
		Now:          time.Now,
		Sleep:        time.Sleep,
	}
}

// Options controls a lock-owner delivery attempt.
type Options struct {
	Project       string
	Prompt        string
	PromoteMarker bool
	Timeout       time.Duration
	Poll          time.Duration
	Stdout        io.Writer
	Stderr        io.Writer
}

// DeliverToCurrentOwner polls the coordinator lease, resolves the active owner
// to its agent record, and submits prompt to that owner's tmux session. It
// returns only after SendVerified confirms the prompt was submitted. Callers
// should mark their durable inbox/queue consumed only after this returns nil.
func DeliverToCurrentOwner(opts Options, deps Deps) (*agent.Record, error) {
	if opts.Project == "" {
		return nil, fmt.Errorf("handoff delivery: project is empty")
	}
	if deps.CurrentOwner == nil || deps.LoadAgent == nil ||
		deps.WaitReady == nil || deps.SessionAlive == nil {
		return nil, fmt.Errorf("handoff delivery: incomplete dependencies")
	}
	if opts.Prompt != "" && deps.SendVerified == nil {
		return nil, fmt.Errorf("handoff delivery: incomplete dependencies")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Sleep == nil {
		deps.Sleep = time.Sleep
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	poll := opts.Poll
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	deadline := deps.Now().Add(timeout)
	var lastErr error
	ownerObserved := false
	for {
		owner, ok := deps.CurrentOwner(opts.Project)
		if ok && owner.AgentID != "" {
			ownerObserved = true
			rec, err := deps.LoadAgent(owner.AgentID)
			if err == nil {
				if werr := deps.WaitReady(rec.TmuxSession); werr != nil {
					alive, perr := deps.SessionAlive(rec.TmuxSession)
					if !alive && perr == nil {
						lastErr = fmt.Errorf("owner %s session %s is dead", owner.AgentID, rec.TmuxSession)
					} else {
						if opts.Stderr != nil {
							_, _ = fmt.Fprintf(opts.Stderr,
								"warning: readiness poll for lock owner %s (%s) did not converge: %v (sending anyway)\n",
								owner.AgentID, rec.TmuxSession, werr)
						}
						if delivered, ferr := finishDelivery(opts, deps, owner, rec); ferr != nil {
							return nil, ferr
						} else if delivered {
							return rec, nil
						}
					}
				} else {
					alive, perr := deps.SessionAlive(rec.TmuxSession)
					if !alive && perr == nil {
						lastErr = fmt.Errorf("owner %s session %s is dead", owner.AgentID, rec.TmuxSession)
					} else {
						if perr != nil && opts.Stderr != nil {
							_, _ = fmt.Fprintf(opts.Stderr,
								"warning: post-readiness probe for lock owner %s (%s) failed: %v (sending anyway)\n",
								owner.AgentID, rec.TmuxSession, perr)
						}
						if delivered, ferr := finishDelivery(opts, deps, owner, rec); ferr != nil {
							return nil, ferr
						} else if delivered {
							return rec, nil
						}
					}
				}
			} else {
				lastErr = fmt.Errorf("load lock owner %s: %w", owner.AgentID, err)
			}
		}
		if !deps.Now().Before(deadline) {
			if lastErr != nil {
				// An owner WAS observed but its send/load failed — keep this
				// pending (do NOT wrap ErrNoOwnerObserved) so a retry re-targets
				// the real owner instead of falling back to a direct send.
				return nil, fmt.Errorf("handoff delivery: no deliverable lock owner for project %s before timeout: %w",
					opts.Project, lastErr)
			}
			if ownerObserved {
				// An owner was observed every pass but ownership kept flipping
				// in the pre-send recheck window (finishDelivery returned
				// (false,nil)) so we never landed a verified send. Real owners
				// DO exist — keep the doc pending (NOT ErrNoOwnerObserved) so a
				// retry targets the eventual stable owner instead of
				// direct-sending to the queued/losing replacement (codex P2).
				return nil, fmt.Errorf("handoff delivery: lock owner for project %s kept changing before a verified send",
					opts.Project)
			}
			// No owner ever observed — a legacy/bare coord that never stamps a
			// lease record. Signal the caller to fall back to a direct send.
			return nil, fmt.Errorf("%w for project %s", ErrNoOwnerObserved, opts.Project)
		}
		wait := poll
		if rem := deadline.Sub(deps.Now()); rem < wait {
			wait = rem
		}
		if wait > 0 {
			deps.Sleep(wait)
		}
	}
}

func finishDelivery(opts Options, deps Deps, owner coordlock.Owner, rec *agent.Record) (bool, error) {
	current, ok := deps.CurrentOwner(opts.Project)
	if !ok || current.AgentID != owner.AgentID || current.PID != owner.PID ||
		current.PidStart != owner.PidStart {
		return false, nil
	}
	if opts.Prompt != "" {
		submitted, serr := deps.SendVerified(rec.TmuxSession, opts.Prompt)
		if serr != nil {
			return false, fmt.Errorf("send resume prompt to lock owner %s (%s): %w",
				owner.AgentID, rec.TmuxSession, serr)
		}
		if !submitted {
			return false, fmt.Errorf("send resume prompt to lock owner %s (%s): prompt was not submitted",
				owner.AgentID, rec.TmuxSession)
		}
		current, ok = deps.CurrentOwner(opts.Project)
		if !ok || current.AgentID != owner.AgentID || current.PID != owner.PID ||
			current.PidStart != owner.PidStart {
			// Prompt already physically submitted to owner's tmux. Ownership
			// flipped in the post-send window; do not loop and re-send to the
			// new owner.
			if opts.Stderr != nil {
				_, _ = fmt.Fprintf(opts.Stderr,
					"warning: lock owner for project %s changed after verified send to %s; "+
						"skipping marker promotion (delivery already landed)\n",
					opts.Project, owner.AgentID)
			}
			return true, nil
		}
	}
	if opts.PromoteMarker && deps.WriteMarker != nil {
		if werr := deps.WriteMarker(opts.Project, owner.AgentID); werr != nil && opts.Stderr != nil {
			_, _ = fmt.Fprintf(opts.Stderr,
				"warning: coord-spawn marker for project %s -> %s failed after verified lease-owner delivery: %v\n",
				opts.Project, owner.AgentID, werr)
		}
	}
	return true, nil
}

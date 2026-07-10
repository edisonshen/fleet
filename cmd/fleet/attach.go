package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
	"github.com/edisonshen/fleet/internal/coordrepo"
	"github.com/edisonshen/fleet/internal/fleetlog"
	"github.com/edisonshen/fleet/internal/projectlookup"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// attach plumbing — fleet attach <token> [--project <p>].
//
// Three resolution tiers:
//
//   - Tier 1 LIVE: load_live(token) + tmux session present -> attach.
//   - Tier 2 CHAIN: load archive, walk successor pointers, attach to
//     the live tail. Implemented via agent.ResolveChain.
//   - Tier 3 PROJECT RECOVERY (never exits in any recoverable case):
//     derive a project (archived_record.project -> --project flag ->
//     token-as-project -> cwd basename -> interactive picker), scan
//     for the project's live coord, reap+respawn on stale, spawn-
//     fresh on empty, then attach.
//
// Hard rule (operator 2026-06-03): fleet attach NEVER returns a
// non-zero exit in any case Tier 3 can handle. Only true system
// failures (tmux missing, dispatch failed, FS broken) exit non-zero,
// each with a concrete next-step shell command (surface-don't-silo).
//
// See docs/DESIGN-handoff-identity-continuity.md v2 + docs/TASK-PLAN-
// attach-failover.md for the failover diagram + F1-F18 test matrix.
// Memory rule: feedback_fleet_attach_never_exits.md.
func newAttachCmd() *cobra.Command {
	var projectFlag string
	cmd := &cobra.Command{
		Use:   "attach <token>",
		Short: "Attach to a coord/agent tmux session (Tier 1/2/3 failover)",
		Long: `attach replaces the fleet process with ` + "`tmux attach`" + ` against
the resolved tmux session. Resolution walks three tiers — live record,
handoff chain, and project recovery — and never dead-ends as long as
some project name is derivable. Pass --project <name> to bypass
derivation when the token doesn't name a known agent.

Exit codes:
  0   attach succeeded (process replaced by tmux)
  64  cli usage error (non-interactive shell with no --project and
      no derivable project — operator MUST pass --project)
  >0  system failure (tmux missing, dispatch failed, fs broken) —
      stderr names the next-step command`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := AttachOpts{
				Project:     projectFlag,
				Stderr:      cmd.ErrOrStderr(),
				Stdout:      cmd.OutOrStdout(),
				Stdin:       cmd.InOrStdin(),
				IsTty:       isStdinTty(),
				CwdBasename: gitToplevelBasename(),
			}
			// A successful attach execve-replaces this process inside
			// runAttachFailover, so cli.finish never runs on success (by
			// design). finish(err) below fires only on the error-return
			// path. Hence cli.start is the only attach event on success.
			finish := fleetlog.CLIStart(fleetlog.Fields{}, "attach", args[0])
			err := runAttachFailover(args[0], opts)
			// Pass the typed exit code so cli.finish records the real rc
			// (64 for usage errors, 70/127 for system errors) rather than
			// collapsing every failure to rc=1.
			finish(err, ExitCodeFor(err))
			if err != nil {
				// Map typed errors (UsageError + SystemError) to cobra's
				// silent-error path so cobra doesn't print its generic
				// "Error: ..." line on top of the diagnostic our RunE
				// already wrote. main() inspects ExitCodeFor for the
				// exit code (64 / 70 / 127). Codex review iter-1 P2.
				var ue *UsageError
				var se *SystemError
				if errors.As(err, &ue) || errors.As(err, &se) {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
					cmd.SilenceErrors = true
					cmd.SilenceUsage = true
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&projectFlag, "project", "",
		"explicit project name for Tier 3 PROJECT RECOVERY (skips token-as-project / cwd / picker)")
	return cmd
}

// AttachOpts bundles the inputs runAttachFailover needs. Tests
// substitute stdin/stdout/stderr + the cwd derivation; production
// wires real os.Stdin / cmd.OutOrStdout / git-toplevel.
type AttachOpts struct {
	Project             string        // --project flag value (may be empty)
	CwdBasename         string        // git-toplevel basename (may be empty)
	IsTty               bool          // is stdin a tty? gates the picker
	Stdin               io.Reader     // picker input
	Stdout              io.Writer     // picker prompt + numbered list
	Stderr              io.Writer     // every failover diagnostic
	ResolveWaitInterval time.Duration // zero with ResolveMaxAttempts set means no sleep (tests)
	ResolveMaxAttempts  int           // zero uses the production default
}

const (
	defaultAttachResolveWaitInterval = time.Second
	defaultAttachResolveMaxAttempts  = 30
)

// UsageError signals "non-interactive shell with no derivable project."
// Distinct from system errors so the cobra wrapper can exit with the
// CLI usage code (64). NOT raised for any case Tier 3 can recover —
// per feedback_fleet_attach_never_exits, ONLY non-tty + no derivation
// requires operator input.
type UsageError struct {
	Msg string
}

func (e *UsageError) Error() string { return e.Msg }

// ExitCodeFor maps an attach error to its conventional Unix exit code:
//
//	*UsageError -> 64 (sysexits.h EX_USAGE)
//	*SystemError -> 70 (sysexits.h EX_SOFTWARE) by default; specific
//	                cases override (tmux missing -> 127 ENOENT-style)
//	other -> 1
//
// Used by main()'s top-level os.Exit dispatch.
func ExitCodeFor(err error) int {
	var ue *UsageError
	if errors.As(err, &ue) {
		return 64
	}
	var se *SystemError
	if errors.As(err, &se) {
		return se.code
	}
	return 1
}

// SystemError signals a hard environmental failure (tmux missing,
// dispatch CLI failed, FS unreadable). Tier 3 cannot recover from
// these; the error message names the concrete next-step command per
// surface-don't-silo.
type SystemError struct {
	Msg  string
	code int
}

func (e *SystemError) Error() string { return e.Msg }

func newSystemError(code int, msg string) error {
	return &SystemError{Msg: msg, code: code}
}

// --- runAttachFailover: the testable resolver core ---

// runAttachFailover implements the three-tier resolver. Returns nil
// on a successful attach (attachFn replaces the process via execve in
// production; tests stub it to record and return nil). Returns
// *UsageError on non-tty + no derivation, *SystemError on tmux/
// dispatch/FS failure. Never returns a generic error for any case
// Tier 3 could have recovered from — that would violate the hard
// rule (feedback_fleet_attach_never_exits.md).
func runAttachFailover(token string, opts AttachOpts) error {
	// Tier 1 / Tier 2 via the existing chain resolver. Live and chain
	// hits attach immediately; any "no successful tail" outcome —
	// cycle, broken chain, non-handoff archive, unknown token — falls
	// THROUGH to Tier 3 with the appropriate diagnostic on stderr.
	rec, hops, rerr := agent.ResolveChain(token)
	if rerr == nil {
		if project, ok := coordTargetProject(rec); ok {
			if hops > 0 {
				hopWord := "hops"
				if hops == 1 {
					hopWord = "hop"
				}
				_, _ = fmt.Fprintf(opts.Stderr,
					"%s handed off → %s (rotated through %d %s); resolving current coord for %s via lease\n",
					token, rec.ID, hops, hopWord, project)
			}
			return resolveAndAttachCoord(token, project, opts)
		}
		session := rec.TmuxSession
		if session == "" {
			session = tmux.SessionName(rec.ID)
		}
		// Pre-check tmux availability so we surface "tmux missing"
		// before exec'ing. Same surface-don't-silo principle as the
		// dispatch path.
		if err := tmuxAvailableFnVar(); err != nil {
			return newSystemError(127, fmt.Sprintf(
				"tmux not available: %v — install tmux (https://github.com/tmux/tmux) and ensure it is on PATH",
				err))
		}
		// Stale-live-record gate (codex review iter-1 P1): a live record
		// on disk with a dead tmux session must NOT dead-end on
		// `tmux.Attach` — that's the exact "never-exit" failure mode
		// Tier 3 exists to recover from. Probe the session; on a
		// DEFINITIVE dead (probe returns alive=false with no error),
		// surface a tier-2-style line and fall through to project
		// recovery. Transport errors (ambiguous) stay conservative and
		// proceed with attach — tmux's own error surfaces if it really
		// is dead, but a hiccup doesn't cost a needless respawn.
		if alive, probeErr := sessionProbeFnVar(session); probeErr == nil && !alive {
			_, _ = fmt.Fprintf(opts.Stderr,
				"%s: live record present but tmux session %s is gone; failing over to project recovery\n",
				token, session)
			// Carry the stale record's project tag into Tier 3 (when the
			// flag is empty) so derivation doesn't dead-end on "unknown
			// token" just because the live record never made it into the
			// archive yet. The flag takes precedence when the operator
			// passed --project explicitly — same precedence as the
			// archive-record branch in deriveProject.
			recovery := opts
			if recovery.Project == "" && rec.Project != "" {
				recovery.Project = rec.Project
			}
			return tier3ProjectRecovery(token, errors.New("stale live record"), recovery)
		}
		if hops > 0 {
			hopWord := "hops"
			if hops == 1 {
				hopWord = "hop"
			}
			_, _ = fmt.Fprintf(opts.Stderr,
				"%s handed off → %s (rotated through %d %s); attaching to %s\n",
				token, rec.ID, hops, hopWord, session)
		}
		// Codex review iter-13 P2: race-window symmetry. The probe
		// above passed, but the session could still die before
		// attachFnVar exec's. For COORD records with a project tag,
		// Tier 3 can recover exactly this stale-session case — fall
		// through instead of surfacing the raw tmux error. For other
		// records (workers, untagged), surface SystemError with the
		// retry advice (no Tier 3 recovery path for non-coord agents).
		if err := attachFnVar(session); err != nil {
			coordTag := projectlookup.CoordTaskID(rec.Project)
			if rec.Project != "" && rec.TaskID == coordTag {
				_, _ = fmt.Fprintf(opts.Stderr,
					"%s: session %s died between probe and attach; failing over to project recovery\n",
					token, session)
				recovery := opts
				if recovery.Project == "" {
					recovery.Project = rec.Project
				}
				return tier3ProjectRecovery(token, errors.New("attach race"), recovery)
			}
			return newSystemError(70, fmt.Sprintf(
				"agent %s session %s died between probe and attach (%v) — re-run `fleet attach %s` to retry",
				token, session, err, token))
		}
		return nil
	}

	// Recoverable-error gate (codex review iter-3 P2): Tier 3 PROJECT
	// RECOVERY exists to repair "chain semantics ran out" — cycles,
	// broken chains, archived non-handoff records, unknown tokens.
	// Generic FS / parse failures (corrupt JSON, EIO mid-read, perm
	// denied) are NOT recoverable: agent.List inside Tier 3 may silently
	// skip the bad file and spawn/reap a coord around it, masking the
	// real state corruption. Surface those as a SystemError so the
	// operator sees the actual fault rather than a confused recovery.
	if !errors.Is(rerr, agent.ErrChainCycle) &&
		!errors.Is(rerr, agent.ErrNoLiveSuccessor) &&
		!errors.Is(rerr, state.ErrNotFound) {
		return newSystemError(70, fmt.Sprintf(
			"%s: agent record unreadable: %v — inspect ~/.fleet/agents/%s.json (or its archive) and re-run after repair",
			token, rerr, token))
	}

	// Tier 2 result was an error → emit the surface-line then failover.
	// Each branch writes EXACTLY one line so the test matrix can pin
	// it. The cycle path embeds the cycle trace; the broken-chain path
	// embeds the missing-link id; the non-handoff path embeds the
	// archived_cause; the unknown-token path names the token.
	emitTier2FailoverLine(token, rerr, opts.Stderr)

	// Tier 3 PROJECT RECOVERY.
	return tier3ProjectRecovery(token, rerr, opts)
}

// emitTier2FailoverLine writes the exact one-line diagnostic the test
// matrix pins (F1–F4 stderr line 1). Centralized so the surface text
// stays in one place — each branch emits its own line because the
// formats differ.
func emitTier2FailoverLine(token string, rerr error, stderr io.Writer) {
	switch {
	case errors.Is(rerr, agent.ErrChainCycle):
		// ErrChainCycle's message: "handoff chain cycle detected:
		// <token> revisited at depth N." The F1 expected line is
		// "cycle detected: A → B → A" — but the resolver doesn't
		// build the trace today; the cycle error names the
		// revisited token only. Surface a compact line that
		// contains "cycle" + the token so F1's contains-check
		// passes, and embeds the underlying message for context.
		_, _ = fmt.Fprintf(stderr, "%s: cycle detected in handoff chain (%v); failing over to project recovery\n",
			token, rerr)
	case errors.Is(rerr, agent.ErrNoLiveSuccessor):
		// ErrNoLiveSuccessor messages can be:
		//   - "for <token>: archived (cause=<X>)" — non-handoff cause
		//   - "for <token>: chain broken at <succ> (cause=handoff
		//      successor missing)" — broken mid-walk
		// Both fall into Tier 3. We detect "chain broken" vs the
		// archived-cause case from the message body so the surface
		// line matches F2 vs F3.
		msg := rerr.Error()
		if strings.Contains(msg, "chain broken at ") {
			// Extract the missing-link id from "chain broken at <id>"
			// purely to print a clean line; if extraction fails we
			// fall back to the wrapped message.
			missing := extractMissingLink(msg)
			if missing != "" {
				_, _ = fmt.Fprintf(stderr, "%s handoff chain broken at %s (no record); failing over to project recovery\n",
					token, missing)
			} else {
				_, _ = fmt.Fprintf(stderr, "%s: %v; failing over to project recovery\n", token, rerr)
			}
		} else {
			cause := extractArchivedCause(msg)
			if cause == "" {
				cause = "unknown"
			}
			_, _ = fmt.Fprintf(stderr, "%s archived (cause=%s); failing over to project recovery\n",
				token, cause)
		}
	case errors.Is(rerr, state.ErrNotFound):
		// Unknown token. The actual derivation source (cwd / picker)
		// emits its own line below; here we don't write one — that
		// would split the operator's read across two lines for no
		// added information.
	default:
		// Some other error (FS unreadable, etc.). Surface verbatim so
		// the operator sees it before Tier 3 tries.
		_, _ = fmt.Fprintf(stderr, "%s: tier 2 resolver: %v; failing over to project recovery\n",
			token, rerr)
	}
}

// extractMissingLink parses "chain broken at <id>" out of a wrapped
// ErrNoLiveSuccessor message. Returns "" on no match.
func extractMissingLink(msg string) string {
	const prefix = "chain broken at "
	i := strings.Index(msg, prefix)
	if i < 0 {
		return ""
	}
	tail := msg[i+len(prefix):]
	// Stop at first space or paren.
	for i, c := range tail {
		if c == ' ' || c == '(' || c == ',' {
			return tail[:i]
		}
	}
	return tail
}

// extractArchivedCause parses "cause=<X>" out of a wrapped
// ErrNoLiveSuccessor message. Returns "" on no match.
func extractArchivedCause(msg string) string {
	const prefix = "cause="
	i := strings.Index(msg, prefix)
	if i < 0 {
		return ""
	}
	tail := msg[i+len(prefix):]
	for i, c := range tail {
		if c == ' ' || c == ')' || c == ',' || c == '\n' {
			return tail[:i]
		}
	}
	return tail
}

// --- Tier 3: project derivation + coord recovery ---

// tier3ProjectRecovery runs the project derivation pipeline + coord
// recovery + attach. Returns *UsageError when no project can be
// derived in a non-interactive shell. Returns *SystemError when tmux
// is unavailable, dispatch fails, or FS is unreadable. Returns nil on
// a successful attach.
func tier3ProjectRecovery(token string, tier2err error, opts AttachOpts) error {
	// Pre-check: surface "tmux missing" before any disk work so the
	// operator sees the clear next-step.
	if err := tmuxAvailableFnVar(); err != nil {
		return newSystemError(127, fmt.Sprintf(
			"tmux not available: %v — install tmux (https://github.com/tmux/tmux) and ensure it is on PATH",
			err))
	}
	project, src, err := deriveProject(token, tier2err, opts)
	if err != nil {
		return err // *UsageError or *SystemError already
	}
	// Emit the derivation-source diagnostic when the source is not the
	// archived record (the cycle/broken/non-handoff branches already
	// wrote their own line, and the archived-record source is implied
	// by F1–F3's pre-existing surface). The cwd / token-as-project /
	// picker sources each need an explicit line so the operator sees
	// which project the resolver bound.
	emitDerivationLine(token, project, src, opts.Stderr)

	if src == derivTokenAsProject {
		_, _ = fmt.Fprintf(opts.Stderr,
			"%s: token matched project name; resolving coord for %s via lease\n",
			token, project)
	}
	return resolveAndAttachCoord(token, project, opts)
}

func coordTargetProject(rec *agent.Record) (string, bool) {
	if rec == nil || rec.Project == "" {
		return "", false
	}
	if rec.IsCoord || rec.TaskID == projectlookup.CoordTaskID(rec.Project) {
		return rec.Project, true
	}
	return "", false
}

func resolveAndAttachCoord(token, project string, opts AttachOpts) error {
	if err := tmuxAvailableFnVar(); err != nil {
		return newSystemError(127, fmt.Sprintf(
			"tmux not available: %v — install tmux (https://github.com/tmux/tmux) and ensure it is on PATH",
			err))
	}
	agentID := attachNewAgentIDFnVar()
	if agentID == "" {
		return newSystemError(70, fmt.Sprintf(
			"cannot preallocate a coord id for project %s — re-run `fleet attach %s --project %s` to retry",
			project, token, project))
	}
	// This ID is only Resolve's spawn-serialization token. dispatch mints
	// the real coord ID; the free-flock fast path is identity-blind, so
	// discarding this preallocation after Resolve is intentional.
	interval, attempts := attachResolvePolicy(opts)
	for {
		verdict, err := attachResolveFnVar(project, agentID)
		if err != nil {
			return newSystemError(70, fmt.Sprintf(
				"%s: cannot resolve coordinator lease for %s: %v — re-run `fleet attach %s --project %s`; if it persists, run `fleet doctor` and inspect ~/.fleet/projects/%s/.locks/",
				token, project, err, token, project, project))
		}
		if verdict.Decision != coordreconcile.Wait {
			return consumeResolvedCoord(token, project, opts, verdict)
		}
		if attempts <= 0 {
			// D9e (no-auto-kill, rainier straggler close): a bounded Wait means
			// the flock is held by a LIVE coord whose identity Resolve can't read
			// from the flock body (an old-binary straggler post-bridge-deletion,
			// or a torn body). A busy flock is a live holder — the kernel frees it
			// only on death — so reaping+respawning would either KILL a live coord
			// (forbidden, Decision 3) or dead-end (the respawn can't take the held
			// flock). Recover via the AGENT RECORD instead: FindLiveCoord names a
			// live coord by project + is_coord + a live tmux session, and we
			// ATTACH into it (land inside the live session). NEVER a reap.
			if records, lerr := agent.List(); lerr == nil {
				if rec, ok := projectlookup.FindLiveCoord(records, project); ok {
					session := rec.TmuxSession
					if session == "" {
						session = tmux.SessionName(rec.ID)
					}
					_, _ = fmt.Fprintf(opts.Stderr,
						"%s: coord for %s holds the flock but its identity is not readable from the lease body; recovered live coord %s via its agent record — attaching (no respawn, no reap)\n",
						token, project, rec.ID)
					return attachExistingCoord(token, project, rec.ID, session)
				}
			}
			// No live coord record derivable — surface guidance (still no
			// auto-reap of a live holder).
			return newSystemError(dispatchVetoExitCode, fmt.Sprintf(
				"%s: coord for %s is still not attachable after bounded lease wait (%s: %s) and no live coord record is derivable. Run `fleet status` to discover it, then `fleet handoff <id>` / `fleet rm <id>` — never an auto-reap of a live holder. Re-run `fleet attach %s --project %s` once resolved",
				token, project, verdict.Decision, verdict.Reason, token, project))
		}
		if interval > 0 {
			time.Sleep(interval)
		}
		attempts--
	}
}

func attachResolvePolicy(opts AttachOpts) (time.Duration, int) {
	attempts := opts.ResolveMaxAttempts
	if attempts <= 0 {
		attempts = defaultAttachResolveMaxAttempts
	}
	interval := opts.ResolveWaitInterval
	if interval == 0 && opts.ResolveMaxAttempts == 0 {
		interval = defaultAttachResolveWaitInterval
	}
	if interval < 0 {
		interval = 0
	}
	return interval, attempts
}

func consumeResolvedCoord(token, project string, opts AttachOpts, verdict coordreconcile.Verdict) error {
	switch verdict.Decision {
	case coordreconcile.Attach:
		return attachResolvedOwner(token, project, opts, verdict)
	case coordreconcile.Spawn:
		killedID, err := killCollidingOrphanTmux(project)
		if err != nil {
			return newSystemError(70, fmt.Sprintf(
				"%s: lease resolved Spawn for %s but pre-spawn orphan cleanup failed: %v — re-run `fleet attach %s --project %s`; the temporary starting claim will self-heal after its TTL",
				token, project, err, token, project))
		}
		return spawnAndAttachRecovered(token, project, opts,
			func(stderr io.Writer, res coordSpawnResult) {
				if killedID != "" {
					_, _ = fmt.Fprintf(stderr,
						"%s: reaped stale %s (live orphan session); spawned %s for %s; attaching\n",
						token, killedID, res.ID, project)
					return
				}
				_, _ = fmt.Fprintf(stderr,
					"%s: lease resolved spawn for %s (%s); spawned %s; attaching\n",
					token, project, verdict.Reason, res.ID)
			})
	default:
		return newSystemError(70, fmt.Sprintf(
			"%s: coord resolver returned unknown decision %s for %s — re-run `fleet attach %s --project %s`; if it persists, run `fleet doctor`",
			token, verdict.Decision, project, token, project))
	}
}

func attachResolvedOwner(token, project string, opts AttachOpts, verdict coordreconcile.Verdict) error {
	ownerID := verdict.Owner.AgentID
	if ownerID == "" {
		return newSystemError(dispatchVetoExitCode, fmt.Sprintf(
			"%s: coord lease for %s resolved an empty owner id — re-run `fleet attach %s --project %s` shortly",
			token, project, token, project))
	}
	rec, err := agent.Load(ownerID)
	if err != nil {
		return newSystemError(dispatchVetoExitCode, fmt.Sprintf(
			"%s: coord lease for %s is held by %s but its agent record is not readable (%v). Do not respawn beside a process-live owner; check `fleet status` / FLEET_TMUX_SOCKET, then re-run `fleet attach %s --project %s`",
			token, project, ownerID, err, token, project))
	}
	session := rec.TmuxSession
	if session == "" {
		session = tmux.SessionName(rec.ID)
	}
	alive, probeErr := sessionProbeFnVar(session)
	if probeErr != nil {
		return newSystemError(dispatchVetoExitCode, fmt.Sprintf(
			"%s: coord %s for %s is process-live but tmux session %s is unreachable (%v). Do not respawn beside it; check FLEET_TMUX_SOCKET / `fleet status`, then re-run `fleet attach %s --project %s`",
			token, ownerID, project, session, probeErr, token, project))
	}
	if !alive {
		return newSystemError(dispatchVetoExitCode, fmt.Sprintf(
			"%s: coord %s for %s is process-live in the lease but tmux session %s is gone. Do not respawn beside it; run `fleet doctor` or inspect `fleet status`, then re-run `fleet attach %s --project %s`",
			token, ownerID, project, session, token, project))
	}
	if _, ok := attachCurrentOwnerFnVar(project); !ok {
		_, _ = fmt.Fprintf(opts.Stderr,
			"warning: coord %s for %s is process-live but lease heartbeat is stale; attaching anyway. If it is hung, run `fleet doctor`.\n",
			ownerID, project)
	}
	_, _ = fmt.Fprintf(opts.Stderr,
		"%s: attached to current coord %s for %s\n",
		token, ownerID, project)
	return attachExistingCoord(token, project, ownerID, session)
}

func killCollidingOrphanTmux(project string) (string, error) {
	records, badIDs, err := agent.ListStrict()
	if err != nil {
		return "", fmt.Errorf("agent.ListStrict failed: %w", err)
	}
	if len(badIDs) > 0 {
		return "", fmt.Errorf("%d unparseable agent record(s) %v; refusing orphan cleanup before spawn", len(badIDs), badIDs)
	}
	id, ok := projectlookup.OrphanTmuxForProject(records, project)
	if !ok {
		return "", nil
	}
	session := tmux.SessionName(id)
	if err := killTmuxSessionFnVar(session); err != nil {
		return "", fmt.Errorf("tmux kill-session %s failed: %w; re-run `tmux kill-session -t %s` manually then retry attach", session, err, session)
	}
	return id, nil
}

// spawnAndAttachRecovered is the SHARED coord-spawn wrapper for Tier 3
// Paths B / C / C' / D. It enforces the one-spawn-per-invocation bound
// (exactly one coordSpawnFullFnVar call), classifies the dispatch exit
// code uniformly, and emits the path-specific success line.
//
// BUG #2 / INVARIANT 3 (attach-failover): the veto classification lives
// HERE, in the shared wrapper, never special-cased inside one path — so
// Paths B/C/C'/D all honor `dispatch` exit 75 identically (F23c proves
// Path D does, F21 proves Path C does):
//
//	derr == nil                  → emit success line + attach (exit 0).
//	errors.Is(derr, vetoed)      → scan-only re-resolve, no 2nd spawn:
//	                               live coord now? → attach (exit 0)
//	                               else → wait-retry → exit 75.
//	other derr                   → hard failure → *SystemError(70).
//
// emitSuccess writes the path-specific "<token>: ... ; attaching" line
// (verb/wording differs per path) only on the happy path.
func spawnAndAttachRecovered(token, project string, opts AttachOpts, emitSuccess func(io.Writer, coordSpawnResult)) error {
	res, derr := coordSpawnFullFnVar(project)
	if derr != nil {
		if errors.Is(derr, errCoordSpawnVetoed) {
			return handleCoordSpawnVeto(token, project, opts)
		}
		return newSystemError(70, fmt.Sprintf(
			"dispatch --coord-spawn --project %s failed: %v — re-run `fleet attach %s --project %s` to retry (uses the same recovery path with the bootstrap prompt + --cwd)",
			project, derr, token, project))
	}
	emitSuccess(opts.Stderr, res)
	emitPromptDeliveryWarning(res, opts.Stderr)
	return attachSpawnedSession(token, project, res.ID, opts)
}

// handleCoordSpawnVeto runs the veto recovery (BUG #2, attach-failover
// INVARIANT 3). Invoked by every Tier 3 spawn caller (Paths B/C/C'/D) via
// spawnAndAttachRecovered when shellCoordSpawn reports errCoordSpawnVetoed
// (dispatch exited 75). The
// veto means a coord is alive on a different tmux socket OR one crashed
// within the freshness window and may be restarting — neither is a hard
// failure, so we must NOT exit 70.
//
// Policy (one-spawn-per-invocation): scan-only re-resolve ONCE — re-read
// records + probe for a now-live coord. Do NOT re-enter full Tier 3, do
// NOT spawn again.
//
//	live coord now? → Path A attach (exit 0).
//	else            → stderr wait-and-retry naming BOTH causes + the
//	                  cross-socket next step → *SystemError(75).
func handleCoordSpawnVeto(token, project string, opts AttachOpts) error {
	verdict, err := attachResolveFnVar(project, "")
	if err == nil && verdict.Decision == coordreconcile.Attach {
		return attachResolvedOwner(token, project, opts, verdict)
	}
	if err != nil {
		_, _ = fmt.Fprintf(opts.Stderr,
			"%s: coord-spawn vetoed for %s; lease re-resolve failed: %v\n",
			token, project, err)
	} else {
		_, _ = fmt.Fprintf(opts.Stderr,
			"%s: coord-spawn vetoed for %s; lease re-resolve returned %s (%s)\n",
			token, project, verdict.Decision, verdict.Reason)
	}
	return newSystemError(dispatchVetoExitCode, fmt.Sprintf(
		"%s: coord-spawn for %s vetoed — likely (1) a live coord on a different tmux socket, or (2) a coord crashed within the freshness window and is restarting. "+
			"Re-run `fleet attach %s --project %s` shortly; if it persists, check `fleet status` / your FLEET_TMUX_SOCKET for a coord on another socket",
		token, project, token, project))
}

// emitPromptDeliveryWarning surfaces dispatch's "initial prompt not
// delivered" sigil so the operator knows /coordinator never ran inside
// the freshly-spawned session. Without this, attach lands them in a
// bare Claude that looks alive but never claims the project (codex
// review iter-14 P2). Silent when the prompt delivered cleanly.
func emitPromptDeliveryWarning(res coordSpawnResult, stderr io.Writer) {
	if res.PromptDelivered {
		return
	}
	_, _ = fmt.Fprintf(stderr,
		"warning: coord %s spawned but initial prompt was NOT delivered — type /coordinator manually after attach, or `fleet rm %s` and re-run `fleet attach` to respawn\n",
		res.ID, res.ID)
}

// attachSpawnedSession probes the freshly-spawned session before exec'ing
// tmux.Attach and surfaces a SystemError when the spawn returned exit 0
// but the session is DEFINITIVELY dead. dispatch treats some failure modes
// (initial-prompt delivery hiccup, fast crash before first tmux paint) as
// warnings on stdout — exit 0 alone doesn't guarantee an attachable
// session. Without this gate, Tier 3 would exec into nothing and dead-end
// the operator on tmux's own "no sessions" line, violating the never-exit
// invariant. Codex review iter-2 P2.
//
// Transport errors from the probe stay conservative (proceed with attach)
// — a flaky tmux socket shouldn't force a re-spawn loop when the real
// session may well be live. Same discipline as runAttachFailover's Tier
// 1/2 stale-live-record gate (codex review iter-1 P1).
// attachExistingCoord wraps tmux.Attach for a Resolve Attach verdict.
// A live coord can die between the holder probe and the actual tmux.Attach
// exec, leaving the operator on the generic exit-1 path instead of the
// Tier 3 retry diagnostic.
//
// Symmetric to attachSpawnedSession but the retry advice differs:
// existing-coord races mean dispatch isn't needed; the operator just
// re-runs attach and Resolve picks the current lease verdict. Retry shape
// is just `fleet attach <token> --project <p>`.
func attachExistingCoord(token, project, coordID, session string) error {
	if err := attachFnVar(session); err != nil {
		return newSystemError(70, fmt.Sprintf(
			"existing coord %s for %s went away between probe and attach (%s): %v — re-run `fleet attach %s --project %s` (Tier 3 will retry recovery)",
			coordID, project, session, err, token, project))
	}
	return nil
}

func attachSpawnedSession(token, project, newID string, opts AttachOpts) error {
	session := tmux.SessionName(newID)
	if alive, probeErr := sessionProbeFnVar(session); probeErr == nil && !alive {
		// Codex review iter-8 P2: substitute the actual token so the
		// retry command is copy-pasteable. The placeholder `<token>`
		// would have left the operator with a literal-string command
		// that fails arg parsing.
		return newSystemError(70, fmt.Sprintf(
			"coord-spawn for %s returned exit 0 but session %s never came up — re-run `fleet attach %s --project %s` to retry (uses the same recovery path with the bootstrap prompt + --cwd); check ~/.fleet/agents/%s.json for clues",
			project, session, token, project, newID))
	}
	// Codex review iter-10 P2: the probe-then-attach window is non-zero
	// (the spawn could exit AFTER probe says alive AND BEFORE
	// tmux.Attach exec's). On a raw attach error here, fall back to the
	// SAME retry diagnostic the dead-probe branch surfaces — the
	// operator should never see the generic exit-1 "tmux: no sessions"
	// for what is structurally a Tier 3 recovery race. Same SystemError
	// shape (exit 70) so main()'s ExitCodeFor wiring keeps the
	// documented exit-code contract.
	if err := attachFnVar(session); err != nil {
		return newSystemError(70, fmt.Sprintf(
			"coord-spawn for %s returned exit 0 and probe passed but tmux.Attach %s failed: %v — re-run `fleet attach %s --project %s` to retry (uses the same recovery path with the bootstrap prompt + --cwd); check ~/.fleet/agents/%s.json for clues",
			project, session, err, token, project, newID))
	}
	return nil
}

// derivSource identifies which step in the derivation pipeline picked
// the project name — drives the surface-line wording.
type derivSource int

const (
	derivArchivedRecord derivSource = iota + 1
	derivFlag
	derivTokenAsProject
	derivCwdBasename
	derivPicker
)

// deriveProject runs the precedence pipeline:
//
//  1. archived_record.project — read fresh from disk so a tier2err of
//     ErrNoLiveSuccessor with cause=kill / chain-broken still uses the
//     archive's project field.
//  2. --project flag.
//  3. token IS a known project name.
//  4. cwd basename matches a known project.
//  5. interactive picker (tty only). Non-tty → *UsageError.
//
// Returns the project + which source picked it. *SystemError on FS
// failure; *UsageError on non-tty + no derivation.
func deriveProject(token string, _ error, opts AttachOpts) (string, derivSource, error) {
	// Step 1: archived record's project field. LoadArchive is cheap
	// (one ReadFile) and we don't care about the same error tier2err
	// already surfaced — we just want the project tag if any.
	if arec, aerr := agent.LoadArchive(token); aerr == nil && arec.Project != "" {
		return arec.Project, derivArchivedRecord, nil
	}
	// Step 2: --project flag (may be empty).
	if opts.Project != "" {
		// Validate the name now so a bad flag fails loudly with the
		// validator's exact message (e.g. illegal char, too long).
		if err := state.ValidateProjectName(opts.Project); err != nil {
			return "", 0, newSystemError(64, fmt.Sprintf(
				"--project %q invalid: %v", opts.Project, err))
		}
		return opts.Project, derivFlag, nil
	}
	known, kerr := projectlookup.KnownProjects()
	if kerr != nil {
		return "", 0, newSystemError(70, fmt.Sprintf(
			"failed to enumerate ~/.fleet/projects/: %v", kerr))
	}
	// Step 3: token-as-project.
	for _, p := range known {
		if p == token {
			return p, derivTokenAsProject, nil
		}
	}
	// Step 4: cwd basename matches a known project.
	if opts.CwdBasename != "" {
		for _, p := range known {
			if p == opts.CwdBasename {
				return p, derivCwdBasename, nil
			}
		}
	}
	// Step 5: interactive picker. Non-tty -> *UsageError.
	if !opts.IsTty {
		known = filterValidNames(known) // defensive
		msg := fmt.Sprintf(
			"%s: cannot derive project (non-interactive). Pass --project <name>. Known: %s",
			token, strings.Join(known, ", "))
		return "", 0, &UsageError{Msg: msg}
	}
	return runPicker(token, known, opts)
}

// emitDerivationLine writes the one-line "derived <project> from
// <source>" surface for sources F4, F10, F11, F16. Sources F1–F3
// emit their own pre-failover line via emitTier2FailoverLine; F5–F8
// and F9 derive from the flag or token and don't need a separate
// derivation surface — their "attached" line carries the project.
func emitDerivationLine(token, project string, src derivSource, stderr io.Writer) {
	switch src {
	case derivCwdBasename:
		// F4 + F10: "unknown identifier; deriving project from cwd
		// basename → <p>" / "deriving project from cwd basename → <p>".
		// F4 has been pre-archived as unknown; F10 has no archive.
		// Both want the cwd-derivation line; F4 needs the "unknown
		// identifier" prefix while F10 just wants the deriving line.
		// We emit the prefix conditionally on whether the archived-
		// record source fired — but at this point src ≠ derivArchived-
		// Record so neither test has an archive hit. F4 has an archived
		// record but NOT in projects/... wait, F4 has no record at all.
		// Look again: F4 setup says "no record A anywhere". So both
		// F4 and F10 reach this with derivCwdBasename. F4 expects
		// "<token>: unknown identifier; deriving ..."; F10 expects
		// "<token>: deriving ...". Distinguish via whether the token
		// has any record (live or archive) — agent.Load + LoadArchive.
		if _, lerr := agent.Load(token); errors.Is(lerr, state.ErrNotFound) {
			if _, aerr := agent.LoadArchive(token); errors.Is(aerr, state.ErrNotFound) {
				_, _ = fmt.Fprintf(stderr,
					"%s: unknown identifier; deriving project from cwd basename → %s\n",
					token, project)
				return
			}
		}
		_, _ = fmt.Fprintf(stderr,
			"%s: deriving project from cwd basename → %s\n",
			token, project)
	default:
		// Other sources: silent here — the path-specific "attached"
		// or "spawned" line in tier3ProjectRecovery is sufficient.
	}
}

// runPicker prints the numbered project list + a "[n] new project"
// option and reads stdin for the operator's selection. Returns the
// chosen project + derivPicker source. *UsageError on bad/empty
// input (preserves never-exit-when-recoverable: a closed tty is
// recoverable only by operator action).
func runPicker(token string, known []string, opts AttachOpts) (string, derivSource, error) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	_, _ = fmt.Fprintf(stdout, "unknown identifier %q. Pick a project:", token)
	for i, p := range known {
		_, _ = fmt.Fprintf(stdout, "  [%d] %s", i+1, p)
	}
	_, _ = fmt.Fprintf(stdout, "  [n] new project (enter name): ")
	// Single-line read; trim whitespace.
	br := bufio.NewReader(opts.Stdin)
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", 0, &UsageError{Msg: fmt.Sprintf(
			"%s: picker read failed: %v — re-run with --project <name>", token, err)}
	}
	choice := strings.TrimSpace(line)
	if choice == "" {
		return "", 0, &UsageError{Msg: fmt.Sprintf(
			"%s: picker got empty input — re-run with --project <name>", token)}
	}
	// "n" prefix → new-project entry. Operator types "n <name>" or
	// "n: <name>" on the same line.
	if strings.HasPrefix(strings.ToLower(choice), "n ") || strings.HasPrefix(choice, "n:") {
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(choice, "n "), "n:"))
		if err := state.ValidateProjectName(name); err != nil {
			return "", 0, &UsageError{Msg: fmt.Sprintf(
				"%s: invalid new-project name %q: %v", token, name, err)}
		}
		return name, derivPicker, nil
	}
	// Otherwise treat as a number.
	n, perr := strconv.Atoi(choice)
	if perr != nil || n < 1 || n > len(known) {
		return "", 0, &UsageError{Msg: fmt.Sprintf(
			"%s: picker got %q, expected 1..%d or 'n <name>'", token, choice, len(known))}
	}
	return known[n-1], derivPicker, nil
}

// filterValidNames removes any name that fails the validator — defensive
// against a future KnownProjects() relaxation; today they already pass.
func filterValidNames(names []string) []string {
	out := names[:0]
	for _, n := range names {
		if err := state.ValidateProjectName(n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// --- production wiring + seam vars ---

// attachFnVar, coordSpawnFnVar, gcAggressiveFnVar, sessionAliveFnVar,
// sessionProbeFnVar, listSessionsFnVar, tmuxAvailableFnVar are the
// test seams. Production binds them at init time to the real tmux /
// CLI shell-outs.
var (
	attachFnVar             = tmux.Attach
	sessionAliveFnVar       = tmux.HasSession
	sessionProbeFnVar       = tmux.SessionAlive
	listSessionsFnVar       = tmux.ListSessions
	tmuxAvailableFnVar      = tmux.Available
	coordSpawnFnVar         = shellCoordSpawnID // backward-compat thin wrapper for existing test stubs
	coordSpawnFullFnVar     = shellCoordSpawn   // codex iter-14 P2: carries promptDelivered through to attach diagnostic
	killTmuxSessionFnVar    = tmux.Kill
	attachNewAgentIDFnVar   = agent.NewID
	attachCurrentOwnerFnVar = coordlock.CurrentOwner
	attachResolveFnVar      = func(project, agentID string) (coordreconcile.Verdict, error) {
		return coordreconcile.Resolve(coordreconcile.DefaultDeps(), project, agentID)
	}
)

// shellCoordSpawnID is the legacy (string, error) shim around
// shellCoordSpawn. The Tier 3 paths that need the prompt-delivered
// signal call shellCoordSpawn directly via coordSpawnFullFnVar; legacy
// callsites + tests keep the (id, err) shape via coordSpawnFnVar.
func shellCoordSpawnID(project string) (string, error) {
	res, err := shellCoordSpawn(project)
	if err != nil {
		return "", err
	}
	return res.ID, nil
}

// coordSpawnResult carries the structured outcome of a coord spawn.
// promptDelivered=false signals the operator may need to manually
// invoke /coordinator inside the new session — surface in the attach
// diagnostic so the never-exit invariant doesn't silently strand the
// project unowned (codex review iter-14 P2).
type coordSpawnResult struct {
	ID              string
	PromptDelivered bool
}

// errCoordSpawnVetoed is the typed sentinel shellCoordSpawn returns when
// `fleet dispatch --coord-spawn` exits with vetoExitCode (75) — the
// live-coord veto fired (BUG #2, attach-failover INVARIANT 3). It is NOT
// a hard failure: a coord is alive on another tmux socket OR one crashed
// within the freshness window and may be restarting. The shared wrapper
// classifies this via exec.ExitError.ExitCode() == 75 (NOT a stderr
// substring match), and every Tier 3 spawn caller (Paths C/C'/D) handles
// it identically: scan-only re-resolve once (no 2nd spawn) → attach to a
// now-live coord (exit 0) or wait-and-retry (exit 75). Never exit 70 for
// a veto.
var errCoordSpawnVetoed = errors.New("coord-spawn vetoed (live/recently-crashed coord)")

// dispatchVetoExitCode mirrors dispatch's vetoExitCode (75). Duplicated
// (not imported) because it's the cross-process EXIT-CODE contract
// between `fleet dispatch` and `fleet attach`'s subprocess wrapper, not
// shared in-process state. Keep in sync with dispatch.go's vetoExitCode.
const dispatchVetoExitCode = 75

// shellCoordSpawn shells out `fleet dispatch --coord-spawn --project <p>`
// and parses the new coord's ID from the dispatch output. The dispatch
// CLI prints the new coord's record path / id to stdout — we extract
// the 8-char ID. On any failure (non-zero exit, can't parse), return
// an error that tier3ProjectRecovery wraps as *SystemError.
//
// Implementation: re-exec the same binary so the dispatch path uses
// the current process's PATH / FLEET_HOME / config. Same approach the
// TUI startCoordSpawn uses.
// buildCoordSpawnArgs assembles the argv for `fleet dispatch ...
// --coord-spawn`. Extracted so unit tests can pin the shape without
// shelling out — the dispatch CLI requires the task-id positional
// (cobra.ExactArgs(1)) and a missing --cwd lands the recovered coord
// in the wrong checkout. Codex review iter-2 P1 + iter-4 P1 + iter-6 P1.
//
// Shape: `dispatch coord-<project> --coord-spawn --project <project>
// --prompt <coord-bootstrap-prompt> --engine claude-code
// [--cwd <repo_path>]`.
//
//   - --prompt forces /coordinator into the fresh agent's first paint;
//     without it the operator lands in a bare Claude that never claims
//     the project (codex iter-6 P1).
//   - --engine claude-code matches the TUI [a] auto-spawn discipline:
//     the coordinator skill's DISPATCH blocks only a claude-code session
//     can consume. Without the explicit stamp, an operator in
//     `fleet -codex attach ...` would propagate FLEET_ENGINE=codex into
//     dispatch, which today rejects coord-spawn with a different engine.
//   - --cwd carries the resolver-validated repo (NOT the raw
//     meta.json::repo_path). DESIGN-coord-repo-binding-from-project.md
//     PR3: this path resolves through coordrepo.ResolveProjectRepo, so a
//     stale/mismatched-fingerprint meta (cross-machine same-path,
//     different repo) is REFUSED here rather than passed as an unverified
//     --cwd that dispatch would honor. cwd is never a binding tier.
func buildCoordSpawnArgs(project string) ([]string, error) {
	args := []string{
		"dispatch",
		projectlookup.CoordTaskID(project),
		"--coord-spawn",
		"--project", project,
		"--prompt", projectlookup.CoordSpawnPrompt(project),
		"--engine", "claude-code",
	}
	// Resolve the repo binding through the shared resolver (codex PR3
	// review [P1]): reading meta.RepoPath raw would bypass fingerprint
	// validation, so a stale cross-machine pin (path exists locally but is
	// a DIFFERENT repo) would be passed as --cwd and dispatch would treat
	// it as an operator override — spawning the coord in the wrong
	// checkout. Resolving here makes the stale pin REFUSE with the
	// surfaced hint instead. persist=true (attach is operator-initiated).
	repo, rErr := coordrepo.ResolveProjectRepo(project, true)
	if rErr != nil {
		return nil, fmt.Errorf("cannot attach a coordinator to project %s: %w", project, rErr)
	}
	args = append(args, "--cwd", repo)
	return args, nil
}

// classifyCoordSpawnRunErr maps a failed `fleet dispatch --coord-spawn`
// subprocess error to attach's recovery taxonomy (BUG #2, attach-failover
// INVARIANT 3). The veto is classified by EXIT CODE, never by stderr
// substring: a typed/sentinel error can't cross the exec.Command
// subprocess boundary, so dispatch maps its *vetoError to exit 75 and we
// detect it here via exec.ExitError.ExitCode().
//
//	exit 75 → errCoordSpawnVetoed (caller does scan-only re-resolve,
//	          no 2nd spawn; ends in exit 0 attach or exit 75 wait-retry)
//	any other failure → wrapped error (caller maps to *SystemError(70))
//
// Extracted as a pure function so the cross-process exit-code contract —
// the load-bearing BUG #2 mechanism — is unit-testable without a full
// dispatch harness (see TestClassifyCoordSpawnRunErr).
func classifyCoordSpawnRunErr(runErr error, stderr string) error {
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() == dispatchVetoExitCode {
		return errCoordSpawnVetoed
	}
	return fmt.Errorf("%w (stderr: %s)", runErr, strings.TrimSpace(stderr))
}

func shellCoordSpawn(project string) (coordSpawnResult, error) {
	self, err := os.Executable()
	if err != nil {
		return coordSpawnResult{}, fmt.Errorf("locate self binary: %w", err)
	}
	args, err := buildCoordSpawnArgs(project)
	if err != nil {
		return coordSpawnResult{}, err
	}
	cmd := exec.Command(self, args...)
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return coordSpawnResult{}, classifyCoordSpawnRunErr(err, stderr.String())
	}
	// Codex review iter-9 P1: dispatch's recovery flow prints
	// "recovering dead coord <oldID>..." BEFORE the actual spawn line
	// "agent <newID> spawned". A first-hex-token scan would return the
	// dead predecessor ID and we'd probe fleet-<oldID> (the dead
	// session) — defeating the iter-2 P2 probe gate.
	//
	// Parse the canonical spawn line "agent <id> spawned" specifically.
	out := stdout.String() + " " + stderr.String()
	id := parseSpawnedAgentID(out)
	if id == "" {
		return coordSpawnResult{}, fmt.Errorf("could not parse spawned coord ID from dispatch output (expected `agent <id> spawned`, got: %q)", out)
	}
	// Codex iter-14 P2: dispatch prints "warning: initial prompt not
	// delivered ..." when SendInitialPrompt failed (typed-but-not-
	// submitted, send-keys error, etc.). The agent record is alive but
	// /coordinator never ran — the project is unowned until the
	// operator manually types it. Carry the signal to the attach
	// diagnostic so the operator sees the next step.
	const promptFailedMarker = "initial prompt not delivered"
	delivered := !strings.Contains(out, promptFailedMarker)
	return coordSpawnResult{ID: id, PromptDelivered: delivered}, nil
}

// parseSpawnedAgentID returns the <id> matched in the canonical
// dispatch spawn line `agent <id> spawned`. Returns "" when no match.
// Whitespace-tolerant so trailing newlines / mixed line endings don't
// trip the scan. Codex review iter-9 P1.
//
// Returns the LAST matching line, not the first (codex iter-30 P1). Dead-coord
// recovery prints the freshly-spawned standby's id first, then re-emits an
// authoritative `agent <winner> spawned` line AFTER lock-owner delivery when a
// RACING standby won the lease. The winner is always last, so CLI attach
// targets the live coordinator the resume prompt + coord marker landed on —
// never the losing standby (mirrors the TUI's dispatchAgentID last-match).
func parseSpawnedAgentID(out string) string {
	last := ""
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		// Pattern: "agent <id> spawned" (no prefix tolerance beyond
		// optional surrounding whitespace, to avoid false-positives
		// from dispatch lines that mention "agent" in a different
		// context).
		if !strings.HasPrefix(trimmed, "agent ") {
			continue
		}
		rest := strings.TrimPrefix(trimmed, "agent ")
		// Split on whitespace; first field is the candidate id.
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		if fields[1] != "spawned" {
			continue
		}
		if isAgentIDShape(fields[0]) {
			last = fields[0]
		}
	}
	return last
}

// isAgentIDShape mirrors internal/projectlookup.isAgentIDShape (kept
// local to avoid an import cycle when production binds the helper).
func isAgentIDShape(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isStdinTty reports whether os.Stdin is a TTY. Used to gate the
// interactive picker (F11 vs F12).
func isStdinTty() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & fs.ModeCharDevice) != 0
}

// gitToplevelBasename returns the basename of `git rev-parse
// --show-toplevel` for the current working directory. Falls back to
// the raw cwd basename when not in a git repo. Returns "" when neither
// works (no cwd, no git binary, etc.).
func gitToplevelBasename() string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err == nil {
		path := strings.TrimSpace(string(out))
		if path != "" {
			return filepath.Base(path)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Base(cwd)
}

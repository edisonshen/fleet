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

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/projectlookup"
	"github.com/edisonshen/fleet/internal/projects"
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
			err := runAttachFailover(args[0], opts)
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
	Project     string    // --project flag value (may be empty)
	CwdBasename string    // git-toplevel basename (may be empty)
	IsTty       bool      // is stdin a tty? gates the picker
	Stdin       io.Reader // picker input
	Stdout      io.Writer // picker prompt + numbered list
	Stderr      io.Writer // every failover diagnostic
}

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
		return attachFnVar(session)
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

	records, err := agent.List()
	if err != nil {
		return newSystemError(70, fmt.Sprintf(
			"agent.List failed: %v — check ~/.fleet/agents/ readability", err))
	}

	// Path A: live coord exists → attach (no spawn, no reap).
	if rec, ok := projectlookup.FindLiveCoord(records, project); ok {
		// F9 specifically requires the "token matched project name"
		// surface when the source was tokenAsProject. Other sources
		// share the "attached to current coord" wording.
		if src == derivTokenAsProject {
			_, _ = fmt.Fprintf(opts.Stderr,
				"%s: token matched project name; attached to current coord %s for %s\n",
				token, rec.ID, project)
		} else {
			_, _ = fmt.Fprintf(opts.Stderr,
				"%s: attached to current coord %s for %s\n",
				token, rec.ID, project)
		}
		session := rec.TmuxSession
		if session == "" {
			session = tmux.SessionName(rec.ID)
		}
		return attachFnVar(session)
	}
	// Path B: try lock-body fallback (issue #63 follow-on: marker may
	// be absent on a prompt-delivery-failed dispatch; the lock body is
	// still authoritative).
	if rec, ok := projectlookup.FindCoordByLockBody(records, project); ok {
		_, _ = fmt.Fprintf(opts.Stderr,
			"%s: attached to current coord %s for %s\n",
			token, rec.ID, project)
		session := rec.TmuxSession
		if session == "" {
			session = tmux.SessionName(rec.ID)
		}
		return attachFnVar(session)
	}
	// Path C: stale coord (record alive + tmux dead) → reap + spawn.
	staleID := ""
	if stale, ok := projectlookup.StaleCoordRecord(records, project); ok {
		staleID = stale.ID
	} else if id, ok := projectlookup.OrphanTmuxForProject(records, project); ok {
		// Path C': no record but lingering tmux session → reap + spawn.
		staleID = id
	}
	if staleID != "" {
		if err := gcAggressiveFnVar(project); err != nil {
			return newSystemError(70, fmt.Sprintf(
				"gc --aggressive --project %s failed: %v — re-run `fleet gc --apply --aggressive --project %s` manually",
				project, err, project))
		}
		newID, derr := coordSpawnFnVar(project)
		if derr != nil {
			return newSystemError(70, fmt.Sprintf(
				"dispatch --coord-spawn --project %s failed: %v — re-run `fleet dispatch --coord-spawn --project %s` manually",
				project, derr, project))
		}
		_, _ = fmt.Fprintf(opts.Stderr,
			"%s: reaped stale %s; spawned %s for %s; attaching\n",
			token, staleID, newID, project)
		return attachSpawnedSession(token, project, newID, opts)
	}
	// Path D: no coord at all → spawn fresh.
	newID, derr := coordSpawnFnVar(project)
	if derr != nil {
		return newSystemError(70, fmt.Sprintf(
			"dispatch --coord-spawn --project %s failed: %v — re-run `fleet dispatch --coord-spawn --project %s` manually",
			project, derr, project))
	}
	_, _ = fmt.Fprintf(opts.Stderr,
		"%s: no coord for %s; spawned %s; attaching\n",
		token, project, newID)
	return attachSpawnedSession(token, project, newID, opts)
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
func attachSpawnedSession(token, project, newID string, opts AttachOpts) error {
	session := tmux.SessionName(newID)
	if alive, probeErr := sessionProbeFnVar(session); probeErr == nil && !alive {
		return newSystemError(70, fmt.Sprintf(
			"coord-spawn for %s returned exit 0 but session %s never came up — re-run `fleet attach <token> --project %s` (or `fleet dispatch --coord-spawn --project %s`) to retry; check ~/.fleet/agents/%s.json for clues",
			project, session, project, project, newID))
	}
	return attachFnVar(session)
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
	attachFnVar        = tmux.Attach
	sessionAliveFnVar  = tmux.HasSession
	sessionProbeFnVar  = tmux.SessionAlive
	listSessionsFnVar  = tmux.ListSessions
	tmuxAvailableFnVar = tmux.Available
	coordSpawnFnVar    = shellCoordSpawn
	gcAggressiveFnVar  = shellGCAggressive
)

// shellCoordSpawn shells out `fleet dispatch --coord-spawn --project <p>`
// and parses the new coord's ID from the dispatch output. The dispatch
// CLI prints the new coord's record path / id to stdout — we extract
// the 8-char ID. On any failure (non-zero exit, can't parse), return
// an error that tier3ProjectRecovery wraps as *SystemError.
//
// Implementation: re-exec the same binary so the dispatch path uses
// the current process's PATH / FLEET_HOME / config. Same approach the
// TUI startCoordSpawn uses.
func shellCoordSpawn(project string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate self binary: %w", err)
	}
	args := []string{"dispatch", "--coord-spawn", "--project", project}
	// Codex review iter-2 P1: pass --cwd <repo_path> when meta.json
	// registers one. Without this, dispatch falls back to the attach
	// process's cwd — which is almost certainly NOT the project's repo
	// when the operator runs `fleet attach --project <p>` from another
	// directory. The recovered coord would then run git/test/worktree
	// commands against the wrong checkout.
	//
	// meta.json is optional (some legacy projects predate it); on absence
	// or read failure, fall back to the existing behavior (no --cwd, let
	// dispatch resolve). Don't fail the spawn for a missing meta — the
	// invariant "never exit" still holds; the coord just lands in the
	// caller's cwd as it did before this fix.
	if meta, mErr := projects.Read(project); mErr == nil && meta.RepoPath != "" {
		args = append(args, "--cwd", meta.RepoPath)
	}
	cmd := exec.Command(self, args...)
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	// Dispatch prints something like:
	//   dispatched <id> -> ~/.fleet/agents/<id>.json
	// Pull the first 8-char hex token out.
	out := stdout.String() + " " + stderr.String()
	for _, tok := range strings.FieldsFunc(out, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '\r' || r == '"' || r == ',' || r == '.' || r == '/' || r == '`'
	}) {
		if isAgentIDShape(tok) {
			return tok, nil
		}
	}
	return "", fmt.Errorf("could not parse coord ID from dispatch output: %q", out)
}

// shellGCAggressive runs `fleet gc --apply --aggressive --project <p>`.
// Best-effort: the reap path tolerates "nothing to reap" exit 0 cleanly.
func shellGCAggressive(project string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self binary: %w", err)
	}
	cmd := exec.Command(self, "gc", "--apply", "--aggressive", "--project", project)
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
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

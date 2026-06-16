// Package spawn is the shared "create a new agent in a tmux session
// and write its record" code path used by both `fleet dispatch` (no
// origin) and `fleet handoff` (origin = OldRecord).
//
// Centralizing here means the chain-field logic (HandoffNumber,
// LastHandoffPath, HandoffType) lives in one place. Dispatch and
// handoff differ only in whether they pass an OldRecord.
//
// Failure mode: if the tmux session comes up but the agent record
// write fails, kill the orphan session before returning. Operators
// must never see a "ghost" session with no record (or a record
// pointing at a dead session).
package spawn

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// ErrCoordStoodDown is returned by Spawn when a lease-wrapped coord-run
// supervisor stood down (a healthy leader already holds the coordinator
// lease) or exited before Spawn finished — its agent record was archived
// by the supervisor's cleanup, so there is NO live agent to prompt or
// attach. Callers (runDispatch) detect this with errors.Is and report a
// clean "a coord is already running" outcome (exit 0) rather than a
// successful-spawn message for a dead id. DESIGN-handoff-drain-storm-leak
// PR2 (codex iter-5 [P2]).
var ErrCoordStoodDown = errors.New("spawn: coord supervisor stood down (a coord is already running for this project)")

// SendInitialPrompt timing knobs. Production needs to ride out
// claude code's startup animation (logo + spinner before the input
// box appears) without staking the delivery on a single fixed sleep
// — custom engine wrappers (per oldRec.Command) can take longer than
// any one number we'd pick. Instead: poll the pane content; once it
// stops changing for stableWindow we treat the agent as ready.
//
// FLEET_INITIAL_PROMPT_STABLE_MS / FLEET_INITIAL_PROMPT_MAX_MS let
// tests pin small values so the suite doesn't pay multi-second
// real-world waits; production uses the constants below.
const (
	defaultInitialPromptStableWindow = 500 * time.Millisecond
	defaultInitialPromptMaxWait      = 30 * time.Second
	initialPromptPollInterval        = 100 * time.Millisecond

	// defaultPromptEnterDelay is the gap between sending the prompt
	// text and sending the Enter key. See SendPromptKeys for why this
	// can't be zero.
	//
	// Bumped from 200ms → 1000ms (issue #65). Operator setup with the
	// stock 200ms gap consistently delivered the prompt text but lost
	// the Enter — Claude Code's bracketed-paste detection still treated
	// the prompt as in-flight when Enter arrived ~200ms later, so the
	// CR landed inside the input buffer as a literal newline rather
	// than a submit. 1s gives the paste window enough slack to close
	// across CI runners, slow shells, and the operator's box. Cost:
	// ~1s added to every coord/handoff spawn — acceptable for "the
	// /coordinator skill actually starts" reliability.
	defaultPromptEnterDelay = 1000 * time.Millisecond

	// defaultPostReadyBuffer is an additional sleep after
	// waitForPaneStable returns, before SendPromptKeys actually types.
	// Pane-stability is necessary but not sufficient for "input box is
	// ready": Claude Code can be stable on its splash screen, onboarding
	// prompt, version-update notice, or model-selection screen — none
	// of which actually accept user input via the prompt box. Without
	// this buffer, send-keys lands during one of those pre-input-ready
	// states and is consumed by something else (a button selector,
	// "press any key" wait, etc.). 1.5s is empirically enough to ride
	// out the typical post-stability animation gap on a fresh Claude
	// Code launch (issue #65 symptom B). Env-overridable via
	// FLEET_POST_READY_BUFFER_MS for tuning / fast tests.
	defaultPostReadyBuffer = 1500 * time.Millisecond

	// defaultPostSendVerifyDelay is how long we wait after Enter
	// before capturing the pane to check whether the prompt was
	// submitted. 500ms gives Claude time to either echo the prompt as
	// a "user turn" (submitted) or leave it sitting verbatim in the
	// input box (not submitted). Shorter risks a false "still in
	// input" because Claude hasn't redrawn yet. Capped tight so the
	// verify path doesn't add multi-second latency to the happy case.
	// Tests pin via FLEET_POST_SEND_VERIFY_MS to keep verifier-path
	// tests fast.
	defaultPostSendVerifyDelay = 500 * time.Millisecond

	// defaultPostSendRetryDelay is the gap before re-sending Enter on
	// the retry path. Always longer than the first-pass
	// defaultPromptEnterDelay since the first pass already failed. We
	// re-send Enter alone (the prompt text is already in the input
	// buffer per the still-unsubmitted observation), wait this long
	// for Claude to clear paste mode, then submit. Tests pin via
	// FLEET_POST_SEND_RETRY_MS.
	defaultPostSendRetryDelay = 1500 * time.Millisecond

	// unsubmittedTailLines is how many lines from the bottom of a
	// capture-pane snapshot we scan to decide whether the prompt is
	// still in Claude's input box. Claude Code's input box renders at
	// the very bottom (input field plus border), so a small tail
	// window captures it without being polluted by the submitted-
	// transcript echo higher up. 12 covers a multi-line wrapped prompt
	// plus the box border on standard 80-col terminals.
	unsubmittedTailLines = 12
)

var (
	supervisorStampWaitAttempts = 25
	supervisorStampWaitDelay    = 100 * time.Millisecond
)

func waitForSupervisorStamp(agentID string) {
	for i := 0; i < supervisorStampWaitAttempts; i++ {
		rec, err := agent.Load(agentID)
		if errors.Is(err, state.ErrNotFound) {
			return
		}
		if err == nil && rec.SupervisorPID > 0 {
			return
		}
		time.Sleep(supervisorStampWaitDelay)
	}
}

func initialPromptStableWindow() time.Duration {
	return envDuration("FLEET_INITIAL_PROMPT_STABLE_MS",
		defaultInitialPromptStableWindow)
}

func initialPromptMaxWait() time.Duration {
	return envDuration("FLEET_INITIAL_PROMPT_MAX_MS",
		defaultInitialPromptMaxWait)
}

func promptEnterDelay() time.Duration {
	return envDuration("FLEET_PROMPT_ENTER_DELAY_MS",
		defaultPromptEnterDelay)
}

func postReadyBuffer() time.Duration {
	return envDuration("FLEET_POST_READY_BUFFER_MS",
		defaultPostReadyBuffer)
}

func postSendVerifyDelay() time.Duration {
	return envDuration("FLEET_POST_SEND_VERIFY_MS",
		defaultPostSendVerifyDelay)
}

func postSendRetryDelay() time.Duration {
	return envDuration("FLEET_POST_SEND_RETRY_MS",
		defaultPostSendRetryDelay)
}

// propagatedRuntimeEnv lists FLEET_* vars whose values must reach the
// agent's tmux session so anything launched from inside the pane
// (notably `_kick_drain → fleet drain`) sees the same configuration
// the operator's fleet was started with. tmux strips non-`-e` vars
// when the server is already running, so we forward these explicitly.
//
// Excluded by design:
//   - FLEET_AGENT_ID is set per-agent on the same tmux -e flight.
//   - FLEET_BIN is computed per-spawn (os.Executable()).
//   - FLEET_ROLE / FLEET_MODE are part of agent.Record, set elsewhere.
//   - FLEET_TEST_PROBE is test-only.
var propagatedRuntimeEnv = []string{
	"FLEET_HOME",                     // queue/handoffs/agents root
	"FLEET_TMUX_SOCKET",              // alt-server isolation
	"FLEET_INITIAL_PROMPT_STABLE_MS", // prompt-timing for slow wrappers
	"FLEET_INITIAL_PROMPT_MAX_MS",
	"FLEET_PROMPT_ENTER_DELAY_MS",
	"FLEET_POST_READY_BUFFER_MS",
	"FLEET_POST_SEND_VERIFY_MS",
	"FLEET_POST_SEND_RETRY_MS",
	// FLEET_ENGINE carries the operator-chosen engine across spawn
	// boundaries so the coordinator skill (Python loop.py + dispatch.py)
	// can read its own engine from os.environ without having to round-
	// trip through ~/.fleet/agents/<coord_id>.json. Used by the
	// reviewer-subagent-arch builders to decide which engine the
	// reviewer subagent runs (claude pinch-hits when coord=codex).
	"FLEET_ENGINE",
	// FLEET_LEASE_FAILOVER must reach the agent's tmux session so the
	// wrapped `fleet coord-run` supervisor sees the same flag the
	// operator's fleet was started with (DESIGN-handoff-drain-storm-leak
	// PR2). tmux strips non-`-e` vars against an already-running server,
	// so without this the supervisor would read the flag as OFF, return
	// ErrFailoverDisabled, and run the child WITHOUT acquiring or
	// heartbeating the lease — the failover path would silently no-op in
	// the common case (codex PR2 iter-1 [P1]). It also propagates so a
	// `fleet drain` / handoff-spawned replacement launched from inside
	// the pane inherits the same failover state.
	"FLEET_LEASE_FAILOVER",
	// FLEET_STANDBY_TIMEOUT (PR-A fork-bomb fence, test-only) must reach the
	// session so a NESTED coord spawn from inside a pane (e.g. a coord that
	// runs `fleet drain`/handoff and lease-wraps a successor) inherits the
	// short test timeout too. Without forwarding, the first-level standby is
	// bounded but a nested `coord-run --standby` would fall back to the 10m
	// default and re-open the orphan-pane window the fence closes (codex
	// iter-3 [P1]). Production never sets it, so it stays a no-op there.
	"FLEET_STANDBY_TIMEOUT",
}

// leaseFailoverEnabled gates whether Spawn wraps a coord in the
// `fleet coord-run` lease supervisor. It is build-tagged:
//
//   - linux||darwin (spawn_lease_unix.go): mirror coordlock.FailoverEnabled's
//     tri-state (PR4 default ON; 0/false/off/no disables).
//   - other GOOS (spawn_lease_other.go): ALWAYS false — the lease primitive
//     (coordlock) is unsupported there, and the cmd-side coord-run stub
//     promises the legacy bare-child path, so Spawn must NOT lease-wrap
//     (codex PR4 [P2]).
//
// Splitting by build tag keeps the all-platform spawn.go free of a hard
// coordlock import (which would break non-unix builds) while preventing the
// default-ON flag from wrapping coords on a platform where the supervisor
// can't actually hold a lease.

// DefaultStandbyTimeout is the single generous bound every lease-wrapped
// coordinator spawn passes to `fleet coord-run --standby`.
const DefaultStandbyTimeout = 10 * time.Minute

// shouldLeaseWrap decides whether this spawn wraps the coord in the
// `fleet coord-run` lease supervisor. PR1 collapses fresh dispatch and
// dead-coord recovery onto coord-run --standby when failover is enabled, while
// workers, unsupported platforms, and explicit legacy fallbacks stay bare.
func shouldLeaseWrap(failoverOn, isCoord, disable bool) bool {
	return failoverOn && isCoord && !disable
}

// coordRunWrap builds the lease-failover exec argv: the engine argv
// supervised by `fleet coord-run --standby`.
// Pure + does not mutate argv. The persisted rec.Command stays clean;
// only the EXECUTION argv is wrapped. `--standby` polls a busy healthy
// leader until it releases or the standby timeout expires.
//
//	["<fleetBin>","coord-run","--agent",<id>,"--project",<p>,
//	 "--standby","--standby-timeout",<T>,"--",<argv...>]
func coordRunWrap(fleetBin, agentID, project string, standbyTimeout time.Duration, argv []string) []string {
	standbyTimeout = standbyTimeoutOrDefault(standbyTimeout)
	wrapped := make([]string, 0, len(argv)+10)
	wrapped = append(wrapped, fleetBin, "coord-run",
		"--agent", agentID, "--project", project)
	wrapped = append(wrapped, "--standby", "--standby-timeout", standbyTimeout.String())
	wrapped = append(wrapped, "--")
	return append(wrapped, argv...)
}

// alreadyCoordRunWrapped reports whether argv already starts a
// `fleet coord-run` supervisor (basename "fleet"-ish + the "coord-run"
// subcommand). Idempotency guard so a caller that pre-wrapped the
// ExecCommand isn't double-wrapped, and so a buggy re-spawn can't nest
// coord-run inside coord-run.
func alreadyCoordRunWrapped(argv []string) bool {
	return len(argv) >= 2 && argv[1] == "coord-run" &&
		strings.Contains(strings.ToLower(filepath.Base(argv[0])), "fleet")
}

// standbyTimeoutOrDefault resolves the bound passed to `coord-run --standby`.
//
// FLEET_STANDBY_TIMEOUT is a TEST-ONLY seam (same shape + caveat as the
// existing FLEET_PID_RESOLVE_S seam, pidresolver.go): when set to a valid >0
// duration it wins UNCONDITIONALLY, overriding even an explicit caller-supplied
// d. Unconditional is REQUIRED for two reasons:
//
//  1. The non-Spawn entry points (runHandoff/runDispatch/Resume/GracefulHandoff)
//     pass an explicit StandbyTimeout: DefaultStandbyTimeout, so a d<=0-gated
//     read could never reach them.
//  2. The integration lane also execs a REAL `fleet` binary as a subprocess
//     (cmd/fleet/coord_run_lease_integration_test.go sets FLEET_STANDBY_TIMEOUT
//     in cmd.Env). In that child, `testing.Testing()` is false, so a
//     test-build-only gate would NOT shrink the child's standby — leaving the
//     exact long-lived `coord-run --standby` pane the fence exists to bound
//     (codex iter-2 [P1]). The env read must therefore be unconditional.
//
// The integration lane sets "3s" so an orphaned standby self-reaps in seconds
// instead of looping for 10m and fork-bombing the box. Production NEVER sets
// this variable (it is fleet-internal and undocumented for users — identical to
// FLEET_PID_RESOLVE_S), so production is a pure no-op: the default stays
// DefaultStandbyTimeout and `--standby-timeout` / the caller's d win.
func standbyTimeoutOrDefault(d time.Duration) time.Duration {
	if raw := strings.TrimSpace(os.Getenv("FLEET_STANDBY_TIMEOUT")); raw != "" {
		if env, err := time.ParseDuration(raw); err == nil && env > 0 {
			return env
		}
	}
	if d <= 0 {
		return DefaultStandbyTimeout
	}
	return d
}

func augmentCoordRunWrap(argv []string, standbyTimeout time.Duration) []string {
	standbyTimeout = standbyTimeoutOrDefault(standbyTimeout)
	sep := len(argv)
	for i, arg := range argv {
		if arg == "--" {
			sep = i
			break
		}
	}

	head := argv[:sep]
	tail := argv[sep:]
	out := make([]string, 0, len(argv)+3)
	hasStandby := false
	for i := 0; i < len(head); i++ {
		arg := head[i]
		switch {
		case arg == "--standby":
			hasStandby = true
			out = append(out, arg)
		case arg == "--standby-timeout":
			if i+1 < len(head) {
				i++
			}
		case strings.HasPrefix(arg, "--standby-timeout="):
			// Replaced below with the caller's current timeout value.
		default:
			out = append(out, arg)
		}
	}
	if !hasStandby {
		out = append(out, "--standby")
	}
	out = append(out, "--standby-timeout", standbyTimeout.String())
	return append(out, tail...)
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if s := os.Getenv(key); s != "" {
		if ms, err := strconv.Atoi(s); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return fallback
}

// WaitForReadyToPrompt polls the tmux session's pane until it
// stabilizes (claude code's startup animation finished, input box
// rendered), or maxWait elapses. Returns nil on stable convergence,
// or an error if the poll didn't converge.
//
// After the stability poll, an additional postReadyBuffer sleep
// runs (default 1.5s, env-overridable via FLEET_POST_READY_BUFFER_MS).
// The buffer is required because pane-stability is a necessary-but-
// not-sufficient signal for "Claude's input box accepts text" —
// Claude can be stable on its splash, onboarding, version-update,
// or model-selection screens. Send-keys during one of those
// intermediate stable states is captured by something other than
// the input box (issue #65 symptom B). The buffer rides out the
// gap empirically. On stability-poll error (didn't converge) we
// SKIP the buffer — the pane is already long-late so adding more
// delay just makes the failure-path slower without helping.
//
// Callers should run this BEFORE queue.Delete so a crash during
// the wait remains recoverable: the queue file is still on disk
// and resumeHandoff / handoffop.Resume will re-run the wait + send
// on retry. Pairing this with SendPromptKeys (run AFTER
// queue.Delete) gives at-most-once delivery with a microsecond-
// scale lost-prompt window, instead of the 30 s window an in-one-
// function design has (codex review iter-5 P2).
//
// Best-effort: caller should NOT abort on this returning error —
// just call SendPromptKeys anyway. Keys queue in tmux's pty buffer
// and the agent consumes them once ready.
func WaitForReadyToPrompt(session string) error {
	if err := waitForPaneStable(session,
		initialPromptStableWindow(),
		initialPromptMaxWait()); err != nil {
		return err
	}
	if buf := postReadyBuffer(); buf > 0 {
		time.Sleep(buf)
	}
	return nil
}

// SendPromptKeys types prompt + Enter into the tmux session, then
// verifies the prompt was actually submitted (and retries Enter once
// if not). No readiness wait — caller must have called
// WaitForReadyToPrompt first (or accept that keys may land before
// claude is ready, in which case they queue in the pty).
//
// Empty prompt is a silent no-op so callers can pass
// handoff.ResumePrompt(docPath) without nil-checking docPath.
//
// The prompt and Enter are sent as TWO separate `tmux send-keys`
// invocations with a small sleep between them. Single-invocation
// `tmux send-keys <prompt> Enter` streams the prompt bytes and the
// trailing CR into the pty as one contiguous burst, and Claude Code
// (and other modern TUIs) treat such a burst as a paste — the CR
// becomes a literal newline inside the pasted content rather than a
// submit. End result without the split: the resume prompt sits in
// the input box and the agent waits for the operator to press Enter
// manually, defeating the whole point of auto-resume. The 1 s gap
// (was 200 ms — see issue #65) is enough for claude's input handler
// to close the paste window before the Enter arrives as its own
// keystroke event.
//
// Post-send verification (issue #65 symptom A): even with the 1 s
// gap, paste-detection occasionally swallows Enter. After the
// initial send we sleep postSendVerifyDelay, capture the pane, and
// check whether the prompt text is still sitting in the input box
// (an unsubmitted prompt). If yes: re-send Enter alone after
// postSendRetryDelay, capture again, log if still unsubmitted.
// Returns nil even on still-unsubmitted — the caller (dispatch /
// handoff) should still attach successfully so the operator can
// manually press Enter.
//
// ASCII flow:
//
//	send <prompt>     ──┐
//	  promptEnterDelay  │ split prevents paste-detection swallowing Enter
//	send Enter        ──┘
//	  postSendVerifyDelay
//	capture pane → contains <prompt>?
//	  no  → submitted, return nil
//	  yes → not submitted; sleep postSendRetryDelay, send Enter again
//	         capture pane → contains <prompt>?
//	           no  → submitted on retry, return nil
//	           yes → log warning, return nil (caller still attaches)
func SendPromptKeys(session, prompt string) error {
	_, err := SendPromptKeysVerified(session, prompt)
	return err
}

// SendPromptKeysVerified is SendPromptKeys with the verification
// outcome surfaced. submitted=true means we observed the prompt
// leave the input box (success), OR pane capture failed and we
// couldn't tell — the latter is a conservative report so a transient
// tmux glitch during verification doesn't drive a spurious retry
// (see promptSubmittedWithDeps). submitted=false means we positively
// observed the prompt sitting in Claude's input box even after the
// retry — the caller should surface a "manual Enter needed" hint to
// the operator.
//
// err is non-nil ONLY for the initial send-keys failures (the prompt
// or first Enter never reached tmux). Verification failures DO NOT
// surface as err — they surface as submitted=false.
//
// Empty prompt is a silent no-op: returns (true, nil).
//
// Used by dispatch.go to log a structured "prompt unsubmitted"
// warning to operator-visible stdout (issue #65 Fix D). Other
// callers (handoffop, cmd/fleet/handoff.go) use SendPromptKeys
// and rely on the stderr warning emitted by the verifier — they
// don't need to act on the boolean.
func SendPromptKeysVerified(session, prompt string) (submitted bool, err error) {
	if prompt == "" {
		return true, nil
	}
	if err := tmux.SendKeys(session, prompt); err != nil {
		return false, err
	}
	time.Sleep(promptEnterDelay())
	if err := tmux.SendKeys(session, "Enter"); err != nil {
		return false, err
	}
	return verifyAndRetry(session, prompt), nil
}

// verifyAndRetry runs the post-send verification + one-shot retry
// using the production tmux send-keys / capture-pane primitives. See
// verifyAndRetryWithDeps for the testable inner core.
func verifyAndRetry(session, prompt string) bool {
	return verifyAndRetryWithDeps(session, prompt,
		tmux.SendKeys, tmux.CapturePane,
		os.Stderr)
}

// verifyAndRetryWithDeps is verifyAndRetry's testable core. It takes
// send-keys / capture-pane primitives + a writer for warnings so a
// unit test can plug stub implementations and assert behavior
// deterministically.
//
// Returns true if the prompt was submitted (either on the first
// Enter or the retry); false if it remained unsubmitted.
//
// On still-unsubmitted after retry, writes a warning to warnW. The
// TUI / dispatch caller surfaces this via SendPromptKeysVerified's
// boolean return; this writer is for log-correlation analysis later.
//
// Errors during the retry's send-keys are non-fatal — we log them
// and trust whatever the last known submission state was. The point
// of the verifier is to FIX a known regression (Symptom A: Enter
// eaten by paste detection); we never want verification to itself
// become a new failure surface.
func verifyAndRetryWithDeps(
	session, prompt string,
	sendKeys func(session string, keys ...string) error,
	capturePane func(session string) ([]byte, error),
	warnW io.Writer,
) bool {
	time.Sleep(postSendVerifyDelay())
	if promptSubmittedWithDeps(session, prompt, capturePane) {
		return true
	}
	// Retry: prompt is sitting in the input box. Send Enter alone
	// after a longer pause to ride out whatever caused the first
	// Enter to be swallowed.
	time.Sleep(postSendRetryDelay())
	if err := sendKeys(session, "Enter"); err != nil {
		_, _ = fmt.Fprintf(warnW,
			"warning: post-send retry Enter failed for %s: %v\n",
			session, err)
		return false
	}
	time.Sleep(postSendVerifyDelay())
	if promptSubmittedWithDeps(session, prompt, capturePane) {
		return true
	}
	_, _ = fmt.Fprintf(warnW,
		"warning: prompt for %s appears unsubmitted after retry; attach and press Enter manually\n",
		session)
	return false
}

// promptSubmittedWithDeps captures the pane via the supplied function
// and returns true if the prompt is NOT still visible in the BOTTOM
// band of the pane — i.e., it has left the input box.
//
// We restrict the search to the last unsubmittedTailLines lines of
// the capture to avoid a false-positive from the submitted
// transcript: when Claude Code accepts a user turn, it clears the
// input box AND re-renders the user message higher up in the
// conversation transcript (typically with a `>` prefix). A naive
// whole-pane substring match would see that transcript line and
// spuriously retry. Restricting to the bottom band catches the
// "still in input box" case (input box renders at the very bottom
// of the pane) while ignoring the post-submit transcript echo.
//
// Capture errors are conservatively treated as "submitted" so the
// verifier doesn't loop on transport hiccups; we'd rather miss a
// retry than run the retry incorrectly. The corollary: a transient
// tmux failure during verification masks the real submission state,
// but that's acceptable because (a) the initial Enter still ran,
// and (b) the post-Enter pane is the only signal we have.
func promptSubmittedWithDeps(
	session, prompt string,
	capturePane func(session string) ([]byte, error),
) bool {
	pane, err := capturePane(session)
	if err != nil {
		return true
	}
	tail := tailLines(pane, unsubmittedTailLines)
	return !bytes.Contains(tail, []byte(prompt))
}

// tailLines returns the last n lines of buf. n <= 0 returns buf
// unchanged. If buf has fewer than n lines, returns buf.
func tailLines(buf []byte, n int) []byte {
	if n <= 0 {
		return buf
	}
	count := 0
	// Walk backwards; each '\n' marks a line boundary.
	for i := len(buf) - 1; i >= 0; i-- {
		if buf[i] != '\n' {
			continue
		}
		count++
		if count > n {
			return buf[i+1:]
		}
	}
	return buf
}

// SendInitialPrompt is the wait-then-send pair as a single call.
// Convenient for tests and for paths where there's no transactional
// boundary to split around (no queue file to worry about). Production
// handoff callers should use WaitForReadyToPrompt + SendPromptKeys
// directly so they can interleave queue.Delete between them.
func SendInitialPrompt(session, prompt string) error {
	if prompt == "" {
		return nil
	}
	if err := WaitForReadyToPrompt(session); err != nil {
		_, _ = fmt.Fprintf(os.Stderr,
			"warning: initial-prompt readiness poll for %s did not converge: %v (sending anyway)\n",
			session, err)
	}
	return SendPromptKeys(session, prompt)
}

// waitForPaneStable polls tmux capture-pane every
// initialPromptPollInterval; returns nil when the pane content has
// not changed for at least stableWindow, or an error if maxWait
// elapses without convergence.
//
// "Stable" is a coarse heuristic for "agent is idle waiting for
// input" — works for any wrapper that prints a startup banner then
// settles, regardless of whether it's claude, codex, or a custom
// shell. Empty captures count toward stability (codex review iter-4
// P2): wrappers that `clear` the screen at startup leave the pane
// blank-but-idle, and gating on len(cur) > 0 would mean those
// wrappers never converge and every handoff stalls for the full
// maxWait.
func waitForPaneStable(session string, stableWindow, maxWait time.Duration) error {
	return waitForPaneStableWithDeps(stableWindow, maxWait,
		func() ([]byte, error) { return tmux.CapturePane(session) },
		time.Sleep, time.Now)
}

// waitForPaneStableWithDeps is waitForPaneStable's testable core.
// Seams: capture (pane-content fetch), sleep (between polls), now
// (deadline math). Tests inject a fake clock + fake capture so the
// deadline contract can be exercised deterministically without real
// time or a real tmux server.
//
// Deadline discipline: the check happens at the LOOP HEADER, before
// any blocking call. The trailing sleep is capped at the remaining
// time-to-deadline, so a single scheduler-jittered sleep cannot
// push elapsed past maxWait by more than the OS's wakeup slop. The
// pre-fix code checked the deadline AFTER the capture but BEFORE
// the sleep, then unconditionally slept the full poll interval —
// on a busy CI runner that left elapsed ≈ maxWait + 100ms +
// scheduler-jitter, which periodically tripped the
// SkipsBufferOnUnstable test's 2s slack.
func waitForPaneStableWithDeps(
	stableWindow, maxWait time.Duration,
	capture func() ([]byte, error),
	sleep func(time.Duration),
	now func() time.Time,
) error {
	start := now()
	deadline := start.Add(maxWait)
	var prev []byte
	first := true
	stableSince := time.Time{}
	for {
		// Deadline check at the loop HEADER, before capture or sleep.
		// Guarantees we never start a fresh poll iteration past the
		// budget. A fresh deadline check also runs after the
		// (capped) sleep on the next iteration's header pass.
		if !now().Before(deadline) {
			return fmt.Errorf("pane did not stabilize within %s", maxWait)
		}
		cur, err := capture()
		if err != nil {
			return err
		}
		// First iteration always counts as "changed" — we have nothing
		// to compare against. Subsequent iterations: equal to prev =>
		// stable, otherwise reset the stable timer.
		if !first && bytes.Equal(cur, prev) {
			if stableSince.IsZero() {
				stableSince = now()
			} else if now().Sub(stableSince) >= stableWindow {
				return nil
			}
		} else {
			stableSince = time.Time{}
		}
		prev = cur
		first = false
		// Cap the sleep at remaining time-to-deadline so a full
		// poll-interval sleep can't overshoot. If remaining <= 0
		// the next loop header trips the deadline immediately.
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			continue
		}
		nap := initialPromptPollInterval
		if remaining < nap {
			nap = remaining
		}
		sleep(nap)
	}
}

// Options control a single Spawn call.
//
// OldRecord nil → fresh dispatch (uses TaskID, Project, no chain).
// OldRecord non-nil → handoff replacement (inherits TaskID/Project/
// Engine/Role/Mode from old, increments HandoffNumber, sets
// LastHandoffPath = NewDocPath if non-empty).
type Options struct {
	OldRecord *agent.Record

	// NewDocPath is the path to the handoff doc this new agent
	// inherits. Only meaningful when OldRecord is non-nil. Stored on
	// the new record's LastHandoffPath so the *next* handoff can
	// build the chain forward.
	NewDocPath string

	// TaskID and Project are used only for fresh dispatch. Ignored
	// when OldRecord is non-nil — the handoff inherits those fields.
	TaskID  string
	Project string

	// Engine is the engine identifier persisted on agent.Record.Engine
	// (e.g. "claude-code", "codex"). Empty means "leave agent.New's
	// DefaultEngine in place" — preserves the v0 byte-shape on the
	// happy path. Ignored when OldRecord is non-nil; the handoff
	// already inherits oldRec.Engine on the existing override path.
	Engine string

	// Cwd is the working directory for the spawned tmux session.
	// Empty inherits the caller's cwd.
	Cwd string

	// Command is the argv of the agent process inside tmux. Defaults
	// to {"claude"} via the dispatch CLI; spawn.Spawn does not
	// default — callers must pass it explicitly so the contract is
	// obvious.
	Command []string

	// PreAllocatedID, if non-empty, overrides the agent.NewID()
	// fresh-allocation. Handoff uses this to journal the successor
	// ID BEFORE spawning, closing the crash window between spawn
	// and journal-write. Empty (the dispatch path) means generate
	// a fresh ID inside Spawn.
	PreAllocatedID string

	// DisableAutoResume sets the new record's DisableAutoResume.
	// Caller computes the right value: dispatch passes the CLI flag
	// directly; handoff inherits from OldRecord by default but the
	// operator can override via `--no-auto-resume` when handing off
	// into a different command class (codex review iter-8 P2).
	DisableAutoResume bool

	// ExecCommand is the argv tmux actually runs IF non-empty,
	// overriding Command for execution only. The persisted agent
	// record keeps Command (the "clean" form) so handoff successors
	// don't inherit per-spawn substitutions like a session-bound
	// `--remote-control "fleet-<id>"` flag.
	//
	// Populated by the dispatch coord-spawn path: opts.Command stays
	// the operator's --command default (or override), and ExecCommand
	// carries the same argv with `--remote-control "fleet-<id>"`
	// injected into the shell wrapper. A subsequent handoff that
	// reads oldRec.Command → spawn.Options.Command sees the clean
	// form; the replacement starts WITHOUT auto-attach until/unless
	// the caller opts back in by setting ExecCommand again. This
	// keeps the v0.1 contract (coord auto-spawn = remote-control;
	// handoff = manual /remote-control) explicit at the call site.
	//
	// Empty (the common case) means tmux runs Command verbatim.
	ExecCommand []string

	// LeaderCheck, when non-nil, reports whether a HEALTHY coordinator
	// lease leader currently exists for the given project. It is the
	// signal that disambiguates a clean lease STAND-DOWN from a real
	// supervisor failure when a lease-wrapped coord-run's record is
	// already archived by the time Spawn's post-launch merge runs (codex
	// PR2 iter-6 [P2]): a healthy leader present -> stand-down (return
	// ErrCoordStoodDown); none -> the supervisor failed and left no leader
	// -> surface a real error instead of a false "already running". nil
	// (off-flag / non-coord callers) collapses to the conservative
	// stand-down report. Production wires this to
	// coordlock.CurrentActiveOwnerPID via cmd/fleet (spawn cannot import
	// coordlock — it is build-tagged to linux/darwin while spawn is
	// all-platforms).
	LeaderCheck    func(project string) bool
	ActiveOwnerPID func(project string) (int, bool)

	// StandbyTimeout bounds how long a lease-wrapped coord-run --standby
	// polls behind a healthy leader before self-exiting cleanly. 0 uses
	// DefaultStandbyTimeout. Ignored when failover is off or this is not a
	// coord spawn.
	StandbyTimeout time.Duration

	// DisableLeaseWrap keeps a coord spawn on the legacy bare-engine path even
	// when lease failover is enabled. It is reserved for handoffop's drain
	// cold-resume fallback, where the existing contract requires the successor
	// to start immediately instead of waiting behind the still-live leader.
	DisableLeaseWrap bool
}

func shouldStandDownLeaseWrappedSpawn(project string, supervisorPID int,
	leaderCheck func(string) bool, activeOwnerPID func(string) (int, bool)) bool {
	if leaderCheck == nil || activeOwnerPID == nil || supervisorPID <= 0 {
		return false
	}
	if !leaderCheck(project) {
		return false
	}
	ownerPID, ok := activeOwnerPID(project)
	return ok && ownerPID > 0 && ownerPID != supervisorPID
}

// Spawn creates a fresh agent (or a handoff replacement, if
// opts.OldRecord is set), brings up its tmux session, and writes
// the agent record. Returns the populated record.
//
// The caller is responsible for killing the OLD agent's tmux session
// (graceful /exit + grace + Kill) and archiving the OLD record.
// Spawn handles only the *new* agent.
//
// On any failure after tmux.Spawn succeeds (e.g., record write
// rejected by disk), the orphan tmux session is killed before
// returning so dispatch is exactly-once from the operator's view.
func Spawn(opts Options) (*agent.Record, error) {
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("spawn.Spawn: Command required")
	}

	id := opts.PreAllocatedID
	if id == "" {
		id = agent.NewID()
	}
	session := tmux.SessionName(id)
	rec := agent.New(id)
	rec.TmuxSession = session
	// Provisional pid: the fleet binary's own pid. Overwritten after
	// tmux.Spawn with the resolved real engine pid (resolveEnginePid
	// walks the pane's child process tree). Without this overwrite,
	// every downstream liveness probe (TUI dead-coord sweep, coord
	// reconcile) would classify the agent as DEAD by construction —
	// the fleet binary exits immediately after dispatch returns.
	rec.PID = os.Getpid()

	// Capture the resolved cwd so `fleet handoff` can place the
	// replacement in the same project checkout even when invoked
	// from a different shell. Empty opts.Cwd means "inherit caller"
	// — resolve via os.Getwd(). Relative paths from --cwd get
	// canonicalized via filepath.Abs so the record always stores
	// an absolute path, immune to "next handoff invoked from a
	// different shell" wrong-tree spawns.
	//
	// Resolution failures (deleted cwd, unreadable parent) abort
	// here rather than silently writing a record with empty/relative
	// Cwd — that would later trip the "legacy record with no stored
	// cwd" guard at handoff time AND let tmux launch the agent in
	// the tmux server's cwd, not the operator's checkout.
	cwd := opts.Cwd
	if cwd == "" {
		// COORD belt (DESIGN-coord-repo-binding-from-project.md §6 /
		// PR3): an empty Cwd on a COORD spawn means the upstream
		// resolver (coordrepo.ResolveProjectRepo, run at the attach /
		// recovery / handoff call-site) either was not run or refused —
		// either way the binding is unknown. A coord must NEVER fall
		// back to os.Getwd(): the launch cwd is not a binding tier
		// (that is the whole bug this design fixes). Refuse so the
		// caller surfaces it, rather than silently binding the wrong
		// tree. Workers legitimately inherit the dispatch cwd, so the
		// os.Getwd() fallback is preserved for the non-coord path.
		effTaskID := opts.TaskID
		effProject := opts.Project
		if opts.OldRecord != nil {
			if opts.OldRecord.TaskID != "" {
				effTaskID = opts.OldRecord.TaskID
			}
			if opts.OldRecord.Project != "" {
				effProject = opts.OldRecord.Project
			}
		}
		if isCoordSpawn(effTaskID, effProject) {
			return nil, fmt.Errorf(
				"coord spawn for project %q has no resolved repo (empty Cwd): "+
					"the repo binding must be resolved via coordrepo.ResolveProjectRepo "+
					"at the call-site — launch cwd is not a binding tier", effProject)
		}
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve current working directory: %w", err)
		}
		cwd = wd
	} else if !filepath.IsAbs(cwd) {
		abs, err := filepath.Abs(cwd)
		if err != nil {
			return nil, fmt.Errorf("canonicalize cwd %q: %w", cwd, err)
		}
		cwd = abs
	}
	rec.Cwd = cwd

	// Capture the launch command so `fleet handoff` preserves any
	// custom engine/wrapper the operator dispatched with.
	rec.Command = append([]string(nil), opts.Command...)

	// FLEET_AGENT_ID is propagated into the agent's process env so
	// fleet-guard (4b/c) can identify which agent record to update
	// without round-tripping via tmux session name parsing.
	//
	// FLEET_BIN stamps the path of THIS fleet binary so the agent's
	// fleet-guard skill can spawn `<this-fleet> drain` without a
	// PATH lookup. Mirrors the TUI's fleetBinary trick (see
	// internal/tui/keys.go) — required for side-loaded installs
	// where `which fleet` resolves to nothing or a different binary.
	// `go run` is partial coverage: works while the parent process
	// is alive (the temp build artifact lives with it), drops to
	// the skill's `which("fleet")` fallback after the parent exits.
	// os.Executable() failure is rare and non-fatal.
	//
	// propagatedRuntimeEnv carries the operator's FLEET_* knobs into
	// the agent's session. Necessary because tmux strips non-`-e`
	// vars when the server is already running (see tmux.Spawn comment),
	// and `_kick_drain` runs `fleet drain` from inside the agent pane
	// — that drain needs to see the same FLEET_HOME, custom socket,
	// and prompt-timing knobs the operator's fleet was started with.
	// Without this propagation: custom FLEET_HOME silently splits
	// reads/writes between operator and agent (TUI doesn't see the
	// agent at all), and slow-wrapper prompt-timing overrides regress
	// to defaults so resume-prompt delivery races.
	extraEnv := []string{"FLEET_AGENT_ID=" + id}
	if exe, err := os.Executable(); err == nil {
		extraEnv = append(extraEnv, "FLEET_BIN="+exe)
	}
	// FLEET_PROJECT is stamped from the agent's intrinsic project
	// (record-derived, not shell-env-derived — see propagatedRuntimeEnv
	// rationale below for why this is NOT a propagated var). The
	// /coordinator skill reads it to pick which project's queue to
	// supervise; without it the skill falls back to a cwd-derived
	// project, which silently misroutes any coord whose cwd doesn't
	// match its record. Mirror the rec.Project resolution that happens
	// further down (handoff branch inherits OldRecord.Project; fresh
	// dispatch uses opts.Project). Empty project (legacy / untargeted
	// dispatch) skips emission so the skill's "no project" branch still
	// fires; an explicit FLEET_PROJECT= entry with an empty value would
	// override the unset-check and break that path. Regresses fleet#170.
	effectiveProject := opts.Project
	if opts.OldRecord != nil && opts.OldRecord.Project != "" {
		effectiveProject = opts.OldRecord.Project
	}
	if effectiveProject != "" {
		extraEnv = append(extraEnv, "FLEET_PROJECT="+effectiveProject)
	}
	// Propagate operator-set FLEET_* knobs. FLEET_ENGINE is a special
	// case on the handoff branch: the replacement agent inherits
	// OldRecord.Engine (set below), so its env must match the record
	// rather than the caller's session env. Without this guard a
	// caller running `fleet --engine codex handoff <claude-agent>`
	// would propagate FLEET_ENGINE=codex into a replacement that's
	// actually running claude-code (codex review iter-2 P1), and
	// any code inside that replacement keying off FLEET_ENGINE — the
	// reviewer-prompt builder, `fleet dispatch` subprocesses — would
	// pick the wrong engine.
	//
	// Legacy records (codex review iter-3 P2): pre-v0.9 agent records
	// predate the engine field, so opts.OldRecord.Engine is "" even
	// though agent.New defaults the new record to claude-code. Without
	// normalization the handoff env would inherit the caller's
	// FLEET_ENGINE while the new record silently sat at claude-code,
	// re-introducing the env/record mismatch on the upgrade path.
	// agent.DefaultEngine = "claude-code" matches what agent.New
	// stamps onto a fresh record, so we substitute it here.
	for _, key := range propagatedRuntimeEnv {
		v := os.Getenv(key)
		if key == "FLEET_ENGINE" && opts.OldRecord != nil {
			eng := opts.OldRecord.Engine
			if eng == "" {
				eng = agent.DefaultEngine
			}
			v = eng
		}
		if v != "" {
			extraEnv = append(extraEnv, key+"="+v)
		}
	}

	if opts.OldRecord != nil {
		// Inherit task identity from outgoing agent.
		rec.TaskID = opts.OldRecord.TaskID
		rec.Project = opts.OldRecord.Project
		// Inherit engine + role + mode so the replacement runs in the
		// same configuration. v1.1 engine adapter relies on this for
		// per-agent engine continuity.
		if opts.OldRecord.Engine != "" {
			rec.Engine = opts.OldRecord.Engine
		}
		if opts.OldRecord.Role != "" {
			rec.Role = opts.OldRecord.Role
		}
		if opts.OldRecord.Mode != "" {
			rec.Mode = opts.OldRecord.Mode
		}
		// Auto-resume policy: caller (cmd/fleet/handoff.go or
		// internal/handoffop) is responsible for computing the right
		// value — typically inherit-from-old, but operator may
		// override on `fleet handoff --no-auto-resume` when the
		// replacement is a different command class (codex review
		// iter-8 P2). Spawn just persists what it's given.
		rec.DisableAutoResume = opts.DisableAutoResume
		// Chain: handoff_number = old + 1, prev_path = doc just written.
		rec.HandoffNumber = opts.OldRecord.HandoffNumber + 1
		if opts.NewDocPath != "" {
			rec.LastHandoffPath = &opts.NewDocPath
		}
		// Mark the spawn origin so the TUI can render the transition.
		manualType := handoff.TypeManual
		rec.HandoffType = &manualType
		// Chain pointer (v2 schema): the successor records its
		// predecessor so `fleet attach <successor>` can show the chain
		// trace (and a future tooling pass can render the chain in TUI
		// alongside HandoffNumber). The predecessor's matching
		// successor_id is stamped at archive time by ArchiveWithHandoff
		// in cmd/fleet/handoff.go.
		rec.PredecessorID = opts.OldRecord.ID
	} else {
		rec.TaskID = opts.TaskID
		rec.Project = opts.Project
		rec.DisableAutoResume = opts.DisableAutoResume
		// Engine override (v0.9 multi-engine MVP). Empty leaves the
		// agent.New default ("claude-code") in place so existing call
		// sites keep their byte shape; non-empty stamps the operator's
		// `fleet -codex` / `fleet --engine <name>` choice. The handoff
		// branch above already inherits oldRec.Engine; we mirror that
		// here for the fresh-dispatch path.
		if opts.Engine != "" {
			rec.Engine = opts.Engine
		}
	}

	// Pass the canonicalized cwd (not opts.Cwd) so the tmux session
	// actually starts in the directory we recorded on rec.Cwd.
	// Otherwise a relative --cwd resolved to one path here could
	// resolve to a different one inside tmux (especially with an
	// existing tmux server), and a future handoff would land the
	// replacement in the wrong checkout.
	//
	// ExecCommand (when non-empty) is the per-spawn substituted argv
	// — used by the dispatch coord-spawn path to inject
	// `--remote-control "fleet-<id>"` for THIS spawn only. The
	// persisted record at rec.Command above stays the clean Command,
	// so a later handoff that reads oldRec.Command doesn't inherit
	// the stale session-name flag (codex review #73 iter-1 P1).
	execArgv := opts.Command
	if len(opts.ExecCommand) > 0 {
		execArgv = opts.ExecCommand
	}

	// Lease-failover supervisor wrap (DESIGN-handoff-drain-storm-leak
	// PR1 spawn-collapse: every coord spawn runs under
	// `fleet coord-run --standby --standby-timeout T` when lease failover
	// is enabled. Applied HERE — not at individual call sites — so fresh
	// dispatch and dead-coord recovery share the same heartbeat-owning
	// supervisor path. Wrapping the EXECUTION argv only keeps the persisted
	// rec.Command clean. Handoffop's documented drain cold-resume fallback can
	// opt out with DisableLeaseWrap because it must start a bare successor
	// while the old leader is still live.
	leaseWrapped := false
	if shouldLeaseWrap(leaseFailoverEnabled(), isCoordSpawn(rec.TaskID, rec.Project), opts.DisableLeaseWrap) {
		standbyTimeout := standbyTimeoutOrDefault(opts.StandbyTimeout)
		switch {
		case alreadyCoordRunWrapped(execArgv):
			execArgv = augmentCoordRunWrap(execArgv, standbyTimeout)
			leaseWrapped = true // a supervisor will run; apply lifecycle handling
		default:
			if fleetBin, exeErr := os.Executable(); exeErr == nil && fleetBin != "" {
				execArgv = coordRunWrap(fleetBin, id, rec.Project, standbyTimeout, execArgv)
				leaseWrapped = true
			} else {
				_, _ = fmt.Fprintf(os.Stderr,
					"warning: FLEET_LEASE_FAILOVER set but os.Executable() failed (%v); "+
						"spawning coord %s WITHOUT the coord-run lease supervisor\n", exeErr, id)
			}
		}
	}
	// Persist the ACTUAL wrap state (codex iter-24 [P2]) so crash-recovery retry
	// paths read the truth instead of inferring it from the producer's
	// cap-approval bit. A bare drain cold-resume coord (DisableLeaseWrap) records
	// false even on a CapApproved queue.
	rec.LeaseWrapped = leaseWrapped

	// PRE-LAUNCH record write for lease-wrapped coords (codex PR2 iter-2
	// [P1]). The wrapped `fleet coord-run` supervisor stamps its identity
	// (pid/start/exe) into THIS record at startup so the STONITH primitive
	// can authenticate a takeover. But coord-run starts the moment
	// tmux.Spawn launches it — BEFORE the post-pid-resolve rec.Write below
	// (resolveEnginePid blocks seconds). Without the record on disk first,
	// the supervisor's stamp hits ErrNotFound and the identity is never
	// recorded -> takeover can't authenticate the hung holder. So write
	// the record (provisional engine PID) BEFORE launch; the final write
	// merges back the supervisor fields the coord-run process added.
	if leaseWrapped {
		if err := rec.Write(); err != nil {
			return nil, fmt.Errorf("pre-launch write agent record for lease coord %s: %w", id, err)
		}
	}

	if err := tmux.Spawn(session, cwd, execArgv, extraEnv); err != nil {
		// Roll back the pre-launch record (codex PR2 iter-3 [P2]): the
		// tmux session never came up, so leaving a live record behind
		// would advertise a coord that does not exist (confusing status /
		// attach / recovery). fleet-owns-its-resources: clean up what we
		// created on the failure path. Best-effort — a removal error is
		// secondary to the spawn failure we are already returning.
		if leaseWrapped {
			if livePath, perr := state.AgentPath(id); perr == nil {
				_ = os.Remove(livePath)
			}
		}
		return nil, err
	}
	if leaseWrapped {
		// Authoritative fork-bomb gate (test-only read): a real standby pane is
		// now live. Non-integration TestMains assert this stays zero, catching a
		// standby-spawning test left in the default lane. No-op in production.
		recordStandbyLaunch()
	}
	// Best-effort: pin a "Ctrl-b d to detach" hint into this session's
	// status bar so operators see it persistently while attached.
	// Failure is silent — TUI keybind hints + the wrapped command's
	// in-session banner are fallback discovery paths.
	_ = tmux.SetStatusHint(session, "[Ctrl-b d to detach]")

	// Resolve the real engine pid via the tmux pane child tree (P0
	// bug fix 2026-05-13). For coord-spawn dispatches, the
	// disambiguator is the per-agent --remote-control session name
	// injected by cmd/fleet/dispatch.go (fleet-coord-<id>). For
	// other dispatches we fall back to engineHint matching ("claude"
	// for the default engine; empty for custom-command spawns).
	// resolveEnginePid blocks up to pidResolveTimeout; on timeout it
	// returns the pane pid as a best-effort fallback (wrong-but-live
	// beats os.Getpid which is dead by construction).
	// Lease-wrapped coords always run through `coord-run --standby`. The
	// supervisor may not launch the engine until after Spawn returns (busy
	// lease), and even a free lease stamps the real engine pid itself after
	// child start. Running the tmux engine-pid resolver here would observe
	// the supervisor rather than the child on the busy path. Non-lease
	// spawns keep the legacy resolver.
	if !leaseWrapped {
		disambiguator := pidResolveDisambiguator(id, execArgv)
		engineHint := pidResolveEngineHint(rec.Engine, opts.OldRecord, opts.Command)
		resolvedPid, _, resolveErr := resolveEnginePid(
			session, disambiguator, engineHint,
			pidResolveTimeout(),
			productionResolveDeps(),
		)
		if resolveErr != nil {
			// Pane pid unreachable — log a warning to stderr but DO NOT
			// fail the spawn. The agent record will still go to disk with
			// PID=os.Getpid (the pre-fix shape); operators can re-resolve
			// via fleet-guard heartbeat once the agent boots and the
			// Stop hook fires. Aborting here would orphan a live tmux
			// session for a transient tmux probe blip.
			_, _ = fmt.Fprintf(os.Stderr,
				"warning: resolve engine pid for %s failed: %v (recording fleet binary pid as fallback)\n",
				session, resolveErr)
		} else if resolvedPid > 0 {
			rec.PID = resolvedPid
		}
	}

	// Merge back supervisor identity the coord-run process may have
	// stamped between the pre-launch write and now (codex PR2 iter-2
	// [P1]). Our in-memory rec has empty supervisor fields; clobbering
	// the disk record with it would erase the stamp the supervisor wrote,
	// re-breaking STONITH authentication. Re-load + carry the three
	// supervisor fields forward.
	//
	// DO-NOT-RESURRECT (codex PR2 iter-3 [P2]): a missing on-disk record
	// here means the supervisor already ran its full lifecycle and
	// archived the pre-launch record — the common case is a `acquired=
	// false` STAND-DOWN (a healthy leader already holds the lease), where
	// coord-run returns nil and coord.Cleanup archives the record + kills
	// the (already-gone) session. Re-writing rec now would RESURRECT a
	// live record for a dead coord. So on ErrNotFound: skip the final
	// write entirely and return the in-memory rec (for the caller's
	// stdout) WITHOUT persisting — the supervisor owns the lifecycle.
	// Final record write.
	//
	// LEASE COORDS: the coord-run supervisor concurrently stamps the 3
	// supervisor fields via agent.StampSupervisorIdentity. To keep both
	// writers' fields (engine PID here + supervisor identity there) the
	// merge + write runs UNDER the per-agent lock both sides take (codex
	// PR2 iter-8 [P2]) — a locked load-modify-write that overlays only the
	// fields this side owns. Also handles the do-not-resurrect /
	// stand-down disambiguation (iter-3/6) inside the same critical
	// section.
	if leaseWrapped {
		if opts.LeaderCheck != nil && opts.ActiveOwnerPID != nil {
			waitForSupervisorStamp(id)
		}
		unlock, lkErr := state.LockAgent(id)
		if lkErr != nil {
			_ = tmux.Kill(session)
			// Roll back the pre-launch record too (codex PR2 iter-12 [P2]):
			// the lock failure means we can't finalize, so leaving the live
			// pre-launch record would advertise a coord for the session we
			// just killed. Best-effort, matching the other lease failure
			// paths.
			if livePath, perr := state.AgentPath(id); perr == nil {
				_ = os.Remove(livePath)
			}
			return nil, fmt.Errorf("lock agent %s for final write (orphan tmux session killed): %w", id, lkErr)
		}
		onDisk, lerr := agent.Load(id)
		switch {
		case errors.Is(lerr, state.ErrNotFound):
			// The supervisor already archived the pre-launch record before
			// we reached the merge. AMBIGUOUS (codex PR2 iter-6 [P2]): a
			// clean STAND-DOWN only if a healthy leader now holds the lease;
			// otherwise coord-run hit a real fault and left NO coord running.
			unlock()
			if opts.LeaderCheck != nil && opts.LeaderCheck(rec.Project) {
				_, _ = fmt.Fprintf(os.Stderr,
					"spawn: coord %s stood down — a healthy leader already holds the lease "+
						"for %q; not resurrecting its record\n", id, rec.Project)
				return rec, ErrCoordStoodDown
			}
			return nil, fmt.Errorf(
				"spawn: coord %s supervisor exited before its record was finalized and no "+
					"healthy leader holds the lease for %q (lease-acquire or child-start "+
					"failure); no coord is running", id, rec.Project)
		case lerr == nil:
			// Preserve the supervisor identity the coord-run process wrote.
			rec.SupervisorPID = onDisk.SupervisorPID
			rec.SupervisorPidStart = onDisk.SupervisorPidStart
			rec.SupervisorExePath = onDisk.SupervisorExePath
			if shouldStandDownLeaseWrappedSpawn(rec.Project, rec.SupervisorPID,
				opts.LeaderCheck, opts.ActiveOwnerPID) {
				unlock()
				_ = tmux.Kill(session)
				if livePath, perr := state.AgentPath(id); perr == nil {
					_ = os.Remove(livePath)
				}
				_, _ = fmt.Fprintf(os.Stderr,
					"spawn: coord %s stood down — a healthy leader already holds the lease "+
						"for %q; not leaving an idle standby record\n", id, rec.Project)
				return rec, ErrCoordStoodDown
			}
			// A lease-wrapped standby spawn skipped engine-pid resolution, so
			// our in-memory rec.PID may still be the provisional spawning-CLI
			// pid. If coord-run already acquired the lease and stamped the real
			// engine child pid before this final locked write, carry it forward
			// so the merge does not clobber it back to the provisional pid.
			if onDisk.PID > 0 && onDisk.PID != rec.PID {
				rec.PID = onDisk.PID
			}
		}
		// Any other load error: fall through to the write (best-effort).
		if werr := rec.Write(); werr != nil {
			unlock()
			_ = tmux.Kill(session)
			// The pre-launch record is already on disk; an atomic write
			// failure leaves THAT stale record advertising a killed
			// session — remove it (codex iter-6 [P2]).
			if livePath, perr := state.AgentPath(id); perr == nil {
				_ = os.Remove(livePath)
			}
			return nil, fmt.Errorf("write agent record (orphan tmux session killed): %w", werr)
		}
		unlock()
	} else if err := rec.Write(); err != nil {
		// Non-lease path (unchanged): single writer, no per-agent lock
		// needed. Orphan rollback: tmux session up, record missing →
		// operator would see a ghost session with no fleet status entry.
		_ = tmux.Kill(session)
		return nil, fmt.Errorf("write agent record (orphan tmux session killed): %w", err)
	}

	// fleet#175: on coord-spawn, stamp the resolved repo path into
	// ~/.fleet/projects/<project>/coord-config.json::repo. loop.py
	// reads that field on every tick to decide where to run
	// `git worktree add`, avoiding the cwd-derived worktree-base bug
	// where a coord whose tmux session inherited the wrong shell cwd
	// silently corrupted cross-project dispatches.
	//
	// Write gate (iter-18, codex P2 refinement of iter-17):
	//   - OldRecord nil → fresh dispatch, always stamp.
	//   - OldRecord non-nil + cwd inherited (== OldRecord.Cwd) →
	//     in-flight handoff inheriting from current coord; skip
	//     stamp (don't restamp with the same value the in-flight
	//     coord chose; if that value was wrong, the operator must
	//     fresh-dispatch with --cwd to correct it).
	//   - OldRecord non-nil + cwd differs from OldRecord.Cwd →
	//     operator explicitly supplied a corrected cwd on a recovery
	//     spawn (`fleet dispatch --coord-spawn --cwd <correct>`
	//     against a dead coord). Restamp with the operator's value.
	//
	// Iter-17 unconditionally skipped on OldRecord non-nil, which
	// broke recovery for dead coords with `--cwd`. iter-18's
	// "cwd-differs" heuristic preserves the in-flight-handoff skip
	// (system-driven inheritance) while allowing recovery restamps
	// (operator-driven override).
	//
	// Best-effort: a write failure is logged to stderr but never
	// fails the spawn. The worst case is the coord skill falls back
	// to legacy cwd-derived behavior + emits a fallback warning.
	if isCoordSpawn(rec.TaskID, rec.Project) {
		// ALWAYS re-stamp coord-config.json::repo on EVERY coord spawn
		// AND handoff (DESIGN-coord-repo-binding-from-project.md §6 /
		// PR3). The input `cwd` is now the resolver's output (run at the
		// attach / recovery / handoff call-site), NOT a launch cwd, so
		// re-stamping is cheap and correct. The previous
		// "skip on OldRecord inheritance" heuristic (iter-17..19) could
		// leave coord-config.json holding a STALE wrong-repo value on a
		// handoff replacement; with the resolver upstream, the freshly
		// resolved repo is authoritative, so we stamp unconditionally.
		// coord-config.json is now a write-only breadcrumb — the Python
		// tick never reads it as a resolution authority (PR4).
		fhome := fleetHomeForSpawn()
		if fhome != "" {
			if werr := writeCoordConfigRepoIdempotent(fhome, rec.Project, cwd); werr != nil {
				_, _ = fmt.Fprintf(os.Stderr,
					"warning: write coord-config.json::repo for %s failed: %v "+
						"(coord skill will fall back to cwd-derived worktree base)\n",
					rec.Project, werr)
			}
		}
	}

	// NOTE: the handoff resume prompt is typed by the caller's retire
	// path (handoffop.retireOldAgent / cmd/fleet/handoff.go step 11b)
	// via SendInitialPrompt, NOT here. Keeping it out of Spawn means
	// crash recovery's "replacement spawned, retire interrupted"
	// branch — which goes through retireOldAgent directly without
	// re-spawning — still delivers the prompt. See codex review
	// iter-1 P1 / iter-2 P2.
	return rec, nil
}

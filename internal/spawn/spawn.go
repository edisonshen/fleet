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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/tmux"
)

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
	if err := tmux.Spawn(session, cwd, execArgv, extraEnv); err != nil {
		return nil, err
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

	if err := rec.Write(); err != nil {
		// Orphan rollback: tmux session up, record missing → operator
		// would see a ghost session in `tmux ls` with no `fleet status`
		// entry. Kill the session so spawn is all-or-nothing.
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
		operatorOverrideCwd := opts.OldRecord == nil ||
			(opts.OldRecord.Cwd != "" && cwd != opts.OldRecord.Cwd)
		if operatorOverrideCwd {
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

package main

// fleet maintenance — operator-facing one-shot diagnostics that don't
// belong on any other subcommand. The first member is
// `bootstrap-remote-control`, a survey that flags live agents whose
// persisted Command lacks `--remote-control`. v1 is REPORT-ONLY: an
// actual live retrofit would require restarting the agent (operator's
// call), so the command just lists candidates with a one-line
// remediation suggestion.
//
// Lineage: handoff-remote-control-shell-wrapper-fix (the dispatch +
// handoff replacement paths now inject the flag correctly via the
// relaxed wrapper-pattern matcher in internal/spawn.InjectRemoteControlFlag,
// but agents spawned BEFORE that fix landed are still running without
// the flag — this survey gives operators a checklist of which to
// hand off when convenient).

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/rc"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// fleetAgentIDPattern matches the agent ID format produced by
// agent.NewID(): 8 lowercase hex characters (happy path), OR the
// crypto/rand fallback shape "t%07x" with the value masked to 32 bits.
// %07x is a MINIMUM width (not maximum), so the fallback can produce
// either t+7hex (value <= 0x0fffffff, the smaller half) or t+8hex
// (value > 0x0fffffff, the larger 15/16ths). The regex must accept
// both to avoid missing real fleet sessions on the fallback path
// (codex iter-5 [P3]).
//
// Codex iter-2 [P1]: without this gate, any tmux session starting with
// `fleet-` would be classified as orphan once older than 90s and killed
// in --kill mode. Operators routinely name unrelated sessions with the
// `fleet-` prefix; the sweeper must scope to the agent.NewID() shape
// only.
var fleetAgentIDPattern = regexp.MustCompile(`^([0-9a-f]{8}|t[0-9a-f]{7,8})$`)

// pruneOrphanTmuxMinFreshness is the floor for the freshness window:
// even when FLEET_PID_RESOLVE_S is unset or tiny, the sweeper waits at
// least this long before classifying a record-absent session as orphan.
//
// Rationale: spawn.Spawn creates the tmux session first (tmux.Spawn),
// then resolves the engine pid (up to spawn.PidResolveTimeout()), then
// writes the agent record (rec.Write). Between tmux create and rec.Write
// the on-disk state matches the orphan shape exactly: session exists,
// record absent. A sweeper that runs in that window (especially via
// cron / startup hook) would kill a brand-new healthy replacement.
//
// 90s is well above the typical 1-5s spawn cost when the operator
// hasn't tuned FLEET_PID_RESOLVE_S — it covers slow Macs, slow disks,
// and ZFS-style snapshot pauses. The dynamic ceiling below adds 2x
// the configured pid-resolve timeout when the operator raised it
// (codex review iter-3 [P1]).
const pruneOrphanTmuxMinFreshness = 90 * time.Second

// pruneOrphanTmuxFreshness computes the actual freshness window to use,
// scaling with FLEET_PID_RESOLVE_S so that operators who raise the
// pid-resolve budget for slow-spawning wrapper engines don't get their
// in-flight replacements terminated by --kill.
//
// Returns max(pruneOrphanTmuxMinFreshness, 2 * spawn.PidResolveTimeout()).
// The 2x multiplier covers the gap between when the pid-resolve clock
// starts (just after tmux.Spawn) and when the record actually lands on
// disk (after pid resolution succeeds OR times out — both go through
// rec.Write), plus a margin for slow filesystems.
//
// Codex review iter-2 [P1] established the lower bound; codex review
// iter-3 [P1] established the dynamic ceiling.
func pruneOrphanTmuxFreshness() time.Duration {
	dynamicCeiling := 2 * spawn.PidResolveTimeout()
	if dynamicCeiling > pruneOrphanTmuxMinFreshness {
		return dynamicCeiling
	}
	return pruneOrphanTmuxMinFreshness
}

func newMaintenanceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "maintenance",
		Short: "One-shot diagnostics and repair helpers",
		Long: `maintenance groups operator-facing diagnostics that don't fit
on any other subcommand.

Subcommands:
  bootstrap-remote-control  — list live agents missing --remote-control
                              in their persisted Command (report only).
  prune-orphan-tmux         — find tmux sessions named fleet-<id> that
                              have no matching live agent record. Dry-run
                              by default; pass --kill to actually kill.`,
	}
	cmd.AddCommand(newMaintenanceBootstrapRemoteControlCmd())
	cmd.AddCommand(newMaintenancePruneOrphanTmuxCmd())
	return cmd
}

// newMaintenancePruneOrphanTmuxCmd wires `fleet maintenance
// prune-orphan-tmux`. Defensive sweeper for the orphan tmux session
// leak that crashed the operator's Mac twice (PR
// fix/orphan-tmux-sweeper-and-leak-plug). The same PR plugs the root
// cause in handoff.go / handoffop.go — this command is the safety net
// for orphans left over from buggy builds AND a periodic operator
// hygiene tool going forward.
func newMaintenancePruneOrphanTmuxCmd() *cobra.Command {
	var kill bool
	cmd := &cobra.Command{
		Use:   "prune-orphan-tmux",
		Short: "Find tmux sessions named fleet-<id> with no live agent record",
		Long: `prune-orphan-tmux enumerates tmux sessions whose name starts with
"fleet-" via ` + "`tmux ls -F '#{session_name}'`" + `, derives the agent ID
from the suffix, and checks whether a live agent record exists at
~/.fleet/agents/<id>.json. Sessions whose ID has NO live record are
orphans — most likely the residue of a previous build's leak.

Default behavior is DRY-RUN: prints the orphan list and exits 0 WITHOUT
killing. Pass --kill to actually run ` + "`tmux kill-session`" + ` on
each orphan.

Output format (one line per session):
  <session>  orphan=<true|false>  state=<missing|live|fresh>  killed=<true|false>

Idempotent. Safe to run while live fleet agents are working — only
sessions whose state.json is absent AND are older than the freshness
threshold are candidates, so live agents (and brand-new spawns in the
tmux-up-but-record-write-pending window) are never touched. Sessions
whose name doesn't match the fleet-<id> pattern are also left alone.

Exit code is always 0 (even if some kills failed; warnings go to
stderr) so the operator can chain this into cron / a startup script
without worrying about ordering edge cases.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMaintenancePruneOrphanTmux(
				cmd.OutOrStdout(),
				cmd.ErrOrStderr(),
				kill,
				agentDirSane,
				tmux.ListSessionsWithCreated,
				agentRecordExists,
				tmux.Kill,
				time.Now,
				pruneOrphanTmuxFreshness(),
			)
		},
	}
	cmd.Flags().BoolVar(&kill, "kill", false,
		"actually kill the orphan tmux sessions (default: dry-run, list only)")
	return cmd
}

// agentDirSane verifies that the live-agent state root (~/.fleet/agents)
// exists AND is a directory before the orphan-tmux sweeper iterates
// sessions.
//
// Codex iter-1 [P2]: without this precondition, a mispointed FLEET_HOME
// or a wiped ~/.fleet/agents (e.g., dotfile rebuild, `rm -rf ~/.fleet`
// during operator triage, transient mount issue) makes every os.Stat in
// agentRecordExists return ErrNotExist → every live fleet-* tmux session
// is misclassified as orphan. With `--kill`, the sweeper then mass-
// terminates healthy agents. agentDirSane fails closed: if the dir is
// missing or not a directory, return an error and the sweeper aborts
// before iterating.
func agentDirSane() error {
	dir, err := state.AgentDir()
	if err != nil {
		return fmt.Errorf("resolve agent dir: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat agent dir %s: %w (live-agent state root unavailable — refusing to sweep, every session would misclassify as orphan)",
			dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("agent dir %s exists but is not a directory (live-agent state root corrupted — refusing to sweep)",
			dir)
	}
	return nil
}

// agentRecordExists reports whether ~/.fleet/agents/<id>.json exists
// AS A LIVE record (archive copies don't count — the operator's brief
// is explicit that "live state.json" is the gate).
//
// Returns false on both "file does not exist" AND "Stat error" (e.g.,
// FLEET_HOME unresolvable). The fallback is conservative for safety:
// if we can't be sure the record is live, we still treat the session
// as "has record" so we don't accidentally kill a session for which we
// just lost read access to its record. The orphan-detection bar is
// "missing record", not "ambiguous record" — operator can retry after
// fixing perms.
func agentRecordExists(id string) bool {
	path, err := state.AgentPath(id)
	if err != nil {
		// Path resolution failure → can't even probe. Conservative:
		// treat as "live" so we don't kill on ambiguity.
		return true
	}
	_, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		return true
	case errors.Is(statErr, os.ErrNotExist):
		return false
	default:
		// Other stat error (permission denied, I/O failure). Be
		// conservative — don't classify as orphan.
		return true
	}
}

// runMaintenancePruneOrphanTmux is the testable core. dirSaneFn verifies
// the live-agent state root before iterating; listInfoFn yields the tmux
// session names + creation times on the active server; existsFn reports
// whether an agent record is live; killFn kills a session (only called
// when performKill is true AND the session is an orphan AND the session
// is older than freshness).
//
// dirSaneFn precondition (codex iter-1 [P2]): if ~/.fleet/agents is
// missing or not a directory, every existsFn call would return false,
// every session would misclassify as orphan, and --kill would mass-
// terminate live agents. Fail closed: return the dirSaneFn error before
// iterating sessions. Tests pass dirSaneFn = `func() error { return nil }`
// for the "agents dir present" path and `func() error { return errors.New(...) }`
// for the "dir missing" abort path.
//
// Freshness gate: a tmux session newer than `freshness` is reported as
// state=fresh and is NEVER killed even when --kill is set. This guards
// the spawn.Spawn race where tmux.Spawn precedes rec.Write by up to a
// few seconds (codex review iter-2 [P1]).
//
// Output contract (pinned by tests):
//   - One line per session whose name starts with "fleet-".
//   - Lines start with the session name, followed by three key=value
//     pairs separated by two spaces: orphan=, state=, killed=.
//   - state= is "live" (record present), "missing" (record absent and
//     session older than freshness — orphan), or "fresh" (record absent
//     but session younger than freshness — likely a spawn in flight).
//   - orphan=true only for state=missing rows.
//   - Sessions whose name doesn't start with "fleet-" are not listed
//     (out of scope for this command).
//   - Stable ordering: alphabetical by session name.
func runMaintenancePruneOrphanTmux(
	stdout, stderr io.Writer,
	performKill bool,
	dirSaneFn func() error,
	listInfoFn func() ([]tmux.SessionInfo, error),
	existsFn func(id string) bool,
	killFn func(session string) error,
	nowFn func() time.Time,
	freshness time.Duration,
) error {
	if err := dirSaneFn(); err != nil {
		return fmt.Errorf("agent dir sanity check: %w", err)
	}
	infos, err := listInfoFn()
	if err != nil {
		return fmt.Errorf("list tmux sessions: %w", err)
	}
	type row struct {
		session string
		id      string
		orphan  bool
		fresh   bool
		killed  bool
	}
	var rows []row
	now := nowFn()
	for _, info := range infos {
		s := info.Name
		if !strings.HasPrefix(s, "fleet-") {
			// Not a fleet-managed session; out of scope.
			continue
		}
		id := strings.TrimPrefix(s, "fleet-")
		if id == "" {
			// Session name was literally "fleet-" — odd but treat as
			// non-fleet so we don't accidentally kill something we
			// can't identify.
			continue
		}
		if !fleetAgentIDPattern.MatchString(id) {
			// Suffix doesn't match agent.NewID()'s shape (8 lowercase
			// hex chars, or "t" + 7 hex chars in the crypto/rand
			// fallback shape). The operator likely named an unrelated
			// session with the `fleet-` prefix (e.g., `fleet-debug`,
			// `fleet-prod-shell`). Skipping out of caution — the
			// sweeper's contract is "agent sessions only", and tmux
			// session names from spawn always come from
			// tmux.SessionName(agent.NewID()) (codex iter-2 [P1]).
			continue
		}
		live := existsFn(id)
		// Freshness gate: record-absent sessions younger than the
		// threshold are NOT orphans yet — they're likely spawn.Spawn
		// in the tmux-up / record-not-yet-written window. A zero
		// Created (parse failure) is treated as fresh-unknown so we
		// don't kill on ambiguous metadata.
		isFresh := !live && (info.Created.IsZero() || now.Sub(info.Created) < freshness)
		r := row{
			session: s,
			id:      id,
			orphan:  !live && !isFresh,
			fresh:   isFresh,
		}
		if performKill && r.orphan {
			if kerr := killFn(s); kerr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: kill %s: %v (continuing)\n", s, kerr)
			} else {
				r.killed = true
			}
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].session < rows[j].session
	})
	for _, r := range rows {
		stateLabel := "live"
		switch {
		case r.fresh:
			stateLabel = "fresh"
		case r.orphan:
			stateLabel = "missing"
		}
		_, _ = fmt.Fprintf(stdout,
			"%s  orphan=%t  state=%s  killed=%t\n",
			r.session, r.orphan, stateLabel, r.killed)
	}
	return nil
}

func newMaintenanceBootstrapRemoteControlCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap-remote-control",
		Short: "Survey live COORD agents missing the --remote-control flag",
		Long: `bootstrap-remote-control walks ~/.fleet/agents/*.json and
identifies live COORD agents (task_id == coord-<project>, via
spawn.IsCoordSpawn) whose persisted Command lacks
--remote-control. Workers / Agent-tool subagents are out of scope —
they never carry the flag by design (push-storm protection).
Live = the record's tmux session is currently alive (HasSession). For
each match, the command prints a one-line remediation suggestion
("fleet handoff <id>" is the typical fix; the replacement spawn bakes
in the native flag automatically).

This is REPORT-ONLY in v1. An actual live retrofit would require
restarting the agent's tmux session, which is the operator's call —
some agents may be mid-task and worth letting run to natural completion.

Exit codes:
  0  — survey ran (regardless of how many agents need attention)
  1  — read error (agents/ dir, etc.)
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMaintenanceBootstrapRemoteControl(
				cmd.OutOrStdout(),
				agent.List,
				tmux.HasSession,
			)
		},
	}
	return cmd
}

// runMaintenanceBootstrapRemoteControl is the testable core. The
// listFn / hasSessionFn injection lets tests substitute fake
// agent-store and tmux-presence views (no real tmux required).
//
// Native model (codex review iter-1 [P2]): RC rides ONLY on coord
// spawns, and rc.Enabled is default-on — so the survey must scope to
// COORD agents (task_id == coord-<project>, via spawn.IsCoordSpawn).
// Workers / Agent-tool subagents are intentionally never
// flagged: reporting them would generate false "needs remediation"
// noise for every normal default-on project. Within coords, the
// survey differentiates "RC disabled for project" (operator opt-out
// via `fleet rc down` — no action) from "RC enabled (the default) but
// persisted argv lacks the flag" (pre-native coord — handoff respawns
// it with native RC).
//
// Native coords persist the CLEAN command FOREVER (the flag rides on
// the per-spawn exec argv only), so the persisted-Command check alone
// would flag every healthy native coord as a permanent false positive
// (/review adversarial F8). The survey therefore ALSO probes the LIVE
// process argv (`ps -p <pid> -o args=`, via the liveArgvHasRCFn seam):
// a coord whose running process carries --remote-control is healthy
// and skipped. PID<=0 / probe failure falls back to the persisted
// check (report-only; better a rare false positive than a kill).
func runMaintenanceBootstrapRemoteControl(
	stdout io.Writer,
	listFn func() ([]*agent.Record, error),
	hasSessionFn func(string) bool,
) error {
	records, err := listFn()
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	type missing struct {
		id        string
		project   string
		taskID    string
		spawnedAt string
		session   string
	}
	var rcEnabledMissing []missing
	var rcDisabledSkipped []missing
	for _, r := range records {
		if r == nil {
			continue
		}
		// Coord-only scope (codex review iter-1 [P2]): workers and
		// Agent-tool subagents never get --remote-control by design;
		// surveying them under the default-on gate would flag every
		// worker on every project. Same predicate as the handoff /
		// drain inject gates: spawn.IsCoordSpawn (the coord-spawn marker
		// is gone, D3).
		if !spawn.IsCoordSpawn(r.TaskID, r.Project) {
			continue
		}
		if commandHasRemoteControl(r.Command) {
			continue
		}
		// Live-argv probe: native coords never persist the flag, but
		// the running claude process carries it. Healthy native coord
		// → skip.
		if r.PID > 0 && liveArgvHasRCFn(r.PID) {
			continue
		}
		if !hasSessionFn(r.TmuxSession) {
			// Dead session — handoff is moot, the record will be
			// reaped by the normal reconciliation path. Don't pollute
			// the report.
			continue
		}
		m := missing{
			id:        r.ID,
			project:   r.Project,
			taskID:    r.TaskID,
			spawnedAt: r.SpawnedAt.UTC().Format("2006-01-02T15:04:05Z"),
			session:   r.TmuxSession,
		}
		if rc.Enabled(r.Project) {
			rcEnabledMissing = append(rcEnabledMissing, m)
		} else {
			rcDisabledSkipped = append(rcDisabledSkipped, m)
		}
	}

	if len(rcEnabledMissing) == 0 && len(rcDisabledSkipped) == 0 {
		_, _ = fmt.Fprintln(stdout, "no live agents are missing --remote-control")
		return nil
	}

	sortFn := func(s []missing) {
		sort.SliceStable(s, func(i, j int) bool {
			if s[i].spawnedAt != s[j].spawnedAt {
				return s[i].spawnedAt < s[j].spawnedAt
			}
			return s[i].id < s[j].id
		})
	}
	sortFn(rcEnabledMissing)
	sortFn(rcDisabledSkipped)

	if len(rcEnabledMissing) > 0 {
		_, _ = fmt.Fprintf(stdout,
			"%d live agent(s) with RC enabled but missing --remote-control:\n\n",
			len(rcEnabledMissing))
		for _, m := range rcEnabledMissing {
			_, _ = fmt.Fprintf(stdout,
				"  %s  project=%s  task=%s  spawned=%s\n"+
					"     remediation: fleet handoff %s   (replacement spawn bakes in native --remote-control)\n"+
					"                  or: type /remote-control in the agent's pane\n\n",
				m.id, m.project, m.taskID, m.spawnedAt, m.id)
		}
	}
	if len(rcDisabledSkipped) > 0 {
		_, _ = fmt.Fprintf(stdout,
			"%d live agent(s) without --remote-control (RC disabled for project — no action needed):\n\n",
			len(rcDisabledSkipped))
		for _, m := range rcDisabledSkipped {
			_, _ = fmt.Fprintf(stdout,
				"  %s  project=%s  task=%s  spawned=%s\n"+
					"     status: RC opted out (`~/.fleet/projects/%s/rc-disabled` marker present, or no project)\n"+
					"     remediation (only if pairing wanted): fleet rc up %s && fleet handoff %s\n\n",
				m.id, m.project, m.taskID, m.spawnedAt,
				m.project, m.project, m.id)
		}
	}
	return nil
}

// liveArgvHasRCFn probes whether the LIVE process at pid carries the
// --remote-control flag in its argv. Production shells out to
// `ps -p <pid> -o args=`; tests stub via the package var. Best-effort:
// any probe failure returns false (fall back to the persisted-command
// heuristic — the survey is report-only).
var liveArgvHasRCFn = func(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "--remote-control")
}

// commandHasRemoteControl returns true iff any element of the
// persisted Command argv contains the literal `--remote-control`
// substring. Conservative match: if the operator's custom command
// happens to embed the flag literally (e.g. inside a comment) we'd
// report it as already-flagged, but that's fine — the survey errs
// on the side of NOT pestering the operator about a false positive.
func commandHasRemoteControl(command []string) bool {
	for _, el := range command {
		if strings.Contains(el, "--remote-control") {
			return true
		}
	}
	return false
}

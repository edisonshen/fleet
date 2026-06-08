// pidresolver — find the real claude (or wrapped engine) pid for a
// freshly-spawned tmux session.
//
// Problem (P0 bug 2026-05-13): spawn used to record os.Getpid() as the
// agent's pid — the fleet binary's own process. That pid dies the moment
// `fleet dispatch` exits, so every liveness probe downstream
// (TUI dead-coord sweep, coord reconcile, supervisor stuck-checks)
// classified every coord as DEAD by construction. Real example:
//
//	Coord    | Recorded pid | ps -p   | Real claude pid in pane | etime
//	0eb3012e | 5868         | dead    | 5876                    | 16m
//	28265972 | 7177         | dead    | 7182                    | 13m
//	bab86984 | 8299         | dead    | 8306                    | 12m
//
// Solution: after tmux.Spawn returns, resolve the real claude pid by
// asking tmux for the pane's pid and walking its child process tree
// until we find a process whose argv looks like the claude engine. For
// coord-spawn dispatches, prefer a child whose argv also contains the
// agent's disambiguator string (e.g. "fleet-coord-<id>") so we don't
// latch onto a sibling claude running for another coord on the same
// host.
//
// Process-tree shape (canonical coord-spawn dispatch):
//
//	tmux server
//	└── sh -c "claude --remote-control fleet-coord-<id> ..."   ← pane pid
//	    └── claude --remote-control fleet-coord-<id> ...        ← real pid
//
// For non-coord dispatches (workers, plain `fleet dispatch`), the pane
// child IS the claude process (no shell wrapper), so the resolver finds
// it on the first walk step.
//
// Polling: claude can take 100-500ms to exec after the wrapper shell
// starts, so we poll for up to 10s (overridable via FLEET_PID_RESOLVE_S).
// On timeout we return the pane pid as a best-effort fallback rather
// than erroring — a "wrong but at least live" pid still beats os.Getpid()
// which is dead by construction. The fleet-guard heartbeat re-resolution
// path (skills/fleet-guard/health.py) catches drift on subsequent fires.
package spawn

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

const (
	// defaultPidResolveTimeout is how long we poll for the claude pid
	// before giving up. claude typically exec's within 100-500ms; 10s
	// covers slow laptops + first-run binary load. Env-overridable via
	// FLEET_PID_RESOLVE_S for tests that want fast resolution AND for
	// operators on slow-spawn engines.
	defaultPidResolveTimeout = 10 * time.Second

	// defaultPidResolvePollInterval is the per-iteration sleep while
	// polling. 50ms gives ~200 attempts per default timeout — enough
	// granularity to catch a fast-spawning claude on the first or
	// second cycle without burning CPU during the steady-state idle.
	defaultPidResolvePollInterval = 50 * time.Millisecond
)

// procEntry is one row of `ps -o pid,ppid,args` output: a process and
// the argv that identifies what it's running. Used by the tree-walk
// to find a descendant matching the engine name + optional disambiguator.
type procEntry struct {
	PID  int
	PPID int
	Args string // full argv as a single string (ps's `args` column)
}

// panePIDFn returns the pid of the active pane in a tmux session. The
// production binding wraps `tmux list-panes -t <session> -F '#{pane_pid}'`;
// tests inject closures over fixed values.
type panePIDFn func(session string) (int, error)

// listProcsFn enumerates every process on the host as procEntry rows.
// Production wraps `ps -o pid,ppid,args -ax`; tests build the slice
// inline from synthetic process-tree fixtures. The cost on the host is
// one ps invocation per resolver poll iteration — ~5-10ms on a typical
// laptop, negligible during the bounded 10s spawn window.
type listProcsFn func() ([]procEntry, error)

// resolveEnginePidDeps bundles the resolver's injected I/O so the pure
// logic can be tested without touching tmux or the kernel. Production
// callers pass productionResolveDeps(); tests pass closures that drive
// the algorithm against fixture inputs.
type resolveEnginePidDeps struct {
	panePID   panePIDFn
	listProcs listProcsFn
	now       func() time.Time
	sleep     func(time.Duration)
}

// resolveEnginePid finds the real claude pid for a freshly-spawned tmux
// session by polling tmux+ps until a descendant of the pane pid matches
// the engine binary name AND (when non-empty) the disambiguator.
//
// disambiguator is the unique argv substring that identifies THIS
// agent's claude among potentially many sibling claudes on the host
// (typically "fleet-coord-<agent_id>" for coord spawns, empty for plain
// worker dispatches).
//
// engineHint is the engine binary's command name (e.g. "claude",
// "codex"). The resolver matches a descendant whose argv contains
// engineHint as a token. Empty hint matches the first non-shell
// descendant — used for custom wrappers where the binary name is
// unknown to fleet.
//
// Returns the resolved pid + the matching argv so the caller can also
// update agent.Record.Command with the real running argv (the stored
// command otherwise stays a stale snapshot of the spawn intent).
//
// Failure mode: if the pane pid is unreachable (tmux died before we
// could probe), returns 0 + error. If polling times out without
// finding a descendant match, returns the PANE pid + nil error +
// empty argv as a best-effort: a wrong-but-live pid still beats
// os.Getpid() which is dead by construction. The fleet-guard
// heartbeat re-resolution catches subsequent drift.
func resolveEnginePid(
	session, disambiguator, engineHint string,
	timeout time.Duration,
	deps resolveEnginePidDeps,
) (int, string, error) {
	if timeout <= 0 {
		timeout = defaultPidResolveTimeout
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.sleep == nil {
		deps.sleep = time.Sleep
	}
	deadline := deps.now().Add(timeout)
	var panePid int
	for {
		var err error
		panePid, err = deps.panePID(session)
		if err == nil && panePid > 0 {
			break
		}
		if !deps.now().Before(deadline) {
			return 0, "", fmt.Errorf("resolve engine pid for %s: pane pid unreachable: %w",
				session, err)
		}
		deps.sleep(defaultPidResolvePollInterval)
	}
	// Walk descendants looking for an engine match. Repeat with a short
	// sleep so claude's brief exec window after the wrapper shell starts
	// has time to land in `ps` output.
	//
	// Stability ladder (codex iter-5):
	//  - stable match (disambiguator on non-shell, OR engine-name hit):
	//    return immediately.
	//  - tentative match (deepest non-shell, custom-shell case): keep
	//    polling; return only when the same pid persists to the LAST
	//    poll before deadline (transient helpers will have exited).
	//  - raw-shell-leaf pane (no children, pane argv is a shell): fast-
	//    return the pane pid — for `fleet dispatch --command sh`, the
	//    pane IS the correct recorded pid and waiting for the timeout
	//    is pure stall.
	var tentativePid int
	var tentativeArgs string
	// rawShellLeafObservations tracks consecutive polls where the
	// pane has been a shell with no children. We require N
	// consecutive observations (~rawShellLeafConfirmPolls × 50ms =
	// ~250ms) before fast-returning the pane pid, so a normal
	// `sh -c "claude ..."` spawn sampled BEFORE claude exec'd
	// doesn't trip the fast-path and record the wrapper shell.
	// Codex iter-7 regression fix (2026-05-14).
	rawShellLeafObservations := 0
	for {
		procs, err := deps.listProcs()
		if err == nil {
			pid, args, stable := findEngineDescendant(procs, panePid, disambiguator, engineHint)
			if pid > 0 {
				if stable {
					return pid, args, nil
				}
				// Tentative — remember it; only return if the same pid
				// persists in subsequent polls (transient helpers will
				// have died by the deadline).
				tentativePid = pid
				tentativeArgs = args
			}
			// Fast-path: pane has been a raw shell with no children
			// for N consecutive polls. Initial poll alone is NOT
			// enough — `sh -c "claude ..."` can momentarily show
			// the shell without its claude child while the fork is
			// in flight. After N stable observations the wrapper
			// has clearly NOT exec'd a real child, so the pane IS
			// the correct recorded pid (operator dispatched a
			// bare shell with no command).
			if leafPid, ok := rawShellLeafPid(procs, panePid); ok {
				rawShellLeafObservations++
				if rawShellLeafObservations >= rawShellLeafConfirmPolls {
					return leafPid, "", nil
				}
			} else {
				rawShellLeafObservations = 0
			}
		}
		if !deps.now().Before(deadline) {
			// Deadline reached. Prefer a tentative match (best-effort
			// non-shell descendant we observed during the window) over
			// the pane pid: even a tentative match is more informative
			// than the wrapper shell.
			if tentativePid > 0 {
				return tentativePid, tentativeArgs, nil
			}
			// No tentative observation either. Return pane pid as the
			// wrong-but-live fallback (callers signal "couldn't
			// disambiguate" via empty argv).
			return panePid, "", nil
		}
		deps.sleep(defaultPidResolvePollInterval)
	}
}

// rawShellLeafConfirmPolls is how many CONSECUTIVE polls the pane
// must be observed as a raw shell with no children before the
// resolver fast-returns the pane pid. ~250ms total (5 polls × 50ms)
// is well above the typical shell-fork-to-exec window (~10-100ms)
// for normal `sh -c "..."` spawns, and far below the 10s default
// timeout. Smaller values risk recording the wrapper shell on a
// slow fork (codex iter-7); larger values delay raw-shell-only
// dispatch unnecessarily.
const rawShellLeafConfirmPolls = 5

// findEngineDescendant walks the process tree rooted at panePID and
// returns (pid, argv) of the first descendant whose argv satisfies the
// disambiguator + engineHint match. Returns (0, "") when no descendant
// matches.
//
// Match priority (best-first):
//  1. Disambiguator non-empty AND argv contains it → exact agent match.
//     This is the strongest signal — fleet-coord-<id> is unique per
//     coord spawn and won't collide with sibling claudes.
//  2. engineHint non-empty AND argv command token == engineHint →
//     engine-family match. Used on plain worker dispatches where no
//     disambiguator exists but we still want a "claude" over a "bash".
//  3. Fallback: deepest non-shell descendant. Custom wrappers where we
//     can't identify the engine by name still get a sensible pid.
//
// "Deepest" matters because the shell wrapper itself (`sh -c "..."`) is
// the pane pid; its child is the engine. Without depth-bias we'd
// occasionally latch onto the shell instead of the engine on slow
// laptops where ps catches the wrapper before claude exec's.
//
// The walk is BFS via a pid-to-descendants map so we don't recurse on
// pathological trees. Cycle detection is implicit — a process can have
// at most one ppid, so the tree is a tree.
// findEngineDescendant returns the resolved pid + argv + a
// "stable" flag. stable=true means the match is a strong signal
// (disambiguator on a non-shell, or engine-name match) and the
// resolver should return immediately. stable=false means we have a
// best-effort fallback (deepest non-shell descendant) which can be
// a transient helper for custom-shell commands like
// `sh -c 'claude --version; sleep 60'`. The resolver should keep
// polling for stable matches and only accept a tentative match on
// the LAST poll before the deadline. Codex iter-5 hardening
// (2026-05-14).
func findEngineDescendant(
	procs []procEntry, panePID int,
	disambiguator, engineHint string,
) (int, string, bool) {
	if panePID <= 0 || len(procs) == 0 {
		return 0, "", false
	}
	// Build parent → children index for O(N) walk, plus pid → entry
	// index so we can seed the queue with the pane process itself.
	children := make(map[int][]procEntry, len(procs))
	byPID := make(map[int]procEntry, len(procs))
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p)
		byPID[p.PID] = p
	}
	// BFS with depth tracking so we can prefer deeper matches in the
	// fallback path (a deeper claude beats a shell wrapper at the
	// pane root).
	type frame struct {
		entry procEntry
		depth int
	}
	queue := make([]frame, 0, 8)
	// Seed with the pane process itself at depth 0. For direct-command
	// spawns (no shell wrapper — `fleet dispatch --command claude` or
	// any non-shell argv tmux runs directly), the pane pid IS the
	// engine pid and never appears in children[panePID]. Without this
	// the resolver burns the full 10s timeout before falling back to
	// the same pane pid (codex review iter-1 finding, 2026-05-14).
	// Depth 0 keeps wrapper-shell spawns picking their deeper claude
	// child via the depth-bias path; the pane process is still in the
	// running for engine-match / disambiguator-match when it qualifies.
	if entry, ok := byPID[panePID]; ok {
		queue = append(queue, frame{entry: entry, depth: 0})
	}
	for _, c := range children[panePID] {
		queue = append(queue, frame{entry: c, depth: 1})
	}
	// Sentinel -1 so depth=0 matches still update best* trackers. A
	// direct-command pane is at depth 0 and must be eligible to win.
	var bestEngine procEntry
	bestEngineDepth := -1
	var bestFallback procEntry
	bestFallbackDepth := -1
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		p := f.entry
		// A WRAPPER process is transport, not the engine: a shell
		// (`sh -c "claude ..."`) OR — under FLEET_LEASE_FAILOVER — the
		// `fleet coord-run` supervisor (DESIGN-handoff-drain-storm-leak
		// PR2), whose argv CONTAINS the engine argv (and therefore the
		// disambiguator + engine-hint) as its tail. Matching the
		// supervisor would record SupervisorPID as Record.PID, violating
		// the PID==engine / SupervisorPID==lease-holder split (codex PR2
		// iter-4 [P2]). Exclude both from every match priority; the real
		// engine is the deeper descendant.
		isWrapper := isShellArgv(p.Args) || isCoordRunArgv(p.Args)
		// Priority 1: exact disambiguator hit — return immediately
		// UNLESS the match is on a wrapper. A wrapper argv that carries
		// the disambiguator is just transport — we want the real engine
		// underneath. If the matched entry is a wrapper, defer to its
		// deeper children; the production poll loop will re-walk once
		// claude exec's.
		if disambiguator != "" && strings.Contains(p.Args, disambiguator) {
			if !isWrapper {
				return p.PID, p.Args, true // strong match
			}
			// Wrapper carrying the disambiguator — keep walking to the
			// real engine underneath (the wrapper-filter below excludes
			// it from bestFallback).
		}
		// Priority 2: engine command match. Keep the deepest one in
		// case multiple processes share the engine name in the tree.
		// Skip wrappers: the coord-run supervisor's argv contains the
		// engine name in its tail but is NOT the engine.
		if engineHint != "" && !isWrapper && argvCommandIs(p.Args, engineHint) {
			if f.depth > bestEngineDepth {
				bestEngine = p
				bestEngineDepth = f.depth
			}
		}
		// Priority 3: fallback to deepest non-wrapper descendant. We
		// pick this only when the engine-match path also failed.
		if !isWrapper {
			if f.depth > bestFallbackDepth {
				bestFallback = p
				bestFallbackDepth = f.depth
			}
		}
		for _, g := range children[p.PID] {
			queue = append(queue, frame{entry: g, depth: f.depth + 1})
		}
	}
	if bestEngineDepth >= 0 {
		// Engine-name match is a strong signal — the operator named
		// this engine, and a process matching the engine binary's
		// command is unlikely to be transient.
		return bestEngine.PID, bestEngine.Args, true
	}
	if bestFallbackDepth >= 0 {
		// Tentative match: deepest non-shell descendant. For custom
		// shell commands (`sh -c 'claude --version; sleep 60'`) this
		// can be a short-lived helper. Caller polls for stability.
		return bestFallback.PID, bestFallback.Args, false
	}
	return 0, "", false
}

// paneIsRawShellLeaf reports whether the pane process is itself a
// shell argv AND has no children in procs. Used by resolveEnginePid
// to fast-return on raw-shell sessions (`fleet dispatch --command
// sh`) where the correct pid IS the pane and waiting for the
// timeout would gratuitously stall the spawn. Codex iter-5
// hardening (2026-05-14).
func paneIsRawShellLeaf(procs []procEntry, panePID int) bool {
	_, ok := rawShellLeafPid(procs, panePID)
	return ok
}

// rawShellLeafPid returns the pid to record as the engine when the coord's
// command is a RAW SHELL/REPL with no real engine child (`--command sh`),
// and ok=false otherwise. It handles two shapes:
//
//   - UNWRAPPED pane: the pane process itself is a shell with no children
//     → record the pane pid (the original raw-shell-leaf case).
//   - COORD-RUN WRAPPED pane (FLEET_LEASE_FAILOVER): the pane is `fleet
//     coord-run`, whose intended engine is a shell child. The supervisor
//     argv is a WRAPPER (excluded from the normal match), so without this
//     the resolver would fall back to the pane pid = the SUPERVISOR pid,
//     giving PID == SupervisorPID and breaking liveness (codex PR2 iter-14
//     [P2]). Here we descend through coord-run to its shell leaf (a shell
//     with no further children) and return THAT pid — the real engine.
//
// "Leaf" = a shell whose only descendants (if any) are not present, i.e.
// it has not exec'd a deeper engine. Returns the SHELL itself, not the
// supervisor.
func rawShellLeafPid(procs []procEntry, panePID int) (int, bool) {
	if panePID <= 0 || len(procs) == 0 {
		return 0, false
	}
	byPID := make(map[int]procEntry, len(procs))
	children := make(map[int][]procEntry, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
		children[p.PPID] = append(children[p.PPID], p)
	}
	pane, ok := byPID[panePID]
	if !ok {
		return 0, false
	}
	hasChild := func(pid int) bool { return len(children[pid]) > 0 }
	// Unwrapped: pane is a shell leaf.
	if isShellArgv(pane.Args) {
		if !hasChild(panePID) {
			return panePID, true
		}
		return 0, false // a shell WITH children -> normal sh -c wrapper, not a leaf
	}
	// Wrapped: pane is the coord-run supervisor. Its intended engine is a
	// direct shell child; treat it as the leaf only if THAT shell has no
	// further children (no deeper engine exec'd).
	if isCoordRunArgv(pane.Args) {
		for _, c := range children[panePID] {
			if isShellArgv(c.Args) && !hasChild(c.PID) {
				return c.PID, true
			}
		}
	}
	return 0, false
}

// argvCommandIs reports whether the first token of argv (the command
// name) equals name. Strips a leading path so "/usr/local/bin/claude"
// matches "claude". The argv string is the `ps args` column — argv[0]
// + " " + argv[1:] joined.
func argvCommandIs(argv, name string) bool {
	first := firstToken(argv)
	if first == "" {
		return false
	}
	// Strip leading path: "/foo/bar/claude" → "claude"; "claude" → "claude".
	if idx := strings.LastIndexByte(first, '/'); idx >= 0 {
		first = first[idx+1:]
	}
	return first == name
}

// isShellArgv reports whether argv looks like a POSIX shell wrapper
// rather than a real engine binary. Used by the fallback path to avoid
// returning the `sh -c "..."` pane pid as the engine. Conservative:
// only matches the well-known shell command names (`sh`, `bash`, `zsh`,
// `dash`, `ksh`). Custom wrappers that wrap the engine in a shell still
// land in the fallback bucket and we pick whatever follows.
func isShellArgv(argv string) bool {
	first := firstToken(argv)
	if first == "" {
		return false
	}
	if idx := strings.LastIndexByte(first, '/'); idx >= 0 {
		first = first[idx+1:]
	}
	switch first {
	case "sh", "bash", "zsh", "dash", "ksh", "fish":
		return true
	}
	return false
}

// isCoordRunArgv reports whether argv is the `fleet coord-run`
// supervisor (DESIGN-handoff-drain-storm-leak PR2). Under
// FLEET_LEASE_FAILOVER the supervisor wraps the engine, so it is the
// pane's top process and its argv CONTAINS the engine argv (disambiguator
// + engine name) as its tail. The pid resolver must treat it as a
// wrapper, not the engine — argv[0] basename contains "fleet" AND the
// second token is "coord-run". Conservative: both conditions required so
// an unrelated process that merely mentions "coord-run" in a deeper arg
// is not misclassified.
func isCoordRunArgv(argv string) bool {
	first := firstToken(argv)
	if first == "" {
		return false
	}
	base := first
	if idx := strings.LastIndexByte(base, '/'); idx >= 0 {
		base = base[idx+1:]
	}
	if !strings.Contains(strings.ToLower(base), "fleet") {
		return false
	}
	rest := strings.TrimLeft(argv[len(first):], " \t")
	return firstToken(rest) == "coord-run"
}

// firstToken returns the first whitespace-separated token of s. Empty
// string when s is empty or all whitespace. Used by argv command
// inspection — argv[0] is everything before the first space in the ps
// args column.
func firstToken(s string) string {
	s = strings.TrimLeft(s, " \t")
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i]
		}
	}
	return s
}

// pidResolveTimeout returns the configured pid-resolution budget,
// honoring FLEET_PID_RESOLVE_S for tests / operators on slow-spawn
// engines. Falls back to defaultPidResolveTimeout when unset / invalid.
func pidResolveTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("FLEET_PID_RESOLVE_S"))
	if raw == "" {
		return defaultPidResolveTimeout
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultPidResolveTimeout
	}
	return time.Duration(n) * time.Second
}

// PidResolveTimeout exposes pidResolveTimeout to callers outside the
// spawn package. Used by `fleet maintenance prune-orphan-tmux` to
// compute a freshness window that's never shorter than the spawn
// path's record-write delay — otherwise the sweeper could kill a
// healthy replacement that's still inside the pid-resolution wait
// (codex review iter-3 [P1]).
func PidResolveTimeout() time.Duration {
	return pidResolveTimeout()
}

// pidResolveDisambiguator picks the unique argv substring that
// identifies THIS spawn's engine among potentially many sibling engines
// on the host.
//
// For coord-spawn dispatches, dispatch.go injects `--remote-control
// "fleet-coord-<id>"` into the wrapper script. We extract that exact
// string from execArgv when present — matching against execArgv (not
// opts.Command) catches the injection even though the persisted
// rec.Command stays the clean form. Returns empty when no
// disambiguator is found; the resolver then falls back to the engine
// hint priority.
//
// Match strategy: scan execArgv for any token containing "fleet-coord-"
// + the agent id. We accept the substring rather than equality because
// the dispatch-side injector wraps the name in quotes inside the shell
// wrapper string, so the argv token in the persisted execArgv is
// `"fleet-coord-<id>"` (literal quotes) but the running claude's argv
// in ps has it bare as `fleet-coord-<id>`. We strip quotes when
// extracting.
func pidResolveDisambiguator(agentID string, execArgv []string) string {
	if agentID == "" {
		return ""
	}
	needle := "fleet-coord-" + agentID
	for _, a := range execArgv {
		if strings.Contains(a, needle) {
			return needle
		}
	}
	return ""
}

// pidResolveEngineHint picks the engine binary's command name for the
// engine-match priority. Falls back to oldRec.Engine on handoff so the
// successor inherits the same hint.
//
// command is opts.Command — the spawn's argv before any shell-wrapping.
// When command[0] doesn't match the engine binary name (a custom
// `--command` spawn like `sh -c 'claude --version; sleep 60'`), we
// return EMPTY hint so the resolver falls back to the deepest-non-
// shell-descendant heuristic. Without this guard the engine-match
// would prefer a short-lived `claude --version` helper over the
// actual long-lived process the operator cares about (codex iter-3
// finding, 2026-05-14).
//
// Returns empty for unknown engines or custom commands. "claude-code"
// maps to "claude" (the binary name); "codex" maps to "codex". Future
// engines add a row to the map.
func pidResolveEngineHint(engine string, oldRec *agent.Record, command []string) string {
	eng := engine
	if eng == "" && oldRec != nil {
		eng = oldRec.Engine
	}
	var hint string
	switch eng {
	case "claude-code", "":
		hint = "claude"
	case "codex":
		hint = "codex"
	default:
		return ""
	}
	// Custom-command detection: when command[0] is not the engine
	// binary, drop the hint so we don't latch onto incidental helper
	// processes (e.g. `claude --version` in `sh -c 'claude --version;
	// sleep 60'`). argv[0] being a shell is the canonical custom
	// shape; an empty command list means no spawn (caller error).
	if len(command) == 0 {
		return hint
	}
	if isShellArgv(command[0]) {
		return ""
	}
	// Strip leading path so "/usr/local/bin/claude" matches "claude".
	first := command[0]
	if idx := strings.LastIndexByte(first, '/'); idx >= 0 {
		first = first[idx+1:]
	}
	if first != hint {
		// Custom binary that isn't the engine — empty hint, fall back
		// to the deepest-non-shell heuristic.
		return ""
	}
	return hint
}

// productionResolveDeps returns the wired deps for the resolver. ps and
// tmux are invoked via exec.Command. Each call costs one fork+exec
// (~5-10ms each on a typical laptop) which is fine inside the 10s
// resolver budget; production-side resolution typically converges in
// 1-2 poll cycles after the wrapper shell exec's into claude.
func productionResolveDeps() resolveEnginePidDeps {
	return resolveEnginePidDeps{
		panePID:   tmuxPanePID,
		listProcs: psListProcs,
	}
}

// tmuxPanePID queries tmux for the pid of the active pane in the given
// session. Returns 0 + error if tmux isn't reachable or the session
// doesn't exist. The list-panes invocation honors FLEET_TMUX_SOCKET
// (via tmuxArgsForResolve) so per-test sockets work the same as
// production.
//
// We deliberately don't reuse internal/tmux's helpers here — that
// package centralizes the user-facing tmux operations (Spawn, Attach,
// Kill, etc.) and adding pane-pid plumbing there would couple it to
// the resolver internals. The shell-out is one line and stays here.
func tmuxPanePID(session string) (int, error) {
	args := []string{"list-panes", "-t", session, "-F", "#{pane_pid}"}
	if sock := os.Getenv("FLEET_TMUX_SOCKET"); sock != "" {
		args = append([]string{"-S", sock}, args...)
	}
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return 0, fmt.Errorf("tmux list-panes %s: %w", session, err)
	}
	// First line only — single-window single-pane is the v0.1 spawn
	// shape; a multi-pane session would mean the operator opened
	// splits manually, in which case the FIRST pane is still the one
	// we spawned into.
	line := strings.TrimSpace(string(bytes.SplitN(out, []byte{'\n'}, 2)[0]))
	if line == "" {
		return 0, fmt.Errorf("tmux list-panes %s: empty output", session)
	}
	pid, err := strconv.Atoi(line)
	if err != nil {
		return 0, fmt.Errorf("parse pane pid %q: %w", line, err)
	}
	return pid, nil
}

// psListProcs enumerates every process on the host with `ps -o
// pid,ppid,args -ax`. Output shape (Darwin + Linux):
//
//	  PID  PPID ARGS
//	    1     0 /sbin/launchd
//	 1241  1239 -/bin/zsh
//	...
//
// Header row is skipped. Lines with non-numeric pid/ppid are skipped
// (defensive against ps formatting drift). The ARGS column may contain
// internal whitespace which we preserve verbatim — argvCommandIs only
// reads the first token.
func psListProcs() ([]procEntry, error) {
	out, err := exec.Command("ps", "-o", "pid,ppid,args", "-ax").Output()
	if err != nil {
		return nil, fmt.Errorf("ps list: %w", err)
	}
	return parsePsOutput(out), nil
}

// parsePsOutput splits `ps -o pid,ppid,args -ax` output into procEntry
// rows. Header line is the first line; we skip it by detecting a
// non-numeric pid column. Used internally + by tests.
func parsePsOutput(raw []byte) []procEntry {
	var out []procEntry
	lines := bytes.Split(raw, []byte{'\n'})
	for _, ln := range lines {
		s := string(bytes.TrimLeft(ln, " "))
		if s == "" {
			continue
		}
		// Split into at most 3 fields: pid, ppid, args (args itself
		// can contain spaces — only the first two whitespace gaps
		// matter).
		pid, rest := splitFirstToken(s)
		if pid == "" {
			continue
		}
		ppid, args := splitFirstToken(rest)
		if ppid == "" {
			continue
		}
		pidN, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}
		ppidN, err := strconv.Atoi(ppid)
		if err != nil {
			continue
		}
		out = append(out, procEntry{
			PID:  pidN,
			PPID: ppidN,
			Args: strings.TrimLeft(args, " "),
		})
	}
	return out
}

// splitFirstToken returns (token, rest) where token is the first
// whitespace-separated chunk of s and rest is the remainder with
// leading whitespace stripped. Used by parsePsOutput to peel pid +
// ppid off the front while preserving spaces inside the args column.
func splitFirstToken(s string) (string, string) {
	s = strings.TrimLeft(s, " \t")
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i], strings.TrimLeft(s[i:], " \t")
		}
	}
	return s, ""
}

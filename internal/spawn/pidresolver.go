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
	for {
		procs, err := deps.listProcs()
		if err == nil {
			pid, args := findEngineDescendant(procs, panePid, disambiguator, engineHint)
			if pid > 0 {
				return pid, args, nil
			}
		}
		if !deps.now().Before(deadline) {
			// Best-effort fallback: return pane pid (wrong but live)
			// + empty argv so the caller signals "couldn't disambiguate".
			return panePid, "", nil
		}
		deps.sleep(defaultPidResolvePollInterval)
	}
}

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
func findEngineDescendant(
	procs []procEntry, panePID int,
	disambiguator, engineHint string,
) (int, string) {
	if panePID <= 0 || len(procs) == 0 {
		return 0, ""
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
		// Priority 1: exact disambiguator hit — return immediately
		// UNLESS the match is on a shell wrapper. A shell argv that
		// carries the disambiguator (via `sh -c "claude ... <disam>"`)
		// is just transport — we want the real engine underneath. If
		// the matched entry is a shell, defer to depth-2+ children;
		// the production poll loop will re-walk once claude exec's.
		if disambiguator != "" && strings.Contains(p.Args, disambiguator) {
			if !isShellArgv(p.Args) {
				return p.PID, p.Args
			}
			// Shell carrying the disambiguator — still keep it as a
			// fallback signal so a pane-only walk (no claude yet)
			// doesn't return 0 and burn the timeout for nothing. We
			// don't slot it into bestFallback because the shell filter
			// below excludes shells; instead, we just keep walking.
		}
		// Priority 2: engine command match. Keep the deepest one in
		// case multiple processes share the engine name in the tree.
		if engineHint != "" && argvCommandIs(p.Args, engineHint) {
			if f.depth > bestEngineDepth {
				bestEngine = p
				bestEngineDepth = f.depth
			}
		}
		// Priority 3: fallback to deepest non-shell descendant. We
		// pick this only when the engine-match path also failed.
		if !isShellArgv(p.Args) {
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
		return bestEngine.PID, bestEngine.Args
	}
	if bestFallbackDepth >= 0 {
		return bestFallback.PID, bestFallback.Args
	}
	return 0, ""
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

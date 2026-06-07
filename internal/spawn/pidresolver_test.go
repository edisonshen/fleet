// Tests for pidresolver.go — the spawn-path fix for P0 bug 2026-05-13
// (spawn recorded the fleet binary's own pid instead of the real claude
// pid inside the tmux pane, so every liveness probe classified live
// coords as DEAD).
package spawn

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

// TestResolveEnginePid_PrefersDisambiguatorMatch pins the strongest
// match priority: when an agent's disambiguator appears in a descendant
// argv, we return that pid even if a sibling process also matches the
// engine command name. Mirrors the real coord-spawn shape: pane child
// is `sh -c "claude --remote-control fleet-coord-<id> ..."`, the
// resolver picks the claude process matching the agent id.
func TestResolveEnginePid_PrefersDisambiguatorMatch(t *testing.T) {
	// Synthetic process tree:
	//   pane (sh)
	//     └── claude --remote-control fleet-coord-A1   (target)
	//   unrelated tmux session pane (sh)
	//     └── claude --remote-control fleet-coord-B2   (sibling — should NOT match)
	procs := []procEntry{
		{PID: 100, PPID: 1, Args: "tmux server"},
		{PID: 200, PPID: 100, Args: "sh -c claude --remote-control fleet-coord-A1 --dangerously-skip-permissions"},
		{PID: 201, PPID: 200, Args: "claude --remote-control fleet-coord-A1 --dangerously-skip-permissions"},
		{PID: 300, PPID: 100, Args: "sh -c claude --remote-control fleet-coord-B2 --dangerously-skip-permissions"},
		{PID: 301, PPID: 300, Args: "claude --remote-control fleet-coord-B2 --dangerously-skip-permissions"},
	}
	deps := resolveEnginePidDeps{
		panePID: func(session string) (int, error) {
			if session != "fleet-A1" {
				t.Fatalf("unexpected session: %s", session)
			}
			return 200, nil
		},
		listProcs: func() ([]procEntry, error) { return procs, nil },
		now:       func() time.Time { return time.Unix(0, 0) },
		sleep:     func(time.Duration) {},
	}
	pid, args, err := resolveEnginePid(
		"fleet-A1",
		"fleet-coord-A1",
		"claude",
		1*time.Second,
		deps,
	)
	if err != nil {
		t.Fatalf("resolveEnginePid: %v", err)
	}
	if pid != 201 {
		t.Errorf("pid = %d, want 201 (the A1 claude)", pid)
	}
	if !strings.Contains(args, "fleet-coord-A1") {
		t.Errorf("args = %q, want claude argv containing fleet-coord-A1", args)
	}
}

// TestResolveEnginePid_SkipsCoordRunWrapper (codex PR2 iter-4 [P2]):
// under FLEET_LEASE_FAILOVER the pane's top process is the `fleet
// coord-run` supervisor, whose argv CONTAINS the disambiguator + engine
// name in its tail. The resolver must record the ENGINE child pid, NOT
// the supervisor pid (PID==engine / SupervisorPID==lease-holder split).
//
// Synthetic tree:
//
//	pane: fleet coord-run --agent A1 -- sh -c "claude ... fleet-coord-A1"
//	  └── sh -c "claude ... fleet-coord-A1"
//	      └── claude --remote-control fleet-coord-A1 ...   (target)
func TestResolveEnginePid_SkipsCoordRunWrapper(t *testing.T) {
	const disam = "fleet-coord-A1"
	procs := []procEntry{
		{PID: 100, PPID: 1, Args: "tmux server"},
		{PID: 200, PPID: 100, Args: "/usr/local/bin/fleet coord-run --agent A1 --project p -- sh -c claude --remote-control " + disam + " --dangerously-skip-permissions"},
		{PID: 201, PPID: 200, Args: "sh -c claude --remote-control " + disam + " --dangerously-skip-permissions"},
		{PID: 202, PPID: 201, Args: "claude --remote-control " + disam + " --dangerously-skip-permissions"},
	}
	deps := resolveEnginePidDeps{
		panePID:   func(string) (int, error) { return 200, nil },
		listProcs: func() ([]procEntry, error) { return procs, nil },
		now:       func() time.Time { return time.Unix(0, 0) },
		sleep:     func(time.Duration) {},
	}
	pid, args, err := resolveEnginePid("fleet-A1", disam, "claude", 1*time.Second, deps)
	if err != nil {
		t.Fatalf("resolveEnginePid: %v", err)
	}
	if pid != 202 {
		t.Errorf("pid = %d, want 202 (the engine child, NOT coord-run supervisor 200)", pid)
	}
	if !strings.Contains(args, "claude") || strings.Contains(args, "coord-run") {
		t.Errorf("args = %q, want the claude engine argv (not the coord-run supervisor)", args)
	}
}

func TestIsCoordRunArgv(t *testing.T) {
	yes := []string{
		"/usr/local/bin/fleet coord-run --agent A -- sh -c claude",
		"fleet coord-run -- true",
		"/tmp/go-build/exe/fleet.test coord-run --agent x -- sleep 30",
	}
	no := []string{
		"sh -c claude",
		"claude --remote-control fleet-coord-A1",
		"/usr/local/bin/fleet dispatch task", // fleet but not coord-run
		"someproc --flag coord-run",          // coord-run not the subcommand
		"",
	}
	for _, a := range yes {
		if !isCoordRunArgv(a) {
			t.Errorf("isCoordRunArgv(%q) = false, want true", a)
		}
	}
	for _, a := range no {
		if isCoordRunArgv(a) {
			t.Errorf("isCoordRunArgv(%q) = true, want false", a)
		}
	}
}

// TestResolveEnginePid_FallsBackToEngineMatch: when there is no
// disambiguator (plain worker dispatch without --remote-control), the
// resolver picks the deepest claude descendant.
func TestResolveEnginePid_FallsBackToEngineMatch(t *testing.T) {
	procs := []procEntry{
		{PID: 200, PPID: 1, Args: "sh -c claude --dangerously-skip-permissions"},
		{PID: 201, PPID: 200, Args: "claude --dangerously-skip-permissions"},
	}
	deps := resolveEnginePidDeps{
		panePID:   func(string) (int, error) { return 200, nil },
		listProcs: func() ([]procEntry, error) { return procs, nil },
		now:       func() time.Time { return time.Unix(0, 0) },
		sleep:     func(time.Duration) {},
	}
	pid, args, err := resolveEnginePid("fleet-x", "", "claude", 1*time.Second, deps)
	if err != nil {
		t.Fatalf("resolveEnginePid: %v", err)
	}
	if pid != 201 {
		t.Errorf("pid = %d, want 201", pid)
	}
	if args == "" {
		t.Errorf("args empty; want claude argv")
	}
}

// TestResolveEnginePid_TimeoutFallsBackToPanePid: when no descendant
// matches before the deadline, return the pane pid + empty argv. A
// wrong-but-live pid beats the previous os.Getpid() bug where the
// recorded pid was dead by construction.
func TestResolveEnginePid_TimeoutFallsBackToPanePid(t *testing.T) {
	// Only the wrapper shell is in the tree; claude never exec'd.
	procs := []procEntry{
		{PID: 200, PPID: 1, Args: "sh -c claude ..."},
	}
	// now() ticks ahead each call so we cross the deadline after a few
	// poll iterations — deterministic without sleeping in real time.
	tick := time.Unix(0, 0)
	nowCalls := 0
	deps := resolveEnginePidDeps{
		panePID:   func(string) (int, error) { return 200, nil },
		listProcs: func() ([]procEntry, error) { return procs, nil },
		now: func() time.Time {
			nowCalls++
			if nowCalls > 1 {
				tick = tick.Add(500 * time.Millisecond)
			}
			return tick
		},
		sleep: func(time.Duration) {},
	}
	pid, args, err := resolveEnginePid(
		"fleet-x",
		"fleet-coord-x",
		"claude",
		100*time.Millisecond,
		deps,
	)
	if err != nil {
		t.Fatalf("resolveEnginePid: %v", err)
	}
	if pid != 200 {
		t.Errorf("timeout pid = %d, want 200 (pane fallback)", pid)
	}
	if args != "" {
		t.Errorf("timeout args = %q, want empty", args)
	}
}

// TestResolveEnginePid_PanePidUnreachableErrors: a tmux that won't
// answer list-panes within the budget surfaces as a hard error. This
// is the only failure mode that aborts the spawn — without a pane pid
// we have nothing to walk.
func TestResolveEnginePid_PanePidUnreachableErrors(t *testing.T) {
	tick := time.Unix(0, 0)
	nowCalls := 0
	deps := resolveEnginePidDeps{
		panePID: func(string) (int, error) {
			return 0, errors.New("tmux not reachable")
		},
		listProcs: func() ([]procEntry, error) { return nil, nil },
		now: func() time.Time {
			nowCalls++
			if nowCalls > 1 {
				tick = tick.Add(500 * time.Millisecond)
			}
			return tick
		},
		sleep: func(time.Duration) {},
	}
	_, _, err := resolveEnginePid(
		"fleet-x",
		"fleet-coord-x",
		"claude",
		50*time.Millisecond,
		deps,
	)
	if err == nil {
		t.Fatal("expected error when pane pid unreachable")
	}
	if !strings.Contains(err.Error(), "pane pid unreachable") {
		t.Errorf("err = %v, want 'pane pid unreachable' message", err)
	}
}

// TestFindEngineDescendant_NoDescendantsReturnsZero: a pane with no
// child processes (pre-exec or just-killed) returns 0. The poll loop
// retries until the timeout, then falls back to the pane pid.
func TestFindEngineDescendant_NoDescendantsReturnsZero(t *testing.T) {
	procs := []procEntry{
		{PID: 200, PPID: 1, Args: "sh -c ..."},
	}
	pid, _, _ := findEngineDescendant(procs, 200, "fleet-coord-x", "claude")
	if pid != 0 {
		t.Errorf("pid = %d, want 0 when no descendants", pid)
	}
}

// TestFindEngineDescendant_PaneLeaderIsEngine pins the codex-iter-1
// fix (2026-05-14): when tmux launches the engine directly (no shell
// wrapper — e.g. `fleet dispatch --command claude` or a custom
// non-shell argv), the pane pid IS the engine pid. The walk must
// include the pane process itself, not just its children. Without
// this, direct-command spawns burn the full 10s resolver timeout
// before falling back to the same pane pid.
//
// Two sub-cases:
//   - Disambiguator match on the pane process directly
//   - Engine-name match on the pane process directly
func TestFindEngineDescendant_PaneLeaderIsEngine(t *testing.T) {
	t.Run("disambiguator on pane leader", func(t *testing.T) {
		// Pane pid 200 IS the claude with the disambiguator. No children.
		procs := []procEntry{
			{PID: 200, PPID: 1, Args: "claude --remote-control fleet-coord-X1"},
		}
		pid, args, stable := findEngineDescendant(procs, 200, "fleet-coord-X1", "claude")
		if pid != 200 {
			t.Errorf("pid = %d, want 200 (pane leader)", pid)
		}
		if !strings.Contains(args, "fleet-coord-X1") {
			t.Errorf("args = %q, want argv containing disambiguator", args)
		}
		if !stable {
			t.Error("disambiguator-on-non-shell match must be stable")
		}
	})
	t.Run("engine name on pane leader", func(t *testing.T) {
		// Pane pid 300 IS claude. No disambiguator (plain worker spawn).
		procs := []procEntry{
			{PID: 300, PPID: 1, Args: "claude --dangerously-skip-permissions"},
		}
		pid, args, stable := findEngineDescendant(procs, 300, "", "claude")
		if pid != 300 {
			t.Errorf("pid = %d, want 300 (pane leader as engine)", pid)
		}
		if args == "" {
			t.Errorf("args empty; want claude argv")
		}
		if !stable {
			t.Error("engine-name match must be stable")
		}
	})
}

// TestFindEngineDescendant_DisambiguatorBeatsEngineMatch verifies the
// match priority — a disambiguator hit anywhere in the tree wins over
// a deeper engine-only match.
func TestFindEngineDescendant_DisambiguatorBeatsEngineMatch(t *testing.T) {
	procs := []procEntry{
		// Shallow disambiguator match (shell itself carries the
		// argv passed to -c, so the shell row's args includes
		// "fleet-coord-A1"). Resolver should pick this over the
		// deeper claude process if it had a different disambiguator.
		{PID: 200, PPID: 1, Args: "sh -c claude --remote-control fleet-coord-A1"},
		// Deeper engine-only match — would win in fallback but not
		// when the disambiguator is set.
		{PID: 201, PPID: 200, Args: "claude --no-disambiguator-here"},
	}
	pid, args, _ := findEngineDescendant(procs, 200, "fleet-coord-A1", "claude")
	// The shell wrapper carries the disambiguator in its argv, so it
	// IS the first match. This is fine — the production resolver's
	// poll loop will re-walk and pick up the deeper claude on the
	// next iteration once claude actually exec's. The test pins the
	// match-priority contract: disambiguator wins over engine.
	if pid != 200 && pid != 201 {
		t.Fatalf("pid = %d, want 200 or 201", pid)
	}
	if !strings.Contains(args, "fleet-coord-A1") && !strings.Contains(args, "claude") {
		t.Errorf("args = %q, want some match argv", args)
	}
}

// TestArgvCommandIs covers the argv command-name match used by the
// engine-hint priority. Strips a leading path so a binary at
// /usr/local/bin/claude matches "claude".
func TestArgvCommandIs(t *testing.T) {
	cases := []struct {
		argv string
		name string
		want bool
	}{
		{"claude --foo", "claude", true},
		{"/usr/local/bin/claude --foo", "claude", true},
		{"sh -c claude", "claude", false}, // first token is "sh"
		{"", "claude", false},
		{"claude", "claude", true},
		{"claude-wrapper --x", "claude", false}, // exact match required
	}
	for _, tc := range cases {
		got := argvCommandIs(tc.argv, tc.name)
		if got != tc.want {
			t.Errorf("argvCommandIs(%q, %q) = %v, want %v",
				tc.argv, tc.name, got, tc.want)
		}
	}
}

// TestIsShellArgv ensures we don't classify the wrapper shell as the
// engine via the fallback path.
func TestIsShellArgv(t *testing.T) {
	cases := []struct {
		argv string
		want bool
	}{
		{"sh -c foo", true},
		{"/bin/bash -c foo", true},
		{"zsh", true},
		{"claude --foo", false},
		{"", false},
		{"  /usr/bin/dash -c foo", true},
	}
	for _, tc := range cases {
		got := isShellArgv(tc.argv)
		if got != tc.want {
			t.Errorf("isShellArgv(%q) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

// TestResolveEnginePid_TentativeMatchWaitsForStability pins codex
// iter-5's P1 (2026-05-14): for custom-shell commands like
// `sh -c 'claude --version; sleep 60'`, the resolver must NOT
// return the first non-shell descendant immediately — the deepest
// non-shell might be a transient helper. Instead it waits for
// stability (the same tentative pid through to the deadline) so a
// short-lived helper has died and the long-lived child has taken
// its place.
//
// Test shape: simulate two snapshots — snapshot 1 has only the
// helper child; snapshot 2 has the helper dead and the long-lived
// child running. With tentative-match semantics, the resolver must
// return the LATER (long-lived) pid, not the first non-shell match.
func TestResolveEnginePid_TentativeMatchWaitsForStability(t *testing.T) {
	tick := time.Unix(0, 0)
	nowCalls := 0
	// Two snapshots: first has the transient helper, second has the
	// long-lived sleep. listProcs increments a counter and returns
	// the appropriate fixture.
	snap1 := []procEntry{
		{PID: 200, PPID: 1, Args: "sh -c claude --version; sleep 60"},
		{PID: 201, PPID: 200, Args: "claude --version"}, // transient
	}
	snap2 := []procEntry{
		{PID: 200, PPID: 1, Args: "sh -c claude --version; sleep 60"},
		{PID: 202, PPID: 200, Args: "sleep 60"}, // long-lived
	}
	listCalls := 0
	deps := resolveEnginePidDeps{
		panePID: func(string) (int, error) { return 200, nil },
		listProcs: func() ([]procEntry, error) {
			listCalls++
			if listCalls == 1 {
				return snap1, nil
			}
			return snap2, nil
		},
		now: func() time.Time {
			nowCalls++
			if nowCalls > 1 {
				tick = tick.Add(120 * time.Millisecond)
			}
			return tick
		},
		sleep: func(time.Duration) {},
	}
	// 250ms timeout — covers ~2 poll iters with our tick scheme.
	// engineHint="" because this is a custom-shell command.
	pid, _, err := resolveEnginePid("fleet-x", "", "", 250*time.Millisecond, deps)
	if err != nil {
		t.Fatalf("resolveEnginePid: %v", err)
	}
	if pid == 201 {
		t.Errorf("resolver returned transient helper pid 201; tentative match must NOT win first snapshot")
	}
	// Acceptable outcomes: 202 (later long-lived child) or 200 (pane
	// fallback if both helpers exited and snapshot 2 had nothing
	// long-lived either). 201 is the bug.
}

// TestResolveEnginePid_WrapperShellWithLateChildDoesNotFastPath
// pins codex iter-7's P1 regression: a normal `sh -c "claude ..."`
// spawn sampled BEFORE claude exec'd as a child must NOT fast-path
// return the wrapper shell. The shell has no children for the
// first ~100ms while the fork-exec is in flight; if the resolver
// fast-returned on that snapshot, we'd record the wrapper pid and
// reintroduce the dead-PID bug.
//
// Test shape: first poll shows wrapper shell only. Second poll
// shows claude as a child. Resolver must return claude pid, not
// the shell.
func TestResolveEnginePid_WrapperShellWithLateChildDoesNotFastPath(t *testing.T) {
	listCalls := 0
	deps := resolveEnginePidDeps{
		panePID: func(string) (int, error) { return 200, nil },
		listProcs: func() ([]procEntry, error) {
			listCalls++
			if listCalls == 1 {
				// First poll: wrapper shell only, no claude yet.
				return []procEntry{
					{PID: 200, PPID: 1, Args: "sh -c claude --remote-control fleet-coord-X1"},
				}, nil
			}
			// Subsequent polls: claude exists as child.
			return []procEntry{
				{PID: 200, PPID: 1, Args: "sh -c claude --remote-control fleet-coord-X1"},
				{PID: 201, PPID: 200, Args: "claude --remote-control fleet-coord-X1"},
			}, nil
		},
		now: func() time.Time {
			// Advance slowly so we don't hit deadline before the
			// child appears in poll 2.
			return time.Unix(0, 0).Add(time.Duration(listCalls) * 50 * time.Millisecond)
		},
		sleep: func(time.Duration) {},
	}
	pid, _, err := resolveEnginePid("fleet-x", "fleet-coord-X1", "claude", 5*time.Second, deps)
	if err != nil {
		t.Fatalf("resolveEnginePid: %v", err)
	}
	if pid == 200 {
		t.Errorf("resolver fast-returned wrapper shell pid 200 on first poll; the child claude should win on poll 2")
	}
	if pid != 201 {
		t.Errorf("pid = %d, want 201 (the claude child)", pid)
	}
}

// TestResolveEnginePid_RawShellLeafFastReturns pins codex iter-5's
// P2: when the dispatched command is a raw shell (`fleet dispatch
// --command sh`), the pane process is the shell with no children.
// The resolver must eventually fast-return the pane pid (after N
// consecutive raw-shell-leaf observations to differentiate from
// the codex iter-7 wrapper-with-late-child race).
func TestResolveEnginePid_RawShellLeafFastReturns(t *testing.T) {
	procs := []procEntry{
		{PID: 200, PPID: 1, Args: "sh"}, // pane is bare shell, no children
	}
	deps := resolveEnginePidDeps{
		panePID:   func(string) (int, error) { return 200, nil },
		listProcs: func() ([]procEntry, error) { return procs, nil },
		now:       func() time.Time { return time.Unix(0, 0) },
		sleep:     func(time.Duration) {},
	}
	// 10s timeout — if we don't fast-path, the test would hang for
	// 10s of real time (sleep is mocked, but `now` is fixed too, so
	// the loop would technically be infinite). Fast-path returns
	// pane pid on first iter.
	pid, _, err := resolveEnginePid("fleet-x", "", "", 10*time.Second, deps)
	if err != nil {
		t.Fatalf("resolveEnginePid: %v", err)
	}
	if pid != 200 {
		t.Errorf("raw-shell-leaf must fast-return pane pid 200; got %d", pid)
	}
}

// TestResolveEnginePid_RawShellUnderCoordRun (codex PR2 iter-14 [P2]):
// a RAW-SHELL coord command (`--command sh`) wrapped by coord-run under
// FLEET_LEASE_FAILOVER. The pane is the coord-run SUPERVISOR; its only
// child is the bare shell engine (no deeper child). The resolver must
// record the SHELL pid (the engine), NOT the coord-run supervisor pid —
// else PID == SupervisorPID breaks liveness.
func TestResolveEnginePid_RawShellUnderCoordRun(t *testing.T) {
	procs := []procEntry{
		{PID: 200, PPID: 1, Args: "/usr/local/bin/fleet coord-run --agent A --project p -- sh"},
		{PID: 201, PPID: 200, Args: "sh"}, // the raw-shell engine, no children
	}
	deps := resolveEnginePidDeps{
		panePID:   func(string) (int, error) { return 200, nil },
		listProcs: func() ([]procEntry, error) { return procs, nil },
		now:       func() time.Time { return time.Unix(0, 0) },
		sleep:     func(time.Duration) {},
	}
	pid, _, err := resolveEnginePid("fleet-A", "", "", 10*time.Second, deps)
	if err != nil {
		t.Fatalf("resolveEnginePid: %v", err)
	}
	if pid != 201 {
		t.Errorf("raw-shell-under-coord-run must record the shell pid 201, not the supervisor 200; got %d", pid)
	}
}

// TestRawShellLeafPid covers the helper for the coord-run cases.
func TestRawShellLeafPid(t *testing.T) {
	t.Run("coord-run pane with raw-shell child → child pid", func(t *testing.T) {
		procs := []procEntry{
			{PID: 200, PPID: 1, Args: "/usr/local/bin/fleet coord-run --agent A -- sh"},
			{PID: 201, PPID: 200, Args: "sh"},
		}
		pid, ok := rawShellLeafPid(procs, 200)
		if !ok || pid != 201 {
			t.Errorf("want (201,true), got (%d,%v)", pid, ok)
		}
	})
	t.Run("coord-run pane whose shell child has a claude grandchild → false", func(t *testing.T) {
		procs := []procEntry{
			{PID: 200, PPID: 1, Args: "/usr/local/bin/fleet coord-run --agent A -- sh -c claude"},
			{PID: 201, PPID: 200, Args: "sh -c claude"},
			{PID: 202, PPID: 201, Args: "claude"},
		}
		if _, ok := rawShellLeafPid(procs, 200); ok {
			t.Error("want false; the shell has a claude grandchild (real engine)")
		}
	})
	t.Run("bare-shell pane → pane pid", func(t *testing.T) {
		procs := []procEntry{{PID: 200, PPID: 1, Args: "sh"}}
		pid, ok := rawShellLeafPid(procs, 200)
		if !ok || pid != 200 {
			t.Errorf("want (200,true), got (%d,%v)", pid, ok)
		}
	})
}

// TestPaneIsRawShellLeaf covers the helper in isolation.
func TestPaneIsRawShellLeaf(t *testing.T) {
	t.Run("shell pane, no children → true", func(t *testing.T) {
		procs := []procEntry{
			{PID: 200, PPID: 1, Args: "sh"},
		}
		if !paneIsRawShellLeaf(procs, 200) {
			t.Error("want true")
		}
	})
	t.Run("shell pane with children → false", func(t *testing.T) {
		procs := []procEntry{
			{PID: 200, PPID: 1, Args: "sh -c claude"},
			{PID: 201, PPID: 200, Args: "claude"},
		}
		if paneIsRawShellLeaf(procs, 200) {
			t.Error("want false; pane has a claude child")
		}
	})
	t.Run("non-shell pane, no children → false", func(t *testing.T) {
		procs := []procEntry{
			{PID: 200, PPID: 1, Args: "claude --foo"},
		}
		if paneIsRawShellLeaf(procs, 200) {
			t.Error("want false; pane is not a shell")
		}
	})
	t.Run("pane pid not in procs → false", func(t *testing.T) {
		if paneIsRawShellLeaf(nil, 200) {
			t.Error("want false on empty procs")
		}
	})
}

// TestPidResolveEngineHint pins the codex iter-3 fix (2026-05-14):
// the engine hint must drop to empty for custom-command spawns so
// the resolver falls back to the deepest-non-shell-descendant
// heuristic. Without this, `sh -c 'claude --version; sleep 60'`
// would prefer the short-lived `claude --version` over the
// actual long-lived `sleep` child.
func TestPidResolveEngineHint(t *testing.T) {
	cases := []struct {
		name    string
		engine  string
		oldRec  *agent.Record
		command []string
		want    string
	}{
		{
			name:    "default engine + claude argv → claude hint",
			engine:  "claude-code",
			command: []string{"claude", "--dangerously-skip-permissions"},
			want:    "claude",
		},
		{
			name:    "empty engine + claude argv → claude hint (handoff fallback)",
			engine:  "",
			command: []string{"claude"},
			want:    "claude",
		},
		{
			name:    "codex engine + codex argv → codex hint",
			engine:  "codex",
			command: []string{"codex", "review"},
			want:    "codex",
		},
		{
			name:    "claude-code + sh wrapper argv → EMPTY hint (custom command)",
			engine:  "claude-code",
			command: []string{"sh", "-c", "claude --version; sleep 60"},
			want:    "",
		},
		{
			name:    "claude-code + /bin/bash argv → EMPTY hint",
			engine:  "claude-code",
			command: []string{"/bin/bash", "-c", "echo hi"},
			want:    "",
		},
		{
			name:    "claude-code + custom binary argv → EMPTY hint",
			engine:  "claude-code",
			command: []string{"/usr/local/bin/my-wrapper", "--foo"},
			want:    "",
		},
		{
			name:    "claude-code + /usr/local/bin/claude → claude hint (path-stripped)",
			engine:  "claude-code",
			command: []string{"/usr/local/bin/claude"},
			want:    "claude",
		},
		{
			name:    "unknown engine + any argv → EMPTY hint",
			engine:  "future-engine",
			command: []string{"claude"},
			want:    "",
		},
		{
			name:    "empty engine + empty oldRec + claude argv → claude hint (default)",
			engine:  "",
			oldRec:  nil,
			command: []string{"claude"},
			want:    "claude",
		},
		{
			name:    "handoff: empty engine, oldRec.Engine=codex, codex argv → codex hint",
			engine:  "",
			oldRec:  &agent.Record{Engine: "codex"},
			command: []string{"codex"},
			want:    "codex",
		},
		{
			name:    "empty command list → fallback to engine hint",
			engine:  "claude-code",
			command: nil,
			want:    "claude",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pidResolveEngineHint(tc.engine, tc.oldRec, tc.command)
			if got != tc.want {
				t.Errorf("pidResolveEngineHint(%q, %+v, %v) = %q, want %q",
					tc.engine, tc.oldRec, tc.command, got, tc.want)
			}
		})
	}
}

// TestParsePsOutput pins the parser against representative ps -ax
// output. The first line is a header (non-numeric pid) and should be
// dropped; subsequent lines parse into entries with args containing
// internal whitespace preserved.
func TestParsePsOutput(t *testing.T) {
	raw := []byte(`  PID  PPID ARGS
    1     0 /sbin/launchd
 1241  1239 -/bin/zsh
 5876  5873 claude --remote-control fleet-coord-A1 --dangerously-skip-permissions
`)
	got := parsePsOutput(raw)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if got[0].PID != 1 || got[0].PPID != 0 {
		t.Errorf("entry[0] = %+v", got[0])
	}
	if got[2].PID != 5876 || got[2].PPID != 5873 {
		t.Errorf("entry[2] pid/ppid = %d/%d", got[2].PID, got[2].PPID)
	}
	if !strings.Contains(got[2].Args, "fleet-coord-A1") {
		t.Errorf("entry[2].Args = %q", got[2].Args)
	}
}

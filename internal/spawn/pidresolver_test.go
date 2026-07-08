// Tests for pidresolver.go — the spawn-path fix for P0 bug 2026-05-13
// (spawn recorded the fleet binary's own pid instead of the real claude
// pid inside the tmux pane, so every liveness probe classified live
// coords as DEAD).
package spawn

import (
	"errors"
	"fmt"
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
// the pane's top process is the `fleet coord-run` supervisor, whose argv
// CONTAINS the disambiguator + engine
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
// a RAW-SHELL coord command (`--command sh`) wrapped by coord-run. The pane
// is the coord-run SUPERVISOR; its only
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

// TestSetFakeResolver pins the fake-tmux pid-resolver seam used by
// internal/testutil/tmuxfake.InstallFake (/review testing finding: the
// fast-clock logic was load-bearing for CI speed but had no direct
// in-package test, so a regression would only resurface as a silent
// ~1s-per-spawn CI slowdown).
//
// It verifies the two resolve shapes return INSTANTLY (the injected
// fast-clock + no-op sleep make even a huge timeout return on the first
// poll) and that the restore func re-points the seam at production.
func TestSetFakeResolver(t *testing.T) {
	const fakePid = 2_000_000_000

	// Production default before install.
	if resolveDepsFn == nil {
		t.Fatal("resolveDepsFn must default to a non-nil deps factory")
	}

	restore := SetFakeResolver()
	t.Cleanup(restore) // safety net: restore seam even if a t.Fatalf fires before explicit restore()

	// IMPORTANT: call resolveDepsFn() FRESH per resolve — each invocation
	// returns deps with its own fast-clock call counter, exactly as the
	// production spawn path does (one resolveDepsFn() per spawn). Reusing a
	// single deps across two resolves would freeze the clock past the first
	// deadline and hang, which is not how the seam is used.

	// (1) Engine-hint spawn: "claude" hint matches the fake leaf's argv as
	// a STABLE match → instant return of fakePid. A large timeout proves
	// the injected clock/sleep short-circuit it (real time would block).
	pid, _, err := resolveEnginePid("fleet-x", "", "claude", time.Hour, resolveDepsFn())
	if err != nil {
		t.Fatalf("engine-hint resolve: %v", err)
	}
	if pid != fakePid {
		t.Fatalf("engine-hint pid = %d, want %d", pid, fakePid)
	}

	// (2) Custom-command spawn (empty hint, e.g. []string{"sleep","60"}):
	// the fake leaf is only a TENTATIVE match, so the resolver would poll
	// to the deadline with real time. The fast-clock makes it accept the
	// tentative match on the first poll → still instant, still fakePid.
	pid, _, err = resolveEnginePid("fleet-y", "", "", time.Hour, resolveDepsFn())
	if err != nil {
		t.Fatalf("custom-command resolve: %v", err)
	}
	if pid != fakePid {
		t.Fatalf("custom-command pid = %d, want %d", pid, fakePid)
	}

	// (3) Restore re-points the seam at production.
	restore()
	if got := fnPtr(resolveDepsFn); got != fnPtr(productionResolveDeps) {
		t.Fatal("restore() did not re-point resolveDepsFn at productionResolveDeps")
	}
}

// fnPtr returns a comparable identity for a func value (Go forbids direct
// func == func). Reflect-free: format the value's address via fmt.
func fnPtr(f func() resolveEnginePidDeps) string {
	return fmt.Sprintf("%p", f)
}

package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
)

// stubCoordArchived replaces coordArchivedFn for tests so we can mark a
// CoordID as terminally stale (its holder record lives in the archive)
// without seeding real archive files. IDs in archived return true.
type stubCoordArchived struct {
	archived map[string]bool
}

func (s *stubCoordArchived) install(t *testing.T) {
	t.Helper()
	prev := coordArchivedFn
	coordArchivedFn = func(id string) bool {
		return s.archived[id]
	}
	t.Cleanup(func() { coordArchivedFn = prev })
}

// stubResetReap replaces resetReapFn for tests so [r] confirm doesn't
// fork real `fleet`. Records each call's (project, coordIDs) and returns
// a canned resetDoneMsg (success by default; set retErr to simulate a
// reap failure).
type stubResetReap struct {
	calls  []resetReapCall
	retErr error
	retOut string
	reaped int // override; if 0 uses len(coordIDs)
}

type resetReapCall struct {
	project  string
	coordIDs []string
}

func (s *stubResetReap) install(t *testing.T) {
	t.Helper()
	prev := resetReapFn
	resetReapFn = func(project string, coordIDs []string) tea.Msg {
		s.calls = append(s.calls, resetReapCall{
			project:  project,
			coordIDs: append([]string(nil), coordIDs...),
		})
		reaped := s.reaped
		if reaped == 0 {
			reaped = len(coordIDs)
		}
		return resetDoneMsg{
			projectName: project,
			reaped:      reaped,
			out:         s.retOut,
			err:         s.retErr,
		}
	}
	t.Cleanup(func() { resetReapFn = prev })
}

// coordRecord builds an agent record tagged as the coord for project.
func coordRecord(id, project string) *agent.Record {
	r := agent.New(id)
	r.Project = project
	r.TaskID = coordTaskID(project)
	r.TmuxSession = "fleet-" + id
	return r
}

// ---------------------------------------------------------------------
// D3 — stale-CoordID fall-through (actionAttachProject Path 1)
// ---------------------------------------------------------------------

func buggyArchivedCoordIDPreflightWouldOfferReset(t *testing.T, project string, resolve func(string, string) (coordreconcile.Verdict, error)) bool {
	t.Helper()
	verdict, err := resolve(project, "")
	return err != nil || verdict.Decision != coordreconcile.Attach
}

func flashSuggestsReset(f *flashMsg) bool {
	if f == nil {
		return false
	}
	text := strings.ToLower(f.text)
	return strings.Contains(text, "[r]") || strings.Contains(text, "reset")
}

// TestKeyA_StaleCoordID_Table is the core regression for the
// dead-end-recovery root cause (2026-05-28): a coordinator.lock body
// named an ARCHIVED coord, the dashboard derived CoordID from it, and
// Path 1's ID-only resolve refused to scan — so [a] looped "pending
// refresh" forever while a live successor sat unreachable.
//
// The table separates the two causes that produce a nil resolveCoord
// Record with CoordID set:
//
//   - transient-race: holder NOT archived, record simply not loaded yet
//     → MUST keep the "pending refresh" flash + no scan (iter-1 P2 guard).
//   - terminal-stale-with-successor: holder archived, a live successor
//     coord exists → MUST attach to the successor (pendingAttach set),
//     NOT loop pending-refresh.
//   - boot/handoff-wait: holder archived, but the lease names a live
//     in-progress coord → MUST enter the safe [a] retry path with no
//     destructive [r] hint.
//   - terminal-stale-no-successor: holder archived, no successor → MUST
//     surface the stale-lock diagnosis + [r] reset hint, NOT pending-
//     refresh.
func TestKeyA_StaleCoordID_Table(t *testing.T) {
	const project = "demo"
	const staleID = "129c9824" // the archived holder named in the lock body
	const liveID = "68c6db50"  // the live successor coord
	const preallocID = "prea110c"

	tests := []struct {
		name                    string
		archived                bool
		successor               *agent.Record // loaded into m.records as a coord, if non-nil
		verdict                 coordreconcile.Verdict
		outcome                 string
		assertOldWouldShowReset bool
	}{
		{
			name:     "transient-race-keeps-pending-refresh",
			archived: false,
			outcome:  "pending-refresh",
		},
		{
			name:      "terminal-stale-with-successor-attaches",
			archived:  true,
			successor: coordRecord(liveID, project),
			verdict: coordreconcile.Verdict{
				Decision: coordreconcile.Attach,
				Owner:    coordlock.Owner{AgentID: liveID, PID: 1},
				Reason:   "successor published",
			},
			outcome: "attach-successor",
		},
		{
			name:     "boot-in-flight-waits-without-reset",
			archived: true,
			verdict: coordreconcile.Verdict{
				Decision: coordreconcile.Wait,
				Reason:   "coord boot in flight (starting within TTL)",
			},
			outcome:                 "safe-retry",
			assertOldWouldShowReset: true,
		},
		{
			name:     "handoff-reservation-waits-without-reset",
			archived: true,
			verdict: coordreconcile.Verdict{
				Decision: coordreconcile.Wait,
				Reason:   "handoff reservation naming successor succ7788",
			},
			outcome: "safe-retry",
		},
		{
			name:     "terminal-stale-no-successor-surfaces-reset",
			archived: true,
			verdict: coordreconcile.Verdict{
				Decision: coordreconcile.Spawn,
				Reason:   "free lease claimed (starting record written)",
			},
			outcome: "reset-hint",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withFleetHome(t)
			(&stubSessionAlive{}).install(t)
			(&stubSessionProbe{}).install(t)
			(&stubCoordArchived{archived: map[string]bool{staleID: tc.archived}}).install(t)
			stubCmd := &stubFleetCmd{}
			stubCmd.install(t)

			var records []*agent.Record
			if tc.successor != nil {
				records = append(records, tc.successor)
			}
			resolveCalls := 0
			prevResolve := tuiCoordResolveFn
			prevOwner := tuiCoordCurrentOwnerFn
			prevNewID := tuiCoordNewAgentIDFn
			prevTick := tuiCoordResolveWaitTickFn
			tuiCoordNewAgentIDFn = func() string { return preallocID }
			tuiCoordResolveWaitTickFn = func(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
				return func() tea.Msg { return f(time.Unix(0, 0)) }
			}
			tuiCoordResolveFn = func(gotProject, agentID string) (coordreconcile.Verdict, error) {
				if gotProject != project {
					t.Fatalf("Resolve project = %q, want %q", gotProject, project)
				}
				if agentID == "" {
					return coordreconcile.Verdict{Decision: coordreconcile.Wait, Reason: "lease free but caller has no id to claim"}, nil
				}
				resolveCalls++
				if agentID != preallocID {
					t.Fatalf("stale CoordID preflight must use preallocated id %q; got %q", preallocID, agentID)
				}
				return tc.verdict, nil
			}
			tuiCoordCurrentOwnerFn = func(string) (coordlock.Owner, bool) {
				if tc.successor == nil {
					return coordlock.Owner{}, false
				}
				return coordlock.Owner{AgentID: tc.successor.ID, PID: 1}, true
			}
			t.Cleanup(func() {
				tuiCoordResolveFn = prevResolve
				tuiCoordCurrentOwnerFn = prevOwner
				tuiCoordNewAgentIDFn = prevNewID
				tuiCoordResolveWaitTickFn = prevTick
			})
			if tc.assertOldWouldShowReset && !buggyArchivedCoordIDPreflightWouldOfferReset(t, project, tuiCoordResolveFn) {
				t.Fatal("adversarial guard failed: old empty-agentID preflight would not have offered reset")
			}

			m := New("test")
			m.records = records
			m.dashboard = &Snapshot{
				Projects: []*ProjectRow{{Name: project, CoordID: staleID}},
			}
			for i, r := range m.dashboardRows() {
				if r.kind == rowProject && r.project != nil && r.project.Name == project {
					m.dashCursor = i
					break
				}
			}

			updated, cmd := m.Update(keyMsg("a"))
			mm := updated.(Model)

			switch tc.outcome {
			case "pending-refresh":
				if resolveCalls != 0 {
					t.Fatalf("transient refresh race must not re-resolve; calls=%d", resolveCalls)
				}
				if cmd != nil {
					t.Fatalf("pending-refresh path must not quit or dispatch; cmd=%T", cmd())
				}
				if mm.pendingAttach != "" {
					t.Fatalf("pendingAttach = %q; want empty", mm.pendingAttach)
				}
				if mm.flash == nil || !mm.flash.isErr || !strings.Contains(mm.flash.text, "pending refresh") {
					t.Fatalf("flash = %+v; want pending refresh error", mm.flash)
				}
				if flashSuggestsReset(mm.flash) {
					t.Fatalf("pending-refresh flash must not suggest reset; got %q", mm.flash.text)
				}
			case "attach-successor":
				if resolveCalls != 1 {
					t.Fatalf("successor path Resolve calls = %d, want 1", resolveCalls)
				}
				if cmd == nil {
					t.Fatal("successor attach must return tea.Quit cmd")
				}
				if mm.pendingAttach != "fleet-"+liveID {
					t.Fatalf("pendingAttach = %q; want fleet-%s", mm.pendingAttach, liveID)
				}
			case "safe-retry":
				if resolveCalls != 1 {
					t.Fatalf("safe-retry path Resolve calls = %d, want 1", resolveCalls)
				}
				if cmd == nil {
					t.Fatal("safe-retry path must return bounded retry tick")
				}
				msg, ok := cmd().(coordResolveRetryMsg)
				if !ok {
					t.Fatalf("safe-retry cmd produced %T, want coordResolveRetryMsg", cmd())
				}
				if msg.projectName != project || msg.agentID != preallocID || msg.context != "project" {
					t.Fatalf("retry msg = %+v; want project=%q agent=%q context=project", msg, project, preallocID)
				}
				if mm.pendingAttach != "" {
					t.Fatalf("pendingAttach = %q; want empty during safe retry", mm.pendingAttach)
				}
				if flashSuggestsReset(mm.flash) {
					t.Fatalf("live Wait flash must not suggest reset; got %q", mm.flash.text)
				}
			case "reset-hint":
				if resolveCalls != 1 {
					t.Fatalf("reset path Resolve calls = %d, want 1", resolveCalls)
				}
				if cmd != nil {
					t.Fatalf("reset-hint path must not quit or dispatch; cmd=%T", cmd())
				}
				if mm.pendingAttach != "" {
					t.Fatalf("pendingAttach = %q; want empty", mm.pendingAttach)
				}
				if mm.flash == nil || !mm.flash.isErr || !strings.Contains(mm.flash.text, "press [r] to reset") {
					t.Fatalf("flash = %+v; want press [r] reset hint", mm.flash)
				}
				if strings.Contains(mm.flash.text, "pending refresh") {
					t.Fatalf("reset flash must not collapse to pending refresh; got %q", mm.flash.text)
				}
			default:
				t.Fatalf("unknown outcome %q", tc.outcome)
			}
			// Never shell out on these paths (no dispatch).
			if len(stubCmd.calls) != 0 {
				t.Errorf("[a] stale-CoordID paths must not shell out; got %v", stubCmd.calls)
			}
		})
	}
}

// ---------------------------------------------------------------------
// D4 — [r] reset
// ---------------------------------------------------------------------

// projectRowResetModel builds a Model with a single project row carrying
// the supplied coord records, cursor landed on the project row.
func projectRowResetModel(t *testing.T, project string, records []*agent.Record) Model {
	t.Helper()
	m := New("test")
	m.records = records
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: project}},
	}
	for i, r := range m.dashboardRows() {
		if r.kind == rowProject && r.project != nil && r.project.Name == project {
			m.dashCursor = i
			return m
		}
	}
	t.Fatalf("no rowProject for %q", project)
	return m
}

// TestKeyR_OnProjectRow_EntersConfirmReset verifies [r] on a project row
// freezes the coord IDs and enters modeConfirmReset without mutating.
func TestKeyR_OnProjectRow_EntersConfirmReset(t *testing.T) {
	withFleetHome(t)
	reap := &stubResetReap{}
	reap.install(t)

	c1 := coordRecord("129c9824", "demo")
	c2 := coordRecord("68c6db50", "demo")
	other := coordRecord("aaaa1111", "otherproj") // not demo's coord
	m := projectRowResetModel(t, "demo", []*agent.Record{c1, c2, other})

	updated, cmd := m.Update(keyMsg("r"))
	mm := updated.(Model)

	if mm.mode != modeConfirmReset {
		t.Fatalf("mode = %v; want modeConfirmReset", mm.mode)
	}
	if cmd != nil {
		t.Errorf("[r] press must not produce a cmd (no mutation until confirm); got one")
	}
	if mm.resetProjectCandidate != "demo" {
		t.Errorf("resetProjectCandidate = %q; want demo", mm.resetProjectCandidate)
	}
	if len(mm.resetCoordIDs) != 2 {
		t.Fatalf("resetCoordIDs = %v; want the 2 demo coord IDs", mm.resetCoordIDs)
	}
	// Confirm render shows the count.
	out := mm.View()
	if !strings.Contains(out, "Reset project demo") || !strings.Contains(out, "Reaps 2 coord") {
		t.Errorf("confirm render missing reset prompt/count; got:\n%s", out)
	}
	if len(reap.calls) != 0 {
		t.Errorf("[r] press must not reap; got %v", reap.calls)
	}
}

// TestKeyR_Confirm_Y_ReapsAndRespawns verifies y in modeConfirmReset
// fires the reap cmd against the frozen coord IDs, and the resetDoneMsg
// handler then spawns a fresh coord.
func TestKeyR_Confirm_Y_ReapsAndRespawns(t *testing.T) {
	withFleetHome(t)
	seedProjectMeta(t, "demo", t.TempDir()) // [r] respawn binds via meta (PR3)
	reap := &stubResetReap{}
	reap.install(t)
	(&stubSessionAlive{}).install(t)
	spawn := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			out := "agent abcd1234 spawned\n  task: coord-demo\n  project: demo\n  tmux: fleet-abcd1234\n"
			return coordSpawnDoneMsgFromArgs(args, out, nil)
		},
	}
	spawn.install(t)

	c1 := coordRecord("129c9824", "demo")
	c2 := coordRecord("68c6db50", "demo")
	m := projectRowResetModel(t, "demo", []*agent.Record{c1, c2})

	// [r] → confirm mode.
	updated, _ := m.Update(keyMsg("r"))
	m = updated.(Model)

	// y → reap cmd.
	updated, cmd := m.Update(keyMsg("y"))
	mm := updated.(Model)

	if mm.mode != modeNav {
		t.Errorf("mode after y = %v; want modeNav", mm.mode)
	}
	if !mm.opInFlight("demo") {
		t.Errorf("coordOpInFlight[demo] must be set to gate stray [a] during reap")
	}
	if cmd == nil {
		t.Fatal("y must produce the reap cmd; got nil")
	}
	// Drain the reap cmd → resetDoneMsg.
	msg := cmd()
	done, ok := msg.(resetDoneMsg)
	if !ok {
		t.Fatalf("reap cmd produced %T; want resetDoneMsg", msg)
	}
	if len(reap.calls) != 1 {
		t.Fatalf("reap called %d times; want 1", len(reap.calls))
	}
	if reap.calls[0].project != "demo" {
		t.Errorf("reap project = %q; want demo", reap.calls[0].project)
	}
	if len(reap.calls[0].coordIDs) != 2 {
		t.Errorf("reap coordIDs = %v; want both demo coords", reap.calls[0].coordIDs)
	}

	// Feed resetDoneMsg back to Update → should spawn fresh coord.
	updated, cmd = mm.Update(done)
	mm = updated.(Model)
	if mm.flash == nil || !strings.Contains(mm.flash.text, "reaped 2 coord") {
		t.Errorf("post-reap flash missing count; got %+v", mm.flash)
	}
	if cmd == nil {
		t.Fatal("resetDoneMsg(success) must spawn a fresh coord; got nil cmd")
	}
	// Draining the batch fires the coord spawn dispatch.
	_ = cmd()
	var sawDispatch bool
	for _, call := range spawn.calls {
		if len(call) >= 2 && call[0] == "dispatch" && call[1] == "coord-demo" {
			sawDispatch = true
		}
	}
	if !sawDispatch {
		t.Errorf("expected a `dispatch coord-demo` spawn after reset; got %v", spawn.calls)
	}
}

// TestKeyR_Confirm_Esc_Cancels verifies esc/n in modeConfirmReset cancels
// with no reap, no respawn, no in-flight flag.
func TestKeyR_Confirm_Esc_Cancels(t *testing.T) {
	for _, key := range []string{"esc", "n", "N"} {
		t.Run(key, func(t *testing.T) {
			withFleetHome(t)
			reap := &stubResetReap{}
			reap.install(t)

			c1 := coordRecord("129c9824", "demo")
			m := projectRowResetModel(t, "demo", []*agent.Record{c1})

			updated, _ := m.Update(keyMsg("r"))
			m = updated.(Model)

			updated, cmd := m.Update(keyMsg(key))
			mm := updated.(Model)

			if mm.mode != modeNav {
				t.Errorf("mode after %s = %v; want modeNav", key, mm.mode)
			}
			if cmd != nil {
				t.Errorf("%s must not produce a cmd; got one", key)
			}
			if mm.resetProjectCandidate != "" || mm.resetCoordIDs != nil {
				t.Errorf("frozen reset state not cleared after %s: %q %v",
					key, mm.resetProjectCandidate, mm.resetCoordIDs)
			}
			if mm.opInFlight("demo") {
				t.Errorf("%s must NOT set coordOpInFlight", key)
			}
			if len(reap.calls) != 0 {
				t.Errorf("%s must not reap; got %v", key, reap.calls)
			}
		})
	}
}

// TestKeyR_ReapFailure_NoRespawn verifies a reap failure surfaces the
// error and does NOT spawn a fresh coord (half-reaped project must not
// get a racing coord).
func TestKeyR_ReapFailure_NoRespawn(t *testing.T) {
	withFleetHome(t)
	reap := &stubResetReap{retErr: errors.New("rm 129c9824: lock busy"), reaped: 1}
	reap.install(t)
	spawn := &stubFleetCmd{}
	spawn.install(t)

	c1 := coordRecord("129c9824", "demo")
	m := projectRowResetModel(t, "demo", []*agent.Record{c1})

	updated, _ := m.Update(keyMsg("r"))
	m = updated.(Model)
	updated, cmd := m.Update(keyMsg("y"))
	mm := updated.(Model)

	msg := cmd()
	done := msg.(resetDoneMsg)
	if done.err == nil {
		t.Fatal("expected reap error")
	}
	updated, _ = mm.Update(done)
	mm = updated.(Model)

	if mm.flash == nil || !mm.flash.isErr {
		t.Fatalf("expected error flash on reap failure; got %+v", mm.flash)
	}
	if !strings.Contains(mm.flash.text, "re-press [r]") {
		t.Errorf("error flash should point at retry; got %q", mm.flash.text)
	}
	if mm.opInFlight("demo") {
		t.Errorf("in-flight gate must be cleared on reap failure")
	}
	if len(spawn.calls) != 0 {
		t.Errorf("reap failure must NOT spawn a fresh coord; got %v", spawn.calls)
	}
}

// TestKeyR_NonProjectRow_Flashes verifies [r] on an agent / task /
// worker / separator row flashes "doesn't apply" with no mutation.
func TestKeyR_NonProjectRow_Flashes(t *testing.T) {
	withFleetHome(t)
	reap := &stubResetReap{}
	reap.install(t)

	// Agent row (right panel). Build a model with a loose agent record so
	// the dashboard renders an agent row, and land the cursor on it.
	a := sampleAgent("loose001")
	m := New("test")
	m.records = []*agent.Record{a}
	m.aliveByID = map[string]bool{a.ID: true}
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "proj"}},
	}
	// Land cursor on a non-project row: find an agent row in the rows.
	var found bool
	for i, r := range m.dashboardRows() {
		if r.kind == rowAgent {
			m.dashCursor = i
			found = true
			break
		}
	}
	if !found {
		t.Skip("no agent row rendered in this harness; covered by mode-gate unit")
	}

	updated, cmd := m.Update(keyMsg("r"))
	mm := updated.(Model)

	if mm.mode != modeNav {
		t.Errorf("mode = %v; want modeNav (no confirm)", mm.mode)
	}
	if cmd != nil {
		t.Errorf("[r] on non-project row must not produce a cmd")
	}
	if mm.flash == nil || !mm.flash.isErr ||
		!strings.Contains(mm.flash.text, "applies to project rows") {
		t.Errorf("expected doesn't-apply flash; got %+v", mm.flash)
	}
	if len(reap.calls) != 0 {
		t.Errorf("[r] on non-project row must not reap; got %v", reap.calls)
	}
}

// TestReapCoordIDsForProject_UnionsOnDiskCoords is the codex iter-1 [P1]
// regression: [r] reset must reap the CURRENT on-disk coord set, not just
// the in-memory snapshot frozen at press time. If [r] is pressed during
// the dashboard/agents refresh race (or while a spawn is in flight),
// m.records — and therefore the frozen IDs — can MISS a live coord. The
// production reap re-reads on-disk state and UNIONs it so the missed coord
// still gets reaped (otherwise: gc clears the lock, a replacement spawns,
// and the missed coord stays alive → split-brain).
func TestReapCoordIDsForProject_UnionsOnDiskCoords(t *testing.T) {
	pdir := withFleetHome(t)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(pdir), "agents"), 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	const project = "demo"

	// On-disk coord records for the project. The "missed" one is the live
	// coord the press-time snapshot didn't have loaded yet.
	frozenCoord := coordRecord("11110000", project)
	missedCoord := coordRecord("22220000", project)
	otherCoord := coordRecord("33330000", "otherproj") // different project
	for _, r := range []*agent.Record{frozenCoord, missedCoord, otherCoord} {
		if err := r.Write(); err != nil {
			t.Fatalf("seed on-disk record %s: %v", r.ID, err)
		}
	}

	// Frozen set captured at press time saw only frozenCoord (the race
	// missed missedCoord).
	frozen := []string{frozenCoord.ID}

	got, err := reapCoordIDsForProject(project, frozen)
	if err != nil {
		t.Fatalf("reapCoordIDsForProject: %v", err)
	}

	// Must include BOTH demo coords, and NOT the other project's coord.
	wantSet := map[string]bool{frozenCoord.ID: true, missedCoord.ID: true}
	if len(got) != len(wantSet) {
		t.Fatalf("reap set = %v; want exactly the 2 demo coords (frozen+on-disk union)", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if !wantSet[id] {
			t.Errorf("reap set includes unexpected id %q (want only demo coords); got %v", id, got)
		}
		if seen[id] {
			t.Errorf("reap set has duplicate id %q; got %v", id, got)
		}
		seen[id] = true
	}
	if !seen[missedCoord.ID] {
		t.Errorf("reap set must include the on-disk coord missed by the snapshot (%s); got %v",
			missedCoord.ID, got)
	}
	// Frozen ID must come first (deterministic order: prompt-counted set,
	// then newly-discovered).
	if got[0] != frozenCoord.ID {
		t.Errorf("frozen IDs must lead the reap order; got[0]=%q want %q", got[0], frozenCoord.ID)
	}
}

// TestReapCoordIDsForProject_EmptyFrozenStillReapsLive covers the
// worst-case race: [r] pressed before ANY coord loaded into m.records, so
// the frozen set is empty. The reap must still discover and reap the live
// on-disk coord — an empty frozen set must NOT mean "reap nothing then
// spawn a duplicate".
func TestReapCoordIDsForProject_EmptyFrozenStillReapsLive(t *testing.T) {
	pdir := withFleetHome(t)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(pdir), "agents"), 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	const project = "demo"
	live := coordRecord("44440000", project)
	if err := live.Write(); err != nil {
		t.Fatalf("seed live coord: %v", err)
	}

	got, err := reapCoordIDsForProject(project, nil)
	if err != nil {
		t.Fatalf("reapCoordIDsForProject: %v", err)
	}
	if len(got) != 1 || got[0] != live.ID {
		t.Fatalf("empty frozen set must still reap the live on-disk coord %s; got %v", live.ID, got)
	}
}

// TestReapCoordIDsForProject_EnumErrorAborts is the codex iter-4 [P2]
// regression: when agent.List() fails (agents dir unreadable), the reap
// must NOT silently fall back to the frozen set — it returns an error so
// resetReapFn aborts before clearing the lock + respawning. Proceeding on
// an incomplete coord set in the exact race this guards could strand a
// live coord alongside the replacement (split-brain).
func TestReapCoordIDsForProject_EnumErrorAborts(t *testing.T) {
	pdir := withFleetHome(t)
	// Make the agents dir a FILE so os.ReadDir errors with ENOTDIR — a
	// non-not-exist error agent.List() surfaces (List returns nil for a
	// missing dir, so we force a real read error instead).
	agentsPath := filepath.Join(filepath.Dir(pdir), "agents")
	if err := os.WriteFile(agentsPath, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("seed agents-as-file: %v", err)
	}

	got, err := reapCoordIDsForProject("demo", []string{"11110000"})
	if err == nil {
		t.Fatal("expected enumeration error when agents dir is unreadable; got nil")
	}
	// The frozen set is still returned (for diagnostics) but the error is
	// the load-bearing signal the reap aborts on.
	if len(got) != 1 || got[0] != "11110000" {
		t.Errorf("frozen set should pass through on error; got %v", got)
	}
}

// TestCoordLockRefusal is the codex iter-2 [P1] regression: `fleet gc
// --kinds=coord-locks --apply` exits 0 even when it REFUSES to unlink a
// lock (verb=surface — live holder / ambiguous probe / inode-swap
// abort). The reset must NOT treat that as a cleared lock, or it spawns
// a fresh coord while the stale lock + old holder persist (split-brain).
// coordLockRefusal parses the gc report and returns the refusal reason
// (non-empty) when any coord-locks line was not `removed`.
func TestCoordLockRefusal(t *testing.T) {
	tests := []struct {
		name       string
		gcOutput   string
		wantReason string // "" means "no refusal" (success)
	}{
		{
			name: "removed-is-success",
			gcOutput: "fleet gc — mode=apply aggressive=false max-age=24h0m0s\n" +
				"coord-locks  /x/.locks/coordinator.lock  verb=removed  reason=stale tmux (session fleet-129c9824 gone for agent 129c9824)\n" +
				"summary: 0 sockets, 0 agents, 0 tmux (surface only by default), 0 worktrees, 1 coord-locks, 0 worker-records\n",
			wantReason: "",
		},
		{
			name: "no-coord-locks-line-is-success",
			gcOutput: "fleet gc — mode=apply aggressive=false max-age=24h0m0s\n" +
				"summary: 0 sockets, 0 agents, 0 tmux (surface only by default), 0 worktrees, 0 coord-locks, 0 worker-records\n",
			wantReason: "",
		},
		{
			name: "surface-live-holder-is-refusal",
			gcOutput: "fleet gc — mode=apply aggressive=false max-age=24h0m0s\n" +
				"coord-locks  /x/.locks/coordinator.lock  verb=surface  reason=dead coord (agent record 129c9824 missing); but holder tmux session fleet-129c9824 still alive — refusing to unlink (would split-brain coord mutual-exclusion); investigate the missing record first\n" +
				"summary: 0 sockets, 0 agents, 0 tmux (surface only by default), 0 worktrees, 1 coord-locks, 0 worker-records\n",
			wantReason: "dead coord",
		},
		{
			name: "remove-failed-surface-is-refusal",
			gcOutput: "fleet gc — mode=apply aggressive=false max-age=24h0m0s\n" +
				"coord-locks  /x/.locks/coordinator.lock  verb=surface  reason=stale tmux (session gone); remove failed: permission denied\n",
			wantReason: "remove failed",
		},
		{
			name:       "would-remove-means-not-removed-is-refusal",
			gcOutput:   "coord-locks  /x/.locks/coordinator.lock  verb=would-remove  reason=stale tmux\n",
			wantReason: "stale tmux",
		},
		{
			name: "removed-one-surface-other-is-refusal",
			gcOutput: "coord-locks  /a/.locks/coordinator.lock  verb=removed  reason=stale\n" +
				"coord-locks  /b/.locks/coordinator.lock  verb=surface  reason=holder tmux still alive — refusing to unlink\n",
			wantReason: "holder tmux still alive",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := coordLockRefusal(tc.gcOutput)
			if tc.wantReason == "" {
				if got != "" {
					t.Errorf("coordLockRefusal = %q; want no refusal", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantReason) {
				t.Errorf("coordLockRefusal = %q; want substring %q", got, tc.wantReason)
			}
		})
	}
}

// TestKeyR_InFlightSpawn_RefusesArm is the codex iter-3 [P2] regression:
// pressing [r] while a coord spawn for the project is in flight
// (coordSpawnInFlight set, the dispatch's coordSpawnDoneMsg not yet
// handled) must REFUSE to arm reset. The just-dispatched coord's record
// isn't written yet, so the frozen snapshot would miss it; reaping then
// respawning would duplicate the spawning coord.
func TestKeyR_InFlightSpawn_RefusesArm(t *testing.T) {
	withFleetHome(t)
	reap := &stubResetReap{}
	reap.install(t)

	c1 := coordRecord("129c9824", "demo")
	m := projectRowResetModel(t, "demo", []*agent.Record{c1})
	m.coordOpInFlight = map[string]string{"demo": coordOpSpawn}

	updated, cmd := m.Update(keyMsg("r"))
	mm := updated.(Model)

	if mm.mode == modeConfirmReset {
		t.Errorf("[r] must NOT arm reset while a spawn is in flight; mode=%v", mm.mode)
	}
	if mm.mode != modeNav {
		t.Errorf("mode should stay modeNav; got %v", mm.mode)
	}
	if cmd != nil {
		t.Errorf("[r] in-flight refusal must not produce a cmd")
	}
	if mm.resetProjectCandidate != "" || mm.resetCoordIDs != nil {
		t.Errorf("[r] in-flight refusal must not freeze reset state; got %q %v",
			mm.resetProjectCandidate, mm.resetCoordIDs)
	}
	if mm.flash == nil || !mm.flash.isErr || !strings.Contains(mm.flash.text, "in flight") {
		t.Errorf("expected in-flight refusal flash; got %+v", mm.flash)
	}
}

// TestCoordLockStillPresent is the codex iter-3 [P2] backstop regression:
// after the reap+gc, a surviving coordinator.lock (gc couldn't classify
// it → no action line, exit 0) must fail the reset, NOT respawn over it.
func TestCoordLockStillPresent(t *testing.T) {
	pdir := withFleetHome(t)
	const project = "demo"

	// No lock present → success (nil).
	if err := coordLockStillPresent(project); err != nil {
		t.Errorf("absent lock should be success; got %v", err)
	}

	// Seed a coordinator.lock that survived the sweep.
	lockDir := filepath.Join(pdir, project, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	lockPath := filepath.Join(lockDir, "coordinator.lock")
	if err := os.WriteFile(lockPath, []byte("garbledbody\n"), 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	err := coordLockStillPresent(project)
	if err == nil {
		t.Fatal("surviving coordinator.lock after gc must fail the reset; got nil")
	}
	if !strings.Contains(err.Error(), "still present") {
		t.Errorf("error should explain the surviving lock; got %v", err)
	}

	// Remove it → success again.
	if rmErr := os.Remove(lockPath); rmErr != nil {
		t.Fatalf("rm lock: %v", rmErr)
	}
	if err := coordLockStillPresent(project); err != nil {
		t.Errorf("removed lock should be success; got %v", err)
	}
}

// ---------------------------------------------------------------------
// Architect-level integration test — the real integrated dashboard path
// ---------------------------------------------------------------------

// TestIntegration_StaleCoordLock_DeadEndRecovery_AndReset drives the
// REAL integrated path end to end against a seeded FLEET_HOME:
//
//   - A coordinator.lock body on disk names an ARCHIVED holder
//     (129c9824 has a record only in agents/archive/, none live).
//   - A live successor coord (68c6db50) holds task_id=coord-<project>
//     with a marker on disk and an alive session.
//
// Assertions:
//
//  1. [a] on the project row resolves the LIVE successor (not an
//     infinite "pending refresh") — this is the dead-end the operator
//     hit on 2026-05-28.
//  2. [r] reset drives confirm → reap → respawn end to end: the reap
//     archives ALL coord records and the gc-coord-locks shell-out
//     clears the stale lock, then a fresh coord is dispatched.
//
// The regression: on the parent commit, step 1 flashes "pending
// refresh" forever (Path 1's ID-only resolve refuses to scan), and
// there is no [r] key at all. After the fix both pass.
func TestIntegration_StaleCoordLock_DeadEndRecovery_AndReset(t *testing.T) {
	projectsRoot := withFleetHome(t)
	const project = "fengshen-site"
	seedProjectMeta(t, project, t.TempDir()) // [r] respawn binds via meta (PR3)
	const staleID = "129c9824"               // archived holder still in the lock body
	const liveID = "68c6db50"                // live successor coord

	// Seed the archived holder record so coordArchivedFn (production,
	// not stubbed here) sees agents/archive/129c9824.json.
	archDir := filepath.Join(filepath.Dir(projectsRoot), "agents", "archive")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archDir, staleID+".json"), []byte(`{"id":"`+staleID+`"}`), 0o644); err != nil {
		t.Fatalf("seed archived holder: %v", err)
	}

	// Seed the stale coordinator.lock body naming the archived holder.
	lockDir := filepath.Join(projectsRoot, project, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "coordinator.lock"), []byte(staleID+"\n"), 0o644); err != nil {
		t.Fatalf("seed lock body: %v", err)
	}

	// Live successor: alive session + matching marker so the marker scan
	// (findExistingCoordForProject) resolves it.
	(&stubSessionAlive{}).install(t)
	(&stubSessionProbe{}).install(t)
	(&stubCoordLeaseIdentity{markers: map[string]string{project: liveID}}).install(t)
	stubTUIResolveAttach(t, project, liveID)

	// Stub the reset reap so we don't fork real `fleet rm`/`fleet gc`,
	// but assert the exact shell-out intent (project + coord IDs).
	reap := &stubResetReap{}
	reap.install(t)
	spawn := &stubFleetCmd{
		stubbed: func(args []string) tea.Msg {
			out := "agent deadbeef spawned\n  task: coord-" + project + "\n  project: " + project + "\n  tmux: fleet-deadbeef\n"
			return coordSpawnDoneMsgFromArgs(args, out, nil)
		},
	}
	spawn.install(t)

	live := coordRecord(liveID, project)

	// Build the model the way the running TUI does: dashboard snapshot
	// with CoordID derived from the lock body (the stale holder), and
	// m.records carrying the live successor.
	newModel := func() Model {
		m := New("test")
		m.records = []*agent.Record{live}
		m.dashboard = &Snapshot{
			Projects: []*ProjectRow{{Name: project, CoordID: staleID}},
		}
		for i, r := range m.dashboardRows() {
			if r.kind == rowProject && r.project != nil && r.project.Name == project {
				m.dashCursor = i
				break
			}
		}
		return m
	}

	// --- Step 1: [a] resolves the live successor, not pending-refresh ---
	{
		m := newModel()
		updated, cmd := m.Update(keyMsg("a"))
		mm := updated.(Model)
		if mm.flash != nil && strings.Contains(mm.flash.text, "pending refresh") {
			t.Fatalf("REGRESSION: [a] looped pending-refresh on a stale lock; flash=%q", mm.flash.text)
		}
		if mm.pendingAttach != "fleet-"+liveID {
			t.Fatalf("[a] should attach the live successor %s; pendingAttach=%q flash=%+v",
				liveID, mm.pendingAttach, mm.flash)
		}
		if cmd == nil {
			t.Fatal("[a] successor-attach must return tea.Quit cmd; got nil")
		}
	}

	// --- Step 2: [r] reset confirm → reap → respawn end to end ---
	{
		m := newModel()
		// [r] → confirm.
		updated, _ := m.Update(keyMsg("r"))
		m = updated.(Model)
		if m.mode != modeConfirmReset {
			t.Fatalf("[r] should enter modeConfirmReset; got %v", m.mode)
		}
		// y → reap cmd.
		updated, cmd := m.Update(keyMsg("y"))
		m = updated.(Model)
		if cmd == nil {
			t.Fatal("y should produce the reap cmd")
		}
		done := cmd().(resetDoneMsg)
		if len(reap.calls) != 1 || reap.calls[0].project != project {
			t.Fatalf("reap should target project %s once; got %v", project, reap.calls)
		}
		// The live successor record is the only coord record loaded, so it
		// is the one reaped.
		if len(reap.calls[0].coordIDs) != 1 || reap.calls[0].coordIDs[0] != liveID {
			t.Errorf("reap should reap the loaded coord record %s; got %v", liveID, reap.calls[0].coordIDs)
		}
		// resetDoneMsg → respawn.
		updated, cmd = m.Update(done)
		m = updated.(Model)
		if cmd == nil {
			t.Fatal("resetDoneMsg(success) should spawn a fresh coord")
		}
		_ = cmd()
		var sawDispatch bool
		for _, call := range spawn.calls {
			if len(call) >= 2 && call[0] == "dispatch" && call[1] == "coord-"+project {
				sawDispatch = true
			}
		}
		if !sawDispatch {
			t.Errorf("reset should dispatch a fresh coord-%s; spawn calls=%v", project, spawn.calls)
		}
		if m.flash == nil || !strings.Contains(m.flash.text, "reaped") {
			t.Errorf("reset success flash missing reaped count; got %+v", m.flash)
		}
	}
}

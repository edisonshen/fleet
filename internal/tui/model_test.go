package tui

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
)

// fakeRecords builds N records spaced 1 minute apart, oldest first.
// The oldest is "a0", newest is "aN-1". sortRecords flips this so the
// newest spawned is at index 0.
func fakeRecords(n int) []*agent.Record {
	now := time.Now().UTC()
	out := make([]*agent.Record, n)
	for i := range n {
		out[i] = &agent.Record{
			SchemaVersion: 1,
			ID:            "a" + string(rune('0'+i)),
			Engine:        "claude-code",
			Role:          "executor",
			Mode:          "execute",
			Project:       "demo",
			TaskID:        "task-" + string(rune('0'+i)),
			SpawnedAt:     now.Add(time.Duration(i) * time.Minute),
		}
	}
	return out
}

func TestNew_Defaults(t *testing.T) {
	m := New("9.9.9")
	if m.version != "9.9.9" {
		t.Errorf("version: got %q, want 9.9.9", m.version)
	}
	if m.cursor != 0 {
		t.Errorf("cursor should start at 0, got %d", m.cursor)
	}
	if len(m.records) != 0 {
		t.Errorf("records should start empty, got %d", len(m.records))
	}
}

func TestUpdate_AgentsMsg_PopulatesAndSorts(t *testing.T) {
	m := New("test")
	updated, _ := m.Update(agentsMsg{records: fakeRecords(3)})
	got := updated.(Model)

	if len(got.records) != 3 {
		t.Fatalf("got %d records, want 3", len(got.records))
	}
	// Newest-first ordering: a2 (newest) should be at index 0.
	if got.records[0].ID != "a2" {
		t.Errorf("expected newest a2 first, got %s", got.records[0].ID)
	}
	if got.records[2].ID != "a0" {
		t.Errorf("expected oldest a0 last, got %s", got.records[2].ID)
	}
}

func TestUpdate_AgentsMsg_PropagatesError(t *testing.T) {
	m := New("test")
	wantErr := errors.New("boom")
	updated, _ := m.Update(agentsMsg{err: wantErr})
	got := updated.(Model)
	if got.err == nil || got.err.Error() != "boom" {
		t.Errorf("err: got %v, want boom", got.err)
	}
}

func TestUpdate_AgentsMsg_KeepsCursorInBounds(t *testing.T) {
	m := New("test")
	m.records = fakeRecords(5)
	m.cursor = 4 // pointing at last

	// Shrink to 2 records — cursor should drop to 1 (last valid index).
	updated, _ := m.Update(agentsMsg{records: fakeRecords(2)})
	got := updated.(Model)
	if got.cursor != 1 {
		t.Errorf("cursor: got %d, want 1 (clamped to last index)", got.cursor)
	}
}

func TestUpdate_AgentsMsg_EmptyDoesNotCrash(t *testing.T) {
	m := New("test")
	m.records = fakeRecords(3)
	m.cursor = 2

	updated, _ := m.Update(agentsMsg{records: nil})
	got := updated.(Model)
	if got.cursor != 0 {
		t.Errorf("empty list should clamp cursor to 0, got %d", got.cursor)
	}
	if len(got.records) != 0 {
		t.Errorf("records should be empty, got %d", len(got.records))
	}
}

func TestKey_Quit(t *testing.T) {
	m := New("test")
	for _, k := range []string{"q", "ctrl+c"} {
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		if k == "ctrl+c" {
			_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		}
		if cmd == nil {
			t.Errorf("%q should return tea.Quit, got nil cmd", k)
		}
	}
}

func TestKey_Navigation(t *testing.T) {
	m := New("test")
	m.records = fakeRecords(3)

	// j moves cursor down
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if updated.(Model).cursor != 1 {
		t.Errorf("j: cursor got %d, want 1", updated.(Model).cursor)
	}

	// k moves cursor back up
	m.cursor = 2
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if updated.(Model).cursor != 1 {
		t.Errorf("k: cursor got %d, want 1", updated.(Model).cursor)
	}

	// G jumps to bottom
	m.cursor = 0
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	if updated.(Model).cursor != 2 {
		t.Errorf("G: cursor got %d, want 2", updated.(Model).cursor)
	}

	// g jumps to top
	m.cursor = 2
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if updated.(Model).cursor != 0 {
		t.Errorf("g: cursor got %d, want 0", updated.(Model).cursor)
	}
}

func TestKey_NavigationClampsAtBounds(t *testing.T) {
	m := New("test")
	m.records = fakeRecords(3)

	// k at top stays at top
	m.cursor = 0
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if updated.(Model).cursor != 0 {
		t.Errorf("k at top: cursor got %d, want 0", updated.(Model).cursor)
	}

	// j at bottom stays at bottom
	m.cursor = 2
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if updated.(Model).cursor != 2 {
		t.Errorf("j at bottom: cursor got %d, want 2", updated.(Model).cursor)
	}
}

func TestUpdate_TickReturnsRefreshAndNextTick(t *testing.T) {
	m := New("test")
	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tickMsg should produce a command (load + next tick)")
	}
	// We can't easily inspect tea.Batch's contents, but a non-nil cmd
	// is the contract.
}

func TestUpdate_FsEventReturnsRefreshCmd(t *testing.T) {
	m := New("test")
	_, cmd := m.Update(fsEventMsg{})
	if cmd == nil {
		t.Fatal("fsEventMsg should produce a load command")
	}
}

func TestView_EmptyState(t *testing.T) {
	m := New("0.0.0")
	out := m.View()
	if !strings.Contains(out, "no agents") {
		t.Errorf("empty view should mention 'no agents', got:\n%s", out)
	}
	// Hint must point operator at the in-TUI dispatch shortcut, not
	// the shell command. With `[d]` already in the footer, this is
	// the discoverable path for someone seeing an empty TUI.
	if !strings.Contains(out, "[d]") {
		t.Errorf("empty view should hint at [d] dispatch shortcut, got:\n%s", out)
	}
	if !strings.Contains(out, "Fleet 0.0.0") {
		t.Errorf("view should include version in title, got:\n%s", out)
	}
}

func TestView_PickerRendersFilterAndCandidates(t *testing.T) {
	m := New("test")
	m.mode = modePickRepo
	m.repoCandidates = []repoCandidate{
		{Path: "/x/alpha", Display: "alpha"},
		{Path: "/x/beta", Display: "beta"},
	}
	m.pickerFilter = "al"
	out := m.View()
	if !strings.Contains(out, "pick repo: al") {
		t.Errorf("picker view missing filter input, got:\n%s", out)
	}
	if !strings.Contains(out, "alpha") {
		t.Errorf("picker view missing alpha row, got:\n%s", out)
	}
	if strings.Contains(out, "beta") {
		t.Errorf("filter 'al' should hide beta, got:\n%s", out)
	}
	if !strings.Contains(out, "[enter] pick") {
		t.Errorf("picker missing keybind hint, got:\n%s", out)
	}
}

func TestView_PickerEmptyShowsHint(t *testing.T) {
	m := New("test")
	m.mode = modePickRepo
	m.repoCandidates = nil
	out := m.View()
	if !strings.Contains(out, "no repos found") {
		t.Errorf("empty picker should show hint, got:\n%s", out)
	}
	if !strings.Contains(out, "FLEET_PROJECT_DIRS") {
		t.Errorf("empty picker should mention env var, got:\n%s", out)
	}
}

func TestView_PromptHeaderShowsPickedRepo(t *testing.T) {
	m := New("test")
	m.mode = modePromptDispatch
	m.pickedRepo = repoCandidate{Path: "/x/fleet", Display: "fleet"}
	out := m.View()
	if !strings.Contains(out, "dispatch task in fleet") {
		t.Errorf("prompt header should reference picked repo, got:\n%s", out)
	}
}

func TestView_ShowsErrorBanner(t *testing.T) {
	m := New("test")
	m.err = errors.New("disk full")
	out := m.View()
	if !strings.Contains(out, "disk full") {
		t.Errorf("view should surface error, got:\n%s", out)
	}
}

func TestView_RendersAgentList(t *testing.T) {
	m := New("test")
	m.records = sortRecords(fakeRecords(2))
	out := m.View()

	// v2 layout drops the AGENT/PROJECT/TASK/MODE/AGE/STATUS column
	// header row — the row is self-documenting via the leading
	// status glyph + cursor arrow + colored cells. Just assert the
	// data shows up.
	if !strings.Contains(out, "a0") || !strings.Contains(out, "a1") {
		t.Errorf("view should include agent IDs, got:\n%s", out)
	}
	// Project group header surfaces the count + active count alongside
	// the project label so the operator can scan groups at a glance.
	// Format: "demo (2 tasks, 2 active)".
	if !strings.Contains(out, "demo") || !strings.Contains(out, "(2 tasks, 2 active)") {
		t.Errorf("view should include project group header, got:\n%s", out)
	}
	// Smart footer summary line: "N project · M agents".
	if !strings.Contains(out, "2 agents") {
		t.Errorf("footer should report agent count, got:\n%s", out)
	}
	// Cursor row gets an inline action chip strip.
	if !strings.Contains(out, "[a]") || !strings.Contains(out, "attach") {
		t.Errorf("selected row should expose inline [a] attach chip, got:\n%s", out)
	}
}

func TestView_CoachHintShownInitiallyHiddenAfterKeypress(t *testing.T) {
	m := New("test")
	m.records = sortRecords(fakeRecords(2))

	// Fresh launch shows the coach hint so a first-time operator
	// understands "the selected row IS the interface" without docs.
	if !strings.Contains(m.View(), "actions appear on the selected row") {
		t.Errorf("coach hint should render before any keypress, got:\n%s", m.View())
	}

	// First nav keypress dismisses it — once you've moved, you've
	// demonstrated you know how.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	out := updated.(Model).View()
	if strings.Contains(out, "actions appear on the selected row") {
		t.Errorf("coach hint should be dismissed after [j], got:\n%s", out)
	}
}

func TestView_AlertBannerShowsBlockedAndHotContext(t *testing.T) {
	m := New("test")
	recs := fakeRecords(3)
	// One blocked, one with hot context. Banner should aggregate
	// both counts independently — a hot+blocked agent would bump
	// both bars, but here we keep them separate for clarity.
	recs[0].Blocked = true
	hot := 75.0
	recs[1].ContextPct = &hot
	m.records = sortRecords(recs)

	out := m.View()
	if !strings.Contains(out, "1 blocked") {
		t.Errorf("banner should show blocked count, got:\n%s", out)
	}
	if !strings.Contains(out, "1 hot context") {
		t.Errorf("banner should show hot-context count, got:\n%s", out)
	}
}

// TestView_AlertBannerSplitsIdleAndReview pins the dogfood-driven
// label split: NeedsInput=true (canonical "waiting") shows as "idle"
// and Mode=="review" shows as "in review" — distinct chips, not the
// pre-fix conflation that rendered both as "in review".
func TestView_AlertBannerSplitsIdleAndReview(t *testing.T) {
	m := New("test")
	recs := fakeRecords(2)
	recs[0].NeedsInput = true // → status "waiting" → "idle"
	recs[1].Mode = "review"   // → status "review" → "in review"
	m.records = sortRecords(recs)

	out := m.View()
	if !strings.Contains(out, "1 idle") {
		t.Errorf("banner should show idle count for NeedsInput agent, got:\n%s", out)
	}
	if !strings.Contains(out, "1 in review") {
		t.Errorf("banner should show in-review count for Mode==review agent, got:\n%s", out)
	}
	// And critically — they must be separate chips, not collapsed:
	// "1 idle" + "1 in review", not "2 in review".
	if strings.Contains(out, "2 in review") {
		t.Errorf("idle and review must not be conflated; got:\n%s", out)
	}
}

func TestView_NoAlertBannerWhenAllHealthy(t *testing.T) {
	m := New("test")
	m.records = sortRecords(fakeRecords(2)) // all default → live
	out := m.View()
	// v2 banner glyphs: ▌ blocked, △ hot context, ✗ dead. The cyan
	// ● review glyph is also a row-level glyph so we can't blanket-
	// check it — instead guard the descriptive words.
	for _, sym := range []string{"▌", "△", "✗", "blocked", "hot context", "dead", "in review", " idle"} {
		if strings.Contains(out, sym) {
			t.Errorf("clean dashboard should hide banner for %q, got:\n%s", sym, out)
		}
	}
}

func TestDeriveStatus_PrecedenceLadder(t *testing.T) {
	// alive is the cached liveness map keyed by agent ID.
	// alive[id]=true → probed alive; alive[id]=false → probed dead;
	// missing entry → conservatively treated as live (no probe yet).
	const id = "x"
	const sess = "fleet-x"
	aliveTrue := map[string]bool{id: true}
	aliveFalse := map[string]bool{id: false}
	manualType := "manual"
	autoYellow := "auto-yellow"
	autoRed := "auto-red"
	precompact := "precompact"

	cases := []struct {
		name  string
		rec   *agent.Record
		alive map[string]bool
		want  string
	}{
		{
			name: "dead session beats every other state",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
				Blocked:     true,
				NeedsInput:  true,
				HandoffType: &autoYellow,
			},
			alive: aliveFalse,
			want:  "dead",
		},
		{
			name: "auto-yellow handoff beats blocked + waiting",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
				Blocked:     true,
				NeedsInput:  true,
				HandoffType: &autoYellow,
			},
			alive: aliveTrue,
			want:  "auto-yellow",
		},
		{
			name: "auto-red surfaces as in-flight handoff",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
				HandoffType: &autoRed,
			},
			alive: aliveTrue,
			want:  "auto-red",
		},
		{
			name: "precompact surfaces as in-flight handoff",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
				HandoffType: &precompact,
			},
			alive: aliveTrue,
			want:  "precompact",
		},
		{
			name: "manual handoff_type is provenance, not status — falls through to waiting",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
				NeedsInput:  true,
				HandoffType: &manualType,
			},
			alive: aliveTrue,
			want:  "waiting",
		},
		{
			name: "manual handoff_type with no other flags falls through to live",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
				HandoffType: &manualType,
			},
			alive: aliveTrue,
			want:  "live",
		},
		{
			name: "blocked beats waiting",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
				Blocked:     true,
				NeedsInput:  true,
			},
			alive: aliveTrue,
			want:  "blocked",
		},
		{
			name: "waiting when only needs_input set",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
				NeedsInput:  true,
			},
			alive: aliveTrue,
			want:  "waiting",
		},
		{
			name:  "live by default",
			rec:   &agent.Record{ID: id, TmuxSession: sess},
			alive: aliveTrue,
			want:  "live",
		},
		{
			name: "empty TmuxSession skips dead-check (legacy / pre-spawn rec)",
			rec:  &agent.Record{ID: id, NeedsInput: true},
			want: "waiting",
		},
		{
			name: "missing alive entry conservatively reads as live (no probe yet)",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
			},
			alive: nil,
			want:  "live",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveStatus(tc.rec, tc.alive); got != tc.want {
				t.Errorf("deriveStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSortRecords_NewestFirst(t *testing.T) {
	records := fakeRecords(3) // a0 oldest, a2 newest
	sorted := sortRecords(records)
	if sorted[0].ID != "a2" || sorted[2].ID != "a0" {
		t.Errorf("sortRecords: order wrong, got %s,%s,%s",
			sorted[0].ID, sorted[1].ID, sorted[2].ID)
	}
	// Original slice should not be mutated.
	if records[0].ID != "a0" {
		t.Error("sortRecords mutated input slice")
	}
}

// TestLoadAgentsCmd_ProbeFailureLeavesCacheUnpoisoned regresses
// codex review iter-5 P2: a tmux probe that fails (binary missing,
// socket broken) MUST NOT write a "false" entry into the alive
// cache, since that would render a healthy agent as "dead" and
// could trick an operator into pressing [x] on a live session.
// Only definitive "session does not exist" results write false.
func TestLoadAgentsCmd_ProbeFailureLeavesCacheUnpoisoned(t *testing.T) {
	(&stubSessionProbe{
		// agent01: probe says definitively dead (writes false)
		// agent02: probe transport error (must NOT write false)
		// agent03: probe says alive (writes true)
		dead:        map[string]bool{"fleet-agent01": true},
		errSessions: map[string]bool{"fleet-agent02": true},
	}).install(t)

	// Seed three agent records on disk so loadAgentsCmd has something
	// to walk. Use the helper from keys_test.go shape (sampleAgent
	// builds a record with TmuxSession = "fleet-<id>").
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(tmp+"/agents", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, id := range []string{"agent01", "agent02", "agent03"} {
		rec := agent.New(id)
		rec.TmuxSession = "fleet-" + id
		rec.SpawnedAt = time.Now().UTC()
		if err := rec.Write(); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	msg := loadAgentsCmd()().(agentsMsg)
	if msg.err != nil {
		t.Fatalf("loadAgentsCmd: %v", msg.err)
	}
	if msg.alive["agent01"] != false {
		t.Errorf("agent01 (definitively dead) should be false, got %v", msg.alive["agent01"])
	}
	if msg.alive["agent03"] != true {
		t.Errorf("agent03 (alive) should be true, got %v", msg.alive["agent03"])
	}
	// The key invariant: probe failure leaves the entry MISSING, not
	// false. deriveStatus's nil-safe fallback then renders "live".
	if _, present := msg.alive["agent02"]; present {
		t.Errorf("agent02 probe failed — entry should be absent (not false), got alive=%v", msg.alive["agent02"])
	}
}

func TestHumanAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{2 * time.Hour, "2h"},
		{3 * 24 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := humanAge(c.d); got != c.want {
			t.Errorf("humanAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestPadRight(t *testing.T) {
	if got := padRight("ab", 5); got != "ab   " {
		t.Errorf("padRight ab,5 = %q", got)
	}
	if got := padRight("abcdef", 3); got != "abcdef" {
		t.Errorf("padRight should not truncate, got %q", got)
	}
}

// TestVisualRows pins down the wrap-aware row count: a soft-wrapped
// long line counts as multiple terminal rows, not one. This drives
// padToBottom so the bottom-pinned footer stays pinned on narrow
// terminals where the archive-confirm prompt or a blocked-reason
// quote wraps to extra rows (codex iter / P2).
func TestVisualRows(t *testing.T) {
	cases := []struct {
		name      string
		s         string
		termWidth int
		want      int
	}{
		{"empty", "", 80, 0},
		{"single line no newline", "hello", 80, 1},
		{"single line with newline", "hello\n", 80, 1},
		{"two lines", "foo\nbar", 80, 2},
		{"trailing newline doesn't double-count", "foo\nbar\n", 80, 2},
		{"empty middle line counts as 1", "foo\n\nbar", 80, 3},
		{"just a newline", "\n", 80, 1},
		// Soft-wrap: 100-char line on an 80-col terminal = 2 rows.
		{"wrap once", strings.Repeat("x", 100), 80, 2},
		// 80-char line fits exactly in 80 cells = 1 row, not 2.
		{"exact fit", strings.Repeat("x", 80), 80, 1},
		// 161 chars on 80 cols → 3 rows (80+80+1).
		{"wrap twice", strings.Repeat("x", 161), 80, 3},
		// termWidth ≤ 0 disables wrap math — falls back to logical lines.
		{"no width known", strings.Repeat("x", 200) + "\n" + strings.Repeat("y", 200), 0, 2},
	}
	for _, c := range cases {
		if got := visualRows(c.s, c.termWidth); got != c.want {
			t.Errorf("visualRows(%q, %d) = %d, want %d", c.name, c.termWidth, got, c.want)
		}
	}
}

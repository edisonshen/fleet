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
	if m.dashCursor != 0 {
		t.Errorf("dashCursor should start at 0, got %d", m.dashCursor)
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

// TestUpdate_AgentsMsg_KeepsCursorInBounds pins that the dashboard
// cursor doesn't dangle off the end when the row count shrinks. With
// no projects/workers seeded, dashboardRows() === records, so a cursor
// at the old last index would land out of bounds when records shrink.
func TestUpdate_AgentsMsg_KeepsCursorInBounds(t *testing.T) {
	m := New("test")
	m.records = fakeRecords(5)
	m.dashCursor = 4 // pointing at last row

	// Shrink to 2 records — cursor should reset to 0.
	updated, _ := m.Update(agentsMsg{records: fakeRecords(2)})
	got := updated.(Model)
	if got.dashCursor < 0 || got.dashCursor >= 2 {
		t.Errorf("dashCursor: got %d, want 0..1 after shrink", got.dashCursor)
	}
}

func TestUpdate_AgentsMsg_EmptyDoesNotCrash(t *testing.T) {
	m := New("test")
	m.records = fakeRecords(3)
	m.dashCursor = 2

	updated, _ := m.Update(agentsMsg{records: nil})
	got := updated.(Model)
	if got.dashCursor != 0 {
		t.Errorf("empty list should clamp dashCursor to 0, got %d", got.dashCursor)
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

// TestKey_Navigation walks the dashCursor across agent rows. With 3
// records and no projects/workers, dashboardRows() returns 3 agent
// rows, so j/k moves dashCursor 0→1→2.
func TestKey_Navigation(t *testing.T) {
	m := New("test")
	m.records = fakeRecords(3)

	// j moves cursor down
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if updated.(Model).dashCursor != 1 {
		t.Errorf("j: dashCursor got %d, want 1", updated.(Model).dashCursor)
	}

	// k moves cursor back up
	m.dashCursor = 2
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if updated.(Model).dashCursor != 1 {
		t.Errorf("k: dashCursor got %d, want 1", updated.(Model).dashCursor)
	}
}

// TestKey_NavigationWrapsAtBounds pins the issue #53 spec: "Wraps at
// boundaries". j at the bottom returns to the top; k at the top wraps
// to the bottom. This differs from v0.1's clamping behavior.
func TestKey_NavigationWrapsAtBounds(t *testing.T) {
	m := New("test")
	m.records = fakeRecords(3)

	// k at top wraps to bottom (index 2).
	m.dashCursor = 0
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if updated.(Model).dashCursor != 2 {
		t.Errorf("k at top: dashCursor got %d, want 2 (wraps)", updated.(Model).dashCursor)
	}

	// j at bottom wraps to top.
	m.dashCursor = 2
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if updated.(Model).dashCursor != 0 {
		t.Errorf("j at bottom: dashCursor got %d, want 0 (wraps)", updated.(Model).dashCursor)
	}
}

func TestUpdate_TickReturnsRefreshAndNextTick(t *testing.T) {
	m := New("test")
	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tickMsg should produce a command (load + next tick)")
	}
}

func TestUpdate_FsEventReturnsRefreshCmd(t *testing.T) {
	m := New("test")
	_, cmd := m.Update(fsEventMsg{})
	if cmd == nil {
		t.Fatal("fsEventMsg should produce a load command")
	}
}

// TestView_PickerRendersFilterAndCandidates pins the [d] picker
// renders the filter input + matching candidates. Picker mode
// replaces the dashboard footer.
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

func TestView_AlertBannerShowsBlockedAndHotContext(t *testing.T) {
	m := New("test")
	recs := fakeRecords(3)
	// One blocked, one with hot context.
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

// TestView_AlertBannerSplitsAskingIdleReview pins the three-way label
// split (asking / idle / review chips on the dashboard banner).
func TestView_AlertBannerSplitsAskingIdleReview(t *testing.T) {
	m := New("test")
	recs := fakeRecords(3)
	recs[0].NeedsInput = true // → idle
	recs[1].NeedsInput = true
	recs[1].HasPendingQuestion = true // → asking
	recs[2].Mode = "review"           // → in review
	m.records = sortRecords(recs)

	out := m.View()
	if !strings.Contains(out, "1 asking") {
		t.Errorf("banner should show asking count, got:\n%s", out)
	}
	if !strings.Contains(out, "1 idle") {
		t.Errorf("banner should show idle count, got:\n%s", out)
	}
	if !strings.Contains(out, "1 in review") {
		t.Errorf("banner should show in-review count, got:\n%s", out)
	}
	// And critically — none can be collapsed into the others.
	for _, bad := range []string{"2 asking", "2 idle", "2 in review", "3 idle", "3 asking"} {
		if strings.Contains(out, bad) {
			t.Errorf("chips must stay distinct; got %q in:\n%s", bad, out)
		}
	}
}

// TestDeriveStatus_PausedReviewerStaysReview pins precedence: review
// beats asking and idle so paused reviewers don't mislabel.
func TestDeriveStatus_PausedReviewerStaysReview(t *testing.T) {
	for _, hasQ := range []bool{false, true} {
		r := &agent.Record{
			ID: "r", TmuxSession: "s",
			Mode: "review", NeedsInput: true, HasPendingQuestion: hasQ,
		}
		alive := map[string]bool{"r": true}
		if got := deriveStatus(r, alive); got != "review" {
			t.Errorf("deriveStatus(review, NeedsInput, hasQ=%v) = %q, want %q",
				hasQ, got, "review")
		}
		if got := statusLabel(deriveStatus(r, alive)); got != "review" {
			t.Errorf("statusLabel for paused reviewer (hasQ=%v) = %q, want %q",
				hasQ, got, "review")
		}
	}
}

func TestView_NoAlertBannerWhenAllHealthy(t *testing.T) {
	m := New("test")
	m.records = sortRecords(fakeRecords(2)) // all default → live
	out := m.View()
	// Banner glyphs we MUST NOT see on a clean dashboard.
	for _, sym := range []string{"▌ ", "△", "1 blocked", "hot context", "1 dead", "1 in review", "1 idle"} {
		if strings.Contains(out, sym) {
			t.Errorf("clean dashboard should hide banner for %q, got:\n%s", sym, out)
		}
	}
}

func TestDeriveStatus_PrecedenceLadder(t *testing.T) {
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
			name: "manual handoff_type is provenance, not status — falls through to idle",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
				NeedsInput:  true,
				HandoffType: &manualType,
			},
			alive: aliveTrue,
			want:  "idle",
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
			name: "blocked beats asking/idle",
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
			name: "idle when only needs_input set (no question)",
			rec: &agent.Record{
				ID:          id,
				TmuxSession: sess,
				NeedsInput:  true,
			},
			alive: aliveTrue,
			want:  "idle",
		},
		{
			name: "asking when needs_input + has_pending_question",
			rec: &agent.Record{
				ID:                 id,
				TmuxSession:        sess,
				NeedsInput:         true,
				HasPendingQuestion: true,
			},
			alive: aliveTrue,
			want:  "asking",
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
			want: "idle",
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
// codex review iter-5 P2: a tmux probe that fails MUST NOT write a
// "false" entry into the alive cache.
func TestLoadAgentsCmd_ProbeFailureLeavesCacheUnpoisoned(t *testing.T) {
	(&stubSessionProbe{
		dead:        map[string]bool{"fleet-agent01": true},
		errSessions: map[string]bool{"fleet-agent02": true},
	}).install(t)

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

// TestVisualRows pins down the wrap-aware row count.
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
		{"wrap once", strings.Repeat("x", 100), 80, 2},
		{"exact fit", strings.Repeat("x", 80), 80, 1},
		{"wrap twice", strings.Repeat("x", 161), 80, 3},
		{"no width known", strings.Repeat("x", 200) + "\n" + strings.Repeat("y", 200), 0, 2},
	}
	for _, c := range cases {
		if got := visualRows(c.s, c.termWidth); got != c.want {
			t.Errorf("visualRows(%q, %d) = %d, want %d", c.name, c.termWidth, got, c.want)
		}
	}
}

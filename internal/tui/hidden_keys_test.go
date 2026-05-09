package tui

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
)

// stubToggleHidden installs an in-memory toggle that mirrors the disk
// behavior so [c] tests don't need a tmpdir + state package round-
// trip. Returns the live list pointer for assertions.
func stubToggleHidden() (*[]string, func()) {
	prev := toggleHiddenProjectFn
	prevRead := readHiddenProjectsFn
	prevWrite := writeHiddenProjectsFn
	var list []string
	toggleHiddenProjectFn = func(name string) (bool, error) {
		for i, n := range list {
			if n == name {
				list = append(list[:i], list[i+1:]...)
				return false, nil
			}
		}
		list = append(list, name)
		return true, nil
	}
	readHiddenProjectsFn = func() []string {
		out := make([]string, len(list))
		copy(out, list)
		sort.Strings(out)
		return out
	}
	writeHiddenProjectsFn = func(names []string) error {
		list = append([]string{}, names...)
		return nil
	}
	return &list, func() {
		toggleHiddenProjectFn = prev
		readHiddenProjectsFn = prevRead
		writeHiddenProjectsFn = prevWrite
	}
}

func TestKeyC_OnProjectRow_TogglesHide(t *testing.T) {
	list, cleanup := stubToggleHidden()
	defer cleanup()

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "alpha", Active: true},
			{Name: "beta", Active: true},
		},
	}
	// Cursor on alpha (row 0).
	m.dashCursor = 0
	mm, _ := m.Update(keyMsg("c"))
	got := mm.(Model)
	if !reflect.DeepEqual(*list, []string{"alpha"}) {
		t.Errorf("after [c] on alpha, hidden should be [alpha], got %v", *list)
	}
	if got.flash == nil || got.flash.isErr {
		t.Errorf("flash should be a success notice, got %+v", got.flash)
	}

	// Second toggle un-hides. To reach the row again we need
	// show-hidden mode on AND hidden-expanded so the cursor can land
	// on alpha. Easier: simulate by setting the model into that state
	// and re-finding the row.
	got.showHidden = true
	got.hiddenExpanded = true
	rows := got.dashboardRows()
	idx := -1
	for i, r := range rows {
		if r.kind == rowProject && r.project.Name == "alpha" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("hidden alpha should render under hidden separator: %+v", rows)
	}
	got.dashCursor = idx
	mm, _ = got.Update(keyMsg("c"))
	got2 := mm.(Model)
	if len(*list) != 0 {
		t.Errorf("second [c] should un-hide, got %v", *list)
	}
	_ = got2
}

func TestKeyC_OffRow_TogglesShowHidden(t *testing.T) {
	_, cleanup := stubToggleHidden()
	defer cleanup()

	// Stub two hidden projects so the separator has something to expand.
	readHiddenProjectsFn = func() []string { return []string{"alpha", "beta"} }

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "alpha"},
			{Name: "beta"},
			{Name: "gamma", Active: true},
		},
	}
	// First find a non-project row. Use the cursor explicitly off
	// project rows. We need an idle separator OR no row. Easiest:
	// move cursor to the IDLE separator (alpha+beta are hidden, gamma
	// active; so dashboardRows is just [gamma] until showHidden flips).
	rows := m.dashboardRows()
	// alpha+beta hidden → not in rows. gamma active. No idle separator,
	// no hidden separator yet. Let's set cursor off any project row by
	// putting it past the project (it'll be a worker/agent row OR
	// nothing). For this test we'll force cursor beyond the project to
	// land on no-row.
	if len(rows) > 0 {
		m.dashCursor = len(rows) // out of bounds → selectedRow returns nil
	}
	mm, _ := m.Update(keyMsg("c"))
	got := mm.(Model)
	if !got.showHidden {
		t.Errorf("[c] off-row should toggle show-hidden on, got false")
	}
	// And [c] off-row again toggles back.
	mm, _ = got.Update(keyMsg("c"))
	got2 := mm.(Model)
	if got2.showHidden {
		t.Errorf("second [c] off-row should toggle show-hidden off")
	}
}

func TestKeyC_OnSeparatorRow_TogglesShowHidden(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden() // none hidden

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "alpha", Active: true},
			{Name: "beta"}, // idle
		},
	}
	// Find idle separator.
	rows := m.dashboardRows()
	sepIdx := -1
	for i, r := range rows {
		if r.kind == rowSeparator && r.separator.kind == separatorIdle {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		t.Fatalf("expected idle separator, got rows: %+v", rows)
	}
	m.dashCursor = sepIdx
	mm, _ := m.Update(keyMsg("c"))
	got := mm.(Model)
	if !got.showHidden {
		t.Errorf("[c] on idle separator should toggle show-hidden")
	}
}

func TestKeyEnter_OnIdleSeparator_TogglesIdleExpansion(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "alpha", Active: true},
			{Name: "beta"},
			{Name: "gamma"},
		},
	}
	// Find idle separator.
	rows := m.dashboardRows()
	sepIdx := -1
	for i, r := range rows {
		if r.kind == rowSeparator && r.separator.kind == separatorIdle {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		t.Fatalf("expected idle separator, got rows: %+v", rows)
	}
	m.dashCursor = sepIdx
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := mm.(Model)
	if !got.idleExpanded {
		t.Errorf("[enter] on idle separator should expand the group")
	}
	// Second [enter] collapses.
	mm, _ = got.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got2 := mm.(Model)
	if got2.idleExpanded {
		t.Errorf("second [enter] on idle separator should collapse the group")
	}
	if !got2.idleCollapseExplicit {
		t.Errorf("explicit collapse flag should pin after [enter]")
	}
}

func TestKeyEnter_OnHiddenSeparator_TogglesHiddenExpansion(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden("hidden-one")

	m := New("test")
	m.showHidden = true
	m.hiddenExpanded = false
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{Name: "hidden-one"},
			{Name: "active-one", Active: true},
		},
	}
	rows := m.dashboardRows()
	sepIdx := -1
	for i, r := range rows {
		if r.kind == rowSeparator && r.separator.kind == separatorHidden {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		t.Fatalf("expected hidden separator, got rows: %+v", rows)
	}
	m.dashCursor = sepIdx
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := mm.(Model)
	if !got.hiddenExpanded {
		t.Errorf("[enter] on hidden separator should expand")
	}
}

func TestKeyC_OnTaskRow_FlashesDoesNotApply(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()

	m := New("test")
	m.expanded = map[string]bool{"alpha": true}
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{
			{
				Name:   "alpha",
				Active: true,
				Tasks:  []*taskRow{{Slug: "do-thing-1234", Status: "todo"}},
			},
		},
	}
	rows := m.dashboardRows()
	taskIdx := -1
	for i, r := range rows {
		if r.kind == rowTask && r.task != nil && r.task.Slug == "do-thing-1234" {
			taskIdx = i
			break
		}
	}
	if taskIdx < 0 {
		t.Fatalf("expected task row, got rows: %+v", rows)
	}
	m.dashCursor = taskIdx
	mm, _ := m.Update(keyMsg("c"))
	got := mm.(Model)
	if got.flash == nil || !got.flash.isErr {
		t.Errorf("[c] on task row should flash error, got %+v", got.flash)
	}
}

func TestKeyC_OnAgentRow_FlashesDoesNotApply(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()

	m := New("test")
	m.dashboard = &Snapshot{}
	m.records = []*agent.Record{
		{ID: "abc12345", Project: "p", TmuxSession: "fleet-abc12345"},
	}
	m.aliveByID = map[string]bool{"abc12345": true}

	rows := m.dashboardRows()
	agentIdx := -1
	for i, r := range rows {
		if r.kind == rowAgent {
			agentIdx = i
			break
		}
	}
	if agentIdx < 0 {
		t.Fatalf("expected agent row, got rows: %+v", rows)
	}
	m.dashCursor = agentIdx
	mm, _ := m.Update(keyMsg("c"))
	got := mm.(Model)
	if got.flash == nil || !got.flash.isErr {
		t.Errorf("[c] on agent row should flash error, got %+v", got.flash)
	}
}

func TestKeyC_OnProjectRow_DiskWriteFails(t *testing.T) {
	prev := toggleHiddenProjectFn
	prevRead := readHiddenProjectsFn
	defer func() {
		toggleHiddenProjectFn = prev
		readHiddenProjectsFn = prevRead
	}()
	readHiddenProjectsFn = func() []string { return nil }
	toggleHiddenProjectFn = func(name string) (bool, error) {
		return false, errors.New("disk full")
	}

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "alpha", Active: true}},
	}
	m.dashCursor = 0
	mm, _ := m.Update(keyMsg("c"))
	got := mm.(Model)
	if got.flash == nil || !got.flash.isErr {
		t.Errorf("disk write failure should surface an error flash, got %+v", got.flash)
	}
}

func TestKeyC_DoesNotCollideWithOtherKeys(t *testing.T) {
	// Sanity: pressing [d] / [h] / [x] / [a] / [n] does NOT trigger
	// the hide flow. We don't run them all here (other tests do); just
	// confirm that the dispatcher in handleActionKey routes [c]
	// distinctly from those keybinds.
	defer resetHiddenStubs()()
	stubHidden()

	m := New("test")
	m.dashboard = &Snapshot{
		Projects: []*ProjectRow{{Name: "alpha", Active: true}},
	}
	m.dashCursor = 0
	// [c] takes the toggleHide branch. After it, mode is unchanged
	// (not modeConfirmArchive, not modePickRepo, etc.).
	prev := toggleHiddenProjectFn
	defer func() { toggleHiddenProjectFn = prev }()
	called := false
	toggleHiddenProjectFn = func(name string) (bool, error) {
		called = true
		return true, nil
	}
	mm, _ := m.Update(keyMsg("c"))
	if !called {
		t.Errorf("[c] should call toggleHiddenProjectFn")
	}
	if mm.(Model).mode != modeNav {
		t.Errorf("[c] should keep modeNav, got %v", mm.(Model).mode)
	}
}


package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

func renderCapProjects(prefix string, n int, active bool) []*ProjectRow {
	projects := make([]*ProjectRow, n)
	for i := range projects {
		projects[i] = &ProjectRow{
			Name:     fmt.Sprintf("%s-%02d", prefix, i),
			RepoSlug: fmt.Sprintf("%s-%02d", prefix, i),
			Active:   active,
		}
	}
	return projects
}

func projectRowNames(rows []dashRow) []string {
	var names []string
	for _, row := range rows {
		if row.kind == rowProject && row.project != nil {
			names = append(names, row.project.Name)
		}
	}
	return names
}

func TestDashboardRows_CapsActiveProjects(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()
	m := New("test")
	m.dashboard = &Snapshot{Projects: renderCapProjects("active", 12, true)}

	rows := m.dashboardRows()
	names := projectRowNames(rows)
	if len(names) != projectRenderCap {
		t.Fatalf("project rows = %d, want %d: %v", len(names), projectRenderCap, names)
	}
	if rows[projectRenderCap].kind != rowSeparator ||
		rows[projectRenderCap].separator.kind != separatorMore ||
		rows[projectRenderCap].separator.count != 2 {
		t.Fatalf("more separator = %+v, want count 2", rows[projectRenderCap])
	}
	for _, row := range rows {
		if row.kind == rowSeparator && row.separator != nil &&
			row.separator.kind == separatorIdle {
			t.Fatal("all-active projects should not render an idle separator")
		}
	}
}

func TestDashboardRows_CapsIdleProjectsByRecency(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()
	now := time.Now()
	projects := renderCapProjects("active", 3, true)
	idle := renderCapProjects("idle", 12, false)
	for i, p := range idle {
		p.LastTick = now.Add(-time.Duration(20+i) * 24 * time.Hour)
	}
	projects = append(projects, idle...)
	m := New("test")
	m.idleExpanded = true
	m.dashboard = &Snapshot{Projects: projects}

	rows := m.dashboardRows()
	names := projectRowNames(rows)
	if len(names) != 10 {
		t.Fatalf("project rows = %d, want 10: %v", len(names), names)
	}
	for i := 0; i < 3; i++ {
		if names[i] != fmt.Sprintf("active-%02d", i) {
			t.Errorf("active row %d = %q, want active-%02d", i, names[i], i)
		}
	}
	for i := 0; i < 7; i++ {
		want := fmt.Sprintf("idle-%02d", i)
		if names[i+3] != want {
			t.Errorf("idle row %d = %q, want %q", i, names[i+3], want)
		}
	}
	sepIdle := -1
	sepMore := -1
	for i, row := range rows {
		if row.kind != rowSeparator || row.separator == nil {
			continue
		}
		switch row.separator.kind {
		case separatorIdle:
			sepIdle = i
			if row.separator.count != 12 {
				t.Errorf("idle separator count = %d, want 12", row.separator.count)
			}
		case separatorMore:
			sepMore = i
			if row.separator.count != 5 {
				t.Errorf("more separator count = %d, want 5", row.separator.count)
			}
		}
	}
	if sepIdle < 0 || sepMore < 0 || sepMore <= sepIdle {
		t.Fatalf("expected idle and more separators in order: %+v", rows)
	}
}

func TestDashboardRows_CapsAutoExpandedIdleProjects(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()
	now := time.Now()
	projects := renderCapProjects("idle", 12, false)
	for i, p := range projects {
		p.LastTick = now.Add(-time.Duration(20+i) * 24 * time.Hour)
	}
	m := New("test")
	m.dashboard = &Snapshot{Projects: projects}

	rows := m.dashboardRows()
	if len(projectRowNames(rows)) != projectRenderCap {
		t.Fatalf("project rows = %d, want %d", len(projectRowNames(rows)), projectRenderCap)
	}
	if rows[projectRenderCap].kind != rowSeparator ||
		rows[projectRenderCap].separator.kind != separatorMore ||
		rows[projectRenderCap].separator.count != 2 {
		t.Fatalf("more separator = %+v, want count 2", rows[projectRenderCap])
	}
	for _, row := range rows {
		if row.kind == rowSeparator && row.separator != nil &&
			row.separator.kind == separatorIdle {
			t.Fatal("auto-expanded idle projects should not render an idle separator")
		}
	}
}

func TestDashboardRows_ActiveBudgetCanHideAllIdleProjects(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()
	now := time.Now()
	projects := renderCapProjects("active", 10, true)
	idle := renderCapProjects("idle", 5, false)
	for i, p := range idle {
		p.LastTick = now.Add(-time.Duration(20+i) * 24 * time.Hour)
	}
	projects = append(projects, idle...)
	m := New("test")
	m.idleExpanded = true
	m.dashboard = &Snapshot{Projects: projects}

	rows := m.dashboardRows()
	names := projectRowNames(rows)
	if len(names) != 10 {
		t.Fatalf("project rows = %d, want 10: %v", len(names), names)
	}
	sepIdle := -1
	sepMore := -1
	for i, row := range rows {
		if row.kind != rowSeparator || row.separator == nil {
			continue
		}
		switch row.separator.kind {
		case separatorIdle:
			sepIdle = i
			if row.separator.count != 5 {
				t.Errorf("idle separator count = %d, want 5", row.separator.count)
			}
		case separatorMore:
			sepMore = i
			if row.separator.count != 5 {
				t.Errorf("more separator count = %d, want 5", row.separator.count)
			}
		}
	}
	if sepIdle < 0 || sepMore != sepIdle+1 {
		t.Fatalf("expected adjacent idle and more separators: %+v", rows)
	}
}

func TestDashboardRows_SearchBypassesProjectCaps(t *testing.T) {
	defer resetHiddenStubs()()
	stubHidden()
	projects := append(
		renderCapProjects("project-active", 12, true),
		renderCapProjects("project-idle", 12, false)...,
	)
	m := New("test")
	m.searchFilter = "project"
	m.dashboard = &Snapshot{Projects: projects}

	rows := m.dashboardRows()
	if got := len(projectRowNames(rows)); got != 24 {
		t.Fatalf("search project rows = %d, want 24", got)
	}
	for _, row := range rows {
		if row.kind == rowSeparator && row.separator != nil &&
			row.separator.kind == separatorMore {
			t.Fatal("search should bypass project caps")
		}
	}
}

func TestProjectLastActivity(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		tick    time.Time
		record  time.Time
		addedAt time.Time
		want    time.Time
	}{
		{
			name:    "last tick",
			tick:    now.Add(-time.Minute),
			record:  now.Add(-2 * time.Minute),
			addedAt: now.Add(-3 * time.Minute),
			want:    now.Add(-time.Minute),
		},
		{
			name:    "record activity",
			tick:    now.Add(-3 * time.Minute),
			record:  now.Add(-time.Minute),
			addedAt: now.Add(-2 * time.Minute),
			want:    now.Add(-time.Minute),
		},
		{
			name:    "added at",
			tick:    now.Add(-3 * time.Minute),
			record:  now.Add(-2 * time.Minute),
			addedAt: now.Add(-time.Minute),
			want:    now.Add(-time.Minute),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &ProjectRow{Name: "alpha", LastTick: tt.tick}
			records := []*agent.Record{
				{Project: "other", LastActivityTS: now},
				{Project: "alpha", LastActivityTS: tt.record},
			}
			if got := projectLastActivity(p, records, tt.addedAt); !got.Equal(tt.want) {
				t.Errorf("projectLastActivity = %v, want %v", got, tt.want)
			}
		})
	}
	if got := projectLastActivity(&ProjectRow{Name: "empty"}, nil, time.Time{}); !got.IsZero() {
		t.Errorf("all-zero projectLastActivity = %v, want zero", got)
	}
}

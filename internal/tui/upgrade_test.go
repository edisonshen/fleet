package tui

import (
	"strings"
	"testing"
)

// TestUpgradeBanner_RenderedWhenSet asserts the upgrade chip appears
// in the rendered dashboard banners when the model has captured a
// non-empty nudge. Format mirrors the spec in issue #26: `⬆ vX.Y.Z —
// brew upgrade fleet`.
func TestUpgradeBanner_RenderedWhenSet(t *testing.T) {
	m := New("0.1.2")
	m.width = 120
	m.height = 30
	m.upgradeBanner = "⬆ v0.1.3 — brew upgrade fleet"

	got := m.View()
	if !strings.Contains(got, "v0.1.3") {
		t.Errorf("View missing upgrade tag; output:\n%s", got)
	}
	if !strings.Contains(got, "brew upgrade fleet") {
		t.Errorf("View missing brew hint; output:\n%s", got)
	}
}

// TestUpgradeBanner_HiddenWhenEmpty asserts an empty banner field does
// NOT render any upgrade-related text.
func TestUpgradeBanner_HiddenWhenEmpty(t *testing.T) {
	m := New("0.1.2")
	m.width = 120
	m.height = 30
	m.upgradeBanner = ""

	got := m.View()
	if strings.Contains(got, "brew upgrade fleet") {
		t.Errorf("View should not render banner when empty; got:\n%s", got)
	}
	if strings.Contains(got, "⬆") {
		t.Errorf("View should not render upgrade glyph when empty; got:\n%s", got)
	}
}

// TestUpgradeMsg_StoresBanner asserts that an upgradeAvailableMsg
// flowing through Update populates m.upgradeBanner.
func TestUpgradeMsg_StoresBanner(t *testing.T) {
	m := New("0.1.2")
	updated, _ := m.Update(upgradeAvailableMsg{text: "⬆ v0.1.3 — brew upgrade fleet"})
	got := updated.(Model)
	if got.upgradeBanner != "⬆ v0.1.3 — brew upgrade fleet" {
		t.Errorf("upgradeBanner: got %q, want set", got.upgradeBanner)
	}
}

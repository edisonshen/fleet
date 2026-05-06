package tui

import (
	"strings"
	"testing"
)

// TestUpgradeBanner_RenderedWhenSet asserts the upgrade chip appears
// in the rendered top block when the model has captured a non-empty
// nudge. Format mirrors the spec in issue #26: `⬆ vX.Y.Z — brew upgrade fleet`.
func TestUpgradeBanner_RenderedWhenSet(t *testing.T) {
	m := New("0.1.2")
	m.upgradeBanner = "⬆ v0.1.3 — brew upgrade fleet"

	got := m.renderTop()
	if !strings.Contains(got, "v0.1.3") {
		t.Errorf("renderTop missing upgrade tag; output:\n%s", got)
	}
	if !strings.Contains(got, "brew upgrade fleet") {
		t.Errorf("renderTop missing brew hint; output:\n%s", got)
	}
}

// TestUpgradeBanner_HiddenWhenEmpty asserts an empty banner field does
// NOT render any upgrade-related text. This is the "fresh build, no
// network" path — must be silent.
func TestUpgradeBanner_HiddenWhenEmpty(t *testing.T) {
	m := New("0.1.2")
	m.upgradeBanner = ""

	got := m.renderTop()
	if strings.Contains(got, "brew upgrade fleet") {
		t.Errorf("renderTop should not render banner when empty; got:\n%s", got)
	}
	if strings.Contains(got, "⬆") {
		t.Errorf("renderTop should not render upgrade glyph when empty; got:\n%s", got)
	}
}

// TestUpgradeMsg_StoresBanner asserts that an upgradeAvailableMsg
// flowing through Update populates m.upgradeBanner so the next render
// surfaces it.
func TestUpgradeMsg_StoresBanner(t *testing.T) {
	m := New("0.1.2")
	updated, _ := m.Update(upgradeAvailableMsg{text: "⬆ v0.1.3 — brew upgrade fleet"})
	got := updated.(Model)
	if got.upgradeBanner != "⬆ v0.1.3 — brew upgrade fleet" {
		t.Errorf("upgradeBanner: got %q, want set", got.upgradeBanner)
	}
}

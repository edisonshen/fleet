// Tests for the dashboard title bar's version-injection behavior.
//
// The title bar reads m.version (set at New()) and produces:
//
//	Released build (Version="0.5.0") → "FLEET — v0.5.0 Ops Console"
//	Dev build      (Version="dev")   → "FLEET — Ops Console"
//	Empty version  (m.version="")    → "FLEET — Ops Console"
//
// "dev" is the default in cmd/fleet/main.go:20 when goreleaser hasn't
// injected -X main.Version=<tag> at link time, so on `go run` /
// `go build` (no ldflags) the title stays clean rather than rendering
// a meaningless "vdev" chip. Released artifacts get the actual tag
// injected and the suffix carries it.
package tui

import (
	"strings"
	"testing"
)

// TestDashboardTitle_InjectsReleasedVersion pins the released-build path:
// when New is called with a real version string ("0.99.0-test"), the
// rendered title must contain "FLEET" plus the v-prefixed version inside
// the "Ops Console" suffix. Substring assertions tolerate lipgloss color
// escapes around each token (the rendered output may carry ANSI
// sequences depending on the test runner's TTY profile).
func TestDashboardTitle_InjectsReleasedVersion(t *testing.T) {
	const synthetic = "0.99.0-test"
	m := New(synthetic)
	m.width = 130
	m.height = 30

	out := renderDashboardHeader(m, 120)
	if !strings.Contains(out, "FLEET") {
		t.Errorf("title should always carry the FLEET label, got:\n%s", out)
	}
	if !strings.Contains(out, "v"+synthetic) {
		t.Errorf("released-build title should carry v%s, got:\n%s", synthetic, out)
	}
	if !strings.Contains(out, "Ops Console") {
		t.Errorf("title should always carry the Ops Console suffix, got:\n%s", out)
	}
}

// TestDashboardTitle_DevVersionRendersBareSuffix pins the dev-build path:
// when New is called with the default "dev" sentinel (cmd/fleet/main.go:20
// pre-goreleaser), the title omits the v-prefix entirely so the operator
// doesn't see "vdev" — that string is meaningless to humans and adds
// noise to a polished title bar.
func TestDashboardTitle_DevVersionRendersBareSuffix(t *testing.T) {
	m := New("dev")
	m.width = 130
	m.height = 30

	out := renderDashboardHeader(m, 120)
	if !strings.Contains(out, "FLEET") {
		t.Errorf("title should always carry the FLEET label, got:\n%s", out)
	}
	if !strings.Contains(out, "Ops Console") {
		t.Errorf("title should always carry the Ops Console suffix, got:\n%s", out)
	}
	// "vdev " (with a trailing space) is the failure mode we're guarding
	// against — the literal "v" + "dev" + " Ops Console" splice that the
	// pre-polish "v0.2 Ops Console" hard-coded title would have produced
	// after a naive `m.version` substitution. Pin it absent so a future
	// refactor that drops the dev-fallback can't regress.
	if strings.Contains(out, "vdev") {
		t.Errorf("dev-build title must NOT carry vdev, got:\n%s", out)
	}
}

// TestDashboardTitle_EmptyVersionRendersBareSuffix is the pathological
// case: m.version == "". This shouldn't happen in normal flows (cobra
// passes a default) but the renderer must still produce a sane title
// rather than "v Ops Console" (an extra space + bare v).
func TestDashboardTitle_EmptyVersionRendersBareSuffix(t *testing.T) {
	m := New("")
	m.width = 130
	m.height = 30

	out := renderDashboardHeader(m, 120)
	if !strings.Contains(out, "FLEET") {
		t.Errorf("title should always carry the FLEET label, got:\n%s", out)
	}
	if !strings.Contains(out, "Ops Console") {
		t.Errorf("title should always carry the Ops Console suffix, got:\n%s", out)
	}
	// Guard against the "v Ops Console" naive-splice failure — title
	// must NOT have a stray "v " prefix on the suffix.
	if strings.Contains(out, "v Ops Console") {
		t.Errorf("empty-version title must NOT splice a stray v prefix, got:\n%s", out)
	}
}

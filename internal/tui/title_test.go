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

	"github.com/charmbracelet/lipgloss"
)

// withTrueColor pins the lipgloss default renderer to truecolor for the
// duration of the test so #E10600 lands in the rendered string as the
// explicit "38;2;225;6;0" SGR sequence the wordmark tests substring on.
// Without this the non-TTY default profile (Ascii) strips all color.
// Caller is responsible for restoring the prior profile via t.Cleanup.
func withTrueColor(t *testing.T) {
	t.Helper()
	r := lipgloss.DefaultRenderer()
	prev := r.ColorProfile()
	r.SetColorProfile(0) // termenv.TrueColor
	t.Cleanup(func() { r.SetColorProfile(prev) })
}

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

// TestTitle_WordmarkRendersFLXXT pins the F1-style brand mark: the
// dashboard's top row carries the literal 5-char "FLΞΞT" (F + L + two
// U+039E Greek capital Xi + T). Substring assertion tolerates lipgloss
// ANSI escapes between glyphs (each glyph is rendered through a
// per-segment style), so we walk the rendered string and confirm the
// glyph runes appear in order with only ANSI/whitespace between them.
//
// "FLEET" must NOT survive in the rendered title — the old plain-text
// label would be a regression marker.
func TestTitle_WordmarkRendersFLXXT(t *testing.T) {
	withTrueColor(t)
	m := New("0.6.0")
	m.width = 130
	m.height = 30

	out := renderDashboardHeader(m, 120)

	// The two Xis are U+039E (Ξ). Each segment may carry its own ANSI
	// envelope, so we walk runes and confirm F, L, Ξ, Ξ, T appear in
	// order across the whole rendered top row.
	want := []rune{'F', 'L', 'Ξ', 'Ξ', 'T'}
	idx := 0
	for _, r := range out {
		if idx < len(want) && r == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Errorf("rendered title should contain FLΞΞT runes in order; matched %d/%d, got:\n%s",
			idx, len(want), out)
	}

	// Plain "FLEET" must be gone — its presence indicates the wordmark
	// swap regressed. Note: "FL" + "T" still appear inside FLΞΞT, so
	// asserting the literal 5-char "FLEET" substring is the correct
	// boundary to guard.
	if strings.Contains(out, "FLEET") {
		t.Errorf("rendered title must not carry the old FLEET label, got:\n%s", out)
	}
}

// TestTitle_XisRenderedRed pins the F1-red treatment on both Xis.
// Under truecolor profile (forced by withTrueColor), lipgloss emits
// the explicit "38;2;225;6;0" SGR fragment for foreground #E10600.
// The Xis MUST carry that fragment — it's the load-bearing signal
// that the wordmark uses the F1 brand red and not some other color
// inherited from the surrounding header style.
//
// We assert the SGR fragment appears at least twice so a partial fix
// (one Xi colored, one not) fails. The bold+truecolor SGR shape is
// "1;38;2;225;6;0" but we substring the foreground triplet only —
// boldness lives at the same SGR layer and would force a more brittle
// match if we asserted the full prefix.
func TestTitle_XisRenderedRed(t *testing.T) {
	withTrueColor(t)
	m := New("0.6.0")
	m.width = 130
	m.height = 30

	out := renderDashboardHeader(m, 120)

	// Truecolor SGR for #E10600 = ESC[38;2;225;6;0m. The "38;2;225;6;0"
	// fragment is what we substring on — independent of bold/reset
	// neighbors at the SGR boundary.
	const redSGR = "38;2;225;6;0"
	count := strings.Count(out, redSGR)
	if count < 2 {
		t.Errorf("title should carry F1-red (%s) on both Xis; found %d occurrences, got:\n%q",
			redSGR, count, out)
	}
}

// Variant A "Ops Console" palette — matches the HTML mockup at
// ~/.gstack/projects/edisonshen-fleet/designs/coord-dashboard-20260506/
// and the ANSI shell mockup tui-mockup-A.sh. Colors are 256-color
// indices so they survive arbitrary terminal palettes (truecolor not
// required).
//
// Mapping to the bash mockup's R/A/G/B/P/D/W/T/RB/GB/BX:
//
//	R   203  red    #f85149   blocked / failed / attention badge
//	A   172  amber  #d29922   in-progress / running / ci
//	G   78   green  #3fb950   active / done / ok
//	B   75   blue   #58a6ff   in-review
//	P   141  purple #bc8cff   worker IDs
//	D   243  dim    #6e7681   secondary text
//	W   255  bright #f0f6fc   primary text bold
//	T   250  text   #c9d1d9   primary text
//	RB  203  bold red          attention chip + ▌ accent border
//	GB  78   bold green         coord active dot
//	BX  240  border line
//
// The "selected row" / cursor highlight from the v0.1 layout is reused
// for the agents-view fallback only — Variant A's dashboard uses a row-
// level red ▌ left-border to mark attention rows; cursor selection is
// expressed through bold + bracket chips on the focused row.
package tui

import "github.com/charmbracelet/lipgloss"

// Variant A palette (read-only constants; lipgloss styles below).
const (
	colorRed    = "203"
	colorAmber  = "172"
	colorGreen  = "78"
	colorBlue   = "75"
	colorPurple = "141"
	colorDim    = "243"
	colorBright = "255"
	colorText   = "250"
	colorBox    = "240"
)

var (
	// Header strip styles.
	headerLabelStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorBright))
	headerTextStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(colorText))
	headerSepStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(colorBox))
	headerAttnStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorRed))
	headerCIStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber))
	headerSubtleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorDim))

	// Column heading "PROJECTS · 3 ACTIVE" / "WORKERS · 5 ACTIVE".
	columnHeadingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorDim))

	// Project row styles.
	projectNameStyle         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorBright))
	projectRepoStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color(colorDim))
	projectCountTodoStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(colorDim))
	projectCountInProgStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber))
	projectCountReviewStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(colorBlue))
	projectCountBlockedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorRed))
	projectCountDoneStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(colorGreen))

	// Coord active/idle indicator.
	coordActiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorGreen))
	coordIdleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(colorDim))

	// Attention badge "● N attn" + ▌ left-border.
	attentionChipStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorRed))
	attentionBorderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorRed))

	// Worker row styles.
	workerIDStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color(colorPurple))
	workerSlugStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color(colorDim))
	workerDotRedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color(colorRed))
	workerDotAmberStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber))
	workerDotGreenStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(colorGreen))
	workerDotBlueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(colorBlue))
	workerStateRedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(colorRed))
	workerStateAmberStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAmber))
	workerStateGreenStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorGreen))
	workerStateBlueStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(colorBlue))

	// Footer keybind chips.
	footerKeyStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colorBright))
	footerLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorDim))

	// Box / divider / border characters.
	boxBorderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colorBox))
)

// workerDotStyle picks the leading-dot style for a worker color code.
// Falls back to dim for unknown values so a future state still renders.
func workerDotStyle(color string) lipgloss.Style {
	switch color {
	case "red":
		return workerDotRedStyle
	case "amber":
		return workerDotAmberStyle
	case "green":
		return workerDotGreenStyle
	case "blue":
		return workerDotBlueStyle
	}
	return workerStateAmberStyle
}

// workerStateStyle picks the trailing "2m rv" style with the same color
// family. Symmetric to workerDotStyle so the dot + label read as a pair.
func workerStateStyle(color string) lipgloss.Style {
	switch color {
	case "red":
		return workerStateRedStyle
	case "amber":
		return workerStateAmberStyle
	case "green":
		return workerStateGreenStyle
	case "blue":
		return workerStateBlueStyle
	}
	return workerStateAmberStyle
}

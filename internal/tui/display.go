// Display-only transforms for the dashboard. Pure functions — no I/O,
// no model state. Kept separate from rows.go (which folds the row list)
// and dashboard_view.go (which lays out cells) so the cosmetic mapping
// from on-disk identity to header text has one obvious home.
package tui

import "strings"

// projectDisplayName converts a cwd-encoded project name into the form
// the operator wants to read in the LEFT column header.
//
// Encoding background: project names are derived from the worker's cwd
// by replacing path separators with hyphens (e.g. cwd `projects/fleet`
// becomes the project tag `projects-fleet`). The encoded form is the
// authoritative identity used for file paths under
// `~/.fleet/projects/<name>/`, agent record fields, lock body content,
// search/filter matching, and tmux session names — none of which this
// helper touches.
//
// For the dashboard header alone (issue #66), we restore readability by
// flipping the FIRST hyphen back to a slash:
//
//	projects-fleet  → projects/fleet
//	projects-rainier → projects/rainier
//	work-my-app     → work/my-app  (later hyphens preserved)
//	single          → single        (no hyphen, passthrough)
//	""              → ""           (empty passthrough)
//
// We deliberately rewrite ONLY the first hyphen: multi-hyphen names are
// ambiguous (was the original `work-my/app` or `work/my-app`?) and
// guessing harder than the operator-visible win is worth.
func projectDisplayName(encoded string) string {
	i := strings.IndexByte(encoded, '-')
	if i < 0 {
		return encoded
	}
	return encoded[:i] + "/" + encoded[i+1:]
}

// Hidden-projects list — operator's persistent view filter for the TUI
// PROJECTS column (issue #98). The list is per-machine; entries name a
// project (~/.fleet/projects/<name>/) that the operator has explicitly
// hidden via [c] in the dashboard.
//
// Hide is a VIEW filter — it never deletes the project tree, agent
// records, or workers. The TUI's unifiedProjects() strips hidden names
// uniformly from both v0.2 dirs AND synthetic-from-agent rows so a
// hidden project disappears from the LEFT column regardless of where
// its row originated. Show-hidden mode (toggled via [c] off-row) flips
// the filter so hidden rows render dim under their own group.
//
// Hide is HARD — fresh activity on a hidden project does NOT auto-
// unhide it. The footer chip surfaces "M with activity" to nudge the
// operator without overriding intent.
//
// Storage: ~/.fleet/hidden-projects.json — a JSON array of project
// names. Atomic publish (WriteAtomic: tmp + fsync + rename) so a
// concurrent reader never sees a torn file.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// HiddenProjectsPath returns ~/.fleet/hidden-projects.json.
func HiddenProjectsPath() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "hidden-projects.json"), nil
}

// ReadHiddenProjects returns the current hidden-list as a string slice.
// Missing file returns an empty slice + nil error — the absence of a
// list is a valid empty state, not a failure.
//
// Malformed JSON is treated as empty + nil error so a hand-edit gone
// wrong (or a partial write the atomic-publish guards against) doesn't
// brick the TUI's project view. Operators see "no projects hidden" and
// can re-hide the rows they meant to.
func ReadHiddenProjects() ([]string, error) {
	path, err := HiddenProjectsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read hidden-projects: %w", err)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		// Tolerate malformed: best-effort, never block the dashboard
		// from rendering. The next WriteHiddenProjects re-publishes
		// canonical content.
		return nil, nil
	}
	// Defensive: drop empty entries (a hand-edit could have inserted
	// "") so callers don't have to filter. Sort for stable ordering on
	// disk and predictable test assertions.
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return dedupe(out), nil
}

// WriteHiddenProjects atomically publishes names as the new hidden
// list. Empty / nil slice writes "[]" — a present file with an empty
// array is the canonical "nothing hidden" state and is distinguishable
// from "no file yet" only via stat (which no caller needs).
//
// Sorts + dedupes before write so the on-disk shape is stable —
// repeated toggle round-trips don't churn the byte content.
func WriteHiddenProjects(names []string) error {
	path, err := HiddenProjectsPath()
	if err != nil {
		return err
	}
	// Make sure the parent dir exists. State.Bootstrap creates ~/.fleet/
	// but tests stubbing FLEET_HOME to a tmpdir may not have run it.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write hidden-projects: mkdir parent: %w", err)
	}
	clean := dedupe(append([]string{}, names...))
	sort.Strings(clean)
	// Filter empty entries defensively — a caller mishap shouldn't
	// poison the on-disk shape.
	out := make([]string, 0, len(clean))
	for _, n := range clean {
		if n != "" {
			out = append(out, n)
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("write hidden-projects: marshal: %w", err)
	}
	// Trailing newline — POSIX-friendly, matches the rest of fleet's
	// JSON files.
	data = append(data, '\n')
	if err := WriteAtomic(path, data); err != nil {
		return fmt.Errorf("write hidden-projects: %w", err)
	}
	return nil
}

// ToggleHiddenProject inverts name's membership in the list and
// publishes the result atomically. Returns (newHidden, err) where
// newHidden is true when the project ENDS UP hidden after the toggle
// (caller can use this to render the right confirmation flash).
//
// Idempotent against a missing file — read treats ENOENT as empty,
// and the first toggle creates the file with [name] inside.
func ToggleHiddenProject(name string) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("ToggleHiddenProject: empty name")
	}
	current, err := ReadHiddenProjects()
	if err != nil {
		return false, err
	}
	// O(N) lookup — N is the operator's hidden-project count, in
	// practice single digits to low double digits. Map allocation
	// would cost more than the scan.
	idx := -1
	for i, n := range current {
		if n == name {
			idx = i
			break
		}
	}
	var next []string
	var nowHidden bool
	if idx >= 0 {
		// Remove. append-skip pattern preserves order without an
		// in-place splice; sort happens inside WriteHiddenProjects.
		next = make([]string, 0, len(current)-1)
		next = append(next, current[:idx]...)
		next = append(next, current[idx+1:]...)
		nowHidden = false
	} else {
		next = append(append([]string{}, current...), name)
		nowHidden = true
	}
	if err := WriteHiddenProjects(next); err != nil {
		return false, err
	}
	return nowHidden, nil
}

// IsHidden returns true when name appears in the current hidden list.
// Convenience wrapper around ReadHiddenProjects for callers that only
// need a boolean answer (e.g. row-level filter checks).
//
// Returns false on read errors — an unreadable list never blocks
// rendering. Callers that need to surface the error should call
// ReadHiddenProjects directly.
func IsHidden(name string) bool {
	if name == "" {
		return false
	}
	hidden, err := ReadHiddenProjects()
	if err != nil {
		return false
	}
	for _, n := range hidden {
		if n == name {
			return true
		}
	}
	return false
}

// dedupe returns names with adjacent duplicates removed. Caller sorts
// first. Stable: preserves first-occurrence order before sort, which
// since we sort before calling means lexicographic dedupe.
func dedupe(names []string) []string {
	if len(names) <= 1 {
		return names
	}
	out := names[:1]
	for i := 1; i < len(names); i++ {
		if names[i] != names[i-1] {
			out = append(out, names[i])
		}
	}
	return out
}

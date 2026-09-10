package handoff

// tracking.go — mirror the handoff's Status rows into tasks.md.
//
// The handoff doc is one file in ~/.fleet/handoffs/ that only the direct
// successor reads. tasks.md is the task registry every coord, worker and
// `fleet tasks show` reads — so at handoff time each tracked task's Notes
// gets one line recording where the outgoing coord left it:
//
//	### Notes
//	Handoff 2026-09-10T00:16:00Z coord abcd1234 (#3): in-progress — PR #137 open, ci PENDING — next: shepherd PR #137
//
// That makes the task's own history show every coord boundary it crossed
// and the state at each, without opening the handoff chain. Terminal rows
// (done/abandoned) are skipped — nothing left to track.
//
// Rows are a snapshot taken when the doc was built; the write happens
// later (enrichment shells out, the auto path waits on the publish
// fence). A task the tick moved meanwhile — status, parked, pr_url — is
// SKIPPED rather than annotated with a stale line, and Updated is never
// moved backwards.
//
// Best-effort like the rest of enrichment: a missing tasks.md, a lock
// timeout, or a parse error returns an error the caller LOGS and ignores;
// the handoff doc is already on disk and must not be failed by a tracking
// side effect. Writes go through tasks.Write (atomic) under the project
// state lock, the same path `fleet tasks note` uses.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
)

// trackingLockTimeout bounds the tasks.md lock acquire. A worker mid-note
// holds it for milliseconds; anything longer and the handoff moves on.
const trackingLockTimeout = 2 * time.Second

// TrackingLine renders the Notes line for one row.
func TrackingLine(r StatusRow, agentID string, number int, ts time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Handoff %s coord %s (#%d): %s", ts.UTC().Format(time.RFC3339), agentID, number, r.Status)
	if r.PRState != "" {
		b.WriteString(" — ")
		b.WriteString(r.PRState)
	} else {
		b.WriteString(" — no PR")
	}
	if r.NextJob != "" {
		b.WriteString(" — next: ")
		b.WriteString(r.NextJob)
	}
	return oneLine(b.String())
}

// RecordTaskTracking appends a TrackingLine to the Notes of every
// non-terminal row in tasks.md under project. Returns the number of tasks
// updated. Rows whose slug is no longer in tasks.md, or whose task no
// longer matches the row (see rowStale), are skipped.
func RecordTaskTracking(project, agentID string, number int, ts time.Time, rows []StatusRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return 0, err
	}
	release, err := state.LockProjectStateTimeout(project, trackingLockTimeout)
	if err != nil {
		return 0, fmt.Errorf("lock tasks.md: %w", err)
	}
	defer release()

	path := filepath.Join(pdir, "tasks.md")
	f, err := tasks.Read(path)
	if err != nil {
		return 0, fmt.Errorf("read tasks.md: %w", err)
	}
	n := 0
	for _, r := range rows {
		if r.Terminal {
			continue
		}
		t, gerr := f.Get(r.Slug)
		if gerr != nil || rowStale(r, t) {
			continue
		}
		line := TrackingLine(r, agentID, number, ts)
		if t.Notes == "" {
			t.Notes = line
		} else {
			t.Notes = t.Notes + "\n\n" + line
		}
		if ts.After(t.Updated) {
			t.Updated = ts.UTC()
		}
		n++
	}
	if n == 0 {
		return 0, nil
	}
	if err := tasks.Write(path, f); err != nil {
		return 0, fmt.Errorf("write tasks.md: %w", err)
	}
	return n, nil
}

// rowStale reports whether the task on disk has moved since the row was
// collected: a different status/parked label, a terminal status, or a
// changed tasks.md pr_url. The row's PRURL may have come from a PR
// watch when tasks.md had none, so an empty on-disk pr_url is not a
// mismatch by itself.
func rowStale(r StatusRow, t *tasks.Task) bool {
	if t.Status == tasks.StatusDone || t.Status == tasks.StatusAbandoned {
		return true
	}
	if statusLabel(t) != r.Status {
		return true
	}
	return t.PRURL != "" && t.PRURL != r.PRURL
}

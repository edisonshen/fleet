package handoff

// status.go — the `## Status` block: the successor's at-a-glance brief.
//
// Every other narrative section answers ONE question (what shipped, why,
// what's blocked). Status answers the question a replacement coord asks
// first — "where does each task stand and what do I do next?" — in one
// compact line per task:
//
//	- <slug> — <status>[ P<n>][ phase=<phase>] — <PR state> — next: <job>
//
// followed by a single `Next job:` line. Every input is durable, local
// state (no `gh`, no tmux): tasks.md for status/priority/pr_url/deps,
// coord-state.json for this coord's session slugs, pr-watches.json for
// the PR watcher's last reduced event + snapshot, and the doc's own
// Active Subagents rows for the worker phase.
//
// Which tasks appear: every task the project has IN FLIGHT (in-progress,
// in-review, blocked, or parked) regardless of which coord dispatched it —
// the successor inherits the whole project — plus THIS coord's session
// slugs (session_tasks / session_next_steps) in any non-abandoned status,
// so ready/todo work the coord queued and done work it landed are visible
// too. Abandoned tasks and other coords' finished backlog are omitted.
//
// BuildStatus is best-effort like the other collectors: missing /
// malformed inputs degrade to fewer rows or a placeholder body, never an
// error.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/edisonshen/fleet/internal/tasks"
)

// statusRowsMax caps the per-task rows so a project with a huge in-flight
// set doesn't bloat the block; the overflow renders as `- … and N more`.
const statusRowsMax = 30

// StatusNonePlaceholder is the Status body when the collector found no
// tracked task (fresh project, or tasks.md unreadable).
const StatusNonePlaceholder = "_(no tasks tracked)_"

// StatusRow is one tracked task in the `## Status` block. Exported so the
// tasks.md tracking writer (RecordTaskTracking) can append the same facts
// to each task's Notes without re-deriving them.
type StatusRow struct {
	Slug     string
	Status   string // tasks.md status enum
	Priority string // P0..P3 or ""
	Phase    string // worker phase from Active Subagents; "" when not in flight
	PRURL    string
	PRState  string // compact PR state, e.g. "PR #137 open, ci SUCCESS, review APPROVED, ready"; "" when no PR
	NextJob  string
	Terminal bool // done/abandoned — tracked for the record, not actionable
}

// Line renders the row exactly as it appears in the handoff doc body.
func (r StatusRow) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s — %s", r.Slug, r.Status)
	if r.Priority != "" {
		b.WriteString(" ")
		b.WriteString(r.Priority)
	}
	if r.Phase != "" {
		fmt.Fprintf(&b, " phase=%s", r.Phase)
	}
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

// prWatch is the subset of one pr-watches.json entry the Status block
// reads. Keys mirror skills/coordinator/pr_watch.py:_new_watch +
// reconcile_watches' last_snapshot write; unknown keys are ignored.
type prWatch struct {
	PRNumber     int      `json:"pr_number"`
	PRURL        string   `json:"pr_url"`
	Tasks        []string `json:"tasks"`
	State        string   `json:"state"`      // open | merged | closed-unmerged | not-found
	LastEvent    string   `json:"last_event"` // reduced event: ready | ci-failed | stale | ...
	LastSnapshot *struct {
		Checks           string `json:"checks"`
		ReviewDecision   string `json:"review_decision"`
		MergeStateStatus string `json:"merge_state_status"`
		IsDraft          bool   `json:"is_draft"`
	} `json:"last_snapshot"`
	InflightAction *struct {
		Kind string `json:"kind"`
	} `json:"inflight_action"`
}

// readPRWatches loads <pdir>/pr-watches.json into {pr_url: watch} and
// {slug: watch}. Missing / malformed → empty maps.
func readPRWatches(pdir string) (byURL map[string]*prWatch, bySlug map[string]*prWatch) {
	byURL = map[string]*prWatch{}
	bySlug = map[string]*prWatch{}
	data, err := os.ReadFile(filepath.Join(pdir, "pr-watches.json"))
	if err != nil {
		return byURL, bySlug
	}
	var raw struct {
		Watches map[string]*prWatch `json:"watches"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return byURL, bySlug
	}
	for key, w := range raw.Watches {
		if w == nil {
			continue
		}
		if w.PRNumber == 0 {
			if n, err := strconv.Atoi(key); err == nil {
				w.PRNumber = n
			}
		}
		if w.PRURL != "" {
			byURL[w.PRURL] = w
		}
		for _, s := range w.Tasks {
			if s != "" {
				bySlug[s] = w
			}
		}
	}
	return byURL, bySlug
}

// prNumberFromURL pulls the trailing /pull/<n> out of a GitHub PR URL.
// 0 when the URL has no numeric tail.
func prNumberFromURL(url string) int {
	url = strings.TrimRight(url, "/")
	i := strings.LastIndex(url, "/")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(url[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// prStateSummary renders the compact PR state for a row: the watcher's
// durable snapshot when it has one, otherwise just the URL (state unknown
// — the successor's first tick re-probes).
func prStateSummary(prURL string, w *prWatch) string {
	if prURL == "" && w == nil {
		return ""
	}
	num := 0
	if w != nil {
		num = w.PRNumber
	}
	if num == 0 {
		num = prNumberFromURL(prURL)
	}
	label := "PR"
	if num > 0 {
		label = fmt.Sprintf("PR #%d", num)
	}
	if w == nil {
		return label + " " + prURL + " (state unknown)"
	}
	st := w.State
	if st == "" {
		st = "open"
	}
	parts := []string{label + " " + st}
	if w.State == "" || w.State == "open" {
		if s := w.LastSnapshot; s != nil {
			if s.IsDraft {
				parts = append(parts, "draft")
			}
			if s.Checks != "" {
				parts = append(parts, "ci "+s.Checks)
			}
			if s.ReviewDecision != "" {
				parts = append(parts, "review "+s.ReviewDecision)
			}
			if s.MergeStateStatus != "" {
				parts = append(parts, "merge "+s.MergeStateStatus)
			}
		}
		if e := w.LastEvent; e != "" && e != "open" && e != "skip" {
			parts = append(parts, e)
		}
		if a := w.InflightAction; a != nil && a.Kind != "" {
			parts = append(parts, "fixer running: "+a.Kind)
		}
	}
	return strings.Join(parts, ", ")
}

// nextJobFor derives the successor's next action for a row from its
// status + PR state. Deterministic and deliberately conservative: it
// names the branch handoff_resume.py / the tick will take, not a guess.
func nextJobFor(t *tasks.Task, inFlight bool, w *prWatch, doneBySlug map[string]bool) string {
	if t.Parked != "" {
		return "parked — operator decision: " + oneLine(t.Parked)
	}
	prLabel := "PR"
	if n := prNumber(t.PRURL, w); n > 0 {
		prLabel = fmt.Sprintf("PR #%d", n)
	}
	switch t.Status {
	case tasks.StatusInProgress:
		if t.PRURL != "" {
			return "shepherd " + prLabel + " (worker already pushed)"
		}
		if inFlight {
			return "let worker finish; re-dispatch from Active Subagents if dead"
		}
		return "re-dispatch worker (no live subagent)"
	case tasks.StatusInReview:
		if t.PRURL == "" {
			return "in-review without PR — verify and requeue or set pr_url"
		}
		if w != nil {
			switch w.State {
			case "merged":
				return "flip to done (PR merged)"
			case "closed-unmerged":
				return "PR closed unmerged — requeue or abandon"
			case "not-found":
				return "PR not found — verify pr_url"
			}
			switch w.LastEvent {
			case "ready":
				return "merge " + prLabel
			case "ci-failed":
				return "fix CI on " + prLabel
			case "changes-requested":
				return "address review on " + prLabel
			case "stale", "behind", "dirty":
				return "rebase " + prLabel
			}
		}
		return "shepherd " + prLabel + " (await CI/review)"
	case tasks.StatusBlocked:
		return "unblock (see Open Questions)"
	case tasks.StatusReady:
		if missing := unmetDeps(t, doneBySlug); len(missing) > 0 {
			return "dispatch once deps done: " + strings.Join(missing, ", ")
		}
		return "dispatch worker"
	case tasks.StatusTodo:
		if missing := unmetDeps(t, doneBySlug); len(missing) > 0 {
			return "waiting on deps: " + strings.Join(missing, ", ")
		}
		return "promote to ready"
	case tasks.StatusDone:
		return "none (done)"
	case tasks.StatusAbandoned:
		return "none (abandoned)"
	}
	return ""
}

func prNumber(prURL string, w *prWatch) int {
	if w != nil && w.PRNumber > 0 {
		return w.PRNumber
	}
	return prNumberFromURL(prURL)
}

func unmetDeps(t *tasks.Task, doneBySlug map[string]bool) []string {
	var out []string
	for _, d := range t.DependsOn {
		d = strings.TrimSpace(d)
		if d != "" && !doneBySlug[d] {
			out = append(out, d)
		}
	}
	return out
}

// statusRank orders rows most-urgent-first: in flight before queued,
// queued before finished.
func statusRank(s tasks.Status, parked bool) int {
	if parked {
		return 2
	}
	switch s {
	case tasks.StatusInProgress:
		return 0
	case tasks.StatusInReview:
		return 1
	case tasks.StatusBlocked:
		return 2
	case tasks.StatusReady:
		return 3
	case tasks.StatusTodo:
		return 4
	case tasks.StatusDone:
		return 5
	default:
		return 6
	}
}

func isInFlightStatus(t *tasks.Task) bool {
	if t.Parked != "" {
		return true
	}
	switch t.Status {
	case tasks.StatusInProgress, tasks.StatusInReview, tasks.StatusBlocked:
		return true
	}
	return false
}

// CollectStatusRows builds the ordered StatusRow set for the project under
// pdir. agentID scopes the session slugs (foreignGeneration); subs are the
// doc's Active Subagents (worker phase + "a live subagent exists").
// Returns nil when tasks.md is missing/unreadable or nothing qualifies.
func CollectStatusRows(pdir, agentID string, subs []ActiveSubagent) []StatusRow {
	all := readTasksTolerant(filepath.Join(pdir, "tasks.md"))
	if len(all) == 0 {
		return nil
	}
	bySlug := make(map[string]*tasks.Task, len(all))
	doneBySlug := map[string]bool{}
	for _, t := range all {
		bySlug[t.Slug] = t
		if t.Status == tasks.StatusDone {
			doneBySlug[t.Slug] = true
		}
	}

	cs := readCoordStateForCollect(filepath.Join(pdir, "coord-state.json"))
	session := map[string]bool{}
	for _, st := range cs.SessionTasks {
		if s := strings.TrimSpace(st.Slug); s != "" && !foreignGeneration(st.CoordID, agentID) {
			session[s] = true
		}
	}
	for _, e := range cs.SessionNextSteps {
		if s := strings.TrimSpace(e.Slug); s != "" && !foreignGeneration(e.CoordID, agentID) {
			session[s] = true
		}
	}

	phaseBySlug := map[string]string{}
	liveBySlug := map[string]bool{}
	for _, s := range subs {
		liveBySlug[s.TaskID] = true
		phaseBySlug[s.TaskID] = s.LastPhase
	}

	watchByURL, watchBySlug := readPRWatches(pdir)

	var picked []*tasks.Task
	for _, t := range all {
		if t.Status == tasks.StatusAbandoned {
			continue
		}
		if isInFlightStatus(t) || session[t.Slug] || liveBySlug[t.Slug] {
			picked = append(picked, t)
		}
	}
	if len(picked) == 0 {
		return nil
	}
	sort.SliceStable(picked, func(i, j int) bool {
		ri, rj := statusRank(picked[i].Status, picked[i].Parked != ""), statusRank(picked[j].Status, picked[j].Parked != "")
		if ri != rj {
			return ri < rj
		}
		pi, pj := priorityRank(picked[i].Priority), priorityRank(picked[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return picked[i].Slug < picked[j].Slug
	})

	rows := make([]StatusRow, 0, len(picked))
	for _, t := range picked {
		w := watchByURL[t.PRURL]
		if w == nil {
			w = watchBySlug[t.Slug]
		}
		prURL := t.PRURL
		if prURL == "" && w != nil {
			prURL = w.PRURL
		}
		status := string(t.Status)
		if t.Parked != "" && t.Status != tasks.StatusBlocked {
			status += " (parked)"
		}
		rows = append(rows, StatusRow{
			Slug:     t.Slug,
			Status:   status,
			Priority: string(t.Priority),
			Phase:    phaseBySlug[t.Slug],
			PRURL:    prURL,
			PRState:  prStateSummary(prURL, w),
			NextJob:  nextJobFor(t, liveBySlug[t.Slug], w, doneBySlug),
			Terminal: t.Status == tasks.StatusDone || t.Status == tasks.StatusAbandoned,
		})
	}
	return rows
}

// RenderStatus renders the `## Status` body: a one-line header, a
// status-count summary, the per-task rows (capped), and the single next
// job. rows==nil renders the header + StatusNonePlaceholder so a coord
// doc always states its status explicitly. nextJob "" is omitted.
func RenderStatus(d *Doc, rows []StatusRow, nextJob string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Handoff from coord %s", d.AgentID)
	if d.ContextPctAtHandoff != nil {
		fmt.Fprintf(&b, " at %s%% context", strconv.FormatFloat(*d.ContextPctAtHandoff, 'f', -1, 64))
	}
	b.WriteString(".")
	if len(rows) == 0 {
		b.WriteString("\n")
		b.WriteString(StatusNonePlaceholder)
	} else {
		b.WriteString("\nTasks: ")
		b.WriteString(statusCounts(rows))
		shown := rows
		hidden := 0
		if len(shown) > statusRowsMax {
			hidden = len(shown) - statusRowsMax
			shown = shown[:statusRowsMax]
		}
		for _, r := range shown {
			b.WriteString("\n")
			b.WriteString(r.Line())
		}
		if hidden > 0 {
			fmt.Fprintf(&b, "\n- … and %d more", hidden)
		}
	}
	if nextJob = oneLine(nextJob); nextJob != "" {
		b.WriteString("\n\nNext job: ")
		b.WriteString(nextJob)
	}
	return b.String()
}

// statusCounts renders `2 in-progress, 1 in-review, 1 ready` in row order
// (rows are already rank-sorted, so the order is stable and urgent-first).
func statusCounts(rows []StatusRow) string {
	counts := map[string]int{}
	var order []string
	for _, r := range rows {
		key := r.Status
		if _, seen := counts[key]; !seen {
			order = append(order, key)
		}
		counts[key]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return strings.Join(parts, ", ")
}

// PickNextJob chooses the single `Next job:` line: the coord's FIRST
// explicit Next Step when it recorded one (the coord's own plan beats a
// derived action), otherwise the first actionable row's derived job.
func PickNextJob(nextSteps string, rows []StatusRow) string {
	for _, line := range strings.Split(nextSteps, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "- [explicit] "); ok && strings.TrimSpace(rest) != "" {
			return strings.TrimSpace(rest)
		}
	}
	for _, r := range rows {
		if r.Terminal || r.NextJob == "" {
			continue
		}
		return r.NextJob + " (" + r.Slug + ")"
	}
	return ""
}

// BuildStatus fills doc.Status from the project under pdir. Call it LAST
// in a producer, after Active Subagents and Next Steps are final — it
// reads both off the doc. Returns the rows so the caller can hand them to
// RecordTaskTracking. Never errors; a nil doc is a no-op.
func BuildStatus(doc *Doc, pdir, agentID string) []StatusRow {
	if doc == nil {
		return nil
	}
	rows := CollectStatusRows(pdir, agentID, doc.ActiveSubagents)
	ns := doc.NextSteps
	if ns == Placeholder {
		ns = ""
	}
	doc.Status = RenderStatus(doc, rows, PickNextJob(ns, rows))
	doc.StatusRows = rows
	return rows
}

// oneLine flattens CR/LF to spaces and trims — every Status line must be
// a single physical line (a stray newline could forge a `## ` header in
// a doc the successor reads as trusted instructions).
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

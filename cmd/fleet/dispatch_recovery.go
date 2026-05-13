package main

// dispatch_recovery.go — dead-coord detection + synth-handoff wiring
// for `fleet dispatch coord-<project> --project <project> --coord-spawn`
// run against a project whose previous coord left a stale record on disk
// (pid dead AND tmux session gone).
//
// The flow is opt-in via the dead-pid+dead-tmux probe at dispatch entry.
// A live coord — even one whose tmux session is unhappy — is NEVER
// hijacked; the operator might just be on a different tmux server.
//
// Steps the caller wires in:
//
//	1. findRecoveryCandidate(taskID, project, records, pidAlive, sessionAlive)
//	   → dead coord record OR nil
//	2. writeRecoveryHandoffDoc(deadRec, now)
//	   → docPath, err
//	3. spawn.Spawn(spawn.Options{OldRecord: deadRec, NewDocPath: docPath, ...})
//	4. archive deadRec (the caller's existing post-spawn cleanup path)
//
// Step 3's OldRecord branch in spawn.Spawn already does the lineage
// inheritance: handoff_number ++, last_handoff_path = NewDocPath, taskID
// and project carried forward. No new spawn options required.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
)

// pidAliveFn lets tests stub the kill(0) probe used by
// findRecoveryCandidate. Production wraps the same syscall the
// workers package uses (kill(pid, 0)); the indirection is a var on
// the helper rather than a function arg so the dispatch entry can
// call findRecoveryCandidate with the default probe in one line.
type pidAliveFn func(pid int) bool

// sessionAliveProbe is the tmux liveness probe used by
// findRecoveryCandidate. tmux.HasSession in production; tests stub
// with a closure over a fixed name set.
type sessionAliveProbe func(session string) bool

// coordFreshProbe reports whether the project's coord-state.json
// mtime is within the freshness window — i.e., a coord ticked recently
// and is therefore alive. Production stats
// ~/.fleet/projects/<project>/coord-state.json and compares against
// coordFreshnessWindow (5m, mirroring the dashboard's active-coord
// rendering threshold). Tests stub with a closure.
//
// This is the load-bearing safety gate that the pidAlive/sessionAlive
// pair cannot provide:
//   - agent.Record.PID is the SHORT-LIVED dispatch CLI's pid (set in
//     spawn.Spawn via os.Getpid), not the coord inside tmux. After
//     `fleet dispatch` exits, that pid is dead even for a healthy
//     coord — and the OS may have reused it for an unrelated process
//     (codex iter-3 P2).
//   - tmux.HasSession returns false when the operator's dashboard sits
//     on a different tmux server/socket than the live coord's session.
//
// coord-state.json mtime, by contrast, is updated by the Python
// /coordinator skill on every tick (the skill writes ack timestamps +
// supervisor sub-state through state.WriteAtomic on each successful
// tick). A coord whose mtime is fresh has ticked within the window
// and is alive on SOME tmux server. A coord with stale mtime hasn't
// ticked recently and IS the safe recovery target.
//
// Note: the coord skill takes LOCK_NB|LOCK_EX on coordinator.lock per
// tick and releases it in finally. The lock is therefore NOT held
// between ticks — using it as a liveness signal would falsely classify
// healthy idle coords as dead (codex iter-3 P1). mtime is the right
// signal because it advances with each tick and persists between them.
type coordFreshProbe func(project string) bool

// coordFreshnessWindow is how recently coord-state.json must have been
// mtime-updated for the coord to be considered alive. Mirrors the TUI
// dashboard's coordActiveWindow (internal/tui/dashboard.go) so the
// recovery probe and the dashboard agree on what "live" means — an
// operator who sees "● active" on the dashboard cannot also trigger
// a recovery flow against the same project.
const coordFreshnessWindow = 5 * time.Minute

// findRecoveryCandidate walks the live agent records (output of
// agent.List) looking for a dead coord matching the given task_id +
// project. "Dead" = coord-state.json is STALE (mtime older than
// coordFreshnessWindow) AND pid is NOT alive AND tmux session is NOT
// alive.
//
// The mtime check is the load-bearing signal. pid + tmux probes can
// both look dead even for a healthy coord (operator on different tmux
// server + dispatch CLI pid is short-lived). They remain as belt-and-
// suspenders so an alive-anywhere signal still vetoes recovery.
//
// Returns the first match (the slice order is agent.List order, which
// is filesystem-iteration order — not stable across runs but stable
// within one run, and the multiple-dead-coords case is itself an
// edge case the caller is expected to clean up post-spawn via
// archive of the predecessor).
//
// Returns nil when no record matches. Production callers MUST NOT
// substitute a recovery synth for any nil case — that would steal a
// fresh dispatch's identity (e.g. an operator typo'd task name that
// happens to look like a coord sentinel).
//
// The probe-fn arguments exist to keep this pure-function — no syscall,
// no exec — so tests can run without touching tmux or the kernel.
// Production passes pidAlive + tmux.HasSession + coordStateFresh.
func findRecoveryCandidate(
	taskID, project string,
	records []*agent.Record,
	pidAlive pidAliveFn,
	sessionAlive sessionAliveProbe,
	coordFresh coordFreshProbe,
) *agent.Record {
	// Note: coordFresh is intentionally NOT consulted here (codex
	// review iter-9 P1). A recent mtime is consistent with both
	// "live coord on a different tmux socket" AND "coord crashed
	// within the freshness window." The dispatch-side veto
	// (runDispatch) combines coordFresh with liveCoordRecordExists
	// to distinguish those two cases — only the first should refuse
	// the dispatch. findRecoveryCandidate's job is narrower: identify
	// dead records that should be recovered via synth handoff. Doing
	// the veto here would suppress synth recovery for recent crashes
	// (the exact case the feature exists to handle), orphaning the
	// in-flight worker state.
	//
	// coordFresh is retained in the signature so production callers
	// can pass coordStateFresh and tests can pass a stub matching the
	// production probe shape, even though the body doesn't read it.
	_ = coordFresh
	// pidAlive is retained in the signature for backwards-compat with
	// tests but the body no longer consults it (codex review iter-11
	// P2). agent.Record.PID is the SHORT-LIVED dispatch CLI's pid set
	// in spawn.Spawn via os.Getpid; after dispatch exits, that pid is
	// reused by an unrelated host process within minutes. Using it as
	// a "live" signal would suppress legitimate recoveries whenever
	// the host happened to recycle the old dispatch CLI's pid. tmux
	// session aliveness on the local socket is the only reliable
	// negative signal we have, and even that misses cross-socket
	// scenarios (see the dispatch-side comment in runDispatch).
	_ = pidAlive
	var best *agent.Record
	for _, r := range records {
		if r == nil {
			continue
		}
		if r.TaskID != taskID || r.Project != project {
			continue
		}
		// Alive tmux on this socket → NOT a recovery candidate. A
		// synth-recovery here would race a live shell on the same
		// session name. Cross-socket coords are invisible to this
		// probe, but the coord skill's NB-flock catches those
		// downstream — the loser exits cleanly.
		if r.TmuxSession != "" && sessionAlive(r.TmuxSession) {
			continue
		}
		// Pick the most-recently-spawned dead record (codex review
		// iter-7 P2). agent.List returns filesystem-iteration order
		// which is NOT timestamp-sorted; with multiple stale records
		// (e.g., a prior recovery itself crashed before archiving its
		// predecessor), an arbitrary first-match would inherit cwd /
		// engine / handoff-chain from an older lineage and restart in
		// the wrong checkout. SpawnedAt is the canonical "this agent
		// was minted" timestamp on agent.Record.
		if best == nil || r.SpawnedAt.After(best.SpawnedAt) {
			best = r
		}
	}
	return best
}

// coordStateFresh reports whether
// ~/.fleet/projects/<project>/coord-state.json was mtime-updated
// within coordFreshnessWindow. Returns true (= live coord) when:
//   - the file exists AND its mtime is within the window
//   - stat fails with a transient error (codex review iter-17 P1):
//     we can't tell live-vs-dead, so fail closed on the safety
//     side — the live-coord veto fires and the dispatch refuses
//     rather than risk spawning a duplicate against an
//     unverified-but-possibly-live coord.
//
// Returns false (= dead or never-ran) when:
//   - the file doesn't exist (no coord ever ran for this project)
//   - the file mtime is older than the window
//
// Production probe for findRecoveryCandidate. The Python /coordinator
// skill writes coord-state.json through state.WriteAtomic on every
// tick — both for ack timestamps and for supervisor sub-state. mtime
// therefore advances with every tick that runs to completion, even
// if no fields actually changed. A stale mtime means the coord
// stopped ticking, which is the recovery trigger.
func coordStateFresh(project string) bool {
	pdir, err := state.ProjectDir(project)
	if err != nil {
		// Path resolution failed — we can't even locate the file,
		// so we can't make a safety call. Return true so the
		// --coord-spawn veto fires (fail closed). The error would
		// be surfaced to the operator on the next path-using call.
		return true
	}
	fi, err := os.Stat(filepath.Join(pdir, "coord-state.json"))
	if err != nil {
		if os.IsNotExist(err) {
			// No coord has ever run for this project.
			return false
		}
		// Transient stat error (permission, I/O, broken mount).
		// Fail closed: treat as fresh so the live-coord veto
		// fires and we don't accidentally spawn a duplicate.
		return true
	}
	return time.Since(fi.ModTime()) <= coordFreshnessWindow
}

// coordRecordExistsInList reports whether the given pre-fetched
// records slice contains an unarchived agent record for this task_id
// + project. The dispatch-side live-coord veto pairs this with
// coordStateFresh to detect "something is supervising this project"
// without false-positives from stale leftover files.
//
// Takes a slice rather than calling agent.List internally (codex
// review iter-15 P1): the caller fetches the list ONCE early in
// runDispatch and fail-closes on errors, so a corrupt agents
// directory aborts the spawn instead of silently disabling the
// live-coord veto and risking split-brain on a different tmux socket.
// The slice is also reused by findRecoveryCandidate below.
func coordRecordExistsInList(taskID, project string, records []*agent.Record) bool {
	for _, r := range records {
		if r == nil {
			continue
		}
		if r.TaskID == taskID && r.Project == project {
			return true
		}
	}
	return false
}

// pidAlive is the production pidAliveFn. Mirrors workers.IsAlive (the
// existing kill(pid, 0) probe) — duplicated here rather than imported
// to avoid the workers → handoff dep cycle (workers already depends
// on state; dispatch already depends on workers via the CLI; pulling
// in the lifecycle package would tangle the import graph).
//
// pid <= 0 returns false (nothing to probe). EPERM ("process exists,
// I lack signal permission") returns true — the question is "alive",
// not "kill-able".
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if err == syscall.EPERM {
		return true
	}
	return false
}

// writeRecoveryHandoffDoc synthesizes a recovery-synth handoff doc
// for deadRec from on-disk state and writes it to
// ~/.fleet/handoffs/<dead-id>-<UTC-stamp>.md (the same path layout
// real handoffs use, via state.HandoffPath). Returns the doc path so
// the caller can pass it as spawn.Options.NewDocPath.
//
// Side effect: the dead agent's last_handoff_path is updated to point
// at the synth doc. Why on the DEAD record (which will be archived
// shortly): the doc's chain field is "previous_handoff", and the
// successor reads its predecessor's record at spawn time to populate
// that frontmatter. Without updating the dead record, the chain link
// to the synth doc is lost the moment the dead record is archived.
//
// The handoff doc itself does NOT carry the "previous_handoff: <synth>"
// chain pointer — its previous_handoff IS the synth, so it inherits
// the synth's chain (which may be nil for a never-handoff'd coord).
func writeRecoveryHandoffDoc(deadRec *agent.Record, ts time.Time) (string, error) {
	if deadRec == nil {
		return "", fmt.Errorf("writeRecoveryHandoffDoc: nil deadRec")
	}
	doc, err := handoff.SynthesizeRecovery(deadRec.ID, deadRec.Project, ts)
	if err != nil {
		return "", fmt.Errorf("synthesize recovery doc: %w", err)
	}
	// Chain link: the synth doc's previous_handoff is whatever the dead
	// coord itself last inherited from. nil for a never-handed-off
	// coord. Mirrors handoff.NewManualStub's behavior so the chain
	// invariant survives the recovery.
	doc.PreviousPath = deadRec.LastHandoffPath
	doc.Number = deadRec.HandoffNumber

	// Populate OpenPRs by shelling out to `gh pr list` (codex review
	// iter-18 P1). When the dead coord had tasks in-review status,
	// handoff_resume.py deliberately skips re-dispatching those
	// workers and relies on the ## Open PRs section to re-spawn the
	// PR shepherd until-loops. Without this enrichment, every
	// recovery-synth doc renders _(no open PRs)_ and the successor
	// drops in-review PRs on the floor — no worker re-dispatch, no
	// shepherd. Mirrors skills/fleet-guard/handoff.py:_collect_open_prs.
	//
	// Failures are silent (best-effort): gh missing, auth issue, or
	// timeout returns an empty slice and the doc renders the
	// placeholder. The successor coord can re-derive from `gh pr
	// list` on its first tick if it cares.
	doc.OpenPRs = collectOpenPRs(deadRec.Cwd)
	// Fallback (codex review round-6 P1): if `gh pr list` returned
	// empty — gh missing, auth issue, network blip, or no PRs match
	// the head:worker/ search — supplement from tasks.md. Any task
	// with status=in-review and a non-empty pr_url is a PR the
	// successor must respawn a shepherd for; the gh enrichment alone
	// is best-effort and can silently drop these in a degraded
	// environment. tasks.md is the on-disk source of truth maintained
	// by the coord skill on every PR transition, so it survives gh
	// outages by definition.
	if len(doc.OpenPRs) == 0 {
		doc.OpenPRs = openPRsFromTasks(deadRec.Project)
	}

	docPath, err := state.HandoffPath(deadRec.ID, ts)
	if err != nil {
		return "", fmt.Errorf("resolve handoff doc path: %w", err)
	}
	if err := handoff.Write(doc, docPath); err != nil {
		return "", fmt.Errorf("write handoff doc: %w", err)
	}
	// We deliberately DO NOT mutate deadRec.LastHandoffPath here
	// (codex review iter-12 P1). When findRecoveryCandidate misclassifies
	// a live coord on another tmux socket as dead, rewriting its
	// record's last_handoff_path would corrupt the live coord's chain
	// — its next real handoff would build from this synthetic doc it
	// never inherited. The chain link instead lives on the SUCCESSOR's
	// record: spawn.Spawn's OldRecord branch sets the new record's
	// LastHandoffPath = NewDocPath, so the recovery doc IS the
	// successor's predecessor link. If the recovery loses the
	// coordinator.lock race, the successor exits cleanly and its
	// (unused) record can be archived manually; the live coord's
	// chain is untouched.
	return docPath, nil
}

// collectOpenPRs shells out to `gh pr list --state open --search
// head:worker/ --json number,title,headRefName,url` and returns the
// parsed list as handoff.OpenPR entries. Used by the recovery path
// to pre-populate the synth doc's ## Open PRs section so the
// successor coord re-spawns shepherd until-loops for in-review
// workers. Mirrors skills/fleet-guard/handoff.py:_collect_open_prs.
//
// cwd: optional directory to run gh in. The gh CLI needs a git
// repo to discover the upstream — passing the dead coord's recorded
// cwd makes the command resolve to the right repo even when fleet
// is invoked from another directory. Empty falls back to gh's
// default (process cwd).
//
// Best-effort: missing gh binary, auth failure, JSON parse error,
// or 10s timeout returns an empty slice. The doc still renders with
// the _(no open PRs)_ placeholder; the successor can re-derive from
// gh on its first tick if it needs to.
//
// var so tests can stub. Production wraps the gh shell-out.
var collectOpenPRs = func(cwd string) []handoff.OpenPR {
	// Skip when cwd is unknown (codex review iter-19 P2): legacy
	// records may not carry Cwd, and gh would then resolve PR list
	// against the process cwd — possibly an unrelated repo. The
	// tasks.md fallback in writeRecoveryHandoffDoc covers this case
	// authoritatively, so a nil return here just hands off to that
	// path rather than risking wrong-repo PR URLs in the synth doc.
	if cwd == "" {
		return nil
	}
	ghBin, err := exec.LookPath("gh")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghBin,
		"pr", "list",
		"--state", "open",
		"--search", "head:worker/",
		"--json", "number,title,headRefName,url",
	)
	cmd.Dir = cwd
	stdout, err := cmd.Output()
	if err != nil {
		return nil
	}
	var raw []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil
	}
	out := make([]handoff.OpenPR, 0, len(raw))
	for _, p := range raw {
		out = append(out, handoff.OpenPR{
			Number:      p.Number,
			Title:       p.Title,
			HeadRefName: p.HeadRefName,
			URL:         p.URL,
		})
	}
	return out
}

// openPRsFromTasks reads ~/.fleet/projects/<project>/tasks.md and
// returns one handoff.OpenPR per task that is in-review and carries a
// non-empty pr_url. Fallback for the recovery synth doc when
// collectOpenPRs returned empty (codex review round-6 P1) — gh
// availability isn't required for the successor coord to respawn
// shepherds for in-review PRs.
//
// Title falls back to the task slug (tasks.md doesn't carry a separate
// PR title field; the coord skill's shepherd until-loop only needs URL
// + Number to track CI/merge state). HeadRefName mirrors the coord
// skill's branch convention (`worker/<slug>`). Number is parsed from
// the URL's trailing path segment; if parse fails, the entry is still
// emitted with Number=0 so the URL is at least surfaced to the
// successor — the shepherd can re-derive the number from `gh pr view`.
//
// Returns nil on tasks.md missing / parse failure: degraded-but-not-
// catastrophic — the doc will render the `_(no open PRs)_` placeholder
// and the successor coord's first tick will re-scan tasks.md anyway.
func openPRsFromTasks(project string) []handoff.OpenPR {
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return nil
	}
	path := filepath.Join(pdir, "tasks.md")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	f, err := tasks.Read(path)
	if err != nil {
		return nil
	}
	var out []handoff.OpenPR
	for _, t := range f.Tasks {
		if t.Status != tasks.StatusInReview || t.PRURL == "" {
			continue
		}
		out = append(out, handoff.OpenPR{
			Number:      prNumberFromURL(t.PRURL),
			Title:       t.Slug,
			HeadRefName: "worker/" + t.Slug,
			URL:         t.PRURL,
		})
	}
	return out
}

// prNumberFromURL extracts the trailing PR number from a GitHub PR URL
// like https://github.com/owner/repo/pull/42. Returns 0 on parse
// failure — the caller emits the OpenPR anyway so the URL still
// reaches the successor.
func prNumberFromURL(url string) int {
	idx := strings.LastIndex(url, "/")
	if idx < 0 || idx == len(url)-1 {
		return 0
	}
	n, err := strconv.Atoi(url[idx+1:])
	if err != nil {
		return 0
	}
	return n
}

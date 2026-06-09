//go:build linux || darwin

package main

// doctor_unix.go — the real `fleet doctor` inspection + recovery, gated to
// linux||darwin because it leans on the lease + authenticated-kill
// primitives (coordlock + internal/coord), which are themselves
// linux||darwin only. doctor_other.go carries the non-unix stub.
//
// INSPECTION (gatherDoctorReport, read-only — mutates NOTHING):
//   - coordinator lease health via coordlock.Diagnose (reuses the SAME
//     staleness math AcquireLease uses — never reinvented here);
//   - the fenced-but-not-acquired escalation as its own diagnosis;
//   - pending spawn-fresh queue files with no live successor (a handoff
//     that didn't complete);
//   - duplicate handoff docs on disk (a handoff "storm");
//   - wedged `fleet drain` run-records (reuses the gc KindDrainProcs
//     classifier in DRY-RUN — gc owns the reaping, doctor only reports);
//   - the coordinator's tmux session liveness.
//
// RECOVERY (doctorRunFix, --fix only — fence -> STONITH -> acquire/respawn):
//   - REFUSES to disturb a live, heartbeating coordinator (T21);
//   - reaps a provably hung/dead coordinator through the ONE authenticated
//     coord.KillCoordIfIdentityMatches primitive (invariant 7 — never a raw
//     kill), via coordlock.AcquireLeaseWithKill's fence->kill->acquire;
//   - re-spawns a successor from the cached old record (RecoverDeadCoord)
//     using the EXISTING productionRecoverSpawn path (no parallel spawn);
//   - the fenced_not_acquired case it cannot authenticate is left as the
//     typed state + surfaced for operator-confirmed recovery (T32 — no
//     silent stall, no second leader).

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coord"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/gc"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// doctorDeps are the injectable seams that keep gatherDoctorReport +
// doctorRunFix deterministic under test (no real lease reads, no real kill,
// no real spawn, no real session probe). Production callers use
// defaultDoctorDeps().
type doctorDeps struct {
	// Diagnose returns the read-only lease health snapshot for a project.
	// Production: coordlock.Diagnose.
	Diagnose func(project string) coordlock.LeaseDiagnosis
	// LeaderPresent reports whether a HEALTHY leader holds the lease.
	// Production: coordlock.LeaderPresent.
	LeaderPresent func(project string) bool
	// ListAgents lists the live agent records (to enumerate coord projects +
	// find the coord record to respawn from). Production: agent.List.
	ListAgents func() ([]*agent.Record, error)
	// LoadAgent loads one agent record. Production: agent.Load.
	LoadAgent func(id string) (*agent.Record, error)
	// CoordMarker returns the recorded coord-spawn agent id for a project
	// ("" if none). Production: state.ReadCoordSpawnMarker.
	CoordMarker func(project string) string
	// SessionAlive reports whether a tmux session is alive. Production:
	// tmux.SessionAlive.
	SessionAlive func(session string) (bool, error)
	// ListPendingQueue lists pending spawn-fresh queue file paths.
	// Production: queue.ListPending.
	ListPendingQueue func() ([]string, error)
	// ReadQueue parses a spawn-fresh queue file. Production: queue.ReadSpawnFresh.
	ReadQueue func(path string) (queue.SpawnFresh, error)
	// DeleteQueue removes a spawn-fresh queue file after the recovery spawn
	// fulfilled its handoff (so a later drain doesn't re-process it).
	// Production: queue.Delete.
	DeleteQueue func(path string) error
	// HandoffDocs lists handoff doc filenames in ~/.fleet/handoffs/.
	// Production: a ReadDir over state.HandoffDir.
	HandoffDocs func() ([]string, error)
	// LeaseProjects lists every project name that has a coordinator lease
	// record on disk (~/.fleet/projects/<name>/.locks/coordinator.epoch),
	// so the default scan reaches a stuck project whose coord record AND
	// queue file are both gone but whose stale lease lingers. Production:
	// defaultDoctorLeaseProjects.
	LeaseProjects func() ([]string, error)
	// WedgedDrains returns the count of WEDGED fleet drain run-records (gc
	// KindDrainProcs, dry-run — gc owns the reaping). Production: a
	// gc.Reconcile dry-run filtered to KindDrainProcs.
	WedgedDrains func() (int, error)
	// TakeOver runs the safety-net takeover (fence -> authenticated kill ->
	// acquire) against a hung/dead coord. Returns acquired + err. Production:
	// a closure over coordlock.AcquireLeaseWithKill + the authenticated kill,
	// releasing the lease immediately (doctor is not the coordinator).
	TakeOver func(project, agentID string) (acquired bool, err error)
	// RecoverSpawn brings up a fresh lease-wrapped successor from the cached
	// old record (dead-coord recovery). Production: productionRecoverSpawn.
	RecoverSpawn func(oldRec *agent.Record, docPath, preAllocatedID string, disableAutoResume bool, stdout, stderr io.Writer) error
	// Self is the caller's pid (never reap self). Production: os.Getpid.
	Self func() int
}

func defaultDoctorDeps() doctorDeps {
	return doctorDeps{
		Diagnose:         coordlock.Diagnose,
		LeaderPresent:    coordlock.LeaderPresent,
		ListAgents:       agent.List,
		LoadAgent:        agent.Load,
		CoordMarker:      state.ReadCoordSpawnMarker,
		SessionAlive:     tmux.SessionAlive,
		ListPendingQueue: queue.ListPending,
		ReadQueue:        queue.ReadSpawnFresh,
		DeleteQueue:      queue.Delete,
		HandoffDocs:      defaultDoctorHandoffDocs,
		LeaseProjects:    defaultDoctorLeaseProjects,
		WedgedDrains:     defaultDoctorWedgedDrains,
		TakeOver: func(project, agentID string) (bool, error) {
			lease, acquired, err := coordlock.AcquireLeaseWithKill(project, agentID,
				func(t coordlock.KillTarget) error {
					return coord.KillCoordIfIdentityMatches(coord.KillTarget{
						Pid:         t.Pid,
						PidStart:    t.PidStart,
						AgentID:     t.AgentID,
						Project:     t.Project,
						FencerEpoch: t.FencerEpoch,
					})
				})
			// doctor is NOT the coordinator — if it accidentally acquired the
			// lease (the old holder was already gone), release it immediately so
			// the respawned successor can lead. The point of the takeover is the
			// fence+kill side effect, not making the doctor the leader.
			if acquired && lease != nil {
				lease.Release()
			}
			return acquired, err
		},
		RecoverSpawn: productionRecoverSpawn,
		Self:         os.Getpid,
	}
}

// defaultDoctorHandoffDocs lists the *.md handoff docs in ~/.fleet/handoffs/.
func defaultDoctorHandoffDocs() ([]string, error) {
	dir, err := state.HandoffDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// defaultDoctorLeaseProjects lists every project that has a coordinator lease
// record on disk: ~/.fleet/projects/<name>/.locks/coordinator.epoch. This is
// the discovery path for a stuck project whose coord AGENT record AND
// spawn-fresh QUEUE file are both gone (archived/drained) but whose stale
// epoch lingers (codex PR6 iter-2 [P2]). Best-effort: a missing projects dir
// yields nil.
func defaultDoctorLeaseProjects() ([]string, error) {
	root, err := state.Root()
	if err != nil {
		return nil, err
	}
	projectsDir := filepath.Join(root, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // .locks/ and other dot-dirs are not projects
		}
		epoch := filepath.Join(projectsDir, name, ".locks", "coordinator.epoch")
		if _, statErr := os.Stat(epoch); statErr == nil {
			out = append(out, name)
		}
	}
	return out, nil
}

// defaultDoctorWedgedDrains counts WEDGED fleet drain run-records via the gc
// KindDrainProcs classifier in DRY-RUN (Apply=false). gc OWNS the reaping;
// the doctor only reports the count + points the operator at `fleet gc` (it
// does not duplicate the reaper).
func defaultDoctorWedgedDrains() (int, error) {
	report, err := gc.Reconcile(gc.Options{
		Apply: false,
		Kinds: []gc.Kind{gc.KindDrainProcs},
	}, gc.DefaultDeps())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range report.Actions {
		// VerbSurface on a drain-procs action == a WEDGED drain (a live pid
		// with a stale heartbeat). VerbWouldRemove == an already-dead record
		// gc would clean — not a "stuck coordinator" symptom, so don't count it.
		if a.Kind == gc.KindDrainProcs && a.Verb == gc.VerbSurface {
			n++
		}
	}
	return n, nil
}

// doctorProjects returns the set of projects to inspect. With --project it
// is just that one; otherwise it is every project that has a coordinator
// record OR a lease record — so a project whose coord crashed (record gone)
// but whose stale lease lingers is still surfaced.
func doctorProjects(opts doctorOpts, d doctorDeps) ([]string, error) {
	if opts.project != "" {
		return []string{opts.project}, nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	recs, err := d.ListAgents()
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if r == nil {
			continue
		}
		if isCoordAgentRecord(r) {
			add(r.Project)
		}
	}
	// ALSO include any project named by a pending spawn-fresh queue file
	// (codex PR6 [P2]): a stuck handoff whose OLD coord record was already
	// archived leaves ONLY the queue file behind. Without this, `fleet status`
	// would print "Run fleet doctor" but `fleet doctor` (no --project) would
	// find nothing to check — the recovery command couldn't reach the very
	// case it advertises. A --project flag still covers a fully-vanished one.
	if paths, qerr := d.ListPendingQueue(); qerr == nil {
		for _, p := range paths {
			if req, rerr := d.ReadQueue(p); rerr == nil {
				add(req.Project)
			}
		}
	}
	// AND include any project whose coordinator lease record lingers on disk
	// even though its agent record + queue file are both gone (codex PR6
	// iter-2 [P2]): a stuck coord that was archived/drained still leaves the
	// stale .locks/coordinator.epoch, which is the only on-disk trace. Without
	// this the operator would have to already know the name to pass --project.
	if leaseProjects, lerr := d.LeaseProjects(); lerr == nil {
		for _, p := range leaseProjects {
			add(p)
		}
	}
	return out, nil
}

// isCoordAgentRecord reports whether a record is a coordinator (its TaskID is
// the coord- prefixed task for its project).
func isCoordAgentRecord(r *agent.Record) bool {
	return strings.HasPrefix(r.TaskID, CoordTaskIDPrefix) && r.Project != ""
}

// gatherDoctorReport runs the read-only diagnosis for every in-scope
// project. It MUTATES NOTHING.
func gatherDoctorReport(opts doctorOpts) (doctorReport, error) {
	return gatherDoctorReportWith(opts, defaultDoctorDeps())
}

func gatherDoctorReportWith(opts doctorOpts, d doctorDeps) (doctorReport, error) {
	d = fillDoctorDeps(d)
	projects, err := doctorProjects(opts, d)
	if err != nil {
		return doctorReport{}, err
	}

	// Wedged-drain count is GLOBAL (drain run-records aren't per-project), so
	// read it once and attach it to each project's report as a shared finding.
	wedged, werr := d.WedgedDrains()
	if werr != nil {
		// Surface-don't-silo: a drain-scan failure shouldn't abort the whole
		// diagnosis. Report 0 wedged + carry on (the operator still sees lease
		// health). The error is folded into the per-project verbose detail.
		wedged = 0
	}

	report := doctorReport{}
	for _, p := range projects {
		pr := diagnoseProject(p, wedged, werr, d)
		report.projects = append(report.projects, pr)
	}
	return report, nil
}

// diagnoseProject builds one project's read-only report.
func diagnoseProject(project string, wedgedDrains int, wedgedErr error, d doctorDeps) doctorProjectReport {
	pr := doctorProjectReport{project: project}
	diag := d.Diagnose(project)

	// Headline status from the lease classification.
	switch diag.Health {
	case coordlock.LeaseHealthOK:
		pr.status = doctorStatusHealthy
	case coordlock.LeaseHealthHung:
		pr.status = doctorStatusUnresponsive
	case coordlock.LeaseHealthDead, coordlock.LeaseHealthReleased:
		pr.status = doctorStatusDead
	case coordlock.LeaseHealthFencedNotAcquired:
		pr.status = doctorStatusNeedsConfirm
	default: // LeaseHealthNone
		pr.status = doctorStatusNone
	}
	if diag.HasRecord {
		pr.verboseDetail = append(pr.verboseDetail, fmt.Sprintf(
			"lease: state=%s epoch=%d owner-pid=%d owner-alive=%v",
			defaultStr(diag.State, "-"), diag.Epoch, diag.OwnerPID, diag.OwnerAlive))
	}

	// Pending spawn-fresh queue files for this project with no live
	// successor == a handoff that didn't complete. This OVERRIDES a
	// none/dead headline with the canonical stuck-handoff status so the
	// plain message names the real symptom the operator sees.
	if pendingStuckHandoff(project, d) {
		if pr.status == doctorStatusNone || pr.status == doctorStatusDead {
			pr.status = doctorStatusHandoffStuck
		}
		pr.findings = append(pr.findings, doctorFinding{
			plain:   "a handoff to a fresh coordinator is pending and hasn't finished",
			verbose: "pending spawn-fresh queue file(s) for " + project + " with no live successor",
		})
	}

	// Coordinator session liveness (only when we have a coord record).
	if marker := d.CoordMarker(project); marker != "" {
		if rec, lerr := d.LoadAgent(marker); lerr == nil && rec != nil && rec.TmuxSession != "" {
			// tmux.SessionAlive reserves a NON-NIL error for an AMBIGUOUS probe
			// (socket/list-sessions failure) that must NOT be read as
			// "definitively dead" (codex PR6 iter-2 [P2]). Only treat the session
			// as gone on a CLEAN (false, nil) result; on an ambiguous error
			// surface the uncertainty under --verbose but do NOT downgrade a
			// healthy lease — a false "stopped" would trigger a needless respawn.
			alive, serr := d.SessionAlive(rec.TmuxSession)
			switch {
			case serr != nil:
				pr.verboseDetail = append(pr.verboseDetail,
					"tmux session "+rec.TmuxSession+" liveness probe was ambiguous: "+serr.Error()+" (not treating as dead)")
			case !alive:
				pr.findings = append(pr.findings, doctorFinding{
					plain:   "the coordinator's terminal session is gone",
					verbose: "tmux session " + rec.TmuxSession + " for coord " + marker + " is not alive",
				})
				if pr.status == doctorStatusHealthy {
					// A healthy lease but a CONFIRMED-dead session is
					// contradictory — downgrade to "stopped" so --fix can respawn.
					pr.status = doctorStatusDead
				}
			}
		}
	}

	// Duplicate handoff docs (a "storm").
	if n := handoffStormCount(project, d); n > 1 {
		pr.findings = append(pr.findings, doctorFinding{
			plain:   fmt.Sprintf("%d leftover handoff notes are piling up", n),
			verbose: fmt.Sprintf("%d handoff docs in ~/.fleet/handoffs/ prefixed for project %s (storm)", n, project),
		})
	}

	// Wedged drains (global; gc owns the reaping).
	if wedgedDrains > 0 {
		pr.findings = append(pr.findings, doctorFinding{
			plain: fmt.Sprintf("%d old background handoff process(es) are stuck — run `fleet gc --apply` to clear them",
				wedgedDrains),
			verbose: fmt.Sprintf("%d WEDGED fleet drain run-record(s) (gc KindDrainProcs); gc owns reaping", wedgedDrains),
		})
	} else if wedgedErr != nil {
		pr.verboseDetail = append(pr.verboseDetail,
			"drain-scan error (non-fatal): "+wedgedErr.Error())
	}

	return pr
}

// pendingStuckHandoff reports whether project has a pending spawn-fresh queue
// file AND no healthy leader currently holds the lease — i.e. a handoff that
// was requested but never completed. A pending queue file WITH a healthy
// leader is a benign in-flight handoff, not a stuck one.
func pendingStuckHandoff(project string, d doctorDeps) bool {
	paths, err := d.ListPendingQueue()
	if err != nil || len(paths) == 0 {
		return false
	}
	hasForProject := false
	for _, p := range paths {
		req, rerr := d.ReadQueue(p)
		if rerr != nil {
			continue
		}
		if req.Project == project {
			hasForProject = true
			break
		}
	}
	if !hasForProject {
		return false
	}
	// A healthy leader means the handoff is in-flight, not stuck.
	return !d.LeaderPresent(project)
}

// handoffStormCount counts handoff docs whose filename is prefixed with the
// project's coord agent id — a proxy for "how many handoff notes for this
// coord are sitting on disk." More than one un-drained doc is the storm
// symptom. Best-effort: an unreadable handoff dir yields 0.
func handoffStormCount(project string, d doctorDeps) int {
	marker := d.CoordMarker(project)
	if marker == "" {
		return 0
	}
	docs, err := d.HandoffDocs()
	if err != nil {
		return 0
	}
	n := 0
	for _, name := range docs {
		if strings.HasPrefix(name, marker+"-") {
			n++
		}
	}
	return n
}

// doctorRunFix runs the --fix recovery for every project that needs it. Each
// action is streamed to stdout as it runs (surface-don't-silo); the
// per-project report records the planned/refused/errored outcome for the
// final render.
func doctorRunFix(opts doctorOpts, report *doctorReport, stdout, stderr io.Writer) {
	doctorRunFixWith(opts, report, stdout, stderr, defaultDoctorDeps())
}

func doctorRunFixWith(opts doctorOpts, report *doctorReport, stdout, stderr io.Writer, d doctorDeps) {
	d = fillDoctorDeps(d)
	for i := range report.projects {
		pr := &report.projects[i]
		if !pr.status.needsRecovery() {
			continue // healthy / nothing-to-do projects are left alone
		}
		fixOneProject(pr, stdout, stderr, d)
	}
}

// fixOneProject recovers one stuck project. ORDER (invariant 5):
// fence -> STONITH -> acquire/respawn. ALL coord termination funnels through
// the authenticated kill primitive inside TakeOver (invariant 7). It NEVER
// clears a live, heartbeating holder (T21) and leaves an un-authenticatable
// fenced_not_acquired state for operator-confirmed recovery (T32).
func fixOneProject(pr *doctorProjectReport, stdout, stderr io.Writer, d doctorDeps) {
	project := pr.project

	// GUARD (T21): re-check liveness right before acting. If a healthy leader
	// is heartbeating now, REFUSE — never steal a live coordinator.
	if d.LeaderPresent(project) {
		pr.fixRefused = "the coordinator is live and responding; leaving it alone"
		pr.verboseDetail = append(pr.verboseDetail,
			"refused: LeaderPresent=true (healthy heartbeating holder) — never clear a live lease")
		_, _ = fmt.Fprintf(stdout, "Project %s: coordinator is responding; nothing to recover.\n", project)
		return
	}

	// Find the coord record to respawn from. Cache it BEFORE the takeover —
	// the takeover's kill archives the record, so a post-kill load would race
	// the archive (same reason drainWaitBarrierOrEscalate caches it).
	cachedOld := cacheCoordRecord(project, d)

	// Cache the pending handoff request (codex PR6 [P1]): a stuck handoff left
	// a spawn-fresh queue file naming the handoff doc + the pre-allocated
	// successor id. The recovery MUST carry those into RecoverSpawn — without
	// the doc the successor comes up IDLE, and without the pre-allocated id it
	// can't ADOPT an already-spawned successor (it would re-spawn / collide).
	// Read it before the takeover so we have the metadata even though OLD's
	// record gets archived. queuePath is the file to delete once the recovery
	// fulfills the request (so a later drain doesn't re-process it).
	queuePath, queueReq := pendingQueueForProject(project, d)

	pr.fixPlanned = true
	_, _ = fmt.Fprintf(stdout,
		"Project %s: %s. Plan: stop the stuck coordinator, then start a fresh one.\n",
		project, pr.status.plainStatusLine())

	// STEP 1+2: fence -> STONITH -> acquire, via the safety-net takeover. The
	// authenticated kill (invariant 7) lives inside d.TakeOver; the doctor
	// never raw-kills.
	_, _ = fmt.Fprintf(stdout, "  - Stopping the stuck coordinator.\n")
	acquired, err := d.TakeOver(project, coordAgentIDForFix(cachedOld, project))
	if err != nil {
		// A fenced_not_acquired the takeover could not authenticate/kill —
		// leave the typed state, surface it, offer operator-confirmed recovery
		// (T32). NO silent stall, NO second leader.
		pr.fixErr = err
		pr.verboseDetail = append(pr.verboseDetail, "takeover error: "+err.Error())
		_, _ = fmt.Fprintf(stderr,
			"Project %s: could not safely stop the stuck coordinator. It may be in an ambiguous state. "+
				"Rerun `fleet doctor --project %s --fix` once it settles, or check `fleet status`.\n",
			project, project)
		return
	}
	if !acquired {
		// The takeover stood down (a healthy holder appeared, or it could not
		// confirm the old coord is gone). Do NOT respawn (would duplicate).
		pr.fixRefused = "the coordinator started responding again; leaving it alone"
		pr.verboseDetail = append(pr.verboseDetail,
			"takeover acquired=false — healthy holder reappeared or old coord un-killable; not respawning (would duplicate)")
		_, _ = fmt.Fprintf(stdout, "  - The coordinator started responding again; not restarting.\n")
		return
	}

	// STEP 3: respawn a fresh lease-wrapped successor from the cached record,
	// via the EXISTING productionRecoverSpawn path (RecoverDeadCoord). It
	// resumes from the last clean checkpoint + intent replay internally.
	if cachedOld == nil {
		// We fenced+killed a hung coord but have no record to respawn from —
		// surface a concrete next step rather than spawning blind (T32 spirit).
		pr.fixErr = fmt.Errorf("doctor: stopped the stuck coordinator for %s but its record was gone; cannot restart it automatically", project)
		_, _ = fmt.Fprintf(stderr,
			"Project %s: stopped the stuck coordinator but couldn't find its record to restart it. "+
				"Run `fleet dispatch --coord-spawn` (or press [a] in the TUI) to bring up a replacement.\n", project)
		return
	}
	_, _ = fmt.Fprintf(stdout, "  - Starting a fresh coordinator.\n")
	// Carry the queued handoff metadata (codex PR6 [P1]): the doc so the
	// successor resumes the in-flight work (not idle), and the pre-allocated
	// successor id so RecoverSpawn ADOPTS an already-spawned replacement
	// instead of colliding. effectiveDisableAutoResume resolves the queue's
	// per-handoff override against OLD's baseline, exactly as the drain path.
	docPath := queueReq.HandoffDoc
	preAllocatedID := queueReq.NewAgentID
	disableAutoResume := effectiveDisableAutoResume(queueReq, cachedOld)
	if rerr := d.RecoverSpawn(cachedOld, docPath, preAllocatedID, disableAutoResume, stdout, stderr); rerr != nil {
		pr.fixErr = rerr
		pr.verboseDetail = append(pr.verboseDetail, "recover-spawn error: "+rerr.Error())
		_, _ = fmt.Fprintf(stderr,
			"Project %s: stopped the stuck coordinator but starting a fresh one failed: %v. "+
				"Run `fleet dispatch --coord-spawn` to retry.\n", project, rerr)
		return
	}
	// The recovery fulfilled the handoff — delete the pending queue file so a
	// later drain doesn't re-process it (a delete failure is surfaced, not
	// silent: a lingering queue file would re-spawn). Skip when there was no
	// pending request (a hung coord with no queued handoff).
	if queuePath != "" {
		if derr := d.DeleteQueue(queuePath); derr != nil {
			pr.verboseDetail = append(pr.verboseDetail, "queue delete failed: "+derr.Error())
			_, _ = fmt.Fprintf(stderr,
				"Project %s: recovered the coordinator but couldn't clear the pending handoff file (%v); "+
					"rerun `fleet drain` to clean it.\n", project, derr)
		}
	}
	pr.fixActions = []string{
		"Stopped the stuck coordinator.",
		"Started a fresh coordinator.",
	}
	_, _ = fmt.Fprintf(stdout, "Project %s: recovery complete.\n", project)
}

// pendingQueueForProject returns the first pending spawn-fresh queue file
// (path + parsed request) for project, or ("", zero) if none. The recovery
// uses it to carry the queued handoff doc + pre-allocated successor id into
// the respawn so the successor resumes the in-flight work instead of coming
// up idle (codex PR6 [P1]). Best-effort + read-only.
func pendingQueueForProject(project string, d doctorDeps) (string, queue.SpawnFresh) {
	paths, err := d.ListPendingQueue()
	if err != nil {
		return "", queue.SpawnFresh{}
	}
	for _, p := range paths {
		req, rerr := d.ReadQueue(p)
		if rerr == nil && req.Project == project {
			return p, req
		}
	}
	return "", queue.SpawnFresh{}
}

// cacheCoordRecord loads the project's coord agent record (via the coord-spawn
// marker, falling back to a TaskID scan) BEFORE a takeover archives it.
// Returns nil when no record can be found (the fixer then surfaces a manual
// recovery step rather than spawning blind).
func cacheCoordRecord(project string, d doctorDeps) *agent.Record {
	if marker := d.CoordMarker(project); marker != "" {
		if rec, err := d.LoadAgent(marker); err == nil && rec != nil {
			return rec
		}
	}
	recs, err := d.ListAgents()
	if err != nil {
		return nil
	}
	for _, r := range recs {
		if r != nil && isCoordAgentRecord(r) && r.Project == project {
			return r
		}
	}
	return nil
}

// coordAgentIDForFix returns the agent id the takeover should authenticate
// against: the cached coord record's id when known, else a synthetic
// project-scoped id (the takeover's kill re-validates identity regardless, so
// an empty/synthetic id only affects diagnostics).
func coordAgentIDForFix(cachedOld *agent.Record, project string) string {
	if cachedOld != nil {
		return cachedOld.ID
	}
	return "doctor-fix-" + project
}

// stuckHandoffStatusFn is the package-level seam for the status surface's
// stuck-handoff scan (so the status test can inject canned deps). Production
// wraps defaultDoctorDeps().
var stuckHandoffStatusFn = func() doctorDeps { return defaultDoctorDeps() }

// emitStuckHandoffSection prints the canonical plain-English stuck-handoff
// line for every project that has a pending coordinator handoff with no live
// leader. Called from runStatus (status.go). Plain output only — no jargon
// (the words live in `fleet doctor --verbose`). Best-effort + read-only: any
// scan error degrades to silence rather than breaking the status table.
func emitStuckHandoffSection(stdout, stderr io.Writer) {
	d := fillDoctorDeps(stuckHandoffStatusFn())
	projects, err := stuckHandoffProjects(d)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: stuck-handoff scan failed: %v (continuing)\n", err)
		return
	}
	for _, p := range projects {
		_, _ = fmt.Fprintf(stdout,
			"%s [%s]\n", doctorStuckHandoffLine, p)
	}
}

// doctorStuckHandoffLine is the canonical operator-approved stuck-handoff
// message (no jargon). Shared by the status surface and the doctor's
// handoff-stuck status so the wording never drifts.
const doctorStuckHandoffLine = "Fleet isn't responding — the handoff to a fresh coordinator didn't complete. " +
	"Run `fleet doctor` to recover."

// stuckHandoffProjects returns the distinct projects with a pending coord
// handoff (a spawn-fresh queue file) AND no live leader — the wedged-handoff
// set. Read-only.
func stuckHandoffProjects(d doctorDeps) ([]string, error) {
	paths, err := d.ListPendingQueue()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		req, rerr := d.ReadQueue(p)
		if rerr != nil || req.Project == "" || seen[req.Project] {
			continue
		}
		if d.LeaderPresent(req.Project) {
			continue // in-flight handoff, not stuck
		}
		seen[req.Project] = true
		out = append(out, req.Project)
	}
	return out, nil
}

func fillDoctorDeps(d doctorDeps) doctorDeps {
	def := defaultDoctorDeps()
	if d.Diagnose == nil {
		d.Diagnose = def.Diagnose
	}
	if d.LeaderPresent == nil {
		d.LeaderPresent = def.LeaderPresent
	}
	if d.ListAgents == nil {
		d.ListAgents = def.ListAgents
	}
	if d.LoadAgent == nil {
		d.LoadAgent = def.LoadAgent
	}
	if d.CoordMarker == nil {
		d.CoordMarker = def.CoordMarker
	}
	if d.SessionAlive == nil {
		d.SessionAlive = def.SessionAlive
	}
	if d.ListPendingQueue == nil {
		d.ListPendingQueue = def.ListPendingQueue
	}
	if d.ReadQueue == nil {
		d.ReadQueue = def.ReadQueue
	}
	if d.DeleteQueue == nil {
		d.DeleteQueue = def.DeleteQueue
	}
	if d.HandoffDocs == nil {
		d.HandoffDocs = def.HandoffDocs
	}
	if d.LeaseProjects == nil {
		d.LeaseProjects = def.LeaseProjects
	}
	if d.WedgedDrains == nil {
		d.WedgedDrains = def.WedgedDrains
	}
	if d.TakeOver == nil {
		d.TakeOver = def.TakeOver
	}
	if d.RecoverSpawn == nil {
		d.RecoverSpawn = def.RecoverSpawn
	}
	if d.Self == nil {
		d.Self = def.Self
	}
	return d
}

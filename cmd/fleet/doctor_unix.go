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
	"github.com/edisonshen/fleet/internal/spawn"
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
	// LoadAgent loads one LIVE agent record. Production: agent.Load.
	LoadAgent func(id string) (*agent.Record, error)
	// LoadArchive loads an ARCHIVED agent record (the old coord whose record
	// was archived when its handoff retired it). Lets the recovery respawn
	// from the queued OldAgentID even when only the queue file remains.
	// Production: agent.LoadArchive.
	LoadArchive func(id string) (*agent.Record, error)
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
	// LockAgent takes the BOUNDED per-agent lock so a concurrent doctor/drain
	// recovery can't both spawn a successor after the (lease-releasing)
	// takeover. Returns a release func. Production: state.LockAgent.
	LockAgent func(agentID string) (func(), error)
	// QueueExists reports whether a queue file is still on disk (the
	// post-takeover re-check: a peer recovery may have already deleted it).
	// Production: an os.Stat. nil when there is no queue (a hung coord with no
	// pending handoff) — the caller skips the re-check.
	QueueExists func(path string) bool
	// Self is the caller's pid (never reap self). Production: os.Getpid.
	Self func() int
}

func defaultDoctorDeps() doctorDeps {
	return doctorDeps{
		Diagnose:         coordlock.Diagnose,
		LeaderPresent:    coordlock.LeaderPresent,
		ListAgents:       agent.List,
		LoadAgent:        agent.Load,
		LoadArchive:      agent.LoadArchive,
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
		LockAgent:    state.LockAgent,
		QueueExists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		Self: os.Getpid,
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
			if req, rerr := d.ReadQueue(p); rerr == nil && isCoordHandoffQueue(req) {
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

// isCoordAgentRecord reports whether a record is THE project coordinator. It
// uses the EXACT spawn.IsCoordSpawn discriminator (TaskID == "coord-"+Project),
// not a bare prefix check (codex PR6 iter-6 [P2]): a worker task legitimately
// named e.g. "coord-cache-warm" in project "ops" prefix-matches "coord-" but
// is NOT the coordinator — selecting it as the old coord would make --fix
// respawn from a WORKER record instead of a lease-wrapped coordinator.
func isCoordAgentRecord(r *agent.Record) bool {
	return spawn.IsCoordSpawn(r.TaskID, r.Project)
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

	// LEGACY MODE (codex PR6 iter-9 [P2]): with FLEET_LEASE_FAILOVER=0 there is
	// no coordinator lease, so coordlock.Diagnose always returns None — which
	// must NOT be rendered as "no coordinator is running" when a live legacy
	// coord record exists. The lease-based diagnosis/recovery doesn't apply;
	// report the legacy mode + the coord's session liveness instead.
	if !coordlock.FailoverEnabled() {
		return diagnoseLegacyProject(project, d)
	}

	diag := d.Diagnose(project)

	// Headline status from the lease classification.
	switch diag.Health {
	case coordlock.LeaseHealthOK, coordlock.LeaseHealthBooting:
		// OK = a heartbeating leader; Booting = a fresh holder mid-startup (a
		// leader is coming). Neither is stuck (codex PR6 iter-16 [P2]).
		pr.status = doctorStatusHealthy
	case coordlock.LeaseHealthHung:
		pr.status = doctorStatusUnresponsive
	case coordlock.LeaseHealthDead:
		pr.status = doctorStatusDead
	case coordlock.LeaseHealthReleased:
		// A `released` lease is a coord that exited CLEANLY (Release writes
		// `released` before coord.Cleanup archives the record). This is NOT a
		// stuck coord to recover (codex PR6 iter-3 [P2]): respawning during the
		// clean-shutdown window would resurrect a coord that meant to stop.
		// Treat it as "no active coordinator"; only a separate stuck signal (a
		// pending coord handoff, below) escalates it to a recoverable status.
		pr.status = doctorStatusNone
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
			case !alive && pr.status != doctorStatusHealthy:
				// Session confirmed gone AND the lease is NOT healthy -> a real
				// stopped coord. Surface it; the headline (dead/none/hung) already
				// drives recovery.
				pr.findings = append(pr.findings, doctorFinding{
					plain:   "the coordinator's terminal session is gone",
					verbose: "tmux session " + rec.TmuxSession + " for coord " + marker + " is not alive",
				})
			case !alive:
				// The lease is HEALTHY (live supervisor heartbeating within TTL)
				// yet THIS record's session is gone — almost always a STALE
				// TmuxSession on the marker record (the live coord runs on a
				// different session), NOT a stuck coord (codex PR6 iter-4 [P2]).
				// Do NOT downgrade to Dead: the read-only path would then advise
				// --fix, but --fix correctly REFUSES a live holder, so the
				// "remedy" could never run. Keep the healthy verdict (the lease's
				// TTL + pid liveness is the stronger signal) and note the stale
				// session only under --verbose.
				pr.verboseDetail = append(pr.verboseDetail,
					"note: recorded tmux session "+rec.TmuxSession+" for coord "+marker+
						" is not alive, but the lease is healthy (likely a stale session field; not a stuck coord)")
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

// diagnoseLegacyProject builds the read-only report for a project when
// FLEET_LEASE_FAILOVER is OFF (codex PR6 iter-9 [P2]). There is no lease, so
// the lease-based health/recovery doesn't apply — `--fix` would refuse anyway
// (coordlock.AcquireLeaseWithKill returns ErrFailoverDisabled). Report the
// legacy mode + the coord's session liveness rather than the misleading "no
// coordinator is running" the None classification would otherwise render.
func diagnoseLegacyProject(project string, d doctorDeps) doctorProjectReport {
	pr := doctorProjectReport{project: project, status: doctorStatusLegacy}
	pr.verboseDetail = append(pr.verboseDetail,
		"FLEET_LEASE_FAILOVER is off: no coordinator lease in play; lease-based recovery does not apply")
	if marker := d.CoordMarker(project); marker != "" {
		if rec, lerr := d.LoadAgent(marker); lerr == nil && rec != nil {
			pr.verboseDetail = append(pr.verboseDetail, "legacy coord record: "+rec.ID)
			if rec.TmuxSession != "" {
				if alive, serr := d.SessionAlive(rec.TmuxSession); serr == nil && !alive {
					pr.findings = append(pr.findings, doctorFinding{
						plain:   "the coordinator's terminal session is gone",
						verbose: "tmux session " + rec.TmuxSession + " for coord " + marker + " is not alive",
					})
				}
			}
		}
	}
	return pr
}

// pendingStuckHandoff reports whether project has a pending spawn-fresh queue
// file AND no healthy leader currently holds the lease — i.e. a handoff that
// was requested but never completed. A pending queue file WITH a healthy
// leader is a benign in-flight handoff, not a stuck one.
func pendingStuckHandoff(project string, d doctorDeps) bool {
	// With failover off there is no lease, so LeaderPresent is always false and
	// a benign legacy handoff would look stuck (codex PR6 iter-5 [P2]). The
	// lease-aware stuck signal doesn't apply in legacy mode.
	if !coordlock.FailoverEnabled() {
		return false
	}
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
		if req.Project == project && isCoordHandoffQueue(req) {
			hasForProject = true
			break
		}
	}
	if !hasForProject {
		return false
	}
	// A healthy leader means the handoff is in-flight, not stuck. Use the
	// READ-ONLY Diagnose-based probe, NOT LeaderPresent (codex PR6 iter-15
	// [P2]): production LeaderPresent's missing-epoch path creates
	// coordinator.flock (O_CREATE), which a read-only diagnosis/status scan
	// must never do.
	return !leaderHealthyReadOnly(project, d)
}

// leaderHealthyReadOnly reports whether a HEALTHY leader holds the lease,
// using the read-only coordlock.Diagnose accessor (LeaseHealthOK) instead of
// LeaderPresent. Diagnose never creates lock files (its missing-epoch probe is
// the non-creating flockBusyReadOnly), so it is safe on the read-only
// status/diagnosis path. Used by the stuck-handoff scan; the --fix guards
// keep using LeaderPresent (they run on a mutating path anyway).
func leaderHealthyReadOnly(project string, d doctorDeps) bool {
	switch d.Diagnose(project).Health {
	case coordlock.LeaseHealthOK, coordlock.LeaseHealthBooting:
		// OK = a heartbeating leader; Booting = a fresh holder mid-startup
		// (a leader is coming). Both mean "not a stuck handoff" (codex PR6
		// iter-16 [P2]) — mirrors LeaderPresent's busy-flock booting check.
		return true
	default:
		return false
	}
}

// isCoordHandoffQueue reports whether a pending spawn-fresh request is a
// COORDINATOR handoff (not a generic worker handoff). queue.SpawnFresh is
// shared by worker + coord handoffs, so the doctor MUST filter to coord
// queues (codex PR6 iter-3 [P1]): selecting a worker queue would feed the
// worker's doc/successor-id into the COORD recovery spawn and then delete the
// worker queue — wrong resume context + a dropped worker handoff. The
// discriminator is the SAME spawn.IsCoordSpawn the drain router uses.
func isCoordHandoffQueue(req queue.SpawnFresh) bool {
	return spawn.IsCoordSpawn(req.TaskID, req.Project)
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

	// Cache the pending handoff request FIRST (codex PR6 [P1]): a stuck handoff
	// left a spawn-fresh queue file naming the handoff doc + the pre-allocated
	// successor id. The recovery MUST carry those into RecoverSpawn — without
	// the doc the successor comes up IDLE, and without the pre-allocated id it
	// can't ADOPT an already-spawned successor (it would re-spawn / collide).
	// queuePath is the file to delete once the recovery fulfills the request
	// (so a later drain doesn't re-process it).
	queuePath, queueReq := pendingQueueForProject(project, d)

	// Find the coord record to respawn from. Cache it BEFORE the takeover —
	// the takeover's kill archives the record, so a post-kill load would race
	// the archive (same reason drainWaitBarrierOrEscalate caches it). Two
	// fallbacks for the record-gone cases:
	//   - queueReq.OldAgentID for the queue-only stuck case (codex PR6 iter-7);
	//   - the lease epoch's OwnerAgentID for the LEASE-ONLY case (only a
	//     lingering coordinator.epoch — no marker, no record, no queue), read
	//     fresh here so --fix can restart from the OLD owner's archived record
	//     instead of dead-ending at cachedOld==nil (codex PR6 iter-12 [P2]).
	leaseOwnerID := d.Diagnose(project).OwnerAgentID
	cachedOld := cacheCoordRecord(project, queueReq, leaseOwnerID, d)

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

	// SERIALIZE the post-takeover recovery (codex PR6 iter-17 [P1]): the
	// production TakeOver RELEASES the lease the instant OLD is proven gone, so
	// two concurrent doctor/drain recoveries can BOTH observe acquired=true.
	// Take the SHORT bounded per-agent lock (the SAME lock the drain path uses)
	// so only ONE proceeds to RecoverSpawn + queue delete; the loser sees the
	// queue gone (re-check below) and stands down. The lock is taken AFTER the
	// takeover (never across it — OLD's dying coord.Cleanup needs this same
	// lock to archive its record). Lock the OLD agent id so doctor + drain
	// contend on the SAME key.
	lockID := postTakeoverLockID(cachedOld, queueReq, project)
	release, lerr := d.LockAgent(lockID)
	if lerr != nil {
		pr.fixErr = fmt.Errorf("doctor: stopped the stuck coordinator for %s but could not lock for recovery: %w", project, lerr)
		pr.verboseDetail = append(pr.verboseDetail, "lock-agent error: "+lerr.Error())
		_, _ = fmt.Fprintf(stderr,
			"Project %s: stopped the stuck coordinator but couldn't serialize the restart (%v); rerun `fleet doctor --fix`.\n",
			project, lerr)
		return
	}
	defer release()

	// Under the lock, re-check the queue: a peer recovery that also won an
	// acquire may have already RecoverSpawned + deleted it. If it is gone, the
	// handoff is fulfilled — stand down (recovering again would duplicate).
	if queuePath != "" && !d.QueueExists(queuePath) {
		pr.fixActions = []string{
			"Stopped the stuck coordinator.",
			"Another recovery already restarted the coordinator; left it in place.",
		}
		pr.verboseDetail = append(pr.verboseDetail,
			"post-takeover: pending handoff already cleared by a concurrent recovery; standing down")
		_, _ = fmt.Fprintf(stdout, "  - Another recovery already restarted the coordinator; nothing more to do.\n")
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
				"Run `fleet dispatch coord-%s --project %s --coord-spawn` (or press [a] in the TUI) to bring up a replacement.\n",
			project, project, project)
		return
	}
	// RE-CHECK for a healthy successor BEFORE spawning (codex PR6 iter-14
	// [P1]): the production TakeOver RELEASES the lease the instant the OLD
	// holder is proven gone, so between that release and here a WARM STANDBY or
	// a concurrent `fleet drain` recovery may have already acquired the lease
	// and become the new leader. Spawning now would create a REDUNDANT
	// coordinator (or collide on the pre-allocated id). If a healthy leader is
	// present, the handoff is already complete — clean the queue and stand
	// down, mirroring the drain path's post-takeover healthySuccessorPresent
	// guard. (LeaderPresent is the live TTL + pid-liveness check; a successor
	// that crashed after writing its epoch is NOT healthy, so we still recover.)
	if d.LeaderPresent(project) {
		pr.fixActions = []string{
			"Stopped the stuck coordinator.",
			"A fresh coordinator was already taking over; left it in place.",
		}
		pr.verboseDetail = append(pr.verboseDetail,
			"post-takeover: a healthy successor acquired the lease (standby/concurrent drain); not respawning (would duplicate)")
		_, _ = fmt.Fprintf(stdout, "  - A fresh coordinator is already taking over; not starting another.\n")
		if queuePath != "" {
			if derr := d.DeleteQueue(queuePath); derr != nil {
				pr.fixErr = fmt.Errorf("doctor: a successor took over %s but the pending handoff file %s could not be cleared: %w",
					project, queuePath, derr)
				pr.verboseDetail = append(pr.verboseDetail, "queue delete failed: "+derr.Error())
				_, _ = fmt.Fprintf(stderr,
					"Project %s: a successor took over but the pending handoff file couldn't be cleared (%v); "+
						"rerun `fleet drain` to clean it.\n", project, derr)
			}
		}
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
				"Run `fleet dispatch coord-%s --project %s --coord-spawn` to retry.\n", project, rerr, project, project)
		return
	}
	// The recovery fulfilled the handoff — delete the pending queue file so a
	// later drain doesn't re-process the ALREADY-fulfilled handoff. A delete
	// failure is a RECOVERY ERROR, not just a warning (codex PR6 iter-4 [P2]):
	// leaving the stale queue file means a later `fleet drain` re-spawns for a
	// handoff that is already done, so the caller must NOT see "complete" + a
	// zero exit. Skip the delete only when there was no pending request (a hung
	// coord with no queued handoff).
	if queuePath != "" {
		if derr := d.DeleteQueue(queuePath); derr != nil {
			pr.fixErr = fmt.Errorf("doctor: recovered %s but could not clear the fulfilled handoff file %s: %w "+
				"(a later drain may re-spawn for it)", project, queuePath, derr)
			pr.verboseDetail = append(pr.verboseDetail, "queue delete failed: "+derr.Error())
			_, _ = fmt.Fprintf(stderr,
				"Project %s: recovered the coordinator but couldn't clear the pending handoff file (%v); "+
					"rerun `fleet drain` to clean it before it re-spawns.\n", project, derr)
			return
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
		if rerr == nil && req.Project == project && isCoordHandoffQueue(req) {
			return p, req
		}
	}
	return "", queue.SpawnFresh{}
}

// cacheCoordRecord loads the project's coord agent record to respawn from
// BEFORE a takeover archives it, trying in order:
//  1. the pending queue's OldAgentID — LIVE then ARCHIVE, FIRST when a coord
//     handoff is pending (codex PR6 iter-19 [P1]). A pending handoff may have
//     already moved the coord-spawn marker to NewAgentID, so the OUTGOING coord
//     the queue names — not the marker's preallocated successor — is the
//     correct recovery source; trusting the marker first would respawn from the
//     stale replacement (corrupting lineage / colliding on preAllocatedID).
//  2. the coord-spawn marker's LIVE record (validated as this project's coord);
//  3. a LIVE-records TaskID scan;
//  4. the lease epoch's OwnerAgentID — LIVE then ARCHIVE (codex PR6 iter-12
//     [P2]). The LEASE-ONLY stuck case (only a lingering coordinator.epoch —
//     no marker, no record, no queue) still names the OLD owner in the epoch.
//
// Every candidate is validated to be THIS project's coordinator (a stale id /
// worker record must never respawn). queueReq is the pending handoff request
// (zero value if none); leaseOwnerID is the lease epoch's owner agent id (empty
// if none). Returns nil only when no record can be found anywhere (the fixer
// then surfaces a manual recovery step rather than spawning blind).
func cacheCoordRecord(project string, queueReq queue.SpawnFresh, leaseOwnerID string, d doctorDeps) *agent.Record {
	// 1. The QUEUE's outgoing coord FIRST (the recovery source for a pending
	// handoff) — live then archive, validated.
	if queueReq.OldAgentID != "" {
		if rec := loadCoordRecordByID(queueReq.OldAgentID, project, d); rec != nil {
			return rec
		}
	}
	// 2. The coord-spawn marker's record (validated — a STALE marker can point
	// at a non-coordinator / other-project record; codex PR6 iter-10 [P2]).
	if marker := d.CoordMarker(project); marker != "" {
		if rec, err := d.LoadAgent(marker); err == nil && rec != nil &&
			rec.Project == project && isCoordAgentRecord(rec) {
			return rec
		}
	}
	// 3. A live-records TaskID scan.
	if recs, err := d.ListAgents(); err == nil {
		for _, r := range recs {
			if r != nil && isCoordAgentRecord(r) && r.Project == project {
				return r
			}
		}
	}
	// 4. The lease epoch's recorded owner id — the lease-only fallback (codex
	// PR6 iter-12 [P2]).
	if leaseOwnerID != "" {
		if rec := loadCoordRecordByID(leaseOwnerID, project, d); rec != nil {
			return rec
		}
	}
	return nil
}

// loadCoordRecordByID loads agent id (LIVE then ARCHIVE) and returns it ONLY
// if it is THIS project's coordinator (rec.Project==project &&
// isCoordAgentRecord) — a stale queue/lease owner id must never respawn a
// worker or another project's record (same validation the marker path does,
// codex PR6 iter-10/iter-12 [P2]). Returns nil otherwise.
func loadCoordRecordByID(id, project string, d doctorDeps) *agent.Record {
	for _, load := range []func(string) (*agent.Record, error){d.LoadAgent, d.LoadArchive} {
		if rec, err := load(id); err == nil && rec != nil &&
			rec.Project == project && isCoordAgentRecord(rec) {
			return rec
		}
	}
	return nil
}

// postTakeoverLockID returns the per-agent lock key the doctor recovery
// contends on with a concurrent drain/doctor (codex PR6 iter-17 [P1]). It must
// match the OLD agent id the drain path locks (state.LockAgent(req.OldAgentID))
// so the two serialize on the SAME key: prefer the cached coord record id, then
// the pending queue's OldAgentID, then a project-scoped fallback.
func postTakeoverLockID(cachedOld *agent.Record, queueReq queue.SpawnFresh, project string) string {
	// Prefer the QUEUE's OldAgentID whenever a coord handoff is pending (codex
	// PR6 iter-19 [P1]): the drain path locks on req.OldAgentID, so doctor MUST
	// use the SAME key for queued handoffs — else a concurrent doctor + drain
	// take DIFFERENT locks (the marker/cache may point at the preallocated
	// successor) and both pass the under-lock queue re-check, double-spawning.
	if queueReq.OldAgentID != "" {
		return queueReq.OldAgentID
	}
	if cachedOld != nil && cachedOld.ID != "" {
		return cachedOld.ID
	}
	return "doctor-fix-" + project
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
	// Only meaningful with the lease failover ON (codex PR6 iter-5 [P2]): with
	// FLEET_LEASE_FAILOVER=0 legacy coords hold NO lease, so LeaderPresent is
	// always false and EVERY pending handoff would look "stuck" — pointing the
	// operator at `fleet doctor`, which can't recover with failover off. The
	// lease-aware stuck-handoff signal simply doesn't apply in legacy mode.
	if !coordlock.FailoverEnabled() {
		return nil, nil
	}
	paths, err := d.ListPendingQueue()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		req, rerr := d.ReadQueue(p)
		if rerr != nil || req.Project == "" || seen[req.Project] || !isCoordHandoffQueue(req) {
			continue
		}
		// READ-ONLY leader probe (codex PR6 iter-15 [P2]): the status surface
		// must not create lock files, so use Diagnose-based health, not the
		// flock-creating LeaderPresent.
		if leaderHealthyReadOnly(req.Project, d) {
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
	if d.LoadArchive == nil {
		d.LoadArchive = def.LoadArchive
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
	if d.LockAgent == nil {
		d.LockAgent = def.LockAgent
	}
	if d.QueueExists == nil {
		d.QueueExists = def.QueueExists
	}
	if d.Self == nil {
		d.Self = def.Self
	}
	return d
}

//go:build linux || darwin

package main

// doctor_test.go — `fleet doctor` PR6 (DESIGN-handoff-drain-storm-leak).
//
//	T19  read-only diagnosis of a HUNG leader (pid alive, heartbeat frozen):
//	     reports "not responding" + a remedy; mutates NOTHING.
//	T20  --fix recovers a stale lease (holder gone) + pending handoff:
//	     takes over, respawns from the cached record, surfaces each action.
//	T21  --fix REFUSES a live, heartbeating holder — never clears it.
//	T32  fenced_not_acquired is surfaced as its own diagnosis; --fix that
//	     can't authenticate the kill leaves the typed state + offers
//	     operator-confirmed recovery (no silent stall, no 2nd leader).
//
// (T22 — the no-jargon user-surface assertions — lives in
// doctor_surface_test.go, which is all-platform so the renderer is tested
// without the unix-only lease machinery.)
//
// All deterministic: the lease/kill/spawn seams are injected via doctorDeps
// (no real flock, no real signals, no time.Sleep). Every test that touches
// the agent store uses an isolated FLEET_HOME with t.Cleanup-reaped temp.

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/queue"
)

// doctorTestDeps returns a doctorDeps with every seam defaulted to a
// "nothing here" stub so a test overrides only the facts it cares about.
// FLEET_HOME isolation is the caller's job (most diagnosis tests don't need
// the real store because every dep is injected).
func doctorTestDeps() doctorDeps {
	return doctorDeps{
		Diagnose: func(string) coordlock.LeaseDiagnosis {
			return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthNone}
		},
		LeaderPresent:    func(string) bool { return false },
		ListAgents:       func() ([]*agent.Record, error) { return nil, nil },
		LoadAgent:        func(string) (*agent.Record, error) { return nil, errors.New("no record") },
		CoordMarker:      func(string) string { return "" },
		SessionAlive:     func(string) (bool, error) { return true, nil },
		ListPendingQueue: func() ([]string, error) { return nil, nil },
		ReadQueue:        func(string) (queue.SpawnFresh, error) { return queue.SpawnFresh{}, nil },
		DeleteQueue:      func(string) error { return nil },
		HandoffDocs:      func() ([]string, error) { return nil, nil },
		LeaseProjects:    func() ([]string, error) { return nil, nil },
		WedgedDrains:     func() (int, error) { return 0, nil },
		TakeOver:         func(string, string) (bool, error) { return false, nil },
		RecoverSpawn:     func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { return nil },
		Self:             func() int { return 4242 },
	}
}

// --- T19: read-only hung-leader diagnosis, mutates nothing ---

func TestDoctor_T19_DiagnoseHungLeader_ReadOnly(t *testing.T) {
	const project = "t19proj"
	d := doctorTestDeps()
	// A HUNG leader: pid alive but the heartbeat is frozen past TTL. Diagnose
	// classifies it LeaseHealthHung (the SAME verdict the acquire path gives).
	d.Diagnose = func(p string) coordlock.LeaseDiagnosis {
		if p != project {
			t.Fatalf("Diagnose called for wrong project %q", p)
		}
		return coordlock.LeaseDiagnosis{
			Health: coordlock.LeaseHealthHung, HasRecord: true,
			Epoch: 7, State: "active", OwnerPID: 999, OwnerAlive: true,
		}
	}
	// Mutation tripwires: a read-only doctor must call NONE of these.
	var mutated int32
	d.TakeOver = func(string, string) (bool, error) { atomic.AddInt32(&mutated, 1); return false, nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
		atomic.AddInt32(&mutated, 1)
		return nil
	}

	report, err := gatherDoctorReportWith(doctorOpts{project: project}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(report.projects) != 1 {
		t.Fatalf("want 1 project report, got %d", len(report.projects))
	}
	pr := report.projects[0]
	if pr.status != doctorStatusUnresponsive {
		t.Fatalf("status = %v, want Unresponsive", pr.status)
	}
	if atomic.LoadInt32(&mutated) != 0 {
		t.Fatalf("read-only doctor mutated state (%d calls)", mutated)
	}

	// The rendered plain output names the symptom + the remedy.
	var out bytes.Buffer
	renderDoctorReport(doctorOpts{project: project}, report, &out)
	s := out.String()
	if !strings.Contains(s, "not responding") {
		t.Errorf("plain output missing 'not responding'; got:\n%s", s)
	}
	if !strings.Contains(s, "fleet doctor") || !strings.Contains(s, "--fix") {
		t.Errorf("plain output missing the remedy hint; got:\n%s", s)
	}
}

// --- T20: --fix recovers a stale lease + pending handoff ---

func TestDoctor_T20_FixRecoversStaleLease(t *testing.T) {
	const (
		project = "t20proj"
		coordID = "t20coord"
	)
	old := agent.New(coordID)
	old.Project = project
	old.TaskID = CoordTaskIDPrefix + project
	old.SupervisorPID = 12345
	old.Command = []string{"claude"}

	d := doctorTestDeps()
	// DEAD holder (pid gone) -> stealable. A pending handoff queue file exists.
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{
			Health: coordlock.LeaseHealthDead, HasRecord: true,
			Epoch: 3, State: "active", OwnerPID: 12345, OwnerAlive: false,
		}
	}
	d.CoordMarker = func(string) string { return coordID }
	d.LoadAgent = func(id string) (*agent.Record, error) {
		if id == coordID {
			return old, nil
		}
		return nil, errors.New("no record")
	}
	const (
		queuePath = "/q/spawn-fresh-" + coordID + ".json"
		handoffMd = "/tmp/handoff-t20.md"
		newID     = "t20successor"
	)
	d.ListPendingQueue = func() ([]string, error) { return []string{queuePath}, nil }
	d.ReadQueue = func(string) (queue.SpawnFresh, error) {
		// A real pending COORD handoff: it names the doc to resume from + the
		// pre-allocated successor id the recovery must carry (codex PR6 [P1]).
		// TaskID = coord- + project so isCoordHandoffQueue accepts it (codex
		// PR6 iter-3 [P1] — a worker queue must NOT be picked).
		return queue.SpawnFresh{
			OldAgentID: coordID, Project: project, TaskID: CoordTaskIDPrefix + project,
			HandoffDoc: handoffMd, NewAgentID: newID,
		}, nil
	}
	var deleted int32
	d.DeleteQueue = func(p string) error {
		atomic.AddInt32(&deleted, 1)
		if p != queuePath {
			t.Errorf("DeleteQueue path = %q, want %q", p, queuePath)
		}
		return nil
	}
	// LeaderPresent is false throughout (dead lease). Track the takeover +
	// respawn so we assert the fence->kill->respawn order ran, AND that the
	// queued handoff metadata (doc + pre-allocated id) reached RecoverSpawn.
	var tookOver, respawned int32
	d.TakeOver = func(p, a string) (bool, error) {
		atomic.AddInt32(&tookOver, 1)
		if p != project {
			t.Errorf("TakeOver project = %q, want %q", p, project)
		}
		return true, nil // acquired: old holder was gone, takeover fenced+killed it
	}
	d.RecoverSpawn = func(rec *agent.Record, doc string, preID string, _ bool, _, _ io.Writer) error {
		atomic.AddInt32(&respawned, 1)
		if rec == nil || rec.ID != coordID {
			t.Errorf("RecoverSpawn got rec %v, want cached old %s", rec, coordID)
		}
		if doc != handoffMd {
			t.Errorf("RecoverSpawn doc = %q, want the queued handoff doc %q (else successor is idle)", doc, handoffMd)
		}
		if preID != newID {
			t.Errorf("RecoverSpawn preAllocatedID = %q, want the queued successor id %q (else it can't adopt)", preID, newID)
		}
		return nil
	}

	report, err := gatherDoctorReportWith(doctorOpts{project: project, fix: true}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out, errOut bytes.Buffer
	doctorRunFixWith(doctorOpts{project: project, fix: true}, &report, &out, &errOut, d)

	if atomic.LoadInt32(&tookOver) != 1 {
		t.Errorf("TakeOver ran %d times, want 1", tookOver)
	}
	if atomic.LoadInt32(&respawned) != 1 {
		t.Errorf("RecoverSpawn ran %d times, want 1", respawned)
	}
	if atomic.LoadInt32(&deleted) != 1 {
		t.Errorf("DeleteQueue ran %d times, want 1 (the fulfilled handoff must be cleared)", deleted)
	}
	pr := report.projects[0]
	if !pr.fixPlanned || pr.fixErr != nil || pr.fixRefused != "" {
		t.Fatalf("fix outcome: planned=%v err=%v refused=%q", pr.fixPlanned, pr.fixErr, pr.fixRefused)
	}
	// Each action surfaced as it ran (surface-don't-silo).
	s := out.String()
	if !strings.Contains(s, "Stopping the stuck coordinator") {
		t.Errorf("missing stop action in surfaced output:\n%s", s)
	}
	if !strings.Contains(s, "Starting a fresh coordinator") {
		t.Errorf("missing start action in surfaced output:\n%s", s)
	}
}

// --- regression (codex PR6 [P2]): doctor (no --project) discovers a
//     queue-ONLY stuck project whose coord record was already archived ---

func TestDoctor_DiscoversQueueOnlyStuckProject(t *testing.T) {
	const project = "queueonly"
	d := doctorTestDeps()
	// No live agent records at all (OLD coord archived) ...
	d.ListAgents = func() ([]*agent.Record, error) { return nil, nil }
	// ... but a pending spawn-fresh queue file names the project, and no
	// healthy leader holds the lease -> a stuck handoff.
	d.ListPendingQueue = func() ([]string, error) { return []string{"/q/spawn-fresh-old.json"}, nil }
	d.ReadQueue = func(string) (queue.SpawnFresh, error) {
		return queue.SpawnFresh{OldAgentID: "old", Project: project, TaskID: CoordTaskIDPrefix + project}, nil
	}
	d.LeaderPresent = func(string) bool { return false }

	// doctor with NO --project must still surface this project (else `fleet
	// status` says "Run fleet doctor" but the command finds nothing).
	report, err := gatherDoctorReportWith(doctorOpts{}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	found := false
	for _, pr := range report.projects {
		if pr.project == project {
			found = true
			if pr.status != doctorStatusHandoffStuck {
				t.Errorf("queue-only project status = %v, want HandoffStuck", pr.status)
			}
		}
	}
	if !found {
		t.Fatalf("doctor (no --project) did not discover queue-only stuck project %q; projects=%v",
			project, report.projects)
	}
}

// --- T21: --fix REFUSES a live, heartbeating holder ---

func TestDoctor_T21_FixRefusesLiveHolder(t *testing.T) {
	const project = "t21proj"
	d := doctorTestDeps()
	// Diagnose says OK (healthy) — but force the headline to a recovery-needed
	// state to PROVE the live-holder guard refuses even if the diagnosis was
	// stale/raced. (Belt and suspenders: a live LeaderPresent must win.)
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthHung, HasRecord: true, OwnerPID: 1, OwnerAlive: true}
	}
	d.LeaderPresent = func(string) bool { return true } // LIVE + heartbeating NOW
	var killed, respawned int32
	d.TakeOver = func(string, string) (bool, error) { atomic.AddInt32(&killed, 1); return false, nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
		atomic.AddInt32(&respawned, 1)
		return nil
	}

	report, err := gatherDoctorReportWith(doctorOpts{project: project, fix: true}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out, errOut bytes.Buffer
	doctorRunFixWith(doctorOpts{project: project, fix: true}, &report, &out, &errOut, d)

	if atomic.LoadInt32(&killed) != 0 {
		t.Errorf("refused-live: TakeOver ran %d times, want 0 (never clear a live holder)", killed)
	}
	if atomic.LoadInt32(&respawned) != 0 {
		t.Errorf("refused-live: RecoverSpawn ran %d times, want 0", respawned)
	}
	pr := report.projects[0]
	if pr.fixRefused == "" {
		t.Fatalf("expected a refusal reason, got none")
	}
	if !strings.Contains(strings.ToLower(pr.fixRefused), "live") &&
		!strings.Contains(strings.ToLower(pr.fixRefused), "responding") {
		t.Errorf("refusal reason = %q, want it to say the coord is live/responding", pr.fixRefused)
	}
}

// --- T32: fenced_not_acquired surfaced + operator-confirmed recovery ---

func TestDoctor_T32_FencedNotAcquired_Surfaced(t *testing.T) {
	const project = "t32proj"
	d := doctorTestDeps()
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{
			Health: coordlock.LeaseHealthFencedNotAcquired, HasRecord: true,
			Epoch: 9, State: "fenced_not_acquired", OwnerPID: 555, OwnerAlive: true,
		}
	}

	// Read-only: surfaced as the needs-confirm status.
	report, err := gatherDoctorReportWith(doctorOpts{project: project}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	pr := report.projects[0]
	if pr.status != doctorStatusNeedsConfirm {
		t.Fatalf("status = %v, want NeedsConfirm", pr.status)
	}

	// --fix where the takeover CANNOT authenticate the kill (ambiguous): the
	// typed state is left, the failure is surfaced (NOT a silent stall), and
	// NO successor is spawned (NO second leader).
	var respawned int32
	d.TakeOver = func(string, string) (bool, error) {
		return false, errors.New("coord.KillCoordIfIdentityMatches: refused (identity gate failed)")
	}
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
		atomic.AddInt32(&respawned, 1)
		return nil
	}
	report2, _ := gatherDoctorReportWith(doctorOpts{project: project, fix: true}, d)
	var out, errOut bytes.Buffer
	doctorRunFixWith(doctorOpts{project: project, fix: true}, &report2, &out, &errOut, d)

	if atomic.LoadInt32(&respawned) != 0 {
		t.Fatalf("fenced_not_acquired --fix spawned a successor (%d) — that is a SECOND leader", respawned)
	}
	pr2 := report2.projects[0]
	if pr2.fixErr == nil {
		t.Fatalf("expected a surfaced recovery error for the un-authenticatable kill, got nil")
	}
	// Surface-don't-silo: the operator sees a concrete next step on stderr.
	es := errOut.String()
	if !strings.Contains(es, "fleet doctor") && !strings.Contains(es, "fleet status") {
		t.Errorf("stderr missing an operator next-step hint; got:\n%s", es)
	}
}

// --- codex PR6 iter-2 regressions ---

// [P2] doctor (no --project) discovers a LEASE-ONLY stuck project — its coord
// agent record AND queue file are both gone, only the stale epoch lingers.
func TestDoctor_DiscoversLeaseOnlyStuckProject(t *testing.T) {
	const project = "leaseonly"
	d := doctorTestDeps()
	d.ListAgents = func() ([]*agent.Record, error) { return nil, nil }
	d.ListPendingQueue = func() ([]string, error) { return nil, nil }
	// Only trace: a lingering lease record for a hung coord.
	d.LeaseProjects = func() ([]string, error) { return []string{project}, nil }
	d.Diagnose = func(p string) coordlock.LeaseDiagnosis {
		if p == project {
			return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthHung, HasRecord: true, OwnerPID: 9, OwnerAlive: true}
		}
		return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthNone}
	}

	report, err := gatherDoctorReportWith(doctorOpts{}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	found := false
	for _, pr := range report.projects {
		if pr.project == project {
			found = true
			if pr.status != doctorStatusUnresponsive {
				t.Errorf("lease-only project status = %v, want Unresponsive", pr.status)
			}
		}
	}
	if !found {
		t.Fatalf("doctor (no --project) did not discover lease-only stuck project %q", project)
	}
}

// [P2] read-only --verbose renders the lease diagnostic detail (it must NOT be
// gated behind --fix).
func TestDoctor_VerboseRendersDiagnosisDetail_NoFix(t *testing.T) {
	const project = "verbproj"
	d := doctorTestDeps()
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{
			Health: coordlock.LeaseHealthHung, HasRecord: true,
			Epoch: 42, State: "active", OwnerPID: 777, OwnerAlive: true,
		}
	}
	report, err := gatherDoctorReportWith(doctorOpts{project: project, verbose: true}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out bytes.Buffer
	// NOTE: fix=false — the diagnostic detail must still render under --verbose.
	renderDoctorReport(doctorOpts{project: project, verbose: true}, report, &out)
	s := out.String()
	if !strings.Contains(s, "epoch=42") || !strings.Contains(s, "owner-pid=777") {
		t.Errorf("read-only --verbose hid the lease diagnostic detail; got:\n%s", s)
	}
}

// [P2] an AMBIGUOUS tmux probe (false, err) must NOT be reported as a dead
// session and must NOT downgrade a healthy lease to "stopped".
func TestDoctor_AmbiguousSessionProbe_NotTreatedDead(t *testing.T) {
	const (
		project = "ambigproj"
		coordID = "ambigcoord"
	)
	rec := agent.New(coordID)
	rec.Project = project
	rec.TaskID = CoordTaskIDPrefix + project
	rec.TmuxSession = "fleet-ambig"

	d := doctorTestDeps()
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthOK, HasRecord: true, OwnerPID: 5, OwnerAlive: true}
	}
	d.CoordMarker = func(string) string { return coordID }
	d.LoadAgent = func(id string) (*agent.Record, error) {
		if id == coordID {
			return rec, nil
		}
		return nil, errors.New("no record")
	}
	// Ambiguous probe: (false, err) — a socket/list failure, NOT "dead".
	d.SessionAlive = func(string) (bool, error) { return false, errors.New("tmux list-sessions: connection refused") }

	report, err := gatherDoctorReportWith(doctorOpts{project: project}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	pr := report.projects[0]
	if pr.status != doctorStatusHealthy {
		t.Errorf("ambiguous tmux probe downgraded a healthy lease to %v; want Healthy", pr.status)
	}
	for _, f := range pr.findings {
		if strings.Contains(f.plain, "terminal session is gone") {
			t.Errorf("ambiguous tmux probe wrongly reported the session gone: %q", f.plain)
		}
	}
}

// --- codex PR6 iter-3 regressions ---

// [P1] a WORKER handoff queue for the same project must NOT be consumed by
// coord recovery: its doc/id must not feed the coord respawn and its queue
// must not be deleted.
func TestDoctor_IgnoresWorkerHandoffQueue(t *testing.T) {
	const (
		project = "wqproj"
		coordID = "wqcoord"
	)
	old := agent.New(coordID)
	old.Project = project
	old.TaskID = CoordTaskIDPrefix + project
	old.SupervisorPID = 222
	old.Command = []string{"claude"}

	d := doctorTestDeps()
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthDead, HasRecord: true, OwnerPID: 222, OwnerAlive: false}
	}
	d.CoordMarker = func(string) string { return coordID }
	d.LoadAgent = func(id string) (*agent.Record, error) {
		if id == coordID {
			return old, nil
		}
		return nil, errors.New("no record")
	}
	// The ONLY pending queue is a WORKER handoff for this project (TaskID is a
	// plain worker task, NOT coord-<project>).
	d.ListPendingQueue = func() ([]string, error) { return []string{"/q/spawn-fresh-worker.json"}, nil }
	d.ReadQueue = func(string) (queue.SpawnFresh, error) {
		return queue.SpawnFresh{OldAgentID: "worker7", Project: project, TaskID: "task-42",
			HandoffDoc: "/tmp/worker-handoff.md", NewAgentID: "worker8"}, nil
	}
	var deleted int32
	d.DeleteQueue = func(string) error { atomic.AddInt32(&deleted, 1); return nil }
	d.TakeOver = func(string, string) (bool, error) { return true, nil }
	var gotDoc, gotID string
	d.RecoverSpawn = func(_ *agent.Record, doc string, preID string, _ bool, _, _ io.Writer) error {
		gotDoc, gotID = doc, preID
		return nil
	}

	report, err := gatherDoctorReportWith(doctorOpts{project: project, fix: true}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out, errOut bytes.Buffer
	doctorRunFixWith(doctorOpts{project: project, fix: true}, &report, &out, &errOut, d)

	if gotDoc == "/tmp/worker-handoff.md" || gotID == "worker8" {
		t.Errorf("coord recovery consumed the WORKER handoff metadata (doc=%q id=%q)", gotDoc, gotID)
	}
	if atomic.LoadInt32(&deleted) != 0 {
		t.Errorf("coord recovery deleted the worker handoff queue (%d) — that drops the worker handoff", deleted)
	}
}

// [P2] a cleanly-RELEASED lease is NOT a stuck coord: read-only diagnosis
// reports nothing-to-recover, and --fix does not respawn.
func TestDoctor_ReleasedLease_NotRecovered(t *testing.T) {
	const project = "relproj"
	d := doctorTestDeps()
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthReleased, HasRecord: true, State: "released"}
	}
	var killed, respawned int32
	d.TakeOver = func(string, string) (bool, error) { atomic.AddInt32(&killed, 1); return false, nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error {
		atomic.AddInt32(&respawned, 1)
		return nil
	}

	report, err := gatherDoctorReportWith(doctorOpts{project: project, fix: true}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	pr := report.projects[0]
	if pr.status.needsRecovery() {
		t.Errorf("released lease classified as needing recovery (status=%v)", pr.status)
	}
	var out, errOut bytes.Buffer
	doctorRunFixWith(doctorOpts{project: project, fix: true}, &report, &out, &errOut, d)
	if atomic.LoadInt32(&killed) != 0 || atomic.LoadInt32(&respawned) != 0 {
		t.Errorf("released lease triggered recovery (killed=%d respawned=%d) — resurrects a cleanly-stopped coord", killed, respawned)
	}
}

// --- codex PR6 iter-4 regressions ---

// [P2] a HEALTHY lease with a (false,nil) dead recorded session must NOT be
// downgraded to Dead — that would advise --fix, which then refuses the live
// holder (an unrunnable remedy). Stays Healthy.
func TestDoctor_HealthyLease_DeadSession_NotDowngraded(t *testing.T) {
	const (
		project = "hlds"
		coordID = "hldscoord"
	)
	rec := agent.New(coordID)
	rec.Project = project
	rec.TaskID = CoordTaskIDPrefix + project
	rec.TmuxSession = "fleet-stale"

	d := doctorTestDeps()
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthOK, HasRecord: true, OwnerPID: 5, OwnerAlive: true}
	}
	d.CoordMarker = func(string) string { return coordID }
	d.LoadAgent = func(id string) (*agent.Record, error) {
		if id == coordID {
			return rec, nil
		}
		return nil, errors.New("no record")
	}
	d.SessionAlive = func(string) (bool, error) { return false, nil } // CONFIRMED gone

	report, err := gatherDoctorReportWith(doctorOpts{project: project}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	pr := report.projects[0]
	if pr.status != doctorStatusHealthy {
		t.Errorf("healthy lease + dead recorded session downgraded to %v; want Healthy (stale session field)", pr.status)
	}
	for _, f := range pr.findings {
		if strings.Contains(f.plain, "terminal session is gone") {
			t.Errorf("a healthy lease wrongly surfaced 'session gone' as a plain finding: %q", f.plain)
		}
	}
}

// [P2] a failed queue delete after a successful respawn is a RECOVERY ERROR
// (not a silent warning) so the caller doesn't think the handoff is safe while
// a stale queue file remains.
func TestDoctor_QueueDeleteFailure_IsRecoveryError(t *testing.T) {
	const (
		project = "qdf"
		coordID = "qdfcoord"
	)
	old := agent.New(coordID)
	old.Project = project
	old.TaskID = CoordTaskIDPrefix + project
	old.SupervisorPID = 333
	old.Command = []string{"claude"}

	d := doctorTestDeps()
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthDead, HasRecord: true, OwnerPID: 333, OwnerAlive: false}
	}
	d.CoordMarker = func(string) string { return coordID }
	d.LoadAgent = func(id string) (*agent.Record, error) {
		if id == coordID {
			return old, nil
		}
		return nil, errors.New("no record")
	}
	d.ListPendingQueue = func() ([]string, error) { return []string{"/q/spawn-fresh-" + coordID + ".json"}, nil }
	d.ReadQueue = func(string) (queue.SpawnFresh, error) {
		return queue.SpawnFresh{OldAgentID: coordID, Project: project, TaskID: CoordTaskIDPrefix + project,
			HandoffDoc: "/tmp/h.md", NewAgentID: "succ"}, nil
	}
	d.TakeOver = func(string, string) (bool, error) { return true, nil }
	d.RecoverSpawn = func(*agent.Record, string, string, bool, io.Writer, io.Writer) error { return nil }
	d.DeleteQueue = func(string) error { return errors.New("disk full") }

	report, err := gatherDoctorReportWith(doctorOpts{project: project, fix: true}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out, errOut bytes.Buffer
	doctorRunFixWith(doctorOpts{project: project, fix: true}, &report, &out, &errOut, d)

	pr := report.projects[0]
	if pr.fixErr == nil {
		t.Fatalf("queue delete failure must be a recovery error, got nil (caller would think the handoff is safe)")
	}
	// And the final render must NOT claim "Recovery complete".
	var rendered bytes.Buffer
	renderDoctorReport(doctorOpts{project: project, fix: true}, report, &rendered)
	if strings.Contains(rendered.String(), "Recovery complete") {
		t.Errorf("rendered a false 'Recovery complete' despite the queue-delete failure:\n%s", rendered.String())
	}
}

// --- codex PR6 iter-6 regression ---

// [P2] a WORKER task whose id prefix-matches "coord-" (e.g. "coord-cache-warm")
// must NOT be selected as the project coordinator — only the EXACT
// coord-<project> sentinel is the coord. Otherwise --fix would respawn from a
// worker record.
func TestDoctor_WorkerTaskNamedCoordPrefix_NotCoord(t *testing.T) {
	const project = "ops"
	worker := agent.New("wcache")
	worker.Project = project
	worker.TaskID = "coord-cache-warm" // a worker task, NOT coord-ops

	if isCoordAgentRecord(worker) {
		t.Fatalf("worker task %q treated as the project coordinator", worker.TaskID)
	}

	realCoord := agent.New("realcoord")
	realCoord.Project = project
	realCoord.TaskID = CoordTaskIDPrefix + project // coord-ops
	if !isCoordAgentRecord(realCoord) {
		t.Fatalf("real coord task %q not recognized", realCoord.TaskID)
	}

	// And the discovery scan must not surface the worker-only project.
	d := doctorTestDeps()
	d.ListAgents = func() ([]*agent.Record, error) { return []*agent.Record{worker}, nil }
	report, err := gatherDoctorReportWith(doctorOpts{}, d)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, pr := range report.projects {
		if pr.project == project {
			t.Errorf("worker-only project %q surfaced as having a coordinator", project)
		}
	}
}

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
		HandoffDocs:      func() ([]string, error) { return nil, nil },
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
	d.ListPendingQueue = func() ([]string, error) { return []string{"/q/spawn-fresh-" + coordID + ".json"}, nil }
	d.ReadQueue = func(string) (queue.SpawnFresh, error) {
		return queue.SpawnFresh{OldAgentID: coordID, Project: project}, nil
	}
	// LeaderPresent is false throughout (dead lease). Track the takeover +
	// respawn so we assert the fence->kill->respawn order ran.
	var tookOver, respawned int32
	d.TakeOver = func(p, a string) (bool, error) {
		atomic.AddInt32(&tookOver, 1)
		if p != project {
			t.Errorf("TakeOver project = %q, want %q", p, project)
		}
		return true, nil // acquired: old holder was gone, takeover fenced+killed it
	}
	d.RecoverSpawn = func(rec *agent.Record, _ string, _ string, _ bool, _, _ io.Writer) error {
		atomic.AddInt32(&respawned, 1)
		if rec == nil || rec.ID != coordID {
			t.Errorf("RecoverSpawn got rec %v, want cached old %s", rec, coordID)
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

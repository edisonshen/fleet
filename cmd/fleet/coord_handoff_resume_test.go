package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	fleetstate "github.com/edisonshen/fleet/internal/state"
)

func setupHandoffResumeRecord(t *testing.T, agentID, project, session string, docPath *string) {
	t.Helper()
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := fleetstate.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := fleetstate.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	rec := agent.New(agentID)
	rec.Project = project
	rec.TaskID = "coord-" + project
	rec.TmuxSession = session
	rec.LastHandoffPath = docPath
	if err := rec.Write(); err != nil {
		t.Fatalf("write agent record: %v", err)
	}
}

func writeTestCoordState(t *testing.T, project string, m map[string]any) {
	t.Helper()
	pdir, err := fleetstate.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal coord-state: %v", err)
	}
	if err := fleetstate.WriteAtomic(filepath.Join(pdir, "coord-state.json"), data); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
}

func readTestCoordState(t *testing.T, project string) map[string]any {
	t.Helper()
	pdir, err := fleetstate.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(pdir, "coord-state.json"))
	if err != nil {
		t.Fatalf("read coord-state: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal coord-state: %v", err)
	}
	return m
}

func baseHandoffResumeConfig(agentID, project, session string, wakeC <-chan time.Time, stderr *bytes.Buffer) handoffResumeNudgeConfig {
	return handoffResumeNudgeConfig{
		agentID:        agentID,
		project:        project,
		session:        session,
		loadAgent:      agent.Load,
		readMarker:     readResumedHandoffPath,
		waitReady:      func(string) error { return nil },
		sendVerified:   func(string, string) (bool, error) { return true, nil },
		initialDelayC:  closedTimeChan(),
		wakeC:          wakeC,
		maxAttempts:    4,
		transportTries: 3,
		stderr:         stderr,
	}
}

func runHandoffResumeLoopAsync(t *testing.T, cfg handoffResumeNudgeConfig) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHandoffResumeNudgeLoop(ctx, cfg)
	}()
	t.Cleanup(cancel)
	return cancel, done
}

func waitHandoffResumeDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handoff resume nudge loop did not finish")
	}
}

func waitHandoffResumeSend(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("handoff resume nudge was not sent")
	}
}

func closedTimeChan() <-chan time.Time {
	ch := make(chan time.Time)
	close(ch)
	return ch
}

func TestHandoffResumeNudgeLoop_DecisionCases(t *testing.T) {
	const (
		agentID = "hrdecide"
		project = "hr-decision"
		session = "fleet-hrdecide"
		docPath = "/tmp/fleet-handoff.md"
	)
	tests := []struct {
		name              string
		lastPath          *string
		marker            string
		disableAutoResume bool
		wantSends         int32
		wantLoadID        string
	}{
		{
			name:       "nil path fresh coord",
			lastPath:   nil,
			marker:     "",
			wantSends:  0,
			wantLoadID: agentID,
		},
		{
			name:       "marker already applied",
			lastPath:   ptrString(docPath),
			marker:     docPath,
			wantSends:  0,
			wantLoadID: agentID,
		},
		{
			name:       "marker behind nudges successor own record",
			lastPath:   ptrString(docPath),
			marker:     "/tmp/old.md",
			wantSends:  1,
			wantLoadID: agentID,
		},
		{
			name:              "disable auto resume suppresses nudge",
			lastPath:          ptrString(docPath),
			marker:            "/tmp/old.md",
			disableAutoResume: true,
			wantSends:         0,
			wantLoadID:        agentID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupHandoffResumeRecord(t, agentID, project, session, tt.lastPath)
			if tt.disableAutoResume {
				rec, err := agent.Load(agentID)
				if err != nil {
					t.Fatalf("load record: %v", err)
				}
				rec.DisableAutoResume = true
				if err := rec.Write(); err != nil {
					t.Fatalf("write disabled record: %v", err)
				}
			}
			writeTestCoordState(t, project, map[string]any{
				resumedHandoffPathCoordStateKey: tt.marker,
			})
			var sends int32
			var gotLoadID string
			var stderr bytes.Buffer
			cfg := baseHandoffResumeConfig(agentID, project, session, nil, &stderr)
			cfg.maxAttempts = 1
			cfg.loadAgent = func(id string) (*agent.Record, error) {
				gotLoadID = id
				return agent.Load(id)
			}
			cfg.sendVerified = func(string, string) (bool, error) {
				atomic.AddInt32(&sends, 1)
				if tt.wantSends > 0 {
					writeTestCoordState(t, project, map[string]any{
						resumedHandoffPathCoordStateKey: docPath,
					})
				}
				return true, nil
			}
			runHandoffResumeNudgeLoop(context.Background(), cfg)
			if got := atomic.LoadInt32(&sends); got != tt.wantSends {
				t.Fatalf("send count = %d, want %d", got, tt.wantSends)
			}
			if gotLoadID != tt.wantLoadID {
				t.Fatalf("load id = %q, want own successor id %q", gotLoadID, tt.wantLoadID)
			}
		})
	}
}

func TestHandoffResumeNudgeLoop_InitialRecheckSuppressesPushDuplicate(t *testing.T) {
	const (
		agentID = "hrrechek"
		project = "hr-recheck"
		session = "fleet-hrrechek"
		docPath = "/tmp/fleet-handoff.md"
	)
	setupHandoffResumeRecord(t, agentID, project, session, ptrString(docPath))
	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: "",
	})
	initialDelayC := make(chan time.Time)
	wakeC := make(chan time.Time)
	var sends int32
	var stderr bytes.Buffer
	cfg := baseHandoffResumeConfig(agentID, project, session, wakeC, &stderr)
	cfg.initialDelayC = initialDelayC
	cfg.sendVerified = func(string, string) (bool, error) {
		atomic.AddInt32(&sends, 1)
		return true, nil
	}
	_, done := runHandoffResumeLoopAsync(t, cfg)

	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: docPath,
	})
	close(initialDelayC)
	waitHandoffResumeDone(t, done)
	if got := atomic.LoadInt32(&sends); got != 0 {
		t.Fatalf("resident nudge sent %d prompts after push ack caught up, want 0", got)
	}
}

func TestHandoffResumeNudgeLoop_DoesNotWriteMarker(t *testing.T) {
	const (
		agentID = "hrnowrit"
		project = "hr-no-writer"
		session = "fleet-hrnowrit"
		docPath = "/tmp/fleet-handoff.md"
	)
	setupHandoffResumeRecord(t, agentID, project, session, ptrString(docPath))
	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: "/tmp/old.md",
		"worker_agent_ids":              map[string]any{"task": "aaaa1111"},
	})
	var sends int32
	var stderr bytes.Buffer
	cfg := baseHandoffResumeConfig(agentID, project, session, nil, &stderr)
	cfg.maxAttempts = 1
	cfg.sendVerified = func(string, string) (bool, error) {
		atomic.AddInt32(&sends, 1)
		return true, nil
	}
	runHandoffResumeNudgeLoop(context.Background(), cfg)
	cs := readTestCoordState(t, project)
	if got := cs[resumedHandoffPathCoordStateKey]; got != "/tmp/old.md" {
		t.Fatalf("nudge wrote marker = %v, want old marker untouched", got)
	}
	if got := atomic.LoadInt32(&sends); got != 1 {
		t.Fatalf("send count = %d, want 1", got)
	}
}

func TestHandoffResumeNudgeLoop_TransportRetryUntilReady(t *testing.T) {
	const (
		agentID = "hrretry"
		project = "hr-retry"
		session = "fleet-hrretry"
		docPath = "/tmp/fleet-handoff.md"
	)
	setupHandoffResumeRecord(t, agentID, project, session, ptrString(docPath))
	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: "",
	})
	var readyCalls int32
	var sends int32
	var stderr bytes.Buffer
	cfg := baseHandoffResumeConfig(agentID, project, session, nil, &stderr)
	cfg.maxAttempts = 1
	cfg.transportTries = 3
	cfg.waitReady = func(string) error {
		if atomic.AddInt32(&readyCalls, 1) < 3 {
			return errors.New("booting")
		}
		return nil
	}
	cfg.sendVerified = func(string, string) (bool, error) {
		atomic.AddInt32(&sends, 1)
		if atomic.LoadInt32(&readyCalls) >= 3 {
			writeTestCoordState(t, project, map[string]any{
				resumedHandoffPathCoordStateKey: docPath,
			})
			return true, nil
		}
		return false, nil
	}
	runHandoffResumeNudgeLoop(context.Background(), cfg)
	if got := atomic.LoadInt32(&readyCalls); got != 3 {
		t.Fatalf("ready calls = %d, want 3", got)
	}
	if got := atomic.LoadInt32(&sends); got != 3 {
		t.Fatalf("send calls = %d, want 3 re-issued sends while readiness converges", got)
	}
}

func TestHandoffResumeNudgeLoop_TransportRetryUntilReadinessSucceedsEvenIfSubmitted(t *testing.T) {
	const (
		agentID = "hrready"
		project = "hr-ready"
		session = "fleet-hrready"
		docPath = "/tmp/fleet-handoff.md"
	)
	setupHandoffResumeRecord(t, agentID, project, session, ptrString(docPath))
	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: "",
	})
	var readyCalls int32
	var sends int32
	var stderr bytes.Buffer
	cfg := baseHandoffResumeConfig(agentID, project, session, nil, &stderr)
	cfg.maxAttempts = 1
	cfg.transportTries = 3
	cfg.waitReady = func(string) error {
		if atomic.AddInt32(&readyCalls, 1) < 3 {
			return errors.New("ready prompt not visible yet")
		}
		return nil
	}
	cfg.sendVerified = func(string, string) (bool, error) {
		atomic.AddInt32(&sends, 1)
		if atomic.LoadInt32(&readyCalls) >= 3 {
			writeTestCoordState(t, project, map[string]any{
				resumedHandoffPathCoordStateKey: docPath,
			})
		}
		return true, nil
	}
	runHandoffResumeNudgeLoop(context.Background(), cfg)
	if got := atomic.LoadInt32(&readyCalls); got != 3 {
		t.Fatalf("ready calls = %d, want retry until readiness succeeds", got)
	}
	if got := atomic.LoadInt32(&sends); got != 3 {
		t.Fatalf("send calls = %d, want 3 verified sends", got)
	}
}

func TestHandoffResumeNudgeLoop_RenudgesAfterRestartWhenMarkerBehind(t *testing.T) {
	const (
		agentID = "hrcrash"
		project = "hr-crash"
		session = "fleet-hrcrash"
		docPath = "/tmp/fleet-handoff.md"
	)
	setupHandoffResumeRecord(t, agentID, project, session, ptrString(docPath))
	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: "",
	})
	var sends int32
	sendCh := make(chan struct{}, 2)
	wakeC := make(chan time.Time)
	var stderr bytes.Buffer
	cfg := baseHandoffResumeConfig(agentID, project, session, wakeC, &stderr)
	cfg.sendVerified = func(string, string) (bool, error) {
		atomic.AddInt32(&sends, 1)
		sendCh <- struct{}{}
		return true, nil
	}
	cancel, done := runHandoffResumeLoopAsync(t, cfg)
	waitHandoffResumeSend(t, sendCh)
	cancel()
	waitHandoffResumeDone(t, done)

	cancel, done = runHandoffResumeLoopAsync(t, cfg)
	waitHandoffResumeSend(t, sendCh)
	cancel()
	waitHandoffResumeDone(t, done)
	if got := atomic.LoadInt32(&sends); got != 2 {
		t.Fatalf("send count across restart = %d, want 2", got)
	}
}

func TestHandoffResumeNudgeLoop_AppliedRetryCappedAndDiagnostic(t *testing.T) {
	const (
		agentID = "hrcap"
		project = "hr-cap"
		session = "fleet-hrcap"
		docPath = "/tmp/fleet-handoff.md"
	)
	setupHandoffResumeRecord(t, agentID, project, session, ptrString(docPath))
	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: "",
	})
	wakeC := make(chan time.Time)
	sendCh := make(chan struct{}, 3)
	var sends int32
	var stderr bytes.Buffer
	cfg := baseHandoffResumeConfig(agentID, project, session, wakeC, &stderr)
	cfg.maxAttempts = 3
	cfg.sendVerified = func(string, string) (bool, error) {
		atomic.AddInt32(&sends, 1)
		sendCh <- struct{}{}
		return true, nil
	}
	_, done := runHandoffResumeLoopAsync(t, cfg)
	waitHandoffResumeSend(t, sendCh)
	wakeC <- time.Now()
	waitHandoffResumeSend(t, sendCh)
	wakeC <- time.Now()
	waitHandoffResumeSend(t, sendCh)
	waitHandoffResumeDone(t, done)

	if got := atomic.LoadInt32(&sends); got != 3 {
		t.Fatalf("send count = %d, want capped at 3", got)
	}
	errText := stderr.String()
	if !strings.Contains(errText, "not applied") || !strings.Contains(errText, "after 3 resume nudges") {
		t.Fatalf("diagnostic missing cap/applied text: %q", errText)
	}
}

func TestHandoffResumeNudgeLoop_CommonCaseZeroRenudge(t *testing.T) {
	const (
		agentID = "hrapplid"
		project = "hr-applied"
		session = "fleet-hrapplid"
		docPath = "/tmp/fleet-handoff.md"
	)
	setupHandoffResumeRecord(t, agentID, project, session, ptrString(docPath))
	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: "",
	})
	wakeC := make(chan time.Time)
	var sends int32
	var stderr bytes.Buffer
	cfg := baseHandoffResumeConfig(agentID, project, session, wakeC, &stderr)
	cfg.sendVerified = func(string, string) (bool, error) {
		atomic.AddInt32(&sends, 1)
		writeTestCoordState(t, project, map[string]any{
			resumedHandoffPathCoordStateKey: docPath,
		})
		return true, nil
	}
	_, done := runHandoffResumeLoopAsync(t, cfg)
	waitHandoffResumeDone(t, done)
	if got := atomic.LoadInt32(&sends); got != 1 {
		t.Fatalf("send count = %d, want one nudge after marker advances", got)
	}
}

func TestHandoffResumeNudgeLoop_NudgeIsPathOnly(t *testing.T) {
	// The coord-run supervisor re-nudge sends the durable doc path only. The
	// ~2KB inline decision block is delivered solely by the verified primary
	// handoff.go path, once; re-inlining it on every liveness re-poke wastes the
	// live coord's context (operator observed three copies of one handoff).
	const (
		agentID = "hrpathonly"
		project = "hr-path-only"
		session = "fleet-hrpathonly"
	)
	docPath := filepath.Join(t.TempDir(), "handoff.md")
	doc := "## Completed\nold work\n\n" +
		"## Key Decisions\n- Path B owns the guarantee\n\n" +
		"## Next Steps (prioritized)\n1. land PR-2\n\n" +
		"## Active Subagents\n_(none)_\n"
	if err := os.WriteFile(docPath, []byte(doc), 0o644); err != nil {
		t.Fatalf("write handoff doc: %v", err)
	}
	setupHandoffResumeRecord(t, agentID, project, session, ptrString(docPath))
	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: "",
	})
	var prompt string
	var stderr bytes.Buffer
	cfg := baseHandoffResumeConfig(agentID, project, session, nil, &stderr)
	cfg.maxAttempts = 1
	cfg.sendVerified = func(_ string, gotPrompt string) (bool, error) {
		prompt = gotPrompt
		writeTestCoordState(t, project, map[string]any{
			resumedHandoffPathCoordStateKey: docPath,
		})
		return true, nil
	}
	runHandoffResumeNudgeLoop(context.Background(), cfg)
	if want := "Read your handoff doc at " + docPath; !strings.Contains(prompt, want) {
		t.Fatalf("nudge prompt missing path directive %q:\n%s", want, prompt)
	}
	for _, notWant := range []string{
		"Inline handoff sections:",
		"## Key Decisions",
		"## Next Steps (prioritized)",
	} {
		if strings.Contains(prompt, notWant) {
			t.Fatalf("path-only nudge unexpectedly contains inline text %q:\n%s", notWant, prompt)
		}
	}
}

func TestCoordRun_HandoffResumeNudge_StandDownDoesNotNudge(t *testing.T) {
	const (
		agentID = "hrloser1"
		project = "hr-standdown"
		session = "fleet-hrloser1"
		docPath = "/tmp/fleet-handoff.md"
	)
	setupHandoffResumeRecord(t, agentID, project, session, ptrString(docPath))
	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: "",
	})
	sentinel := filepath.Join(t.TempDir(), "child-ran")
	var sends int32
	stoodDown := false
	opts := coordRunOpts{
		agentID:  agentID,
		project:  project,
		session:  session,
		argv:     childSentinelArgv(sentinel),
		killTmux: func(string) error { return nil },
		acquireLease: func() (coordLease, bool, []liveHolderInfo, error) {
			return &fakeLease{}, false, nil, nil
		},
		onStandDown: func() { stoodDown = true },
		handoffResumeSendVerified: func(string, string) (bool, error) {
			atomic.AddInt32(&sends, 1)
			return true, nil
		},
	}
	if err := runCoordRun(context.Background(), opts, os.Stdout, os.Stderr); err != nil {
		t.Fatalf("runCoordRun standdown: %v", err)
	}
	if !stoodDown {
		t.Fatal("standdown path did not run")
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("standdown started child sentinel: %v", err)
	}
	if got := atomic.LoadInt32(&sends); got != 0 {
		t.Fatalf("non-flock-winner sent %d nudges, want 0", got)
	}
}

func TestCoordRun_HandoffResumeNudge_FlockWinnerStartsNudger(t *testing.T) {
	const (
		agentID = "hrwinner"
		project = "hr-winner"
		session = "fleet-hrwinner"
		docPath = "/tmp/fleet-handoff.md"
	)
	setupHandoffResumeRecord(t, agentID, project, session, ptrString(docPath))
	writeTestCoordState(t, project, map[string]any{
		resumedHandoffPathCoordStateKey: "",
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	lease := &fakeLease{}
	sendCh := make(chan struct{}, 1)
	var sends int32
	var stderr bytes.Buffer
	opts := coordRunOpts{
		agentID:  agentID,
		project:  project,
		session:  session,
		argv:     []string{"sleep", "30"},
		killTmux: func(string) error { return nil },
		acquireLease: func() (coordLease, bool, []liveHolderInfo, error) {
			return lease, true, nil, nil
		},
		handoffResumeInitialDelayC: closedTimeChan(),
		handoffResumeWakeC:         make(chan time.Time),
		handoffResumeWaitReady:     func(string) error { return nil },
		handoffResumeSendVerified: func(string, string) (bool, error) {
			if atomic.AddInt32(&sends, 1) == 1 {
				writeTestCoordState(t, project, map[string]any{
					resumedHandoffPathCoordStateKey: docPath,
				})
				sendCh <- struct{}{}
			}
			return true, nil
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCoordRun(ctx, opts, os.Stdout, &stderr)
	}()
	waitHandoffResumeSend(t, sendCh)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("runCoordRun did not exit after cancel; stderr=%q", stderr.String())
	}
	if got := atomic.LoadInt32(&sends); got != 1 {
		t.Fatalf("send count = %d, want flock winner nudge", got)
	}
	if got := lease.releases(); got != 1 {
		t.Fatalf("lease releases = %d, want 1", got)
	}
}

func ptrString(s string) *string {
	return &s
}

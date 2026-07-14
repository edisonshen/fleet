package workers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/fleetlog"
	"github.com/edisonshen/fleet/internal/projects"
	"github.com/edisonshen/fleet/internal/state"
)

func TestStateRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	in := &State{
		Slug:            "add-readme-7a3c",
		Project:         "fleet",
		Phase:           PhaseTDDRefactor,
		PhasesCompleted: []Phase{PhaseBranch, PhaseTDDRed, PhaseTDDGreen},
		StartedAt:       time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC),
		PID:             12345,
	}
	if err := WriteState("fleet", in.Slug, in); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	out, err := ReadState("fleet", in.Slug)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if out.Slug != in.Slug || out.Phase != in.Phase || out.PID != in.PID {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
	if out.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt not bumped")
	}
}

func TestUpdateStateConcurrent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	const N = 20
	// Prime initial state.
	if err := WriteState("fleet", "shared-slug-aaaa", &State{
		Slug:      "shared-slug-aaaa",
		Project:   "fleet",
		Phase:     PhaseStarting,
		StartedAt: time.Now().UTC(),
		PID:       0,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var counter atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := UpdateState("fleet", "shared-slug-aaaa", func(s *State) {
				// Each goroutine bumps PID by 1. If the read-
				// modify-write isn't serialized, we lose updates.
				s.PID++
				counter.Add(1)
			})
			if err != nil {
				t.Errorf("UpdateState: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := ReadState("fleet", "shared-slug-aaaa")
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got.PID != int(counter.Load()) {
		t.Errorf("PID=%d after %d updates; want %d (race lost updates)", got.PID, N, N)
	}
}

func TestPhaseValidation_DoneRequiresPR(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	s := &State{
		Slug: "x-aaaa", Project: "fleet", Phase: PhaseDone,
		StartedAt: time.Now().UTC(), PID: 1,
	}
	err := WriteState("fleet", s.Slug, s)
	if !errors.Is(err, ErrPhaseRequiresPR) {
		t.Errorf("got %v; want ErrPhaseRequiresPR", err)
	}
	s.PRURL = "https://github.com/x/y/pull/1"
	if err := WriteState("fleet", s.Slug, s); err != nil {
		t.Errorf("WriteState with pr_url: %v", err)
	}
}

func TestPhaseValidation_BlockedRequiresReason(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	s := &State{
		Slug: "y-aaaa", Project: "fleet", Phase: PhaseBlocked,
		StartedAt: time.Now().UTC(), PID: 1,
	}
	err := WriteState("fleet", s.Slug, s)
	if !errors.Is(err, ErrPhaseRequiresWhy) {
		t.Errorf("got %v; want ErrPhaseRequiresWhy", err)
	}
	s.BlockedReason = "stuck on auth header"
	if err := WriteState("fleet", s.Slug, s); err != nil {
		t.Errorf("WriteState with reason: %v", err)
	}
}

func TestWriteState_RejectsSlugMismatch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	s := &State{
		Slug:      "wrong-slug-1234",
		Project:   "fleet",
		Phase:     PhaseStarting,
		StartedAt: time.Now().UTC(),
	}
	err := WriteState("fleet", "right-slug-5678", s)
	if !errors.Is(err, ErrInvalidState) {
		t.Errorf("got %v; want ErrInvalidState (slug mismatch)", err)
	}
}

func TestWriteState_RejectsProjectMismatch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	s := &State{
		Slug:      "x-1234",
		Project:   "wrong-project",
		Phase:     PhaseStarting,
		StartedAt: time.Now().UTC(),
	}
	err := WriteState("right-project", "x-1234", s)
	if !errors.Is(err, ErrInvalidState) {
		t.Errorf("got %v; want ErrInvalidState (project mismatch)", err)
	}
}

func TestPhaseValidation_InvalidPhase(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	s := &State{
		Slug: "z-aaaa", Project: "fleet", Phase: Phase("bogus"),
		StartedAt: time.Now().UTC(), PID: 1,
	}
	err := WriteState("fleet", s.Slug, s)
	if !errors.Is(err, ErrInvalidPhase) {
		t.Errorf("got %v; want ErrInvalidPhase", err)
	}
}

func TestIsAlive_DeadProcess(t *testing.T) {
	// Spawn /bin/true and wait; its PID is then dead.
	cmd := startSleeper(t, 0)
	pid := cmd.Process.Pid
	_ = cmd.Wait() // reap; pid is now reusable but not alive in our parent.
	if IsAlive(pid) {
		// On macOS / Linux a reaped child's PID returns ESRCH on
		// kill(pid, 0). Edge case: the kernel may have already
		// recycled the pid for another process; in that case this
		// test is racy, but in CI the pid space is large enough
		// that recycling within milliseconds is rare. Skip if so.
		t.Skipf("PID %d was recycled; cannot test reliably", pid)
	}
}

func TestIsAlive_LivingProcess(t *testing.T) {
	cmd := startSleeper(t, 5*time.Second)
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if !IsAlive(cmd.Process.Pid) {
		t.Errorf("IsAlive(%d) reported false; process is alive", cmd.Process.Pid)
	}
}

func TestIsAlive_ZeroPID(t *testing.T) {
	if IsAlive(0) {
		t.Errorf("IsAlive(0) returned true; want false")
	}
	if IsAlive(-1) {
		t.Errorf("IsAlive(-1) returned true; want false")
	}
}

func TestArchive_AtomicMove(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	deadPID := terminatedPID(t)
	if err := WriteState("fleet", "ar-aaaa", &State{
		Slug: "ar-aaaa", Project: "fleet", Phase: PhaseDone,
		PRURL:     "https://github.com/x/y/pull/1",
		StartedAt: time.Now().UTC(), PID: deadPID,
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := Archive("fleet", "ar-aaaa"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	src, _ := state.WorkerDir("fleet", "ar-aaaa")
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src dir still exists after Archive: %v", err)
	}
	// Archive dir under workers/archive/ should exist with one entry.
	dir, _ := state.ProjectDir("fleet")
	entries, err := os.ReadDir(filepath.Join(dir, "workers", "archive"))
	if err != nil {
		t.Fatalf("readdir archive: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("archive entries=%d; want 1", len(entries))
	}
	if !startsWith(entries[0].Name(), "ar-aaaa-") {
		t.Errorf("archive entry name=%q; want prefix 'ar-aaaa-'", entries[0].Name())
	}
}

func TestArchive_RefusesLiveWorker(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := WriteState("fleet", "live-aaaa", &State{
		Slug: "live-aaaa", Project: "fleet", Phase: PhaseTDDGreen,
		StartedAt: time.Now().UTC(), PID: 1,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := Archive("fleet", "live-aaaa")
	if !errors.Is(err, ErrPreconditionLive) {
		t.Errorf("got %v; want ErrPreconditionLive", err)
	}
}

func TestArchive_TrustsRecordedExitOverPIDReuse(t *testing.T) {
	// State has phase=done + a recorded exit code, but PID happens
	// to be alive (recycled by another process). Archive must trust
	// the exit code over IsAlive — the original worker is gone.
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	live := startSleeper(t, 5*time.Second)
	defer func() { _ = live.Process.Kill(); _ = live.Wait() }()
	exit := 0
	if err := WriteState("fleet", "exited-aaaa", &State{
		Slug: "exited-aaaa", Project: "fleet", Phase: PhaseDone,
		PRURL: "https://x/y/1", StartedAt: time.Now().UTC(),
		PID:  live.Process.Pid, // recycled — pretend this is a re-used PID
		Exit: &exit,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Archive("fleet", "exited-aaaa"); err != nil {
		t.Errorf("Archive returned %v; want nil (recorded exit beats IsAlive)", err)
	}
}

func TestArchive_RefusesZeroPIDWithoutExit(t *testing.T) {
	// UpdateState bootstraps states with phase=starting and no PID.
	// If a worker flips straight to a terminal phase before its PID
	// or exit is persisted, Archive must NOT consider that a safe
	// archive — the process may still be running. Require a recorded
	// exit code OR a non-zero PID before we trust the precondition
	// (codex iter-10 P2).
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := WriteState("fleet", "ghost-aaaa", &State{
		Slug: "ghost-aaaa", Project: "fleet", Phase: PhaseDone,
		PRURL:     "https://x/y/1",
		StartedAt: time.Now().UTC(),
		PID:       0, // unknown
		Exit:      nil,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := Archive("fleet", "ghost-aaaa")
	if !errors.Is(err, ErrPreconditionLive) {
		t.Errorf("got %v; want ErrPreconditionLive (pid=0 + no exit)", err)
	}
	// And recovery: write an exit code, then archive proceeds.
	if err := UpdateState("fleet", "ghost-aaaa", func(s *State) {
		exit := 0
		s.Exit = &exit
	}); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	if err := Archive("fleet", "ghost-aaaa"); err != nil {
		t.Errorf("Archive after exit recorded: %v; want nil", err)
	}
}

func TestArchive_AllowsBlockedAndFailed(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	deadPID := terminatedPID(t)
	for _, c := range []struct {
		slug   string
		phase  Phase
		reason string
	}{
		{"blocked-aaaa", PhaseBlocked, "stuck on auth"},
		{"failed-bbbb", PhaseFailed, ""},
	} {
		s := &State{
			Slug: c.slug, Project: "fleet", Phase: c.phase,
			StartedAt: time.Now().UTC(), PID: deadPID,
		}
		if c.phase == PhaseBlocked {
			s.BlockedReason = c.reason
		}
		if err := WriteState("fleet", c.slug, s); err != nil {
			t.Fatalf("seed %s: %v", c.slug, err)
		}
		if err := Archive("fleet", c.slug); err != nil {
			t.Errorf("Archive(%s) phase=%s: %v", c.slug, c.phase, err)
		}
	}
}

// TestArchive_SerializesWithUpdateState confirms an in-flight
// UpdateState blocks Archive from racing the rename. We launch
// UpdateState on a goroutine that holds the lock for ~50ms, then
// call Archive — which must wait, then re-check the precondition.
// Since UpdateState bumps phase to a non-terminal value, Archive
// must surface ErrPreconditionLive.
func TestArchive_SerializesWithUpdateState(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	deadPID := terminatedPID(t)
	if err := WriteState("fleet", "race-aaaa", &State{
		Slug: "race-aaaa", Project: "fleet", Phase: PhaseDone,
		PRURL: "https://x/y/1", StartedAt: time.Now().UTC(), PID: deadPID,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	updateStarted := make(chan struct{})
	updateDone := make(chan struct{})
	go func() {
		defer close(updateDone)
		_ = UpdateState("fleet", "race-aaaa", func(s *State) {
			close(updateStarted)
			// Hold the lock briefly so Archive must wait.
			time.Sleep(50 * time.Millisecond)
			// Bump to non-terminal — Archive's re-check should reject.
			s.Phase = PhaseTDDGreen
			s.PRURL = ""
		})
	}()
	<-updateStarted
	// Archive should block on the same lock, then re-check and refuse.
	err := Archive("fleet", "race-aaaa")
	<-updateDone
	if !errors.Is(err, ErrPreconditionLive) {
		t.Errorf("got %v; want ErrPreconditionLive (UpdateState bumped phase under lock)", err)
	}
}

func TestArchive_RefusesLiveProcessAtTerminalPhase(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	live := startSleeper(t, 5*time.Second)
	defer func() { _ = live.Process.Kill(); _ = live.Wait() }()
	// State says "done" but PID is alive — Archive must refuse.
	if err := WriteState("fleet", "live-done-aaaa", &State{
		Slug: "live-done-aaaa", Project: "fleet", Phase: PhaseDone,
		PRURL: "https://x/y/1", StartedAt: time.Now().UTC(),
		PID: live.Process.Pid,
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	err := Archive("fleet", "live-done-aaaa")
	if !errors.Is(err, ErrPreconditionLive) {
		t.Errorf("got %v; want ErrPreconditionLive (alive PID)", err)
	}
}

func TestArchive_CollisionRetriesWithSalt(t *testing.T) {
	// Same-second double archive: producer must retry with `-N`
	// salt rather than fail with EEXIST.
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	deadPID := terminatedPID(t)
	dir, _ := state.ProjectDir("fleet")

	// Pre-create a colliding archive entry.
	stamp := time.Now().UTC().Format("20060102-150405")
	collide := filepath.Join(dir, "workers", "archive", "twin-aaaa-"+stamp)
	if err := os.MkdirAll(collide, 0o755); err != nil {
		t.Fatalf("seed collision: %v", err)
	}

	if err := WriteState("fleet", "twin-aaaa", &State{
		Slug: "twin-aaaa", Project: "fleet", Phase: PhaseDone,
		PRURL: "https://x/y/1", StartedAt: time.Now().UTC(), PID: deadPID,
	}); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	if err := Archive("fleet", "twin-aaaa"); err != nil {
		t.Fatalf("Archive (with collision): %v", err)
	}
	// Both the colliding placeholder and the salted archive exist.
	entries, err := os.ReadDir(filepath.Join(dir, "workers", "archive"))
	if err != nil {
		t.Fatalf("readdir archive: %v", err)
	}
	if len(entries) < 2 {
		t.Errorf("archive entries=%d; want >=2 (collision + salted retry)", len(entries))
	}
}

func TestArchive_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	// No worker created. Archive should be a no-op (nil error).
	if err := Archive("fleet", "no-such-aaaa"); err != nil {
		t.Errorf("Archive on missing dir returned %v; want nil", err)
	}
}

func TestPruneArchive_UsesEmbeddedTimestamp(t *testing.T) {
	// Decision must come from the timestamp encoded in the dir name,
	// NOT mtime — rename(2) preserves the source dir's mtime, which
	// would otherwise let us prune just-archived dirs immediately.
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	dir, _ := state.ProjectDir("fleet")
	archRoot := filepath.Join(dir, "workers", "archive")
	if err := os.MkdirAll(archRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Two dirs: one with embedded stamp 30d ago, one with 1d ago.
	// Set mtime to NOW for both — the implementation must ignore it.
	old := filepath.Join(archRoot, "old-aaaa-20260401-000000")
	young := filepath.Join(archRoot, "young-bbbb-20260505-000000")
	for _, d := range []string{old, young} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		now := time.Now()
		if err := os.Chtimes(d, now, now); err != nil {
			t.Fatalf("chtimes %s: %v", d, err)
		}
	}
	// Cutoff between the two stamps.
	cutoff := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	n, err := PruneArchive("fleet", cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned=%d; want 1 (only old)", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old still exists after prune")
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("young missing after prune: %v", err)
	}
}

func TestPruneArchive_SkipsUnrecognizedNames(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	dir, _ := state.ProjectDir("fleet")
	archRoot := filepath.Join(dir, "workers", "archive")
	weird := filepath.Join(archRoot, "manually-created")
	if err := os.MkdirAll(weird, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Cutoff far in the future — would prune everything if we trusted
	// mtime; must skip the unrecognized name.
	cutoff := time.Now().Add(time.Hour)
	n, err := PruneArchive("fleet", cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned=%d; want 0 (unrecognized name)", n)
	}
	if _, err := os.Stat(weird); err != nil {
		t.Errorf("weird dir removed: %v", err)
	}
}

func TestListActive(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	deadPID := terminatedPID(t)
	for _, slug := range []string{"alpha-aaaa", "beta-bbbb", "gamma-cccc"} {
		if err := WriteState("fleet", slug, &State{
			Slug: slug, Project: "fleet", Phase: PhaseStarting,
			StartedAt: time.Now().UTC(), PID: deadPID,
		}); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
	// Mark alpha terminal + dead PID (Archive precondition) before archiving it.
	if err := WriteState("fleet", "alpha-aaaa", &State{
		Slug: "alpha-aaaa", Project: "fleet", Phase: PhaseDone,
		PRURL: "https://x/y/1", StartedAt: time.Now().UTC(), PID: deadPID,
	}); err != nil {
		t.Fatalf("update alpha to done: %v", err)
	}
	if err := Archive("fleet", "alpha-aaaa"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	got, err := ListActive("fleet")
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("active=%d; want 2 (alpha is archived)", len(got))
	}
	// Slug sort order.
	if got[0].Slug != "beta-bbbb" || got[1].Slug != "gamma-cccc" {
		t.Errorf("got slugs %v; want [beta-bbbb gamma-cccc]", []string{got[0].Slug, got[1].Slug})
	}
}

func TestListAll(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	deadPID := terminatedPID(t)
	if err := WriteState("fleet", "live-aaaa", &State{
		Slug: "live-aaaa", Project: "fleet", Phase: PhaseStarting,
		StartedAt: time.Now().UTC(), PID: deadPID,
	}); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	if err := WriteState("fleet", "gone-bbbb", &State{
		Slug: "gone-bbbb", Project: "fleet", Phase: PhaseDone,
		PRURL: "https://x/y/1", StartedAt: time.Now().UTC(), PID: deadPID,
	}); err != nil {
		t.Fatalf("seed gone: %v", err)
	}
	if err := Archive("fleet", "gone-bbbb"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	active, archived, err := ListAll("fleet")
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(active) != 1 || active[0].Slug != "live-aaaa" {
		t.Errorf("active=%v; want [live-aaaa]", slugs(active))
	}
	if len(archived) != 1 || archived[0].Slug != "gone-bbbb" {
		t.Errorf("archived=%v; want [gone-bbbb]", slugs(archived))
	}
}

func TestUpdateState_BootstrapsIfMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := UpdateState("fleet", "fresh-dddd", func(s *State) {
		s.Phase = PhaseBranch
	}); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	got, err := ReadState("fleet", "fresh-dddd")
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got.Phase != PhaseBranch {
		t.Errorf("Phase=%q; want branch", got.Phase)
	}
	if got.StartedAt.IsZero() {
		t.Errorf("StartedAt not set on bootstrap")
	}
}

// ---------- helpers ----------

func startSleeper(t *testing.T, d time.Duration) *exec.Cmd {
	t.Helper()
	if d <= 0 {
		bin, err := exec.LookPath("true")
		if err != nil {
			t.Skipf("no `true` in PATH: %v", err)
		}
		cmd := exec.Command(bin)
		if err := cmd.Start(); err != nil {
			t.Fatalf("exec true: %v", err)
		}
		return cmd
	}
	bin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no `sleep` in PATH: %v", err)
	}
	cmd := exec.Command(bin, fmt.Sprintf("%.1f", d.Seconds()))
	if err := cmd.Start(); err != nil {
		t.Fatalf("exec sleep: %v", err)
	}
	return cmd
}

// terminatedPID returns a PID that is reaped (no live process). The
// kernel may recycle it for another process at any moment, so this is
// best-effort — but `true` exits in microseconds and the test checks
// IsAlive immediately after, so race window is tiny on busy CI.
func terminatedPID(t *testing.T) int {
	t.Helper()
	bin, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("no `true` in PATH: %v", err)
	}
	cmd := exec.Command(bin)
	if err := cmd.Start(); err != nil {
		t.Fatalf("exec true: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	if IsAlive(pid) {
		t.Skipf("PID %d recycled too quickly; cannot test reliably", pid)
	}
	return pid
}

func startsWith(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return s[:len(prefix)] == prefix
}

func slugs(ss []*State) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Slug)
	}
	return out
}

// ---------- Delete (issue #101) ----------

func TestDelete_RemovesDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.EnsureProjectInitialized("fleet"); err != nil {
		t.Fatalf("init: %v", err)
	}
	dir, err := state.WorkerDir("fleet", "del-test-1a2b")
	if err != nil {
		t.Fatalf("WorkerDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Drop in a couple files including a nested dir to exercise rm -rf.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "output.log"), []byte("logs"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("nested mkdir: %v", err)
	}

	if err := Delete("fleet", "del-test-1a2b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir still exists: %v", err)
	}
}

func TestDelete_IdempotentOnMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.EnsureProjectInitialized("fleet"); err != nil {
		t.Fatalf("init: %v", err)
	}
	// First call on a never-existed slug returns nil.
	if err := Delete("fleet", "never-existed-9999"); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	// Same slug again — still nil.
	if err := Delete("fleet", "never-existed-9999"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestDelete_RejectsInvalidSlug(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.EnsureProjectInitialized("fleet"); err != nil {
		t.Fatalf("init: %v", err)
	}
	cases := []string{"", "..", "../escape", "/abs", "with/slash"}
	for _, slug := range cases {
		t.Run(slug, func(t *testing.T) {
			err := Delete("fleet", slug)
			if err == nil {
				t.Fatalf("Delete(%q) succeeded; want error", slug)
			}
			if !errors.Is(err, ErrInvalidSlug) {
				t.Fatalf("got %v; want ErrInvalidSlug", err)
			}
		})
	}
}

func TestDelete_RefusesArchiveSlug(t *testing.T) {
	// "archive" as a slug would resolve to workers/archive/, blowing
	// away every archived worker. Refuse explicitly.
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.EnsureProjectInitialized("fleet"); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Plant an archive dir to confirm Delete doesn't touch it.
	pdir, err := state.ProjectDir("fleet")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	archDir := filepath.Join(pdir, "workers", "archive", "old-1234-20260101-000000")
	if err := os.MkdirAll(archDir, 0o755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}

	err = Delete("fleet", "archive")
	if err == nil {
		t.Fatalf("Delete(archive) succeeded; want error")
	}
	if _, statErr := os.Stat(archDir); statErr != nil {
		t.Fatalf("archive dir was disturbed: %v", statErr)
	}
}

func TestDelete_LeavesProjectDirIntact(t *testing.T) {
	// After Delete, the project dir + workers/ root must still exist.
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.EnsureProjectInitialized("fleet"); err != nil {
		t.Fatalf("init: %v", err)
	}
	dir, err := state.WorkerDir("fleet", "leave-1a2b")
	if err != nil {
		t.Fatalf("WorkerDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := Delete("fleet", "leave-1a2b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	pdir, err := state.ProjectDir("fleet")
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	if _, err := os.Stat(pdir); err != nil {
		t.Fatalf("project dir gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pdir, "workers")); err != nil {
		t.Fatalf("workers root gone: %v", err)
	}
}

// ---------- three-stage review schema (reviewer-subagent-arch) ----------

// TestStateRoundTrip_ReviewFields confirms the new review_* fields are
// JSON-round-trippable. Schema-only commit — no enforcement on
// phase=push yet (that lands in commit 2). Workers pre-dating these
// fields read back zero values, which is fine.
func TestStateRoundTrip_ReviewFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	in := &State{
		Slug:                  "review-roundtrip-aaaa",
		Project:               "fleet",
		Phase:                 PhaseReviewDone,
		StartedAt:             time.Date(2026, 5, 11, 14, 0, 0, 0, time.UTC),
		PID:                   12345,
		ReviewAlphaStatus:     ReviewStatusSkipped,
		ReviewAlphaRounds:     3,
		ReviewAlphaSkipReason: "rate-limited",
		ReviewAlphaEngine:     ReviewEngineCodex,
		ReviewAlphaModel:      "codex-default",
		ReviewBetaStatus:      ReviewStatusPassed,
		ReviewBetaRounds:      1,
		ReviewBetaEngine:      ReviewEngineClaude,
		ReviewBetaModel:       "opus-4.8",
	}
	if err := WriteState("fleet", in.Slug, in); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	out, err := ReadState("fleet", in.Slug)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if out.ReviewAlphaStatus != ReviewStatusSkipped {
		t.Errorf("ReviewAlphaStatus=%q; want skipped", out.ReviewAlphaStatus)
	}
	if out.ReviewAlphaRounds != 3 {
		t.Errorf("ReviewAlphaRounds=%d; want 3", out.ReviewAlphaRounds)
	}
	if out.ReviewAlphaSkipReason != "rate-limited" {
		t.Errorf("ReviewAlphaSkipReason=%q; want rate-limited", out.ReviewAlphaSkipReason)
	}
	if out.ReviewAlphaEngine != ReviewEngineCodex || out.ReviewAlphaModel != "codex-default" {
		t.Errorf("alpha identity=(%q,%q); want codex/codex-default", out.ReviewAlphaEngine, out.ReviewAlphaModel)
	}
	if out.ReviewBetaStatus != ReviewStatusPassed {
		t.Errorf("ReviewBetaStatus=%q; want passed", out.ReviewBetaStatus)
	}
	if out.ReviewBetaRounds != 1 {
		t.Errorf("ReviewBetaRounds=%d; want 1", out.ReviewBetaRounds)
	}
}

// TestStateRoundTrip_ReviewFieldsOmittedWhenEmpty: pre-three-stage
// workers leave the new fields zero; the on-disk JSON should omit them
// so the file size doesn't bloat for every worker (omitempty pulls
// its weight here).
func TestStateRoundTrip_ReviewFieldsOmittedWhenEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	in := &State{
		Slug:      "no-review-aaaa",
		Project:   "fleet",
		Phase:     PhaseTDDGreen,
		StartedAt: time.Now().UTC(),
		PID:       1,
	}
	if err := WriteState("fleet", in.Slug, in); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	path, _ := stateJSONPath("fleet", in.Slug)
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read raw: %v", rerr)
	}
	body := string(raw)
	for _, field := range []string{
		"review_alpha_status", "review_alpha_rounds",
		"review_alpha_skip_reason", "review_alpha_engine", "review_alpha_model",
		"review_beta_status", "review_beta_rounds",
		"review_beta_skip_reason", "review_beta_engine", "review_beta_model",
	} {
		if containsSubstring(body, field) {
			t.Errorf("raw state.json contains %q; want omitted (omitempty)", field)
		}
	}
}

// TestStateRoundTrip_RejectsBogusReviewStatus: schema validation
// rejects unknown enum values at WriteState time, the same way it
// rejects unknown Phase values.
func TestStateRoundTrip_RejectsBogusReviewStatus(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	cases := []struct {
		name string
		mut  func(*State)
	}{
		{
			name: "alpha bogus",
			mut: func(s *State) {
				s.ReviewAlphaStatus = ReviewStatus("bogus")
			},
		},
		{
			name: "beta bogus",
			mut: func(s *State) {
				s.ReviewBetaStatus = ReviewStatus("nope")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &State{
				Slug:      "bogus-rev-" + sanitizeSlug(c.name),
				Project:   "fleet",
				Phase:     PhaseReviewClaude,
				StartedAt: time.Now().UTC(),
				PID:       1,
			}
			c.mut(s)
			err := WriteState("fleet", s.Slug, s)
			if !errors.Is(err, ErrInvalidReviewStat) {
				t.Errorf("got %v; want ErrInvalidReviewStat", err)
			}
		})
	}
}

func stateWithReview(project, slug string, phase Phase, alphaStatus ReviewStatus, alphaEngine, alphaModel, alphaReason string, betaStatus ReviewStatus, betaEngine, betaModel, betaReason string) *State {
	return &State{
		Slug:                  slug,
		Project:               project,
		Phase:                 phase,
		StartedAt:             time.Now().UTC(),
		PID:                   1,
		ReviewAlphaStatus:     alphaStatus,
		ReviewAlphaEngine:     alphaEngine,
		ReviewAlphaModel:      alphaModel,
		ReviewAlphaSkipReason: alphaReason,
		ReviewBetaStatus:      betaStatus,
		ReviewBetaEngine:      betaEngine,
		ReviewBetaModel:       betaModel,
		ReviewBetaSkipReason:  betaReason,
	}
}

func TestValidateReviewGate(t *testing.T) {
	cases := []struct {
		name    string
		gitMode bool
		state   func(project, slug string) *State
		wantErr error
	}{
		{
			name:    "both slots passed",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusPassed, ReviewEngineClaude, "sonnet-5", "", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
		},
		{
			name:    "one slot pending",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusPending, ReviewEngineClaude, "sonnet-5", "", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrPhaseRequiresReview,
		},
		{
			name:    "empty slots fail closed",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, "", "", "", "", "", "", "", "")
			},
			wantErr: ErrPhaseRequiresReview,
		},
		{
			name:    "passed slot with empty engine",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusPassed, "", "sonnet-5", "", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrReviewSlotIdentity,
		},
		{
			name:    "passed slot with empty model",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusPassed, ReviewEngineClaude, "", "", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrReviewSlotIdentity,
		},
		{
			name:    "invalid non-empty engine",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusPassed, "gpt", "gpt-5.5", "", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrReviewSlotIdentity,
		},
		{
			name:    "alpha codex skipped git rate limited",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusSkipped, ReviewEngineCodex, "codex-default", "rate-limited", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
		},
		{
			name:    "alpha claude skipped rejected",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusSkipped, ReviewEngineClaude, "sonnet-5", "rate-limited", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrPhaseRequiresReview,
		},
		{
			name:    "alpha codex skipped non-git rejected",
			gitMode: false,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhaseDone, ReviewStatusSkipped, ReviewEngineCodex, "codex-default", "rate-limited", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrPhaseRequiresReview,
		},
		{
			name:    "alpha codex skipped no-git reason rejected",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusSkipped, ReviewEngineCodex, "codex-default", "no-git", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrCodexSkipNeedsReason,
		},
		{
			name:    "alpha codex skipped empty reason rejected",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusSkipped, ReviewEngineCodex, "codex-default", "", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrCodexSkipNeedsReason,
		},
		{
			name:    "alpha codex skipped garbage reason rejected",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusSkipped, ReviewEngineCodex, "codex-default", "garbage", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrCodexSkipNeedsReason,
		},
		{
			name:    "alpha single-claude-degraded accepted",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusSingleClaudeDegraded, ReviewEngineClaude, "opus-4.8", "", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
		},
		{
			name:    "single-claude-degraded with mismatched models rejected",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusSingleClaudeDegraded, ReviewEngineClaude, "sonnet-5", "", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrReviewDegradedModelMismatch,
		},
		{
			name:    "alpha skipped in single-claude slot rejected",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusSkipped, ReviewEngineClaude, "opus-4.8", "rate-limited", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrPhaseRequiresReview,
		},
		{
			name:    "both slots codex skipped rejected",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusSkipped, ReviewEngineCodex, "codex-default", "rate-limited", ReviewStatusSkipped, ReviewEngineCodex, "codex-default", "rate-limited")
			},
			wantErr: ErrReviewBetaAnchor,
		},
		{
			name:    "beta engine codex rejected",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusPassed, ReviewEngineClaude, "sonnet-5", "", ReviewStatusPassed, ReviewEngineCodex, "codex-default", "")
			},
			wantErr: ErrReviewBetaAnchor,
		},
		{
			name:    "single-claude-degraded with codex engine rejected",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusSingleClaudeDegraded, ReviewEngineCodex, "codex-default", "", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrPhaseRequiresReview,
		},
		{
			name:    "gate does not fire off terminal phase",
			gitMode: true,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhaseReviewDone, "", "", "", "", "", "", "", "")
			},
		},
		{
			name:    "git done needs only pr url",
			gitMode: true,
			state: func(project, slug string) *State {
				s := stateWithReview(project, slug, PhaseDone, "", "", "", "", "", "", "", "")
				s.PRURL = "https://github.com/example/repo/pull/123"
				return s
			},
		},
		{
			name:    "non-git done is review gated",
			gitMode: false,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhaseDone, "", "", "", "", "", "", "", "")
			},
			wantErr: ErrPhaseRequiresReview,
		},
		{
			name:    "non-git push rejected",
			gitMode: false,
			state: func(project, slug string) *State {
				return stateWithReview(project, slug, PhasePush, ReviewStatusPassed, ReviewEngineClaude, "sonnet-5", "", ReviewStatusPassed, ReviewEngineClaude, "opus-4.8", "")
			},
			wantErr: ErrPhasePushNonGit,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("FLEET_HOME", tmp)
			project := "git-proj"
			if !c.gitMode {
				project = "ng-proj"
				writeNonGitProject(t, project)
			}
			slug := "gate-" + sanitizeSlug(c.name) + "-aaaa"
			err := WriteState(project, slug, c.state(project, slug))
			if c.wantErr == nil && err != nil {
				t.Fatalf("WriteState: %v; want nil", err)
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("WriteState: got %v; want %v", err, c.wantErr)
			}
		})
	}
}

// TestWorkers_PhaseDoneStillRequiresPR: phase=done's pr_url
// precondition is unchanged. The new review gate fires only on
// phase=push; phase=done's gate is independent.
func TestWorkers_PhaseDoneStillRequiresPR(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	s := &State{
		Slug:      "done-no-pr-aaaa",
		Project:   "fleet",
		Phase:     PhaseDone,
		StartedAt: time.Now().UTC(),
		PID:       1,
		PRURL:     "", // missing
	}
	err := WriteState("fleet", s.Slug, s)
	if !errors.Is(err, ErrPhaseRequiresPR) {
		t.Errorf("got %v; want ErrPhaseRequiresPR (done without pr_url, review terminal)", err)
	}
}

func TestPhaseValidation_NewReviewPhasesAccepted(t *testing.T) {
	// review-pending + review-done must be valid Phase values; the
	// validPhase check rejects unknowns and would block the three-stage
	// flow if we forgot to enumerate them.
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	for _, p := range []Phase{PhaseReviewPending, PhaseReviewDone} {
		s := &State{
			Slug:      "phase-" + sanitizeSlug(string(p)) + "-aaaa",
			Project:   "fleet",
			Phase:     p,
			StartedAt: time.Now().UTC(),
			PID:       1,
		}
		if err := WriteState("fleet", s.Slug, s); err != nil {
			t.Errorf("WriteState phase=%s: %v; want nil", p, err)
		}
	}
}

func containsSubstring(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func sanitizeSlug(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		case c == '-':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func TestDelete_LeavesSiblingsAlone(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.EnsureProjectInitialized("fleet"); err != nil {
		t.Fatalf("init: %v", err)
	}
	d1, err := state.WorkerDir("fleet", "tgt-1a2b")
	if err != nil {
		t.Fatalf("WorkerDir 1: %v", err)
	}
	d2, err := state.WorkerDir("fleet", "keep-cd34")
	if err != nil {
		t.Fatalf("WorkerDir 2: %v", err)
	}
	for _, d := range []string{d1, d2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
		if err := os.WriteFile(filepath.Join(d, "state.json"), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write %s: %v", d, err)
		}
	}

	if err := Delete("fleet", "tgt-1a2b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(d1); !os.IsNotExist(err) {
		t.Fatalf("target still present: %v", err)
	}
	if _, err := os.Stat(d2); err != nil {
		t.Fatalf("sibling was removed: %v", err)
	}
}

// writeNonGitProject seeds a meta.json with IsGit=false at the
// FLEET_HOME-scoped project tree. Tests for non-git workers call this
// before invoking WriteState so the validator sees the correct mode.
func writeNonGitProject(t *testing.T, project string) {
	t.Helper()
	m := projects.Meta{
		Schema:   projects.SchemaVersion,
		RepoPath: "/tmp/fake-non-git",
		AddedAt:  time.Now().UTC(),
		IsGit:    projects.BoolPtr(false),
	}
	if err := projects.Write(project, m); err != nil {
		t.Fatalf("seed non-git meta: %v", err)
	}
}

// TestUpdateStateGen_CAS is T2 (DESIGN §2.2): the generation-CAS'd
// writer rejects a stale generation (incl. when state.json is absent),
// accepts a matching one, and repairs a prior-generation file with a
// FRESH replacement — never a field-merge — so an old PR can't leak.
func TestUpdateStateGen_CAS(t *testing.T) {
	// 1. Stale write against a re-dispatched slug is REJECTED even when
	//    the file is ABSENT (a stale worker may not bootstrap/poison it).
	t.Run("stale rejected when file absent", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("FLEET_HOME", tmp)
		// task-row authority = 2; a stale gen-1 worker writes.
		err := UpdateStateGen("p", "absent-aaaa", 1, 2, func(s *State) {
			s.Phase = PhaseDone
			s.PRURL = "https://example.com/pr/1"
		})
		if !errors.Is(err, ErrStaleGeneration) {
			t.Fatalf("want ErrStaleGeneration, got %v", err)
		}
		// The file must NOT have been bootstrapped by the rejected write.
		if _, rerr := ReadState("p", "absent-aaaa"); !errors.Is(rerr, ErrNotFound) {
			t.Fatalf("stale write bootstrapped an absent file: ReadState err=%v", rerr)
		}
	})

	// 2. Matching generation bootstraps a fresh file + applies the update.
	t.Run("matching generation succeeds", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("FLEET_HOME", tmp)
		if err := UpdateStateGen("p", "match-bbbb", 1, 1, func(s *State) {
			s.Phase = PhaseBranch
		}); err != nil {
			t.Fatalf("matching gen should succeed: %v", err)
		}
		got, err := ReadState("p", "match-bbbb")
		if err != nil {
			t.Fatalf("ReadState: %v", err)
		}
		if got.DispatchGeneration != 1 {
			t.Errorf("stamped gen = %d, want 1", got.DispatchGeneration)
		}
		if got.Phase != PhaseBranch {
			t.Errorf("phase = %q, want branch", got.Phase)
		}
	})

	// 3. Stale write against an EXISTING current-gen file is rejected and
	//    does NOT clobber the live state.
	t.Run("stale rejected against present file", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("FLEET_HOME", tmp)
		// Live current attempt (gen 3) wrote in-progress + no PR.
		if err := UpdateStateGen("p", "live-cccc", 3, 3, func(s *State) {
			s.Phase = PhaseTDDGreen
		}); err != nil {
			t.Fatalf("seed live state: %v", err)
		}
		// A stale gen-2 worker tries to clobber it with phase=done.
		err := UpdateStateGen("p", "live-cccc", 2, 3, func(s *State) {
			s.Phase = PhaseDone
			s.PRURL = "https://example.com/pr/stale"
		})
		if !errors.Is(err, ErrStaleGeneration) {
			t.Fatalf("want ErrStaleGeneration, got %v", err)
		}
		got, _ := ReadState("p", "live-cccc")
		if got.Phase != PhaseTDDGreen || got.PRURL != "" {
			t.Errorf("stale write clobbered live state: phase=%q pr=%q", got.Phase, got.PRURL)
		}
		if got.DispatchGeneration != 3 {
			t.Errorf("gen = %d, want 3 (unchanged)", got.DispatchGeneration)
		}
	})

	// 3b. Codex iter-1 [P1]: a HIGHER on-disk generation is rejected, NOT
	//     merged-and-downgraded. The CAS authority is read outside the
	//     worker lock, so a stale worker can pass n==taskGen==N while a
	//     newer attempt already bootstrapped state.json at N+1. Merging
	//     would clobber the live newer attempt.
	t.Run("rejected when on-disk gen is newer than authority", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("FLEET_HOME", tmp)
		// Current attempt already wrote gen 5.
		if err := UpdateStateGen("p", "newer-eeee", 5, 5, func(s *State) {
			s.Phase = PhaseTDDGreen
		}); err != nil {
			t.Fatalf("seed gen-5 state: %v", err)
		}
		// A racing stale writer read authority N=4 (before the bump) and
		// arrives with n==taskGen==4. The on-disk file is already gen 5.
		err := UpdateStateGen("p", "newer-eeee", 4, 4, func(s *State) {
			s.Phase = PhaseDone
			s.PRURL = "https://example.com/pr/stale"
		})
		if !errors.Is(err, ErrStaleGeneration) {
			t.Fatalf("higher on-disk gen must reject, got %v", err)
		}
		got, _ := ReadState("p", "newer-eeee")
		if got.Phase != PhaseTDDGreen || got.DispatchGeneration != 5 {
			t.Errorf("live newer state was clobbered: phase=%q gen=%d",
				got.Phase, got.DispatchGeneration)
		}
	})

	// 4. Repair: the current generation overwriting a PRIOR-generation
	//    file is a FRESH replacement — empty pr_url/review/phases + new
	//    started_at — never a field-merge (so an old PR can't leak into
	//    the branch→PR fallback).
	t.Run("repair is fresh replacement not merge", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("FLEET_HOME", tmp)
		// Prior attempt (gen 1) left a terminal state with a PR + review
		// status + completed phases on disk.
		if err := UpdateStateGen("p", "repair-dddd", 1, 1, func(s *State) {
			s.Phase = PhaseReviewDone
			s.PhasesCompleted = []Phase{PhaseBranch, PhaseTDDGreen}
			s.ReviewAlphaStatus = ReviewStatusPassed
			s.ReviewBetaStatus = ReviewStatusPassed
		}); err != nil {
			t.Fatalf("seed prior-gen state: %v", err)
		}
		oldStarted := func() time.Time {
			s, _ := ReadState("p", "repair-dddd")
			return s.StartedAt
		}()
		// Re-dispatch bumps the authority to 2; the current worker's
		// bootstrap write (gen 2) repairs the gen-1 file.
		if err := UpdateStateGen("p", "repair-dddd", 2, 2, func(s *State) {
			s.Phase = PhaseStarting
		}); err != nil {
			t.Fatalf("repair write should succeed: %v", err)
		}
		got, err := ReadState("p", "repair-dddd")
		if err != nil {
			t.Fatalf("ReadState: %v", err)
		}
		if got.DispatchGeneration != 2 {
			t.Errorf("gen = %d, want 2", got.DispatchGeneration)
		}
		if got.PRURL != "" {
			t.Errorf("repair leaked old pr_url %q (must be fresh)", got.PRURL)
		}
		if got.ReviewAlphaStatus != "" || got.ReviewBetaStatus != "" {
			t.Errorf("repair leaked old review status: alpha=%q beta=%q",
				got.ReviewAlphaStatus, got.ReviewBetaStatus)
		}
		if len(got.PhasesCompleted) != 0 {
			t.Errorf("repair leaked completed phases: %v", got.PhasesCompleted)
		}
		if !got.StartedAt.After(oldStarted) && !got.StartedAt.Equal(oldStarted) {
			// started_at is reset to a fresh now(); it must not be older
			// than the prior attempt's (a merge would keep the old one,
			// but a fresh one is >= old).
			t.Errorf("repair started_at %v older than prior %v", got.StartedAt, oldStarted)
		}
	})
}

// readFleetlogLines scans all *.jsonl in dir and returns parsed JSON lines.
func readFleetlogLines(t *testing.T, dir string) []map[string]any {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fleetlog dir %s: %v", dir, err)
	}
	var out []map[string]any
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, ln := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if ln == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(ln), &m); err != nil {
				t.Fatalf("invalid JSON in %s: %q: %v", e.Name(), ln, err)
			}
			out = append(out, m)
		}
	}
	return out
}

// TestUpdateStateEmitsFleetlog verifies that UpdateState emits a
// state.transition fleetlog event on every phase change. This is the P1 fix:
// the real `fleet workers update` code path uses UpdateState/UpdateStateGen,
// not WriteState, so agents reading the debug log need transitions from those
// paths to reconstruct the full worker lifecycle.
func TestUpdateStateEmitsFleetlog(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	t.Setenv("XDG_STATE_HOME", "")
	logDir := fleetlog.Dir()

	if err := UpdateState("proj", "slug-fl1", func(s *State) {
		s.Phase = PhaseBranch
	}); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	lines := readFleetlogLines(t, logDir)
	var found bool
	for _, m := range lines {
		if m["type"] == "state.transition" && m["slug"] == "slug-fl1" {
			found = true
			data, _ := m["data"].(map[string]any)
			if data["to"] != string(PhaseBranch) {
				t.Errorf("state.transition data.to want %q got %v", PhaseBranch, data["to"])
			}
		}
	}
	if !found {
		t.Errorf("UpdateState: no state.transition fleetlog event for slug-fl1; lines: %v", lines)
	}
}

// TestUpdateStateGenEmitsFleetlog verifies that UpdateStateGen (the CAS writer
// used by `fleet workers update --dispatch-generation`) also emits
// state.transition events.
func TestUpdateStateGenEmitsFleetlog(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	t.Setenv("XDG_STATE_HOME", "")
	logDir := fleetlog.Dir()

	if err := UpdateStateGen("proj", "slug-fl2", 1, 1, func(s *State) {
		s.Phase = PhaseTDDGreen
	}); err != nil {
		t.Fatalf("UpdateStateGen: %v", err)
	}
	lines := readFleetlogLines(t, logDir)
	var found bool
	for _, m := range lines {
		if m["type"] == "state.transition" && m["slug"] == "slug-fl2" {
			found = true
			data, _ := m["data"].(map[string]any)
			if data["to"] != string(PhaseTDDGreen) {
				t.Errorf("state.transition data.to want %q got %v", PhaseTDDGreen, data["to"])
			}
		}
	}
	if !found {
		t.Errorf("UpdateStateGen: no state.transition fleetlog event for slug-fl2; lines: %v", lines)
	}
}

// TestUpdateStateNoPhaseChangeOmitsFleetlog verifies that a metadata-only
// UpdateState call (e.g. `fleet workers update --review-*` which bumps
// review counters without changing Phase) does NOT emit a state.transition
// event. A bogus "review-done -> review-done" log line would mislead
// agents reconstructing worker lifecycle from the debug log.
func TestUpdateStateNoPhaseChangeOmitsFleetlog(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	t.Setenv("XDG_STATE_HOME", "")
	logDir := fleetlog.Dir()

	// Seed initial state at review-done.
	if err := WriteState("proj", "slug-fl3", &State{
		Slug: "slug-fl3", Project: "proj", Phase: PhaseReviewDone,
		ReviewAlphaStatus: ReviewStatusPassed,
		ReviewBetaStatus:  ReviewStatusPassed,
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	// Metadata-only update — phase stays review-done.
	if err := UpdateState("proj", "slug-fl3", func(s *State) {
		s.ReviewAlphaRounds = 2 // bump counter; Phase unchanged
	}); err != nil {
		t.Fatalf("UpdateState no-phase-change: %v", err)
	}
	lines := readFleetlogLines(t, logDir)
	var bogus int
	for _, m := range lines {
		if m["type"] == "state.transition" && m["slug"] == "slug-fl3" {
			d, _ := m["data"].(map[string]any)
			if d["from"] == d["to"] {
				bogus++
			}
		}
	}
	if bogus > 0 {
		t.Errorf("UpdateState no-phase-change: emitted %d bogus state.transition(s) with from==to", bogus)
	}
}

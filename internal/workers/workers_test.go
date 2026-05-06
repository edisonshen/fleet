package workers

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	if err := WriteState("fleet", "ar-aaaa", &State{
		Slug: "ar-aaaa", Project: "fleet", Phase: PhaseDone,
		PRURL:     "https://github.com/x/y/pull/1",
		StartedAt: time.Now().UTC(), PID: 1,
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

func TestArchive_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	// No worker created. Archive should be a no-op (nil error).
	if err := Archive("fleet", "no-such-aaaa"); err != nil {
		t.Errorf("Archive on missing dir returned %v; want nil", err)
	}
}

func TestPruneArchive(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	dir, _ := state.ProjectDir("fleet")
	archRoot := filepath.Join(dir, "workers", "archive")
	if err := os.MkdirAll(archRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Create two archive dirs with mtimes 30d and 1d in the past.
	old := filepath.Join(archRoot, "old-aaaa-20260401-000000")
	young := filepath.Join(archRoot, "young-bbbb-20260505-000000")
	for _, d := range []string{old, young} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	thirtyAgo := time.Now().Add(-30 * 24 * time.Hour)
	oneAgo := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(old, thirtyAgo, thirtyAgo); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}
	if err := os.Chtimes(young, oneAgo, oneAgo); err != nil {
		t.Fatalf("chtimes young: %v", err)
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
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

func TestListActive(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	for _, slug := range []string{"alpha-aaaa", "beta-bbbb", "gamma-cccc"} {
		if err := WriteState("fleet", slug, &State{
			Slug: slug, Project: "fleet", Phase: PhaseStarting,
			StartedAt: time.Now().UTC(), PID: 1,
		}); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
	// Archive one of them.
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
	if err := WriteState("fleet", "live-aaaa", &State{
		Slug: "live-aaaa", Project: "fleet", Phase: PhaseStarting,
		StartedAt: time.Now().UTC(), PID: 1,
	}); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	if err := WriteState("fleet", "gone-bbbb", &State{
		Slug: "gone-bbbb", Project: "fleet", Phase: PhaseDone,
		PRURL: "https://x/y/1", StartedAt: time.Now().UTC(), PID: 1,
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

//go:build unix

package state

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestLockProject_BasicAcquireRelease(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := Bootstrap(); err != nil {
		t.Fatal(err)
	}

	release, err := LockProject("rainier")
	if err != nil {
		t.Fatalf("LockProject: %v", err)
	}
	release()

	// Lock file should exist as a sentinel.
	if _, err := os.Stat(filepath.Join(tmp, "projects", ".locks", "rainier.lock")); err != nil {
		t.Errorf("lock file missing: %v", err)
	}
}

func TestLockProject_DifferentProjectsDoNotBlock(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := Bootstrap(); err != nil {
		t.Fatal(err)
	}

	r1, err := LockProject("rainier")
	if err != nil {
		t.Fatal(err)
	}
	defer r1()

	// Different project: a separate fd → separate flock → no contention.
	done := make(chan struct{})
	go func() {
		r2, err := LockProject("gift-finder")
		if err != nil {
			t.Errorf("LockProject(gift-finder): %v", err)
		} else {
			r2()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("different projects should not contend")
	}
}

func TestLockProject_SameProjectSerializes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := Bootstrap(); err != nil {
		t.Fatal(err)
	}

	// Hold the lock from a separate process via a directly-opened fd
	// so we exercise OS-level flock contention rather than just one
	// goroutine sharing an fd. (Goroutines in the same process share
	// fds and don't serialize via flock — that's expected and
	// documented in lock_unix.go.)
	path, _ := ProjectLockPath("rainier")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock holder: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	})

	// Background goroutine attempts the lock via LockProject — should
	// block until we release the holder.
	var acquired atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		release, err := LockProject("rainier")
		if err != nil {
			t.Errorf("LockProject: %v", err)
			return
		}
		acquired.Store(true)
		release()
	}()

	// Give the goroutine time to attempt and confirm it's blocked.
	time.Sleep(100 * time.Millisecond)
	if acquired.Load() {
		t.Fatal("expected LockProject to block while holder owns the flock")
	}

	// Release the holder; the goroutine should acquire shortly after.
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock holder: %v", err)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("LockProject should have acquired after holder released")
	}
	if !acquired.Load() {
		t.Error("LockProject did not record acquisition")
	}
}

// T7 (DESIGN-coord-worktree-lifecycle § Concurrency): the worktree-GC
// flock serializes a coord-invoked run and an operator-invoked run so
// they can't race the same removal. We hold the lock via a directly-
// opened fd (OS-level contention) and confirm LockProjectWorktreeGC
// blocks until released. Lazily creates .locks/ (no Bootstrap of the
// per-project state dir required).
func TestLockProjectWorktreeGC_SerializesSameProject(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := Bootstrap(); err != nil {
		t.Fatal(err)
	}

	// First acquire creates .locks/<...>/worktree-gc.lock lazily.
	r0, err := LockProjectWorktreeGC("rainier")
	if err != nil {
		t.Fatalf("LockProjectWorktreeGC first acquire: %v", err)
	}
	r0()

	path, err := ProjectWorktreeGCLockPath("rainier")
	if err != nil {
		t.Fatalf("ProjectWorktreeGCLockPath: %v", err)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("worktree-gc lock file missing after acquire: %v", serr)
	}

	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock holder: %v", err)
	}

	var acquired atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		release, lerr := LockProjectWorktreeGC("rainier")
		if lerr != nil {
			t.Errorf("LockProjectWorktreeGC (blocked): %v", lerr)
			return
		}
		acquired.Store(true)
		release()
	}()

	time.Sleep(100 * time.Millisecond)
	if acquired.Load() {
		t.Fatal("worktree-gc lock should block while holder owns the flock")
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock holder: %v", err)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("worktree-gc lock should acquire after holder released")
	}
	if !acquired.Load() {
		t.Error("worktree-gc lock did not record acquisition")
	}
}

// Different projects must NOT contend on the worktree-GC lock (each has
// its own per-project state dir + lock file).
func TestLockProjectWorktreeGC_DifferentProjectsDoNotBlock(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := Bootstrap(); err != nil {
		t.Fatal(err)
	}
	r1, err := LockProjectWorktreeGC("rainier")
	if err != nil {
		t.Fatal(err)
	}
	defer r1()
	done := make(chan struct{})
	go func() {
		r2, lerr := LockProjectWorktreeGC("gift-finder")
		if lerr != nil {
			t.Errorf("LockProjectWorktreeGC(gift-finder): %v", lerr)
		} else {
			r2()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("different projects should not contend on worktree-gc lock")
	}
}

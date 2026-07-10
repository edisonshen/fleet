//go:build linux || darwin

package coordreconcile_test

import (
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
	"github.com/edisonshen/fleet/internal/state"
)

// setupReconcileHome points FLEET_HOME at a temp dir + bootstraps it so the real
// coordlock lease primitives (AcquireLease / the flock readers) resolve a
// per-project .locks dir. Used by the integration test below.
func setupReconcileHome(t *testing.T) {
	t.Helper()
	t.Setenv("FLEET_HOME", t.TempDir())
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
}

// TestResolve_RealFlockReaders is the flock-only INTEGRATION test: it drives
// coordreconcile.Resolve with the REAL coordlock readers (DefaultDeps) against a
// REAL held flock (via coordlock.AcquireLease), crossing the
// coordlock→coordreconcile boundary the unit table fakes. Covers T1 (busy coord
// with identity ⇒ Attach; freed ⇒ Spawn) and T13 (identity-less body ⇒ Wait).
//
// Build-tagged linux||darwin (codex PR2-review iter-2 [P2]): coordlock.AcquireLease
// is itself gated to those GOOS values (lease.go), so an untagged test file
// calling it directly failed to compile on every other platform
// (`undefined: coordlock.AcquireLease`) even though the package under test,
// coordreconcile, compiles everywhere. The platform-independent table tests
// (TestResolveMatrix, TestResolveOrdering) stay in coordreconcile_test.go —
// they only exercise reconciletest fakes, never the real lease primitive.
func TestResolve_RealFlockReaders(t *testing.T) {
	t.Run("T1_busy_coord_with_identity_attaches_then_freed_spawns", func(t *testing.T) {
		setupReconcileHome(t)
		const project = "resolve-integ-attach"
		lease, acquired, err := coordlock.AcquireLease(project, "holder01")
		if err != nil || !acquired {
			t.Fatalf("AcquireLease: acquired=%v err=%v", acquired, err)
		}
		t.Cleanup(lease.Release)

		// Busy flock with a stamped identity ⇒ Attach — never spawn beside it
		// (the incident regression), even though the epoch is only `starting`.
		v, err := coordreconcile.Resolve(coordreconcile.DefaultDeps(), project, "newcaller")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if v.Decision != coordreconcile.Attach || v.Owner.AgentID != "holder01" {
			t.Fatalf("busy coord ⇒ Attach holder01; got %s owner=%q", v.Decision, v.Owner.AgentID)
		}

		// Release frees the flock ⇒ a fresh Resolve now Spawns.
		lease.Release()
		v, err = coordreconcile.Resolve(coordreconcile.DefaultDeps(), project, "newcaller")
		if err != nil {
			t.Fatalf("Resolve after release: %v", err)
		}
		if v.Decision != coordreconcile.Spawn {
			t.Fatalf("freed lease ⇒ Spawn; got %s (reason=%q)", v.Decision, v.Reason)
		}
	})

	t.Run("T13_identity_less_flock_waits", func(t *testing.T) {
		setupReconcileHome(t)
		const project = "resolve-integ-wait"
		// An empty agentID stamps an identity-less body (the old-binary / torn
		// body case) — the flock is held but names no agent.
		lease, acquired, err := coordlock.AcquireLease(project, "")
		if err != nil || !acquired {
			t.Fatalf("AcquireLease: acquired=%v err=%v", acquired, err)
		}
		t.Cleanup(lease.Release)
		v, err := coordreconcile.Resolve(coordreconcile.DefaultDeps(), project, "newcaller")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if v.Decision != coordreconcile.Wait {
			t.Fatalf("flock held with identity-less body ⇒ Wait; got %s owner=%q", v.Decision, v.Owner.AgentID)
		}
	})
}

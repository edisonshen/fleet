package spawn

import (
	"errors"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/testutil/tmuxtest"
	"github.com/edisonshen/fleet/internal/tmux"
)

type refuseBackend struct {
	spawnCount int
}

func installRefuseBackend(t *testing.T) *refuseBackend {
	t.Helper()
	b := &refuseBackend{}
	restore := tmux.Install(b)
	t.Cleanup(restore)
	return b
}

func isolateTmuxSocket(t *testing.T) string {
	t.Helper()
	return tmuxtest.IsolateSocket(t)
}

func (b *refuseBackend) Available() error { return nil }

func (b *refuseBackend) HasSession(string) bool { return false }

func (b *refuseBackend) SessionAlive(string) (bool, error) { return false, nil }

func (b *refuseBackend) Spawn(string, string, []string, []string) error {
	b.spawnCount++
	return nil
}

func (b *refuseBackend) Attach(string) error { return nil }

func (b *refuseBackend) SendKeys(string, ...string) error { return nil }

func (b *refuseBackend) CapturePane(string) ([]byte, error) { return nil, nil }

func (b *refuseBackend) SetStatusHint(string, string) error { return nil }

func (b *refuseBackend) Kill(string) error { return nil }

func (b *refuseBackend) ListSessions() ([]string, error) { return nil, nil }

func (b *refuseBackend) ListSessionsWithCreated() ([]tmux.SessionInfo, error) {
	return nil, nil
}

func restoreSpawnLeaseSeams(t *testing.T) {
	t.Helper()
	origExecutable := executablePath
	origLeaseSupported := leaseSupportedProbe
	origRealBackend := realTmuxBackendProbe
	t.Cleanup(func() {
		executablePath = origExecutable
		leaseSupportedProbe = origLeaseSupported
		realTmuxBackendProbe = origRealBackend
	})
}

func TestSpawn_CoordLeaseUnsupportedRefusesBeforeTmux(t *testing.T) {
	isolateTmuxSocket(t)
	restoreSpawnLeaseSeams(t)
	f := installRefuseBackend(t)
	setupFleetHome(t)

	leaseSupportedProbe = func() bool { return false }
	realTmuxBackendProbe = func() bool {
		t.Fatal("real backend probe should not run after lease support refusal")
		return false
	}
	executablePath = func() (string, error) {
		t.Fatal("executable path should not be resolved after lease support refusal")
		return "", nil
	}

	const id = "nogoos01"
	_, err := Spawn(Options{
		TaskID:         "coord-rainier",
		Project:        "rainier",
		Cwd:            t.TempDir(),
		Command:        []string{"sleep", "30"},
		PreAllocatedID: id,
	})
	if err == nil {
		t.Fatal("expected unsupported lease coord spawn to refuse")
	}
	if !strings.Contains(err.Error(), "coordinator lease is unsupported") {
		t.Fatalf("error = %v, want unsupported-lease refusal", err)
	}
	if f.spawnCount != 0 {
		t.Fatalf("tmux backend recorded %d spawn(s), want 0 after refusal", f.spawnCount)
	}
	if _, lerr := agent.Load(id); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("agent record for refused coord exists or load failed unexpectedly: %v", lerr)
	}
}

func TestSpawn_CoordExecutableFailureRefusesWithoutBareSession(t *testing.T) {
	isolateTmuxSocket(t)
	restoreSpawnLeaseSeams(t)
	f := installRefuseBackend(t)
	setupFleetHome(t)

	leaseSupportedProbe = func() bool { return true }
	realTmuxBackendProbe = func() bool { return true }
	exeErr := errors.New("exec path unavailable")
	executablePath = func() (string, error) { return "", exeErr }

	const id = "execline1"
	_, err := Spawn(Options{
		TaskID:         "coord-rainier",
		Project:        "rainier",
		Cwd:            t.TempDir(),
		Command:        []string{"sleep", "30"},
		PreAllocatedID: id,
	})
	if !errors.Is(err, exeErr) {
		t.Fatalf("Spawn error = %v, want wrapped executable error %v", err, exeErr)
	}
	if !strings.Contains(err.Error(), "coordinator lease supervisor required") {
		t.Fatalf("error = %v, want loud coord supervisor refusal", err)
	}
	if f.spawnCount != 0 {
		t.Fatalf("tmux backend recorded %d spawn(s), want 0; must not spawn bare coord", f.spawnCount)
	}
	if _, lerr := agent.Load(id); !errors.Is(lerr, state.ErrNotFound) {
		t.Fatalf("agent record for refused coord exists or load failed unexpectedly: %v", lerr)
	}
}

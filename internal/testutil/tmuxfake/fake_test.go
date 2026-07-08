package tmuxfake

import (
	"errors"
	"testing"

	"github.com/edisonshen/fleet/internal/tmux"
)

// TestFake_SpawnLifecycle exercises the create -> probe -> kill lifecycle
// through the installed seam so the test drives tmux.Spawn / tmux.Kill,
// not the Fake methods directly.
func TestFake_SpawnLifecycle(t *testing.T) {
	f := InstallFake(t)

	if tmux.HasSession("fleet-a") {
		t.Fatal("session should not exist before spawn")
	}
	if err := tmux.Spawn("fleet-a", "/tmp", []string{"sh", "-c", "sleep 1"}, []string{"K=V"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !tmux.HasSession("fleet-a") {
		t.Fatal("session should exist after spawn")
	}
	alive, err := tmux.SessionAlive("fleet-a")
	if err != nil || !alive {
		t.Fatalf("SessionAlive = (%v, %v), want (true, nil)", alive, err)
	}
	if got := f.SessionCommand("fleet-a"); len(got) != 3 || got[0] != "sh" {
		t.Fatalf("recorded command = %v", got)
	}
	if got := f.SessionEnv("fleet-a"); len(got) != 1 || got[0] != "K=V" {
		t.Fatalf("recorded env = %v", got)
	}
	if err := tmux.Kill("fleet-a"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if tmux.HasSession("fleet-a") {
		t.Fatal("session should be gone after kill")
	}
}

// TestFake_SpawnDuplicate mirrors real tmux rejecting a duplicate session
// name.
func TestFake_SpawnDuplicate(t *testing.T) {
	InstallFake(t)
	if err := tmux.Spawn("fleet-dup", "", []string{"sh"}, nil); err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	if err := tmux.Spawn("fleet-dup", "", []string{"sh"}, nil); err == nil {
		t.Fatal("duplicate Spawn should error")
	}
}

// TestFake_SpawnEmptyCommand mirrors realSpawn's empty-command rejection.
func TestFake_SpawnEmptyCommand(t *testing.T) {
	InstallFake(t)
	if err := tmux.Spawn("fleet-x", "", nil, nil); err == nil {
		t.Fatal("empty command should error")
	}
}

// TestFake_KillIdempotent mirrors realKill returning nil for a missing
// session (cleanup paths depend on this).
func TestFake_KillIdempotent(t *testing.T) {
	InstallFake(t)
	if err := tmux.Kill("fleet-missing"); err != nil {
		t.Fatalf("Kill on missing session = %v, want nil", err)
	}
}

// TestFake_SendKeysRecords records key sequences and rejects sends to a
// dead session with ErrNoSession.
func TestFake_SendKeysRecords(t *testing.T) {
	f := InstallFake(t)
	if err := tmux.Spawn("fleet-s", "", []string{"sh"}, nil); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := tmux.SendKeys("fleet-s", "/exit", "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if got := f.Keys("fleet-s"); len(got) != 2 || got[0] != "/exit" || got[1] != "Enter" {
		t.Fatalf("recorded keys = %v", got)
	}
	_ = tmux.Kill("fleet-s")
	if err := tmux.SendKeys("fleet-s", "x"); !errors.Is(err, tmux.ErrNoSession) {
		t.Fatalf("SendKeys to dead session = %v, want ErrNoSession", err)
	}
}

// TestFake_CapturePane returns seeded output and ErrNoSession for a dead
// session.
func TestFake_CapturePane(t *testing.T) {
	f := InstallFake(t)
	if err := tmux.Spawn("fleet-c", "", []string{"sh"}, nil); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Blank pane before seeding.
	if out, err := tmux.CapturePane("fleet-c"); err != nil || len(out) != 0 {
		t.Fatalf("CapturePane blank = (%q, %v)", out, err)
	}
	f.SetCaptureOutput("fleet-c", []byte("ready> "))
	out, err := tmux.CapturePane("fleet-c")
	if err != nil || string(out) != "ready> " {
		t.Fatalf("CapturePane = (%q, %v)", out, err)
	}
	_ = tmux.Kill("fleet-c")
	if _, err := tmux.CapturePane("fleet-c"); !errors.Is(err, tmux.ErrNoSession) {
		t.Fatalf("CapturePane dead = %v, want ErrNoSession", err)
	}
}

// TestFake_KillClearsStaleCaptureOutput pins codex iter-2 [P3]: a
// session re-spawned under the same name after a Kill must start with a
// BLANK pane, not the prior session's seeded capture bytes (real tmux
// gives the replacement a fresh pane). Covers both the Kill path and the
// re-Spawn-without-Kill path.
func TestFake_KillClearsStaleCaptureOutput(t *testing.T) {
	f := InstallFake(t)
	if err := tmux.Spawn("fleet-reuse", "", []string{"sh"}, nil); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.SetCaptureOutput("fleet-reuse", []byte("OLD PANE"))

	// Path 1: Kill then re-Spawn the same name → blank pane.
	_ = tmux.Kill("fleet-reuse")
	if err := tmux.Spawn("fleet-reuse", "", []string{"sh"}, nil); err != nil {
		t.Fatalf("re-Spawn after Kill: %v", err)
	}
	if out, err := tmux.CapturePane("fleet-reuse"); err != nil || len(out) != 0 {
		t.Fatalf("pane after Kill+reSpawn = (%q, %v), want blank", out, err)
	}

	// Path 2: re-Spawn WITHOUT an intervening Kill also clears stale bytes.
	f.SetCaptureOutput("fleet-reuse", []byte("STALE"))
	if err := tmux.Spawn("fleet-reuse", "", []string{"sh"}, nil); err == nil {
		t.Fatal("re-Spawn of a live session should fail (duplicate)")
	}
	// The duplicate Spawn is rejected BEFORE mutating state, so the
	// seeded bytes survive — that's correct (no new pane was created).
	if out, _ := tmux.CapturePane("fleet-reuse"); string(out) != "STALE" {
		t.Fatalf("rejected dup Spawn must not clear capture; got %q", out)
	}
	// Now Kill + Spawn fresh: blank again.
	_ = tmux.Kill("fleet-reuse")
	if err := tmux.Spawn("fleet-reuse", "", []string{"sh"}, nil); err != nil {
		t.Fatalf("Spawn fresh: %v", err)
	}
	if out, _ := tmux.CapturePane("fleet-reuse"); len(out) != 0 {
		t.Fatalf("fresh pane = %q, want blank", out)
	}
}

// TestFake_ListSessions returns sorted live sessions and creation info.
func TestFake_ListSessions(t *testing.T) {
	InstallFake(t)
	for _, s := range []string{"fleet-b", "fleet-a"} {
		if err := tmux.Spawn(s, "", []string{"sh"}, nil); err != nil {
			t.Fatalf("Spawn %s: %v", s, err)
		}
	}
	names, err := tmux.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(names) != 2 || names[0] != "fleet-a" || names[1] != "fleet-b" {
		t.Fatalf("ListSessions = %v, want sorted [fleet-a fleet-b]", names)
	}
	infos, err := tmux.ListSessionsWithCreated()
	if err != nil || len(infos) != 2 {
		t.Fatalf("ListSessionsWithCreated = (%v, %v)", infos, err)
	}
	if infos[0].Name != "fleet-a" || infos[0].Created.IsZero() {
		t.Fatalf("first info = %+v, want named+created", infos[0])
	}
}

// TestFake_AvailableOverride exercises the version-gate error path.
func TestFake_AvailableOverride(t *testing.T) {
	f := InstallFake(t)
	if err := tmux.Available(); err != nil {
		t.Fatalf("default Available = %v, want nil", err)
	}
	want := errors.New("tmux too old")
	f.SetAvailable(want)
	if err := tmux.Available(); !errors.Is(err, want) {
		t.Fatalf("Available after override = %v, want %v", err, want)
	}
}

// TestFake_AttachOnDeadSession returns ErrNoSession; on live, nil.
func TestFake_AttachOnDeadSession(t *testing.T) {
	InstallFake(t)
	if err := tmux.Attach("fleet-none"); !errors.Is(err, tmux.ErrNoSession) {
		t.Fatalf("Attach dead = %v, want ErrNoSession", err)
	}
	if err := tmux.Spawn("fleet-live", "", []string{"sh"}, nil); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := tmux.Attach("fleet-live"); err != nil {
		t.Fatalf("Attach live = %v, want nil", err)
	}
}

// TestFake_SetStatusHint records the hint.
func TestFake_SetStatusHint(t *testing.T) {
	f := InstallFake(t)
	if err := tmux.Spawn("fleet-h", "", []string{"sh"}, nil); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := tmux.SetStatusHint("fleet-h", "Ctrl-b d to detach"); err != nil {
		t.Fatalf("SetStatusHint: %v", err)
	}
	if got := f.StatusHint("fleet-h"); got != "Ctrl-b d to detach" {
		t.Fatalf("StatusHint = %q", got)
	}
}

// TestFake_RestoresRealBackend proves t.Cleanup puts the real backend
// back so a later non-faked test (or production) is unaffected. We can't
// assert production behavior here, but we can assert a fresh Install in a
// subtest sees an empty fake (no leakage from a prior fake's sessions).
func TestFake_RestoresRealBackend(t *testing.T) {
	t.Run("first", func(t *testing.T) {
		f := InstallFake(t)
		if tmux.IsRealBackend() {
			t.Fatal("fake install reported real backend")
		}
		_ = tmux.Spawn("fleet-leak", "", []string{"sh"}, nil)
		if f.Count() != 1 {
			t.Fatal("expected 1 session in first fake")
		}
	})
	t.Run("second", func(t *testing.T) {
		f := InstallFake(t)
		if tmux.IsRealBackend() {
			t.Fatal("fake install reported real backend")
		}
		if f.Count() != 0 {
			t.Fatalf("second fake leaked %d sessions from first", f.Count())
		}
		if tmux.HasSession("fleet-leak") {
			t.Fatal("session from first subtest leaked into second")
		}
	})
	if !tmux.IsRealBackend() {
		t.Fatal("fake cleanup did not restore real backend")
	}
}

// TestFake_FailSpawn exercises the injected spawn-failure path.
func TestFake_FailSpawn(t *testing.T) {
	f := InstallFake(t)
	boom := errors.New("unwritable socket")
	f.SetFailSpawn(boom)
	if err := tmux.Spawn("fleet-f", "", []string{"sh"}, nil); !errors.Is(err, boom) {
		t.Fatalf("Spawn with failSpawn = %v, want %v", err, boom)
	}
}

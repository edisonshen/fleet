package rc

import (
	"errors"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/projects"
)

func TestResolveWorkingDir_OverrideWins(t *testing.T) {
	withFleetHome(t)
	got, err := ResolveWorkingDir("demo", "/explicit/override")
	if err != nil {
		t.Fatalf("ResolveWorkingDir: %v", err)
	}
	if got != "/explicit/override" {
		t.Fatalf("override should win; got %q", got)
	}
}

func TestResolveWorkingDir_MetaJSON(t *testing.T) {
	withFleetHome(t)
	prev := agentList
	agentList = func() ([]*agent.Record, error) { return nil, nil }
	defer func() { agentList = prev }()

	if err := projects.Write("demo", projects.Meta{
		Schema:   projects.SchemaVersion,
		RepoPath: "/from/meta",
		AddedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("projects.Write: %v", err)
	}
	got, err := ResolveWorkingDir("demo", "")
	if err != nil {
		t.Fatalf("ResolveWorkingDir: %v", err)
	}
	if got != "/from/meta" {
		t.Fatalf("expected meta.json repo_path; got %q", got)
	}
}

func TestResolveWorkingDir_LiveCoord(t *testing.T) {
	withFleetHome(t)
	prev := agentList
	agentList = func() ([]*agent.Record, error) {
		return []*agent.Record{{ID: "abc", Project: "demo", Cwd: "/from/coord"}}, nil
	}
	defer func() { agentList = prev }()

	got, err := ResolveWorkingDir("demo", "")
	if err != nil {
		t.Fatalf("ResolveWorkingDir: %v", err)
	}
	if got != "/from/coord" {
		t.Fatalf("expected live-coord Cwd fallback; got %q", got)
	}
}

func TestResolveWorkingDir_FailsCleanly(t *testing.T) {
	withFleetHome(t)
	prev := agentList
	agentList = func() ([]*agent.Record, error) { return nil, nil }
	defer func() { agentList = prev }()

	_, err := ResolveWorkingDir("demo", "")
	if !errors.Is(err, ErrCwdUnresolvable) {
		t.Fatalf("expected ErrCwdUnresolvable; got %v", err)
	}
}

func TestResolveWorkingDir_LiveCoordSkipsWrongProject(t *testing.T) {
	withFleetHome(t)
	prev := agentList
	agentList = func() ([]*agent.Record, error) {
		return []*agent.Record{
			{ID: "wrong", Project: "other", Cwd: "/from/other"},
			{ID: "right", Project: "demo", Cwd: "/from/demo"},
		}, nil
	}
	defer func() { agentList = prev }()

	got, err := ResolveWorkingDir("demo", "")
	if err != nil {
		t.Fatalf("ResolveWorkingDir: %v", err)
	}
	if got != "/from/demo" {
		t.Fatalf("expected /from/demo; got %q", got)
	}
}

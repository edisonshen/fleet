package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/edisonshen/fleet/internal/enginecfg"
)

// installPaths is where the selected engine's CLI reads Fleet's bundled
// skills and hook registrations from. `fleet init`, autoinit and `fleet
// skills` all install for the DOMINANT engine only — the helper engine
// (optional second reviewer) is driven through its non-interactive CLI
// and never loads the coordinator / fleet-guard skills.
//
//	engine       skillHome    hooksFile
//	claude-code  ~/.claude    ~/.claude/settings.json
//	codex        ~/.agents    ~/.codex/hooks.json
type installPaths struct {
	engine    string
	skillHome string // skills live at <skillHome>/skills/<name>/
	hooksFile string // JSON file whose top-level "hooks" object we merge into
}

func (p installPaths) skillDir(name string) string {
	return filepath.Join(p.skillHome, "skills", name)
}

// currentEngine is the engine root's PersistentPreRunE stamped into
// FLEET_ENGINE for this invocation (default claude-code). Unknown values
// fall back to the default rather than inventing a third skill home.
func currentEngine() string {
	if e := os.Getenv(FleetEngineEnv); enginecfg.Known(e) {
		return e
	}
	return enginecfg.DefaultEngine
}

// resolveInstallPaths returns the install locations for currentEngine().
// A non-empty override (tests) becomes the skill home and the hooks file
// sits inside it under the engine's usual basename (settings.json /
// hooks.json), so a redirected test never touches the real ~/.claude or
// ~/.codex.
func resolveInstallPaths(override string) (installPaths, error) {
	engine := currentEngine()
	if override != "" {
		return installPaths{
			engine:    engine,
			skillHome: override,
			hooksFile: filepath.Join(override, filepath.Base(enginecfg.HooksFileRel(engine))),
		}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return installPaths{}, fmt.Errorf("resolve home: %w", err)
	}
	return installPaths{
		engine:    engine,
		skillHome: enginecfg.SkillHome(home, engine),
		hooksFile: enginecfg.HooksFile(home, engine),
	}, nil
}

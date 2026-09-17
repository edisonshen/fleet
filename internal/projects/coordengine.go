package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/edisonshen/fleet/internal/state"
)

// CoordConfigPath returns <fleetHome>/projects/<project>/coord-config.json.
func CoordConfigPath(fleetHome, project string) string {
	return filepath.Join(fleetHome, "projects", project, "coord-config.json")
}

// GlobalCoordConfigPath returns <fleetHome>/coord-config.json — the
// operator-wide defaults file (`parallelism` from `fleet init`, `engine`
// from `fleet -codex` / `fleet -claude`).
func GlobalCoordConfigPath(fleetHome string) string {
	return filepath.Join(fleetHome, "coord-config.json")
}

// ReadCoordConfigEngine returns the `engine` stamped into the project's
// coord-config.json by the last coord spawn, or "" when the file, the key,
// or the value is missing/unreadable. Callers treat "" as "no persisted
// choice" and fall through to their own default. Never errors: a torn or
// hand-edited file must not block a spawn or a state write.
func ReadCoordConfigEngine(fleetHome, project string) string {
	if fleetHome == "" || project == "" {
		return ""
	}
	return readEngineKey(CoordConfigPath(fleetHome, project))
}

// ReadGlobalCoordConfigEngine returns the operator's persisted engine
// choice from <fleetHome>/coord-config.json::engine, or "" when unset.
func ReadGlobalCoordConfigEngine(fleetHome string) string {
	if fleetHome == "" {
		return ""
	}
	return readEngineKey(GlobalCoordConfigPath(fleetHome))
}

// ResolveEngineChoice returns the engine the NEXT coord for project
// should run under, or "" when nothing is persisted:
//
//  1. <fleetHome>/coord-config.json::engine — the operator's last
//     explicit `fleet -codex` / `fleet -claude`. Operator-wide: one
//     dominant engine, not one per project.
//  2. <fleetHome>/projects/<project>/coord-config.json::engine — what
//     the project's last coord actually ran (stamped on every coord
//     spawn). Only consulted when the operator never chose explicitly.
//
// Running coords are never touched by a change here; the choice is read
// at the next coord start, recovery, or handoff.
func ResolveEngineChoice(fleetHome, project string) string {
	if eng := ReadGlobalCoordConfigEngine(fleetHome); eng != "" {
		return eng
	}
	return ReadCoordConfigEngine(fleetHome, project)
}

// WriteGlobalCoordConfigEngine persists engine as the operator-wide
// dominant engine in <fleetHome>/coord-config.json::engine, preserving
// every other key (parallelism, ...). Unreadable/malformed existing
// content is treated as empty, matching loop.py's loader. Atomic via
// state.WriteAtomic.
func WriteGlobalCoordConfigEngine(fleetHome, engine string) error {
	if fleetHome == "" || engine == "" {
		return errors.New("WriteGlobalCoordConfigEngine: empty input")
	}
	if err := os.MkdirAll(fleetHome, 0o755); err != nil {
		return fmt.Errorf("mkdir fleet home: %w", err)
	}
	cfgPath := GlobalCoordConfigPath(fleetHome)
	cfg := map[string]any{}
	if raw, err := os.ReadFile(cfgPath); err == nil {
		var existing map[string]any
		if json.Unmarshal(raw, &existing) == nil && existing != nil {
			cfg = existing
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", cfgPath, err)
	}
	if cur, ok := cfg["engine"].(string); ok && cur == engine {
		return nil
	}
	cfg["engine"] = engine
	blob, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode coord-config: %w", err)
	}
	return state.WriteAtomic(cfgPath, append(blob, '\n'))
}

func readEngineKey(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		Engine string `json:"engine"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	return cfg.Engine
}

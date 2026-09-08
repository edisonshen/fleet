package projects

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// CoordConfigPath returns <fleetHome>/projects/<project>/coord-config.json.
func CoordConfigPath(fleetHome, project string) string {
	return filepath.Join(fleetHome, "projects", project, "coord-config.json")
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
	raw, err := os.ReadFile(CoordConfigPath(fleetHome, project))
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

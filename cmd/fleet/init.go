package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	fleet "github.com/edisonshen/fleet"
	"github.com/edisonshen/fleet/internal/state"
)

// hookEvents are the Claude Code hook event names fleet-guard listens on.
// Order is fixed for deterministic settings.json output and matches
// SKILL.md's Hook bindings table.
var hookEvents = []string{"Stop", "PreCompact", "SessionStart"}

func newInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install the fleet-guard skill into ~/.claude/skills/ and register hooks",
		Long: `Writes the embedded fleet-guard skill files to
~/.claude/skills/fleet-guard/ and merges Stop / PreCompact / SessionStart
hook registrations into ~/.claude/settings.json.

Idempotent: re-running on an installed skill prints "skip (exists)" for
each existing file unless --force is given. Existing settings.json
entries with the same command are not duplicated.`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runInit(c.OutOrStdout(), force, "")
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite existing skill files (settings.json is always merged, never replaced)")
	return cmd
}

// runInit copies embedded skill files into ~/.claude/skills/fleet-guard/
// and merges hook registrations into ~/.claude/settings.json.
//
// claudeHomeOverride lets tests redirect ~/.claude/. Production passes "".
func runInit(stdout io.Writer, force bool, claudeHomeOverride string) error {
	claudeHome := claudeHomeOverride
	if claudeHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home: %w", err)
		}
		claudeHome = filepath.Join(home, ".claude")
	}
	skillRoot := filepath.Join(claudeHome, "skills", "fleet-guard")

	if err := installSkillFiles(stdout, skillRoot, force); err != nil {
		return err
	}

	mainPath := filepath.Join(skillRoot, "main.py")
	if err := mergeHookRegistrations(stdout, claudeHome, mainPath); err != nil {
		return err
	}

	fmt.Fprintln(stdout, "fleet init: done")
	return nil
}

// installSkillFiles walks the embedded FS and writes each file to dst.
// Files are written via state.WriteAtomic so a crashed install never
// publishes a half-written main.py to the hook runner.
func installSkillFiles(stdout io.Writer, dst string, force bool) error {
	fsys := fleet.FleetGuardFS()
	return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		target := filepath.Join(dst, path)
		if !force {
			if _, err := os.Stat(target); err == nil {
				fmt.Fprintf(stdout, "skip (exists): %s\n", target)
				return nil
			}
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
		}
		if err := state.WriteAtomic(target, data); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		// .py and .sh files get exec bit set so the hook runner can invoke
		// them via shebang if Claude Code ever switches from explicit
		// `python3 path` to direct execution.
		if strings.HasSuffix(path, ".py") || strings.HasSuffix(path, ".sh") {
			if err := os.Chmod(target, 0o755); err != nil {
				return fmt.Errorf("chmod %s: %w", target, err)
			}
		}
		fmt.Fprintf(stdout, "wrote: %s\n", target)
		return nil
	})
}

// mergeHookRegistrations adds Stop / PreCompact / SessionStart entries to
// ~/.claude/settings.json that invoke the installed main.py. Existing
// entries (e.g., the spike's stop-hook.py) are preserved — fleet-guard
// is appended, never replaces. Idempotent: re-running with the same
// mainPath does not duplicate.
func mergeHookRegistrations(stdout io.Writer, claudeHome, mainPath string) error {
	settingsPath := filepath.Join(claudeHome, "settings.json")
	command := "/usr/bin/env python3 " + mainPath

	settings, err := loadSettings(settingsPath)
	if err != nil {
		return err
	}

	// Distinguish "missing" (fine, create) from "wrong type" (refuse). A
	// hand-edited settings.json that put the hooks key as an array or
	// string would otherwise be silently overwritten with an empty map,
	// destroying operator config. Bail with a clear pointer instead so
	// the operator can repair manually.
	var hooks map[string]any
	if raw, present := settings["hooks"]; present && raw != nil {
		typed, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf(
				"settings.json: 'hooks' is %T, expected JSON object — refusing to overwrite; repair %s manually",
				raw, filepath.Join(claudeHome, "settings.json"))
		}
		hooks = typed
	} else {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}

	added := 0
	for _, event := range hookEvents {
		if ensureHookEntry(hooks, event, command) {
			added++
			fmt.Fprintf(stdout, "registered: %s → %s\n", event, mainPath)
		} else {
			fmt.Fprintf(stdout, "skip (registered): %s\n", event)
		}
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}
	if err := state.WriteAtomic(settingsPath, out); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}
	if added == 0 {
		fmt.Fprintln(stdout, "settings.json already up to date")
	}
	return nil
}

func loadSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read settings.json: %w", err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parse settings.json: %w", err)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	return settings, nil
}

// ensureHookEntry adds a hook entry for `event` invoking `command` if
// none of the existing entries already invoke that exact command.
// Returns true if the entry was added (false = already present).
//
// Schema follows the live shape in ~/.claude/settings.json:
//
//	hooks.<Event>: [
//	  { "hooks": [{ "type": "command", "command": "..." }] }
//	]
func ensureHookEntry(hooks map[string]any, event, command string) bool {
	arr, _ := hooks[event].([]any)
	for _, raw := range arr {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		sub, _ := entry["hooks"].([]any)
		for _, h := range sub {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, _ := hm["command"].(string); cmd == command {
				return false
			}
		}
	}
	newEntry := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
			},
		},
	}
	arr = append(arr, newEntry)
	hooks[event] = arr
	return true
}

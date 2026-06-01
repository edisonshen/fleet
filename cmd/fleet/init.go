package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	fleet "github.com/edisonshen/fleet"
	"github.com/edisonshen/fleet/internal/install"
	"github.com/edisonshen/fleet/internal/state"
)

// hookEvents are the Claude Code hook event names fleet-guard listens on.
// Order is fixed for deterministic settings.json output and matches
// SKILL.md's Hook bindings table. UserPromptSubmit was added alongside
// the needs_input flag so the TUI can surface "waiting on operator" —
// without that hook the flag would set true on Stop and never clear.
var hookEvents = []string{"Stop", "PreCompact", "SessionStart", "UserPromptSubmit"}

func newInitCmd() *cobra.Command {
	var force bool
	var upgrade bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install bundled skills into ~/.claude/skills/ and register hooks",
		Long: `Writes the embedded skills (fleet-guard, coordinator) to
~/.claude/skills/<name>/ and merges Stop / PreCompact / SessionStart /
UserPromptSubmit hook registrations into ~/.claude/settings.json. Also
seeds a canonical ~/.fleet/standards.md when one is missing — that file
is the operator-edited "the bar" the v0.2 coordinator inlines into
worker prompts.

Idempotent: re-running on an installed skill prints "skip (up to date)"
for each existing file. --upgrade overwrites bundled skill files (so a
newer fleet binary auto-refreshes the install) but never overwrites
~/.fleet/standards.md (operator edits to the bar are preserved). --force
is the legacy alias for --upgrade.`,
		RunE: func(c *cobra.Command, _ []string) error {
			return runInit(c.OutOrStdout(), force || upgrade, "")
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"alias for --upgrade (kept for compat with v0.1 muscle memory)")
	cmd.Flags().BoolVar(&upgrade, "upgrade", false,
		"refresh bundled skill files from the binary (settings.json is always merged; ~/.fleet/standards.md is preserved)")
	return cmd
}

// runInit copies every embedded skill into ~/.claude/skills/<name>/,
// merges hook registrations into ~/.claude/settings.json, and seeds
// ~/.fleet/standards.md from the bundled template when missing.
//
// claudeHomeOverride lets tests redirect ~/.claude/. Production passes "".
// fleet-guard remains the source of the hook command — coordinator runs
// as a slash skill the coord agent invokes itself, not via Claude Code
// hooks (matches SKILL.md "Hook bindings" section).
func runInit(stdout io.Writer, force bool, claudeHomeOverride string) error {
	claudeHome := claudeHomeOverride
	if claudeHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home: %w", err)
		}
		claudeHome = filepath.Join(home, ".claude")
	}

	// Walk every bundled skill in a stable order so install output is
	// deterministic across runs (fleet-guard first, coordinator second).
	// fleet.SkillFS() is the single source of truth — adding a new skill
	// here means adding the //go:embed directive in embed.go and the
	// map entry in SkillFS().
	skills := fleet.SkillFS()
	for _, name := range skillInstallOrder(skills) {
		// A symlinked skill dir means the operator ran `fleet skills link`
		// to follow a repo checkout live. Writing the embedded copy here
		// would follow the symlink and OVERWRITE the operator's repo files
		// with stale embedded bytes — corrupting their checkout. Leave the
		// link intact; `fleet skills sync --force` is the explicit opt-out
		// for converting back to a copy.
		if install.IsSymlink(claudeHome, name) {
			_, _ = fmt.Fprintf(stdout, "skip (symlinked, live): %s\n",
				filepath.Join(claudeHome, "skills", name))
			continue
		}
		skillRoot := filepath.Join(claudeHome, "skills", name)
		if err := installSkillFilesFS(stdout, skills[name], skillRoot, force); err != nil {
			return fmt.Errorf("install %s: %w", name, err)
		}
	}

	// fleet-guard owns the hook registrations. Coordinator is invoked
	// via the slash-skill path, not via hooks — see SKILL.md "Hook
	// bindings". Adding coordinator here would double-fire on every
	// Stop / PreCompact and run loop.tick() inside fleet-guard's hook
	// payload, which the skill explicitly does not want.
	mainPath := filepath.Join(claudeHome, "skills", "fleet-guard", "main.py")
	if err := mergeHookRegistrations(stdout, claudeHome, mainPath); err != nil {
		return err
	}

	if err := seedStandardsTemplate(stdout); err != nil {
		return fmt.Errorf("seed standards: %w", err)
	}

	_, _ = fmt.Fprintln(stdout, "fleet init: done")
	return nil
}

// skillInstallOrder returns skill names in a deterministic order so
// install output is stable across runs (helps tests + UX). Sort by
// skill name; fleet-guard happens to sort before coordinator, so the
// historical "fleet-guard first" order is preserved without a
// hardcoded list.
func skillInstallOrder(skills map[string]fs.FS) []string {
	names := make([]string, 0, len(skills))
	for name := range skills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// installSkillFilesFS walks the supplied embedded FS and writes each
// file to dst. Files are written via state.WriteAtomic so a crashed
// install never publishes a half-written main.py to the hook runner.
//
// Self-healing on upgrade: when force is false and the target exists,
// the embedded content is byte-compared against the installed file.
// Matches are skipped (idempotent re-run); mismatches are overwritten
// so upgrading the fleet binary auto-refreshes the bundled skill code.
// Without this, hookEvents additions (e.g., UserPromptSubmit) would
// fire against a stale main.py that has no handler for the new hook.
// force=true short-circuits the compare and always rewrites.
//
// Generalized in v0.2 to accept any fs.FS so the same loop installs
// fleet-guard, coordinator, and any future bundled skill (the embed
// directives live in embed.go; this function doesn't care which).
func installSkillFilesFS(stdout io.Writer, fsys fs.FS, dst string, force bool) error {
	return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		target := filepath.Join(dst, path)
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		if !force {
			existing, statErr := os.ReadFile(target)
			switch {
			case statErr == nil && bytes.Equal(existing, data):
				_, _ = fmt.Fprintf(stdout, "skip (up to date): %s\n", target)
				return nil
			case statErr == nil:
				_, _ = fmt.Fprintf(stdout, "refresh (stale): %s\n", target)
			case !os.IsNotExist(statErr):
				return fmt.Errorf("stat %s: %w", target, statErr)
			}
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
		_, _ = fmt.Fprintf(stdout, "wrote: %s\n", target)
		return nil
	})
}

// seedStandardsTemplate writes the canonical ~/.fleet/standards.md
// template if no standards.md exists. NEVER overwrites — operator edits
// to the bar are preserved across `fleet init --upgrade` (matches the
// PR 12 contract: skill files refresh, standards.md does not).
//
// Bootstrap is required first so ~/.fleet/ exists; the template is
// rendered via state.WriteAtomic so a crash mid-write can't leave a
// torn frontmatter that the parser would later reject.
func seedStandardsTemplate(stdout io.Writer) error {
	root, err := state.Bootstrap()
	if err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}
	target := filepath.Join(root, "standards.md")
	if _, err := os.Stat(target); err == nil {
		_, _ = fmt.Fprintf(stdout, "skip (existing): %s\n", target)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	if err := state.WriteAtomic(target, fleet.StandardsTemplate()); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	_, _ = fmt.Fprintf(stdout, "seeded: %s\n", target)
	return nil
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
			_, _ = fmt.Fprintf(stdout, "registered: %s → %s\n", event, mainPath)
		} else {
			_, _ = fmt.Fprintf(stdout, "skip (registered): %s\n", event)
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
		_, _ = fmt.Fprintln(stdout, "settings.json already up to date")
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

package enginecfg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edisonshen/fleet/internal/state"
)

// CodexConfigDir is the directory Codex reads its user config from:
// $CODEX_HOME when set, else <home>/.codex.
func CodexConfigDir(home string) string {
	if ch := os.Getenv("CODEX_HOME"); ch != "" {
		return filepath.Clean(ch)
	}
	return filepath.Join(home, ".codex")
}

// CodexConfigPath is the Codex CLI's user config file inside cfgDir
// (see CodexConfigDir).
func CodexConfigPath(cfgDir string) string {
	return filepath.Join(cfgDir, "config.toml")
}

// codexProjectHeader renders the TOML table header Codex uses to record a
// trusted project directory:
//
//	[projects."/abs/path"]
//	trust_level = "trusted"
func codexProjectHeader(dir string) string {
	return `[projects."` + tomlBasicEscape(dir) + `"]`
}

func tomlBasicEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// CodexProjectTrusted reports whether cfgDir/config.toml already carries
// a [projects."<dir>"] table for dir (any trust_level).
func CodexProjectTrusted(cfgDir, dir string) bool {
	raw, err := os.ReadFile(CodexConfigPath(cfgDir))
	if err != nil {
		return false
	}
	return codexConfigHasProject(string(raw), dir)
}

func codexConfigHasProject(body, dir string) bool {
	want := codexProjectHeader(dir)
	wantSingle := `[projects.'` + dir + `']`
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == want || line == wantSingle {
			return true
		}
	}
	return false
}

// EnsureCodexProjectTrust appends a trusted [projects."<dir>"] table to
// cfgDir/config.toml when dir has no entry yet. The interactive Codex
// TUI otherwise stops at a "Do you trust the contents of this directory?"
// prompt on startup — a `-c projects.…` CLI override does not satisfy it
// — which stalls an unattended coord/worker in its tmux pane.
//
// Append-only: existing content is preserved byte-for-byte and a new
// top-level table at end of file is always valid TOML. Returns true when
// the file was modified.
func EnsureCodexProjectTrust(cfgDir, dir string) (bool, error) {
	if cfgDir == "" || dir == "" {
		return false, errors.New("EnsureCodexProjectTrust: empty input")
	}
	dir = filepath.Clean(dir)
	cfgPath := CodexConfigPath(cfgDir)
	raw, err := os.ReadFile(cfgPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", cfgPath, err)
	}
	mode := fs.FileMode(0o600)
	if st, serr := os.Stat(cfgPath); serr == nil {
		mode = st.Mode().Perm()
	}
	body := string(raw)
	if codexConfigHasProject(body, dir) {
		return false, nil
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", cfgDir, err)
	}
	var sb strings.Builder
	sb.WriteString(body)
	if body != "" && !strings.HasSuffix(body, "\n") {
		sb.WriteString("\n")
	}
	if body != "" {
		sb.WriteString("\n")
	}
	sb.WriteString(codexProjectHeader(dir) + "\n")
	sb.WriteString("trust_level = \"trusted\"\n")
	if err := state.WriteAtomic(cfgPath, []byte(sb.String())); err != nil {
		return false, err
	}
	if err := os.Chmod(cfgPath, mode); err != nil {
		return true, fmt.Errorf("chmod %s: %w", cfgPath, err)
	}
	return true, nil
}

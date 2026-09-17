package enginecfg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

const codexTrustedLine = `trust_level = "trusted"`

// CodexProjectTrusted reports whether cfgDir/config.toml carries a
// [projects."<dir>"] table for dir with trust_level = "trusted".
func CodexProjectTrusted(cfgDir, dir string) bool {
	raw, err := os.ReadFile(CodexConfigPath(cfgDir))
	if err != nil {
		return false
	}
	_, _, val := codexProjectTable(strings.Split(string(raw), "\n"), filepath.Clean(dir))
	return val == "trusted"
}

// codexProjectTable locates dir's [projects."<dir>"] table in lines.
// Returns the header line index (-1 when absent), the index of its
// trust_level key (-1 when the table has none) and that key's value. The
// table body ends at the next `[` table header.
func codexProjectTable(lines []string, dir string) (header, level int, value string) {
	want := codexProjectHeader(dir)
	wantSingle := `[projects.'` + dir + `']`
	header, level = -1, -1
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if header < 0 {
			if line == want || line == wantSingle {
				header = i
			}
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		if v := codexTrustLevelValue(line); v != "" {
			return header, i, v
		}
	}
	return header, level, ""
}

// codexTrustLevelValue returns the unquoted value of a `trust_level = "x"`
// line, or "" when line is not a trust_level assignment.
func codexTrustLevelValue(line string) string {
	key, val, ok := strings.Cut(line, "=")
	if !ok || strings.TrimSpace(key) != "trust_level" {
		return ""
	}
	val = strings.TrimSpace(val)
	if i := strings.Index(val, "#"); i >= 0 {
		val = strings.TrimSpace(val[:i])
	}
	return strings.Trim(val, `"'`)
}

// EnsureCodexProjectTrust makes cfgDir/config.toml mark dir as a trusted
// project. The interactive Codex TUI otherwise stops at a "Do you trust
// the contents of this directory?" prompt on startup — a `-c projects.…`
// CLI override does not satisfy it — which stalls an unattended
// coord/worker in its tmux pane.
//
// No [projects."<dir>"] table: one is appended (a new top-level table at
// end of file is always valid TOML, existing content is preserved
// byte-for-byte). Table present with another trust_level (e.g.
// "untrusted" from a past prompt answer): that single line is rewritten
// to "trusted". Table present without trust_level: the key is inserted
// under the header. Returns true when the file was modified.
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
	lines := strings.Split(body, "\n")
	header, level, value := codexProjectTable(lines, dir)
	var out string
	switch {
	case value == "trusted":
		return false, nil
	case level >= 0:
		lines[level] = codexTrustedLine
		out = strings.Join(lines, "\n")
	case header >= 0:
		lines = slices.Insert(lines, header+1, codexTrustedLine)
		out = strings.Join(lines, "\n")
	default:
		var sb strings.Builder
		sb.WriteString(body)
		if body != "" && !strings.HasSuffix(body, "\n") {
			sb.WriteString("\n")
		}
		if body != "" {
			sb.WriteString("\n")
		}
		sb.WriteString(codexProjectHeader(dir) + "\n")
		sb.WriteString(codexTrustedLine + "\n")
		out = sb.String()
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", cfgDir, err)
	}
	if err := state.WriteAtomic(cfgPath, []byte(out)); err != nil {
		return false, err
	}
	if err := os.Chmod(cfgPath, mode); err != nil {
		return true, fmt.Errorf("chmod %s: %w", cfgPath, err)
	}
	return true, nil
}

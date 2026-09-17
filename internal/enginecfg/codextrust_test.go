package enginecfg_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/enginecfg"
)

func TestCodexConfigDir_HonoursCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/tmp/custom-codex/")
	if got := enginecfg.CodexConfigDir("/home/x"); got != "/tmp/custom-codex" {
		t.Fatalf("CodexConfigDir with CODEX_HOME = %q", got)
	}
	t.Setenv("CODEX_HOME", "")
	if got := enginecfg.CodexConfigDir("/home/x"); got != filepath.Join("/home/x", ".codex") {
		t.Fatalf("CodexConfigDir default = %q", got)
	}
}

func TestEnsureCodexProjectTrust_CreatesConfig(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), ".codex")
	dir := "/work/proj"
	added, err := enginecfg.EnsureCodexProjectTrust(cfgDir, dir)
	if err != nil || !added {
		t.Fatalf("first call: added=%v err=%v", added, err)
	}
	raw, err := os.ReadFile(enginecfg.CodexConfigPath(cfgDir))
	if err != nil {
		t.Fatal(err)
	}
	want := "[projects.\"/work/proj\"]\ntrust_level = \"trusted\"\n"
	if string(raw) != want {
		t.Fatalf("config body:\n%s\nwant:\n%s", raw, want)
	}
	if !enginecfg.CodexProjectTrusted(cfgDir, dir) {
		t.Fatal("CodexProjectTrusted false after write")
	}
	// Idempotent: second call is a no-op.
	added, err = enginecfg.EnsureCodexProjectTrust(cfgDir, dir)
	if err != nil || added {
		t.Fatalf("second call: added=%v err=%v", added, err)
	}
	raw2, _ := os.ReadFile(enginecfg.CodexConfigPath(cfgDir))
	if string(raw2) != want {
		t.Fatalf("second call rewrote config:\n%s", raw2)
	}
}

func TestEnsureCodexProjectTrust_AppendsAndPreserves(t *testing.T) {
	cfgDir := t.TempDir()
	cfgPath := enginecfg.CodexConfigPath(cfgDir)
	existing := "model = \"gpt-5\"\n\n[projects.\"/other\"]\ntrust_level = \"trusted\"" // no trailing newline
	if err := os.WriteFile(cfgPath, []byte(existing), 0o640); err != nil {
		t.Fatal(err)
	}
	if enginecfg.CodexProjectTrusted(cfgDir, "/work/proj") {
		t.Fatal("untrusted dir reported trusted")
	}
	if !enginecfg.CodexProjectTrusted(cfgDir, "/other") {
		t.Fatal("existing entry not detected")
	}
	added, err := enginecfg.EnsureCodexProjectTrust(cfgDir, "/work/proj/")
	if err != nil || !added {
		t.Fatalf("added=%v err=%v", added, err)
	}
	raw, _ := os.ReadFile(cfgPath)
	body := string(raw)
	if !strings.HasPrefix(body, existing+"\n\n") {
		t.Fatalf("existing content not preserved verbatim:\n%s", body)
	}
	if !strings.HasSuffix(body, "[projects.\"/work/proj\"]\ntrust_level = \"trusted\"\n") {
		t.Fatalf("new table not appended:\n%s", body)
	}
	if strings.Count(body, "[projects.\"/other\"]") != 1 {
		t.Fatalf("existing table duplicated:\n%s", body)
	}
	st, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("file mode changed to %o", st.Mode().Perm())
	}
}

func TestEnsureCodexProjectTrust_EscapesQuotesAndBackslashes(t *testing.T) {
	cfgDir := t.TempDir()
	dir := `/work/we"ird\path`
	if _, err := enginecfg.EnsureCodexProjectTrust(cfgDir, dir); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(enginecfg.CodexConfigPath(cfgDir))
	if !strings.Contains(string(raw), `[projects."/work/we\"ird\\path"]`) {
		t.Fatalf("header not escaped:\n%s", raw)
	}
	if !enginecfg.CodexProjectTrusted(cfgDir, dir) {
		t.Fatal("escaped entry not detected on re-read")
	}
}

func TestEnsureCodexProjectTrust_EmptyInput(t *testing.T) {
	if _, err := enginecfg.EnsureCodexProjectTrust("", "/x"); err == nil {
		t.Fatal("expected error for empty cfgDir")
	}
	if _, err := enginecfg.EnsureCodexProjectTrust(t.TempDir(), ""); err == nil {
		t.Fatal("expected error for empty dir")
	}
}

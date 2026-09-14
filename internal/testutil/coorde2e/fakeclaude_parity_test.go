package coorde2e_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/testutil/coorde2e"
)

// Scenario S6 — the Go shim (coorde2e.FakeClaude) and the shell shim
// (scripts/fake-claude.sh) are two writers of ONE contract: `claude`
// stand-in that Fleet's dispatch wrapper execs. Go e2e tests use the
// former; `scripts/scenario.sh` puts the latter first on PATH. If the
// two drift, a scenario that passes by hand fails in Go (or vice versa)
// for a reason that has nothing to do with Fleet. So, like
// tmuxfake/parity_test.go, this drives both against the same input and
// diffs the observable result: exit code and stdout, pid-normalised.

var pidRE = regexp.MustCompile(`pid \d+`)

type shimResult struct {
	exit   int
	stdout string
}

func runShim(t *testing.T, path string, env []string, stdin string) shimResult {
	t.Helper()
	cmd := exec.Command(path, "--dangerously-skip-permissions")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("run %s: %v", path, err)
	}
	return shimResult{exit: cmd.ProcessState.ExitCode(), stdout: pidRE.ReplaceAllString(out.String(), "pid N")}
}

func shellShimPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "fake-claude.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("scripts/fake-claude.sh: %v", err)
	}
	return p
}

func TestFakeClaude_ShellAndGoShimsAgree(t *testing.T) {
	shell := shellShimPath(t)
	const prompt = "hello\n"

	cases := []struct {
		mode      string
		wantExit  int
		wantFirst string
		wantAck   bool
	}{
		{coorde2e.ModeOK, 1, "fake claude: ready pid N", true},
		{coorde2e.ModeExit, 1, "fake claude: startup failure pid N", false},
		{coorde2e.ModeCrashOnce, 1, "fake claude: crashing once pid N", false},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			modeFile := coorde2e.FakeClaude(t, dir)
			coorde2e.SetMode(t, modeFile, tc.mode)
			goRes := runShim(t, filepath.Join(dir, "claude"), nil, prompt)

			shRes := runShim(t, shell, []string{
				"FLEET_FAKE_CLAUDE_MODE=" + tc.mode,
				"FLEET_FAKE_CLAUDE_STATE=" + filepath.Join(dir, "sh.crashed"),
			}, prompt)

			if goRes != shRes {
				t.Fatalf("mode %s: go and shell shims disagree\n go:    exit=%d %q\n shell: exit=%d %q",
					tc.mode, goRes.exit, goRes.stdout, shRes.exit, shRes.stdout)
			}
			if goRes.exit != tc.wantExit {
				t.Fatalf("mode %s: exit=%d want %d", tc.mode, goRes.exit, tc.wantExit)
			}
			first, _, _ := strings.Cut(goRes.stdout, "\n")
			if first != tc.wantFirst {
				t.Fatalf("mode %s: first line %q want %q", tc.mode, first, tc.wantFirst)
			}
			if got := strings.Contains(goRes.stdout, coorde2e.PromptAck+" (5 chars)"); got != tc.wantAck {
				t.Fatalf("mode %s: prompt ack present=%v want %v\n%s", tc.mode, got, tc.wantAck, goRes.stdout)
			}
		})
	}
}

// crash-once is stateful: the second start of each shim must be a plain
// `ok` run, and re-arming (SetMode / removing the marker) crashes again.
func TestFakeClaude_CrashOnce_SecondStartIsOK(t *testing.T) {
	shell := shellShimPath(t)
	dir := t.TempDir()
	modeFile := coorde2e.FakeClaude(t, dir)
	coorde2e.SetMode(t, modeFile, coorde2e.ModeCrashOnce)
	shEnv := []string{
		"FLEET_FAKE_CLAUDE_MODE=" + coorde2e.ModeCrashOnce,
		"FLEET_FAKE_CLAUDE_STATE=" + filepath.Join(dir, "sh.crashed"),
	}

	first := runShim(t, filepath.Join(dir, "claude"), nil, "")
	second := runShim(t, filepath.Join(dir, "claude"), nil, "")
	shFirst := runShim(t, shell, shEnv, "")
	shSecond := runShim(t, shell, shEnv, "")

	if first != shFirst || second != shSecond {
		t.Fatalf("crash-once sequence differs\n go:    %+v then %+v\n shell: %+v then %+v", first, second, shFirst, shSecond)
	}
	if !strings.HasPrefix(first.stdout, "fake claude: crashing once") {
		t.Fatalf("first start: %+v", first)
	}
	if !strings.HasPrefix(second.stdout, "fake claude: ready") {
		t.Fatalf("second start should be ok: %+v", second)
	}

	coorde2e.SetMode(t, modeFile, coorde2e.ModeCrashOnce)
	if err := os.Remove(filepath.Join(dir, "sh.crashed")); err != nil {
		t.Fatal(err)
	}
	rearmed := runShim(t, filepath.Join(dir, "claude"), nil, "")
	shRearmed := runShim(t, shell, shEnv, "")
	if rearmed != shRearmed || !strings.HasPrefix(rearmed.stdout, "fake claude: crashing once") {
		t.Fatalf("re-armed crash-once: go %+v shell %+v", rearmed, shRearmed)
	}
}

// hang never prints the `> ` prompt and never exits on its own; both shims
// must be killable and leave exactly the banner behind.
func TestFakeClaude_Hang_BannerOnlyUntilKilled(t *testing.T) {
	shell := shellShimPath(t)
	dir := t.TempDir()
	modeFile := coorde2e.FakeClaude(t, dir)
	coorde2e.SetMode(t, modeFile, coorde2e.ModeHang)

	for name, path := range map[string]string{
		"go":    filepath.Join(dir, "claude"),
		"shell": shell,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, path)
		cmd.WaitDelay = time.Second
		cmd.Env = append(os.Environ(), "FLEET_FAKE_CLAUDE_MODE="+coorde2e.ModeHang)
		cmd.Stdin = strings.NewReader("hello\n")
		var out bytes.Buffer
		cmd.Stdout = &out
		err := cmd.Run()
		cancel()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("%s: exited on its own (err=%v) — hang must block until killed", name, err)
		}
		got := pidRE.ReplaceAllString(out.String(), "pid N")
		if got != "fake claude: hanging pid N\n" {
			t.Fatalf("%s: hang stdout = %q, want banner only", name, got)
		}
	}
}

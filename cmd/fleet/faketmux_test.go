package main

// faketmux_test.go — ci-perf-pr1 (P0): a REUSABLE PATH-level fake `tmux`
// recorder. It is deliberately self-contained and general so PR-2 can reuse it
// as a "did a routed test exec real tmux?" bypass detector (the fake's call
// log IS the bypass evidence: any real-tmux exec would not pass through here).
//
// HOW IT WORKS:
//
//	t.Setenv("PATH", <tmpdir>:<old PATH>)  with a `tmux` shell script in tmpdir
//	        │
//	        ▼  code under test runs exec.Command("tmux", "-S", <sock>, "ls", ...)
//	   the script appends its full argv (one line) to a log file, then
//	   for an `ls` it prints a fleet-<id> session name + exit 0 so callers
//	   that gate on a live fleet session (firstFleetSession) see a match.
//	        │
//	        ▼
//	   rec.socketProbes() parses the log back into the `-S <sock>` paths.
//
// Why a PATH shim and not a function seam: firstFleetSession / socketLiveOnDisk
// exec `tmux` directly via os/exec (no internal/tmux indirection), so the only
// honest interception point at this layer is PATH. This also makes the helper
// engine-agnostic and reusable across packages in PR-2.

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTmuxRecorder captures every `tmux` invocation routed through the PATH
// shim it installs.
type fakeTmuxRecorder struct {
	t       *testing.T
	logPath string
}

// newFakeTmuxRecorder installs a fake `tmux` at the FRONT of PATH for the
// duration of the test (restored by t.Setenv cleanup) and returns a recorder
// for its invocation log. The fake's `ls` response carries a fleet-<id>
// session name so firstFleetSession treats the probed socket as live.
func newFakeTmuxRecorder(t *testing.T) *fakeTmuxRecorder {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "tmux-invocations.log")

	// The script records argv ONE ARG PER LINE, prefixed by a `=INVOCATION=`
	// record separator (claude adversarial F3): joining args with "$*" loses
	// the boundary when a socket path contains whitespace, which would let a
	// bogus probe path slip past the "all probes under dir" assertion. One arg
	// per line is faithful (paths can't contain a newline). Then, for an `ls`
	// subcommand, it prints a synthetic fleet session so the caller's
	// live-session gate matches; `fleet-deadbeef` matches fleetAgentIDPattern
	// (lowercase hex id) used by firstFleetSession.
	script := "#!/bin/sh\n" +
		"printf '=INVOCATION=\\n' >> " + shellQuote(logPath) + "\n" +
		"for a in \"$@\"; do\n" +
		"  printf '%s\\n' \"$a\" >> " + shellQuote(logPath) + "\n" +
		"done\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in\n" +
		"    ls|list-sessions) printf 'fleet-deadbeef\\n'; exit 0 ;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 0\n"
	tmuxPath := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(tmuxPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return &fakeTmuxRecorder{t: t, logPath: logPath}
}

// invocations returns the recorded `tmux` calls, each as its raw argv slice
// (one element per arg, whitespace within an arg preserved). The on-disk log
// is `=INVOCATION=` separators followed by one line per arg.
func (r *fakeTmuxRecorder) invocations() [][]string {
	b, err := os.ReadFile(r.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // never invoked
		}
		r.t.Fatalf("read fake-tmux log: %v", err)
	}
	var out [][]string
	var cur []string
	started := false
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if ln == "=INVOCATION=" {
			if started {
				out = append(out, cur)
			}
			cur = nil
			started = true
			continue
		}
		if started {
			cur = append(cur, ln)
		}
	}
	if started {
		out = append(out, cur)
	}
	return out
}

// lines returns each recorded invocation as a space-joined argv string, for
// human-readable failure messages.
func (r *fakeTmuxRecorder) lines() []string {
	var out []string
	for _, args := range r.invocations() {
		out = append(out, strings.Join(args, " "))
	}
	return out
}

// socketProbes returns the socket path argument from every invocation that
// passed `-S <sock>` (the per-socket probe shape used by firstFleetSession /
// socketLiveOnDisk). The arg immediately after a `-S` arg is the socket path;
// because args are recorded one-per-line, a path with spaces stays intact.
func (r *fakeTmuxRecorder) socketProbes() []string {
	var out []string
	for _, args := range r.invocations() {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-S" {
				out = append(out, args[i+1])
			}
		}
	}
	return out
}

// listenUnix opens a real Unix-domain socket at path so the file carries
// os.ModeSocket (required by firstFleetSession's symlink guard). Shared by the
// scan-dir tests that need a genuine socket fixture.
func listenUnix(t *testing.T, path string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen unix %s: %v", path, err)
	}
	return ln
}

// shellQuote single-quotes s for safe embedding in the POSIX `sh` shim. Test
// temp paths are well-formed, but quoting keeps the shim robust to spaces.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

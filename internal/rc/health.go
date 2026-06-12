package rc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// HealthStatus enumerates the three states `fleet rc status --healthy`
// can report. Stable wire contract — operator-facing diagnostic.
const (
	HealthHealthy            = "healthy"
	HealthDeadNoServiceEntry = "dead-no-service-entry"
	HealthDeadPID            = "dead-pid"
	HealthUnknown            = "unknown"
)

// HealthResult is the structured probe outcome.
type HealthResult struct {
	Status     string `json:"status"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

// Health probes the Claude daemon registry for project's listener.
// Per design A1: shells out to `claude daemon remote-control list`
// and matches against the recorded session_prefix.
//
// Resolution:
//   - State PID dead → HealthDeadPID.
//   - PID alive + service-entry present (working_dir match) → HealthHealthy.
//   - PID alive + service-entry absent → HealthDeadNoServiceEntry.
//   - Claude CLI absent or daemon command unsupported → HealthUnknown
//     (codex round 3 verify-via-smoke-test caveat).
func Health(project string) HealthResult {
	cur, err := ReadState(project)
	if err != nil {
		return HealthResult{
			Status: HealthUnknown,
			Diagnostic: "no rc-state.json on disk — NORMAL under the native model " +
				"(RC rides on the coord's own claude process; no standalone listener " +
				"is recorded). This probe only diagnoses LEGACY pre-native listeners.",
		}
	}
	if cur.PID <= 0 || !IsAlive(cur.PID) {
		return HealthResult{Status: HealthDeadPID, Diagnostic: fmt.Sprintf("recorded PID %d not alive", cur.PID)}
	}
	out, listErr := claudeDaemonListFn()
	if listErr != nil {
		// CLI absent / unsupported subcommand / network timeout — fall
		// back to PID-alive evidence only (codex round 3 fallback).
		if errors.Is(listErr, exec.ErrNotFound) {
			return HealthResult{Status: HealthUnknown, Diagnostic: "claude CLI not on PATH"}
		}
		return HealthResult{Status: HealthUnknown, Diagnostic: "claude daemon remote-control list failed: " + listErr.Error()}
	}
	// Match the recorded working_dir against the daemon's list output.
	// Best-effort substring match — the daemon's exact format isn't
	// pinned; the smoke-test step in the worker dispatch should
	// confirm the surface before relying on a stricter parse.
	if strings.Contains(out, cur.WorkingDir) {
		return HealthResult{Status: HealthHealthy}
	}
	return HealthResult{Status: HealthDeadNoServiceEntry, Diagnostic: fmt.Sprintf("PID %d alive but working_dir %q not in daemon registry", cur.PID, cur.WorkingDir)}
}

// claudeDaemonListFn is the test seam. Production execs
// `claude daemon remote-control list` with a 5s timeout.
var claudeDaemonListFn = func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "daemon", "remote-control", "list")
	data, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// SetClaudeDaemonListForTest swaps the test seam; returns a restore
// func tests defer.
func SetClaudeDaemonListForTest(fn func() (string, error)) func() {
	prev := claudeDaemonListFn
	claudeDaemonListFn = fn
	return func() { claudeDaemonListFn = prev }
}

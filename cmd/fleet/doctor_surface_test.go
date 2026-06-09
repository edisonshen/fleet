package main

// doctor_surface_test.go — T22 (DESIGN-handoff-drain-storm-leak PR6): the
// user-facing `fleet doctor` output is FLEET-LEVEL plain English. The jargon
// words (tmux / wedged / flock / epoch / STONITH / lease / fence) appear ONLY
// under --verbose. This file is ALL-PLATFORM (no build tag) because the
// renderer lives in doctor.go (all platforms) and the test injects a canned
// doctorReport — it never touches the unix-only lease machinery.

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// jargonWords are the engineer-only terms that must NOT leak into the
// default (non-verbose) user surface. Exactly the set the design names.
var jargonWords = []string{"tmux", "wedged", "flock", "epoch", "STONITH", "lease", "fence"}

// cannedDoctorReport returns a report exercising every surface (a stuck
// status, findings with verbose detail, and a --fix outcome with verbose
// recovery lines) so the no-jargon assertion covers the whole renderer.
func cannedDoctorReport() doctorReport {
	return doctorReport{
		projects: []doctorProjectReport{{
			project: "proj",
			status:  doctorStatusUnresponsive,
			findings: []doctorFinding{
				{
					plain:   "a handoff to a fresh coordinator is pending and hasn't finished",
					verbose: "pending spawn-fresh queue file(s) with no live successor",
				},
				{
					plain:   "the coordinator's terminal session is gone",
					verbose: "tmux session fleet-abc is not alive", // jargon in VERBOSE only
				},
				{
					plain:   "3 leftover handoff notes are piling up",
					verbose: "3 handoff docs (storm)",
				},
			},
			fixPlanned: true,
			fixActions: []string{
				"Stopped the stuck coordinator.",
				"Started a fresh coordinator.",
			},
			verboseDetail: []string{
				"lease: state=active epoch=7 owner-pid=999 owner-alive=true", // jargon in VERBOSE only
				"refused: STONITH fence skipped",                             // jargon in VERBOSE only
			},
		}},
	}
}

func TestDoctor_T22_PlainSurface_NoJargon(t *testing.T) {
	report := cannedDoctorReport()

	var out bytes.Buffer
	renderDoctorReport(doctorOpts{project: "proj", fix: true, verbose: false}, report, &out)
	plain := out.String()

	for _, w := range jargonWords {
		if strings.Contains(strings.ToLower(plain), strings.ToLower(w)) {
			t.Errorf("non-verbose output leaked jargon %q:\n%s", w, plain)
		}
	}
	// It still says something useful in plain English.
	if !strings.Contains(plain, "not responding") {
		t.Errorf("plain output missing the symptom line:\n%s", plain)
	}
	if !strings.Contains(plain, "handoff to a fresh coordinator") {
		t.Errorf("plain output missing the plain handoff finding:\n%s", plain)
	}
}

func TestDoctor_T22_VerboseSurface_AllowsJargon(t *testing.T) {
	report := cannedDoctorReport()

	var out bytes.Buffer
	renderDoctorReport(doctorOpts{project: "proj", fix: true, verbose: true}, report, &out)
	verbose := out.String()

	// Under --verbose the engineer detail (which DOES name lease/epoch/tmux/
	// STONITH) is present. Assert at least the lease + tmux detail surfaced so
	// we know the verbose path actually renders the jargon-bearing strings.
	for _, w := range []string{"epoch", "tmux", "lease"} {
		if !strings.Contains(strings.ToLower(verbose), w) {
			t.Errorf("--verbose output missing engineer detail %q:\n%s", w, verbose)
		}
	}
	// And the plain lines are still there too.
	if !strings.Contains(verbose, "not responding") {
		t.Errorf("--verbose output missing the plain symptom line:\n%s", verbose)
	}
}

// The canonical stuck-handoff status line itself must be jargon-free (it is
// what `fleet status` and the doctor headline both show the operator).
func TestDoctor_T22_StuckHandoffLine_NoJargon(t *testing.T) {
	line := doctorStatusHandoffStuck.plainStatusLine()
	for _, w := range jargonWords {
		if strings.Contains(strings.ToLower(line), strings.ToLower(w)) {
			t.Errorf("stuck-handoff status line leaked jargon %q: %q", w, line)
		}
	}
	if !strings.Contains(line, "handoff to a fresh coordinator didn't complete") {
		t.Errorf("stuck-handoff line lost its canonical phrasing: %q", line)
	}
}

// codex PR6 iter-20 [P2]: a recovery error returned to main() (printed
// verbatim as "error: ...") must be SANITIZED in non-verbose mode — no jargon
// or internal paths leak. Under --verbose the raw error is allowed.
func TestDoctor_T20_ReturnedError_SanitizedNonVerbose(t *testing.T) {
	jargonErr := errors.New("coordlock.WriteCheckpoint: lease epoch /home/u/.fleet/projects/p/.locks/coordinator.flock torn")
	report := doctorReport{projects: []doctorProjectReport{{
		project: "p", status: doctorStatusDead, fixPlanned: true, fixErr: jargonErr,
	}}}

	origGather, origFix := doctorGatherFn, doctorFixFn
	doctorGatherFn = func(doctorOpts) (doctorReport, error) { return report, nil }
	doctorFixFn = func(doctorOpts, *doctorReport, io.Writer, io.Writer) {}
	t.Cleanup(func() { doctorGatherFn, doctorFixFn = origGather, origFix })

	var out bytes.Buffer
	err := runDoctor(doctorOpts{project: "p", fix: true, verbose: false}, &out, &out)
	if err == nil {
		t.Fatal("expected a non-nil exit error for a recovery failure")
	}
	low := strings.ToLower(err.Error())
	for _, w := range jargonWords {
		if strings.Contains(low, strings.ToLower(w)) {
			t.Errorf("non-verbose returned error leaked jargon %q: %q", w, err.Error())
		}
	}
	if strings.Contains(err.Error(), "/home/") || strings.Contains(err.Error(), ".fleet") {
		t.Errorf("non-verbose returned error leaked a path: %q", err.Error())
	}

	// Verbose: the raw error IS returned (engineer surface).
	out.Reset()
	verr := runDoctor(doctorOpts{project: "p", fix: true, verbose: true}, &out, &out)
	if verr == nil || !strings.Contains(verr.Error(), "coordlock.WriteCheckpoint") {
		t.Errorf("verbose should return the raw error, got: %v", verr)
	}
}

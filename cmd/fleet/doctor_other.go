//go:build !linux && !darwin

package main

// doctor_other.go — the non-linux/darwin stub for `fleet doctor`. The lease
// + authenticated-kill primitives the real doctor leans on are linux||darwin
// only (coordlock is build-tagged), so on other platforms there is never a
// coordinator lease to diagnose or recover. Mirrors lease_check_other.go:
// report the feature is unavailable rather than failing — the command still
// builds and runs everywhere.

import "io"

// gatherDoctorReport on non-unix returns the "unsupported" report so the
// renderer prints the unavailable notice. Mutates nothing.
func gatherDoctorReport(_ doctorOpts) (doctorReport, error) {
	return doctorReport{unsupported: true}, nil
}

// doctorRunFix on non-unix is a no-op: there is no lease to recover.
func doctorRunFix(_ doctorOpts, _ *doctorReport, _, _ io.Writer) {}

// emitStuckHandoffSection on non-unix is a no-op: with no lease primitive
// there is no coordinator handoff to be stuck. Keeps `fleet status` building
// everywhere.
func emitStuckHandoffSection(_, _ io.Writer) {}

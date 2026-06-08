//go:build darwin

package coordlock

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// platform_darwin.go — darwin-pinned liveness primitives for the lease.
//
// Three facts the lease needs that are inherently OS-specific:
//
//	pidStartNanos(pid)  — process birthtime, to make pid-liveness
//	                      PID-reuse-safe (kill(pid,0) alone is unsafe; a
//	                      recycled PID would read as "alive"). On darwin
//	                      this is KERN_PROC_PID's kp_proc.p_starttime.
//	monotonicNanos()    — raw CLOCK_MONOTONIC ns for TTL elapsed, immune
//	                      to wall-clock/NTP jumps (T24).
//	bootID()            — a per-boot identity, so a renewed_at_mono value
//	                      from a previous boot reads as expired (P3). On
//	                      darwin there's no /proc boot_id; we derive a
//	                      stable per-boot token from kern.boottime.

// pidStartNanos returns the start time of pid in Unix nanoseconds, or an
// error if the process does not exist / cannot be inspected. Two calls
// for the same live pid return the same value; a recycled pid returns a
// different (later) value, which is what makes liveness PID-reuse-safe.
func pidStartNanos(pid int) (int64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("pidStartNanos: invalid pid %d", pid)
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("pidStartNanos: sysctl kern.proc.pid %d: %w", pid, err)
	}
	tv := kp.Proc.P_starttime
	return tv.Nano(), nil
}

// ppidOf returns the parent pid of pid via KERN_PROC_PID's
// kp_eproc.e_ppid. Used by the skill-side ownership proof to walk the
// getppid chain and confirm the lease owner is an ancestor of the calling
// tick. Returns (0,false) if the pid is gone / unreadable.
func ppidOf(pid int) (int, bool) {
	if pid <= 0 {
		return 0, false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, false
	}
	return int(kp.Eproc.Ppid), true
}

// monotonicNanos returns raw CLOCK_MONOTONIC nanoseconds. The value is
// meaningful only as a difference (elapsed), never as an absolute time.
// It does not move when the wall clock is stepped by NTP/admin, so the
// TTL elapsed check (now-renewed_at) cannot be spuriously expired or kept
// alive by a clock jump (T24).
func monotonicNanos() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		// CLOCK_MONOTONIC is always available on darwin; this branch is
		// unreachable in practice. Surface a sentinel rather than
		// panicking — callers compare elapsed only.
		return 0
	}
	return ts.Nano()
}

// bootID returns a stable per-boot identity. A monotonic-ns value is only
// comparable within the same boot (the clock resets on reboot), so the
// epoch record carries the boot-id and a reader treats a record from a
// different boot as expired/stealable (P3). On darwin we use the boot
// time (kern.boottime) as the token — stable for the life of the boot,
// distinct across reboots.
func bootID() string {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		// No reliable boot identity available. Fall back to a
		// host-qualified marker so the field is non-empty (an empty
		// string would make every record look cross-boot/expired).
		if hn, herr := os.Hostname(); herr == nil {
			return "unknown-boot@" + hn
		}
		return "unknown-boot"
	}
	return fmt.Sprintf("darwin-boot-%d.%06d", tv.Sec, tv.Usec)
}

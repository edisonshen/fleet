//go:build linux

package coordlock

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// platform_linux.go — linux-pinned liveness primitives for the lease.
// See platform_darwin.go for the contract of each function; only the
// OS mechanism differs.
//
//	pidStartNanos(pid)  — /proc/<pid>/stat field 22 (starttime), in clock
//	                      ticks since boot, scaled to ns by SC_CLK_TCK.
//	                      Returned as ns-since-boot (NOT Unix ns) — it is
//	                      only ever compared for equality against another
//	                      reading on the SAME boot, so a boot-relative
//	                      origin is fine and avoids a boottime read.
//	monotonicNanos()    — raw CLOCK_MONOTONIC ns (T24).
//	bootID()            — /proc/sys/kernel/random/boot_id (a UUID minted
//	                      once per boot; ideal cross-boot guard for P3).

// clkTck is the kernel's USER_HZ (clock ticks per second) used to scale
// /proc/<pid>/stat field 22. SC_CLK_TCK is 100 on virtually every Linux,
// but read it rather than hardcode.
var clkTck = func() int64 {
	if v, err := unix.Sysconf(unix.SC_CLK_TCK); err == nil && v > 0 {
		return v
	}
	return 100
}()

// pidStartNanos returns pid's start time as nanoseconds since boot, or an
// error if the process does not exist / cannot be inspected. The value is
// boot-relative (not Unix epoch); it is only ever compared for equality
// against another reading on the same boot (PID-reuse safety), so the
// origin does not matter.
//
// /proc/<pid>/stat layout is "pid (comm) state ...". comm can contain
// spaces and parens, so we split on the LAST ')' and index fields after
// it: field 22 (starttime) is index 19 in the post-')' slice (fields 3..
// are at index 0..).
func pidStartNanos(pid int) (int64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("pidStartNanos: invalid pid %d", pid)
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("pidStartNanos: read /proc/%d/stat: %w", pid, err)
	}
	s := string(b)
	close := strings.LastIndexByte(s, ')')
	if close < 0 || close+2 > len(s) {
		return 0, fmt.Errorf("pidStartNanos: malformed /proc/%d/stat", pid)
	}
	// Fields after comm: state(3) ppid(4) ... starttime(22). The slice
	// rest[0] == field 3, so starttime is rest[22-3] == rest[19].
	rest := strings.Fields(s[close+2:])
	const starttimeIdx = 19
	if len(rest) <= starttimeIdx {
		return 0, fmt.Errorf("pidStartNanos: /proc/%d/stat has %d post-comm fields, need >%d", pid, len(rest), starttimeIdx)
	}
	ticks, err := strconv.ParseInt(rest[starttimeIdx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pidStartNanos: parse starttime for pid %d: %w", pid, err)
	}
	// ticks / clkTck = seconds since boot; scale to ns without losing the
	// sub-second part: ticks * 1e9 / clkTck.
	return ticks * 1_000_000_000 / clkTck, nil
}

// monotonicNanos returns raw CLOCK_MONOTONIC nanoseconds (elapsed-only;
// jump-immune — T24).
func monotonicNanos() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0
	}
	return ts.Nano()
}

// bootID returns the kernel's per-boot UUID from
// /proc/sys/kernel/random/boot_id — minted once per boot, so a record
// carrying a different boot_id is provably from a previous boot and its
// monotonic timestamps are meaningless (treated as expired — P3).
func bootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		if hn, herr := os.Hostname(); herr == nil {
			return "unknown-boot@" + hn
		}
		return "unknown-boot"
	}
	return "linux-boot-" + strings.TrimSpace(string(b))
}

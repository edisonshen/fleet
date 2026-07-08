//go:build !integration

package main

import (
	"fmt"
	"os"

	"github.com/edisonshen/fleet/internal/spawn"
)

// assertNoStandbyLaunches is the authoritative fork-bomb gate for the default
// test lane in cmd/fleet. TestMain calls it LAST — after the socket-leak reap —
// replacing the trailing os.Exit(code), so the reap still runs before this
// exits. See internal/spawn/standby_counter.go for the shared counter and
// docs/DESIGN-spawn-test-fork-bomb-root-fix.md for the rationale.
func assertNoStandbyLaunches(code int) {
	if n := spawn.StandbyLaunchCount(); n > 0 {
		fmt.Fprintf(os.Stderr,
			"FORK-BOMB GATE: default test lane launched %d lease-wrapped standby "+
				"coordinator(s); a standby-spawning test must move to the integration "+
				"lane (//go:build integration). "+
				"See docs/DESIGN-spawn-test-fork-bomb-root-fix.md.\n", n)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

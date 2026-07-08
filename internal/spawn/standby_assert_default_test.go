//go:build !integration

package spawn

import (
	"fmt"
	"os"
)

// assertNoStandbyLaunches is the authoritative fork-bomb gate for the default
// test lane. TestMain calls it LAST — after the socket-leak reap — replacing the
// trailing os.Exit(code), so the reap still runs before this exits.
//
// If any lease-wrapped standby was launched (counter > 0), a standby-spawning
// test was left in the default lane: fail the suite loudly with a nonzero exit
// rather than let it pile up 10-minute panes and fork-bomb the box. Otherwise
// exit with the suite's own code.
//
// The //go:build integration twin (standby_assert_integration_test.go) is a
// no-op that just exits with code, because standbys ARE expected there.
func assertNoStandbyLaunches(code int) {
	if n := StandbyLaunchCount(); n > 0 {
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

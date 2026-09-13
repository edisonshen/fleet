package coorde2e_test

import (
	"os"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/testutil"
)

// TestMain gives coorde2e the same suite-start/end sweep the other
// real-tmux packages carry: helpers_test.go spawns real sessions on an
// isolated socket, so this package would otherwise leak sockets onto the
// CI runner exactly the way tmuxfake/parity_test.go once did.
//
// External test package (coorde2e_test) so importing internal/testutil
// for the sweeper cannot cycle with coorde2e.
func TestMain(m *testing.M) {
	cleanup := testutil.IsolateSweepDir()
	_ = testutil.Sweep(time.Hour)
	code := m.Run()
	_ = testutil.ForceReapTestServers()
	cleanup() // before os.Exit — os.Exit skips defers
	os.Exit(code)
}

//go:build integration

package spawn

import "os"

// assertNoStandbyLaunches is a no-op in the integration lane: standby launches
// ARE expected there (the genuine spawner tests run with
// FLEET_LEASE_FAILOVER=1). It just exits with the suite's own code, mirroring
// the !integration twin's signature so TestMain compiles in both builds.
func assertNoStandbyLaunches(code int) { os.Exit(code) }

package spawn

import "sync/atomic"

// standbyLaunchCount records how many times Spawn has launched a real
// lease-wrapped standby coordinator (a `coord-run --standby` tmux pane) via a
// SUCCESSFUL tmux.Spawn. It is the authoritative gate for the test fork-bomb
// fix: each non-integration TestMain asserts this is zero after m.Run() (see
// assertNoStandbyLaunches), so a genuine standby-spawning test accidentally
// left in the default lane fails the suite loudly instead of silently piling
// up 10-minute panes.
//
// It is incremented ONLY after tmux.Spawn of the wrapped coord returns nil, so
// the rollback case (TestSpawn_LeaseCoord_RollsBackPrelaunchRecordOnTmuxFailure,
// which forces tmux.Spawn to fail) does NOT count — it never launches a
// surviving standby. In normal production runs nobody reads the counter, so the
// hook is a no-op beyond a single atomic add on the coord-spawn path.
var standbyLaunchCount atomic.Int64

// recordStandbyLaunch bumps the successful-standby-launch counter. Called from
// Spawn immediately after a lease-wrapped tmux.Spawn succeeds.
func recordStandbyLaunch() { standbyLaunchCount.Add(1) }

// StandbyLaunchCount returns the number of successful lease-wrapped standby
// launches observed in this process. Read by each package's non-integration
// TestMain to assert the default lane spawned zero standbys.
func StandbyLaunchCount() int64 { return standbyLaunchCount.Load() }

//go:build !linux && !darwin

package coordlock

// CurrentOwner is unsupported on platforms without the coordinator lease
// primitive.
func CurrentOwner(string) (Owner, bool) { return Owner{}, false }

// LeaseRecordActive is false on platforms without the lease primitive (no epoch
// records are ever written), so handoff delivery treats every coord as a
// legacy/bare coord and direct-sends — matching the never-wrapped spawn path.
func LeaseRecordActive(string) bool { return false }

// FailoverEnabled is false on platforms without the lease primitive.
func FailoverEnabled() bool { return false }

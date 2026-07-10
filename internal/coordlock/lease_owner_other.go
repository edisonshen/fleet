//go:build !linux && !darwin

package coordlock

// CurrentOwner is unsupported on platforms without the coordinator lease
// primitive.
func CurrentOwner(string) (Owner, bool) { return Owner{}, false }

// LiveOwner is unsupported on platforms without the coordinator lease
// primitive.
func LiveOwner(string) (Owner, bool) { return Owner{}, false }

// LeaseRecordActive is false on platforms without the lease primitive (no
// flock is ever held), so handoff delivery treats every coord as a
// legacy/bare coord and direct-sends — matching the never-wrapped spawn path.
func LeaseRecordActive(string) bool { return false }

// LeaseSupported is false on platforms without the lease primitive.
func LeaseSupported() bool { return false }

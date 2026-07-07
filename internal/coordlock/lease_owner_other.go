//go:build !linux && !darwin

package coordlock

import "time"

// CurrentOwner is unsupported on platforms without the coordinator lease
// primitive.
func CurrentOwner(string) (Owner, bool) { return Owner{}, false }

// LiveOwner is unsupported on platforms without the coordinator lease
// primitive.
func LiveOwner(string) (Owner, bool) { return Owner{}, false }

// CurrentHandoff is unsupported without the lease primitive (no epoch record
// is ever written), so there is never a successor reservation to read.
func CurrentHandoff(string) (Handoff, bool) { return Handoff{}, false }

// CurrentStarting is unsupported without the lease primitive.
func CurrentStarting(string) (StartingStatus, bool) { return StartingStatus{}, false }

// SupersedeStartingLease is unsupported without the lease primitive.
func SupersedeStartingLease(string, int64) (bool, error) { return false, nil }

// ClaimStartingRecord is unsupported without the lease primitive; the caller
// falls back to the legacy (non-lease) spawn path.
func ClaimStartingRecord(string, string) (bool, error) { return true, nil }

// ReserveHandoff is unsupported without the lease primitive.
func ReserveHandoff(string, string, time.Duration) (bool, error) { return false, nil }

// LeaseRecordActive is false on platforms without the lease primitive (no epoch
// records are ever written), so handoff delivery treats every coord as a
// legacy/bare coord and direct-sends — matching the never-wrapped spawn path.
func LeaseRecordActive(string) bool { return false }

// FailoverEnabled is false on platforms without the lease primitive.
func FailoverEnabled() bool { return false }

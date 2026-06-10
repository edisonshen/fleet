//go:build !linux && !darwin

package coordlock

// CurrentOwner is unsupported on platforms without the coordinator lease
// primitive.
func CurrentOwner(string) (Owner, bool) { return Owner{}, false }

// FailoverEnabled is false on platforms without the lease primitive.
func FailoverEnabled() bool { return false }

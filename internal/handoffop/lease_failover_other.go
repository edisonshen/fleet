//go:build !linux && !darwin

package handoffop

func leaseFailoverEnabled() bool { return false }

func leaseActiveOwnerPID(string) (int, bool) { return 0, false }

func leaseLeaderPresent(string) bool { return false }

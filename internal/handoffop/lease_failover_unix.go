//go:build linux || darwin

package handoffop

import "github.com/edisonshen/fleet/internal/coordlock"

func leaseFailoverEnabled() bool { return coordlock.FailoverEnabled() }

func leaseActiveOwnerPID(project string) (int, bool) {
	return coordlock.CurrentActiveOwnerPID(project)
}

func leaseLeaderPresent(project string) bool {
	return coordlock.LeaderPresent(project)
}

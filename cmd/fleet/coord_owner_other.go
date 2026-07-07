//go:build !linux && !darwin

package main

// coordOwnerLeaseIdentity on non-linux/darwin: the lease primitive is
// unsupported here (coordlock is build-tagged linux||darwin), so there is never
// a lease to read. Report all-empty so loop.py's gate falls through exactly as
// it would when failover is disabled.
func coordOwnerLeaseIdentity(_ string) coordOwnerInfo {
	return coordOwnerInfo{}
}

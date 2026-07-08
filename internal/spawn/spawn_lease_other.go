//go:build !linux && !darwin

package spawn

// leaseSupported (non-linux/darwin): ALWAYS false. The coordinator lease
// primitive (internal/coordlock) is build-tagged linux||darwin, and cmd/fleet's
// coord-run stub reports the lease unsupported on these platforms — so Spawn
// must run the legacy bare-child path and NEVER wrap a coord in `fleet
// coord-run`.
func leaseSupported() bool { return false }

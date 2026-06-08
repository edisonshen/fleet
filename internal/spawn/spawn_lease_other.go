//go:build !linux && !darwin

package spawn

// leaseFailoverEnabled (non-linux/darwin): ALWAYS false. The coordinator
// lease primitive (internal/coordlock) is build-tagged linux||darwin, and
// cmd/fleet's coord-run stub reports the lease unsupported on these
// platforms — so Spawn must run the legacy bare-child path and NEVER wrap a
// coord in `fleet coord-run`, regardless of FLEET_LEASE_FAILOVER (codex
// PR4 [P2]).
func leaseFailoverEnabled() bool { return false }

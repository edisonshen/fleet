//go:build linux || darwin

package spawn

import (
	"os"
	"strings"
)

// leaseFailoverEnabled (linux||darwin): mirror coordlock.FailoverEnabled's
// tri-state gate. Kept as a local copy rather than importing coordlock so
// spawn.go's all-platform file stays import-clean; this MUST stay
// byte-for-byte consistent with coordlock.parseFailover. PR4 flipped the
// default to ON: enabled unless EXPLICITLY one of 0/false/off/no
// (case-insensitive).
func leaseFailoverEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLEET_LEASE_FAILOVER"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

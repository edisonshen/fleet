//go:build linux || darwin

package handoffop

import "github.com/edisonshen/fleet/internal/coordlock"

func leaseFailoverEnabled() bool { return coordlock.FailoverEnabled() }

//go:build !linux && !darwin

package handoffop

func leaseFailoverEnabled() bool { return false }

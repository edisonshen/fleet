//go:build linux || darwin

package spawn

// leaseSupported (linux||darwin) reports platform support for the
// coordinator lease. spawn.go's all-platform file keeps this as a tiny local
// helper rather than importing coordlock.
func leaseSupported() bool {
	return true
}

//go:build !linux && !darwin

package main

// leaseCheckOwnership on non-linux/darwin: the lease primitive is
// unsupported here (coordlock is build-tagged linux||darwin), so there is
// never a lease to fence against. Report OK so the skill behaves exactly as
// pre-lease — consistent with leaseFailoverEnabled never selecting the
// lease path on these platforms.
func leaseCheckOwnership(_ string, _ int) (leaseCheckOutcome, error) {
	return leaseCheckOK, nil
}

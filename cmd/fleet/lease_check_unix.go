//go:build linux || darwin

package main

import (
	"errors"

	"github.com/edisonshen/fleet/internal/coordlock"
)

// leaseCheckOwnership runs the real ancestor-ownership proof via coordlock
// (linux/darwin only). leaseCheckNotOwner maps the typed ErrNotLeaseOwner
// refusal; any other error is leaseCheckError. reacquire selects the
// coordinator tick's renew-in-place variant; false is strictly read-only.
func leaseCheckOwnership(project string, pid int, reacquire bool) (leaseCheckOutcome, error) {
	var err error
	if reacquire {
		err = coordlock.LeaseCheckByAncestorReacquire(project, pid)
	} else {
		err = coordlock.LeaseCheckByAncestor(project, pid)
	}
	switch {
	case err == nil:
		return leaseCheckOK, nil
	case errors.Is(err, coordlock.ErrNotLeaseOwner):
		return leaseCheckNotOwner, err
	default:
		return leaseCheckError, err
	}
}

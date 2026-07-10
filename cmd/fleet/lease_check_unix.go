//go:build linux || darwin

package main

import (
	"errors"

	"github.com/edisonshen/fleet/internal/coordlock"
)

// leaseCheckOwnership runs the real ancestor-ownership proof via coordlock
// (linux/darwin only). leaseCheckNotOwner maps the typed ErrNotLeaseOwner
// refusal; any other error is leaseCheckError. PR-2 (D7): the proof is strictly
// READ-ONLY — it walks the caller's ppid chain to the FLOCK holder. There is no
// epoch to renew, so the deleted --reacquire variant is gone (holding the flock
// IS ownership).
func leaseCheckOwnership(project string, pid int) (leaseCheckOutcome, error) {
	err := coordlock.LeaseCheckByAncestor(project, pid)
	switch {
	case err == nil:
		return leaseCheckOK, nil
	case errors.Is(err, coordlock.ErrNotLeaseOwner):
		return leaseCheckNotOwner, err
	default:
		return leaseCheckError, err
	}
}

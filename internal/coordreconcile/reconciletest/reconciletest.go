package reconciletest

import (
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
)

// State is a deterministic coordreconcile.Deps builder. Tests set only the
// fields relevant to a row; the zero value is a free lease.
type State struct {
	LiveOwner *coordlock.Owner
	Starting  *coordlock.StartingStatus
	Handoff   *coordlock.Handoff

	SupersedeOK  bool
	SupersedeErr error
	ClaimOK      bool
	ClaimErr     error

	SupersedeCalls int
	SupersedeEpoch int64
	ClaimCalls     int
	ClaimedID      string
}

func (s *State) Deps() coordreconcile.Deps {
	return coordreconcile.Deps{
		LiveOwner: func(string) (coordlock.Owner, bool) {
			if s.LiveOwner == nil {
				return coordlock.Owner{}, false
			}
			return *s.LiveOwner, true
		},
		CurrentStarting: func(string) (coordlock.StartingStatus, bool) {
			if s.Starting == nil {
				return coordlock.StartingStatus{}, false
			}
			return *s.Starting, true
		},
		CurrentHandoff: func(string) (coordlock.Handoff, bool) {
			if s.Handoff == nil {
				return coordlock.Handoff{}, false
			}
			return *s.Handoff, true
		},
		Supersede: func(_ string, epoch int64) (bool, error) {
			s.SupersedeCalls++
			s.SupersedeEpoch = epoch
			return s.SupersedeOK, s.SupersedeErr
		},
		ClaimStarting: func(_ string, id string) (bool, error) {
			s.ClaimCalls++
			s.ClaimedID = id
			return s.ClaimOK, s.ClaimErr
		},
	}
}

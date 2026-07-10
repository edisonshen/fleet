package reconciletest

import (
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
)

// State is a deterministic coordreconcile.Deps builder (flock-only, PR-2 D4).
// Tests set only the fields relevant to a row; the zero value is a free lease
// with no handoff journal (HandoffNone) — i.e. a clean Spawn.
//
// LiveOwner models the flock: non-nil ⇒ a live holder (busy⇒owner); nil ⇒ the
// flock is FREE, and the ClassifyHandoff journal disposition then decides
// Wait (in-flight) vs Spawn (none/abandoned-after-clear).
type State struct {
	LiveOwner *coordlock.Owner

	// HandoffDisp + HandoffJournal model coordlock.ClassifyFreeFlockHandoff for
	// a FLOCK-FREE project. ClassifyErr models a torn-journal read.
	HandoffDisp    coordlock.HandoffDisposition
	HandoffJournal coordlock.HandoffJournal
	ClassifyErr    error

	// ClearErr models a failed clear-before-spawn; ClearCalls counts invocations.
	ClearErr   error
	ClearCalls int
}

func (s *State) Deps() coordreconcile.Deps {
	return coordreconcile.Deps{
		LiveOwner: func(string) (coordlock.Owner, bool) {
			if s.LiveOwner == nil {
				return coordlock.Owner{}, false
			}
			return *s.LiveOwner, true
		},
		ClassifyHandoff: func(string) (coordlock.HandoffDisposition, coordlock.HandoffJournal, error) {
			return s.HandoffDisp, s.HandoffJournal, s.ClassifyErr
		},
		ClearHandoff: func(string) error {
			s.ClearCalls++
			return s.ClearErr
		},
	}
}

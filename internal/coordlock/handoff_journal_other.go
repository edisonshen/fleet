//go:build !linux && !darwin

package coordlock

import (
	"errors"
	"fmt"
)

// Non-linux/darwin stubs for the handoff journal (handoff_journal.go). The
// lease primitive is unsupported here, so there is never a journal to read or
// write; every reader reports "no journal" and every writer is a no-op-ish
// error. Untagged packages (coordreconcile, handoffop, cmd/fleet) reference
// these symbols, so the types + signatures must exist on every platform.

type HandoffJournal struct {
	Project           string   `json:"project"`
	OldAgentID        string   `json:"old_agent_id"`
	SuccessorID       string   `json:"successor_id"`
	BarrierID         string   `json:"barrier_id"`
	SuccessorPID      int      `json:"successor_pid"`
	SuccessorPidStart int64    `json:"successor_pid_start"`
	SuccessorBootID   string   `json:"successor_boot_id"`
	SuccessorSession  string   `json:"successor_session,omitempty"`
	RespawnCommand    []string `json:"respawn_command,omitempty"`
	RespawnCwd        string   `json:"respawn_cwd,omitempty"`
	RespawnDocPath    string   `json:"respawn_doc_path,omitempty"`
	StampWall         int64    `json:"stamp_wall"`
}

type HandoffDisposition int

const (
	HandoffNone HandoffDisposition = iota
	HandoffInFlight
	HandoffAbandoned
)

func (d HandoffDisposition) String() string {
	switch d {
	case HandoffNone:
		return "none"
	case HandoffInFlight:
		return "in-flight"
	case HandoffAbandoned:
		return "abandoned"
	default:
		return fmt.Sprintf("handoff-disposition(%d)", int(d))
	}
}

var ErrHandoffJournalExists = errors.New("coordlock: handoff journal already exists")

func HandoffBarrierPath(string, string) (string, error) {
	return "", errors.New("coordlock: handoff journal unsupported on this platform")
}

func NewBarrierID() string { return "" }

func CreateHandoffJournal(HandoffJournal) error {
	return errors.New("coordlock: handoff journal unsupported on this platform")
}

func ReadHandoffJournal(string) (HandoffJournal, bool, error) { return HandoffJournal{}, false, nil }

func DeleteHandoffJournal(string) error { return nil }

func HandoffSuccessorAlive(HandoffJournal) bool { return false }

func ClassifyFreeFlockHandoff(string) (HandoffDisposition, HandoffJournal, error) {
	return HandoffNone, HandoffJournal{}, nil
}

func CurrentHandoffSuccessor(string) (string, bool) { return "", false }

package handoffdelivery

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
)

// fakeClock advances deterministically on each Sleep so the delivery loop hits
// its deadline without real wall-clock sleeps.
func fakeClock() (now func() time.Time, sleep func(time.Duration)) {
	cur := time.Unix(0, 0)
	now = func() time.Time { return cur }
	sleep = func(d time.Duration) { cur = cur.Add(d) }
	return now, sleep
}

// No owner is ever observed (legacy/bare coord, no lease record). The call must
// time out with ErrNoOwnerObserved so the caller can fall back to a direct send
// instead of looping the same doomed owner-poll forever.
func TestDeliverToCurrentOwner_NoOwnerEver_ReturnsSentinel(t *testing.T) {
	now, sleep := fakeClock()
	_, err := DeliverToCurrentOwner(Options{
		Project: "rainier",
		Prompt:  "read the doc",
		Timeout: time.Second,
		Poll:    100 * time.Millisecond,
	}, Deps{
		CurrentOwner: func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{}, false // never an owner
		},
		LoadAgent:    func(string) (*agent.Record, error) { return nil, nil },
		WaitReady:    func(string) error { return nil },
		SessionAlive: func(string) (bool, error) { return true, nil },
		SendVerified: func(string, string) (bool, error) { return true, nil },
		Now:          now,
		Sleep:        sleep,
	})
	if err == nil {
		t.Fatal("expected timeout error when no owner is ever observed")
	}
	if !errors.Is(err, ErrNoOwnerObserved) {
		t.Fatalf("error = %v, want ErrNoOwnerObserved", err)
	}
}

// An owner WAS observed but its record never loads (transient). The error must
// NOT be ErrNoOwnerObserved — the caller must keep the doc pending for a
// real-owner retry rather than fall back to a stale direct send.
func TestDeliverToCurrentOwner_OwnerSeenButUndeliverable_NotSentinel(t *testing.T) {
	now, sleep := fakeClock()
	_, err := DeliverToCurrentOwner(Options{
		Project: "rainier",
		Prompt:  "read the doc",
		Timeout: time.Second,
		Poll:    100 * time.Millisecond,
	}, Deps{
		CurrentOwner: func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{AgentID: "winner1", PID: 1, PidStart: 2}, true
		},
		LoadAgent:    func(string) (*agent.Record, error) { return nil, fmt.Errorf("transient load failure") },
		WaitReady:    func(string) error { return nil },
		SessionAlive: func(string) (bool, error) { return true, nil },
		SendVerified: func(string, string) (bool, error) { return true, nil },
		Now:          now,
		Sleep:        sleep,
	})
	if err == nil {
		t.Fatal("expected timeout error when owner is undeliverable")
	}
	if errors.Is(err, ErrNoOwnerObserved) {
		t.Fatalf("error = %v, must NOT be ErrNoOwnerObserved (owner was observed)", err)
	}
}

// TestDeliverToCurrentOwnerTargetsLeaseOwnerAndDelivers pins the happy path:
// the active lease owner is resolved to its agent record and the prompt is
// verified-sent to that owner's tmux session, returning the owner record. The
// lease owner IS the coord identity now, so there is nothing to promote after
// the send.
func TestDeliverToCurrentOwnerTargetsLeaseOwnerAndDelivers(t *testing.T) {
	winner := &agent.Record{
		ID:          "winner1",
		Project:     "rainier",
		TmuxSession: "fleet-winner1",
	}
	var sentSession, sentPrompt string
	rec, err := DeliverToCurrentOwner(Options{
		Project: "rainier",
		Prompt:  "read the doc",
		Timeout: time.Second,
		Poll:    time.Millisecond,
	}, Deps{
		CurrentOwner: func(project string) (coordlock.Owner, bool) {
			if project != "rainier" {
				t.Fatalf("CurrentOwner project = %q", project)
			}
			return coordlock.Owner{AgentID: winner.ID, PID: 4242, PidStart: 99, EngineStamped: true}, true
		},
		LoadAgent: func(id string) (*agent.Record, error) {
			if id != winner.ID {
				t.Fatalf("loaded non-owner id %q", id)
			}
			return winner, nil
		},
		WaitReady:    func(string) error { return nil },
		SessionAlive: func(string) (bool, error) { return true, nil },
		SendVerified: func(session, prompt string) (bool, error) {
			sentSession, sentPrompt = session, prompt
			return true, nil
		},
		Now:   time.Now,
		Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("DeliverToCurrentOwner: %v", err)
	}
	if rec != winner {
		t.Fatalf("delivered rec = %+v, want winner", rec)
	}
	if sentSession != winner.TmuxSession || sentPrompt != "read the doc" {
		t.Fatalf("sent (%q,%q), want (%q,%q)", sentSession, sentPrompt, winner.TmuxSession, "read the doc")
	}
}

// TestDeliverToCurrentOwner_OwnerFlipAfterSend_StaysPending pins codex iter-32
// P1: when the lock owner flips AFTER the verified send but BEFORE we can
// confirm the recipient still owns the lease, the prompt landed on the
// now-superseded owner. DeliverToCurrentOwner must surface this as PENDING (an
// error), NOT success — otherwise the caller deletes the durable queue + drops
// the superseded replacement, stranding the handoff (the new owner never got
// the prompt, discovery points at a stale agent, no retry).
//
// Invariant that REMAINS: no double-send WITHIN this call. The retry (a fresh
// DeliverToCurrentOwner) re-targets the new stable owner (a benign duplicate
// prompt, §5c).
func TestDeliverToCurrentOwner_OwnerFlipAfterSend_StaysPending(t *testing.T) {
	ownerA := coordlock.Owner{AgentID: "owner-a", PID: 1001, PidStart: 11}
	ownerB := coordlock.Owner{AgentID: "owner-b", PID: 2002, PidStart: 22}
	recA := &agent.Record{ID: ownerA.AgentID, Project: "rainier", TmuxSession: "fleet-owner-a"}
	recB := &agent.Record{ID: ownerB.AgentID, Project: "rainier", TmuxSession: "fleet-owner-b"}

	currentCalls := 0
	sendCalls := 0
	var sentSession, sentPrompt string
	var stderr strings.Builder

	rec, err := DeliverToCurrentOwner(Options{
		Project: "rainier",
		Prompt:  "resume now",
		Timeout: time.Second,
		Poll:    time.Millisecond,
		Stderr:  &stderr,
	}, Deps{
		CurrentOwner: func(project string) (coordlock.Owner, bool) {
			if project != "rainier" {
				t.Fatalf("CurrentOwner project = %q", project)
			}
			currentCalls++
			if currentCalls <= 2 {
				return ownerA, true
			}
			return ownerB, true
		},
		LoadAgent: func(id string) (*agent.Record, error) {
			switch id {
			case ownerA.AgentID:
				return recA, nil
			case ownerB.AgentID:
				return recB, nil
			default:
				t.Fatalf("LoadAgent id = %q", id)
				return nil, nil
			}
		},
		WaitReady:    func(string) error { return nil },
		SessionAlive: func(string) (bool, error) { return true, nil },
		SendVerified: func(session, prompt string) (bool, error) {
			sendCalls++
			sentSession, sentPrompt = session, prompt
			return true, nil
		},
		Now:   time.Now,
		Sleep: func(time.Duration) {},
	})
	if err == nil {
		t.Fatalf("DeliverToCurrentOwner must return an error (pending) on post-send owner flip; got rec=%+v nil err", rec)
	}
	if rec != nil {
		t.Fatalf("delivered rec = %+v, want nil on pending owner-flip", rec)
	}
	if sendCalls != 1 {
		t.Fatalf("SendVerified calls = %d, want 1 (no double-send within the call)", sendCalls)
	}
	if sentSession != recA.TmuxSession || sentPrompt != "resume now" {
		t.Fatalf("sent (%q,%q), want (%q,%q)", sentSession, sentPrompt, recA.TmuxSession, "resume now")
	}
	if !strings.Contains(stderr.String(), "changed after verified send") {
		t.Fatalf("stderr = %q, want owner-flip warning", stderr.String())
	}
	if !strings.Contains(err.Error(), "flipped after verified send") {
		t.Fatalf("err = %q, want owner-flip-pending error", err.Error())
	}
}

func TestDeliverToCurrentOwnerUnsubmittedIsNotDelivered(t *testing.T) {
	_, err := DeliverToCurrentOwner(Options{
		Project: "rainier",
		Prompt:  "read the doc",
		Timeout: time.Second,
		Poll:    time.Millisecond,
	}, Deps{
		CurrentOwner: func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{AgentID: "winner1", PID: 4242, PidStart: 99}, true
		},
		LoadAgent: func(string) (*agent.Record, error) {
			return &agent.Record{ID: "winner1", TmuxSession: "fleet-winner1"}, nil
		},
		WaitReady:    func(string) error { return nil },
		SessionAlive: func(string) (bool, error) { return true, nil },
		SendVerified: func(string, string) (bool, error) {
			return false, nil
		},
		Now:   time.Now,
		Sleep: func(time.Duration) {},
	})
	if err == nil {
		t.Fatal("expected unsubmitted prompt to be an error")
	}
	if !strings.Contains(err.Error(), "not submitted") {
		t.Fatalf("error = %v, want not submitted", err)
	}
}

// codex P2 regression: an owner is observed every pass but ownership flips in
// the pre-send recheck window (finishDelivery returns (false,nil)) until the
// deadline. The error must NOT be ErrNoOwnerObserved — owners DID exist, so the
// caller must keep the doc pending for a retry, never direct-send to the loser.
func TestDeliverToCurrentOwner_OwnerKeepsFlipping_NotSentinel(t *testing.T) {
	now, sleep := fakeClock()
	flip := 0
	_, err := DeliverToCurrentOwner(Options{
		Project: "rainier",
		Prompt:  "read the doc",
		Timeout: time.Second,
		Poll:    100 * time.Millisecond,
	}, Deps{
		// Each CurrentOwner call returns a DIFFERENT owner, so the post-read
		// recheck inside finishDelivery never matches -> (false,nil), loop.
		CurrentOwner: func(string) (coordlock.Owner, bool) {
			flip++
			return coordlock.Owner{AgentID: fmt.Sprintf("owner%d", flip), PID: flip, PidStart: int64(flip)}, true
		},
		LoadAgent: func(id string) (*agent.Record, error) {
			return &agent.Record{ID: id, TmuxSession: "fleet-" + id}, nil
		},
		WaitReady:    func(string) error { return nil },
		SessionAlive: func(string) (bool, error) { return true, nil },
		SendVerified: func(string, string) (bool, error) { return true, nil },
		Now:          now,
		Sleep:        sleep,
	})
	if err == nil {
		t.Fatal("expected error when ownership never stabilizes")
	}
	if errors.Is(err, ErrNoOwnerObserved) {
		t.Fatalf("error = %v, must NOT be ErrNoOwnerObserved (owners were observed)", err)
	}
}

// codex iter-22 [P1] regression: CurrentOwner returns ok=false for a stale/dead
// active owner (health-strict), but a lease record IS still on disk. That is NOT
// a legacy/bare coord — the error must NOT be ErrNoOwnerObserved, so callers keep
// the doc pending for the healthy takeover owner instead of direct-sending to the
// queued replacement (which may be a prompt-less standby).
func TestDeliverToCurrentOwner_StaleOwnerButLeaseActive_NotSentinel(t *testing.T) {
	now, sleep := fakeClock()
	_, err := DeliverToCurrentOwner(Options{
		Project: "rainier",
		Prompt:  "read the doc",
		Timeout: time.Second,
		Poll:    100 * time.Millisecond,
	}, Deps{
		CurrentOwner:      func(string) (coordlock.Owner, bool) { return coordlock.Owner{}, false },
		LeaseRecordActive: func(string) bool { return true }, // lease present, owner unhealthy
		LoadAgent:         func(string) (*agent.Record, error) { return nil, nil },
		WaitReady:         func(string) error { return nil },
		SessionAlive:      func(string) (bool, error) { return true, nil },
		SendVerified:      func(string, string) (bool, error) { return true, nil },
		Now:               now,
		Sleep:             sleep,
	})
	if err == nil {
		t.Fatal("expected timeout error when owner is stale but lease is active")
	}
	if errors.Is(err, ErrNoOwnerObserved) {
		t.Fatalf("error = %v, must NOT be ErrNoOwnerObserved (lease record is active)", err)
	}
}

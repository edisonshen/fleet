package handoffdelivery

import (
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
)

func TestDeliverToCurrentOwnerTargetsLeaseOwnerAndPromotesMarker(t *testing.T) {
	winner := &agent.Record{
		ID:          "winner1",
		Project:     "rainier",
		TmuxSession: "fleet-winner1",
	}
	var sentSession, sentPrompt, markerProject, markerID string
	rec, err := DeliverToCurrentOwner(Options{
		Project:       "rainier",
		Prompt:        "read the doc",
		PromoteMarker: true,
		Timeout:       time.Second,
		Poll:          time.Millisecond,
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
		WriteMarker: func(project, id string) error {
			markerProject, markerID = project, id
			return nil
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
	if markerProject != "rainier" || markerID != winner.ID {
		t.Fatalf("marker = (%q,%q), want (rainier,%s)", markerProject, markerID, winner.ID)
	}
}

func TestDeliverToCurrentOwner_OwnerFlipAfterSend_NoDoubleSend(t *testing.T) {
	ownerA := coordlock.Owner{AgentID: "owner-a", PID: 1001, PidStart: 11}
	ownerB := coordlock.Owner{AgentID: "owner-b", PID: 2002, PidStart: 22}
	recA := &agent.Record{ID: ownerA.AgentID, Project: "rainier", TmuxSession: "fleet-owner-a"}
	recB := &agent.Record{ID: ownerB.AgentID, Project: "rainier", TmuxSession: "fleet-owner-b"}

	currentCalls := 0
	sendCalls := 0
	markerWrites := 0
	var sentSession, sentPrompt string
	var stderr strings.Builder

	rec, err := DeliverToCurrentOwner(Options{
		Project:       "rainier",
		Prompt:        "resume now",
		PromoteMarker: true,
		Timeout:       time.Second,
		Poll:          time.Millisecond,
		Stderr:        &stderr,
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
		WriteMarker: func(string, string) error {
			markerWrites++
			return nil
		},
		Now:   time.Now,
		Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("DeliverToCurrentOwner: %v", err)
	}
	if rec != recA {
		t.Fatalf("delivered rec = %+v, want owner A", rec)
	}
	if sendCalls != 1 {
		t.Fatalf("SendVerified calls = %d, want 1", sendCalls)
	}
	if sentSession != recA.TmuxSession || sentPrompt != "resume now" {
		t.Fatalf("sent (%q,%q), want (%q,%q)", sentSession, sentPrompt, recA.TmuxSession, "resume now")
	}
	if markerWrites != 0 {
		t.Fatalf("marker writes = %d, want 0 after post-send owner flip", markerWrites)
	}
	if !strings.Contains(stderr.String(), "changed after verified send") {
		t.Fatalf("stderr = %q, want owner-flip warning", stderr.String())
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

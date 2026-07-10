//go:build linux || darwin

package coordlock

// Lease lifecycle observability. Under the flock-only lease (PR-2) the only
// lifecycle events are lease.acquire (on a successful flock acquire) and
// lease.release (on Release of a held flock). The epoch-era renew sampler /
// renew.fail / demoted / takeover-suppression / epoch.lock events are gone with
// the write path. These tests pin the surviving emitter contract via the cfg
// seam so a regression that drops acquire/release logging is caught.

import (
	"sync"
	"testing"
)

// capturingEmitter records every (evt, data) the lease emits so a test can
// assert the lifecycle without shelling out to fleetlog.
type capturingEmitter struct {
	mu     sync.Mutex
	events []capturedEvent
}

type capturedEvent struct {
	evt  string
	data map[string]any
}

func (c *capturingEmitter) emit(evt string, data map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedEvent{evt: evt, data: data})
}

func (c *capturingEmitter) count(evt string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.evt == evt {
			n++
		}
	}
	return n
}

// TestLeaseAcquireRelease_EmitsLifecycle: a normal acquire→release logs exactly
// one lease.acquire and one lease.release through the cfg.emit seam.
func TestLeaseAcquireRelease_EmitsLifecycle(t *testing.T) {
	setupHome(t)
	const project = "obs-lifecycle"
	em := &capturingEmitter{}
	cfg := testCfg(&fakeClock{}, newFakeLiveness())
	cfg.emit = em.emit

	lease, acquired, err := acquireLease(project, "cand", cfg)
	if err != nil || !acquired {
		t.Fatalf("acquireLease: acquired=%v err=%v", acquired, err)
	}
	lease.Release()

	if got := em.count("lease.acquire"); got != 1 {
		t.Fatalf("lease.acquire count = %d, want 1", got)
	}
	if got := em.count("lease.release"); got != 1 {
		t.Fatalf("lease.release count = %d, want 1", got)
	}
}

// TestLeaseRelease_NoFlockNoEvent: Release with no held flock (a stand-down
// lease that never acquired) emits NOTHING — the event only fires when a real
// flock fd is dropped.
func TestLeaseRelease_NoFlockNoEvent(t *testing.T) {
	em := &capturingEmitter{}
	l := &Lease{cfg: leaseConfig{emit: em.emit}}
	l.Release()
	if got := em.count("lease.release"); got != 0 {
		t.Fatalf("lease.release count = %d, want 0 (no flock held)", got)
	}
}

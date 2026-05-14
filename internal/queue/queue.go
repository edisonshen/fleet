// Package queue owns the JSON files under ~/.fleet/queue/ that
// declare async work items between producers (CLI, fleet-guard skill
// in 4b/c) and consumers (the fleet binary draining inline or via
// the TUI background loop).
//
// The "queue" is just a directory of files. Producer writes one file
// atomically (state.WriteAtomic); consumer lists, reads, processes,
// and deletes. Per-project flock (state.LockProject, Step 6) keeps
// concurrent consumers from racing on the same project. There is no
// queue server, no protocol, no DLQ — that's intentional. See the
// 4a planning thread for the Temporal vs filesystem-journal decision.
//
// Schema versions are explicit so future readers can refuse newer
// shapes rather than silently misinterpret fields.
package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// SchemaVersion is bumped when SpawnFresh's on-disk shape changes
// incompatibly. Readers compare and refuse newer versions.
//
// v2 added DisableAutoResume (the per-handoff auto-resume override).
// An older fleet binary reading a v2 file would ignore the new field
// and drain the handoff with the wrong policy — bumping the version
// makes that case a clean refuse-to-read instead. Codex review
// iter-12 P2.
const SchemaVersion = 2

// SpawnFreshPrefix is the queue filename prefix for "spawn a
// replacement agent" requests. Producers use SpawnFreshName(id) to
// compute the full filename; consumers filter ListPending output by
// this prefix.
const SpawnFreshPrefix = "spawn-fresh-"

// SpawnFresh is the request a producer writes to declare "agent X
// just handed off, please spawn its replacement using the doc at
// HandoffDoc." Read by the consumer in cmd/fleet/handoff.go (4a) and
// by the TUI background drain (4b+).
//
// Crash-recovery contract: NewAgentID and NewSession are populated
// AFTER spawn.Spawn succeeds, by re-writing the queue file. A retry
// that finds an existing queue file with NewAgentID set knows the
// replacement already exists and must NOT spawn a second one — it
// should complete the in-flight handoff (kill old, archive, delete
// queue) instead.
type SpawnFresh struct {
	SchemaVersion int    `json:"schema_version"`
	OldAgentID    string `json:"old_agent_id"`
	// HandoffDoc is the absolute path to the doc the new agent will
	// be told to read on startup (via FLEET_EXTRA_CLAUDE_MD).
	HandoffDoc string `json:"handoff_doc"`
	Project    string `json:"project"`
	TaskID     string `json:"task_id"`
	// NewAgentID and NewSession are empty before spawn, populated
	// after. Their presence in a re-read queue file is the signal
	// "replacement already exists; do not re-spawn."
	NewAgentID string `json:"new_agent_id,omitempty"`
	NewSession string `json:"new_session,omitempty"`
	// DisableAutoResume captures the per-handoff auto-resume override
	// chosen by the operator (e.g. `fleet handoff --no-auto-resume`
	// or `--auto-resume`). Pointer semantics so we can distinguish:
	//   - nil → no override; recovery should inherit from the
	//     outgoing record's DisableAutoResume.
	//   - &true → operator explicitly disabled auto-resume for this
	//     handoff regardless of the outgoing record's policy.
	//   - &false → operator explicitly enabled auto-resume for this
	//     handoff regardless of the outgoing record's policy.
	// Persisting it here ensures that a crashed handoff resumed by
	// `fleet drain` / TUI watcher honors the same policy
	// (codex review iter-10 / iter-11 P2).
	DisableAutoResume *bool `json:"disable_auto_resume,omitempty"`
	// CapApproved is true when the queue file's producer already
	// passed the FLEET_MAX_SESSIONS gate before writing this file.
	// `fleet handoff` sets it to true (the operator-triggered path
	// checks the cap at step 4a before writing the queue).
	// fleet-guard auto-handoff leaves it false (the producer is the
	// queue file itself; the consumer in handoffop.spawnAndRetire
	// must check the cap before spawning).
	//
	// Recovery semantic: when `fleet drain` / TUI watcher / `fleet
	// handoff` retries a stale queue file, CapApproved=true bypasses
	// the consumer's cap check — the handoff was already authorized.
	// Without this, a tightened cap (operator lowered
	// FLEET_MAX_SESSIONS after a crash, leak accumulated) could
	// permanently strand authorized queue files. Conversely,
	// CapApproved=false on an auto-handoff queue keeps the
	// spawnAndRetire cap firing so the auto-drainer can't blow
	// past the ceiling.
	CapApproved bool      `json:"cap_approved,omitempty"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
}

// SpawnFreshName returns the queue filename (without .json) for
// spawning a replacement of agent oldID. One queue file per agent;
// re-enqueuing for the same agent intentionally clobbers the prior
// request via state.WriteAtomic.
func SpawnFreshName(oldID string) string {
	return SpawnFreshPrefix + oldID
}

// WriteSpawnFresh atomically publishes req to its canonical path.
// Returns the absolute path written so callers can pass it on to a
// drain step or log it.
func WriteSpawnFresh(req SpawnFresh) (string, error) {
	if req.OldAgentID == "" {
		return "", fmt.Errorf("queue.WriteSpawnFresh: OldAgentID required")
	}
	if req.HandoffDoc == "" {
		return "", fmt.Errorf("queue.WriteSpawnFresh: HandoffDoc required")
	}
	if req.SchemaVersion == 0 {
		req.SchemaVersion = SchemaVersion
	}
	if req.EnqueuedAt.IsZero() {
		req.EnqueuedAt = time.Now().UTC()
	}
	path, err := state.QueuePath(SpawnFreshName(req.OldAgentID))
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal spawn-fresh: %w", err)
	}
	data = append(data, '\n')
	if err := state.WriteAtomic(path, data); err != nil {
		return "", err
	}
	return path, nil
}

// ReadSpawnFresh parses a queue file at path. Refuses unknown future
// schema versions — if a 4b-era fleet writes schema_version=2 and an
// older binary tries to drain, returning an error here is safer than
// guessing.
func ReadSpawnFresh(path string) (SpawnFresh, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SpawnFresh{}, fmt.Errorf("read %s: %w", path, state.ErrNotFound)
		}
		return SpawnFresh{}, fmt.Errorf("read %s: %w", path, err)
	}
	var req SpawnFresh
	if err := json.Unmarshal(data, &req); err != nil {
		return SpawnFresh{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if req.SchemaVersion > SchemaVersion {
		return SpawnFresh{}, fmt.Errorf("queue file %s schema_version=%d newer than supported %d (upgrade fleet)",
			path, req.SchemaVersion, SchemaVersion)
	}
	return req, nil
}

// ListPending returns absolute paths of every spawn-fresh-*.json
// file currently in ~/.fleet/queue/. Order is filesystem-dependent;
// callers should not assume FIFO. The drain loop holds a per-project
// flock so out-of-order processing across projects is safe.
func ListPending() ([]string, error) {
	dir, err := state.QueueDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("readdir queue: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, SpawnFreshPrefix) {
			continue
		}
		if filepath.Ext(name) != ".json" {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}

// Delete removes a queue file after a successful drain.
//
// This is the consumer's commit point — the work is durable on disk
// (handoff doc written, replacement spawned, old agent archived)
// before the queue file goes away. If the consumer crashes between
// "work done" and "Delete," the next drain will see a still-present
// queue file pointing at an old_agent_id that's already archived,
// and skip it (the drainer checks agent existence before re-running).
func Delete(path string) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("delete queue file %s: %w", path, err)
	}
	return nil
}

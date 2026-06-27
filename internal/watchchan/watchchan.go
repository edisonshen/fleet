// Package watchchan is the append-safe, crash-safe message channel that
// lets many read-only *watchers* report events to the single *coord* (the
// dispatch controller) without racing. It is the foundation slice of
// DESIGN-coord-supervisor-in-daemon: the substrate Slices 2-4 build on.
//
// Why not internal/queue? The queue writes one file per AGENT and
// re-enqueuing CLOBBERS the prior message (state.WriteAtomic overwrites a
// stable name). That is correct for its per-agent spawn-fresh use, but it
// cannot fan IN N watchers — a second watcher's report would erase the
// first. This channel instead writes one file per MESSAGE under a
// per-project directory, so N producers append concurrently and never
// clobber each other.
//
// Two message disciplines, because events and heartbeats have opposite
// needs:
//
//	EVENTS (worker_delta, pr_event)   — unique-name append, never clobbered.
//	  filename: <unix_nanos>-<watcher_id>-<seq>.json
//	  uniqueness triple: (unix_nanos, watcher_id, seq)
//	    - watcher_id embeds the pid, so two PROCESSES never share one
//	      (a per-process seq alone can't stop a cross-process clobber).
//	    - seq is a per-process atomic counter breaking same-nanos ties
//	      WITHIN a process.
//	  drained by the Python reader (watchchan.py) apply-before-delete.
//
//	HEARTBEATS (liveness_heartbeat) — keyed by WORKER, overwrite-in-place.
//	  filename: hb-<worker_id>.json  (NO pid — worker-keyed)
//	  A latest-state file, atomically overwritten each probe. Because the
//	  name depends only on the worker, a watcher restart (new pid)
//	  overwrites the SAME file in place rather than orphaning a stale one,
//	  so heartbeats are structurally bounded at <= N_workers regardless of
//	  watcher churn. They are NEVER drained/deleted as events and are
//	  CAP-EXEMPT (dropping a heartbeat under cap pressure would skew the
//	  stuck-timer Slice 3 derives from it).
//
// Both writers reuse state.WriteAtomic (tmp+fsync+rename+parent-fsync); no
// lock is taken in this layer — filename uniqueness (events) and stable-name
// last-rename-wins overwrite (heartbeats) make them multi-producer-safe.
package watchchan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// SchemaVersion is the wire version shared by the Go writer and the Python
// reader (skills/coordinator/watchchan.py SCHEMA_VERSION). The reader
// refuses a message whose `v` is newer than it understands rather than
// mis-parsing — mirroring internal/queue.ReadSpawnFresh's refuse-newer
// discipline.
const SchemaVersion = 1

// DirName is the per-project subdirectory holding the channel:
// ~/.fleet/projects/<project>/watch-msgs/.
const DirName = "watch-msgs"

// HeartbeatPrefix names the worker-keyed latest-state heartbeat files. The
// event drain skips any file with this prefix; the cap never counts or
// drops them.
const HeartbeatPrefix = "hb-"

// Message kinds.
const (
	KindWorkerDelta = "worker_delta"       // an actionable worker-liveness change
	KindPREvent     = "pr_event"           // a PR/CI state move (carries SHAs)
	KindHeartbeat   = "liveness_heartbeat" // a per-epoch liveness probe (heartbeat file)
)

// DefaultCap bounds the number of EVENT messages on disk. When exceeded the
// writer drops the oldest events (logging each) so an undrained channel
// (coord dead) cannot grow without bound. Heartbeats are NOT in this cap.
const DefaultCap = 10000

// Message is the self-describing wire shape, identical on both sides.
//
// `seq` is intentionally NOT a field here: it is a filename-only
// disambiguator for events, not part of the delivered payload.
type Message struct {
	V         int            `json:"v"`          // schema version (refuse-newer on read)
	Kind      string         `json:"kind"`       // one of the Kind* constants
	WatcherID string         `json:"watcher_id"` // process-unique (embeds pid) — see WatcherID
	EpochID   int64          `json:"epoch_id"`   // watcher's per-pass probe counter (NOT a dispatch_generation)
	Key       string         `json:"key"`        // delivery key; PR events include the head SHA
	Payload   map[string]any `json:"payload,omitempty"`
	TS        string         `json:"ts"` // UTC-ISO human/audit field (filename nanos is the ordering key)
}

// nowNanos is the clock seam (overridable in tests to force same-nanosecond
// appends). Production reads the monotonic-free wall clock in nanoseconds.
var nowNanos = func() int64 { return time.Now().UnixNano() }

// seqCounter is the per-process monotonic sequence. atomic so concurrent
// AppendEvent calls in one process each get a distinct value, breaking
// same-nanosecond ties. Combined with the pid-bearing watcher_id this makes
// the filename unique across ALL producers.
var seqCounter atomic.Uint64

// WatcherID returns a process-unique watcher id by appending this process's
// pid to base (e.g. "<agent_id>" -> "<agent_id>-<pid>"). The pid is
// load-bearing: it is what keeps two watcher PROCESSES from emitting the
// same event filename at the same nanosecond+seq. It is the EVENT-filename
// component only; heartbeats are keyed by worker, so a watcher restart never
// churns heartbeat files.
func WatcherID(base string) string {
	return base + "-" + strconv.Itoa(os.Getpid())
}

// Channel is the per-project message channel rooted at
// ~/.fleet/projects/<project>/watch-msgs/.
type Channel struct {
	dir  string                           // the watch-msgs directory
	cap  int                              // event-message cap (0 = unbounded)
	logf func(format string, args ...any) // drop-logging sink (never silent)
}

// New builds a Channel under projectDir (typically the result of
// state.ProjectDir(project)). The watch-msgs directory is created lazily on
// first write.
func New(projectDir string) *Channel {
	return &Channel{
		dir:  filepath.Join(projectDir, DirName),
		cap:  DefaultCap,
		logf: func(format string, args ...any) { fmt.Fprintf(os.Stderr, "watchchan: "+format+"\n", args...) },
	}
}

// Dir returns the absolute watch-msgs directory path.
func (c *Channel) Dir() string { return c.dir }

// eventFilename builds the unique event filename. unix_nanos is zero-padded
// to 19 digits so lexicographic sort == time order (int64 nanos stays 19
// digits until year 2262), and the (unix_nanos, watcher_id, seq) triple is
// what guarantees no clobber across producers.
func eventFilename(unixNanos int64, watcherID string, seq uint64) string {
	return fmt.Sprintf("%019d-%s-%d.json", unixNanos, watcherID, seq)
}

// AppendEvent publishes msg as a new unique-name event file and returns its
// absolute path. msg.WatcherID must be process-unique (use WatcherID). The
// channel fills v/ts and assigns the filename nanos+seq. After writing it
// enforces the event cap (heartbeats unaffected).
func (c *Channel) AppendEvent(msg Message) (string, error) {
	if msg.WatcherID == "" {
		return "", fmt.Errorf("watchchan: AppendEvent requires WatcherID")
	}
	if msg.Kind == KindHeartbeat {
		return "", fmt.Errorf("watchchan: heartbeats use WriteHeartbeat, not AppendEvent")
	}
	if msg.Kind == "" {
		return "", fmt.Errorf("watchchan: AppendEvent requires Kind")
	}
	stamp(&msg)
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return "", fmt.Errorf("watchchan: mkdir %s: %w", c.dir, err)
	}
	data, err := marshal(msg)
	if err != nil {
		return "", err
	}
	seq := seqCounter.Add(1)
	path := filepath.Join(c.dir, eventFilename(nowNanos(), msg.WatcherID, seq))
	if err := state.WriteAtomic(path, data); err != nil {
		return "", err
	}
	c.enforceCap()
	return path, nil
}

// WriteHeartbeat publishes a worker-keyed latest-state heartbeat at
// hb-<workerID>.json, atomically overwriting any prior heartbeat for that
// worker. Heartbeats are never drained as events and never counted against
// the cap.
//
// Concurrency model: safe across PROCESSES (the real case — old+standby
// watchers overlapping during a handoff). WriteAtomic's temp file is
// pid-scoped, so two processes use distinct temps and the atomic rename
// gives last-rename-wins, which is benign for a liveness signal (either
// proves the worker live; the latest epoch wins). It is NOT a guarantee for
// two goroutines in ONE process writing the SAME worker's heartbeat
// concurrently — they would share the pid-scoped temp. That never happens by
// construction: a watcher probes each worker sequentially within a pass, and
// distinct workers have distinct filenames.
func (c *Channel) WriteHeartbeat(workerID string, msg Message) (string, error) {
	if workerID == "" {
		return "", fmt.Errorf("watchchan: WriteHeartbeat requires workerID")
	}
	msg.Kind = KindHeartbeat
	stamp(&msg)
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return "", fmt.Errorf("watchchan: mkdir %s: %w", c.dir, err)
	}
	data, err := marshal(msg)
	if err != nil {
		return "", err
	}
	path := filepath.Join(c.dir, heartbeatName(workerID))
	if err := state.WriteAtomic(path, data); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveHeartbeat reaps the heartbeat for a worker that has terminated. The
// live watcher calls this on observing the worker gone (no watcher-restart
// orphan exists — the stable name overwrites in place). Missing file is a
// no-op.
func (c *Channel) RemoveHeartbeat(workerID string) error {
	path := filepath.Join(c.dir, heartbeatName(workerID))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("watchchan: remove heartbeat %s: %w", path, err)
	}
	return nil
}

// ReapOrphanHeartbeats is the gc backstop: it removes any hb-<worker>.json
// whose worker is not in live. Keyed on worker-existence — the only reap
// trigger needed, since the stable name means no watcher-restart orphan can
// occur. Returns the paths reaped.
func (c *Channel) ReapOrphanHeartbeats(live map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("watchchan: readdir %s: %w", c.dir, err)
	}
	var reaped []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		worker, ok := workerFromHeartbeat(e.Name())
		if !ok {
			continue
		}
		if live[worker] {
			continue
		}
		p := filepath.Join(c.dir, e.Name())
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return reaped, fmt.Errorf("watchchan: reap %s: %w", p, err)
		}
		reaped = append(reaped, p)
	}
	return reaped, nil
}

// enforceCap drops the oldest EVENT messages once the event count exceeds
// c.cap, logging each drop (never silent). Heartbeats and .tmp sidecars are
// not counted or dropped.
func (c *Channel) enforceCap() {
	if c.cap <= 0 {
		return
	}
	events := c.listEventNames()
	if len(events) <= c.cap {
		return
	}
	// names sort lexicographically == oldest-first (nanos-prefixed).
	drop := events[:len(events)-c.cap]
	for _, name := range drop {
		p := filepath.Join(c.dir, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			c.logf("cap %d exceeded but failed to drop %s: %v", c.cap, name, err)
			continue
		}
		c.logf("cap %d exceeded, dropped oldest event %s", c.cap, name)
	}
}

// listEventNames returns event filenames (sorted oldest-first), excluding
// heartbeats and .tmp sidecars.
func (c *Channel) listEventNames() []string {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if isEventFile(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// isEventFile reports whether name is a drainable event message (a .json
// file that is not a heartbeat and not a .tmp sidecar).
func isEventFile(name string) bool {
	if strings.HasPrefix(name, HeartbeatPrefix) {
		return false
	}
	if filepath.Ext(name) != ".json" {
		return false // .tmp.<pid> sidecars end in the pid, not .json
	}
	return true
}

func heartbeatName(workerID string) string { return HeartbeatPrefix + workerID + ".json" }

// workerFromHeartbeat extracts the worker id from hb-<worker>.json, or
// (\"\", false) if name is not a heartbeat file.
func workerFromHeartbeat(name string) (string, bool) {
	if !strings.HasPrefix(name, HeartbeatPrefix) || !strings.HasSuffix(name, ".json") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(name, HeartbeatPrefix), ".json"), true
}

// stamp fills the wire fields the channel owns: the schema version (always
// the current one — the writer never emits an old shape) and a default
// UTC-ISO ts when the caller left it empty.
func stamp(msg *Message) {
	msg.V = SchemaVersion
	if msg.TS == "" {
		msg.TS = time.Now().UTC().Format(time.RFC3339)
	}
}

func marshal(msg Message) ([]byte, error) {
	data, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("watchchan: marshal: %w", err)
	}
	return append(data, '\n'), nil
}

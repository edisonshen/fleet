// Package fleetlog is Fleet's local debug-log emitter (agent/LLM-consumed).
//
// Every Fleet process appends a structured JSONL event stream to ITS OWN
// file under ~/.fleet/logs/ (or $XDG_STATE_HOME/fleet/logs). A successor
// agent reads the raw JSONL with jq/grep to reconstruct what happened.
// Files older than 3 days are pruned. See docs/DESIGN-fleet-debug-logs.md.
//
// Design invariants this package upholds:
//
//   - Per-process files, never a shared one. The filename carries the pid
//     AND a process-start fingerprint (pid_start) so same-day PID reuse can
//     never collide. No flock, no rotation library, no write contention.
//     Any merge across files happens only at READ time (jq/sort).
//
//     coord  -> fleet-DATE-coord-PID-START.jsonl   -+
//     worker -> fleet-DATE-worker-PID-START.jsonl  -+-> read: jq/grep/sort by ts
//     cli    -> fleet-DATE-cli-PID-START.jsonl     -+
//
//   - One self-describing JSON object per line (JSONL). Single O_APPEND
//     write of pre-marshaled bytes — no buffered writer, so a record is one
//     syscall and a truncated tail never corrupts the rest.
//
//   - Best-effort / fire-and-forget. Log NEVER returns an error and NEVER
//     panics; a logging failure must never fail a tick or a command.
package fleetlog

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/edisonshen/fleet/internal/state"
)

// Component names that appear in both the filename and the `comp` field.
const (
	CompCoord  = "coord"
	CompWorker = "worker"
	CompCLI    = "cli"
)

// Types is the closed event vocabulary. The reader's primary filter; a
// successor LLM pattern-matches on these without parsing prose. Logging
// does NOT reject an unknown type (best-effort), but callers should stay
// within this set and tests assert membership.
var Types = map[string]bool{
	"coord.start": true, "coord.handoff": true, "coord.resume": true,
	"coord.tick": true, "decision": true, "dispatch.worker": true,
	"worker.start": true, "tool.call": true, "tool.result": true,
	"model.call": true, "state.transition": true, "pr.opened": true,
	"pr.status": true, "task.completed": true, "worker.failed": true,
	"cli.start": true, "cli.finish": true, "error": true, "cleanup": true,
}

// dataCap bounds any single string value in `data` (a size bound, not a
// secret control — values are logged raw by design). Larger strings are
// truncated with an ASCII elision marker.
const dataCap = 2048

const elision = "...[truncated]"

// Fields carries the optional/variable parts of an event envelope. Zero
// values are omitted from the line.
type Fields struct {
	Proj       string
	Agent      string
	Gen        int
	Session    string
	DispatchID string
	Slug       string
	PR         int
	CausedBy   string
	Msg        string
	Data       map[string]any
}

// envelope is the on-disk line shape. Field order here IS the JSON key
// order; the Python emitter mirrors it. omitempty keeps absent
// correlation keys off the line so equivalent events share a key set.
type envelope struct {
	TS         string         `json:"ts"`
	Seq        int64          `json:"seq"`
	Type       string         `json:"type"`
	Lvl        string         `json:"lvl"`
	Comp       string         `json:"comp"`
	PID        int            `json:"pid"`
	Proj       string         `json:"proj,omitempty"`
	Agent      string         `json:"agent,omitempty"`
	Gen        int            `json:"gen,omitempty"`
	Session    string         `json:"session,omitempty"`
	DispatchID string         `json:"dispatch_id,omitempty"`
	Slug       string         `json:"slug,omitempty"`
	PR         int            `json:"pr,omitempty"`
	CausedBy   string         `json:"caused_by,omitempty"`
	ID         string         `json:"id"`
	Msg        string         `json:"msg,omitempty"`
	Data       map[string]any `json:"data,omitempty"`
}

var (
	seqCounter atomic.Int64
	procToken  = newProcToken()
	// pidStart fingerprints THIS process so a reused PID (later process,
	// same pid) writes a distinct filename. Captured once at package
	// init: the kernel's real start time is unix-only, but for filename
	// collision-avoidance a process-init nanosecond stamp is equivalent
	// and portable across every GOOS the fleet binary builds for.
	pidStart = time.Now().UnixNano()
	// nowFn is the injectable clock (date rollover / prune cutoff tests).
	nowFn = time.Now
)

func newProcToken() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Best-effort: fall back to a time-derived token. Never fatal.
		return strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 16)
	}
	return hex.EncodeToString(b[:])
}

// Dir returns the directory logs are written to. Honors $XDG_STATE_HOME
// (the XDG-designated home for logs) -> $XDG_STATE_HOME/fleet/logs; else
// state.Root()/logs (which honors FLEET_HOME). It deliberately does NOT
// extend state.Root() to read XDG — that would relocate the WHOLE state
// tree (agents/, queue/, ...), but the design only wants logs under XDG.
func Dir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "fleet", "logs")
	}
	root, err := state.Root()
	if err != nil {
		// Last-resort: write under the OS temp dir rather than a relative
		// "logs" path. A relative path would land inside whatever the
		// process's cwd happens to be (e.g. a git repo), dirtying the
		// working tree with JSONL files. os.TempDir() is always absolute
		// and survives even a missing $HOME.
		return filepath.Join(os.TempDir(), "fleet-logs")
	}
	return filepath.Join(root, "logs")
}

func fileName(comp string, now time.Time) string {
	return "fleet-" + now.UTC().Format("2006-01-02") + "-" + comp + "-" +
		strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(pidStart, 10) + ".jsonl"
}

// Log appends one event for component comp (coord/worker/cli), event type
// evt, severity lvl, plus the variable Fields. Best-effort: every error is
// swallowed. Returns the generated event id so a caller can thread it into
// a later event's CausedBy (e.g. cli.start -> cli.finish); the returned id
// is valid even when the write itself failed.
func Log(comp, evt, lvl string, f Fields) string {
	now := nowFn()
	seq := seqCounter.Add(1)
	id := procToken + "-" + strconv.FormatInt(seq, 10)
	if lvl == "" {
		lvl = "info"
	}
	env := envelope{
		TS:         now.UTC().Format("2006-01-02T15:04:05.000000Z07:00"),
		Seq:        seq,
		Type:       evt,
		Lvl:        lvl,
		Comp:       comp,
		PID:        os.Getpid(),
		Proj:       f.Proj,
		Agent:      f.Agent,
		Gen:        f.Gen,
		Session:    f.Session,
		DispatchID: f.DispatchID,
		Slug:       f.Slug,
		PR:         f.PR,
		CausedBy:   f.CausedBy,
		ID:         id,
		Msg:        f.Msg,
		Data:       capData(f.Data),
	}
	line, err := marshalLine(env)
	if err != nil {
		return id // swallow: a non-marshalable payload must not break the caller
	}
	dir := Dir()
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, fileName(comp, now))
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return id // swallow: unwritable dir, disk full, etc.
	}
	// Single Write of the whole pre-marshaled line (newline included).
	// O_APPEND + one syscall is the per-process atomicity the design
	// leans on; no buffered writer that could split a record.
	_, _ = fd.Write(line)
	_ = fd.Close()
	return id
}

// marshalLine renders env to a compact JSON line + '\n'. HTML escaping is
// disabled so output aligns with the Python emitter (json.dumps does not
// HTML-escape) and stderr/url text (e.g. "x>y", "a&b") is logged raw per
// the design. encoder.Encode already appends the trailing newline.
func marshalLine(env envelope) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func capData(d map[string]any) map[string]any {
	if len(d) == 0 {
		return nil
	}
	out := make(map[string]any, len(d))
	for k, v := range d {
		switch s := v.(type) {
		case string:
			if len(s) > dataCap {
				out[k] = truncateUTF8(s, dataCap) + elision
			} else {
				out[k] = s
			}
		default:
			// Non-string values (slices, maps, ints…): marshal to JSON to
			// bound the wire size. If the representation is small, keep the
			// original typed value (better for consumers). If it's large, store
			// a human-readable size hint so the cap is visible in the log line.
			b, err := json.Marshal(v)
			if err == nil && len(b) > dataCap {
				out[k] = fmt.Sprintf("<capped: %d bytes>", len(b))
			} else {
				out[k] = v
			}
		}
	}
	return out
}

// truncateUTF8 returns s[:maxBytes] truncated to the last valid UTF-8
// rune boundary so the result is always valid UTF-8. maxBytes must be > 0.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut]
}

var fileRe = regexp.MustCompile(`^fleet-(\d{4}-\d{2}-\d{2})-.+\.jsonl$`)

// pruneMu serializes a prune against concurrent prunes in the same
// process (cheap; cross-process is naturally safe because each Remove is
// idempotent and a missing file is not an error).
var pruneMu sync.Mutex

// PruneOlderThan deletes logs/fleet-*.jsonl whose filename date is older
// than maxAge (e.g. 72h => keep today + the prior 3 calendar days).
// Best-effort: a missing dir or an unremovable file is ignored.
func PruneOlderThan(maxAge time.Duration) {
	pruneMu.Lock()
	defer pruneMu.Unlock()
	dir := Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// Compare at day granularity so "older than 3 days" means strictly
	// before (today_midnight - maxAge); a file dated exactly maxAge ago
	// is kept.
	cutoff := nowFn().UTC().Truncate(24 * time.Hour).Add(-maxAge)
	for _, e := range entries {
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		d, err := time.Parse("2006-01-02", m[1])
		if err != nil {
			continue
		}
		if d.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

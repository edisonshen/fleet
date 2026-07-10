//go:build linux || darwin

package coordlock

// rpo.go — the lease-gated mutation BOUNDARY + durable RPO machinery
// (DESIGN-handoff-drain-storm-leak PR4 of 6). This is the activation PR:
// it adds the one place every side effect must pass through to be FENCED,
// plus the intent/completion log + atomic-checkpoint machinery that bounds
// lost work on a crash.
//
// Two guarantees live here:
//
//  1. FENCING AT A BOUNDARY (split-brain guard). Every externally-visible
//     side effect routes through GuardWithLease(tok, ...). The boundary
//     re-reads the live epoch under coordinator.epoch.lock and admits the
//     action ONLY IF state==active AND tok.Epoch==current AND owner==tok's
//     full identity. A woken zombie (stale epoch / no longer owner) is
//     REJECTED with ErrLeaseLost before it can touch anything. Discipline
//     ("remember to tag your write") is not a guarantee — the boundary is.
//
//  2. BOUNDED RPO (lost-work guard). GuardWithLease writes an epoch-tagged,
//     fsynced INTENT record before it runs the action and a COMPLETION
//     record after. Each action carries a stable idempotency key, so a
//     crash between intent and completion is recovered by ReplayIntents,
//     which re-drives the action through its registered handler — a no-op
//     if it already finished, never a duplicate. Checkpoints are written
//     atomically (.tmp->fsync->rename) with a {len, CRC32C, epoch} trailer;
//     ReadCheckpoint QUARANTINES a torn (bad len/CRC) or future-epoch
//     (zombie-written) checkpoint and falls back to the last clean one.
//
//	side effect ─▶ GuardWithLease(tok, key, fn)
//	                 ├─ StillOwned()?  no ─▶ ErrLeaseLost (REJECT, no intent)
//	                 ├─ write intent  (epoch+key, fsync)
//	                 ├─ fn()          (the actual mutation)
//	                 └─ write completion (epoch+key, fsync)
//
//	cold rebuild ─▶ ReadCheckpoint  ─▶ torn/future-epoch? QUARANTINE
//	            ─▶ ReplayIntents(tok) ─▶ intent w/o completion ─▶ handler(key)

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ErrLeaseLost is the typed rejection the boundary returns when the
// supplied token no longer owns the active lease (stale epoch, fenced, or
// non-owner). Callers branch on errors.Is(err, ErrLeaseLost) to
// self-demote (exit) before any destructive action — Patroni "only act if
// I still own the lease."
var ErrLeaseLost = errors.New("coordlock: lease lost (stale epoch or non-owner) — self-demote")

// crc32cTable is the Castagnoli (CRC32C) polynomial table — the same
// checksum Postgres/iSCSI use for on-disk integrity. Computed once.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// --- the fencing boundary ---

// GuardWithLease is THE central lease-gated mutation boundary. Every side
// effect (state write, spawn, dispatch, worktree mutation, PR action) runs
// through it so fencing is enforced in ONE place, not by per-call-site
// discipline (one missed tag reopens split-brain).
//
// Order, per the design's lost-work guarantee:
//
//  1. tok.StillOwned()  — re-read the live epoch; reject a stale/non-owner
//     token with ErrLeaseLost BEFORE any side effect.
//  2. write INTENT      — epoch-tagged, fsynced, keyed by idempotencyKey.
//  3. fn()              — the actual mutation.
//  4. write COMPLETION  — epoch-tagged, fsynced, same key.
//
// idempotencyKey is a stable identifier for the action (dispatch ID /
// branch name / PR node-id) so a crash between (2) and (4) is recovered by
// ReplayIntents idempotently. fn's error is returned as-is (after a
// best-effort completion-skip — a failed action writes NO completion, so it
// is replayed). An ErrLeaseLost return means the caller must self-demote.
func GuardWithLease(tok LeaseToken, idempotencyKey string, fn func() error) error {
	if strings.TrimSpace(idempotencyKey) == "" {
		return fmt.Errorf("coordlock.GuardWithLease: idempotencyKey must not be empty")
	}
	// Step 1: FENCE. Re-read the live epoch and reject a zombie at the
	// boundary before it mutates anything.
	if !tok.StillOwned() {
		return ErrLeaseLost
	}
	store, err := tok.intentStore()
	if err != nil {
		return fmt.Errorf("coordlock.GuardWithLease: resolve intent store: %w", err)
	}
	// Step 2: INTENT before the act (the lost-work guarantee). Fsynced so a
	// crash immediately after fn() begins still leaves a replayable record.
	if werr := store.writeIntent(idempotencyKey, tok.Epoch); werr != nil {
		return fmt.Errorf("coordlock.GuardWithLease: write intent %q: %w", idempotencyKey, werr)
	}
	// Step 2.5: RE-FENCE immediately before the side effect (codex PR4 [P1]).
	// The intent write + its fsync take real wall-clock time; a candidate
	// could fence this leader DURING that window. Re-reading the live epoch
	// here narrows the check-to-act gap to the minimum — we cannot hold a
	// lock across fn() (the design forbids any lock across a side effect /
	// tmux / network), so a double-check (before intent + immediately before
	// the act) is the tightest fence possible. The orphaned intent we just
	// wrote is harmless: it has no completion, so a real successor's replay
	// would re-drive it idempotently (and an idempotency key makes that a
	// no-op if the action never ran).
	if !tok.StillOwned() {
		return ErrLeaseLost
	}
	// Step 3: the actual mutation. A failed action writes NO completion, so
	// it is replayed on rebuild (idempotently).
	if ferr := fn(); ferr != nil {
		return ferr
	}
	// Step 3.5: RE-FENCE before the completion (codex PR4 [P1]). fn() can run
	// long enough for a candidate to fence us. A fenced owner must NOT write
	// the completion marker: a stale .done.json would make the new leader's
	// ReplayIntents SKIP this intent (treating it finished) even though it was
	// produced by a non-owner. We return ErrLeaseLost instead — the action
	// already ran, but it carries an idempotency key, so the successor's
	// replay re-drives it as a no-op (never a duplicate). Matches the
	// per-action fence ReplayIntents already applies before its completion.
	if !tok.StillOwned() {
		return ErrLeaseLost
	}
	// Step 4: COMPLETION after the act. A write fault here is surfaced but
	// is benign for correctness — replay re-drives idempotently.
	if cerr := store.writeCompletion(idempotencyKey, tok.Epoch); cerr != nil {
		return fmt.Errorf("coordlock.GuardWithLease: action %q done but completion write failed "+
			"(will replay idempotently): %w", idempotencyKey, cerr)
	}
	return nil
}

// --- intent / completion log ---

// intentRecord is one row of the durable intent/completion log. The
// completion is a sibling file, not a flag, so a torn intent and a torn
// completion are independently detectable.
type intentRecord struct {
	Key       string `json:"key"`
	Epoch     int64  `json:"epoch"`
	StampWall int64  `json:"stamp_wall"`
}

// intentStore is the on-disk intent/completion log rooted at
// <project>/coord/intents/. intent files are <key>.intent.json; completion
// files are <key>.done.json. Both are atomic-written (.tmp->fsync->rename).
type intentStore struct {
	dir string
}

func (t LeaseToken) intentStore() (intentStore, error) {
	// The intent log lives beside the lease files (.locks/) under a
	// dedicated intents/ subdir so a torn intent can be quarantined without
	// touching the lease state.
	dir := filepath.Join(filepath.Dir(t.paths.flock), "intents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return intentStore{}, err
	}
	return intentStore{dir: dir}, nil
}

// keyFile sanitizes an idempotency key into a filesystem-safe basename. A
// key can be a branch name or PR node-id with slashes; we encode them so
// one key maps to exactly one file pair without escaping the intents dir.
func keyFile(key, suffix string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	// Append a short CRC of the raw key so two distinct keys that sanitize
	// to the same basename (e.g. "a/b" and "a_b") never collide.
	sum := crc32.Checksum([]byte(key), crc32cTable)
	return fmt.Sprintf("%s-%08x%s", b.String(), sum, suffix)
}

func (s intentStore) intentPath(key string) string {
	return filepath.Join(s.dir, keyFile(key, ".intent.json"))
}

func (s intentStore) completionPath(key string) string {
	return filepath.Join(s.dir, keyFile(key, ".done.json"))
}

func (s intentStore) writeIntent(key string, epoch int64) error {
	return writeJSONAtomicFsync(s.intentPath(key), intentRecord{
		Key: key, Epoch: epoch, StampWall: time.Now().UnixNano(),
	})
}

func (s intentStore) writeCompletion(key string, epoch int64) error {
	return writeJSONAtomicFsync(s.completionPath(key), intentRecord{
		Key: key, Epoch: epoch, StampWall: time.Now().UnixNano(),
	})
}

// readCompletion reads a VALID completion record's EPOCH for key.
//
//	epoch>=0, err=nil          -> a parseable completion (key match); its epoch
//	epoch=0,  err=os.ErrNotExist -> no completion file (pending)
//	epoch=0,  err=other        -> a present-but-corrupt/unreadable completion
//
// Callers (pendingIntents) compare the returned epoch to the intent's epoch:
// a completion suppresses replay only when it covers this-or-a-later
// generation of the action (codex PR4 [P2]); a corrupt completion is NOT
// trusted and the intent is replayed idempotently rather than skipped.
func (s intentStore) readCompletion(key string) (epoch int64, err error) {
	b, rerr := os.ReadFile(s.completionPath(key))
	if rerr != nil {
		return 0, rerr // includes os.ErrNotExist (pending) + real read faults
	}
	var rec intentRecord
	if uerr := json.Unmarshal(b, &rec); uerr != nil {
		return 0, fmt.Errorf("corrupt completion: %w", uerr)
	}
	if rec.Key != key {
		return 0, fmt.Errorf("completion key mismatch: got %q want %q", rec.Key, key)
	}
	return rec.Epoch, nil
}

// pendingIntents returns the keys of intents that have NO completion — the
// in-flight actions a crash left mid-flight. Sorted for deterministic
// replay order. A torn (unparseable) intent file is reported via the second
// return so the caller can enter degraded recovery (fleet doctor, PR6)
// rather than silently dropping work.
func (s intentStore) pendingIntents() (keys []string, torn []string, err error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	type pend struct {
		key   string
		stamp int64
	}
	var pendList []pend
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".intent.json") {
			continue
		}
		full := filepath.Join(s.dir, name)
		b, rerr := os.ReadFile(full)
		if rerr != nil {
			torn = append(torn, full)
			continue
		}
		var rec intentRecord
		if json.Unmarshal(b, &rec) != nil || rec.Key == "" {
			torn = append(torn, full)
			continue
		}
		// Completion present, VALID, and for THIS intent generation? skip.
		// Validate, don't just os.Stat (codex PR4 [P2]): a torn completion
		// (corrupt bytes, or a writeCompletion that renamed but lost the
		// dir-fsync on a crash) must NOT suppress replay. ALSO compare epochs
		// (codex PR4 [P2]): an idempotency key can be REUSED in a later lease
		// epoch (e.g. a branch-name keyed action). A STALE completion from a
		// PRIOR epoch (compEpoch < intent epoch) must not suppress the NEW
		// pending intent — that would drop a real in-flight action. Suppress
		// only when the completion's epoch is >= the intent's epoch (it covers
		// this generation of the action).
		switch compEpoch, derr := s.readCompletion(rec.Key); {
		case derr == nil && compEpoch >= rec.Epoch:
			continue // clean completion for this-or-later generation -> finished
		case derr == nil && compEpoch < rec.Epoch:
			// Stale completion from an earlier epoch -> the key was reused;
			// the new intent is genuinely pending.
			fmt.Fprintf(os.Stderr,
				"coordlock: stale completion for intent %q (done@%d < intent@%d); will replay\n",
				rec.Key, compEpoch, rec.Epoch)
		case errors.Is(derr, os.ErrNotExist):
			// No completion -> pending (normal case).
		default:
			// Unreadable/corrupt completion -> don't trust it; replay.
			fmt.Fprintf(os.Stderr,
				"coordlock: completion for intent %q unreadable (%v); will replay idempotently\n",
				rec.Key, derr)
		}
		pendList = append(pendList, pend{key: rec.Key, stamp: rec.StampWall})
	}
	sort.Slice(pendList, func(i, j int) bool {
		if pendList[i].stamp != pendList[j].stamp {
			return pendList[i].stamp < pendList[j].stamp
		}
		return pendList[i].key < pendList[j].key
	})
	for _, p := range pendList {
		keys = append(keys, p.key)
	}
	sort.Strings(torn)
	return keys, torn, nil
}

// ErrTornIntentLog signals that the intent log contains an unparseable
// record — the new leader must enter operator-confirmed degraded recovery
// (fleet doctor, PR6) rather than silently trusting partial state.
var ErrTornIntentLog = errors.New(
	"coordlock: torn intent-log record(s) — degraded recovery required (run `fleet doctor`)")

// ReplayIntents re-drives every intent-without-completion through handler,
// keyed by the idempotency key, then marks each replayed action complete.
// handler MUST be idempotent: re-driving an action that already finished is
// a no-op, not a duplicate (the design's WAL redo-log pattern). It is
// called only when tok still owns the lease (the boundary applies to
// replay too — a fenced successor must not replay). A torn intent record
// makes ReplayIntents return ErrTornIntentLog after replaying the clean
// ones, so the caller surfaces degraded recovery rather than guessing.
func ReplayIntents(tok LeaseToken, handler func(idempotencyKey string) error) error {
	if !tok.StillOwned() {
		return ErrLeaseLost
	}
	store, err := tok.intentStore()
	if err != nil {
		return fmt.Errorf("coordlock.ReplayIntents: resolve intent store: %w", err)
	}
	keys, torn, perr := store.pendingIntents()
	if perr != nil {
		return fmt.Errorf("coordlock.ReplayIntents: scan intents: %w", perr)
	}
	for _, key := range keys {
		// PER-ACTION FENCE (codex PR4 [P1]). Replay can span a slow handler
		// and many pending intents; a candidate could fence us (or we could
		// self-expire) mid-loop. Re-check ownership BEFORE each handler and
		// BEFORE each completion write so a fenced successor stops driving
		// side effects as a non-owner — the same boundary GuardWithLease
		// enforces, applied to replay. A partially-replayed log is safe: the
		// real successor re-drives the remaining intents idempotently.
		if !tok.StillOwned() {
			return ErrLeaseLost
		}
		if herr := handler(key); herr != nil {
			return fmt.Errorf("coordlock.ReplayIntents: replay %q: %w", key, herr)
		}
		// Mark complete under the CURRENT lease epoch so a subsequent
		// rebuild sees it finished. Re-fence first: the handler may have run
		// long enough for a takeover to fence us; a non-owner must not write
		// a completion (which would suppress the real successor's replay).
		if !tok.StillOwned() {
			return ErrLeaseLost
		}
		if cerr := store.writeCompletion(key, tok.Epoch); cerr != nil {
			return fmt.Errorf("coordlock.ReplayIntents: mark %q complete: %w", key, cerr)
		}
	}
	if len(torn) > 0 {
		return fmt.Errorf("%w: %d record(s): %s",
			ErrTornIntentLog, len(torn), strings.Join(torn, ", "))
	}
	return nil
}

// --- atomic, integrity-checked, quarantined checkpoints ---

// checkpointTrailer is appended after the JSON payload: a magic marker, the
// payload length, its CRC32C, and the writer's epoch. On read we verify all
// three (len + CRC catch a torn write; epoch catches a zombie's future
// write). The trailer is fixed-width + newline-delimited so a truncated
// file fails the length check.
const checkpointMagic = "FLEETCKPT1"

type checkpointTrailer struct {
	Magic   string `json:"magic"`
	Length  int    `json:"len"`
	CRC32C  uint32 `json:"crc32c"`
	Epoch   int64  `json:"epoch"`
	WroteAt int64  `json:"wrote_at"`
}

// ErrCheckpointTorn / ErrCheckpointFutureEpoch are the typed quarantine
// reasons ReadCheckpoint reports so the caller can log precisely why it
// fell back to the last clean checkpoint.
var (
	ErrCheckpointTorn         = errors.New("coordlock: checkpoint torn (bad length/CRC/trailer)")
	ErrCheckpointFutureEpoch  = errors.New("coordlock: checkpoint epoch newer than current lease (zombie write)")
	ErrNoCleanCheckpoint      = errors.New("coordlock: no clean checkpoint available")
	errCheckpointNoTrailerYet = errors.New("coordlock: checkpoint has no trailer")
)

// checkpointPaths returns the live + quarantine paths for a checkpoint
// rooted at <project>/coord/. The live file is checkpoint.json; a torn or
// future-epoch file is renamed aside to checkpoint.quarantine-<ts>.json so
// it can never poison a later read (the pg_rewind clean-stop lesson) yet
// stays on disk for fleet doctor inspection.
func checkpointPaths(tok LeaseToken) (live, dir string, err error) {
	dir = filepath.Join(filepath.Dir(tok.paths.flock), "coord")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "checkpoint.json"), dir, nil
}

// WriteCheckpoint atomically writes payload as the project's coordinator
// checkpoint: .tmp -> fsync -> rename, with a {magic,len,CRC32C,epoch}
// trailer so a later ReadCheckpoint can detect a torn or future-epoch file.
// The write is fenced — a non-owner token is rejected (a zombie must not
// write a checkpoint the successor would then quarantine). The previous
// clean checkpoint is preserved as checkpoint.prev.json BEFORE the rename
// so ReadCheckpoint has a fallback when the new one is torn.
func WriteCheckpoint(tok LeaseToken, payload []byte) error {
	if !tok.StillOwned() {
		return ErrLeaseLost
	}
	live, _, err := checkpointPaths(tok)
	if err != nil {
		return fmt.Errorf("coordlock.WriteCheckpoint: resolve paths: %w", err)
	}
	// Roll the current live checkpoint to .prev (best-effort) so a torn new
	// write still has a clean fallback. A missing live file is fine.
	prev := live + ".prev"
	if _, serr := os.Stat(live); serr == nil {
		// Only roll a VERIFIED-clean live file forward; rolling a torn one
		// to .prev would just give us a torn fallback. Verify first.
		if _, verr := decodeCheckpoint(live, tok.Epoch); verr == nil {
			_ = os.Rename(live, prev)
		}
	}
	blob := encodeCheckpoint(payload, tok.Epoch)
	// RE-FENCE at the COMMIT POINT, not before the I/O (codex PR4 [P1]). The
	// gated write runs StillOwned() AFTER the tmp file is written+fsynced and
	// IMMEDIATELY before the rename — the only instant the new bytes become
	// visible. A zombie's checkpoint carries its STALE epoch (<= successor's),
	// which ReadCheckpoint would NOT quarantine (only future-epoch is), so a
	// late publish would roll the successor's state back. Gating the rename
	// itself closes that window to the minimum a no-lock-across-IO design
	// allows. On a lost lease the tmp is discarded and nothing is published.
	werr := writeBytesAtomicFsyncGated(live, blob, func() error {
		if !tok.StillOwned() {
			return ErrLeaseLost
		}
		return nil
	})
	if errors.Is(werr, ErrLeaseLost) {
		return ErrLeaseLost
	}
	if werr != nil {
		return fmt.Errorf("coordlock.WriteCheckpoint: %w", werr)
	}
	return nil
}

// ReadCheckpoint returns the payload of the last CLEAN checkpoint whose
// epoch is <= the current lease epoch. If the live checkpoint is torn (bad
// len/CRC/trailer) OR its epoch is newer than tok.Epoch (a zombie write),
// the live file is QUARANTINED (renamed aside) and the .prev fallback is
// tried. The typed return tells the caller which quarantine fired; a
// recovery from .prev returns the payload with a nil error plus the reason
// via the returned quarantined slice. Returns ErrNoCleanCheckpoint when no
// clean checkpoint exists at all (a fresh project).
func ReadCheckpoint(tok LeaseToken) (payload []byte, quarantined []string, err error) {
	live, dir, perr := checkpointPaths(tok)
	if perr != nil {
		return nil, nil, fmt.Errorf("coordlock.ReadCheckpoint: resolve paths: %w", perr)
	}
	prev := live + ".prev"

	// Try the live checkpoint first.
	pl, derr := decodeCheckpoint(live, tok.Epoch)
	switch {
	case derr == nil:
		return pl, nil, nil
	case errors.Is(derr, os.ErrNotExist):
		// No live file — fall through to .prev.
	case errors.Is(derr, ErrCheckpointFutureEpoch):
		// A checkpoint stamped NEWER than our token's epoch. Two readings:
		//   - WE are the valid owner and a zombie wrote it -> quarantine it.
		//   - WE are a fenced/stale reader and a real SUCCESSOR wrote a valid
		//     newer checkpoint -> quarantining it would DELETE the new
		//     leader's good state (codex PR4 [P2]). Re-check ownership: if we
		//     no longer own the lease, REFUSE (we are the zombie reader, not
		//     the checkpoint) — never touch the successor's file.
		if !tok.StillOwned() {
			return nil, nil, ErrLeaseLost
		}
		q, qerr := quarantineFile(dir, live, derr)
		if qerr != nil {
			return nil, nil, fmt.Errorf("coordlock.ReadCheckpoint: quarantine future-epoch live: %w", qerr)
		}
		quarantined = append(quarantined, q)
	case errors.Is(derr, ErrCheckpointTorn), errors.Is(derr, errCheckpointNoTrailerYet):
		// A torn (bad len/CRC/trailer) file is garbage regardless of who
		// reads it — safe to quarantine + fall back to .prev (pg_rewind
		// clean-stop lesson).
		q, qerr := quarantineFile(dir, live, derr)
		if qerr != nil {
			return nil, nil, fmt.Errorf("coordlock.ReadCheckpoint: quarantine torn live: %w", qerr)
		}
		quarantined = append(quarantined, q)
	default:
		return nil, nil, fmt.Errorf("coordlock.ReadCheckpoint: read live: %w", derr)
	}

	// Live was missing or quarantined — try the clean .prev fallback.
	pl2, derr2 := decodeCheckpoint(prev, tok.Epoch)
	switch {
	case derr2 == nil:
		return pl2, quarantined, nil
	case errors.Is(derr2, os.ErrNotExist):
		return nil, quarantined, ErrNoCleanCheckpoint
	case errors.Is(derr2, ErrCheckpointFutureEpoch):
		// Same stale-reader guard as the live branch (codex PR4 [P2]).
		if !tok.StillOwned() {
			return nil, quarantined, ErrLeaseLost
		}
		q, qerr := quarantineFile(dir, prev, derr2)
		if qerr != nil {
			return nil, quarantined, fmt.Errorf("coordlock.ReadCheckpoint: quarantine future-epoch prev: %w", qerr)
		}
		quarantined = append(quarantined, q)
		return nil, quarantined, ErrNoCleanCheckpoint
	case errors.Is(derr2, ErrCheckpointTorn), errors.Is(derr2, errCheckpointNoTrailerYet):
		q, qerr := quarantineFile(dir, prev, derr2)
		if qerr != nil {
			return nil, quarantined, fmt.Errorf("coordlock.ReadCheckpoint: quarantine torn prev: %w", qerr)
		}
		quarantined = append(quarantined, q)
		return nil, quarantined, ErrNoCleanCheckpoint
	default:
		return nil, quarantined, fmt.Errorf("coordlock.ReadCheckpoint: read prev: %w", derr2)
	}
}

// encodeCheckpoint frames payload with the integrity trailer:
//
//	<payload bytes>\n<trailer json>\n
//
// The newline before the trailer makes the trailer a single last line so a
// reader can split it off deterministically; a truncated write loses the
// trailer (or part of the payload) and fails verification.
func encodeCheckpoint(payload []byte, epoch int64) []byte {
	tr := checkpointTrailer{
		Magic:   checkpointMagic,
		Length:  len(payload),
		CRC32C:  crc32.Checksum(payload, crc32cTable),
		Epoch:   epoch,
		WroteAt: time.Now().UnixNano(),
	}
	trJSON, _ := json.Marshal(tr) // checkpointTrailer marshals cleanly
	out := make([]byte, 0, len(payload)+len(trJSON)+2)
	out = append(out, payload...)
	out = append(out, '\n')
	out = append(out, trJSON...)
	out = append(out, '\n')
	return out
}

// decodeCheckpoint reads + verifies a framed checkpoint file. It returns
// ErrCheckpointTorn on a bad length/CRC/trailer, ErrCheckpointFutureEpoch
// when the file's epoch exceeds curEpoch (a zombie's write), and the
// payload bytes on a clean read. os.ErrNotExist passes through unwrapped.
func decodeCheckpoint(path string, curEpoch int64) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err // includes os.ErrNotExist
	}
	// The trailer is the last newline-delimited line. Split on the FINAL
	// newline that precedes a parseable trailer.
	body := raw
	if len(body) > 0 && body[len(body)-1] == '\n' {
		body = body[:len(body)-1] // drop the trailing terminator newline
	}
	nl := lastIndexByte(body, '\n')
	if nl < 0 {
		return nil, fmt.Errorf("%w: %s", errCheckpointNoTrailerYet, path)
	}
	payload := body[:nl]
	trLine := body[nl+1:]
	var tr checkpointTrailer
	if json.Unmarshal(trLine, &tr) != nil || tr.Magic != checkpointMagic {
		return nil, fmt.Errorf("%w: %s (bad trailer)", ErrCheckpointTorn, path)
	}
	if tr.Length != len(payload) {
		return nil, fmt.Errorf("%w: %s (len %d != %d)", ErrCheckpointTorn, path, tr.Length, len(payload))
	}
	if crc32.Checksum(payload, crc32cTable) != tr.CRC32C {
		return nil, fmt.Errorf("%w: %s (CRC mismatch)", ErrCheckpointTorn, path)
	}
	if tr.Epoch > curEpoch {
		return nil, fmt.Errorf("%w: %s (epoch %d > current %d)",
			ErrCheckpointFutureEpoch, path, tr.Epoch, curEpoch)
	}
	// Return a copy so the caller can't see into the read buffer's tail.
	out := make([]byte, len(payload))
	copy(out, payload)
	return out, nil
}

func lastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// quarantineFile renames a poisoned checkpoint aside to
// <name>.quarantine-<unixnano>.json so it can never be read again but stays
// on disk for fleet doctor inspection. Returns the quarantine path.
func quarantineFile(dir, path string, reason error) (string, error) {
	base := filepath.Base(path)
	q := filepath.Join(dir, fmt.Sprintf("%s.quarantine-%d", base, time.Now().UnixNano()))
	if err := os.Rename(path, q); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "coordlock: quarantined checkpoint %s -> %s (%v)\n", path, q, reason)
	return q, nil
}

// --- skill-side ownership proof (the --lease-check CLI backs this) ---

// ErrNotLeaseOwner is the typed refusal LeaseCheckByAncestor returns when
// the caller's process tree does NOT descend from the live active lease
// owner — i.e. a fenced/zombie coord's skill-side tick. The skill aborts
// its disk mutation on this, exactly as a Go *WithLease caller aborts on
// ErrLeaseLost. Surface-don't-silo: a typed, explanatory refusal.
var ErrNotLeaseOwner = errors.New(
	"coordlock: caller is not under the active lease owner (fenced/stale coord) — refuse mutation")

// Fence branch tags (DESIGN-coord-lease-false-fence-prevention piece 1).
// STABLE strings embedded in every fence return's error text so the
// observability layer (loop.py's _parse_fence_detail) can forward WHICH branch
// fenced. A fence verdict only ever SKIPS a tick — no fence path kills a
// session. PR-2 (D7) moved the proof onto the FLOCK, so the tag set collapsed
// to the three flock outcomes; the epoch-era tags are gone.
const (
	// fenceTagDifferentOwner: the flock is held by a LIVE process the caller
	// is NOT a descendant of — a live successor leads; the caller is the zombie.
	fenceTagDifferentOwner = "different-owner-fenced"
	// fenceTagFlockFree: the flock is FREE (kernel-freed on death, or dropped
	// by a graceful Release() mid-handoff). The persisted body still names the
	// old (possibly-alive) supervisor, so it is NOT trusted — a released-but-
	// alive coord's descendant must not PROCEED and write after ownership moved.
	fenceTagFlockFree = "flock-free-fenced"
	// fenceTagFlockHeldUnreadable: the flock is HELD but its body is
	// torn/empty/pid-less, so the caller cannot prove it descends from the
	// holder — fence conservatively (a live holder exists that is not provably us).
	fenceTagFlockHeldUnreadable = "flock-held-unreadable-fenced"
)

// LeaseCheckByAncestor decides whether the calling process (startPid, normally
// os.Getppid()) may mutate, given the project's FLOCK-only lease state (D7). It
// is the executable form of the design's skill-side ownership proof: the tick
// (loop.py) and the fleet-guard per-turn producer fence shell out to it before
// any disk mutation, so a fenced/stale coord's skill-side writes are REFUSED in
// Go (the same boundary the *WithLease APIs enforce), not merely discouraged.
//
// PR-2 (D7) moved the discriminant off the (no-longer-written) epoch onto the
// FLOCK. It reuses PR-1's LOCK_SH busy-probe, NOT the flock body alone: the
// body persists after Release() and still names the old (possibly-alive)
// supervisor, so trusting it would let a released-but-alive coord's descendant
// PROCEED and write after ownership moved. Rule:
//
//	probe coordinator.flock (LOCK_SH|LOCK_NB):
//	  ├─ file never existed (ENOENT) -> PROCEED (legacy / non-wrapped coord).
//	  ├─ FREE (probe acquires) -> FENCE (flock-free-fenced): a stale body after
//	  │    Release / a momentarily-unheld mid-handoff flock is NOT trusted.
//	  └─ HELD (EWOULDBLOCK, a LIVE holder) -> read the body for the holder pid:
//	        ├─ startPid descends from the holder (pid + pid_start match) -> PROCEED.
//	        ├─ NOT a descendant -> FENCE (different-owner-fenced): a live
//	        │    successor holds the flock; the caller is the zombie.
//	        └─ body torn / pid-less -> FENCE (flock-held-unreadable-fenced): a
//	             live holder exists that is not provably us.
//
// The walk is bounded so an unexpected ppid cycle / deep tree can't spin.
// READ-ONLY: it never acquires or mutates the flock — the epoch's --reacquire
// renew path is gone (holding the flock IS ownership; there is nothing to
// renew), so LeaseCheckByAncestorReacquire collapsed into this single entry.
func LeaseCheckByAncestor(project string, startPid int) error {
	return leaseCheckByAncestorWithCfg(project, startPid, defaultLeaseConfig())
}

// leaseCheckByAncestorWithCfg is the seam-injected core so tests drive a
// deterministic pid-liveness probe + ppid walk against a real flock.
func leaseCheckByAncestorWithCfg(project string, startPid int, cfg leaseConfig) error {
	paths, err := resolvePaths(project)
	if err != nil {
		return fmt.Errorf("coordlock.LeaseCheck: resolve paths: %w", err)
	}
	f, oerr := os.OpenFile(paths.flock, os.O_RDONLY, 0)
	if oerr != nil {
		if errors.Is(oerr, os.ErrNotExist) {
			// The flock file was never created -> no lease in play -> PROCEED.
			// A legacy / non-lease-wrapped coord; fencing it would wedge the
			// pre-lease path the reversibility contract preserves.
			return nil
		}
		return fmt.Errorf("coordlock.LeaseCheck: open flock: %w", oerr)
	}
	defer func() { _ = f.Close() }()
	lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if lerr == nil {
		// We acquired the shared lock -> NO exclusive holder -> the flock is
		// FREE. Release the probe and FENCE: the persisted body may still name a
		// released-but-alive old supervisor, and trusting it would let its
		// descendant PROCEED and write after ownership moved (the zombie-write
		// guard). A momentarily-unheld mid-handoff flock is covered too (the
		// successor will hold it; the caller re-checks next tick).
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return fmt.Errorf("%w: %s: coordinator flock for %q is free (no live holder); a stale body is not trusted",
			ErrNotLeaseOwner, fenceTagFlockFree, project)
	}
	if lerr != syscall.EWOULDBLOCK { //nolint:errorlint // bare errno from syscall.Flock.
		return fmt.Errorf("coordlock.LeaseCheck: probe flock: %w", lerr)
	}
	// HELD by a LIVE holder (the kernel frees the flock on death). Read the body
	// for the holder's pid + pid_start; walk startPid's ancestors to it.
	holderPid, holderStart, ok := flockHolderIdentity(paths.flock)
	if !ok {
		// Torn / empty / pid-less body on a HELD flock: a live holder exists we
		// cannot name, so we cannot prove the caller descends from it. Fence.
		return fmt.Errorf("%w: %s: coordinator flock for %q is held by a live holder whose identity is unreadable",
			ErrNotLeaseOwner, fenceTagFlockHeldUnreadable, project)
	}
	ppidFn := cfg.ppid
	if ppidFn == nil {
		ppidFn = ppidOf
	}
	pid := startPid
	const maxDepth = 64
	for depth := 0; depth < maxDepth && pid > 1; depth++ {
		if pid == holderPid {
			// pid matches the holder; confirm it is the SAME process
			// (PID-reuse-safe start-time match). A mismatch means a recycled pid.
			if st, sok := cfg.pidStart(pid); sok && st == holderStart {
				return nil // caller descends from the live flock holder -> PROCEED
			}
			break
		}
		ppid, pok := ppidFn(pid)
		if !pok || ppid == pid {
			break
		}
		pid = ppid
	}
	// The caller is NOT a descendant of the live flock holder -> a live successor
	// holds the lease; the caller is the zombie. FENCE (skip the tick).
	return fmt.Errorf("%w: %s: coordinator flock for %q is held by pid=%d, not an ancestor of pid=%d",
		ErrNotLeaseOwner, fenceTagDifferentOwner, project, holderPid, startPid)
}

// flockHolderIdentity reads coordinator.flock's body for the holder's pid +
// pid_start. ok=false when the body is absent / torn / pid-less. Best-effort:
// the caller has already confirmed the flock is HELD (LOCK_SH busy-probe), so
// this only supplies the identity for the ancestor walk.
func flockHolderIdentity(flockPath string) (pid int, pidStart int64, ok bool) {
	b, err := os.ReadFile(flockPath)
	if err != nil || len(b) == 0 {
		return 0, 0, false
	}
	var body flockBody
	if json.Unmarshal(b, &body) != nil || body.Pid <= 0 {
		return 0, 0, false
	}
	return body.Pid, body.PidStart, true
}

// --- atomic write helpers (shared) ---

// fsyncDir fsyncs a directory so a rename into it is durable across a
// crash/power loss (the renamed entry, not just the file contents). Mirrors
// internal/state.fsyncParent.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// writeJSONAtomicFsync marshals v and writes it atomically (.tmp -> fsync
// -> rename), fsyncing the tmp file before the rename so a crash never
// leaves a torn record visible at the live path.
func writeJSONAtomicFsync(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return writeBytesAtomicFsync(path, b)
}

func writeBytesAtomicFsync(path string, b []byte) error {
	return writeBytesAtomicFsyncGated(path, b, nil)
}

// writeBytesAtomicFsyncGated is writeBytesAtomicFsync with an optional
// beforeCommit hook that runs AFTER the tmp file is written+fsynced but
// IMMEDIATELY BEFORE the rename (the commit point). If beforeCommit returns
// an error, the tmp is removed and the live file is left untouched — nothing
// is published. This is the tightest possible just-in-time fence for a
// lease-gated atomic write (codex PR4 [P1]): the rename is the only moment
// the new bytes become visible, so re-validating ownership right before it
// closes the check-to-commit window that a check-before-all-the-IO leaves
// open. No lock is held across the I/O (design constraint); the hook is a
// fast epoch re-read.
func writeBytesAtomicFsyncGated(path string, b []byte, beforeCommit func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	// UNIQUE temp per writer (codex PR4 [P2]). This helper holds NO lock
	// across the I/O, so a stale and a current writer can race the same
	// target. A FIXED ".tmp" name would let a stale writer's cleanup (os.Remove
	// on a lost-lease beforeCommit) delete the bytes the new leader is mid-
	// writing, or two writers clobber one tmp. A pid+nanos suffix isolates
	// each writer's tmp; only the winning Rename publishes. Mirrors
	// state.WriteAtomic's ".tmp.<pid>" pattern.
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open tmp: %w", err)
	}
	if _, werr := f.Write(b); werr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fsync tmp: %w", serr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", cerr)
	}
	// Just-in-time gate: the LAST thing before the bytes become visible.
	if beforeCommit != nil {
		if gerr := beforeCommit(); gerr != nil {
			_ = os.Remove(tmp)
			return gerr
		}
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", rerr)
	}
	// Fsync the PARENT DIRECTORY so the renamed entry survives a crash/power
	// loss (codex PR4 [P2]). Without this the file contents are durable but
	// the directory entry pointing at them can be lost on recovery — an
	// intent/completion/checkpoint would vanish from replay, breaking the
	// bounded-RPO guarantee. Mirrors internal/state.WriteAtomic's parent
	// fsync. The fsync FAILING is surfaced (we promise durability here); a
	// torn replay is worse than a loud error.
	if derr := fsyncDir(filepath.Dir(path)); derr != nil {
		return fmt.Errorf("fsync dir: %w", derr)
	}
	return nil
}

# TASK PLAN — make TestConcurrentWriters_NoLostUpdate deterministic

- **Status:** DRAFT — pending dual review → PROMOTE
- **Priority:** P1 (test-hygiene; degrades the merge pipeline) · **PR-base:** `main` · **Depends-on:** none
- **Scope:** one test function in `internal/dispatch/durability_test.go`. No production-code change.
- **Impl engine:** codex (xhigh)

## The problem (plain English)

A test called `TestConcurrentWriters_NoLostUpdate` fails at random on CI, even on
pull requests that don't touch the code it exercises. It reddened PR #265 (2026-07-09)
and again PR #274 (2026-07-12) — both diffs were `internal/tui`-only, zero overlap with
the tested code, so the failure is noise, not a real regression. Noise in CI forces
re-runs and erodes trust in a red check. Our house rule is that tests must be
deterministic — no assertion that depends on timing or scheduler luck.

## How it works today

The test proves the *no-lost-update* property of the dispatch journal: many goroutines
run read-modify-write mutations against the **same** journal file at once, and none may
clobber another's write. It does this by having 50 goroutines each call `ReserveReplay`,
which — under a per-journal file lock (flock) — increments a counter and writes it back.
If the lock works, the final counter equals 50 (every increment survived). A broken or
non-shared lock would lose increments and the counter would come up short.

```
50 goroutines ──► ReserveReplay(id) ──► [ take per-id flock ]
                                          j.ReplayEmitAttempts++      ← the RMW
                                          write journal
                                          release flock
final assert:  ReplayEmitAttempts == 50   ← the real invariant (no lost update)
```

## What goes wrong

`ReserveReplay` takes the flock with a **bounded acquire deadline**. When 50 goroutines
pile onto one lock on a loaded CI runner, some don't win the lock before their deadline
and get back `outcome = "contention"` — a first-class, *correct* result of `ReserveReplay`
(one of `reserved` / `capped` / `not_pending` / `absent` / `contention`), rooted in the
dispatch-durability #184 flock-deadline design. `ReserveReplay` returns `contention` only
*after* the full acquire deadline elapses without winning the lock. Contention means "try
again later," not "failure."

But the test asserts that **all 50** goroutines return `"reserved"`:

```go
if res.Outcome == ReplayReserved { reserved[i] = true }
else { t.Errorf("...want reserved") }        // ← fires on a legit contention
...
if got != n { t.Fatalf("only %d/%d reservations succeeded", got, n) }  // ← the flake
```

On PR #274 CI, 14 of 50 timed out on the lock → `got == 36` → `only 36/50 reservations
succeeded`. Nothing was actually wrong; the test just treated a legitimate "retry me"
as a hard failure.

```
loaded CI runner:  36 goroutines win the flock → "reserved"
                   14 hit the acquire deadline  → "contention"   (correct!)
   test today:     asserts all 50 == "reserved"  →  FAIL (flake)
```

## The fix

Treat `"contention"` as "retry," not "failure." Each goroutine loops on `ReserveReplay`,
retrying **only** on `ReplayContention`, until it reserves — bounded so a genuinely stuck
lock can't hang the test. Because the flock actually serializes and the critical section
is tiny, every goroutine wins its lock within a few retries, so all 50 still reserve and
the final counter still equals 50. The flake disappears; the invariant is unchanged.

```
each goroutine:  loop:
                   res = ReserveReplay(id, cap)
                   if res.Outcome == ReplayReserved → done
                   if res.Outcome == ReplayContention → retry (bounded)
                   else → t.Errorf (capped/absent/not-pending are real bugs here)
final assert:   all 50 reserved  AND  ReplayEmitAttempts == 50   (unchanged)
```

### Why this doesn't weaken the test

The no-lost-update invariant is untouched: after all 50 reserve, `ReplayEmitAttempts`
must still equal 50. A **non-shared lock** (each goroutine grabbing its own private lock)
never contends, so every call returns `reserved` — but the unsynchronized read-modify-write
loses increments, so `ReplayEmitAttempts < 50` and the test still fails. A **stuck lock**
(never releases) is caught by the retry bound: it exhausts the retries and fails with a
clear message rather than passing. And the "lock actually blocks under contention"
property — the deterministic discriminator — is pinned separately by two sibling tests
that this change does **not** touch — `TestMarkLaunchAttempted_ContentionRetriesNeverSkips`
(durability_test.go:126) and `TestAcquireReleaseTakeFlock` (durability_test.go:223) — so
the discriminator that a broken lock demands (per the concurrent-safety standard: a
no-lost-update test alone can pass with a non-shared lock; pair it with a busy-lock
blocker) is present and unweakened.

---

## Implementation detail (for engineers)

### The one change

`internal/dispatch/durability_test.go`, `TestConcurrentWriters_NoLostUpdate` (~167-214):
replace the single `ReserveReplay` call in the goroutine body with a bounded retry loop:

```go
go func(i int) {
    defer wg.Done()
    // Contention is a legitimate ReserveReplay outcome (#184): under a loaded
    // runner a goroutine can miss the flock deadline. Retry on contention only —
    // the flock serializes and the critical section is O(1), so every goroutine
    // reserves within a few tries. A genuinely stuck lock exhausts the bound and
    // fails loudly via the t.Errorf below (each try blocks up to the ~2s flock
    // deadline, so 50*2s=100s worst case stays well under Go's test timeout).
    const maxTries = 50
    for try := 0; try < maxTries; try++ {
        res, err := ReserveReplay(id, cap)
        if err != nil {
            t.Errorf("reserve %d: %v", i, err)
            return
        }
        switch res.Outcome {
        case ReplayReserved:
            reserved[i] = true
            return
        case ReplayContention:
            continue // retry
        default:
            t.Errorf("reserve %d: outcome=%q want reserved", i, res.Outcome)
            return
        }
    }
    t.Errorf("reserve %d: contention never cleared in %d tries (stuck lock?)", i, maxTries)
}(i)
```

Everything after `wg.Wait()` stays as-is: `got == n` and `ReplayEmitAttempts == n`.

### Decisions / notes

- **No `time.Sleep` between retries.** A sleep would re-introduce a timing dependency and
  slow the test. The flock's own blocking acquire (up to its deadline) already paces the
  retry — a tight loop is fine and stays deterministic.
- **Bound = 50.** Each `ReserveReplay` blocks up to the ~2s journal-lock deadline before
  returning `contention`, so `contention` is only returned after a full 2s wait. In a
  healthy run that wait essentially never elapses (holders release in microseconds), so a
  goroutine reserves in 1–2 tries and 50 is enormous headroom. For a genuinely stuck lock,
  50 × 2s = 100s worst case stays well under Go's default 10-minute test timeout — so the
  explicit `t.Errorf("...stuck lock?")` fires with a clear message instead of the test
  dying as a timeout goroutine dump. (A 1000 bound would take ~33 min and be killed by the
  timeout first, defeating the "fails loudly" guarantee.)
- **`cap = n + 1` is unchanged**, so `ReplayCapped` cannot occur; it lands in the `default`
  (real-bug) arm, which is correct.
- **No production code changes.** `ReserveReplay` already returns `ReplayContention`
  correctly; the bug is entirely in the test's assertion.

### Non-goals

- The Python sibling flake (`durability-test-flake-bbef`,
  `skills/coordinator/tests`, order-dependent full-suite flake) is a separate task —
  different language, different failure mode, separate PR.
- No change to `ReserveReplay` or any flock/deadline behavior.
- No change to the two sibling busy-lock discriminator tests.

## Test plan

| # | Scenario | Input | Expected |
|---|----------|-------|----------|
| 1 | Fixed test passes normally | `go test -race -run TestConcurrentWriters_NoLostUpdate ./internal/dispatch` | PASS; final `ReplayEmitAttempts == 50` |
| 2 | Stress / no flake under load | `go test -race -count=50 -run TestConcurrentWriters_NoLostUpdate ./internal/dispatch` | PASS all 50 iterations (was intermittently red) |
| 3 | Whole package still green | `go test -race -count=1 ./internal/dispatch` | PASS; sibling discriminator tests unaffected |
| 4 | Invariant still discriminates a broken lock | (reviewer thought-check, no code) | non-shared lock ⇒ `ReplayEmitAttempts < 50` ⇒ still FAIL; stuck lock ⇒ retry bound exhausts ⇒ still FAIL |

## Acceptance criteria

- `TestConcurrentWriters_NoLostUpdate` no longer asserts a specific per-goroutine outcome
  count; it retries on `ReplayContention` and still asserts `got == n` **and**
  `ReplayEmitAttempts == n`.
- `go test -race -count=50 -run TestConcurrentWriters_NoLostUpdate ./internal/dispatch`
  is green (no flake under repetition).
- No production files changed; no other test changed.
- `go build ./... && go test -race -count=1 ./internal/dispatch` and
  `golangci-lint run ./...` pass.

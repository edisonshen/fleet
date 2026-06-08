# TASK PLAN — leak-coord-spawn-idempot-b46d

| | |
|---|---|
| **Parent design** | [`docs/DESIGN-lifecycle-leak-recurrence.md`](DESIGN-lifecycle-leak-recurrence.md) PR-C |
| **Priority** | **P1** |
| **Branch** | `worker/leak-coord-spawn-idempot-b46d` |
| **PR base** | `main` |

---

## TL;DR

The NB-flock (`internal/coordlock/coordlock.go:90`) only serializes *concurrent* dispatchers in a single `runDispatch` call (~hundreds of ms). The cross-invocation veto (`cmd/fleet/dispatch.go:635-644`) is gated on `coordStateFresh && recordExists`, but `coord-state.json` doesn't exist until the coord's first tick — a 3–5 min **cold-start window** where neither guard catches a double-spawn. Two `projects-spark` coords appeared 18.8 s apart in the 2026-05-29 investigation; a `projects-rainier` recurrence was observed 2026-05-29 mid-session. Fix: durable pending-spawn claim under the flock + OR-based veto + idempotent spawn keyed on the stable `coord-<project>` task_id.

---

## Before / After

```
TODAY                                          AFTER
=====                                          =====
T+0     dispatch #1: take flock, spawn,        T+0     dispatch #1: take flock, write
        release flock                                  pending-claim, spawn, release
T+1s    coord boots (3-5 min cold start);             flock. pending-claim survives.
        coord-state.json NOT yet written        T+1s    coord boots
T+18s   dispatch #2: take flock (free —         T+18s   dispatch #2: take flock
        first holder released)                          → check veto: pending-claim fresh?
        → check veto: coord-state.json                    YES → refuse spawn
          absent? → veto SKIPPED                        → exit 0 "coord-spawn already in flight
        → spawn ANOTHER coord                             for <project> (claim age 18s)"
        → DUPLICATE                             T+3min  coord's first tick clears
                                                        pending-claim; veto switches to
                                                        live-record + coord-state path.
```

---

## Acceptance checklist

- [ ] `cmd/fleet/dispatch.go::runDispatch`: under the `coord-spawn.lock` critical section, BEFORE `spawn.Spawn`, write a durable claim file `~/.fleet/projects/<p>/coord-spawn-pending` containing `{agent_id, spawned_at_utc, pid}`. Atomic .tmp + rename.
- [ ] `cmd/fleet/dispatch_recovery.go` (or wherever `coordStateFresh` lives): new helper `coordPendingClaimFresh(project, budget)` that reads the pending file and returns true if `now - spawned_at < budget` (default 5 min).
- [ ] `cmd/fleet/dispatch.go::runDispatch` veto becomes OR:
  ```
  veto if (coordStateFresh(project) && recordExistsInList(project))
       OR (any agent record on disk with task_id=="coord-"+project + fresh last_activity_ts + live tmux session)
       OR (coordPendingClaimFresh(project, 5min))
  ```
- [ ] Coord's first `/coordinator` tick clears the pending claim (writes `coord-spawn-pending.cleared` marker or removes the file) — see `skills/coordinator/loop.py`.
- [ ] Stale pending claims (older than the budget) are ignored by the veto and reaped on the next dispatch attempt — no infinite block.
- [ ] Spawn path becomes idempotent on the stable `coord-<project>` task_id: scan `~/.fleet/agents/*.json` for any non-archived record with that task_id + live PID/tmux; if found, RETURN ATTACH (or print "coord <id> already alive for <project>; attach with `fleet attach <id>`") instead of spawning.
- [ ] All gates green; codex + `/review` to 0 P0/P1 in two consecutive runs.

---

## Test cases

| # | Scenario | Setup | Input | Expected |
|---|---|---|---|---|
| T1 | Cold-start double-spawn refused via pending claim | dispatch #1 just wrote pending-claim + spawned (spawn_at = now - 18s); coord-state.json NOT yet present | dispatch #2 for same project | refusal; stderr: `coord-spawn already in flight for projects-fleet (claim age 18s, agent abc)`; exit 0; only 1 coord process |
| T2 | Cold-start refused via live-record (claim removed but coord-state still absent) | claim cleared by coord's first tick; coord-state.json STILL not written; live agent record for `coord-projects-fleet` + live tmux | dispatch #2 | refusal via record-path; exit 0 |
| T3 | Stale pending claim is ignored | pending-claim with spawn_at = now - 10min (way past budget); no live coord | dispatch #2 | proceeds with spawn; old pending claim overwritten |
| T4 | Live coord exists → attach instead of spawn | live record for `coord-projects-fleet` (tmux session alive); operator runs dispatch | dispatch (cmd/fleet/dispatch.go --coord-spawn projects-fleet) | message: "coord <id> already alive for projects-fleet; attach with `fleet attach <id>`"; no spawn |
| T5 | Coord first-tick clears pending claim | spawn writes pending-claim; coord starts; first tick runs | inspect `~/.fleet/projects/<p>/coord-spawn-pending` after first tick | file removed or `.cleared` marker present |
| T6 | NB-flock still wins for true concurrent dispatchers (sub-second race) | dispatch #1 and dispatch #2 invoked within 50ms (same project) | both ran in parallel | exactly one acquires the flock; the other returns "lock-busy" cleanly |
| T7 | Veto OR-logic exhaustive | for each veto signal (coord-state, record, pending-claim) present alone and in combination → refuse | parameterized test | refuses in all 7 truthy combinations; allows when all three absent |

---

## Files to modify

| File | Why |
|---|---|
| `cmd/fleet/dispatch.go` | Pending-claim write under flock; OR-veto |
| `cmd/fleet/dispatch_recovery.go` | New `coordPendingClaimFresh` helper + tests |
| `internal/coordlock/coordlock.go` | Possibly: a claim helper if the file lives near the lock |
| `skills/coordinator/loop.py` | Clear pending claim on first tick |
| `internal/tui/keys.go` | Optional: TUI's in-memory `coordSpawnInFlight` augmented to also write the disk pending-claim (so TUI-initiated and CLI-initiated dispatches dedup against each other) |
| Tests: `cmd/fleet/dispatch_test.go`, `cmd/fleet/dispatch_recovery_test.go`, `internal/coordlock/coordlock_test.go`, `skills/coordinator/tests/test_first_tick.py` | T1–T7 |

---

## Non-goals

- Rewriting the coord spawn path itself (only adding the pending-claim layer).
- Cross-host coordination.
- Changing the cold-start duration (3-5 min is determined by Claude session boot).

---

## Worker contract reminders

- Read `/Users/pinkbear/.claude/CLAUDE.md` + `/Users/pinkbear/projects/fleet/CLAUDE.md` first.
- WIP at `~/.fleet/subagent-wip/leak-coord-spawn-idempot-b46d.md`.
- Three-stage flow. PR `--base main`.

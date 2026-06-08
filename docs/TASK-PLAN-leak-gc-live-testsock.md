# TASK PLAN — leak-gc-live-testsock-74c9

| | |
|---|---|
| **Parent design** | [`docs/DESIGN-lifecycle-leak-recurrence.md`](DESIGN-lifecycle-leak-recurrence.md) PR-D |
| **Priority** | **P1** |
| **Branch** | `worker/leak-gc-live-testsock-74c9` |
| **PR base** | `main` |

---

## TL;DR

`fleet gc` today reaps bare socket FILES on `/tmp/fleet-test-*.sock` but **deliberately spares any socket whose tmux server is still alive** (per `internal/testutil/sweeper.go:119-127` — to avoid stranding a bound server during a real test). That's the wrong behavior for orphaned test sessions whose owning `go test` process is long dead. The operator had to manually `tmux -S <sock> kill-server` during the 2026-05-29 OOM — violating the `feedback_fleet_owns_its_resources.md` rule that fleet's tools must reap fleet's resources, never the operator by hand. This PR: extend `fleet gc` to reap LIVE `fleet-<id>` tmux servers bound to `/tmp/fleet-test-*.sock` when no live `go test` parent process owns them. Surface by default; kill under `--apply --aggressive`.

---

## Before / After

```
TODAY                                          AFTER
=====                                          =====
operator finds 7-day-old leaked test claude    operator runs `fleet gc --kinds orphan-tmux`
$ fleet gc --apply                               (default kinds includes the new test-sock check)
  → reaps bare .sock files (no server)         → surface: "orphan-tmux  fleet-7bbb1727
  → SPARES live test-sock servers (safety)         on /tmp/fleet-test-d8c39c.sock
operator must hand-kill:                           verb=surface  reason=no live go test parent;
  $ tmux -S /tmp/fleet-test-d8c39c.sock              pass --aggressive to kill"
    kill-server                                $ fleet gc --apply --aggressive
  ↑ violates "operator never manually kills"     → kills the orphan tmux servers via tmux -S kill-server
                                                 → operator never types `tmux kill-server` again
```

---

## Acceptance checklist

- [ ] `internal/gc/gc.go::orphan-tmux` (or `sockets` kind — wherever the check best fits) gains a sub-detector for live `fleet-<id>` tmux servers bound to `/tmp/fleet-test-*.sock`:
  - Enumerate `/tmp/fleet-test-*.sock` files.
  - For each, `tmux -S <sock> ls` to see if a `fleet-<id>` session exists.
  - For each session: identify if any live `go test` (or `fleet.test` binary) PID owns it. If NO live owner → orphan.
- [ ] Surface by default — print one line per orphan with the exact `tmux -S <sock> kill-server` command in the reason.
- [ ] Under `--apply --aggressive`: invoke `tmux -S <sock> kill-server` for each orphan; verify it's gone; print `verb=killed`.
- [ ] Without `--aggressive` (just `--apply`): SKIP — preserves the existing surface-don't-silo default for live sessions. Reason: `surface only; pass --aggressive to kill orphan test-sock tmux servers`.
- [ ] A LIVE test (real `go test` running, owning the socket) is NEVER reaped — the live-owner check protects in-flight tests.
- [ ] Help text + CLI flag docs updated to describe the new behavior.
- [ ] All gates green; codex + `/review` to 0 P0/P1 in two consecutive runs.

---

## Test cases

| # | Scenario | Setup | Input | Expected |
|---|---|---|---|---|
| T1 | Orphan test-sock tmux surfaced in dry-run | `tmux -S /tmp/fleet-test-AAA.sock new-session -d -s fleet-orphan -c /tmp` (no live `go test` parent) | `fleet gc --kinds orphan-tmux` | stdout includes `orphan-tmux  fleet-orphan  verb=surface  reason=test-sock with no live go test parent (run `fleet gc --apply --aggressive` to kill)`; server still alive after the dry-run |
| T2 | `--apply --aggressive` kills orphan test-sock tmux | same as T1 | `fleet gc --apply --aggressive --kinds orphan-tmux` | stdout: `verb=killed`; `tmux -S /tmp/fleet-test-AAA.sock ls 2>&1` returns "no server running"; socket file gone (cleanup follows kill) |
| T3 | `--apply` without `--aggressive` spares orphan test-sock | same as T1 | `fleet gc --apply --kinds orphan-tmux` | stdout: `verb=skip`; reason: `surface only; pass --aggressive`; server still alive |
| T4 | Live `go test` parent → NOT reaped | actual `go test` process spawns tmux on test-sock (real test); PID of go test alive | `fleet gc --apply --aggressive --kinds orphan-tmux` | session NOT killed; verb=skip; reason: `live go test parent PID=<n>` |
| T5 | Bare socket file (no server) — still reaped by existing `sockets` kind | `/tmp/fleet-test-BBB.sock` exists with no server; older than `--max-age` | `fleet gc --apply --kinds sockets` (existing behavior) | file removed (existing behavior preserved) |
| T6 | Non-test `fleet-<id>` session (default tmux socket — coord/agent) NOT reaped by this check | `tmux new-session -d -s fleet-coord-XYZ` on the default socket (not `/tmp/fleet-test-*`) | `fleet gc --apply --aggressive --kinds orphan-tmux` | session NOT killed by the test-sock detector; gc's existing `orphan-tmux` logic for default-socket coord sessions still applies separately |

---

## Files to modify

| File | Why |
|---|---|
| `internal/gc/gc.go` | New detector branch for orphan test-sock tmux; live-owner check |
| `cmd/fleet/gc.go` | Help text update; ensure `--aggressive` flag is wired to the new detector |
| `internal/gc/gc_test.go` | T1–T6 (uses test fixtures that spawn/spare real tmux on custom sockets — pair with the cleanup helper from `leak-test-spawn-stub` if that lands first; otherwise inline safe fixtures) |

---

## Non-goals

- Modifying the existing `sockets` kind file-removal logic.
- Touching coord/agent tmux on the default socket — those have a separate orphan-tmux branch with its own rules (operator-marker gate).
- Adding a watchdog daemon — `fleet gc` is one-shot and stays one-shot.

---

## Dependency

- **None hard**, but pairs well with `leak-test-spawn-stub-a696` (which closes the test-suite teardown gap) and `leak-rc-daemon-lifecycle-efc2` (sibling gc-kind work). Branches off `main`; coord serializes via cap=1 unless operator bumps cap with a Files: declaration.

---

## Worker contract reminders

- Read `/Users/pinkbear/.claude/CLAUDE.md` + `/Users/pinkbear/projects/fleet/CLAUDE.md` first.
- WIP at `~/.fleet/subagent-wip/leak-gc-live-testsock-74c9.md`.
- Three-stage flow. PR `--base main`.

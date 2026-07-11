# TASK PLAN — Durable handoff resume delivery

- **Status:** DUAL-REVIEW CLEAN (codex 2 runs PASS + `/review` PASS; P1 retry-cadence resolved, P2s fixed). Collapsed to **2 PRs** (was 3 — split by priority not mechanism). **Ready for operator OK to dispatch PR-1.**
- **Priority:** **P0** (Path B) / P1 (Path A) / P1 (Path C inline)
- **Design:** `docs/DESIGN-handoff-resume-delivery-durable.md` (DUAL-REVIEW CLEAN, operator-approved 2026-07-10). This plan is *execution only* — see the design for the why/what.
- **Depends-on:** builds on merged `HandoffJournal` (flock-only PR-1/PR-2, commits 70a0e6bb / 6390fac2). No blocking deps.
- **PR-base:** `main` (all PRs; stacked at branch level only).
- **Task:** `handoff-resume-durable-p-e618`.

---

## TL;DR

Ship the design as **2 PRs**, P0 first — split by *priority*, not by mechanism:

| PR | Unit | Priority | Depends |
|----|------|----------|---------|
| **PR-1** | **Path B — durable pull** (supervisor reads record → bounded retryable nudge; single-writer marker) | **P0** | none |
| PR-2 | **Path A hardened push + OQ1 payload inline** (target session by name; consumption gate; slice `## Next Steps (prioritized)` + `## Key Decisions` into the directive) | P1 | PR-1 |

**PR-1 alone closes the P0** — it's the load-bearing guarantee; ship + dogfood it immediately (this coord hit the bug). PR-2 bundles the complementary hardening (Path A) and the enrichment (inline) into one follow-up; they're facets of the same feature, not separate business units, so they don't warrant separate PRs. PR-2 stacks on PR-1 (the inline wires into PR-1's nudge builder).

```
today:  fleet handoff ──send-keys──X (lost) ──►  successor boots BLANK
                        record.last_handoff_path ✅ but nothing reads it

PR-1:   coord-run (flock holder) ── reads record.last_handoff_path ──► retryable nudge
                                     marker (resumed_handoff_path) written by handoff_resume.py on exit-0
        ▲ durable: no keystroke dependency for the pull; retry owned by resident supervisor
```

---

## PR-1 — Path B: the durable pull (P0)

**Goal:** a handoff successor always receives its handoff, even if every send-keys is lost.

**Deliverables**
1. **coord-state schema:** add `resumed_handoff_path` (string) to coord-state.json — the Go read side and the Python write side.
2. **Marker writer (single writer):** in `handoff_resume.py`, on successful `main()` (exit 0), write `resumed_handoff_path = <doc path>` via its existing `_take_coord_lock` + `_write_coord_state` RMW (lines ~488-520 — reuse, no new plumbing). Project resolution: `FLEET_PROJECT`, falling back to the handoff doc's frontmatter `project:` (always present) so an unset env can't silently no-op the write.
3. **Supervisor pull + bounded retryable nudge:** in `coord.go` (`coord-run`), after the flock is won for a successor, read `record.LastHandoffPath` (spawn.go:1079 — durable; **not** the journal's `RespawnDocPath`, deleted at acquire in coord_lease_unix.go:116). Nudge cadence is **two levels, both bounded** (design → *Nudge cadence*): (a) *transport retry within one nudge* — `SendPromptKeysVerified` re-issued until child readiness converges (booting-drop); (b) *applied retry across the marker* — re-drive whenever `record.LastHandoffPath != coord-state.resumed_handoff_path`, evaluated on flock-acquire **and on each resident `coord-run` wake** (reuse its wake surface, or a small bounded timer for PR-1) so the guarantee doesn't hinge on a future re-acquire; cap at **K** un-applied re-nudges, then **stop and surface a loud operator diagnostic** (never infinite spam, never silent strand). Only the flock winner nudges.
4. **SKILL update:** `SKILL.md` "Resume after handoff" — name `record.last_handoff_path` the source of truth; make running `handoff_resume.py` to completion **mandatory** (its exit-0 is the applied ack).

**Files:** `skills/coordinator/handoff_resume.py`, `skills/coordinator/SKILL.md`, `cmd/fleet/coord.go`, coord-state Go read helper (reuse `readCoordStateDict` / `withCoordState` in `cmd/fleet/checkpoint.go` — the RMW precedent; note `coordStateFresh` in `dispatch_recovery.go` is an mtime-freshness check, not the reader to mirror), + tests.

**Tests** (Setup / Input / Expected)
- T1 marker-writer-success: run `handoff_resume.py` on a valid doc / exit 0 / `resumed_handoff_path=<doc>` written under lock.
- T2 marker-writer-fail: script errors or killed pre-completion / non-zero / marker **not** written.
- T3 no-other-writer: a nudge or a tick that didn't run the script / — / marker untouched.
- T4 project-fallback: `FLEET_PROJECT` unset, doc frontmatter has `project:` / run script / marker written to the right project (not a no-op).
- T5 nudge-decision: record has `last_handoff_path`, marker behind / acquire / supervisor nudges; after script runs, marker catches up → no further nudge.
- T6 nudge-retry: child not ready on first nudge / poll / supervisor re-nudges until ready (retry owned by resident supervisor).
- T7 nil-path: `last_handoff_path==nil` (fresh coord) / acquire / no nudge.
- T8 own-record: supervisor reads the successor's own record id / — / never targets the outgoing coord's doc.
- T9 re-nudge-on-crash: prompt landed but agent turn ended pre-script (marker un-advanced) / next acquire-or-wake / re-nudges (benign); never permanently suppressed; survives `loop.py`/`coord-run` restart.
- T10 two-contenders: handoff/standby overlap / — / only the flock winner nudges; loser doesn't.
- T11 applied-retry-no-restart (the P1 cadence guard): successor takes a turn but skips the mandatory script, `coord-run` does **not** restart / resident wakes / re-nudges each wake while marker is behind, capped at K, then stops + surfaces the operator diagnostic — no infinite spam, no silent strand.
- T12 common-case-zero-renudge: successor runs the script on the first nudged turn / marker advances / no re-nudge on subsequent wakes.

---

## PR-2 — Path A hardened push + OQ1 payload inline (P1, stacks on PR-1)

Complementary follow-up: aim the push right, and enrich the directive payload. Two facets of the same feature, one PR (multiple commits fine).

**Part A — hardened push**
1. **Transport gate:** pass the intended successor's session name (`fleet-<new-id>`) to the delivery, instead of "current lock owner."
2. **Consumption gate (independent):** mark the doc consumed only when `lock-owner == intended successor`. A named-but-losing standby that received keys must **not** mark consumed → leave pending so PR-1's pull still fires.

**Part B — payload inline (OQ1)**
3. **Go section slicer:** slice `## Next Steps (prioritized)` and `## Key Decisions` — **exact** rendered headings (`internal/handoff/handoff.go:295`); mirror `handoff.ParseActiveSubagents` (`internal/handoff/parse.go`, exact-match, break on next `## `).
4. **Wire into both directive builders:** Path A push (delivery.go) + Path B nudge (coord.go) assemble `path + inlined sections`, reading the doc once at delivery.
5. **Guards:** per-section byte cap (curated → short, but bound it); missing sections → path-only, no error.

**Files:** `internal/handoffdelivery/delivery.go`, `cmd/fleet/handoff.go`, `internal/handoff/parse.go` (slicer), `cmd/fleet/coord.go` (wire the inline into PR-1's nudge builder), + tests.

**Tests**
- T13 mid-swap: owner == outgoing coord ≠ intended successor / deliver / does **not** mark consumed, though it targeted the named session.
- T14 push-happy: owner == intended successor, ready / deliver / verified send, marked consumed. (Marker still advanced only by the script per PR-1 — Path A never advances it.)
- T15 inline-present: doc has both exact headings / build directive / payload = path + both sliced sections verbatim.
- T16 inline-missing: placeholder/operator-triggered doc lacks them / build / payload = path only; no error, no empty-section noise.
- T17 inline-cap: pathological oversize section / build / each section truncated to the byte cap; payload bounded.

---

## Test shape (all PRs)

Table-driven per behavior; shared fixture builders for `agent.Record` + a temp `coord-state.json` + a synthetic handoff doc (reuse existing `internal/handoff` test builders and `handoff_resume.py` test fixtures). Budget ≈1.5× production LOC; reviewers flag boilerplate as P2. E2E (T-E2E): full `fleet handoff` old→new with the *initial* delivery dropped and readiness converging after N polls → assert the supervisor's retried nudge resumes the successor (a permanently-dead transport is out of scope — broken terminal).

## Rollout / verify

Standard gate per PR: `go build ./... && go test -race -count=1 ./...`, `golangci-lint run ./...`, `python3 -m pytest skills/ -q`, then codex + /review to two-consecutive-clean, then push + PR (base main). Dogfood: this coord (`d1b783ec`) is itself a handoff successor that hit the bug — after PR-1 merges + rebuild, a `fleet handoff` of a live coord must surface its handoff without any manual doc read.

## Non-goals

Handoff doc *contents*; lease/ownership mechanics; the fresh (non-handoff) coord spawn prompt; retiring the push (OQ2 default = keep both).

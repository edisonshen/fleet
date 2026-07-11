# DESIGN — Durable handoff resume delivery (the successor coord must always get its handoff)

- **Status:** OPERATOR-APPROVED 2026-07-10. Cadence ambiguity resolved in this design (see *Nudge cadence*: acquire + bounded resident re-check, cap K, then surface); implementation PR-1 carries the confirming review evidence.
- **Priority:** **P0** — operator: *"this is P0"*. Silent, total loss of coordinator context on a missed handoff; observed ≥2 times in the field, plus this session.
- **Author:** coord `d1b783ec` (fleet)
- **Scope:** how a handoff-successor coord *receives* (or *fetches*) its handoff doc. NOT the doc's contents (that's `DESIGN-handoff-curated-context.md`) and NOT lease/ownership mechanics (`DESIGN-coord-lease-*`).
- **Depends-on:** builds on `DESIGN-coord-spawn-unified-standby.md` (PR2 "safe delivery", merged) — this design explains why that push-side hardening is necessary but *not sufficient*, and adds the missing pull path.
- **PR-base:** `main`
- **Related rules:** `feedback_fleet_attach_never_exits.md`, `feedback_surface_dont_silo.md`, `project_coord_recovery_needed.md`, `feedback_handoff_curated_not_dumped.md`.

---

## TL;DR

When one coordinator hands off to its replacement, the replacement is supposed to
wake up already knowing what its predecessor did — the "handoff message." Today
that message reaches it by exactly **one** channel: Fleet *types* a line into the
new terminal window ("Read your handoff doc at `<path>`"). If those keystrokes
miss — and during a handoff they routinely can — the new coordinator boots
**blank**, with no idea a handoff doc even exists, even though the doc is sitting
right there on disk and the coordinator's own record points straight at it.

The fix has two halves, and we should ship **both**:

- **Pull (the durable guarantee):** the flock-holding `coord-run` supervisor — the
  resident process that *replaces the epoch* as the liveness owner — reads the
  successor's record, finds `last_handoff_path`, and drives the resume itself,
  retrying (bounded — capped, then it surfaces to the operator) until it lands. No CLI
  keystroke required, and it does **not** depend on the epoch or on a self-perpetuating
  tick. This is the load-bearing fix.
- **Push (hardened UX):** keep typing the prompt, but aim it at the **new**
  session by name (not "whoever holds the lock right now," which mid-handoff can
  be the *outgoing* coordinator that's about to be killed).

---

## The problem, in plain English

A **coordinator** ("coord") is the long-lived agent that owns one project: it
files tasks, dispatches workers, watches PRs. Coords don't live forever — when
one fills its context window (or the operator retires it), Fleet performs a
**handoff**: it writes a **handoff doc** (what's done, what's queued, key
decisions) and spawns a **successor** coord that should pick up exactly where the
predecessor left off.

For that to work, the successor has to actually *read* the handoff doc. The bug:
**it often never learns the doc exists.** It boots as a fresh, contextless Claude
session. The operator talks to it and discovers it knows nothing about the work
in flight. All the continuity the predecessor carefully wrote down is stranded on
disk.

This is not cosmetic. The whole point of a handoff is zero-loss continuity across
coord restarts. A handoff that silently drops its payload is worse than no
handoff — it *looks* like it worked.

---

## How it works today

The handoff doc travels to the successor inside a single typed prompt.

```
  fleet handoff <old-coord>
        │
        │ 1. write handoff doc  → ~/.fleet/handoffs/<old>-<ts>.md
        │ 2. spawn new coord    → tmux session fleet-<new>, running `claude`
        │    (claude takes several seconds to boot and accept input)
        │ 3. set new coord's record.last_handoff_path = <doc path>   ← durable!
        │ 4. send-keys into "the current lock owner":
        │        "Read your handoff doc at <doc path> and continue the task."
        ▼
   successor coord reads that line → opens the doc → resumes.
```

Step 4 is `ResumePrompt()` (`internal/handoff/handoff.go`), delivered by
`DeliverToCurrentOwner` (`internal/handoffdelivery/delivery.go`). The coordinator
SKILL's "Resume after handoff" section then says, literally: *"Read the handoff
doc **named in the spawn prompt**."*

So the doc path is carried **only** by those keystrokes. Note steps 1–3 put
everything durable on disk — the doc, and a pointer to it in the successor's own
record. But nothing on the successor side ever *reads* that pointer. It waits
passively for step 4 to land.

---

## What goes wrong

Two independent failure modes, same result (blank successor):

### Failure A — the keystrokes land on the wrong (dying) coordinator

Be precise about what the current code *does*, because it's already more careful
than a naive one-shot send. `DeliverToCurrentOwner` polls `CurrentOwner` for up to
30s and `finishDelivery` re-validates the owner identity **both before and after**
the verified send. So the residual gap is **not** "there's no stability check" — it
is narrower: *a transiently-stable owner can still BE the outgoing coord.*
"Verified-stable owner" is not the same as "the successor we just spawned." Real
timeline from this session (`d1b783ec` replacing `5ccd6ebe`):

```
  23:14:42  new coord fleet-d1b783ec spawned      (claude still booting)
  23:14:47  OLD coord 5ccd6ebe releases lock + gets SIGKILLed
  23:14:48  NEW coord d1b783ec acquires the lock   ← becomes owner 6s AFTER spawn
```

For ~5–6 seconds after spawn, "the current lock owner" is still the **outgoing**
coord — and it's a *stable* owner during that window, so the pre/post re-validation
passes. A send in that window is typed into a session killed moments later,
`finishDelivery` reports *"delivered!"*, and the doc is marked consumed. The
**surviving** coord got nothing, and nothing retries. I verified: the resume prompt
**never appears in this session's scrollback at all.** The fix is target
*selection* (aim at the successor we spawned), not adding a stability check that
already exists.

### Failure B — the keystrokes are dropped while claude is still booting

Even when targeting is correct, `claude` needs several seconds to paint before it
accepts input. The delivery path has a readiness poll (`WaitReady`), but on timeout
it logs a warning and **"sends anyway"** — into a not-yet-ready TUI, where the keys
can be typed-but-not-submitted or lost. A single one-shot send has no second chance.

### Why the existing "safe delivery" design didn't close this

`DESIGN-coord-spawn-unified-standby.md` PR2 already made delivery a durable
doc-inbox: *"keep the doc pending; mark consumed only after verified delivery; if
delivery fails, the next `fleet attach` re-delivers."* That closes the *unguarded*
race — but two holes remain, exactly the two failures above:

1. A **verified send to a stable-but-outgoing owner** (Failure A) counts as success
   → doc marked consumed → no pending state left to retry.
2. The re-delivery trigger is a **future `fleet attach`**. A live handoff successor
   is already "up," so the operator just starts typing at it — no attach ever runs,
   so re-delivery never fires. And a "send-anyway" into a booting TUI (Failure B) is
   likewise counted delivered.

The push path is fundamentally a *signal that must land at the right session, once*.
The project's own rule is the opposite: **filesystem state must survive; don't
depend on a signal.**

```
   Everything durable already exists          Nothing consumes it
   ┌─────────────────────────────┐            ┌────────────────────────┐
   │ handoff doc on disk        ✅ │            │ successor reads its own│
   │ record.last_handoff_path   ✅ │  ── gap ─► │ record → finds path →  │
   │   → points right at the doc  │            │ loads doc              │
   └─────────────────────────────┘            └────────────────────────┘
                                                  ← this does not exist
```

---

## The fix — two paths, ship both

### Path B (primary): the flock-holding supervisor pulls the handoff

**Anchor it on the `coord-run` supervisor, not "the first tick."** This distinction
is load-bearing (see "Does this survive deleting the epoch?" below). The tick
(`loop.py`) fires from a Claude Code Stop hook, which only fires *after the agent
takes a turn* — and the agent's **first** turn is itself triggered by the send-keys
prompt that Failure A/B can lose. So "surface it on the first tick" would
transitively depend on the very keystroke that's missing. A dead coord takes no
turn, runs no tick, and the pull never fires.

The durable anchor is the **`coord-run` supervisor**: the resident Go parent that
holds the flock, alive for the whole coord lifetime, and — post-epoch — *the*
liveness owner. Every coord now starts this way (`DESIGN-coord-spawn-unified-standby`
made `coord-run --standby` the single spawn path), so on any lease-supported
(unix) platform a handoff successor always has a resident supervisor. On acquiring
the flock for a successor it:

1. Reads the successor's record `~/.fleet/agents/<id>.json` → `last_handoff_path`.
   **This read is the pull** — sourced from disk, not from any prompt the CLI may
   have failed to deliver. (It reads the *record's* `last_handoff_path`, set at
   spawn time and durable — **not** the write-once handoff journal's
   `RespawnDocPath`, which is deleted at this very acquire step; see impl detail.)
2. If `last_handoff_path` is set, drives a **retryable resume-nudge** to the claude
   child: polls the child's readiness and re-nudges until the child actually takes a
   turn carrying *"Read your handoff doc at `<path>`."* Because the supervisor is
   resident, it retries — unlike today's one-shot `fleet handoff` send that fires
   once and exits.
3. An idempotency marker (`coord-state.json:resumed_handoff_path`) suppresses
   re-nudging once the handoff is applied. The marker is written by
   `handoff_resume.py` on success (single writer — see impl detail); the supervisor
   only *reads* it to decide whether to nudge.

Why this is the durable fix:

- **No keystroke dependency for the pull.** The path is read from the record by the
  supervisor. Even if every send-keys is lost, the supervisor still knows the doc
  exists and keeps nudging (bounded to K, then it surfaces — see *Nudge cadence*).
- **Retry is owned by a resident process, not a one-shot CLI.** The nudge transport
  is still send-keys, but the supervisor re-issues it until the child takes a turn —
  Failure B (booting-drop) is retried away instead of lost.
- **No targeting race.** The supervisor reads the *successor's own* record, keyed by
  the successor id it just spawned. It cannot read the outgoing coord's handoff
  (Failure A).
- **Self-healing.** A successor that booted blank self-corrects on the supervisor's
  next nudge cycle — the "self-healing" framing the coordinator model wants.

### Does this survive deleting the epoch?

Yes — and the re-anchoring above is *why*. The epoch (`Activate` + `Heartbeat`/TTL,
`coordinator.epoch`) was only ever the **ownership/liveness** mechanism; it never
drove the tick. Post-PR-2, ownership is the **kernel flock** held by `coord-run`:
"a live process holding the flock is the coord." Path B hangs on that same
flock-holding `coord-run` supervisor — the thing that *replaces* the epoch — not on
the epoch and not on a self-perpetuating tick loop. Deleting the epoch removes a
liveness detail Path B never used.

**Degraded mode (the one real case):** the only spawn without a resident supervisor
is a **lease-unsupported platform** — the `coord_lease_other.go` stub returns
"coordinator lease failover is unsupported on this platform" and runs a bare child.
(There is no `FLEET_LEASE_FAILOVER` env toggle anymore — that control surface was
deleted; do not cite it.) On such a platform Path B's supervisor anchor is absent,
so it falls back to the loop.py-marker surfacing on the next Stop-hook tick —
correct but not continuous. On the operator's platform (darwin/unix) the resident
supervisor is always present, so the primary path always applies. Name the
degradation; don't silently break it.

### Path A (complementary): harden the push

Keep the typed prompt for immediacy (nice when it works — the operator sees the
resume happen in the pane right away), but fix its targeting. **These are two
separate gates, not one:**

- **Transport gate — target the spawned session by name** (`fleet-<new-id>`, which
  handoff already knows) instead of "current lock owner." Removes Failure A's
  mis-targeting of *where the keys go*.
- **Consumption gate — mark consumed only when `lock-owner == intended successor`.**
  This is independent of the transport gate: a named-but-losing standby can receive
  keys and then still die or lose the lock race, so "we sent to the named session"
  must never by itself mark the doc consumed. If the confirmed owner ≠ intended
  successor, leave the doc pending so Path B still fires.

Path A alone is not enough (Failure B remains, and any new race would strand it
again). Path A + Path B is belt-and-suspenders: push for latency, pull for
guarantee.

### Recommendation

Ship **Path B as the load-bearing guarantee**, Path A as complementary hardening.
If we had to pick one for a first cut, it's B — it makes the outcome correct even
when every keystroke is lost. A is a UX/latency improvement on top.

---

## Implementation detail (for engineers)

**Path B — where the pull lives.** Three cooperating pieces:

- *The pull + nudge (durable anchor):* the `coord-run` supervisor
  (`cmd/fleet/coord.go`, the resident flock-holder). After it acquires the flock for
  a successor it reads that successor's record → `last_handoff_path` and, if set,
  drives a retryable resume-nudge to the child (reuse the readiness-verified send
  surface — the same `SendPromptKeysVerified` machinery the delivery path uses). This
  piece depends on no CLI-delivered keystroke. Composes with the supervisor-in-daemon
  **turn-nudge** (`DESIGN-coord-supervisor-in-daemon.md` §6) when that lands.
  - **Nudge cadence — two levels, both bounded (resolves the "retry until applied"
    guarantee).** (1) *Transport retry within one nudge:* poll child readiness and
    re-issue the send until the child takes a turn (handles the booting-drop, Failure
    B). (2) *Applied retry across the marker:* the nudge is (re-)driven whenever
    `record.last_handoff_path != resumed_handoff_path` — i.e. the handoff is not yet
    applied — evaluated on flock-acquire **and on a resident re-check.** Note
    `coord-run`'s leader path today just blocks on `child.Wait()` with no periodic
    wake, so PR-1 **builds** a small bounded timer/goroutine for the re-check (it is
    new work, not a reuse; it composes with / is superseded by the daemon watcher in
    `DESIGN-coord-supervisor-in-daemon.md` when that lands). So the guarantee does
    **not** depend on a future re-acquire: a successor that took a turn but skipped the
    mandatory script gets re-nudged on the next re-check. Bound it: after **K** un-applied re-nudges (small,
    e.g. 3–5) the marker still behind, `coord-run` **stops nudging and surfaces a loud
    operator diagnostic** (`feedback_surface_dont_silo`) — "handoff `<path>` delivered
    but not applied after K attempts; attach and run `handoff_resume.py`" — never
    infinite spam, never a silent strand. The common case (agent runs the script)
    advances the marker on the first turn → zero re-nudges.
  - **Reconcile with the write-once handoff journal (already merged).** The acquire
    step Path B hooks is the *same* point where `defaultAcquireLease`
    (`cmd/fleet/coord_lease_unix.go:116`) already calls
    `coordlock.DeleteHandoffJournal`. The journal
    (`internal/coordlock/handoff_journal.go`) carries its own doc path,
    `RespawnDocPath` — but it is **deleted right here**, so it is NOT a safe source
    for Path B (reading it risks a use-after-delete race). Path B reads
    `record.LastHandoffPath` instead: set at spawn (`internal/spawn/spawn.go:1079`),
    never touched by `DeleteHandoffJournal`, durable for the coord's life. State this
    explicitly so an implementer doesn't wire it to the about-to-be-deleted journal.
    The pull must read the record **after** the flock is won (owner-fenced), whether
    it runs just before or just after the journal delete.
  - **Only the flock holder nudges.** The nudge happens solely on the process that
    *won* the flock; a losing standby / outgoing coord never nudges. This closes the
    two-live-contenders duplicate-nudge case by construction. (The marker itself is
    written by `handoff_resume.py`, not by the nudging supervisor — see below.)
- *The idempotency marker — one writer, and "applied" is a concrete signal:*
  `coord-state.json:resumed_handoff_path`. The **only** writer is `handoff_resume.py`
  itself, which writes `resumed_handoff_path = <doc path>` **on successful exit**
  (`main()` returns 0), under the short `coordinator.lock`. That successful exit *is*
  the "applied" signal — the doc has been surfaced into a live agent turn and its
  resume dispatches emitted. Nothing else writes the marker: not `coord-run`, not the
  push, not `loop.py`. Readers (`coord-run` / `loop.py`) only compare
  `record.last_handoff_path != state.resumed_handoff_path` to decide whether to
  (re-)nudge. `handoff_resume.py` already runs per SKILL "Resume after handoff" step 2
  and already returns an exit code + imports `fcntl` — the marker write is a small
  addition, not new plumbing. This mirrors the sister design's precedent
  (`DESIGN-coord-spawn-unified-standby` §5c: mark-before-effect is the
  *prompt-dropping* ordering — do not do it). If the prompt lands but the agent's turn
  ends before `handoff_resume.py` runs to completion (crash, context exhaustion), the
  marker is **not** advanced → the supervisor's next acquire-or-wake re-check (see
  *Nudge cadence* above) safely **re-nudges** (bounded to K, then surfaces), never
  permanently suppressing an un-applied handoff. This is precisely the failure the
  "on surface" ordering would have re-created.
  - *Project resolution for the write:* `handoff_resume.py` must resolve the project
    robustly before writing the marker — `FLEET_PROJECT` with a fallback to the
    handoff doc's frontmatter `project:` field (the doc it was handed). An unset env
    with no fallback would silently no-op the ack write and nudge forever; the doc
    frontmatter is always present, so use it as the backstop.
  - *Read is lock-free, write is locked:* `coord-run`'s marker read is a plain read;
    `handoff_resume.py`'s write is atomic under `coordinator.lock`. A read racing an
    in-flight write yields at most **one extra benign nudge** (the accepted residual
    above) — never a strand or a corrupt marker. Note it; a test asserts the race is
    benign, not that it's absent.
- *Payload (decided — Open Question 1):* the resume nudge carries the doc **path**
  *plus* the inlined text of the handoff doc's `## Next Steps (prioritized)` and
  `## Key Decisions` sections. The path drives `handoff_resume.py <path>` (the full
  resume + re-dispatch flow); the two inlined sections put the decision-critical
  context directly into the agent's turn **even if the later file read fails**.
  Mechanism: slice the two sections with the same `##`-delimited section parser
  already in the code (`skills/coordinator/handoff_resume.py` slices
  `## Active Subagents` / `## Open PRs` the same way; the Go side mirrors it via
  `handoff.ParseActiveSubagents`) — a reuse, not new plumbing. **Match the exact
  rendered headings**, not paraphrases: the renderer emits
  `## Next Steps (prioritized)` (`internal/handoff/handoff.go:295`) and
  `## Key Decisions`, and the slicers are exact-string matchers — a paraphrased
  `## Next Steps` would silently never match and degrade OQ1 to path-only. The slice happens in the **directive builder** (the supervisor
  for Path B / the delivery path for Path A), reading the doc once at delivery time,
  so the inline survives a later read failure. Guard the payload: these sections are
  *curated and short* by construction (`feedback_handoff_curated_not_dumped`), but cap
  each to a defensive byte bound so a pathological doc can't blow up the send-keys
  payload. A handoff doc missing those sections (e.g., an operator-triggered handoff
  with placeholder bodies) → slicer returns empty → nudge carries just the path (no
  inline), no error.
- *SKILL text:* update "Resume after handoff" so `record.last_handoff_path` is named
  the source of truth (a human resuming manually pulls from the record, not only a
  pasted prompt), and so **running `handoff_resume.py` to completion is mandatory** on
  resume — not optional. If the agent reads the doc but skips the script, the marker
  never advances → the supervisor re-nudges up to K times (per *Nudge cadence*), then
  surfaces to the operator: bounded and loud, never a silent strand and never infinite
  spam. Making the script step non-skippable keeps that path to the common (zero
  re-nudge) case.

**Path A — push targeting.** In `deliverHandoffResumePrompt`
(`cmd/fleet/handoff.go`) / `DeliverToCurrentOwner`
(`internal/handoffdelivery/delivery.go`): pass the intended successor's session
name (transport gate) and gate the **mark-consumed** step on
`owner == intended successor`, not "owner exists" (consumption gate). When they
differ (mid-swap), keep the doc pending. Reuse `SendPromptKeysVerified` unchanged.

**Path A and Path B converge on the same marker without either advancing it.**
Both paths only *deliver the directive* ("run `handoff_resume.py <path>`"); neither
writes `resumed_handoff_path`. The marker is advanced solely by `handoff_resume.py`
on successful exit (above). So the reconciliation is automatic: whether the directive
arrived via Path A's push or Path B's supervisor-nudge, the *same* script run applies
it and advances the *same* marker. There is no "advance on verified send" step to
contradict the mark-after-applied rule. Concretely: if Path A's push lands and the
agent runs the script to completion, the marker advances → a later `coord-run`
restart sees `marker == last_handoff_path` and does **not** re-nudge. If the push
lands but the script never completes, the marker stays behind → the supervisor
re-nudges. The only residual duplicate is a crash *between* a completed script and
its own marker write (a sub-millisecond window under the lock) — one extra benign
nudge, explicitly **accepted** (never a strand).

**Non-goals.** No change to handoff doc *contents*, lease/ownership, or the fresh
(non-handoff) coord spawn prompt.

---

## Test plan

Scenario / Input / Expected — one line each.

**Path B — supervisor pull + nudge (Go, `cmd/fleet/coord.go`):**
1. Supervisor acquires flock for a successor whose record has `last_handoff_path`
   set → reads it from the **record** and drives a resume-nudge to the child (assert
   via the send seam; no CLI prompt involved).
2. Child not yet ready on first nudge → supervisor **retries** until the readiness
   seam reports ready (retry owned by the resident supervisor, not a one-shot).
3. Successor record has `last_handoff_path == nil` (fresh coord) → **no** nudge.
4. Supervisor reads the *successor's own* record id → never targets the outgoing
   coord's handoff (Failure A guard).
5. Source-of-truth: journal `RespawnDocPath` present but `DeleteHandoffJournal` runs
   at acquire → Path B still resolves the path from `record.LastHandoffPath` (no
   read of the deleted journal; guards the use-after-delete wiring mistake).
6. Two live contenders (handoff/standby overlap) → **only the flock winner** nudges;
   the loser does not (duplicate-nudge guard). Neither writes the marker.

**Marker — `handoff_resume.py` is the single writer (`skills/coordinator`):**
7. `handoff_resume.py` completes successfully (exit 0) → writes
   `resumed_handoff_path = <doc path>` under `coordinator.lock`.
8. `handoff_resume.py` errors / is killed before completing → marker **not** written
   (mark-after-applied; the successful exit is the only trigger).
9. `coord-run`/`loop.py` never write `resumed_handoff_path` — assert the marker is
   untouched by a nudge or a tick that didn't run the script.

**Marker — nudge decision (`coord-run` reads only):**
10. `record.last_handoff_path` set, marker behind → supervisor nudges. After the
    agent runs the script to completion, marker catches up → **no** further nudge.
11. Prompt landed but agent turn ended before the script ran (marker un-advanced) →
    next acquire **or resident re-check** **re-nudges** (benign); never permanently
    suppressed. Persists across a `loop.py` / `coord-run` restart.
12. Un-applied after **K** re-nudges (agent keeps skipping the script, no restart) →
    supervisor **stops nudging and surfaces the operator diagnostic** — no infinite
    spam, no silent strand (the cap-then-surface path).
13. New handoff bumps `last_handoff_path` past the marker → nudges the new path;
    marker catches up only after the new script run.

**Path A push targeting:**
14. Mid-swap, current lock owner == outgoing coord ≠ intended successor → delivery
    does **not** mark consumed (consumption gate) even though it targeted the named
    session (transport gate).
15. Push lands → agent runs the script → marker advances (via the script) → a later
    `coord-run` restart sees `marker == last_handoff_path` and does **not** re-nudge
    (Path A/B converge on the one marker; the push itself never advanced it).

**Payload inlining (OQ1):**
16. Handoff doc has `## Next Steps (prioritized)` + `## Key Decisions` (exact rendered
    headings) → nudge payload carries the path **and** both sliced sections verbatim.
17. Doc missing those sections (placeholder/operator-triggered) → payload carries the
    path only; no error, no empty-section noise.
18. Pathological oversize section → each inlined section is truncated to the byte cap;
    the send-keys payload stays bounded.

**Headline regression + E2E:**
19. Exact field failure — the **one-shot** CLI delivery is dropped (as happened this
    session: prompt never landed, no retry). The **resident supervisor retries** the
    nudge; it lands once the child's readiness converges → resume runs, marker advances
    once. **This is the test that guards this session's bug** — the guarantee is the
    retry, not zero-transport delivery.
20. Full `fleet handoff` old→new with the initial delivery dropped and readiness
    converging after N polls → assert the supervisor's retried nudge resumes the
    successor (both halves compose end-to-end). (A permanently-dead transport is a
    broken-terminal degenerate case, explicitly out of scope.)

---

## Open questions

1. **Inline the doc body, or just the path?** — **DECIDED (operator, 2026-07-10):**
   path **+ inlined `## Next Steps` and `## Key Decisions`.** Mechanism + payload cap
   specified in Implementation detail → *Payload*.
2. **Do we retire the push entirely once pull is proven?** Keeping both costs a
   little duplication but gives immediate in-pane feedback. **Default: keep both**;
   revisit after a release of field data. (Operator has not asked to retire it.)

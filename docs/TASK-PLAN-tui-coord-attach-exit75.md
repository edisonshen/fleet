# TASK-PLAN — TUI project-row `[a]` must attach the live coord on lease stand-down, never error

- **Slug:** tui-coord-attach-exit75 · **Priority:** P0 · **Base:** main · **PR:** 1 PR
- **Parent:** this plan (bug surfaced live 2026-06-25; no separate design doc — narrow regression)
- **Review:** codex round-1 (high effort) folded in — 2×P1 (wrong resolver / stale records) + 1×P2 (under-tested) corrected below; PR-stage codex + claude /review still mandatory, multi-round to clean.
- **Operator decisions:** attach must NEVER surface an error when a live coord exists — recover and attach (memory: `fleet attach` never exits). P0 (operator call 2026-06-25).

## Goal
When an operator presses `[a]` on a project row whose coord is already alive, the TUI must attach to the live coord — never render the EX_TEMPFAIL (exit 75) "a coord is already running" line as a fatal banner.

## Workstream summary
| WS | Goal | Effort (S/M/L) | Risk | Decision |
|----|------|----------------|------|----------|
| WS1 | Classify dispatch exit 75 in `startCoordSpawn`; **re-list records from disk** and resolve the leader with the **markerless** `FindLiveCoord`+`FindCoordByLockBody` (exactly what `attach.go`'s `handleCoordSpawnVeto` does), then attach | S | Low | Mirror `attach.go`'s exit-75 path verbatim, not the TUI dedup helper |
| WS2 | Regression tests: marker-backed, **markerless**, and **lock-body-only** live leaders → attach; unresolvable → recoverable flash; non-75 → banner | S | Low | Stub `runFleetCmd` to return exit-75 ExitError; seed disk records, not just `m.records` |

## Problem

The headline "attach to your coord" flow is dead from its natural entry point.

The operator opens the Fleet TUI, moves to a project row (e.g. `projects/fleet`), and presses `[a]` to attach to that project's coordinator. The coord is alive and healthy. Instead of attaching, the TUI prints a red banner:

```
project projects-fleet: dispatch: exit status 75
spawn: coord b43a9587 stood down — a healthy leader already holds the lease for "projects-fleet"
error: a coord is already running for projects-fleet (coordinator lease held by the live leader);
       did not spawn a duplicate — attach to the live coord via TUI [a] or `fleet attach`
```

The banner even tells the operator to "attach via TUI [a]" — which is exactly the key they just pressed. Dead end.

This violates the operator's hard rule: **attach must never exit / never error when a live coord exists — it must recover and attach.**

### How it works today

```
 [a] on project row
        │
        ▼
 startCoordSpawn(project)
        │  shells out:  fleet dispatch coord-<project> --coord-spawn ...
        ▼
 fleet dispatch  ── a healthy leader already holds the lease ──►  exits 75 (EX_TEMPFAIL)
        │                                                          (this is the "attach the
        │                                                           live one" signal, NOT a failure)
        ▼
 runFleetCmd callback:  err != nil  ──►  coordSpawnDoneMsg{err: "dispatch: exit status 75 ..."}
        │
        ▼
 FATAL BANNER  ✗   (operator never attaches)
```

### What goes wrong

Exit code **75** is `EX_TEMPFAIL` from `dispatch.go` (`vetoExitCode = 75`). It is the *designed* signal that a live coord already holds the lease and the caller should **attach to the live one**. The CLI side (`cmd/fleet/attach.go`, `dispatchVetoExitCode = 75`) classifies this correctly and re-resolves + attaches.

The TUI's `startCoordSpawn` callback does **not** classify it. It treats any non-nil `err` from `runFleetCmd` (an `*exec.ExitError`) as fatal and renders the captured output as a banner. The "75 means attach the live leader" branch is simply missing on the TUI project-row path.

### The fix

```
 runFleetCmd callback  (runs in a tea.Cmd goroutine — disk I/O here is fine, no m.records dep):
        │
   err is *exec.ExitError with ExitCode()==75 ?
        │
        ├─ yes ──►  records = agent.ListStrict()              // RE-READ FROM DISK, not cached m.records
        │           rec = FindLiveCoord(records, project)     // markerless live-record scan
        │                 ?? FindCoordByLockBody(records, project)   // lock-body fallback
        │                 │
        │                 ├─ resolved ──►  coordSpawnDoneMsg{ agentID:rec.ID, session, attachedExisting:true }
        │                 │                       │  (NEW flag — NOT the spawn-success shape)
        │                 │                       ▼
        │                 │                 Update (dedicated branch): m.pendingAttach = session; tea.Quit
        │                 │                       └─ attach ONLY — does NOT write the coord-spawn marker  ✓ ATTACH
        │                 │
        │                 └─ unresolved ─►  coordSpawnDoneMsg{ recoverable flash: "coord lease held,
        │                                          live record not yet visible — press [a] again" }
        │
        └─ no  ──►  existing behavior (genuine failure → banner; success → parse agent ID → attach)
```

**Resolver choice (codex round-1 P1-A).** Do NOT use `findExistingCoordForProject` — it is intentionally *marker-gated* (rejects a live coord that lacks the TUI coord-spawn marker, e.g. one started from the shell or after a prompt-delivery failure). The CLI veto path (`cmd/fleet/attach.go` `handleCoordSpawnVeto`) does NOT use it; it uses the **markerless** `projectlookup.FindLiveCoord` with a `projectlookup.FindCoordByLockBody` fallback. The TUI fix mirrors that exact pair so any live leader attaches, not just marker-backed ones.

**Fresh records (codex round-1 P1-B).** The `coordSpawnDoneMsg` handler in `Update` consumes the **cached** `m.records` slice — `loadAgentsCmd()` is a separate command, not an implicit pre-resolve refresh. Relying on `m.records` would false-negative a real winner whose record is already on disk. So resolve in the **callback goroutine** via a fresh `agent.ListStrict()` (exactly as `handleCoordSpawnVeto` re-lists on veto) and hand `Update` the already-resolved session.

**Dedicated attach branch — do NOT reuse the spawn-success shape (codex round-2 P1).** The existing `coordSpawnDoneMsg` success path is spawn-specific: with `promptDelivered:true` it calls `writeCoordSpawnMarkerFn` (`internal/tui/model.go:1019`), which would falsely stamp the coord-spawn marker onto a markerless live leader the TUI did **not** spawn — corrupting future `[a]` dedup semantics. With the flag false it emits the prompt-failed *error* flash. Neither fits "attached an already-live coord." So the resolved exit-75 case carries a **new** flag (`attachedExisting bool`) and gets its **own** branch in `Update`: set `m.pendingAttach = session`, benign non-`isErr` flash ("attached to live coord %s for %s"), `tea.Quit` — and **no marker write**.

## Success criteria

1. `[a]` on a project row with a live coord attaches to that coord's tmux session; no banner.
2. Exit 75 from `fleet dispatch --coord-spawn` is never rendered as a fatal error in the TUI.
3. A genuine spawn failure (non-75 exit, or 75 with no resolvable live record) still surfaces clearly — no silent swallow.
4. The duplicate-`[a]`-during-boot idempotency path is unchanged (still attaches without re-dispatch).

## Deliverables

- WS1: `internal/tui/keys.go` — exit-75 classification in `startCoordSpawn`'s `runFleetCmd` callback; callback re-lists disk records (`agent.ListStrict`) and resolves via `projectlookup.FindLiveCoord` + `projectlookup.FindCoordByLockBody`, emitting `coordSpawnDoneMsg{agentID,session,attachedExisting:true}` or a recoverable-flash `coordSpawnDoneMsg`. `internal/tui/model.go` — `coordSpawnDoneMsg` gains an `attachedExisting bool` field and a **dedicated** branch that attaches (`pendingAttach`+`tea.Quit`) with a benign flash and **no marker write**, plus the recoverable-flash branch.
- WS2: table tests in `internal/tui/*_test.go` covering five branches: exit-75 + marker-backed live leader → attach; exit-75 + **markerless** live leader → attach; exit-75 + **lock-body-only** live leader → attach; exit-75 + no live record → recoverable flash; non-75 error → banner.

## Execution order

1. WS1 — wire the classification + attach-live branch (the behavior fix).
2. WS2 — regression tests pinning all three branches.

(One commit each is fine; one PR.)

## Work breakdown

- **WS1 — classify and attach.** In `startCoordSpawn`'s callback, detect `*exec.ExitError` with `ExitCode() == 75`. On match, **inside the callback goroutine** (not `Update`): re-read records with `agent.ListStrict()`, resolve the leader with `projectlookup.FindLiveCoord(records, project)` then `projectlookup.FindCoordByLockBody(records, project)`. If resolved, emit `coordSpawnDoneMsg{agentID, session, attachedExisting:true}`. If unresolved, emit a `coordSpawnDoneMsg` carrying a recoverable (non-`isErr`) flash. In `Update`, the `attachedExisting` branch attaches (`pendingAttach`+`tea.Quit`) with a benign flash and **does not write the coord-spawn marker** (codex round-2 P1 — reusing the spawn-success path would falsely mark a leader the TUI didn't spawn). Resolving in the callback (off the UI thread, reading disk directly) sidesteps the stale-`m.records` trap and mirrors `attach.go`'s `handleCoordSpawnVeto` line-for-line. The in-flight op gate is already cleared by the `coordSpawnDoneMsg` handler (codex confirmed) — no extra gate code needed.
- **WS2 — tests.** Stub `runFleetCmd` to return a synthetic exit-75 `*exec.ExitError`. Seed **on-disk** records (so the callback's `agent.ListStrict()` sees them) for three live-leader shapes — marker-backed, markerless (`task_id`+`project` only, no coord-spawn marker), and lock-body-only (ID only in `coordinator.lock`) — and assert each ends with `pendingAttach == fleet-<liveID>`. Add a no-live-record variant asserting a non-fatal recoverable flash, and a non-75 (exit 1) variant asserting the existing banner still fires.

## Dependencies

None. Self-contained TUI change; no schema, no CLI flag, no cross-task coupling.

## Blockers (when to STOP and escalate, not push)

- If exit 75 cannot be reliably extracted from the `runFleetCmd` error across platforms (it can: `errors.As` to `*exec.ExitError`, `.ExitCode()`), STOP — do not string-match the banner text.
- If neither `FindLiveCoord` nor `FindCoordByLockBody` resolves the leader on the stand-down path (the winning record genuinely isn't on disk yet), do NOT swallow and do NOT respawn — emit a recoverable (non-fatal) flash telling the operator to press `[a]` again. This matches `handleCoordSpawnVeto`'s "wait-and-retry, never exit 70" contract.

## Acceptance criteria

| WS | Concrete check (regression that proves it) |
|----|--------------------------------------------|
| WS1 | Test: on-disk **marker-backed** live `projects-fleet` coord + `runFleetCmd` stubbed exit 75 → `m.pendingAttach == "fleet-<liveID>"`, no `isErr` flash. |
| WS1 | Test: on-disk **markerless** live coord (`task_id`+`project`, no marker) + exit 75 → attaches. *(This is the case codex P1-A proves `findExistingCoordForProject` would have dropped.)* |
| WS1 | Test: **lock-body-only** live coord (ID only in `coordinator.lock`) + exit 75 → attaches via `FindCoordByLockBody`. |
| WS1 | Test: markerless live leader + exit 75 attaches **without** writing the coord-spawn marker — stub `writeCoordSpawnMarkerFn` and assert it is **not** called (proves codex round-2 P1: no false marker on a coord the TUI didn't spawn). |
| WS1 | Test: exit 75 + no live record on disk → recoverable (non-`isErr`) flash, `pendingAttach` empty, no respawn. |
| WS2 | Test: non-75 exit (e.g. exit 1) → `coordSpawnDoneMsg.err` set, banner renders (existing behavior preserved). |
| WS2 | Test: duplicate `[a]` idempotency path unchanged — existing test still green. |

## Risks

- **Masking a real failure as "attach the live one."** Mitigation: only the exact code 75 takes the attach-live branch; every other exit keeps current banner behavior.
- **Attaching to a stale/dead session.** Mitigation: `FindLiveCoord`/`FindCoordByLockBody` both gate on a live tmux session via the tristate probe (a transport hiccup doesn't drop a live claim); no new liveness logic introduced.

## Non-goals

- Changing `fleet dispatch`'s exit-75 contract or `cmd/fleet/attach.go` (the CLI path already works).
- Reworking the coord lease / stand-down mechanism.
- Any change to agent-row `[a]` (already attaches directly and correctly).
- Auto-retrying the spawn — on lease held we attach the existing leader, we do not respawn.

## Validation

```
go build ./... && go test -race -count=1 ./internal/tui/...
golangci-lint run ./internal/tui/...
```

- Attach the exit-75 unit tests to the PR.
- Manual: with a live coord, press `[a]` on its project row → attaches (no banner). Captured before/after in PR body.

---

## Implementation Notes (for engineers)

- **Broken site:** `internal/tui/keys.go` `startCoordSpawn` → `runFleetCmd(args, func(out string, err error) tea.Msg { if err != nil { return coordSpawnDoneMsg{err: ...} } ... })` (callback begins ~keys.go:2219). The `if err != nil` arm is unconditional — that is the bug.
- **Exit-code extraction:**
  ```go
  var ee *exec.ExitError
  if errors.As(err, &ee) && ee.ExitCode() == dispatchVetoExitCode { /* attach-live */ }
  ```
  `dispatchVetoExitCode` is defined in `cmd/fleet/attach.go:910` as `75`, mirroring `dispatch.go:273` `vetoExitCode = 75`. The TUI package can't import the `main` package const — duplicate it as an unexported `const tuiCoordVetoExitCode = 75` in the tui package with a comment pointing at the two existing definitions (same pattern attach.go used: "Duplicated … mirrors dispatch's vetoExitCode").
- **Veto source:** `cmd/fleet/dispatch.go:1099` returns `&vetoError{...}` on `spawn.ErrCoordStoodDown` (`internal/spawn/spawn.go:40`), which `dispatch`'s top-level maps to exit 75 (`dispatch.go:264-277`).
- **Resolver — use the markerless pair (codex round-1 P1-A).** `internal/tui/keys.go:1953` `findExistingCoordForProject` is marker-gated by design ("ID matches the project's coord-spawn marker") and explicitly drops live coords lacking the marker. Do NOT use it here. Use `projectlookup.FindLiveCoord` (`internal/projectlookup/projectlookup.go:177`, markerless — "any live coord for the project is acceptable") then `projectlookup.FindCoordByLockBody` (lock-body fallback). Both already return a record with `TmuxSession` populated even for legacy empty-session records, so `pendingAttach = rec.TmuxSession` is safe. This is the exact pair `cmd/fleet/attach.go:572-602` (`handleCoordSpawnVeto`) uses.
- **Fresh records — re-list, don't trust `m.records` (codex round-1 P1-B).** The `coordSpawnDoneMsg` handler reads the **cached** model slice (`internal/tui/model.go:572`, `:928`); `loadAgentsCmd()` is a follow-up command, not a pre-resolve refresh. Resolve in the **callback goroutine** with `agent.ListStrict()` (the strict variant attach.go uses so an unparseable record that might BE the leader doesn't vanish), mirroring `attach.go:578`.
- **Message plumbing (codex round-2 P1 — do NOT reuse the spawn-success shape):** add an `attachedExisting bool` field to `coordSpawnDoneMsg`. The spawn-success `default` branch in `internal/tui/model.go` (~:1019) calls `writeCoordSpawnMarkerFn(msg.projectName, msg.agentID)` when `promptDelivered` is true; emitting that shape for an already-live leader would stamp the coord-spawn marker on a coord the TUI never spawned, corrupting `[a]` dedup. So `Update` gets a **new** branch: `case msg.attachedExisting:` → `m.pendingAttach = msg.session`; benign flash `"attached to live coord %s for %s"` (non-`isErr`); `return m, tea.Quit`. No `writeCoordSpawnMarkerFn` call. The unresolved case is a second new shape: a recoverable (non-`isErr`) flash, no attach, no respawn. The in-flight op gate (`coordOpSpawn`, set keys.go:1442) is **already cleared** at the top of the `coordSpawnDoneMsg` handler (model.go:928, codex-confirmed) — that clearing runs before any branch, so both new branches inherit it; do not add gate code.
- **Stand-down contract is exit-75-only (codex-confirmed):** the legacy "print message but exit 0" path was removed (`cmd/fleet/dispatch.go:1042`, `cmd/fleet/main.go:259`). So classifying purely on `ExitCode()==75` is complete; no stdout sniffing needed.
- **Reference working path:** `cmd/fleet/attach.go:895-977` + `handleCoordSpawnVeto` (`:570-615`) document the identical exit-75 → "scan-only re-resolve, no 2nd spawn → attach or wait-retry" contract. The TUI fix is the GUI-side equivalent of that CLI logic — port it, don't reinvent.
- **Live repro context (2026-06-25):** live coord `b58674fb` (tmux `fleet-b58674fb`, project `projects-fleet`) held the lease; a `[a]`-triggered spawn `b43a9587` stood down → exit 75 → banner instead of attach. Workaround in use: agent-row `[a]` on `b58674fb` (sets `pendingAttach` directly) or `tmux attach -t fleet-b58674fb`.

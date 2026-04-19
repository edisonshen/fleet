# TODOS

Deferred work captured from engineering reviews. Items are not PR-ready; they
require design thought and operator decision before a branch gets opened.

Format: `[ ]` = open, `[~]` = in-progress, `[x]` = closed (keep with PR link
for a few weeks then prune).

---

## From engineering review 2026-04-18 (Hermes / OpenClaw / EvoMap / SwarmClaw research)

### [ ] F3 — Event-sourcing framing: progress JSONL as leader

**What.** Make `~/.fleet/progress/<task-id>.jsonl` the append-only event log
of record. Derived state (current agent, handoff count, task status) is a
projection replayed from the log on startup, not a pairwise reconciliation
across multiple state files.

**Why.** Hermes Agent treats session messages as the ground truth and
derives session metadata from them. Fleet's progress JSONL is already
append-only per turn; promoting it to leader simplifies A1 reconcile from
"walk every directory and cross-check" to "replay events, rebuild
projections." Also makes debugging easier: `tail -f progress/*.jsonl` shows
exactly what the system thinks happened, in order.

**Pros.** Simpler crash recovery, cleaner mental model, better observability,
cheaper to implement multi-agent queries later ("which agents were blocked
overnight").

**Cons.** Makes the progress log load-bearing (currently it's only TUI
convenience). Changes the v1 mental model mid-flight. Needs clear write
ordering rules (event writes go first, projection writes second).

**Context.** Relevant Hermes files:
`agent/context_compressor.py`, `hermes_state.py` (SQLite + WAL message log
as leader). Relevant reading: "event sourcing" / "CQRS-lite."

**Depends on.** A1 (atomic writes) landing first. Event sourcing on top of
a write-temp-rename model is additive.

**Decision needed.** Is this v1.1 or v1? Ship v1 with projection-leader
model, revisit for v1.1 based on real debugging pain? Or architect v1 with
event leader from day one?

---

### [ ] F5 — Handoff template: borrow Hermes's "different assistant" framing

**What.** Update the handoff doc template (DESIGN.md "Handoff Doc Structure"
section) to include a prefix that frames the document as a handoff from a
"different assistant," with explicit "do not re-execute already-done work"
guidance.

**Why.** Hermes's `context_compressor.py` has an evolved `SUMMARY_PREFIX`
that went through multiple iterations:

```
[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted into
the summary below. This is a handoff from a previous context window —
treat it as background reference, NOT as active instructions. Do NOT
answer questions or fulfill requests mentioned in this summary; they
were already addressed. Your current task is identified in the '## Active
Task' section of the summary — resume exactly from there. Respond ONLY
to the latest user message that appears AFTER this summary.
```

Fleet's handoff docs will hit the same failure modes Hermes already solved
(agents re-executing work the previous agent completed; answering questions
addressed in the handoff as if they were new; taking "completed" bullet
points as fresh action items). Borrow the wording.

**Context.** See `context_compressor.py:38-48` in the Hermes repo for the
exact prefix. Also OpenCode's "Do not respond to any questions" preamble
and Codex's "different assistant" framing.

**Depends on.** fleet-guard skill implementation (Week 4).

**Decision needed.** Adopt verbatim or paraphrase for Fleet tone.
Recommendation: paraphrase lightly, keep the "different assistant" and
"reference not instructions" framing because they're the load-bearing
pieces.

---

### [ ] F6 — Pre-handoff memory-save reminder pattern

**What.** Before triggering handoff at the Red threshold, fleet-guard
injects a reminder to the agent: "save important notes to MEMORY.md /
task file Progress section before the handoff doc is written."

**Why.** OpenClaw does this automatically before compaction: "Before
compacting, OpenClaw automatically reminds the agent to save important
notes to memory files." Prevents context loss between handoff turns.

**Pros.** Cheap (one extra message injection). Prevents agent from losing
context-sensitive details that wouldn't fit the handoff template.

**Cons.** Adds one extra agent turn before the actual handoff doc write.
May cause recursive PreCompact if context is already at the edge.

**Depends on.** Handoff doc writing flow (Week 4).

**Decision needed.** Whether to implement v1 or defer to v1.1. Low-risk
enhancement; recommendation: v1.

---

### [ ] F7 — Tool-call/result pairing in handoff doc

**What.** If the agent is mid-tool-call when the handoff trigger fires
(tool call issued, result not yet received), the handoff doc should note
that explicitly so the next agent knows the state is indeterminate.

**Why.** OpenClaw's compaction "keeps assistant tool calls paired with
their matching toolResult entries. If a split point lands inside a tool
block, OpenClaw moves the boundary so the pair stays together." Fleet's
kill-and-respawn pattern can lose the result mid-flight; the replacement
needs to know.

**Context.** Add a frontmatter field `handoff_mid_tool_call: true` and a
body section `## Interrupted Tool Calls` when applicable. See
OpenClaw `docs/concepts/compaction.md`.

**Depends on.** Skill's ability to introspect the current turn state
(spike may or may not expose this in the Stop hook payload).

---

### [ ] F8 — Identifier-preserving handoff summary

**What.** When fleet-guard prompts the agent to fill the handoff doc's
sections, instruct it to preserve opaque identifiers (file paths, commit
SHAs, issue numbers, PR IDs, env var names) verbatim rather than
paraphrasing.

**Why.** OpenClaw has `identifierPolicy: "strict"` for compaction for
exactly this reason. Fuzzy paraphrasing of "issue #142" into "the auth
issue" costs the next agent a search they shouldn't have to run.

**Context.** Add an instruction to the skill's handoff prompt template.

**Depends on.** Handoff doc writing flow.

**Decision.** Trivial to add; include in Week 4 skill implementation.

---

### [ ] T1 — Prompt/LLM change eval scope (from prior review's test plan)

**What.** Claude Code hook behavior and fleet-guard prompt changes are both
prompt/LLM-change-adjacent. Define a minimal eval suite that:

1. Replays 5 canonical hook payloads and asserts health JSON is written
   correctly.
2. Replays a context-at-73% transcript and asserts fleet-guard triggers
   Yellow-but-not-Red.
3. Replays a context-at-80% transcript and asserts Red handoff fires with
   correct frontmatter.

**Why.** Fleet-guard prompt tuning is iterative. Without an eval harness,
regressions ship silently.

**Depends on.** Week 0 spike (need real hook payloads to feed the eval).

---

### [ ] T2 — CI workflow for `go test ./...` on PRs (separate from release-on-tag)

**What.** Add `.github/workflows/test.yml` that runs `go test ./...`,
`go vet ./...`, and `gofmt -l` on every PR and push to main. Currently
DESIGN.md only specifies release-on-tag CI.

**Why.** Without pre-merge tests, Week 4 handoff-flow regressions don't
get caught until release. The bar is "no red tests on main."

**Depends on.** Week 1 cobra scaffold landing (needs actual Go code to
test).

---

### [ ] D1 — docs/HOOK-PAYLOAD-SAMPLES.md

**What.** Commit anonymized sample payloads from the Week 0 spike's Stop,
PreCompact, and SessionStart hooks so future contributors don't have to
re-run the spike to know payload shape.

**Why.** The spike's deliverable is a decision doc, not the raw evidence.
Future contributors writing new hook handlers need to see example payloads.

**Depends on.** Week 0 spike completing.

---

## From prior review 2026-04-17 (for reference; most items resolved in 2026-04-18)

- [x] A1 atomicity — decided 2026-04-18: write-temp-rename + startup reconcile. See `docs/STATE.md` A1.
- [x] A2 concurrent CLI lock — decided 2026-04-18: flock(2) on per-project lock. See `docs/STATE.md` A2.
- [x] A3 handoff signal — decided 2026-04-18: watch `handoffs/` via fsnotify, atomic rename = signal. See `docs/STATE.md` A3.
- [x] A4 auto-spawn rate limit — decided 2026-04-18: 3/hour + 30s cooldown + unhealthy flag. See `docs/STATE.md` A4.
- [x] A5 schema versioning — decided 2026-04-18: `schema_version` on every JSON + startup check. See `docs/STATE.md` A5.
- [x] A6 explicit tmux env — applied to `docs/SPIKE-context-pct.md`.
- [x] A7 UTC+UUID handoff filenames — applied to `docs/SPIKE-context-pct.md`.
- [x] A8 fsnotify on macOS — applied to `docs/SPIKE-context-pct.md` and `docs/STATE.md`.

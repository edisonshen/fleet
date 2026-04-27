# Fleet — Claude Code working notes

You are likely Claude Code working in this repo. Read these before touching anything.

## Source of truth

- **Design:** `docs/DESIGN.md` — the approved big-bang launch plan. Do not deviate without explicit operator approval.
- **Week 0 spike:** `docs/SPIKE-context-pct.md` — passive parallel collection. Stop hook fires on every assistant turn; data accumulates while other work proceeds. Spike must clear before **v0.1 release**, but does NOT block implementation. Outcome scopes one piece of fleet-guard (the auto-handoff trigger at 50%/70%) and the DESIGN.md "self-healing" framing only — the rest of v1 is invariant of spike outcome.
- **Skill scaffold:** `skills/fleet-guard/SKILL.md` — agent-side half. Structural code (writing agent JSON, watching context, relaying operator inbox messages) is invariant of spike outcome and can land in parallel. Only the auto-handoff trigger logic at 50%/70% thresholds depends on spike PASS — gate that one piece behind a build flag or land it last, after the spike decision commit.

## Current state

- v0.0.0 stub. `cmd/fleet/main.go` prints version and exits.
- Module: `github.com/edisonshen/fleet`. Go 1.25+.
- No dependencies yet. `go build ./...` should pass with stdlib only.

## What to work on next

Spike runs passively in parallel with everything below. Run `python3 spike/analyze.py` periodically and capture 5 `/context` checkpoints into `spike/q3-checkpoints.md` as you work. Commit the spike decision to `docs/SPIKE-context-pct.md` before v0.1 release.

Implementation order (parallel-safe):

1. **Week 1 — CLI scaffold.** `fleet dispatch`, `fleet attach`, `fleet status`. Add cobra. Spawn Claude in a detached tmux session and write a stub agent record to `~/.fleet/agents/`.
2. **Weeks 2-3 — TUI.** bubbletea + lipgloss. Read agent JSON via fsnotify. Polling fallback at 1s.
3. **Week 4 — Handoffs.** Integrate `fleet-guard` into the spawn flow. Trigger queue + restart-on-handoff. Reconciliation for orphaned tasks. The auto-handoff trigger (50%/70% thresholds) is the only piece that needs to wait for the spike PASS commit — everything else (manual `[h]` handoff, queue plumbing, replacement spawn, handoff doc writing) is spike-invariant.
4. **Week 5 — Release.** GoReleaser, brew tap, demo gif. Spike must be PASS or DESIGN.md rescoped before tagging v0.1.
5. **Week 6 — Dogfood.** Use Fleet to build Fleet for one full week.
6. **Week 7 — Launch.** Show HN + tweet.

## Engineering preferences

- Boring tech by default (bubbletea, cobra, fsnotify, GoReleaser — already in design). Innovation tokens go to agent-health-as-primitive only.
- Filesystem state must survive crashes. Atomic writes (write to `.tmp` then rename), fsync before signaling via queue files.
- Tests come with the feature, not after. CI (`.github/workflows/ci.yml`) runs `go build ./...`, `go test ./...`, and `golangci-lint run ./...` on every PR and push to main (separate from the release-on-tag CI). Local lint: `golangci-lint run ./...` (config at `.golangci.yml`).
- Single-binary distribution. No runtime deps beyond tmux.

## House style

- Terse commits. `fix(handoff): clear assigned field on archive`.
- ASCII diagrams in code comments for non-obvious flows (handoff sequence, reconciliation, fsnotify fan-out).
- No premature abstractions. Three similar lines is better than a generic helper that takes nine arguments.

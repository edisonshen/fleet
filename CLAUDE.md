# Fleet — Claude Code working notes

You are likely Claude Code working in this repo. Read these before touching anything.

## Source of truth

- **Design:** `docs/DESIGN.md` — the approved big-bang launch plan. Do not deviate without explicit operator approval.
- **Week 0 spike:** `docs/SPIKE-context-pct.md` — gates everything. Until the spike's three questions are answered, no `fleet` binary code beyond the current stub.
- **Skill scaffold:** `skills/fleet-guard/SKILL.md` — agent-side half, also gated on the spike.

## Current state

- v0.0.0 stub. `cmd/fleet/main.go` prints version and exits.
- Module: `github.com/edisonshen/fleet`. Go 1.22+.
- No dependencies yet. `go build ./...` should pass with stdlib only.

## What to work on next

In order:

1. **Week 0 spike** (gating). Fill in `docs/SPIKE-context-pct.md` by writing a real Stop-hook handler, dumping payloads, and answering the three questions. Commit the decision doc before any other work.
2. **Week 1 — CLI scaffold.** `fleet dispatch`, `fleet attach`, `fleet status`. Add cobra. Spawn Claude in a detached tmux session and write a stub agent record to `~/.fleet/agents/`.
3. **Weeks 2-3 — TUI.** bubbletea + lipgloss. Read agent JSON via fsnotify. Polling fallback at 1s.
4. **Week 4 — Handoffs.** Integrate `fleet-guard` into the spawn flow. Trigger queue + restart-on-handoff. Reconciliation for orphaned tasks.
5. **Week 5 — Release.** GoReleaser, brew tap, demo gif.
6. **Week 6 — Dogfood.** Use Fleet to build Fleet for one full week.
7. **Week 7 — Launch.** Show HN + tweet.

## Engineering preferences

- Boring tech by default (bubbletea, cobra, fsnotify, GoReleaser — already in design). Innovation tokens go to agent-health-as-primitive only.
- Filesystem state must survive crashes. Atomic writes (write to `.tmp` then rename), fsync before signaling via queue files.
- Tests come with the feature, not after. CI (`.github/workflows/ci.yml`) runs `go build ./...`, `go test ./...`, and `golangci-lint run ./...` on every PR and push to main (separate from the release-on-tag CI). Local lint: `golangci-lint run ./...` (config at `.golangci.yml`).
- Single-binary distribution. No runtime deps beyond tmux.

## House style

- Terse commits. `fix(handoff): clear assigned field on archive`.
- ASCII diagrams in code comments for non-obvious flows (handoff sequence, reconciliation, fsnotify fan-out).
- No premature abstractions. Three similar lines is better than a generic helper that takes nine arguments.

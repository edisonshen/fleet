# Fleet

> **Everyone is a manager.**

An open-source command console for running many Claude Code agents in parallel. One operator, many concurrent agents across many repos, one TUI to keep them all productive.

## Status

Pre-v0.1. Week 0 feasibility spike is gating — see [`docs/SPIKE-context-pct.md`](docs/SPIKE-context-pct.md).

The full design lives at [`docs/DESIGN.md`](docs/DESIGN.md).

## Why

The bottleneck running multiple Claude Code agents is not Claude. It is the operator. Every context switch costs minutes of re-onboarding. Run four agents naively and you spend more time re-engaging than supervising.

Fleet treats supervision capacity as the constraint and optimizes everything around it: a single TUI shows every agent's health (context %, last activity, blocked state), automatic handoffs prevent context-degraded decisions, and a structured handoff format keeps the next agent productive within seconds.

## What ships in v1

Three pillars launched together (big-bang launch — the parallelism demo only carries with the full picture):

1. **Fleet view** — TUI showing every agent across every project with health badges.
2. **Deploy/attach/peek/message** — full operator → agent communication surface, including async messages that don't interrupt.
3. **Context-guard** — `fleet-guard` Claude Code skill that watches context % and triggers structured handoffs at 50%/75% thresholds.

## Install

Not yet released. After v0.1:

```sh
brew install edisonshen/tap/fleet
```

## License

MIT — see [LICENSE](LICENSE).

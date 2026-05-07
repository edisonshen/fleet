---
schema: v1
---

# Standards

## Testing

- TDD required: a failing test on disk before the implementation.
- Tests use stdlib `testing` only — no testify, no ginkgo.
- All bug fixes carry a regression test that fails on the parent commit.
- Integration tests preferred over heavy mocking when feasible.

## Code review

- Reviewers must be ruthless; no rubber stamps.
- Run /review and /codex review; fix every P0/P1 finding before merging.
- Both reviewers report clean OR every flagged item carries a documented "wontfix" rationale.

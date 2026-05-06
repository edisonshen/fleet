---
schema: v1
---

## task: noisy-9999

- status: in-progress
- priority: P2
- worker_pid: 4242
- worktree: /tmp/noisy
- pr_url:
- branch: worker/noisy
- created: 2026-05-06T09:00:00Z
- updated: 2026-05-06T11:30:00Z
- depends_on: []
- spawned_by: user

### Spec

Worker writes multi-paragraph notes that must round-trip verbatim.

### Acceptance

Notes preserved.

### Notes

First paragraph from worker. Says it tried approach A, didn't work
because of caching.

Second paragraph: switched to approach B, succeeded after fixing
the test fixture.

- bullet one
- bullet two

Third paragraph wraps up. Triple-blank lines below should be
collapsed by no parser; they round-trip verbatim.



End of notes.


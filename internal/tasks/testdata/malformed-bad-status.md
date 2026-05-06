---
schema: v1
---

## task: bad-status-0000

- status: not-a-real-status
- priority: P1
- worker_pid: 0
- worktree:
- pr_url:
- branch:
- created: 2026-05-06T10:00:00Z
- updated: 2026-05-06T10:00:00Z
- depends_on: []
- spawned_by: user

### Spec

Bad status enum should fail parse.

### Acceptance

Parser refuses.

### Notes


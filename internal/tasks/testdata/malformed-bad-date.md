---
schema: v1
---

## task: bad-date-0000

- status: todo
- priority: P1
- worker_pid: 0
- worktree:
- pr_url:
- branch:
- created: NOT-A-DATE
- updated: 2026-05-06T10:00:00Z
- depends_on: []
- spawned_by: user

### Spec

Bad created date should fail parse with line+col.

### Acceptance

Parser refuses.

### Notes


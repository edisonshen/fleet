---
schema: v1
---

## task: foundation-aaaa

- status: ready
- priority: P0
- worker_pid: 0
- worktree:
- pr_url:
- branch:
- created: 2026-05-01T10:00:00Z
- updated: 2026-05-01T10:00:00Z
- depends_on: []
- spawned_by: user

### Spec

Foundation task.

### Acceptance

Foundation done.

### Notes


## task: middle-bbbb

- status: todo
- priority: P1
- worker_pid: 0
- worktree:
- pr_url:
- branch:
- created: 2026-05-02T10:00:00Z
- updated: 2026-05-02T10:00:00Z
- depends_on: [foundation-aaaa]
- spawned_by: user

### Spec

Middle task; depends on foundation.

### Acceptance

Middle done.

### Notes


## task: top-cccc

- status: todo
- priority: P1
- worker_pid: 0
- worktree:
- pr_url:
- branch:
- created: 2026-05-03T10:00:00Z
- updated: 2026-05-03T10:00:00Z
- depends_on: [foundation-aaaa, middle-bbbb]
- spawned_by: user

### Spec

Top task; depends on foundation and middle.

### Acceptance

Top done.

### Notes


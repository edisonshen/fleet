"""ID allocation matching internal/agent.NewID (Go).

8 lowercase hex characters from 4 random bytes. Collision probability is
negligible at v1's 1-20 concurrent agent ceiling. Pre-allocated by the skill
before queueing a spawn-fresh request so crash recovery (cmd/fleet/handoff.go
iter-13) can identify the successor on retry.
"""
from __future__ import annotations

import secrets


def new_id() -> str:
    return secrets.token_hex(4)

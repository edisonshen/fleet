"""Tests for clearing the coord-spawn pending claim on tick.

The Go dispatch path writes a durable pending-spawn claim
(~/.fleet/projects/<p>/coord-spawn-pending) under the coord-spawn flock
before spawning, so a second cold-start dispatch is vetoed during the
3-5 min window before coord-state.json exists. The coord's tick must
clear that claim once it starts ticking, so the veto switches to the
live-record / coord-state path and a legitimately-needed respawn isn't
blocked forever.

T5 (Python half): clear_coord_spawn_pending removes the claim file and
is idempotent (clearing an absent claim is a no-op).
"""
from __future__ import annotations

from pathlib import Path

import loop


def test_clear_coord_spawn_pending_removes_file(tmp_path: Path) -> None:
    project_dir = tmp_path / "projects" / "fleet"
    project_dir.mkdir(parents=True)
    claim = project_dir / "coord-spawn-pending"
    claim.write_text('{"agent_id": "abc", "spawned_at": "2026-06-06T00:00:00Z"}')

    loop.clear_coord_spawn_pending(project_dir)

    assert not claim.exists(), "pending claim must be removed by clear"


def test_clear_coord_spawn_pending_idempotent(tmp_path: Path) -> None:
    project_dir = tmp_path / "projects" / "fleet"
    project_dir.mkdir(parents=True)
    # No claim file present.
    # Must not raise.
    loop.clear_coord_spawn_pending(project_dir)
    assert not (project_dir / "coord-spawn-pending").exists()

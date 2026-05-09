"""Auto-archive tests for the coordinator tick.

When tasks.md grows past FLEET_AUTO_ARCHIVE_THRESHOLD (default 50)
the coord shells `fleet tasks archive` for the OLDEST done/abandoned
slugs (sort: finished_at asc, fall back to updated asc) until the
count drops back to or below the threshold.

Active statuses are NEVER archived regardless of count.
"""
from __future__ import annotations

import datetime as _dt
from pathlib import Path
from unittest.mock import patch

import pytest

import loop
import parse


# ---------- helpers ----------


def _mk(
    slug: str,
    *,
    status: str = "done",
    priority: str = "P2",
    finished_at: _dt.datetime | None = None,
    updated: _dt.datetime | None = None,
    created: _dt.datetime | None = None,
) -> parse.Task:
    """Build a Task with explicit lifecycle stamps."""
    base = _dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc)
    if updated is None:
        updated = base
    if created is None:
        created = base
    return parse.Task(
        slug=slug,
        status=status,
        priority=priority,
        worker_pid=0,
        worktree="",
        pr_url="",
        branch="",
        created=created,
        updated=updated,
        started_at=None,
        finished_at=finished_at,
        depends_on=[],
        spawned_by="user",
        spec="s",
        acceptance="a",
        notes="",
    )


def _write_tasks(project_dir: Path, tasks: list[parse.Task]) -> None:
    project_dir.mkdir(parents=True, exist_ok=True)
    (project_dir / ".locks").mkdir(exist_ok=True)
    f = parse.File(schema=parse.SCHEMA_VERSION, tasks=tasks)
    parse.write(str(project_dir / "tasks.md"), f)


@pytest.fixture
def fleet_home(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    home.mkdir()
    (home / "inbox").mkdir()
    (home / "inbox" / "archive").mkdir()
    (home / "projects").mkdir()
    return home


@pytest.fixture
def project_dir(fleet_home: Path) -> Path:
    p = fleet_home / "projects" / "fleet"
    p.mkdir(parents=True, exist_ok=True)
    (p / ".locks").mkdir(exist_ok=True)
    return p


@pytest.fixture
def fleet_run_recorder():
    """Record every loop._run_fleet call. Returns the calls list."""
    calls: list[list[str]] = []

    def fake_run(cmd, timeout_s=30.0):
        calls.append(list(cmd))

    with patch.object(loop, "_run_fleet", side_effect=fake_run):
        yield calls


# ---------- tests ----------


def test_threshold_default_is_fifty(monkeypatch) -> None:
    """Empty / unset env → default threshold 50."""
    monkeypatch.delenv("FLEET_AUTO_ARCHIVE_THRESHOLD", raising=False)
    assert loop._auto_archive_threshold() == 50


def test_threshold_zero_disables(monkeypatch) -> None:
    monkeypatch.setenv("FLEET_AUTO_ARCHIVE_THRESHOLD", "0")
    assert loop._auto_archive_threshold() == -1


def test_threshold_custom(monkeypatch) -> None:
    monkeypatch.setenv("FLEET_AUTO_ARCHIVE_THRESHOLD", "20")
    assert loop._auto_archive_threshold() == 20


def test_threshold_garbage_falls_back(monkeypatch) -> None:
    monkeypatch.setenv("FLEET_AUTO_ARCHIVE_THRESHOLD", "not-a-number")
    assert loop._auto_archive_threshold() == 50


def test_no_archive_when_below_threshold(
    project_dir: Path, fleet_run_recorder, monkeypatch,
) -> None:
    """49 rows < 50 threshold → no archive shells."""
    monkeypatch.delenv("FLEET_AUTO_ARCHIVE_THRESHOLD", raising=False)
    rows = [_mk(f"slug-{i:03d}") for i in range(49)]
    _write_tasks(project_dir, rows)

    result = loop._maybe_auto_archive(
        project_dir / "tasks.md", "fleet", "fleet",
    )
    assert result is None  # no work done
    archive_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "archive"]]
    assert archive_calls == []


def test_archive_picks_oldest_finished_first(
    project_dir: Path, fleet_run_recorder, monkeypatch,
) -> None:
    """51 done rows → archive the OLDEST one until count = 50.

    Sort key is finished_at asc — slug-000's finished_at is min, so it
    archives first.
    """
    monkeypatch.delenv("FLEET_AUTO_ARCHIVE_THRESHOLD", raising=False)
    base = _dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc)
    rows = [
        _mk(f"slug-{i:03d}", finished_at=base + _dt.timedelta(hours=i))
        for i in range(51)
    ]
    _write_tasks(project_dir, rows)

    result = loop._maybe_auto_archive(
        project_dir / "tasks.md", "fleet", "fleet",
    )
    assert result is not None
    assert result.archived == 1
    archive_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "archive"]]
    assert len(archive_calls) == 1
    # Last positional is the slug
    assert archive_calls[0][-1] == "slug-000"


def test_archive_skips_active(
    project_dir: Path, fleet_run_recorder, monkeypatch,
) -> None:
    """50 active + 1 active (51 total) → no archive (no candidates).

    Brief: "Never archive non-terminal statuses regardless of count."
    """
    monkeypatch.delenv("FLEET_AUTO_ARCHIVE_THRESHOLD", raising=False)
    rows = [_mk(f"todo-{i:03d}", status="todo") for i in range(51)]
    _write_tasks(project_dir, rows)

    result = loop._maybe_auto_archive(
        project_dir / "tasks.md", "fleet", "fleet",
    )
    # Result is non-None but archived count is 0; the warning string
    # surfaces on result.errors.
    assert result is not None
    assert result.archived == 0
    assert any("no done/abandoned candidates" in e for e in result.errors)
    archive_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "archive"]]
    assert archive_calls == []


def test_archive_threshold_env_override(
    project_dir: Path, fleet_run_recorder, monkeypatch,
) -> None:
    """Setting threshold=10 fires archives at 11 entries."""
    monkeypatch.setenv("FLEET_AUTO_ARCHIVE_THRESHOLD", "10")
    base = _dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc)
    # 11 done rows; threshold=10 → archive ONE.
    rows = [
        _mk(f"slug-{i:03d}", finished_at=base + _dt.timedelta(hours=i))
        for i in range(11)
    ]
    _write_tasks(project_dir, rows)

    result = loop._maybe_auto_archive(
        project_dir / "tasks.md", "fleet", "fleet",
    )
    assert result.archived == 1
    archive_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "archive"]]
    assert len(archive_calls) == 1


def test_archive_disabled(
    project_dir: Path, fleet_run_recorder, monkeypatch,
) -> None:
    """FLEET_AUTO_ARCHIVE_THRESHOLD=0 disables auto-archive entirely."""
    monkeypatch.setenv("FLEET_AUTO_ARCHIVE_THRESHOLD", "0")
    rows = [_mk(f"slug-{i:03d}") for i in range(60)]
    _write_tasks(project_dir, rows)

    result = loop._maybe_auto_archive(
        project_dir / "tasks.md", "fleet", "fleet",
    )
    assert result is None  # disabled
    archive_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "archive"]]
    assert archive_calls == []


def test_archive_idempotent_on_second_run(
    project_dir: Path, fleet_run_recorder, monkeypatch,
) -> None:
    """Running twice without growing tasks.md is a no-op the second time.

    The fake _run_fleet doesn't actually mutate tasks.md, so this test
    spoofs the second run by mutating tasks.md before re-entering.
    """
    monkeypatch.delenv("FLEET_AUTO_ARCHIVE_THRESHOLD", raising=False)
    base = _dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc)
    rows = [
        _mk(f"slug-{i:03d}", finished_at=base + _dt.timedelta(hours=i))
        for i in range(51)
    ]
    _write_tasks(project_dir, rows)

    result1 = loop._maybe_auto_archive(
        project_dir / "tasks.md", "fleet", "fleet",
    )
    assert result1.archived == 1

    # Spoof: archive succeeded, drop the slug from tasks.md so the
    # second pass sees a 50-row file (at threshold). Auto-archive must
    # then no-op.
    rows_after = [r for r in rows if r.slug != "slug-000"]
    _write_tasks(project_dir, rows_after)
    fleet_run_recorder.clear()
    result2 = loop._maybe_auto_archive(
        project_dir / "tasks.md", "fleet", "fleet",
    )
    assert result2 is None
    archive_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "archive"]]
    assert archive_calls == []


def test_archive_finished_at_fallback_to_updated(
    project_dir: Path, fleet_run_recorder, monkeypatch,
) -> None:
    """Old rows missing finished_at use coalesce(updated, created) as rank.

    Two rows:
      old-row  finished_at=None,  updated=10:00  → rank=10:00
      new-row  finished_at=11:00, updated=12:00  → rank=11:00

    old-row's effective rank (10:00) is older than new-row's (11:00),
    so old-row archives first. Threshold=1 → archive one row total.
    """
    monkeypatch.setenv("FLEET_AUTO_ARCHIVE_THRESHOLD", "1")
    t10 = _dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc)
    t11 = _dt.datetime(2026, 5, 6, 11, 0, 0, tzinfo=_dt.timezone.utc)
    t12 = _dt.datetime(2026, 5, 6, 12, 0, 0, tzinfo=_dt.timezone.utc)
    rows = [
        _mk("old-row", finished_at=None, updated=t10),
        _mk("new-row", finished_at=t11, updated=t12),
    ]
    _write_tasks(project_dir, rows)

    result = loop._maybe_auto_archive(
        project_dir / "tasks.md", "fleet", "fleet",
    )
    assert result.archived == 1
    archive_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "archive"]]
    assert archive_calls[0][-1] == "old-row"


def test_archive_coalesce_keeps_recent_legacy_row_when_done_is_older(
    project_dir: Path, fleet_run_recorder, monkeypatch,
) -> None:
    """Coalesce semantics regress test: a legacy row (no finished_at)
    with a RECENT updated outranks a stamped row with an OLDER
    finished_at — so the stamped row archives first.

    Without coalesce, the legacy row would archive first (None sentinel
    < any real timestamp), which is WRONG: an old finished_at means
    the row finished a long time ago and should be archived, not the
    legacy row that was touched yesterday.

    Layout:
      legacy-row  finished_at=None,  updated=2030 (recent) → rank=2030
      stamped-row finished_at=2020 (old)                   → rank=2020

    Threshold=1 → archive ONE row. Coalesce-correct: stamped-row goes.
    """
    monkeypatch.setenv("FLEET_AUTO_ARCHIVE_THRESHOLD", "1")
    old = _dt.datetime(2020, 1, 1, 10, 0, 0, tzinfo=_dt.timezone.utc)
    recent = _dt.datetime(2030, 1, 1, 10, 0, 0, tzinfo=_dt.timezone.utc)
    rows = [
        _mk("legacy-row", finished_at=None, updated=recent, created=old),
        _mk("stamped-row", finished_at=old, updated=old, created=old),
    ]
    _write_tasks(project_dir, rows)

    result = loop._maybe_auto_archive(
        project_dir / "tasks.md", "fleet", "fleet",
    )
    assert result.archived == 1
    archive_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "archive"]]
    assert archive_calls[0][-1] == "stamped-row", (
        f"coalesce regress: expected stamped-row (oldest by rank), got "
        f"{archive_calls[0][-1]}"
    )


def test_archive_passes_project_flag(
    project_dir: Path, fleet_run_recorder, monkeypatch,
) -> None:
    """Each `fleet tasks archive` shell carries `--project <name>` so
    the worker doesn't fall back to the cwd-derived project tag."""
    monkeypatch.setenv("FLEET_AUTO_ARCHIVE_THRESHOLD", "10")
    base = _dt.datetime(2026, 5, 6, 10, 0, 0, tzinfo=_dt.timezone.utc)
    rows = [
        _mk(f"slug-{i:03d}", finished_at=base + _dt.timedelta(hours=i))
        for i in range(11)
    ]
    _write_tasks(project_dir, rows)

    loop._maybe_auto_archive(project_dir / "tasks.md", "myproj", "fleet")
    archive_calls = [c for c in fleet_run_recorder if c[1:3] == ["tasks", "archive"]]
    assert len(archive_calls) == 1
    cmd = archive_calls[0]
    assert "--project" in cmd
    assert cmd[cmd.index("--project") + 1] == "myproj"

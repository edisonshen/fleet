"""Project-ownership guard for loop.tick() — fleet#171.

A coord agent's authoritative project owner is its agent record at
`~/.fleet/agents/$FLEET_AGENT_ID.json`. When `loop.tick(project=X,
coord_id=A)` is invoked but the agent record for A says it owns
project Y, the tick MUST refuse — surface a concrete stderr
diagnostic AND populate `TickResult.skipped=True` /
`TickResult.reason="project-ownership-mismatch"` BEFORE acquiring
the project lock or writing any state. The lock file must NOT carry
the foreign agent_id.

This mirrors the `feedback_coord_scope_strict` operator rule: derive
project from the agent record, never from cwd/argv. Without this
guard, `loop.py fleet` invoked from inside a non-fleet coord's
Claude session silently hijacks `~/.fleet/projects/fleet/.locks/
coordinator.lock` — observed live with agent 922e7c7d (projects-spark
coord) corrupting the fleet project's lock content.

Edge cases (per fleet#171 acceptance):

  1. Mismatch (agent.project=A, argv=B) → skipped + reason +
     stderr diagnostic + mismatch entry in `result.errors`; lock
     file content NOT mutated.
  2. Match (agent.project=A, argv=A) → proceeds normally.
  3. Empty `coord_id` (no `FLEET_AGENT_ID`, manual operator invocation
     from a shell) → allow. Backwards-compat for diagnostic uses.
  4. Missing agent record → warning written to stderr (NOT
     result.errors — keeping that surface clean keeps the TUI from
     alerting on every legacy/upgrade tick); allow.
  5. Empty `rec.project` field → allow. Same legacy reason.
  6. Unparseable agent record → same as missing.

Implementation note: the guard runs BEFORE `_try_lock`, so a
mismatched call never opens the lock file at all. Tests assert this
by checking that the lock file either doesn't exist or, if it does
exist (because the test pre-seeded a different owner), the on-disk
holder is still the pre-test value.
"""
from __future__ import annotations

import json
from pathlib import Path

import pytest

import loop


# ---------- helpers ----------


def _seed_agent_record(home: Path, agent_id: str, project: str) -> Path:
    """Drop an agent JSON record at ~/.fleet/agents/<id>.json."""
    agents_dir = home / "agents"
    agents_dir.mkdir(parents=True, exist_ok=True)
    rec_path = agents_dir / f"{agent_id}.json"
    rec_path.write_text(json.dumps({
        "id": agent_id,
        "project": project,
        "kind": "coord",
    }))
    return rec_path


def _minimal_project(home: Path, project: str) -> Path:
    """Create a project dir with a minimal valid tasks.md so tick()
    would otherwise succeed (no parse error). The guard must fire
    before this is even read on the mismatch path; we still need it
    on the match path so tick() can proceed and not error out for
    unrelated reasons."""
    pdir = home / "projects" / project
    pdir.mkdir(parents=True, exist_ok=True)
    (pdir / ".locks").mkdir(exist_ok=True)
    # Minimal tasks.md with current schema header. parse.SCHEMA_VERSION
    # is the source of truth; we read it via the module so the fixture
    # stays in sync with schema bumps.
    import parse
    f = parse.File(schema=parse.SCHEMA_VERSION, tasks=[], footer="")
    parse.write(str(pdir / "tasks.md"), f)
    return pdir


@pytest.fixture
def fleet_home(tmp_path: Path) -> Path:
    home = tmp_path / "fleet"
    home.mkdir()
    (home / "inbox").mkdir()
    (home / "inbox" / "archive").mkdir()
    (home / "projects").mkdir()
    (home / "agents").mkdir()
    return home


# ---------- mismatch path ----------


def test_mismatch_skips_and_surfaces_reason(
    fleet_home: Path, capsys: pytest.CaptureFixture[str],
) -> None:
    """Agent record says project=A, tick called with project=B.
    Expected: result.skipped, reason=project-ownership-mismatch,
    stderr diagnostic written, lock file NOT mutated to carry
    the foreign agent_id."""
    agent_id = "aaaa1111"
    _seed_agent_record(fleet_home, agent_id, project="projects-spark")
    # Pre-create the foreign project dir so we can assert the lock
    # file content didn't get our agent_id written into it.
    foreign_pdir = _minimal_project(fleet_home, project="fleet")
    foreign_lock = foreign_pdir / ".locks" / "coordinator.lock"
    # Pre-seed the lock with a sentinel string so we can prove the
    # guard fired BEFORE _try_lock ever ran (which would have
    # overwritten with the foreign coord_id).
    foreign_lock.write_text("PRE_EXISTING_OWNER_DO_NOT_CLOBBER")

    result = loop.tick(
        project="fleet",
        coord_id=agent_id,
        cwd="/tmp/anywhere",
        fleet_home=str(fleet_home),
    )

    assert result.skipped is True, (
        "mismatch must skip the tick; got skipped=False"
    )
    assert result.reason == "project-ownership-mismatch", (
        f"reason should pin the mismatch; got {result.reason!r}"
    )
    # Diagnostic landed in result.errors so callers that swallow
    # stderr still see it.
    joined_errs = "\n".join(result.errors)
    assert "refusing to operate on" in joined_errs, (
        f"expected refusal phrase in errors; got {result.errors!r}"
    )
    assert agent_id in joined_errs, (
        f"diagnostic must name the offending agent_id {agent_id!r}; "
        f"got {result.errors!r}"
    )
    assert "projects-spark" in joined_errs, (
        f"diagnostic must name the agent's real project; "
        f"got {result.errors!r}"
    )
    assert "fleet" in joined_errs, (
        f"diagnostic must name the foreign project; "
        f"got {result.errors!r}"
    )
    # stderr diagnostic also present (operator running loop.py from a
    # shell needs to see it without parsing TickResult).
    captured = capsys.readouterr()
    assert "refusing to operate on" in captured.err, (
        f"stderr must carry the refusal; got err={captured.err!r}"
    )
    # Lock file content untouched — the guard fired BEFORE _try_lock.
    assert foreign_lock.read_text() == "PRE_EXISTING_OWNER_DO_NOT_CLOBBER", (
        "guard must run BEFORE _try_lock so the lock content is not "
        "overwritten by the foreign agent_id"
    )


# ---------- match path ----------


def test_match_proceeds_normally(
    fleet_home: Path,
) -> None:
    """Agent record says project=A, tick called with project=A.
    Expected: tick is NOT skipped for ownership-mismatch reasons.

    (It may still skip for other reasons like an empty tasks.md
    yielding zero work, but reason must not be project-ownership-
    mismatch.)"""
    agent_id = "bbbb2222"
    _seed_agent_record(fleet_home, agent_id, project="fleet")
    _minimal_project(fleet_home, project="fleet")

    result = loop.tick(
        project="fleet",
        coord_id=agent_id,
        cwd="/tmp/anywhere",
        fleet_home=str(fleet_home),
    )

    # The guard must NOT fire. result may still skip for other
    # reasons (e.g., remote_control bootstrap failure inside an
    # isolated tmp_path), so we assert specifically on the reason
    # rather than result.skipped.
    assert result.reason != "project-ownership-mismatch", (
        f"match path must not trigger ownership guard; got "
        f"reason={result.reason!r}"
    )


# ---------- empty agent_id path (operator shell) ----------


def test_empty_agent_id_allows(
    fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    """No FLEET_AGENT_ID env, no explicit coord_id. Operator running
    `python3 loop.py myproj` from a shell for diagnostics. Guard
    must allow — there's no record to compare against and refusing
    would break manual use."""
    monkeypatch.delenv("FLEET_AGENT_ID", raising=False)
    _minimal_project(fleet_home, project="fleet")

    result = loop.tick(
        project="fleet",
        coord_id="",
        cwd="/tmp/anywhere",
        fleet_home=str(fleet_home),
    )

    assert result.reason != "project-ownership-mismatch", (
        f"empty coord_id must NOT trigger ownership guard; got "
        f"reason={result.reason!r}"
    )


# ---------- missing record path (legacy pre-schema) ----------


def test_missing_record_warns_and_allows(
    fleet_home: Path, capsys: pytest.CaptureFixture[str],
) -> None:
    """coord_id provided but no agent record on disk. Legacy
    pre-schema records can be missing during upgrades; refusing
    would brick the upgrade path. Expected: warning written to
    stderr (NOT result.errors — that surface drives TUI alerts),
    tick proceeds (reason != mismatch)."""
    agent_id = "cccc3333"
    # Deliberately do NOT seed the agent record.
    _minimal_project(fleet_home, project="fleet")

    result = loop.tick(
        project="fleet",
        coord_id=agent_id,
        cwd="/tmp/anywhere",
        fleet_home=str(fleet_home),
    )

    assert result.reason != "project-ownership-mismatch", (
        f"missing record must NOT trigger ownership guard; got "
        f"reason={result.reason!r}"
    )
    # Warning surfaced on stderr (NOT result.errors — leaving
    # result.errors clean keeps the TUI from flagging every
    # legacy/upgrade-path tick as a tick error).
    captured = capsys.readouterr()
    assert agent_id in captured.err, (
        f"warning must name the agent_id {agent_id!r} on stderr; "
        f"got err={captured.err!r}"
    )
    assert "project-ownership guard" in captured.err, (
        f"warning must label itself as the ownership guard; "
        f"got err={captured.err!r}"
    )


# ---------- empty rec.project path (legacy schema) ----------


def test_empty_record_project_allows(
    fleet_home: Path,
) -> None:
    """Agent record exists but `project` field is empty (older
    schema, or coord that hasn't been re-registered post-rule).
    Guard must allow — refusing would block upgrade."""
    agent_id = "dddd4444"
    agents_dir = fleet_home / "agents"
    agents_dir.mkdir(parents=True, exist_ok=True)
    (agents_dir / f"{agent_id}.json").write_text(json.dumps({
        "id": agent_id,
        "project": "",
        "kind": "coord",
    }))
    _minimal_project(fleet_home, project="fleet")

    result = loop.tick(
        project="fleet",
        coord_id=agent_id,
        cwd="/tmp/anywhere",
        fleet_home=str(fleet_home),
    )

    assert result.reason != "project-ownership-mismatch", (
        f"empty rec.project must NOT trigger ownership guard; got "
        f"reason={result.reason!r}"
    )


# ---------- unparseable record path (corrupt JSON) ----------


def test_unparseable_record_warns_and_allows(
    fleet_home: Path, capsys: pytest.CaptureFixture[str],
) -> None:
    """Agent record exists but JSON is corrupt. Treat as missing —
    legacy data corruption (operator-edit, partial write, etc.)
    shouldn't brick the coord. Warning lands on stderr, result.errors
    stays clean (same TUI-friendly rationale as the missing path)."""
    agent_id = "eeee5555"
    agents_dir = fleet_home / "agents"
    agents_dir.mkdir(parents=True, exist_ok=True)
    (agents_dir / f"{agent_id}.json").write_text("{not valid json")
    _minimal_project(fleet_home, project="fleet")

    result = loop.tick(
        project="fleet",
        coord_id=agent_id,
        cwd="/tmp/anywhere",
        fleet_home=str(fleet_home),
    )

    assert result.reason != "project-ownership-mismatch", (
        f"unparseable record must NOT trigger ownership guard; got "
        f"reason={result.reason!r}"
    )
    captured = capsys.readouterr()
    assert agent_id in captured.err, (
        f"warning must name the agent_id {agent_id!r} on stderr; "
        f"got err={captured.err!r}"
    )
    assert "project-ownership guard" in captured.err, (
        f"warning must label itself as the ownership guard; "
        f"got err={captured.err!r}"
    )

"""loop.py forwards the fence branch tag + epoch snapshot into fleetlog
(DESIGN-coord-lease-false-fence-prevention piece 2 /
TASK-PLAN-lease-observability-8c4e).

5a90 made `fleet lease-check`'s exit-3 stderr carry WHICH branch fenced; this
task appends the fence-time epoch snapshot and threads both out through
`_lease_check_fn`'s return contract into the coord.tick skipped event's data,
so a stalled-renewal incident is diagnosable from logs alone.

  parse   — _parse_fence_detail pulls (tag, snapshot) from real lease-check
            stderr; a bare/internal-error stderr yields empty best-effort
            fields.
  fwd/pre  — a fenced tick at the pre-lock proof forwards fence_tag +
             epoch_snapshot into the fleetlog coord.tick event.
  fwd/post — same forwarding when the fence fires at the post-lock re-proof.
"""
from __future__ import annotations

import importlib
import json
from pathlib import Path

import pytest

import loop
import parse


@pytest.fixture
def home(tmp_path, monkeypatch):
    h = tmp_path / "fleet"
    for sub in ("inbox/archive", "agents", "queue", "logs", "projects"):
        (h / sub).mkdir(parents=True, exist_ok=True)
    monkeypatch.setenv("FLEET_HOME", str(h))
    monkeypatch.delenv("XDG_STATE_HOME", raising=False)
    monkeypatch.delenv("FLEET_AGENT_ID", raising=False)
    import fleetlog
    importlib.reload(fleetlog)
    return h


def _minimal_project(home: Path, project: str = "fleet") -> Path:
    pdir = home / "projects" / project
    pdir.mkdir(parents=True, exist_ok=True)
    (pdir / ".locks").mkdir(exist_ok=True)
    parse.write(str(pdir / "tasks.md"),
                parse.File(schema=parse.SCHEMA_VERSION, tasks=[], footer=""))
    return pdir


def _tick_events(home: Path, evt: str = "coord.tick") -> list[dict]:
    events = []
    for f in sorted((home / "logs").glob("*.jsonl")):
        for ln in f.read_text().splitlines():
            if ln.strip():
                m = json.loads(ln)
                if m.get("type") == evt:
                    events.append(m)
    return events


# A realistic exit-3 stderr line the Go binary emits for the own-expired-rival
# branch, with the appended epoch snapshot.
_SNAP = ('{"boot_id":"test-boot-1","epoch":5,"owner_pid":500,'
         '"renewed_at_mono":0,"renewed_at_wall":1234567890,"state":"fencing"}')
_REFUSE = (
    "lease-check: REFUSE: coordlock: caller is not under the active lease "
    "owner (fenced/stale coord) — refuse mutation: own-expired-rival-fenced: "
    "a takeover rival exists for our expired lease (state=fencing, candidate "
    f"pid=800) [epoch-snapshot {_SNAP}]"
)


# ---------- parse ----------


def test_parse_fence_detail_extracts_tag_and_snapshot():
    tag, snapshot = loop._parse_fence_detail(_REFUSE)
    assert tag == "own-expired-rival-fenced", f"got tag {tag!r}"
    assert json.loads(snapshot)["epoch"] == 5, f"snapshot not parseable: {snapshot!r}"


def test_parse_fence_detail_best_effort_on_bare_stderr():
    # The fail-CLOSED exit-1 path has no branch tag / snapshot — both empty,
    # never an exception.
    tag, snapshot = loop._parse_fence_detail("lease-check: some internal error\n")
    assert tag == ""
    assert snapshot == ""


# ---------- forwarding: pre-lock ----------


def test_prelock_fence_forwards_detail_to_fleetlog(home, monkeypatch):
    _minimal_project(home)
    monkeypatch.setattr(
        loop, "_lease_check_fn",
        lambda project, *, home, fleet_bin="fleet": loop.LeaseCheckResult(
            "fenced", tag="own-expired-rival-fenced", snapshot=_SNAP),
    )

    result = loop.tick(project="fleet", coord_id="feedface",
                       cwd=str(home / "projects" / "fleet"), fleet_home=str(home))

    assert result.reason == "lease-fenced"
    evts = [e for e in _tick_events(home)
            if e.get("data", {}).get("reason") == "lease-fenced"]
    assert evts, "expected a lease-fenced coord.tick fleetlog event"
    data = evts[0]["data"]
    assert data.get("fence_tag") == "own-expired-rival-fenced", data
    assert json.loads(data.get("epoch_snapshot", "{}"))["epoch"] == 5, data


# ---------- forwarding: post-lock ----------


def test_postlock_fence_forwards_detail_to_fleetlog(home, monkeypatch):
    _minimal_project(home)
    calls = {"n": 0}

    def _fence_second(project, *, home, fleet_bin="fleet"):
        calls["n"] += 1
        if calls["n"] == 1:
            return loop.LeaseCheckResult("owner")
        return loop.LeaseCheckResult(
            "fenced", tag="different-owner-fenced", snapshot=_SNAP)

    monkeypatch.setattr(loop, "_lease_check_fn", _fence_second)

    result = loop.tick(project="fleet", coord_id="feedface",
                       cwd=str(home / "projects" / "fleet"), fleet_home=str(home))

    assert result.reason == "lease-fenced"
    evts = [e for e in _tick_events(home)
            if e.get("data", {}).get("phase") == "post-lock"]
    assert evts, "expected a post-lock lease-fenced coord.tick fleetlog event"
    data = evts[0]["data"]
    assert data.get("fence_tag") == "different-owner-fenced", data
    assert json.loads(data.get("epoch_snapshot", "{}"))["epoch"] == 5, data

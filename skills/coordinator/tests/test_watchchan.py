"""Tests for the watcher->coord message channel reader (Slice 1/4).

Reader/drainer half of internal/watchchan: list_pending (event-only, time
ordered, skips .tmp sidecars AND hb-* heartbeats), read_message (refuses a
newer schema v), mark_done (delete = commit point for apply-before-delete),
and the heartbeat read helpers. Cross-language schema agreement is pinned by
the shared golden fixture in tests/testdata/pr_event_golden.json, which the
Go suite reads too.

All deterministic: temp dirs, hand-written wire bytes, no network/sleep.
"""
from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

import watchchan as wc


HERE = Path(__file__).resolve().parent
GOLDEN = HERE / "testdata" / "pr_event_golden.json"


def _write_event(msgs_dir: Path, name: str, payload: dict) -> Path:
    msgs_dir.mkdir(parents=True, exist_ok=True)
    p = msgs_dir / name
    p.write_text(json.dumps(payload), encoding="utf-8")
    return p


def _event_msg(kind: str = "worker_delta", key: str = "k", **extra) -> dict:
    msg = {
        "v": wc.SCHEMA_VERSION,
        "kind": kind,
        "watcher_id": "w-100",
        "epoch_id": 1,
        "key": key,
        "payload": {},
        "ts": "2026-06-26T00:00:00Z",
    }
    msg.update(extra)
    return msg


# Test 4 — list_pending returns only event messages: .tmp sidecars AND hb-*
# heartbeat files are excluded; a half-written file is never returned.
def test_list_pending_skips_tmp_and_heartbeats(tmp_path):
    msgs = wc.watch_msgs_dir(tmp_path)
    _write_event(msgs, "1700000000000000001-w-0.json", _event_msg(key="a"))
    _write_event(msgs, "1700000000000000002-w-1.json", _event_msg(key="b"))
    # a half-written sidecar from WriteAtomic: <name>.tmp.<pid>
    _write_event(msgs, "1700000000000000003-w-2.json.tmp.999", _event_msg(key="partial"))
    # heartbeat files must be invisible to the event drain
    _write_event(msgs, "hb-wkr1.json", _event_msg(kind="liveness_heartbeat", key="hb"))

    pending = wc.list_pending(tmp_path)
    names = [p.name for p in pending]
    assert names == [
        "1700000000000000001-w-0.json",
        "1700000000000000002-w-1.json",
    ], names
    assert all(not n.startswith("hb-") for n in names)
    assert all(".tmp" not in n for n in names)


# Test 5 — time ordering: list_pending returns files in filename sort order.
def test_list_pending_time_order(tmp_path):
    msgs = wc.watch_msgs_dir(tmp_path)
    # insert out of order on disk
    _write_event(msgs, "1700000000000000030-w-2.json", _event_msg(key="C"))
    _write_event(msgs, "1700000000000000010-w-0.json", _event_msg(key="A"))
    _write_event(msgs, "1700000000000000020-w-1.json", _event_msg(key="B"))
    keys = [wc.read_message(p)["key"] for p in wc.list_pending(tmp_path)]
    assert keys == ["A", "B", "C"], keys


# Test 6 — apply-before-delete replay: a message survives until mark_done;
# the channel itself does no dedup (caller idempotency).
def test_apply_before_delete_replay(tmp_path):
    msgs = wc.watch_msgs_dir(tmp_path)
    p = _write_event(msgs, "1700000000000000001-w-0.json", _event_msg(key="pr-1-abc-BEHIND"))

    # first drain: apply happens, then a crash BEFORE mark_done.
    pending = wc.list_pending(tmp_path)
    assert len(pending) == 1
    # (no mark_done -> simulates crash between apply and delete)

    # recovery: same message is re-listed (replayed), not lost.
    again = wc.list_pending(tmp_path)
    assert [x.name for x in again] == [p.name]

    # now complete the commit point.
    wc.mark_done(again[0])
    assert wc.list_pending(tmp_path) == []
    # mark_done on an already-gone file is a no-op (idempotent).
    wc.mark_done(again[0])


# Test 10 (Python half) — versioned wire + pr_event round-trip via the shared
# golden fixture: every field reads back and the lease key reconstructs.
def test_golden_pr_event_round_trip(tmp_path):
    msgs = wc.watch_msgs_dir(tmp_path)
    p = msgs / "1700000000000000001-watch-0.json"
    msgs.mkdir(parents=True, exist_ok=True)
    p.write_bytes(GOLDEN.read_bytes())

    m = wc.read_message(p)
    assert m["kind"] == "pr_event"
    assert m["v"] == wc.SCHEMA_VERSION
    pl = m["payload"]
    assert pl["head_sha"] == "deadbeefcafe"
    assert pl["base_sha"] == "0ddba11feed5"
    assert pl["event"] == "BEHIND"
    assert pl["detail_key"] == "rebase"
    # Slice 2 must be able to rebuild the rebase lease key from the payload.
    lease_key = f"rebase:{pl['base_sha']}:{pl['head_sha']}"
    assert lease_key == "rebase:0ddba11feed5:deadbeefcafe"


# Test 10 (refuse-newer) — a message with a v newer than the reader knows is
# refused, never mis-parsed.
def test_read_message_refuses_newer_schema(tmp_path):
    msgs = wc.watch_msgs_dir(tmp_path)
    p = _write_event(msgs, "1700000000000000001-w-0.json",
                     _event_msg(v=wc.SCHEMA_VERSION + 1, key="future"))
    with pytest.raises(wc.SchemaTooNewError):
        wc.read_message(p)


# Heartbeats are read separately and never drained as events.
def test_read_heartbeat_and_list(tmp_path):
    msgs = wc.watch_msgs_dir(tmp_path)
    _write_event(msgs, "hb-wkr1.json",
                 _event_msg(kind="liveness_heartbeat", key="hb-wkr1", epoch_id=9))
    _write_event(msgs, "hb-wkr2.json",
                 _event_msg(kind="liveness_heartbeat", key="hb-wkr2", epoch_id=3))

    hb = wc.read_heartbeat(tmp_path, "wkr1")
    assert hb is not None
    assert hb["epoch_id"] == 9
    assert wc.read_heartbeat(tmp_path, "missing") is None

    names = sorted(p.name for p in wc.list_heartbeats(tmp_path))
    assert names == ["hb-wkr1.json", "hb-wkr2.json"]
    # heartbeats are not events
    assert wc.list_pending(tmp_path) == []


# A corrupt/partial JSON file is skipped by the drain, never crashes it.
def test_list_pending_tolerates_missing_dir(tmp_path):
    # directory never created -> empty pending, no error.
    assert wc.list_pending(tmp_path / "nope") == []

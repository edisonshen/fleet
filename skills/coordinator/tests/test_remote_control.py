"""Tests for the RETIRED remote_control module (native model).

rc-default-native-startup: every coord is spawned with the native
`claude --remote-control "<name>"` flag baked into its argv by the Go
side (`fleet dispatch --coord-spawn` / handoff / drain). The Python
coord tick no longer bootstraps anything — no standalone listener
daemon, no inbox seeding, no marker files.

These tests pin the retirement:

  - the shims are PURE no-ops: no subprocess, no filesystem writes;
  - the module never imports subprocess at all (the v0.12 push-storm
    came from a tick path that could fork `claude remote-control`;
    making a fork impossible is the structural fix);
  - loop.py no longer wires remote_control into the tick.

Supersedes the v0.12 test_remote_control.py + the
FLEET_RC_BOOTSTRAP_DISABLED env-gate tests (test_rc_bootstrap_env_gate
.py) — there is no Python-side spawn left to gate.
"""
from __future__ import annotations

import ast
import os
from pathlib import Path

import pytest

import remote_control

SKILL_DIR = Path(remote_control.__file__).resolve().parent


def _no_fs_mutation(tmp_path, fn):
    """Run fn with FLEET_HOME pointed at an empty dir; assert nothing
    was created."""
    before = sorted(tmp_path.rglob("*"))
    fn()
    after = sorted(tmp_path.rglob("*"))
    assert before == after, f"retired shim mutated the filesystem: {after}"


class TestRetiredShims:
    def test_bootstrap_returns_native_and_touches_nothing(
        self, tmp_path, monkeypatch
    ):
        monkeypatch.setenv("FLEET_HOME", str(tmp_path))
        _no_fs_mutation(
            tmp_path,
            lambda: remote_control.bootstrap_remote_control(
                "rainier", "abcd1234", fleet_home=tmp_path
            ),
        )
        status = remote_control.bootstrap_remote_control(
            "rainier", "abcd1234", fleet_home=tmp_path
        )
        assert status == remote_control.STATUS_NATIVE

    @pytest.mark.parametrize(
        "project,coord_id",
        [
            ("", ""),
            ("rainier", "abcd1234"),
            ("../sneaky", "NOT-HEX!"),  # even garbage is a safe no-op now
        ],
    )
    def test_bootstrap_is_total_noop_for_any_input(
        self, tmp_path, monkeypatch, project, coord_id
    ):
        monkeypatch.setenv("FLEET_HOME", str(tmp_path))
        status = remote_control.bootstrap_remote_control(
            project, coord_id, fleet_home=tmp_path
        )
        assert status == remote_control.STATUS_NATIVE
        assert sorted(tmp_path.rglob("*")) == []

    def test_spawn_daemon_if_needed_noop_true(self, tmp_path, monkeypatch):
        monkeypatch.setenv("FLEET_HOME", str(tmp_path))
        _no_fs_mutation(
            tmp_path,
            lambda: remote_control.spawn_daemon_if_needed("rainier", "abcd1234"),
        )
        assert remote_control.spawn_daemon_if_needed("rainier", "abcd1234") is True

    def test_spawn_daemon_status_noop_ok(self):
        assert (
            remote_control.spawn_daemon_status("rainier", "abcd1234")
            == remote_control.SPAWN_OK
        )


class TestNoSpawnPossible:
    """The structural push-storm fix: the module cannot fork anything."""

    def test_module_never_imports_subprocess(self):
        src = (SKILL_DIR / "remote_control.py").read_text(encoding="utf-8")
        tree = ast.parse(src)
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                names = [a.name for a in node.names]
                assert "subprocess" not in names, (
                    "remote_control.py must not import subprocess — the "
                    "native model has no Python-side spawn"
                )
            if isinstance(node, ast.ImportFrom):
                assert node.module != "subprocess"

    def test_no_popen_or_run_references(self):
        src = (SKILL_DIR / "remote_control.py").read_text(encoding="utf-8")
        for needle in ("Popen", "subprocess.run", "os.system", "os.exec"):
            # Docstrings may mention the history; check code lines only.
            code_lines = [
                line
                for line in src.splitlines()
                if not line.lstrip().startswith(("#",))
            ]
            # Strip the module docstring via ast for a precise check.
            tree = ast.parse(src)
            body_src = ast.unparse(
                ast.Module(
                    body=[
                        n
                        for n in tree.body
                        if not (
                            isinstance(n, ast.Expr)
                            and isinstance(n.value, ast.Constant)
                        )
                    ],
                    type_ignores=[],
                )
            )
            assert needle not in body_src, (
                f"remote_control.py code references {needle!r}; the retired "
                "module must be spawn-free"
            )


class TestLoopDecoupled:
    """loop.py no longer wires the bootstrap into the tick."""

    def test_loop_does_not_import_remote_control(self):
        src = (SKILL_DIR / "loop.py").read_text(encoding="utf-8")
        tree = ast.parse(src)
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                assert "remote_control" not in [a.name for a in node.names], (
                    "loop.py must not import remote_control (bootstrap retired)"
                )
            if isinstance(node, ast.ImportFrom):
                assert node.module != "remote_control"

    def test_loop_does_not_call_bootstrap(self):
        src = (SKILL_DIR / "loop.py").read_text(encoding="utf-8")
        assert "bootstrap_remote_control(" not in src

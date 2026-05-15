"""Per-project FIRST_ACTION render: pin the contract that the
auto-handoff doc's FirstAction bash block carries the project name into
the spawned remote-control daemon's session-name-prefix value.

Mirrors the Go-side regression at
internal/handoff/firstaction_project_test.go. Both sides MUST emit the
same per-project bash block so auto-handoff (this skill) and
operator-triggered handoff (Go side) write identical doc bodies for
the same project — the byte-golden test in test_handoff.py already
asserts byte-equality and will fail loudly if drift creeps in.
"""

from __future__ import annotations

from datetime import datetime, timezone
from pathlib import Path
import sys

# Skill modules sit under skills/fleet-guard/, tests under
# skills/fleet-guard/tests/. Mirror existing test_handoff.py path setup.
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import handoff  # noqa: E402


class TestFirstActionPerProject:
    """The bash block printed in every handoff doc must spawn a
    daemon with `--remote-control-session-name-prefix
    "fleet-handoff-<project>"` so per-project handoff daemons coexist
    and the operator can distinguish per-project sessions on phone /
    claude.ai.
    """

    def test_first_action_is_callable_with_project(self) -> None:
        """FIRST_ACTION must be invocable as a function taking a
        project name. Pre-fix it is a module-level string constant;
        post-fix it becomes a function `first_action(project)` that
        substitutes the project into the daemon prefix and pgrep
        guard.
        """
        # The function name we expect post-fix.
        assert hasattr(handoff, "first_action"), (
            "skills/fleet-guard/handoff.py must expose first_action(project) "
            "for per-project daemon prefix support"
        )
        body = handoff.first_action("spark")
        assert isinstance(body, str)

    def test_first_action_carries_project_in_daemon_prefix(self) -> None:
        body = handoff.first_action("spark")
        want = '"fleet-handoff-spark"'
        assert want in body, (
            f"first_action('spark') must reference {want!r} as the daemon "
            f"--remote-control-session-name-prefix value; got body:\n{body}"
        )
        # Legacy generic prefix (without project) must not appear as a
        # quoted standalone value.
        legacy = '"fleet-handoff"'
        assert legacy not in body, (
            "first_action must not reference the legacy generic "
            "'fleet-handoff' prefix (drift means project suffix wasn't "
            f"applied); got body:\n{body}"
        )

    def test_first_action_pgrep_narrowed_to_project(self) -> None:
        """The pgrep -f guard must reference the project-scoped prefix
        so per-project daemons don't mask each other on launch (broad
        `pgrep -f "claude remote-control"` matches ANY handoff daemon
        and would skip launching project B's daemon when project A's
        was already up).
        """
        body = handoff.first_action("rainier")
        # Find the pgrep line; it must include the project literal.
        pgrep_idx = body.find("pgrep -f")
        assert pgrep_idx >= 0, f"pgrep guard missing from body:\n{body}"
        nl = body.find("\n", pgrep_idx)
        line = body[pgrep_idx:nl] if nl >= 0 else body[pgrep_idx:]
        assert "fleet-handoff-rainier" in line, (
            f"pgrep guard line must contain the project-scoped prefix; "
            f"got line:\n{line}"
        )

    def test_first_action_distinct_per_project(self) -> None:
        """Two different projects must produce different bash blocks
        (regression bracket for "I refactored away the project arg").
        """
        a = handoff.first_action("spark")
        b = handoff.first_action("rainier")
        assert a != b, (
            "first_action must produce distinct output per project "
            f"(both = {a!r})"
        )

    def test_render_doc_uses_project_in_first_action(self) -> None:
        """The full _render_doc output must embed the project-scoped
        prefix into the First Action body. Without this, the printed
        doc on disk advertises the wrong daemon prefix for the
        project's handoff successor."""
        ts = datetime(2026, 4, 28, 12, 34, 56, tzinfo=timezone.utc)
        got = handoff._render_doc(
            agent_id="abcd1234",
            task_id="demo",
            project="tatoosh",
            handoff_type="auto-yellow",
            number=1,
            prev_path=None,
            context_pct=50.0,
            ts=ts,
            recent_activity="x",
        )
        assert b'"fleet-handoff-tatoosh"' in got, (
            "rendered doc must embed the project-scoped daemon prefix "
            "(per-project visibility on phone / claude.ai); got:\n"
            f"{got.decode('utf-8', errors='replace')}"
        )

    def test_first_action_pgrep_escapes_project_dot(self) -> None:
        """Codex review iter-1 [P2] regression bracket — pin the
        regex-escape contract for project names containing `.`.

        ValidateProjectName allows `.` (e.g. `v2.1`); without escaping,
        the bash block's pgrep pattern `^...fleet-handoff-v2.1( |$)`
        treats `.` as `match-any-char`, so a daemon process for a
        different project named `v2a1` would mask the launch of the
        v2.1 daemon, leaving /remote-control with no compatible daemon
        to attach to. Escaping `.` to `\\.` keeps the match strictly
        literal so daemons for project `v2.1` and `v2a1` coexist
        correctly. Mirror of internal/handoff TestFirstAction_PgrepEscapesProjectDot.
        """
        body = handoff.first_action("v2.1")

        # The pgrep -f single-quoted regex must contain the LITERAL
        # `\.` escape (two chars: backslash then dot).
        want_escaped = "fleet-handoff-v2\\.1( |$)"
        assert want_escaped in body, (
            f"first_action('v2.1') pgrep guard must contain {want_escaped!r} "
            f"(escaped `.` so a daemon for `v2a1` doesn't false-positive); "
            f"got body:\n{body}"
        )
        # The daemon-prefix flag value (a shell-quoted arg, not a
        # regex) keeps the literal `.` so the spawned daemon registers
        # under the correct project name.
        want_literal_flag = (
            '--remote-control-session-name-prefix "fleet-handoff-v2.1"'
        )
        assert want_literal_flag in body, (
            f"first_action('v2.1') daemon-prefix flag must contain "
            f"{want_literal_flag!r} (literal `.` because the flag value "
            f"is shell-quoted, not regex); got body:\n{body}"
        )

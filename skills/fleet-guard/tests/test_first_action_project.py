"""v0.12 FIRST_ACTION render: the auto-handoff doc's body is operator-
instruction markdown (not bash bootstrap). Mirrors the Go-side
regression at internal/handoff/firstaction_project_test.go. Both sides
MUST emit byte-identical text for the same project — the byte-golden
test in test_handoff.py asserts the exact bytes.

DESIGN-rc-listener-lifecycle.md §"Handoff doc rewrite" retires the
embedded bash bootstrap. The new body directs the operator to run
`fleet rc connect <project>` to re-attach mobile/web pairing — a
small UX regression (one terminal command) traded for a large
architectural win (no exec'd bash from a markdown file → no
5,620-mobile-push regressions from a stuck reviewer loop).
"""

from __future__ import annotations

from datetime import datetime, timezone
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import handoff  # noqa: E402


class TestFirstActionPerProject:
    """v0.12: every handoff doc's First Action body tells the operator
    to run `fleet rc connect <project>`. The body is plain markdown
    with NO `claude remote-control` bash exec.
    """

    def test_first_action_is_callable_with_project(self) -> None:
        assert hasattr(handoff, "first_action"), (
            "skills/fleet-guard/handoff.py must expose first_action(project)"
        )
        body = handoff.first_action("spark")
        assert isinstance(body, str)

    def test_first_action_references_fleet_rc_connect(self) -> None:
        body = handoff.first_action("spark")
        for want in (
            "fleet rc connect spark",
            "fleet rc up spark",
            "/remote-control",
            "/coordinator",
        ):
            assert want in body, (
                f"first_action('spark') must contain {want!r}; got body:\n{body}"
            )

    def test_first_action_no_bash_bootstrap(self) -> None:
        """v0.12 retired the embedded bash bootstrap — regression
        bracket for the 5,620-mobile-push incident.
        """
        body = handoff.first_action("spark")
        for forbidden in (
            "nohup claude remote-control",
            "pgrep -f",
            "```bash",
            "--remote-control-session-name-prefix",
        ):
            assert forbidden not in body, (
                f"first_action MUST NOT contain {forbidden!r} (v0.12 "
                f"retired bash bootstrap; operator-instruction text only)"
            )

    def test_first_action_distinct_per_project(self) -> None:
        a = handoff.first_action("spark")
        b = handoff.first_action("rainier")
        assert a != b, (
            "first_action must produce distinct output per project "
            f"(both = {a!r})"
        )

    def test_first_action_empty_project_fallback(self) -> None:
        body = handoff.first_action("")
        assert "fleet rc connect <project>" in body, (
            f"first_action('') should emit `<project>` placeholder; got:\n{body}"
        )

    def test_render_doc_uses_project_in_first_action(self) -> None:
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
        assert b"fleet rc connect tatoosh" in got, (
            "rendered doc must embed the project-scoped fleet rc connect "
            f"instruction; got:\n{got.decode('utf-8', errors='replace')}"
        )

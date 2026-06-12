"""Native-RC FIRST_ACTION render (rc-default-native-startup): the
auto-handoff doc's body is a status note — pairing is native at coord
spawn (`--remote-control` baked into the replacement's claude argv) —
plus the opt-out escape hatch. Mirrors the Go-side regression at
internal/handoff/firstaction_project_test.go. Both sides MUST emit
byte-identical text for the same project — the byte-golden test in
test_handoff.py asserts the exact bytes.

No bash bootstrap (gone since v0.12) and no retired `fleet rc connect`
instruction.
"""

from __future__ import annotations

from datetime import datetime, timezone
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import handoff  # noqa: E402


class TestFirstActionPerProject:
    """Native model: every handoff doc's First Action body is a status
    note (pairing is native) + opt-out escape hatch. Plain markdown
    with NO `claude remote-control` bash exec.
    """

    def test_first_action_is_callable_with_project(self) -> None:
        assert hasattr(handoff, "first_action"), (
            "skills/fleet-guard/handoff.py must expose first_action(project)"
        )
        body = handoff.first_action("spark")
        assert isinstance(body, str)

    def test_first_action_references_native_rc_surfaces(self) -> None:
        body = handoff.first_action("spark")
        for want in (
            "--remote-control",
            "fleet rc status spark",
            "fleet rc up spark",
            "/coordinator",
        ):
            assert want in body, (
                f"first_action('spark') must contain {want!r}; got body:\n{body}"
            )
        assert "fleet rc connect" not in body, (
            "first_action must NOT reference the retired `fleet rc connect`"
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
            "fleet rc connect",  # retired send-keys attach path
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
        assert "fleet rc status <project>" in body, (
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
        assert b"fleet rc status tatoosh" in got, (
            "rendered doc must embed the project-scoped native RC status "
            f"note; got:\n{got.decode('utf-8', errors='replace')}"
        )

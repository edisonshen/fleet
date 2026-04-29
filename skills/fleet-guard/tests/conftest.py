"""pytest config for fleet-guard skill tests.

The skill ships as a flat directory of .py files invoked via `python3 main.py`,
not as an installed package. To let tests import sibling modules (health,
handoff, inbox, ids) without adding `__init__.py` (which would change how the
skill is discovered by Claude Code), we put the skill directory on sys.path.
"""
from __future__ import annotations

import os
import sys

_SKILL_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _SKILL_DIR not in sys.path:
    sys.path.insert(0, _SKILL_DIR)

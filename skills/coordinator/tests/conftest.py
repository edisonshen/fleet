"""pytest config for coordinator skill tests.

The skill ships as a flat directory of .py files invoked via Claude Code,
not as an installed package. To let tests import sibling modules
(parse, dispatch, conflict, loop) without adding `__init__.py` (which
would change how Claude Code discovers the skill), put the skill
directory on sys.path here. Mirrors fleet-guard's conftest discipline.
"""
from __future__ import annotations

import os
import sys

_SKILL_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _SKILL_DIR not in sys.path:
    sys.path.insert(0, _SKILL_DIR)

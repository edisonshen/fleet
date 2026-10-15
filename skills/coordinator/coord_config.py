"""coord-config.json writer + project↔origin validation heuristic.

Schema (additive — new fields land alongside without breaking the loader):

    {
        "parallelism":        <int>,    # _load_parallelism (loop.py)
        "worktree_timeout_s": <number>, # _load_worktree_timeout (loop.py)
        "profile":            <str>,    # budget (default) | quality — load_profile
        "review_effort":      <str>,    # load_review_effort: low|medium|high
                                        # (profile default: budget medium, quality high)
        "handoff_soft_tokens": <int>,   # load_handoff_tokens: coord soft handoff at
        "handoff_hard_tokens": <int>,   # this many context tokens (budget 250000 /
                                        # 300000; quality unset -> 40% / 50% of window)
        "unavailable_models": [<str>],  # _load_unavailable_models (loop.py)
        "engine":             <str>,    # claude-code|codex — Go-owned (internal/spawn,
                                        # internal/projects); project file = last coord's
                                        # engine, ~/.fleet/coord-config.json = operator's
                                        # explicit `fleet -codex` / `-claude` choice
        "repo":               <str>     # absolute path to project's git checkout
    }

Every key resolves the same way: the project's
~/.fleet/projects/<p>/coord-config.json, then ~/.fleet/coord-config.json,
then the default. An explicit key beats the `profile` default for it.
`read_value` / `load_profile` / `load_review_effort` /
`load_handoff_tokens` are the single resolver for the profile-driven keys;
the coordinator tick and fleet-guard (sibling skill dir) both import them.

Repo binding is owned by Go (Design 3,
DESIGN-coord-repo-binding-from-project.md). The single binder —
`fleet project resolve-repo --project <p>` — is the ONLY authority on
which checkout a project binds to. The Python coordinator tick shells
out to it (read-only) instead of reimplementing a tier ladder; it no
longer reads meta.json or coord-config.json::repo as a resolution
authority. coord-config.json::repo is now a write-only breadcrumb the Go
binder stamps at coord spawn/handoff (the launch cwd is never a tier).

For repo binding this module keeps only:

  - `write_repo_idempotent` — the Python mirror of Spawn's coord-config
    writer, exercised by tests of the merge/atomic-write logic.
  - `git_remote_origin` / `remote_matches_project` — the fuzzy
    origin/basename heuristic. These survive ONLY as the reference for
    (and a parallel of) the Go binder's tier-2 legacy-bridge
    corroboration signal; they are no longer a Python resolution gate.

The pre-Design-3 Python tier ladder (its own meta.json read via
`read_project_repo_path`, the coord-config `read_repo` tier, and the
os.getcwd() fallback) was DELETED: a Python fast-path that read/validated
binding state its own way recreated the exact split-authority drift Go
validates one way and Python validated another.
"""
from __future__ import annotations

import json
import os
import subprocess
import tempfile
from pathlib import Path
from typing import Any, Callable

CONFIG_FILE = "coord-config.json"

PROFILE_BUDGET = "budget"
PROFILE_QUALITY = "quality"
PROFILES: tuple[str, ...] = (PROFILE_BUDGET, PROFILE_QUALITY)
DEFAULT_PROFILE = PROFILE_BUDGET

REVIEW_EFFORTS: tuple[str, ...] = ("low", "medium", "high")

# Per-profile defaults for keys the operator did not set explicitly. A
# None handoff token threshold means "use the percent-of-window trigger".
PROFILE_DEFAULTS: dict[str, dict[str, Any]] = {
    PROFILE_BUDGET: {
        "review_effort": "medium",
        "handoff_soft_tokens": 250_000,
        "handoff_hard_tokens": 300_000,
    },
    PROFILE_QUALITY: {
        "review_effort": "high",
        "handoff_soft_tokens": None,
        "handoff_hard_tokens": None,
    },
}


def _read_file(cfg_path: Path) -> dict:
    try:
        with open(cfg_path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return {}
    return data if isinstance(data, dict) else {}


def read_value(
    project_dir: Path | None,
    home: Path | None,
    key: str,
    parse: Callable[[Any], Any],
) -> Any:
    """First valid `key` from the project file, then the home file.

    `parse` maps a raw JSON value to the normalized value, or None when
    it is invalid (an invalid project value falls through to home).
    Returns None when neither file carries a valid value.
    """
    for base in (project_dir, home):
        if base is None:
            continue
        data = _read_file(Path(base) / CONFIG_FILE)
        if key not in data:
            continue
        value = parse(data[key])
        if value is not None:
            return value
    return None


def _parse_choice(choices: tuple[str, ...]) -> Callable[[Any], str | None]:
    def parse(raw: Any) -> str | None:
        if isinstance(raw, str) and raw.strip().lower() in choices:
            return raw.strip().lower()
        return None
    return parse


def _parse_positive_int(raw: Any) -> int | None:
    if isinstance(raw, bool) or not isinstance(raw, int) or raw <= 0:
        return None
    return raw


def load_profile(project_dir: Path | None, home: Path | None) -> str:
    """`profile`: budget | quality, default budget."""
    return (
        read_value(project_dir, home, "profile", _parse_choice(PROFILES))
        or DEFAULT_PROFILE
    )


def load_review_effort(project_dir: Path | None, home: Path | None) -> str:
    """`review_effort` (low|medium|high); unset -> the profile default."""
    explicit = read_value(
        project_dir, home, "review_effort", _parse_choice(REVIEW_EFFORTS),
    )
    if explicit:
        return explicit
    return PROFILE_DEFAULTS[load_profile(project_dir, home)]["review_effort"]


def load_handoff_tokens(
    project_dir: Path | None, home: Path | None,
) -> tuple[int | None, int | None]:
    """(soft, hard) coordinator handoff thresholds in context tokens.

    Each side is the explicit `handoff_soft_tokens` / `handoff_hard_tokens`
    when set, else the profile default; None means that side triggers on
    percent of the model window instead.
    """
    defaults = PROFILE_DEFAULTS[load_profile(project_dir, home)]
    out: list[int | None] = []
    for key in ("handoff_soft_tokens", "handoff_hard_tokens"):
        explicit = read_value(project_dir, home, key, _parse_positive_int)
        out.append(explicit if explicit is not None else defaults[key])
    return out[0], out[1]


def write_repo_idempotent(config_path: Path | str, repo: str) -> None:
    """Stamp `repo` into coord-config.json, preserving sibling fields.

    Idempotent rules:
      - File missing → create with {"repo": repo}.
      - File exists, `repo` field missing/empty → merge: add `repo`,
        preserve all other fields.
      - File exists, `repo` non-empty string → leave untouched (operator
        may have set this to a fork checkout or out-of-tree location).

    Atomic: write to a tempfile in the same directory, fsync, rename.
    A crash mid-write never leaves coord-config.json in a partial state.

    Note: this writer is invoked from Spawn (Go side has its own
    equivalent). The Python implementation exists so tests of the
    coord skill can exercise the merge logic deterministically without
    shelling out to the Go binary.
    """
    path = Path(config_path)
    parent = path.parent
    parent.mkdir(parents=True, exist_ok=True)
    # Load existing config (if any).
    data: dict = {}
    try:
        with open(path, "r", encoding="utf-8") as fh:
            existing = json.load(fh)
        if isinstance(existing, dict):
            data = existing
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        # Treat unreadable/malformed config as "no config" — we'll write
        # a clean one. This preserves the operator's intent (record the
        # repo) without inheriting garbage.
        data = {}
    # iter-9 (codex P1): always overwrite `repo` with the respawn
    # cwd. See internal/spawn/coord_config.go for the full rationale —
    # short version: respawning IS the operator's signal that the
    # previous value was wrong. Preservation would trap projects
    # without meta.json in the #175 wrong-repo state. Operators
    # wanting a permanent pin use `fleet project add` to set
    # meta.json::repo_path (which wins over coord-config in loop.tick).
    data["repo"] = repo
    # Atomic write: tmp + fsync + rename.
    fd, tmp = tempfile.mkstemp(prefix=path.name + ".tmp.", dir=str(parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(data, fh, indent=2, sort_keys=True)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


def git_remote_origin(repo_path: str) -> str:
    """Return `git -C <repo> remote get-url origin`, stripped. Empty on
    any failure (missing git, missing repo, no `origin` remote)."""
    try:
        proc = subprocess.run(
            ["git", "-C", repo_path, "remote", "get-url", "origin"],
            capture_output=True, text=True, timeout=5,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired, OSError):
        return ""
    if proc.returncode != 0:
        return ""
    return (proc.stdout or "").strip()


def remote_matches_project(remote_url: str, project: str) -> bool:
    """Heuristic: does the `origin` URL look like it belongs to `project`?

    Fleet project tags from `internal/projects.TagForPath` are
    `<parent>-<base>` where `<base>` is the repo basename. Both halves
    can contain `-` (sanitized `my-org` parent, sanitized `my-project`
    base), so a perfectly unambiguous inversion isn't possible. The
    chosen heuristic: split on the FIRST `-` only, treating everything
    after as the expected basename. When the tag has no `-` at all
    (single-segment registration), match the whole tag.

    Rationale (iter-12, codex P1 final resolution): two earlier
    candidates were rejected.

      - Strict suffix match (iter-5..11) — `project.endswith("-" + url)`.
        Allowed `projects-rainier-app` ↔ `app.git` to pass silently,
        defeating #175's safety guarantee for the legacy/no-meta.json
        path (codex iter-9 P1).

      - Try-all-segment splits (a permissive alternative) — accept any
        `-`-split-point match. Same problem: ambiguous tags pass too
        many spurious matches.

    Strict first-`-` split rejects codex iter-9's bad case. The cost:
    hyphenated-PARENT directories (`my-org/my-project` → tag
    `my-org-my-project`, base=`my-project`, but iter-12 splits as
    bare=`org-my-project`) won't match the heuristic. Those operators
    register via `fleet project add` → meta.json::repo_path, which
    wins over coord-config + bypasses the heuristic entirely (iter-7).

    Examples (all True):
      ('https://github.com/edisonshen/rainier.git', 'projects-rainier')
      ('git@github.com:acme/my-project.git', 'repos-my-project')
      ('https://github.com/edisonshen/fleet', 'projects-fleet')
      ('git@github.com:foo/rainier-app.git', 'projects-rainier-app')

    Examples (all False):
      ('https://github.com/org/rainier-app.git', 'projects-rainier')
      ('git@github.com:foo/fleet-cli.git', 'projects-fleet')
      ('https://github.com/org/projects-rainier-app.git', 'projects-rainier')
      ('https://github.com/org/app.git', 'projects-rainier-app')   # iter-9
      ('git@github.com:src/my-project.git', 'my-org-my-project')   # use meta.json

    Empty-origin: returns False. CALLER (loop.tick) branches on empty
    origin BEFORE calling this — see
    test_tick_local_only_repo_no_origin_warns_but_proceeds.
    """
    if not remote_url or not project:
        return False
    # Strip protocol + path prefix down to the URL basename.
    tail = remote_url.rsplit("/", 1)[-1]
    tail = tail.rsplit(":", 1)[-1]
    if tail.endswith(".git"):
        tail = tail[: -len(".git")]
    if not tail:
        return False
    # Normalize case — TagForPath lowercases project tags
    # (`Repos/MyProject` becomes `repos-myproject`) but
    # `git remote get-url origin` returns verbatim URLs. Lowercasing
    # the URL basename matches the project-tag normalization
    # (iter-13, codex P2).
    tail = tail.lower()
    project = project.lower()
    # Two acceptance modes, in order:
    #
    # 1. Whole-tag equality. Handles manually-named projects (
    #    `--project my-project` ↔ `my-project.git`, codex iter-16 P1)
    #    AND single-segment registrations (`--project fleet` ↔
    #    `fleet.git`). The operator's explicit project name equals
    #    the URL basename verbatim — unambiguously legitimate.
    #
    # 2. TagForPath split convention. For tags WITH `-`, also accept
    #    when the trailing segment matches the URL basename.
    #    `projects-rainier` ↔ `rainier.git` works via this branch.
    #
    # Known edge: this DOES allow `projects-rainier` ↔
    # `projects-rainier.git` through mode 1 — a coord whose `--project
    # projects-rainier` literally matches a github repo named
    # `projects-rainier.git` (different from intended `rainier.git`)
    # would pass validation. Codex iter-13/iter-15 surfaced this as
    # P2; codex iter-16 surfaced the reverse (rejecting it blocks
    # manually-named hyphenated projects with no working recovery
    # path — `fleet project add` always uses TagForPath, so a custom
    # tag like `my-project` can't be re-registered to itself).
    #
    # Final engineering call: accept the iter-13 edge case (extremely
    # rare — requires a github repo to literally be named with the
    # TagForPath-style prefix) in order to support the common
    # iter-16 case (manually-named projects whose tag IS the github
    # repo basename). Operators hitting the iter-13 case can manually
    # edit coord-config.json or use a TagForPath-style tag instead.
    if tail == project:
        return True
    if "-" not in project:
        return False
    return project.split("-", 1)[1] == tail

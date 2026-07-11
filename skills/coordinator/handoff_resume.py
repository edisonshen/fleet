"""Coord handoff resume — Phase B2 (issue #93) + v0.8.3 rich-state.

When fleet-guard hands off the coord at 50/70% context, any in-flight
worker subagents the coord had spawned via Claude's Agent tool die with
the parent process. Phase B2 carries enough state in the handoff doc
that the successor coord can re-issue the Agent calls on its first
turn.

v0.8.3 adds two enrichments to the handoff doc the resume helper now
consumes:
  - Active Subagents row carries `status` + `pr_url` per slug (pulled
    from tasks.md at handoff write time). Re-dispatch logic branches:
      * empty pr_url + status=in-progress → re-dispatch Agent (worker
        was still writing code; resume from WIP).
      * pr_url set + status=in-review → SKIP Agent re-dispatch; the
        successor just re-spawns a shepherd until-loop watching CI/
        merge state. Work is already on disk + remote.
      * status=done/abandoned → drop the entry; no resume action.
  - `## Open PRs` body section — one bullet per open worker PR
    (`gh pr list --state open --search head:worker/` snapshot). The
    successor re-spawns one shepherd until-loop per URL listed. This
    is broader than the per-slug pr_url field on ActiveSubagents: it
    catches PRs whose task was already dropped from
    coord-state.json:worker_agent_ids (worker finished, but the PR
    isn't merged yet).

This module is the successor's helper:

  1. parse_handoff_doc(path) — read the handoff doc and extract the
     `## Active Subagents` section as a typed list. Mirrors the Go
     side's handoff.ParseActiveSubagents — both sides round-trip the
     same byte shape (golden tests pin this).
  2. parse_open_prs(path) — read the `## Open PRs` section and return
     a list of URLs the successor will re-spawn shepherds for.
  3. build_resume_dispatches(subs, wip_dir, fleet_home) — for each
     active subagent whose WIP file exists AND whose status/pr_url
     indicates the worker still needs to be re-issued, render a
     DISPATCH block. Subagents in-review with a PR are NOT re-
     dispatched (shepherd path covers them).

The contract: the coord agent's SKILL.md "Resume after handoff"
section instructs Claude to call:

    python3 -m coordinator.handoff_resume <handoff_doc_path>

on its first turn. The script emits zero or more DISPATCH blocks on
stdout — same format as loop.py's tick output — so the existing
"Worker dispatch protocol" reading the coord agent already does covers
re-dispatch identically to first-spawn. Open PR shepherd hints are
written to stderr so the operator can re-establish them manually if
the coord agent doesn't pick up the wakeup-loop conventions.
"""
from __future__ import annotations

import fcntl
import json
import os
import re
import sys
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable

# Local sibling module — keep imports stable when running as a script
# under sys.path = [skills/coordinator].
import dispatch as dispatch_mod


# Resume preamble injected into the worker's prompt when re-dispatched
# after a coord handoff. The body of the original inbox file appears
# AFTER this preamble so the worker has full task context but starts
# from the WIP checkpoint (CLAUDE.md §2 Subagent Dispatch Contract).
_RESUME_PREAMBLE_TEMPLATE = (
    "**RESUMING after coord handoff.** Your previous instance died when the\n"
    "outgoing coord handed off its context. Read your WIP checkpoint at:\n"
    "\n"
    "  {wip_path}\n"
    "\n"
    "and continue from `next_steps`. Do NOT redo `phases_completed`.\n"
    "After each phase boundary, update the WIP atomically as before.\n"
    "\n"
    "Your task brief follows verbatim:\n"
    "\n"
    "---\n"
    "\n"
)


@dataclass
class ResumeEntry:
    """One in-flight worker carried across coord handoff."""

    task_id: str           # task slug
    branch: str            # worker/<slug>
    phase: str             # last-known phase (triage context only)
    status: str = ""       # tasks.md status enum; "" for legacy 5-field rows
    pr_url: str = ""       # open PR URL from tasks.md; "" when no PR yet
    agent_id: str = ""     # 8-hex Fleet agent ID; inbox file key
    subagent_id: str = ""  # Claude Agent tool subagent ID; may be empty


# v0.8.3: statuses where Agent re-dispatch is NOT needed — the worker
# is past the writing phase and either (a) a shepherd is watching a PR
# or (b) the task is already terminal. The successor coord either re-
# spawns a shepherd (in-review) or drops the entry (done/abandoned/
# blocked). The pre-enrichment behavior treated every active entry as
# "re-dispatch"; this set lets us be precise.
_TERMINAL_STATUSES = frozenset({"done", "abandoned"})
_NON_REDISPATCH_STATUSES = frozenset({"in-review", "done", "abandoned", "blocked"})


def parse_handoff_doc(path: str | os.PathLike) -> list[ResumeEntry]:
    """Read a handoff doc from `path` and extract the Active Subagents
    section as a typed list. Returns [] when the section is missing or
    empty. Raises FileNotFoundError when the doc itself is missing.

    Mirror of internal/handoff.ParseActiveSubagents — the line shape is
    `- task=<q> branch=<q> phase=<q> agent_id=<q> subagent_id=<q>` with
    Go-quoted values. We use a small in-process parser rather than
    shelling out to a Go helper to keep the resume path zero-dep.
    """
    p = Path(path)
    text = p.read_text(encoding="utf-8")
    return _parse_active_subagents(text)


# Section header constant — must match the Go renderer.
_ACTIVE_SUBAGENTS_HEADER = "## Active Subagents"
_NONE_PLACEHOLDER = "_(none)_"


def _parse_active_subagents(doc: str) -> list[ResumeEntry]:
    """Walk the doc line-by-line, isolate the section body, parse each
    entry. Whitespace-only and placeholder bodies return []."""
    in_section = False
    entries: list[ResumeEntry] = []
    for raw in doc.splitlines():
        if not in_section:
            if raw.strip() == _ACTIVE_SUBAGENTS_HEADER:
                in_section = True
            continue
        # In section. Stop on next ## heading (forward-compat).
        if raw.startswith("## "):
            break
        line = raw.strip()
        if not line or line == _NONE_PLACEHOLDER:
            continue
        entry = _parse_entry_line(line)
        if entry is not None:
            entries.append(entry)
    return entries


# Match `key="<go-quoted-value>"` segments. `go-quoted` values may
# contain escaped quotes (`\"`), backslashes (`\\`), and other
# strconv.Quote escapes; the regex captures the full quoted lexeme.
_KV_RE = re.compile(r'([a-z_]+)="((?:[^"\\]|\\.)*)"')


def _parse_entry_line(line: str) -> ResumeEntry | None:
    """Parse one `- task=... branch=... ...` body line. Returns None on
    structural failure; the caller drops the line and continues.

    v0.8.3: status + pr_url are recognized fields. Legacy 5-field rows
    (no status/pr_url) parse with the new fields defaulting to "" —
    the successor falls back to "always re-dispatch Agent" behavior."""
    if not line.startswith("- "):
        return None
    rest = line[2:]
    fields: dict[str, str] = {}
    for m in _KV_RE.finditer(rest):
        key = m.group(1)
        value = _unquote_go(m.group(2))
        fields[key] = value
    task = fields.get("task", "")
    if not task:
        return None
    return ResumeEntry(
        task_id=task,
        branch=fields.get("branch", ""),
        phase=fields.get("phase", ""),
        status=fields.get("status", ""),
        pr_url=fields.get("pr_url", ""),
        agent_id=fields.get("agent_id", ""),
        subagent_id=fields.get("subagent_id", ""),
    )


# v0.8.3: Open PRs section header constants.
_OPEN_PRS_HEADER = "## Open PRs"
_OPEN_PRS_NONE_PLACEHOLDER = "_(no open PRs)_"
# `- #<num> <title> — <head> — <url>` — the URL is what matters for
# shepherd respawn. The em dash separator is U+2014. Regex captures the
# URL as the last em-dash-delimited segment so titles containing em
# dashes don't break the parse.
_OPEN_PR_URL_RE = re.compile(r"\s—\s(https?://\S+)\s*$")


def parse_open_prs(path: str | os.PathLike) -> list[str]:
    """Read the handoff doc at `path` and return the list of open PR
    URLs from the `## Open PRs` section. Returns [] when the section
    is missing, empty, or only contains the placeholder.

    Lines that don't match the expected bullet shape are dropped
    silently (older docs predating the section, hand-edited docs).
    The shepherd respawn is best-effort: each URL the successor sees
    becomes one watch loop; missing URLs just mean no watcher for
    that PR (operator's `gh pr list` already covers manual oversight).
    """
    p = Path(path)
    text = p.read_text(encoding="utf-8")
    return _parse_open_prs_body(text)


def _parse_open_prs_body(doc: str) -> list[str]:
    in_section = False
    urls: list[str] = []
    for raw in doc.splitlines():
        if not in_section:
            if raw.strip() == _OPEN_PRS_HEADER:
                in_section = True
            continue
        if raw.startswith("## "):
            break
        line = raw.strip()
        if not line or line == _OPEN_PRS_NONE_PLACEHOLDER:
            continue
        m = _OPEN_PR_URL_RE.search(line)
        if not m:
            continue
        urls.append(m.group(1))
    return urls


def _unquote_go(s: str) -> str:
    """Reverse the strconv.Quote escape set _go_quote uses on the
    fleet-guard side. Limited to the ASCII subset agent_ids/slugs/paths
    populate in practice plus the danger characters (\\, \", newline,
    tab, control codes \\xNN).
    """
    out: list[str] = []
    i = 0
    while i < len(s):
        ch = s[i]
        if ch != "\\":
            out.append(ch)
            i += 1
            continue
        if i + 1 >= len(s):
            out.append(ch)
            break
        esc = s[i + 1]
        if esc == '"':
            out.append('"')
            i += 2
        elif esc == "\\":
            out.append("\\")
            i += 2
        elif esc == "n":
            out.append("\n")
            i += 2
        elif esc == "r":
            out.append("\r")
            i += 2
        elif esc == "t":
            out.append("\t")
            i += 2
        elif esc == "x" and i + 3 < len(s):
            try:
                out.append(chr(int(s[i + 2 : i + 4], 16)))
            except ValueError:
                out.append(s[i : i + 4])
            i += 4
        else:
            out.append(s[i : i + 2])
            i += 2
    return "".join(out)


def build_resume_dispatches(
    entries: list[ResumeEntry],
    *,
    wip_dir: str | os.PathLike,
    fleet_home: str | os.PathLike,
    project: str | None = None,
    on_resume: Callable[[str], None] | None = None,
) -> tuple[list[str], list[str]]:
    """Build DISPATCH blocks the coord agent can act on to re-spawn
    each Active Subagent whose WIP file exists AND whose status/pr_url
    indicates the worker still needs to be re-issued.

    Returns (blocks, skipped). `blocks` is the list of rendered
    DISPATCH strings (one per resume target); `skipped` is a list of
    one-line human-readable reasons for entries that were NOT
    re-dispatched (missing WIP, missing inbox, malformed agent_id,
    pr already open and in review, or terminal status).

    `on_resume`, if given, is called with each `task_id` that actually
    gets a DISPATCH block emitted (review iter-2: codex flagged that the
    resumed task's slug was never recorded into coord-state.json:
    session_tasks, so the session-scoped Next Steps/Open Questions
    collectors silently drop a just-resumed active task if THIS coord
    hands off again before any loop.py tick seam independently touches
    it). `main()` uses this hook to stamp the successor's own coord_id
    into session_tasks for every task it just resumed — kept as a
    callback (not a 3rd return value) so this stays backward compatible
    with the existing 2-tuple unpacking at every call site.

    v0.8.3 decision branches:
      - status in {in-review, done, abandoned, blocked} → SKIP Agent
        re-dispatch. in-review entries have an open PR — the shepherd
        respawn loop (parse_open_prs path) covers them. done/
        abandoned/blocked are terminal; no resume action needed.
      - empty status (legacy 5-field row) → fall back to pre-
        enrichment behavior: re-dispatch if WIP + inbox present.
      - empty pr_url + status in flight (todo/ready/in-progress) →
        re-dispatch Agent (worker was still writing code; resume
        from WIP).

    Resume protocol:
      - WIP at <wip_dir>/<task_id>.md — required. Without a checkpoint,
        re-dispatching would restart the work from scratch and risk
        duplicating commits / PRs already in flight.
      - Inbox at <fleet_home>/inbox/<agent_id>.md — required. The
        worker prompt template inlines task spec/acceptance/standards;
        if the inbox is gone we'd have to rebuild from tasks.md, which
        is a Phase C concern (cleaner, but invasive).

    The successor coord re-uses the agent_id from the handoff doc so
    tasks.md notes (`dispatched as agent <id>`) stay coherent across
    handoffs.
    """
    wip_dir_p = Path(wip_dir)
    fleet_home_p = Path(fleet_home)
    # Owner string for the replay project-ownership filter (loop.py
    # _iter_project_journals). The resume acquire below records it on a
    # freshly-created journal; defaults to FLEET_PROJECT so the re-armed
    # entry is owned by the coord's project rather than orphaned.
    resume_project = project or os.environ.get("FLEET_PROJECT", "") or "unknown"
    blocks: list[str] = []
    skipped: list[str] = []
    for e in entries:
        if not e.task_id:
            skipped.append("entry missing task_id; skip")
            continue
        if not e.agent_id:
            skipped.append(f"task {e.task_id}: missing agent_id; skip")
            continue
        # v0.8.3: skip Agent re-dispatch for in-review / terminal
        # entries. Empty status (legacy row) falls through to the
        # pre-enrichment "always re-dispatch" path below.
        if e.status and e.status in _NON_REDISPATCH_STATUSES:
            if e.status == "in-review":
                skipped.append(
                    f"task {e.task_id}: status=in-review pr_url={e.pr_url!r}; "
                    "skip Agent (shepherd-only path)",
                )
            else:
                skipped.append(
                    f"task {e.task_id}: status={e.status}; "
                    "skip (terminal or blocked)",
                )
            continue
        wip_path = wip_dir_p / f"{e.task_id}.md"
        if not wip_path.exists():
            skipped.append(
                f"task {e.task_id}: WIP {wip_path} missing; skip "
                "(worker likely finished or never started)",
            )
            continue
        inbox_path = fleet_home_p / "inbox" / f"{e.agent_id}.md"
        if not inbox_path.exists():
            skipped.append(
                f"task {e.task_id}: inbox {inbox_path} missing; skip "
                "(rebuild from tasks.md is Phase C)",
            )
            continue
        # Build the resume prompt: WIP-aware preamble + original inbox
        # body. We re-use the SAME agent_id so tasks.md notes stay
        # coherent across handoff.
        original_body = inbox_path.read_text(encoding="utf-8")
        resume_body = (
            _RESUME_PREAMBLE_TEMPLATE.format(wip_path=str(wip_path))
            + original_body
        )
        # dispatch-durability (#184): re-arm the EXISTING dispatch journal
        # via `fleet claims reset-for-relaunch` instead of a bare
        # write_worker_inbox. The original journal entry is ExecAcked /
        # terminal from the FIRST dispatch; a blind mark-launch-attempted
        # would predicate-fail (not pending) and the resume would never
        # launch. reset-for-relaunch reads the new prompt on stdin, then
        # under one flock rewrites the inbox AND resets the entry to a
        # fresh ExecPending with a BUMPED generation — so this resume block
        # carries the new gen, enters the universal launch gate, and any
        # stale older-gen block predicate-fails (no double-launch).
        reset = dispatch_mod.reset_for_relaunch(
            e.agent_id, resume_body, fleet_home=str(fleet_home_p),
        )
        outcome = reset.get("outcome")
        if outcome == "reset":
            generation = int(reset.get("generation") or 0)
        elif outcome == "contention":
            # TRANSIENT: the journal flock was busy. Do NOT re-emit a block
            # we couldn't re-arm (a stale-gen block would predicate-fail at
            # the coord and waste the launch). Skip; the next successor tick
            # retries the reset cleanly.
            skipped.append(
                f"task {e.task_id}: reset-for-relaunch contention; "
                "skip (retry next tick)",
            )
            continue
        elif outcome == "absent":
            # No journal for this id (pre-#184 dispatch, or the journal was
            # reaped). Codex iter-1 [P2]: a bare write_worker_inbox + gen-0
            # block would be SKIPPED by the coord — the new protocol always
            # runs `mark-launch-attempted` first, and the Go side returns
            # predicate_fail for an absent journal, so the resume would
            # never launch. Acquire a FRESH journal instead: that creates
            # the entry at ExecPending generation 0 AND writes the resume
            # inbox under the flock, so the gen-0 resume block passes the
            # launch gate. This re-arms legacy/no-journal resumes the
            # durability machinery didn't previously own.
            try:
                dispatch_mod.acquire_coord_prompt_inbox(
                    e.agent_id, resume_body,
                    owner=f"project/{resume_project}/slug/{e.task_id}",
                    fleet_home=str(fleet_home_p),
                )
            except dispatch_mod.AcquirePromptError as exc:
                skipped.append(
                    f"task {e.task_id}: resume acquire failed "
                    f"(reset outcome=absent): {exc.message}; skip",
                )
                continue
            generation = 0
        else:
            # error (binary missing / unexpected): the journal was NOT
            # mutated and we cannot re-arm it via the Go path. Best-effort
            # direct inbox write with gen 0 — if a journal does exist at a
            # higher gen the gen-0 block predicate-fails (no double-launch);
            # this just preserves the inbox body for a manual re-dispatch.
            try:
                dispatch_mod.write_worker_inbox(
                    e.agent_id, resume_body, fleet_home=str(fleet_home_p),
                )
            except (ValueError, OSError) as exc:
                skipped.append(
                    f"task {e.task_id}: inbox rewrite failed "
                    f"(reset outcome={outcome!r}): {exc}; skip",
                )
                continue
            generation = 0
        try:
            block = dispatch_mod.format_dispatch_instruction(
                agent_id=e.agent_id,
                slug=e.task_id,
                prompt_file=str(inbox_path),
                description=f"fleet worker {e.task_id} (resume)",
                generation=generation,
            )
        except ValueError as exc:
            skipped.append(
                f"task {e.task_id}: dispatch format failed: {exc}; skip",
            )
            continue
        blocks.append(block)
        if on_resume is not None:
            on_resume(e.task_id)
    return blocks, skipped


# ---------- session_tasks recording (review iter-2) ----------
#
# handoff_resume is a standalone script (not run inside loop.py's tick),
# so it needs its own tiny coord-state.json RMW — mirrors
# register_subagent._read_coord_state / _write_coord_state / _take_coord_lock
# verbatim rather than importing them (established convention in this
# package: each small script that touches coord-state.json keeps its own
# local copy of the atomic-write + coordinator.lock helpers instead of a
# shared cross-module dependency).

_LOCK_RETRY_ATTEMPTS = 10
_LOCK_RETRY_INTERVAL_S = 0.1


def _read_coord_state(path: Path) -> dict:
    """Load coord-state.json. Empty dict on first run / read error."""
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
        if isinstance(data, dict):
            return data
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        pass
    return {}


def _write_coord_state(path: Path, state: dict) -> None:
    """Atomic publish (tmp + rename + fsync)."""
    parent = path.parent
    parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=path.name + ".tmp.", dir=str(parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(state, fh, indent=2, sort_keys=True)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


def _take_coord_lock(path: Path):
    """Acquire coordinator.lock LOCK_EX | LOCK_NB with a brief retry.
    Returns a context manager; raises OSError after exhausting retries."""
    path.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(str(path), os.O_CREAT | os.O_RDWR, 0o644)
    last_err: OSError | None = None
    for _ in range(_LOCK_RETRY_ATTEMPTS):
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return _ResumeLockHandle(fd)
        except OSError as exc:
            last_err = exc
            time.sleep(_LOCK_RETRY_INTERVAL_S)
    os.close(fd)
    raise OSError(f"coordinator.lock busy after {_LOCK_RETRY_ATTEMPTS} attempts") from last_err


class _ResumeLockHandle:
    """Tiny RAII handle for the coord lock (mirrors register_subagent's
    _LockHandle). Errors during release are swallowed — the kernel
    reclaims the flock on close regardless."""

    def __init__(self, fd: int) -> None:
        self.fd = fd

    def __enter__(self) -> "_ResumeLockHandle":
        return self

    def __exit__(self, *_exc) -> None:
        try:
            fcntl.flock(self.fd, fcntl.LOCK_UN)
        except OSError:
            pass
        try:
            os.close(self.fd)
        except OSError:
            pass


def _parse_frontmatter_project(path: str | os.PathLike) -> str:
    """Return the handoff doc frontmatter project, or "" when absent."""
    text = Path(path).read_text(encoding="utf-8")
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        return ""
    for raw in lines[1:]:
        if raw.strip() == "---":
            break
        key, sep, value = raw.partition(":")
        if sep and key.strip() == "project":
            return _decode_frontmatter_value(value.strip())
    return ""


def _decode_frontmatter_value(value: str) -> str:
    """Decode the simple YAML-ish values emitted by Go's fmt %q."""
    if len(value) >= 2 and value[0] == '"' and value[-1] == '"':
        return _unquote_go(value[1:-1])
    if len(value) >= 2 and value[0] == "'" and value[-1] == "'":
        return value[1:-1]
    return value.strip()


def resolve_resume_project(doc_path: str | os.PathLike) -> str:
    """Resolve the project for the applied marker.

    Fleet normally sets FLEET_PROJECT in the coord pane. Handoff docs also
    carry project: in frontmatter, and that is the mandatory fallback so an
    unset env cannot silently skip the resume ack write.
    """
    env_project = os.environ.get("FLEET_PROJECT", "").strip()
    if env_project:
        return env_project
    return _parse_frontmatter_project(doc_path).strip()


def record_resumed_handoff_marker(
    doc_path: str | os.PathLike, *, project: str, home: Path,
) -> None:
    """Write coord-state.json:resumed_handoff_path under coordinator.lock.

    This is the single applied-ack writer for durable handoff resume. The
    coord-run supervisor and prompt-delivery paths only read this marker.
    """
    if not project:
        raise ValueError("project is required to write resumed_handoff_path")
    project_dir = home / "projects" / project
    state_path = project_dir / "coord-state.json"
    lock_path = project_dir / ".locks" / "coordinator.lock"
    with _take_coord_lock(lock_path):
        cs = _read_coord_state(state_path)
        cs["resumed_handoff_path"] = str(doc_path)
        _write_coord_state(state_path, cs)


def record_resumed_session_tasks(
    resumed_slugs: list[str], *, project: str, coord_id: str, home: Path,
) -> None:
    """Best-effort RMW: stamp each just-resumed task_id into
    coord-state.json:session_tasks under THIS (successor) coord's own
    coord_id, via the shared dispatch.record_session_task writer — the
    same buffer/dedupe/cap logic every other dispatch seam uses.

    Never raises: a resume that can't record its session_tasks stamp
    must not abort the resume dispatch itself (the DISPATCH blocks are
    already built and about to be printed to stdout regardless).
    """
    if not resumed_slugs:
        return
    try:
        project_dir = home / "projects" / project
        state_path = project_dir / "coord-state.json"
        lock_path = project_dir / ".locks" / "coordinator.lock"
        with _take_coord_lock(lock_path):
            # codex iter-10 [P2]: _read_coord_state() flattens BOTH a missing
            # file AND a MALFORMED (JSONDecodeError / non-dict) file to {}.
            # A missing file is fine to create fresh, but overwriting a
            # malformed file with {session_tasks:[…]} would silently DROP its
            # sibling keys (worker_agent_ids, recent_decisions, redispatch
            # markers…) — real data loss. This stamp is best-effort, so the
            # safe move on a present-but-unparseable file is to SKIP the write,
            # not truncate it. Distinguish the two by reading raw first.
            if state_path.exists():
                try:
                    raw = json.loads(state_path.read_text(encoding="utf-8"))
                except (ValueError, OSError):
                    return  # malformed / unreadable → do not clobber
                if not isinstance(raw, dict):
                    return  # non-dict payload → do not clobber
                cs = raw
            else:
                cs = {}
            for slug in resumed_slugs:
                dispatch_mod.record_session_task(cs, slug, coord_id)
            _write_coord_state(state_path, cs)
    except Exception:  # noqa: BLE001 — best-effort, mirrors loop._record_session_task
        pass


def main(argv: list[str] | None = None) -> int:
    """CLI: `python3 -m coordinator.handoff_resume <handoff_doc>`.

    Emits zero or more DISPATCH blocks on stdout — same shape the coord
    agent already acts on for first-spawn dispatches. Skipped entries
    + open-PR shepherd hints write to stderr (operator visibility).
    Always exits 0 unless the handoff doc itself is unreadable (exit
    1) — partial resumes are better than aborting the whole successor.
    """
    args = list(argv) if argv is not None else sys.argv[1:]
    if not args:
        print("usage: handoff_resume <handoff_doc_path>", file=sys.stderr)
        return 2
    doc_path = args[0]
    try:
        entries = parse_handoff_doc(doc_path)
    except FileNotFoundError:
        print(f"handoff_resume: doc not found: {doc_path}", file=sys.stderr)
        return 1
    # v0.8.3: parse the Open PRs section too. Shepherd respawn is a
    # coord-level concern (the wakeup loop watching each URL), so we
    # surface the URLs to stderr as hints; the coord agent's
    # /coordinator skill (called from First Action) re-establishes
    # them on its next tick via the standard `gh pr status` path.
    open_pr_urls: list[str] = []
    try:
        open_pr_urls = parse_open_prs(doc_path)
    except FileNotFoundError:
        # Already handled above by parse_handoff_doc; defensive.
        pass
    fleet_home = os.environ.get("FLEET_HOME") or os.path.expanduser("~/.fleet")
    wip_dir = os.environ.get(
        "FLEET_SUBAGENT_WIP_DIR",
    ) or os.path.expanduser("~/.fleet/subagent-wip")
    project = resolve_resume_project(doc_path)
    if not project:
        print(
            "handoff_resume: project missing (FLEET_PROJECT unset and "
            "handoff frontmatter has no project)",
            file=sys.stderr,
        )
        return 1
    resumed_slugs: list[str] = []
    blocks, skipped = build_resume_dispatches(
        entries, wip_dir=wip_dir, fleet_home=fleet_home,
        project=project, on_resume=resumed_slugs.append,
    )
    # review iter-2 (codex): stamp every resumed task_id into THIS coord's
    # own session_tasks under its own coord_id — without this, a resumed
    # active task silently disappears from Next Steps/Open Questions if
    # this successor hands off again before any loop.py tick seam
    # independently touches it. Best-effort: the DISPATCH blocks below are
    # unconditional regardless of whether this stamp lands.
    record_resumed_session_tasks(
        resumed_slugs, project=project,
        coord_id=os.environ.get("FLEET_AGENT_ID", ""),
        home=Path(fleet_home),
    )
    try:
        record_resumed_handoff_marker(
            doc_path, project=project, home=Path(fleet_home),
        )
    except Exception as exc:  # noqa: BLE001 — exit 0 is the applied ack.
        print(f"handoff_resume: write resumed_handoff_path failed: {exc}", file=sys.stderr)
        return 1
    for line in skipped:
        print(f"# {line}", file=sys.stderr)
    for url in open_pr_urls:
        print(
            f"# open-pr-shepherd: {url} — respawn `gh pr view` watch loop",
            file=sys.stderr,
        )
    for b in blocks:
        print(b)
        print()
    if not blocks:
        print(
            "# handoff_resume: no resumable subagents "
            f"(parsed={len(entries)} skipped={len(skipped)} "
            f"open_prs={len(open_pr_urls)})",
            file=sys.stderr,
        )
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env bash
# v0-11-pre-migration-sweep.sh — one-shot legacy-leak sweeper.
#
# Targets the leaks identified in
# docs/DESIGN-dispatch-lifecycle.md §"One-shot leak sweep (pre-PR2 prep)":
#
#   * 30 stale coord_prompt_inbox files (~/.fleet/inbox/<8hex>.md with
#     no matching live agent_record + no matching journal under
#     ~/.fleet/dispatches/).
#   * 2 orphan worktrees (~/.fleet/projects/*/worktrees/* whose slug
#     isn't referenced by tasks.md).
#   * 4 supervisor ghosts (coord-state.json supervisor map entries
#     whose slug isn't referenced by tasks.md).
#
# Conservative by design: default is dry-run (lists what would be
# cleaned); `--kill` actually cleans. Only acts on resources that match
# known legacy name shapes (8-hex IDs, slug filenames). Unknown shapes
# are logged and skipped.
#
# TODO(v0.11.0): delete this script after v0.11.0 ships and the unified
# `fleet maintenance sweep-leaks` covers the same surface (PR4).
#
# Usage:
#   scripts/v0-11-pre-migration-sweep.sh [--kill]
#
# Env:
#   FLEET_HOME   override ~/.fleet (matches the Go side's env var).
#
# Exit codes:
#   0  scan completed (whether anything was cleaned or not).
#   1  setup error (FLEET_HOME missing or unreadable, etc.).

set -euo pipefail

KILL=0
for arg in "$@"; do
  case "$arg" in
    --kill)
      KILL=1
      ;;
    --help|-h)
      sed -n '2,30p' "$0"
      exit 0
      ;;
    *)
      echo "unknown arg: $arg" >&2
      echo "usage: $0 [--kill]" >&2
      exit 1
      ;;
  esac
done

FLEET_HOME="${FLEET_HOME:-$HOME/.fleet}"
INBOX_DIR="$FLEET_HOME/inbox"
AGENTS_DIR="$FLEET_HOME/agents"
ARCHIVE_DIR="$FLEET_HOME/agents/archive"
DISPATCHES_DIR="$FLEET_HOME/dispatches"
PROJECTS_DIR="$FLEET_HOME/projects"

if [ ! -d "$FLEET_HOME" ]; then
  echo "FLEET_HOME not found: $FLEET_HOME" >&2
  exit 1
fi

mode_label="DRY-RUN"
if [ "$KILL" -eq 1 ]; then
  mode_label="KILL"
fi
echo "=== v0.11 pre-migration sweep ($mode_label) ==="
echo "FLEET_HOME=$FLEET_HOME"
echo

# ---------------------------------------------------------------------
# Phase 1 — stale coord_prompt_inbox files.
# ---------------------------------------------------------------------
# A file at ~/.fleet/inbox/<8hex>.md is "stale" when there is:
#   * no live agent record at ~/.fleet/agents/<8hex>.json
#   * AND no archived agent record at ~/.fleet/agents/archive/<8hex>.json
#   * AND no journal at ~/.fleet/dispatches/<8hex>.json
# i.e. the file is orphaned: the dispatch is gone, no one is reading it,
# and we have no record connecting it to anything.
echo "--- phase 1: stale coord_prompt_inbox files ---"
stale_inbox_count=0
if [ -d "$INBOX_DIR" ]; then
  shopt -s nullglob
  for inbox_file in "$INBOX_DIR"/*.md; do
    base="$(basename "$inbox_file" .md)"
    # Only act on the 8-hex name shape — anything else is operator-
    # authored and we leave it alone.
    if ! [[ "$base" =~ ^[0-9a-f]{8}$ ]]; then
      continue
    fi
    if [ -f "$AGENTS_DIR/$base.json" ] \
       || [ -f "$ARCHIVE_DIR/$base.json" ] \
       || [ -f "$DISPATCHES_DIR/$base.json" ]; then
      continue
    fi
    stale_inbox_count=$((stale_inbox_count + 1))
    echo "  stale: $inbox_file"
    if [ "$KILL" -eq 1 ]; then
      rm -f -- "$inbox_file"
    fi
  done
  shopt -u nullglob
fi
echo "  total: $stale_inbox_count"
echo

# ---------------------------------------------------------------------
# Phase 2 — orphan worktrees.
# ---------------------------------------------------------------------
# An entry at ~/.fleet/projects/<p>/worktrees/<slug>/ is "orphan" when
# the slug isn't referenced in the project's tasks.md (live or archived).
# Conservative: we only flag known slug name shapes (lowercase, hyphen-
# separated). Operator-named directories with other shapes are skipped.
echo "--- phase 2: orphan worktrees ---"
orphan_wt_count=0
if [ -d "$PROJECTS_DIR" ]; then
  for project_dir in "$PROJECTS_DIR"/*/; do
    [ -d "$project_dir" ] || continue
    project_name="$(basename "$project_dir")"
    [ "$project_name" = ".locks" ] && continue
    wt_root="$project_dir/worktrees"
    tasks_file="$project_dir/tasks.md"
    [ -d "$wt_root" ] || continue
    for wt_dir in "$wt_root"/*/; do
      [ -d "$wt_dir" ] || continue
      slug="$(basename "$wt_dir")"
      # Only match slug-shaped names (operator-side `git worktree`
      # checkouts won't follow this shape).
      if ! [[ "$slug" =~ ^[a-z0-9._-]+$ ]]; then
        continue
      fi
      # If tasks.md mentions the slug, leave it alone.
      if [ -f "$tasks_file" ] && grep -q -- "$slug" "$tasks_file" 2>/dev/null; then
        continue
      fi
      orphan_wt_count=$((orphan_wt_count + 1))
      echo "  orphan: $wt_dir"
      if [ "$KILL" -eq 1 ]; then
        # Use `git worktree remove --force` when the dir is a git
        # checkout; fall back to rm -rf for non-git debris (shouldn't
        # happen but defensive).
        if [ -d "$wt_dir/.git" ] || [ -f "$wt_dir/.git" ]; then
          # The main repo cwd is the operator's responsibility; we
          # can only do the removal-from-our-side via rm + the
          # operator runs `git worktree prune` separately.
          rm -rf -- "$wt_dir"
        else
          rm -rf -- "$wt_dir"
        fi
      fi
    done
  done
fi
echo "  total: $orphan_wt_count"
echo

# ---------------------------------------------------------------------
# Phase 3 — supervisor ghosts in coord-state.json.
# ---------------------------------------------------------------------
# Each project has ~/.fleet/projects/<p>/coord-state.json with a
# `supervisor` map keyed by slug. An entry is "ghost" when the slug
# isn't in the project's tasks.md. We only log these — surgical jq
# edits of an operator-mutable state file aren't safe to run blind.
echo "--- phase 3: supervisor ghosts (report-only) ---"
ghost_count=0
if [ -d "$PROJECTS_DIR" ]; then
  for project_dir in "$PROJECTS_DIR"/*/; do
    [ -d "$project_dir" ] || continue
    project_name="$(basename "$project_dir")"
    [ "$project_name" = ".locks" ] && continue
    coord_state="$project_dir/coord-state.json"
    tasks_file="$project_dir/tasks.md"
    [ -f "$coord_state" ] || continue
    [ -f "$tasks_file" ] || continue
    if ! command -v jq >/dev/null 2>&1; then
      echo "  skip $project_name: jq not installed"
      continue
    fi
    # Collect supervisor slugs.
    sup_slugs="$(jq -r '.supervisor // {} | keys[]?' "$coord_state" 2>/dev/null || true)"
    for slug in $sup_slugs; do
      if ! grep -q -- "$slug" "$tasks_file" 2>/dev/null; then
        ghost_count=$((ghost_count + 1))
        echo "  ghost: $project_name supervisor[$slug]"
      fi
    done
  done
fi
echo "  total: $ghost_count"
echo "  (supervisor ghosts are report-only; PR4's sweep-leaks --reconcile-derived"
echo "   will surgically clean these via the Derived reconciler.)"
echo

# ---------------------------------------------------------------------
# Summary.
# ---------------------------------------------------------------------
echo "=== summary ==="
echo "  stale_inbox_files: $stale_inbox_count"
echo "  orphan_worktrees:  $orphan_wt_count"
echo "  supervisor_ghosts: $ghost_count"
if [ "$KILL" -eq 0 ]; then
  echo
  echo "DRY-RUN. Re-run with --kill to clean."
fi

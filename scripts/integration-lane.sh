#!/usr/bin/env bash
# integration-lane.sh — run exactly the //go:build integration tests of one
# package.
#
# usage: scripts/integration-lane.sh <pkg> [extra go test flags]
#   e.g. FLEET_STANDBY_TIMEOUT=3s scripts/integration-lane.sh ./cmd/fleet
#
# The `integration` build tag only ADDS files — it does not subtract the
# default ones — so a bare `go test -tags=integration <pkg>` re-runs the whole
# default suite that the `go test -race ./...` lane already covered. This
# script derives the integration-ONLY set (names present in the tagged build
# and absent from the default build) and -run-scopes to it, which ci.yml used
# to hand-maintain per package. A new `//go:build integration` test is picked
# up automatically; an empty set is a no-op (exit 0), not a failure.
#
# Fail-closed: a build error in either listing aborts (set -e) rather than
# degrading to an empty set.
set -euo pipefail

if [ "$#" -lt 1 ]; then
    echo "usage: $0 <pkg> [extra go test flags]" >&2
    exit 2
fi
pkg=$1
shift

default_list=$(go test -count=1 -list . "$pkg")
tagged_list=$(go test -count=1 -tags=integration -list . "$pkg")

only=$(comm -13 \
    <(printf '%s\n' "$default_list" | grep '^Test' | sort) \
    <(printf '%s\n' "$tagged_list" | grep '^Test' | sort) \
    | paste -sd'|' -)

if [ -z "$only" ]; then
    echo "no integration-only tests in $pkg"
    exit 0
fi

exec go test -tags=integration -count=1 -timeout=5m -run "^(${only})\$" "$@" "$pkg"

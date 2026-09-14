#!/usr/bin/env bash
# Run only the tests of <pkg> that exist in the `integration` build and not in
# the default build (the tag adds files, so a bare -tags=integration run would
# repeat the default suite). Empty set: print a message, exit 0. A build error
# in either listing aborts (set -e).
#
# usage: scripts/integration-lane.sh <pkg> [extra go test flags]
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

# Testing Fleet — the `## Sandbox` for Fleet-the-project

How **Fleet itself** is run, observed and gated by a worker. The worker prompt is project-agnostic — it reads
only the merged standards' `## Sandbox` / `## Gates` — so every project writes its own `## Sandbox`; this is Fleet's.

## 1. Fleet's project standards (paste via `fleet standards edit --project fleet`)

```markdown
## Sandbox
tier: local
up: scripts/scenario.sh up            # builds $FLEET_DEV_BIN, prints export FLEET_HOME/FLEET_TMUX_SOCKET/PATH(fake claude)
down: scripts/scenario.sh down
isolation: namespace
observe: scripts/scenario.sh capture   # tmux pane text; plus cat of state/queue files named in the row
credentials:
notes: fake claude modes ok|exit|crash-once|hang via scripts/fake-claude.sh; see docs/TESTING.md

## Gates
- go build ./...
- gofmt -l .
- golangci-lint run ./...
- go test -race -count=1 -timeout=5m ./...
- FLEET_STANDBY_TIMEOUT=3s bash scripts/integration-lane.sh ./cmd/fleet
- python3 -m pytest skills/ scripts/ -q
- bash scripts/lint-test-isolation.sh
baseline: git worktree add /tmp/fleet-base-$FLEET_WORKER_SLUG origin/main && cd there && <same gate>

## Testing (Fleet addendum)
Distinct H2 on purpose — a project `## Testing` would SHADOW the global rules, not extend them.
stdlib `testing` / pytest only; e2e tests are `//go:build integration` in cmd/fleet or
internal/tui using internal/testutil/coorde2e; integ may use tmuxfake / tmuxtest.
```

## 2. What `up` does — build the product under test, isolate the sandbox

`scripts/scenario.sh` (PR #308) wraps this recipe as `up`/`down`/`capture`; until it lands, run it by hand. Everything `up` creates is keyed by `$FLEET_WORKER_SLUG`.

```sh
FLEET_DEV_BIN=$(mktemp -d)/fleet && go build -o "$FLEET_DEV_BIN" ./cmd/fleet
export FLEET_HOME=$(mktemp -d) FLEET_TMUX_SOCKET=/tmp/fleet-test-$FLEET_WORKER_SLUG-$$.sock FLEET_STANDBY_TIMEOUT=3s
```

Never test against the `fleet` on PATH — it is the operator's binary. `FLEET_HOME` keeps state out of
`~/.fleet`; `FLEET_TMUX_SOCKET` is passed to tmux as `-S <path>` (absolute — a bare name lands in your cwd)
and keeps panes off the operator's default server (`internal/tmux` refuses it under `go test`);
`FLEET_STANDBY_TIMEOUT` makes a leftover standby self-reap. First-run auto-init writes skills into
`$HOME/.claude`, so run the binary with `HOME=$FLEET_HOME` too. Fake claude (`coorde2e.FakeClaude(t, dir)` +
`coorde2e.SetMode`, or by hand) stands in for the real CLI — `ok` prints a banner and acks each stdin line
`fake claude: got prompt (<n> chars)`; `exit` exits 1 at startup:

```sh
FAKE=$(mktemp -d); printf '#!/bin/sh\necho "fake claude: ready pid $$"\nwhile IFS= read -r l; do echo "fake claude: got prompt (${#l} chars)"; done\n' > "$FAKE/claude"
chmod +x "$FAKE/claude" && export PATH="$FAKE:$PATH"
REPO=$(mktemp -d) && "$FLEET_DEV_BIN" project add --project demo "$REPO"     # seed (coorde2e.SeedProject)
```

## 3. Trigger, `capture`, `down`

Run the exact argv the product runs (`coorde2e.Dispatch` / `DispatchArgs` is the TUI's `[a]`), read the observable,
paste it verbatim into `verification.md` (`## Evidence — before` / `— after`, PASS/FAIL per S<n>):

```sh
HOME=$FLEET_HOME "$FLEET_DEV_BIN" dispatch coord-demo --project demo --coord-spawn --prompt "Run /coordinator now." --cwd "$REPO" --engine claude-code
tmux -S "$FLEET_TMUX_SOCKET" capture-pane -p -t "$(tmux -S "$FLEET_TMUX_SOCKET" list-sessions -F '#S')"   # capture
cat "$FLEET_HOME"/agents/*.json
tmux -S "$FLEET_TMUX_SOCKET" kill-server; rm -rf "$FLEET_HOME" "$REPO" "$FAKE"                            # down
```

## 4. Where tests live, how the gates run them, baseline

- **e2e** (`//go:build integration`, real binary + real tmux via `coorde2e`): `cmd/fleet/`, `internal/tui/`.
  Isolate with `tmuxtest.RequireTmux(t)` + `t.Setenv("FLEET_HOME", …)`. The CI lane discovers
  `//go:build integration` tests automatically — `scripts/integration-lane.sh <pkg>` runs exactly the tests present
  in the tagged build and absent from the default one, so there is no `-run` list to add your name to:
  `FLEET_STANDBY_TIMEOUT=3s bash scripts/integration-lane.sh ./cmd/fleet`
- **integ** (real FS / flock / in-process `tmuxfake`): the package's own `_test.go`, no build tag. **unit**:
  only pure logic a scenario cannot reach (`unit — pure`). `bash scripts/lint-test-isolation.sh` is the static
  tripwire for tests that reach tmux without isolation.
- **Baseline** (`baseline:` above): `git worktree add /tmp/fleet-base-$FLEET_WORKER_SLUG origin/main`, run the same
  gate there, record both outputs under `## Baseline`. Red that does not reproduce there is yours.

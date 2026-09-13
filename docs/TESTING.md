# Testing Fleet — scenario-first

The bar is `## Testing` in `templates/standards.md`: reproduce the scenario on the built product,
encode what you saw as a test at the boundary the operator sees, verify by hand *and* via the test.
Harness: `internal/testutil/coorde2e` (built binary + real tmux), `tmuxfake` (in-process tmux),
`tmuxtest` (isolation helpers).

## 1. Build the product under test, isolate the sandbox

```sh
FLEET_DEV_BIN=$(mktemp -d)/fleet && go build -o "$FLEET_DEV_BIN" ./cmd/fleet
export FLEET_HOME=$(mktemp -d) FLEET_TMUX_SOCKET=/tmp/fleet-test-<slug>-$$.sock FLEET_STANDBY_TIMEOUT=3s
```

Never test against the `fleet` on PATH — it is the operator's binary.

`FLEET_HOME` keeps state out of `~/.fleet`; `FLEET_TMUX_SOCKET` is passed to tmux as `-S <path>` (absolute —
a bare name lands in your cwd) and keeps panes off the operator's default server (`internal/tmux` refuses
it under `go test`); `FLEET_STANDBY_TIMEOUT` makes a leftover standby self-reap. First-run auto-init
writes skills into `$HOME/.claude`, so run the binary with `HOME=$FLEET_HOME` too.

## 2. Fake claude

`coorde2e.FakeClaude(t, dir)` writes an executable `claude` shim into `dir` (put it first on `PATH`) and
returns its mode file; `coorde2e.SetMode` picks `ok` (banner, reads stdin forever, acks each line
`fake claude: got prompt (<n> chars)`) or `exit` (exits 1 at startup; pane + supervisor go away). By hand:

```sh
FAKE=$(mktemp -d); printf '#!/bin/sh\necho "fake claude: ready pid $$"\nwhile IFS= read -r l; do echo "fake claude: got prompt (${#l} chars)"; done\n' > "$FAKE/claude"
chmod +x "$FAKE/claude" && export PATH="$FAKE:$PATH"
```

## 3. Seed state

`repo := coorde2e.SeedProject(t, project)` registers a non-git project on a temp repo;
`coorde2e.SeedInFlightWorker(t, project, slug, workerID, phase)` records an in-flight worker. By hand:

```sh
REPO=$(mktemp -d) && "$FLEET_DEV_BIN" project add --project demo "$REPO"
```

## 4. Trigger, observe, tear down

Run the exact argv the product runs (`coorde2e.Dispatch` / `DispatchArgs` is the TUI's `[a]`), then read the observable:

```sh
HOME=$FLEET_HOME "$FLEET_DEV_BIN" dispatch coord-demo --project demo --coord-spawn --prompt "Run /coordinator now." --cwd "$REPO" --engine claude-code
tmux -S "$FLEET_TMUX_SOCKET" list-sessions
tmux -S "$FLEET_TMUX_SOCKET" capture-pane -p -t "$(tmux -S "$FLEET_TMUX_SOCKET" list-sessions -F '#S')"
cat "$FLEET_HOME"/agents/*.json
tmux -S "$FLEET_TMUX_SOCKET" kill-server; rm -rf "$FLEET_HOME" "$REPO" "$FAKE"
```

Paste what you saw verbatim (pane text, file contents, exit code) into `verification.md` under
`## Evidence — before` / `— after`, PASS/FAIL per S<n> row.

## 5. Where tests live and how CI runs them

- **e2e** (`//go:build integration`, real binary + real tmux via `coorde2e`): `cmd/fleet/` and
  `internal/tui/`. Isolate with `tmuxtest.RequireTmux(t)` + `t.Setenv("FLEET_HOME", …)`.
- **integ** (real FS / flock / in-process `tmuxfake`): the package's own `_test.go`, no build tag.
- **unit**: only for pure logic a scenario cannot reach (the contract row says `unit — <reason>`).

Integration lanes in `.github/workflows/ci.yml` are a hard-coded `-run` list per package; an
unlisted test never runs — add your new test's name. Run one lane locally exactly as CI does:

```sh
FLEET_STANDBY_TIMEOUT=3s go test -tags=integration -count=1 -timeout=5m -run '^(TestCoordLeaderCheck)$' ./cmd/fleet
```

`bash scripts/lint-test-isolation.sh` is the static tripwire for tests that reach tmux without isolation.

## 6. Baseline — proving a red gate is pre-existing

Re-run the exact failing command on untouched `origin/main`; record both outputs under `## Baseline`.
Red that does not reproduce there is yours: fix it before `review-pending`.

```sh
git worktree add /tmp/fleet-base-<slug> origin/main && (cd /tmp/fleet-base-<slug> && <exact gate command>)
```

"""Drive ONE real loop.py coord tick that dispatches a ready task, so the
Python fleetlog emitter writes coord.tick (start+end) + dispatch.worker to
the shared log dir. Invoked as a subprocess by the Go lifecycle test
(cmd/fleet/fleetlog_lifecycle_test.go) — the only practical way to exercise
the Python emitter from a Go test.

The three shell-out seams are stubbed in-process (mirroring the pytest
conftest) so the tick is hermetic and dispatch is inert:
  - loop._resolve_repo_fn -> echo the test cwd
  - loop._lease_check_fn  -> "owner" (proven)
  - loop._run_fleet       -> no-op (mutations are inert; the agent_id is
                             minted locally so dispatch.worker still fires)

argv: <skill_dir> <fleet_home> <cwd> <project> <slug>
"""
import datetime
import os
import sys

skill_dir, fleet_home, cwd, project, slug = sys.argv[1:6]
sys.path.insert(0, skill_dir)

import loop  # noqa: E402
import parse  # noqa: E402

# Hermetic tick env.
os.environ["FLEET_COORD_POLL_INTERVAL_S"] = "0"
os.environ["FLEET_COORD_POLL_BASE_INTERVAL_S"] = "0"
os.environ["FLEET_RC_BOOTSTRAP_DISABLED"] = "1"
os.environ.pop("FLEET_AGENT_ID", None)

# One ready task for the tick to dispatch.
pdir = os.path.join(fleet_home, "projects", project)
os.makedirs(os.path.join(pdir, ".locks"), exist_ok=True)
now = datetime.datetime(2026, 5, 6, 10, 0, 0, tzinfo=datetime.timezone.utc)
task = parse.Task(
    slug=slug, status="ready", priority="P1", worker_pid=0, pr_url="",
    created=now, updated=now, spawned_by="user", depends_on=[],
    spec="spec", acceptance="acc", notes="", dispatch_generation=0,
)
parse.write(
    os.path.join(pdir, "tasks.md"),
    parse.File(schema=parse.SCHEMA_VERSION, tasks=[task], footer=""),
)

# Stub the shell-out seams (mirror tests/conftest.py).
loop._resolve_repo_fn = lambda project, *, home, fleet_bin="fleet", cwd=None: (cwd, "")
loop._lease_check_fn = lambda project, *, home, fleet_bin="fleet": "owner"
loop._run_fleet = lambda cmd, timeout_s=30.0: None

result = loop.tick(project, coord_id="", cwd=cwd, fleet_home=fleet_home)
sys.stderr.write(
    f"dispatched={result.dispatched} skipped={result.skipped} "
    f"reason={result.reason}\n"
)
if result.dispatched != 1:
    sys.exit(3)
sys.exit(0)

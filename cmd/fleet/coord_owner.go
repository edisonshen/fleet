package main

// coord_owner.go — `fleet coord-owner --project <p> [--json]`. A thin READ-ONLY
// diagnostics window onto the coordinator LEASE identity for the skill-side
// Python tick (skills/coordinator/loop.py). It is NOT an attach source of truth:
// CLI/TUI attach must call coordreconcile.Resolve, which understands starting,
// fencing, handoff reservations, and claim tokens atomically.
//
// WHY THIS EXISTS: the coord-spawn marker used to be Python's identity reader —
// `_read_coord_spawn_marker(project_dir) == coord_id` answered "is THIS session
// the project's intended/successor coord, so it must never self-exit?"
// (loop.py::_classify_lock_busy gate 5). Deleting the marker
// (TASK-PLAN-coord-lease-sole-identity D5/D6) forces a lease-based replacement,
// but `fleet lease-check` only returns an exit-code verdict (owner/fenced/
// unknown) — it cannot report WHICH agent ID owns the lease or is the named
// handoff successor. This command prints those IDs so loop.py can re-key gate 5
// to "never self-exit when the lease names me" (live owner OR handoff successor).
//
// Output shape (--json, the only consumer today):
//
//	{"project":"p","live_owner_id":"<id|>","live_owner_present":<bool>,
//	 "identity_pending":<bool>,"live_owner_pid":<int>,"owner_id":"<id|>",
//	 "handoff_successor_id":"<id|>"}
//
// Every ID is "" when the lease has no such state (free lease, no handoff
// journal, or an unsupported platform). PR-2 (D9d) added the machine-readable
// live_owner_present / identity_pending / live_owner_pid fields so loop.py's
// gate-5 distinguishes a live-but-UNNAMED holder (present && identity_pending —
// skip self-exit) from a NAMED different live holder (present && !pending — the
// genuine duplicate, MUST still self-exit) rather than reading an empty
// live_owner_id as "no coord". The coordlock reads live in coord_owner_unix.go
// / coord_owner_other.go (build-tagged) because the lease primitive is
// linux||darwin only.

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// coordOwnerInfo is the JSON shape emitted by `fleet coord-owner --json`.
type coordOwnerInfo struct {
	Project string `json:"project"`
	// LiveOwnerID is coordlock.LiveOwner's agent id — the process-live flock
	// holder (busy⇒owner). "" when free OR when the holder's body has no
	// readable identity (identity-pending). The "am I the coord?" signal.
	LiveOwnerID string `json:"live_owner_id"`
	// LiveOwnerPresent is true whenever a live process holds the flock
	// (busy⇒owner, D9a), even if its identity is not yet readable. loop.py's
	// gate-5 skips self-exit ONLY when present && identity_pending.
	LiveOwnerPresent bool `json:"live_owner_present"`
	// IdentityPending is true when the flock is HELD but the body carries no
	// readable agent_id (old-binary straggler / torn body): present-but-unnamed.
	IdentityPending bool `json:"identity_pending"`
	// LiveOwnerPID is the flock holder's supervisor pid (0 when free). Lets
	// loop.py tell "unnamed but same pid as me" from a foreign holder.
	LiveOwnerPID int `json:"live_owner_pid"`
	// OwnerID is coordlock.CurrentOwner — the identity-gated flock holder.
	// Reported for completeness/diagnostics; loop.py keys on LiveOwnerID.
	OwnerID string `json:"owner_id"`
	// HandoffSuccessorID is coordlock.CurrentHandoffSuccessor — the write-once
	// handoff journal's in-flight successor id (process alive). PR-2 flock-only
	// replacement for the deleted epoch CurrentHandoff reservation.
	HandoffSuccessorID string `json:"handoff_successor_id"`
}

func newCoordOwnerCmd() *cobra.Command {
	var project string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "coord-owner --project <project> [--json]",
		Short: "Print the coordinator lease identity (owner + handoff successor) for a project",
		Long: "Report the agent IDs the coordinator lease currently names for " +
			"--project: the process-live active owner (live_owner_id), the " +
			"TTL-gated active owner (owner_id), and any in-flight handoff " +
			"successor reservation (handoff_successor_id). Each is empty when the " +
			"lease has no such state (free lease / no reservation / unsupported " +
			"platform). Read-only; never mutates the lease. Diagnostics only; " +
			"attach paths use coordreconcile.Resolve instead.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCoordOwner(project, asJSON, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project whose lease identity to report (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a single JSON object")
	return cmd
}

func runCoordOwner(project string, asJSON bool, stdout io.Writer) error {
	if project == "" {
		return fmt.Errorf("coord-owner: --project is required")
	}
	info := coordOwnerLeaseIdentity(project) // build-tagged
	info.Project = project
	if asJSON {
		enc := json.NewEncoder(stdout)
		return enc.Encode(info)
	}
	_, _ = fmt.Fprintf(stdout, "project:              %s\n", info.Project)
	_, _ = fmt.Fprintf(stdout, "live_owner_id:        %s\n", info.LiveOwnerID)
	_, _ = fmt.Fprintf(stdout, "live_owner_present:   %t\n", info.LiveOwnerPresent)
	_, _ = fmt.Fprintf(stdout, "identity_pending:     %t\n", info.IdentityPending)
	_, _ = fmt.Fprintf(stdout, "live_owner_pid:       %d\n", info.LiveOwnerPID)
	_, _ = fmt.Fprintf(stdout, "owner_id:             %s\n", info.OwnerID)
	_, _ = fmt.Fprintf(stdout, "handoff_successor_id: %s\n", info.HandoffSuccessorID)
	return nil
}

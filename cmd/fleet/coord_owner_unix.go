//go:build linux || darwin

package main

import "github.com/edisonshen/fleet/internal/coordlock"

// coordOwnerLeaseIdentity reads the coordinator lease identity for project via
// coordlock (linux/darwin only). Each field is "" when the corresponding lease
// state is absent. The reads are read-only (no CAS, no renewal) and safe to run
// from any process — they only stat/parse the on-disk epoch record.
func coordOwnerLeaseIdentity(project string) coordOwnerInfo {
	var info coordOwnerInfo
	if o, ok := coordlock.LiveOwner(project); ok {
		info.LiveOwnerID = o.AgentID
	}
	if o, ok := coordlock.CurrentOwner(project); ok {
		info.OwnerID = o.AgentID
	}
	if h, ok := coordlock.CurrentHandoff(project); ok {
		info.HandoffSuccessorID = h.SuccessorID
	}
	return info
}

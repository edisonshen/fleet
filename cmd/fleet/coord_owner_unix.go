//go:build linux || darwin

package main

import "github.com/edisonshen/fleet/internal/coordlock"

// coordOwnerLeaseIdentity reads the coordinator lease identity for project via
// coordlock (linux/darwin only). The reads are read-only (no CAS, no renewal)
// and safe to run from any process — they only LOCK_SH-probe the flock + parse
// its body. PR-2 (D9d): LiveOwner reports busy⇒present regardless of identity,
// so a held-but-unnamed flock serializes as present=true, identity_pending=true
// (not an empty "no coord").
func coordOwnerLeaseIdentity(project string) coordOwnerInfo {
	var info coordOwnerInfo
	if o, ok := coordlock.LiveOwner(project); ok {
		info.LiveOwnerPresent = true
		info.LiveOwnerID = o.AgentID
		info.LiveOwnerPID = o.PID
		info.IdentityPending = o.AgentID == ""
	}
	if o, ok := coordlock.CurrentOwner(project); ok {
		info.OwnerID = o.AgentID
	}
	if succ, ok := coordlock.CurrentHandoffSuccessor(project); ok {
		info.HandoffSuccessorID = succ
	}
	return info
}

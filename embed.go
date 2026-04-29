// Package fleet at the repo root exists for one purpose: anchor the
// //go:embed directive that ships the fleet-guard skill files inside the
// fleet binary.
//
// Go embed patterns must resolve relative to the source file containing
// the //go:embed directive — and they cannot escape that directory with
// `..`. The skill lives at `<repo>/skills/fleet-guard/`, so the directive
// has to live at `<repo>/<file>.go` to reach it. There is no other place
// in the tree from which the pattern resolves cleanly.
//
// `cmd/fleet/init.go` imports this package and walks FleetGuardFS() to
// install the skill on the operator's machine.
package fleet

import (
	"embed"
	"io/fs"
)

// Tests are deliberately excluded — they exist to validate the skill in
// CI and developer machines, not to ship in the operator's binary. Each
// runtime file is listed explicitly so a stray new file (e.g., __pycache__,
// editor backups) doesn't silently bloat the binary.
//
//go:embed skills/fleet-guard/SKILL.md skills/fleet-guard/main.py skills/fleet-guard/health.py skills/fleet-guard/handoff.py skills/fleet-guard/inbox.py skills/fleet-guard/ids.py
var skillRaw embed.FS

// FleetGuardFS returns the embedded skill files as a filesystem rooted at
// the skill directory itself, so callers see "SKILL.md" and "main.py" as
// top-level entries rather than paths under "skills/fleet-guard/".
//
// The fs.Sub call cannot fail at runtime because the prefix exists in the
// embedded FS at compile time — a panic here means the //go:embed directive
// above lost the skill directory, which is a build-time configuration bug.
func FleetGuardFS() fs.FS {
	sub, err := fs.Sub(skillRaw, "skills/fleet-guard")
	if err != nil {
		panic("fleet: //go:embed skills/fleet-guard configuration drift: " + err.Error())
	}
	return sub
}

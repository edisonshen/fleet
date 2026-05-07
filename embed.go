// Package fleet at the repo root exists for one purpose: anchor the
// //go:embed directives that ship the bundled skill files and seed
// templates inside the fleet binary.
//
// Go embed patterns must resolve relative to the source file containing
// the //go:embed directive — and they cannot escape that directory with
// `..`. The skills live at `<repo>/skills/<name>/` and the seed templates
// at `<repo>/templates/`, so the directives have to live at
// `<repo>/<file>.go` to reach them. There is no other place in the tree
// from which the patterns resolve cleanly.
//
// `cmd/fleet/init.go` imports this package and walks the FS helpers
// below to install the skills + seed templates on the operator's
// machine.
package fleet

import (
	"embed"
	"io/fs"
)

// fleet-guard runtime files. Tests are deliberately excluded — they
// exist to validate the skill in CI and developer machines, not to
// ship in the operator's binary. Each runtime file is listed
// explicitly so a stray new file (e.g., __pycache__, editor backups)
// doesn't silently bloat the binary.
//
//go:embed skills/fleet-guard/SKILL.md skills/fleet-guard/main.py skills/fleet-guard/health.py skills/fleet-guard/handoff.py skills/fleet-guard/inbox.py skills/fleet-guard/ids.py
var fleetGuardRaw embed.FS

// coordinator runtime files. Mirrors fleet-guard's explicit-file
// pattern: tests/ excluded so __pycache__ / .pytest_cache never leak
// into the binary, and any new skill file requires a deliberate
// addition here (catches typos that would silently ship a half-skill).
//
//go:embed skills/coordinator/SKILL.md skills/coordinator/loop.py skills/coordinator/parse.py skills/coordinator/dispatch.py skills/coordinator/conflict.py skills/coordinator/worktree.py
var coordinatorRaw embed.FS

// Seed templates rendered into ~/.fleet/ on first init. Currently just
// the canonical standards.md template; expand the directive when more
// templates land. Listed by full path because templates/ holds
// operator-facing seed files and must not pick up stray dotfiles.
//
//go:embed templates/standards.md
var templatesRaw embed.FS

// FleetGuardFS returns the embedded fleet-guard skill files as a
// filesystem rooted at the skill directory itself, so callers see
// "SKILL.md" and "main.py" as top-level entries rather than paths
// under "skills/fleet-guard/".
//
// The fs.Sub call cannot fail at runtime because the prefix exists in the
// embedded FS at compile time — a panic here means the //go:embed directive
// above lost the skill directory, which is a build-time configuration bug.
func FleetGuardFS() fs.FS {
	sub, err := fs.Sub(fleetGuardRaw, "skills/fleet-guard")
	if err != nil {
		panic("fleet: //go:embed skills/fleet-guard configuration drift: " + err.Error())
	}
	return sub
}

// CoordinatorFS returns the embedded coordinator skill files as a
// filesystem rooted at the skill directory itself. Same shape as
// FleetGuardFS so the install path can walk both with one helper.
func CoordinatorFS() fs.FS {
	sub, err := fs.Sub(coordinatorRaw, "skills/coordinator")
	if err != nil {
		panic("fleet: //go:embed skills/coordinator configuration drift: " + err.Error())
	}
	return sub
}

// SkillFS returns every bundled skill keyed by skill directory name
// ("fleet-guard", "coordinator"). Each value is rooted at the skill
// directory itself (so the install path can write SKILL.md / main.py /
// loop.py at the top level). Adding a new skill means adding the
// //go:embed directive AND wiring it into this map — a single source
// of truth so cmd/fleet/init.go's "install all skills" loop stays
// trivially correct.
func SkillFS() map[string]fs.FS {
	return map[string]fs.FS{
		"fleet-guard": FleetGuardFS(),
		"coordinator": CoordinatorFS(),
	}
}

// StandardsTemplate returns the bytes of the canonical
// ~/.fleet/standards.md seed (frontmatter + "# Standards" H1 + Testing
// + Code review sections). Used by `fleet init` to bootstrap a global
// standards file when one is missing. ReadFile is cheap (the bytes
// are in the binary), so callers don't need to cache the result.
//
// A read failure here means the embedded template file went missing
// at compile time — a build configuration bug, not a runtime concern.
func StandardsTemplate() []byte {
	data, err := templatesRaw.ReadFile("templates/standards.md")
	if err != nil {
		panic("fleet: //go:embed templates/standards.md configuration drift: " + err.Error())
	}
	return data
}

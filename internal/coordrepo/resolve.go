// Package coordrepo is the SINGLE shared repo-binding resolver: it maps a
// project tag to the git checkout on disk a coordinator runs all of that
// project's git work against (worktree add, fetch, branch, PR watch). The
// binding is a property of the PROJECT, NEVER of the folder `fleet` was
// launched in — there is deliberately ZERO os.Getwd in this package.
//
// DESIGN-coord-repo-binding-from-project.md (Option C) — the tier ladder:
//
//	resolveProjectRepo(project, persist)
//	    │
//	    ├─ projects.Read(project)
//	    │     read error (not ErrNotFound) → ErrRepoUnresolved (meta unreadable)
//	    │     non-git project              → bind repo_path if isDir, else refuse
//	    │
//	    ├─ TIER 1  meta.RepoPath isDir (operator pin; NOT fuzzy-gated):
//	    │     stored fp matches  → bind
//	    │     stored fp mismatch → REFUSE (cross-machine stale, no write)
//	    │     no stored fp       → bind on existence (operator authority);
//	    │                          opportunistically mint+persist when
//	    │                          persist && non-shallow; shallow → warn
//	    │
//	    ├─ TIER 2  deriveStrong (strong-signal gated):
//	    │     per-worktree base via gc.ProjectWorktreeBases;
//	    │     all registered + (if >1) agree on ONE repo; gate:
//	    │       stored fp match  OR  (legacy) >=2 agree AND fuzzy heuristic.
//	    │     persist? → mintFingerprint + projects.SetRepoPath (CAS)
//	    │
//	    └─ TIER 3  ErrRepoUnresolved (refuse + actionable hint)
//
// Two fingerprint functions, never one: repoIdentity is READ-ONLY (never
// writes .git/config); mintFingerprint is WRITE-ALLOWED and runs ONLY on
// an accepted persist. A comparison / no-persist path must never write
// Fleet state into a (possibly wrong) repo.
package coordrepo

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edisonshen/fleet/internal/gc"
	"github.com/edisonshen/fleet/internal/projects"
)

// errShallow is the sentinel returned by repoIdentity/mintFingerprint for
// a shallow clone: its root commit is the shallow boundary, not the real
// root, so the fingerprint would be unstable (it changes after
// `git fetch --unshallow` / against a full clone). Callers bind on
// existence and surface a `git fetch --unshallow` hint instead of storing
// an unstable value.
var errShallow = errors.New("coordrepo: repo is a shallow clone — cannot compute a stable fingerprint")

// ErrAmbiguousWorktrees: the project's worktrees derive to DIFFERENT base
// repos (a contaminated/mixed tree). Refuse rather than pick one.
type ErrAmbiguousWorktrees struct {
	Project string
	Repos   []string
}

func (e ErrAmbiguousWorktrees) Error() string {
	return fmt.Sprintf("coordrepo: project %q worktrees derive to different repos %v — ambiguous, refusing", e.Project, e.Repos)
}

// ErrWeakDerivation: a derivation exists but the signal is too weak to
// persist (single worktree with no stored fingerprint, heuristic miss,
// no-origin legacy repo, fingerprint mismatch on the derived repo).
type ErrWeakDerivation struct {
	Project string
	Reason  string
}

func (e ErrWeakDerivation) Error() string {
	return fmt.Sprintf("coordrepo: project %q derivation too weak to trust (%s)", e.Project, e.Reason)
}

// ErrRepoUnresolved is the tier-3 refusal. It carries the project name and
// the per-source rejection reasons so callers (PR3 TUI flash, PR4 stderr)
// render the exact §7.1 hint via Error(). Candidate is the launch cwd IF
// it is a git checkout, else "" (Error() emits the placeholder).
type ErrRepoUnresolved struct {
	Project string
	// Source-keyed rejection reasons; empty string means "not evaluated".
	MetaReason     string // meta.json line ("absent" | "...missing on disk" | "...different repo...")
	WorktreeReason string // worktrees line ("none" | "ambiguous..." | "single worktree...")
	// Candidate is a path the operator can pin (the launch cwd when it is a
	// git checkout). Set by the caller — the resolver never reads cwd.
	Candidate string
	// Rejected is a freeform fallback used when a single coarse reason is
	// enough (e.g. "meta.json unreadable: ..."); when set it overrides the
	// structured Meta/Worktree lines.
	Rejected string
}

func (e ErrRepoUnresolved) Error() string {
	meta := e.MetaReason
	if meta == "" {
		meta = "absent"
	}
	wt := e.WorktreeReason
	if wt == "" {
		wt = "none"
	}
	candidate := e.Candidate
	if candidate == "" {
		candidate = "<path-to-checkout>"
	}
	if e.Rejected != "" {
		// Coarse single-reason refusal (e.g. unreadable meta) still gets the
		// full actionable shape so callers render one consistent hint.
		meta = e.Rejected
	}
	var b strings.Builder
	fmt.Fprintf(&b, "project %s: no usable checkout — cannot attach a coordinator.\n", e.Project)
	fmt.Fprintf(&b, "  - meta.json: %s\n", meta)
	fmt.Fprintf(&b, "  - worktrees: %s\n", wt)
	b.WriteString("  - launch cwd was intentionally ignored (binding is a property of the\n")
	b.WriteString("    project, not where you ran fleet).\n")
	fmt.Fprintf(&b, "Run: fleet project add --project %s %s", e.Project, candidate)
	return b.String()
}

// warnf emits a one-time surfaced notice to stderr (feedback_surface_dont_silo:
// a needs-attention state must be loud, never silently auto-recovered).
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fleet: "+format+"\n", args...)
}

// isDir reports whether p exists and is a directory.
func isDir(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// ResolveProjectRepo is the exported entry point (downstream consumers:
// internal/tui, cmd/fleet, and the orphan-worktree reaper design). persist
// gates whether a tier-2 derivation is written back to meta.json — the
// operator-initiated paths (attach/recovery/handoff) pass true; routine
// read-only ticks pass false. Tier 1 and tier 3 never persist regardless.
func ResolveProjectRepo(project string, persist bool) (string, error) {
	return resolveOnce(project, persist, 0)
}

// maxResolveRetries bounds the persist-lost-race re-resolution. A lost CAS
// race means a concurrent `project add` won and durably wrote a pin, so the
// very next read hits tier 1 and terminates. The bound guards against a
// pathological ping-pong of concurrent racing writers from looping forever
// — after the bound we refuse + surface rather than spin.
const maxResolveRetries = 3

// resolveProjectRepo runs the tier ladder once (top-level entry). NO
// os.Getwd on any path.
func resolveProjectRepo(project string, persist bool) (string, error) {
	return resolveOnce(project, persist, 0)
}

// resolveOnce is resolveProjectRepo with an explicit retry depth so the
// lost-race re-resolution is bounded (see maxResolveRetries).
func resolveOnce(project string, persist bool, depth int) (string, error) {
	meta, err := projects.Read(project)
	// DISTINGUISH meta read errors: only ErrNotFound permits tier-2
	// derivation. A malformed / EACCES meta.json is a hand-edit gone wrong,
	// NOT "no project" — refuse + surface (even on read-only ticks).
	if err != nil && !errors.Is(err, projects.ErrNotFound) {
		return "", ErrRepoUnresolved{Project: project,
			Rejected: "meta.json unreadable: " + err.Error()}
	}
	metaPresent := err == nil

	// ABSOLUTE-PATH GUARD — the load-bearing "cwd is never a tier" invariant.
	// `fleet project add` always stores an absolute, cleaned repo_path, but a
	// LEGACY or HAND-EDITED meta.json can carry a RELATIVE one. A relative
	// path makes os.Stat / `git -C <rel>` resolve against the process launch
	// cwd — silently reintroducing the exact cwd-binding leak Option C forbids
	// (a launch dir holding `./repo` would hijack a legacy pin, and persist
	// would first-certify the cwd-relative checkout). Refuse + surface before
	// ANY isDir / git probe touches the path. Empty repo_path falls through
	// (tier-2 derive / refuse handles it; "" is not a cwd hijack).
	if metaPresent && meta.RepoPath != "" && !filepath.IsAbs(meta.RepoPath) {
		return "", ErrRepoUnresolved{Project: project,
			MetaReason: fmt.Sprintf("repo_path %q is not absolute — refusing (a relative path would bind against the launch cwd; re-pin with `fleet project add`)", meta.RepoPath)}
	}

	// NON-GIT short-circuit — no git identity, no worktrees to derive.
	if metaPresent && !meta.GitMode() {
		if isDir(meta.RepoPath) {
			return meta.RepoPath, nil // dir-exists is the only check
		}
		return "", ErrRepoUnresolved{Project: project,
			MetaReason: nonGitMissingReason(meta.RepoPath)}
	}

	// TIER 1 — operator pin (git project; NOT gated by the fuzzy heuristic).
	if metaPresent && meta.RepoPath != "" && isDir(meta.RepoPath) {
		path, done, terr := tier1(project, meta, persist)
		if errors.Is(terr, errPinRetry) {
			// first-certify lost the race → re-resolve so tier 1 binds the
			// winning operator pin (never the stale path). Bounded retry.
			if depth >= maxResolveRetries {
				return "", ErrRepoUnresolved{Project: project,
					Rejected: "repo pin kept changing under concurrent writers — retry the attach"}
			}
			return resolveOnce(project, persist, depth+1)
		}
		if done {
			return path, terr
		}
		// not done → reserved for future fall-through; today an existing
		// on-disk pin always decides at tier 1 (bind / refuse / certify).
	}

	// TIER 2 — derive from worktrees, STRONG-signal gated, then persist.
	derived, derr := deriveStrong(project, meta)
	if derr == nil && derived != "" {
		if persist {
			fp, ferr := mintFingerprint(derived)
			if ferr != nil && ferr != errShallow {
				return "", ferr
			}
			if ferr == nil {
				// prevPath = meta.RepoPath observed at start ("" if absent).
				if perr := projects.SetRepoPath(project, meta.RepoPath, derived, fp); perr != nil {
					// A `project add` for a DIFFERENT repo won the race and
					// pinned identity mid-flight. Our derived path is now
					// possibly wrong → re-resolve from the freshly-written meta
					// (tier 1 uses the operator pin). Bounded retry.
					if errors.As(perr, &projects.ErrPinLostRace{}) {
						if depth >= maxResolveRetries {
							return "", ErrRepoUnresolved{Project: project,
								Rejected: "repo pin kept changing under concurrent writers — retry the attach"}
						}
						return resolveOnce(project, persist, depth+1)
					}
					return "", perr
				}
			}
		}
		return derived, nil
	}

	// TIER 3 — refuse + surface.
	return "", ErrRepoUnresolved{Project: project,
		MetaReason:     tier3MetaReason(metaPresent, meta),
		WorktreeReason: tier3WorktreeReason(derr)}
}

// tier1 handles the operator-pin path (meta.RepoPath exists on disk).
// Returns (path, done=true, err) when tier 1 decides; (·, done=false, nil)
// is never returned for an existing path — every on-disk-pin case decides
// here (bind, refuse, or first-certify). The bool exists so a caller could
// fall through in the future without restructuring; today an existing pin
// always resolves at tier 1.
func tier1(project string, meta projects.Meta, persist bool) (string, bool, error) {
	if meta.RepoFingerprint != "" {
		id, ierr := repoIdentity(meta.RepoPath) // READ-ONLY compare
		if ierr == nil && id == meta.RepoFingerprint {
			return meta.RepoPath, true, nil // strong match
		}
		// path exists but is a DIFFERENT repo (cross-machine stale), or the
		// identity is uncomputable → refuse, don't re-derive, NO write.
		return "", true, ErrRepoUnresolved{Project: project,
			MetaReason: fmt.Sprintf("repo_path %s is a different repo (fingerprint mismatch — pin is stale on this machine)", meta.RepoPath)}
	}

	// NO stored fingerprint (legacy meta). The pin must be a USABLE git
	// checkout — verify identity is computable (read-only probe).
	_, ierr := repoIdentity(meta.RepoPath)
	if ierr != nil && ierr != errShallow {
		// path exists but is NOT a usable git checkout (stale same-path
		// non-git dir, git error). For a git project that must refuse.
		return "", true, ErrRepoUnresolved{Project: project,
			MetaReason: fmt.Sprintf("repo_path %s is not a usable git checkout: %v", meta.RepoPath, ierr)}
	}
	if ierr == errShallow {
		warnf("project %s: %s is a shallow clone — cannot stamp a stable "+
			"identity. Binding on path existence only; a copied meta.json "+
			"could silently bind a different same-path repo. Run "+
			"`git fetch --unshallow` then re-attach to harden.",
			project, meta.RepoPath)
		return meta.RepoPath, true, nil // operator authority
	}
	// usable + non-shallow → FIRST-CERTIFY when persisting (mint writes
	// .git/config, so only on the persist path; read-only ticks never write).
	if persist {
		fp, merr := mintFingerprint(meta.RepoPath) // == repoIdentity here; WRITE-allowed
		if merr != nil {
			// Mint can fail mid-write (.git/config lock, EACCES) even though
			// the read-only probe above succeeded. Do NOT persist an empty
			// fingerprint — that would falsely look "certified" while storing
			// no verifiable identity. Bind on operator authority (the pin is
			// real), skip the stamp, and surface so the next persist retries.
			warnf("project %s: could not stamp repo identity for %s (%v) — "+
				"binding on path existence only; re-attach to retry hardening.",
				project, meta.RepoPath, merr)
			return meta.RepoPath, true, nil
		}
		if perr := projects.SetRepoPath(project, meta.RepoPath, meta.RepoPath, fp); perr != nil {
			if errors.As(perr, &projects.ErrPinLostRace{}) {
				// A `project add` for a different repo won the race; re-resolve
				// so tier 1 binds the winning operator pin (never the stale path).
				return "", true, errPinRetry
			}
			return "", true, perr
		}
		warnf("project %s: established repo identity from %s (no prior "+
			"fingerprint). If this meta.json was copied from another machine, "+
			"verify the checkout — run `fleet project add --project %s <path>` "+
			"to re-pin.", project, meta.RepoPath, project)
	}
	return meta.RepoPath, true, nil // operator authority
}

// errPinRetry is an internal sentinel: tier1 first-certify lost the race;
// resolveProjectRepo must re-resolve. Kept out of tier1's public surface.
var errPinRetry = errors.New("coordrepo: pin lost race during first-certify — re-resolve")

// deriveStrong derives the base repo from the project's own fleet
// worktrees, gated by a STRONG signal. READ-ONLY — it never mints; the
// persist path computes the fingerprint to store via mintFingerprint.
//
// Gate (§4 move 2):
//   - all worktrees must derive to a base repo AND be registered in it;
//   - if >1 worktree they must all agree on ONE repo (mixed → ambiguous);
//   - stored fingerprint set → require repoIdentity(derived) == it;
//   - no stored fp (legacy) → require >=2 agreeing worktrees AND the fuzzy
//     origin/basename heuristic; a single worktree never persists.
func deriveStrong(project string, meta projects.Meta) (string, error) {
	bases, err := gc.ProjectWorktreeBases(project)
	if err != nil {
		return "", ErrWeakDerivation{Project: project, Reason: "list worktrees failed: " + err.Error()}
	}

	// Collect derivable worktrees and the distinct repos they point at.
	type wt struct{ dir, repo string }
	var derivable []wt
	repoSet := map[string]bool{}
	for _, b := range bases {
		if b.Repo == "" {
			continue // underivable dir — ignore (can't corroborate)
		}
		derivable = append(derivable, wt{dir: b.Worktree, repo: b.Repo})
		repoSet[b.Repo] = true
	}
	if len(derivable) == 0 {
		return "", ErrWeakDerivation{Project: project, Reason: "no derivable worktree"}
	}
	if len(repoSet) > 1 {
		var repos []string
		for r := range repoSet {
			repos = append(repos, r)
		}
		return "", ErrAmbiguousWorktrees{Project: project, Repos: repos}
	}

	// Single agreed repo. Every worktree must be REGISTERED in it.
	derived := derivable[0].repo
	for _, w := range derivable {
		reg, rerr := gc.WorktreeRegistered(derived, w.dir)
		if rerr != nil {
			return "", ErrWeakDerivation{Project: project,
				Reason: fmt.Sprintf("registration probe failed for %s: %v", w.dir, rerr)}
		}
		if !reg {
			return "", ErrWeakDerivation{Project: project,
				Reason: fmt.Sprintf("worktree %s not registered in derived repo %s", w.dir, derived)}
		}
	}

	// Strong-signal gate.
	if meta.RepoFingerprint != "" {
		id, ierr := repoIdentity(derived) // READ-ONLY
		if ierr == nil && id == meta.RepoFingerprint {
			return derived, nil // authoritative — works for single or multiple
		}
		return "", ErrWeakDerivation{Project: project,
			Reason: "derived repo fingerprint does not match stored fingerprint"}
	}

	// Legacy, no stored fp: >=2 worktrees agreeing (already established) AND
	// the fuzzy heuristic. Count alone is NOT enough (uniform contamination
	// agrees); the heuristic alone is NOT enough (basename collisions).
	if len(derivable) < 2 {
		return "", ErrWeakDerivation{Project: project,
			Reason: "single worktree with no stored fingerprint — refusing to self-heal"}
	}
	if !remoteMatchesProject(gitRemoteOrigin(derived), project) {
		return "", ErrWeakDerivation{Project: project,
			Reason: "no stored fingerprint and origin/basename heuristic does not match the project tag"}
	}
	return derived, nil
}

// ─── fingerprint helpers ────────────────────────────────────────────────
//
// Stored form: `<root-commit-sha>|<origin-hash-OR-fleet-repoId>`.

// repoIdentity computes the fingerprint for COMPARISON — READ-ONLY. For a
// no-origin repo it READS git config fleet.repoId but does NOT create it;
// if absent the remote component is "" (so a never-stamped local repo
// simply won't match a stored fingerprint — correct, it falls through to
// refuse/derive rather than silently matching). Returns errShallow for a
// shallow clone.
func repoIdentity(repo string) (string, error) {
	return computeFingerprint(repo, false)
}

// mintFingerprint computes the fingerprint and, for a no-origin repo
// lacking fleet.repoId, CREATES the UUID (writes .git/config) before
// returning. Called ONLY on an accepted persist. Returns errShallow for a
// shallow clone (no stamp).
func mintFingerprint(repo string) (string, error) {
	return computeFingerprint(repo, true)
}

// computeFingerprint shares the root-commit + origin logic. mint controls
// whether a missing fleet.repoId is created (write) or left empty (read).
func computeFingerprint(repo string, mint bool) (string, error) {
	if shallow, serr := isShallow(repo); serr != nil {
		return "", serr
	} else if shallow {
		return "", errShallow
	}
	root, err := rootCommit(repo)
	if err != nil {
		return "", err
	}
	remote, err := remoteIdentity(repo, mint)
	if err != nil {
		return "", err
	}
	return root + "|" + remote, nil
}

// isShallow reports `git rev-parse --is-shallow-repository == true`.
func isShallow(repo string) (bool, error) {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--is-shallow-repository").Output()
	if err != nil {
		return false, fmt.Errorf("rev-parse --is-shallow-repository in %s: %w", repo, err)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// rootCommit returns the machine-independent root commit SHA
// (`git rev-list --max-parents=0 HEAD`, first line if multiple roots).
func rootCommit(repo string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "rev-list", "--max-parents=0", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("rev-list root in %s: %w", repo, err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("rev-list root in %s: empty (no commits?)", repo)
	}
	// Multiple root commits (merged histories): the first line is stable
	// across machines (rev-list order is deterministic for a fixed history).
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s, nil
}

// remoteIdentity returns the origin-derived identity component:
//   - has origin → SHA-256 of the credential-stripped, normalized origin
//     URL (scheme+host lowercased, PATH left case-sensitive; userinfo +
//     query/fragment stripped). Hashed so a token in the remote never
//     lands raw in meta.json.
//   - no origin → the durable per-clone fleet.repoId UUID in .git/config.
//     Read-only mode leaves it "" when absent; mint mode creates it.
func remoteIdentity(repo string, mint bool) (string, error) {
	// Use the STRICT probe here (not the lenient gitRemoteOrigin the fuzzy
	// heuristic uses): a transient/corrupt-config git failure must NOT be
	// silently treated as "no origin", or we'd mint a fleet.repoId and persist
	// a no-origin identity for a repo that actually HAS an origin — a later
	// successful probe would then mismatch and wrongly refuse (codex [P2]). A
	// real error propagates; only a genuine "no origin remote" falls through to
	// the fleet.repoId path.
	origin, err := gitRemoteOriginStrict(repo)
	if err != nil {
		return "", err
	}
	if origin != "" {
		return hashOrigin(origin), nil
	}
	// No origin — use the durable per-clone fleet.repoId. Read from LOCAL
	// config only: a fleet.repoId in global/system config would otherwise be
	// returned for every no-origin repo, collapsing distinct clones onto one
	// identity (codex iter-2 [P2]).
	id := gitConfigGetLocal(repo, "fleet.repoId")
	if id != "" {
		return id, nil
	}
	if !mint {
		return "", nil // read-only: don't create
	}
	newID, err := newUUID()
	if err != nil {
		return "", err
	}
	if err := gitConfigSet(repo, "fleet.repoId", newID); err != nil {
		return "", err
	}
	// Re-read after the set: if a concurrent first-certify of the SAME repo
	// raced us, last-write-wins on .git/config means the stored value may be
	// the OTHER process's UUID. Return whatever is actually stored so the
	// persisted fingerprint always matches .git/config — never a value the
	// other process already overwrote (codex iter-2 [P2]).
	if stored := gitConfigGetLocal(repo, "fleet.repoId"); stored != "" {
		return stored, nil
	}
	return newID, nil
}

// hashOrigin normalizes + hashes an origin URL. scheme+host lowercased,
// PATH case-sensitive (git hosting paths can be case-sensitive — lowering
// would collapse distinct repos); userinfo + query + fragment stripped so
// an HTTPS token never leaks. SCP-style (git@host:org/repo.git) is
// normalized to host + path. Hashed with SHA-256.
func hashOrigin(origin string) string {
	norm := normalizeOrigin(origin)
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// normalizeOrigin returns the canonical "<host-lower>/<path-cased>" form.
func normalizeOrigin(origin string) string {
	origin = strings.TrimSpace(origin)
	// SCP-like syntax: [user@]host:path (no "://"). Detect before url.Parse,
	// which mis-handles it.
	if !strings.Contains(origin, "://") {
		if at := strings.LastIndex(origin, "@"); at >= 0 {
			origin = origin[at+1:] // strip user@
		}
		if colon := strings.Index(origin, ":"); colon >= 0 {
			host := strings.ToLower(origin[:colon])
			path := strings.TrimSuffix(origin[colon+1:], ".git")
			return host + "/" + strings.TrimPrefix(path, "/")
		}
		// Bare path / local filesystem remote — case-sensitive, as-is.
		return strings.TrimSuffix(origin, ".git")
	}
	u, err := url.Parse(origin)
	if err != nil {
		// Unparseable but has "://" — fall back to a trimmed raw form so two
		// identical malformed origins still hash equal.
		return strings.TrimSuffix(origin, ".git")
	}
	host := strings.ToLower(u.Host)
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:] // url.Host shouldn't include userinfo, but be safe
	}
	path := strings.TrimSuffix(u.Path, ".git")
	return strings.ToLower(u.Scheme) + "://" + host + path
}

// gitRemoteOrigin returns `git remote get-url origin`, trimmed; "" on any
// failure (no git, no repo, no origin remote). LENIENT — used by the fuzzy
// tier-2 heuristic where "" just fails the heuristic conservatively. Identity
// minting uses gitRemoteOriginStrict instead.
func gitRemoteOrigin(repo string) string {
	out, err := exec.Command("git", "-C", repo, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRemoteOriginStrict returns the origin URL, distinguishing a genuine
// "no origin remote" (→ "", nil) from a real git failure (→ "", err). git
// exits 2 with "No such remote 'origin'" / "No such remote: 'origin'" when the
// remote is simply absent; any OTHER non-zero exit (corrupt config, not a git
// repo, git missing) is a real error the caller must not paper over as
// no-origin. Used ONLY on the identity-mint path so a transient failure never
// freezes a false no-origin fingerprint.
func gitRemoteOriginStrict(repo string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "remote", "get-url", "origin").CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	msg := strings.ToLower(string(out))
	// git's wording has varied ("No such remote 'origin'" /
	// "No such remote: 'origin'") — match the stable prefix + the remote name.
	if strings.Contains(msg, "no such remote") && strings.Contains(msg, "origin") {
		return "", nil // genuinely no origin remote
	}
	return "", fmt.Errorf("git remote get-url origin in %s: %w (%s)", repo, err, strings.TrimSpace(string(out)))
}

// gitConfigGetLocal returns `git config --local --get <key>`, trimmed; ""
// on miss/error. --local scopes the read to the repo's own .git/config so
// a fleet.repoId in global/system config cannot be mistaken for a
// per-clone identity.
func gitConfigGetLocal(repo, key string) string {
	out, err := exec.Command("git", "-C", repo, "config", "--local", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitConfigSet writes a per-clone local repo config value
// (`git config --local <key> <val>`).
func gitConfigSet(repo, key, val string) error {
	if out, err := exec.Command("git", "-C", repo, "config", "--local", key, val).CombinedOutput(); err != nil {
		return fmt.Errorf("git config %s in %s: %w (%s)", key, repo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// newUUID returns a random RFC-4122 v4 UUID string (stdlib crypto/rand;
// no external dep per project policy).
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("coordrepo: generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// MintFingerprint exposes mintFingerprint so `fleet project add` (PR2) can
// stamp repo_fingerprint at registration using the SAME minting logic the
// resolver persists with — no second implementation to drift.
func MintFingerprint(repo string) (string, error) { return mintFingerprint(repo) }

// ─── fuzzy origin/basename heuristic (tier-2 legacy bridge ONLY) ─────────
//
// Go port of skills/coordinator/coord_config.py
// git_remote_origin / remote_matches_project. Used ONLY in deriveStrong's
// no-stored-fingerprint branch — NEVER gates tier 1, NEVER strong enough
// alone to persist a single-worktree derivation. Kept behaviorally
// identical to the Python so the two engines agree.

// remoteMatchesProject reports whether origin looks like it belongs to
// project. Strict: whole-tag equality, else first-`-` split convention.
func remoteMatchesProject(remoteURL, project string) bool {
	if remoteURL == "" || project == "" {
		return false
	}
	// Strip down to the URL basename.
	tail := remoteURL
	if i := strings.LastIndex(tail, "/"); i >= 0 {
		tail = tail[i+1:]
	}
	if i := strings.LastIndex(tail, ":"); i >= 0 {
		tail = tail[i+1:]
	}
	tail = strings.TrimSuffix(tail, ".git")
	if tail == "" {
		return false
	}
	tail = strings.ToLower(tail)
	project = strings.ToLower(project)
	// 1. Whole-tag equality (manually-named + single-segment registrations).
	if tail == project {
		return true
	}
	// 2. TagForPath split: accept when the trailing segment after the FIRST
	//    "-" matches the URL basename.
	if !strings.Contains(project, "-") {
		return false
	}
	return strings.SplitN(project, "-", 2)[1] == tail
}

// ─── tier-3 / non-git reason formatting ─────────────────────────────────

func nonGitMissingReason(repoPath string) string {
	if repoPath == "" {
		return "non-git project has no repo_path registered"
	}
	return fmt.Sprintf("non-git repo_path %s is missing on disk", repoPath)
}

func tier3MetaReason(present bool, meta projects.Meta) string {
	if !present {
		return "absent"
	}
	if meta.RepoPath == "" {
		return "present but repo_path is empty"
	}
	// repo_path set but not a usable dir (isDir failed above, so tier 1 was
	// skipped) — it's missing on disk.
	return fmt.Sprintf("repo_path %s missing on disk", meta.RepoPath)
}

func tier3WorktreeReason(derr error) string {
	if derr == nil {
		return "none"
	}
	var amb ErrAmbiguousWorktrees
	if errors.As(derr, &amb) {
		return "ambiguous (derive to different repos)"
	}
	var weak ErrWeakDerivation
	if errors.As(derr, &weak) {
		switch {
		case strings.Contains(weak.Reason, "single worktree"):
			return "single worktree without strong identity"
		case strings.Contains(weak.Reason, "not registered"):
			return "not registered in derived repo"
		case strings.Contains(weak.Reason, "no derivable"):
			return "none"
		default:
			return "present but signal too weak to trust"
		}
	}
	return "none"
}

// Package planreview reads the plan-review records written by
// skills/coordinator/review_slot.py --plan and the operator approvals
// written by `fleet tasks approve`, and evaluates whether a task's linked
// TASK-PLAN doc is reviewed, approved, and unchanged since. The
// `fleet tasks promote` gate and the TUI both go through Evaluate. See
// docs/DESIGN-plan-review-gate.md.
package planreview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/edisonshen/fleet/internal/projects"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tasks"
)

// Finding is one reviewer finding. Category is bug | flag | security.
type Finding struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	Section  string `json:"section"`
	Summary  string `json:"summary"`
	Slot     string `json:"slot"`
}

// Slot summarizes one reviewer slot's run.
type Slot struct {
	Engine     string  `json:"engine"`
	Model      string  `json:"model"`
	Exit       int     `json:"exit"`
	SkipReason *string `json:"skip_reason"`
	Error      *string `json:"error"`
}

// Review is plan-reviews/<slug>.json.
type Review struct {
	Schema     int             `json:"schema"`
	Project    string          `json:"project"`
	Slug       string          `json:"slug"`
	Doc        string          `json:"doc"`
	DocSHA256  string          `json:"doc_sha256"`
	ReviewedAt string          `json:"reviewed_at"`
	Exit       int             `json:"exit"`
	Clean      bool            `json:"clean"`
	Slots      map[string]Slot `json:"slots"`
	Findings   []Finding       `json:"findings"`
}

// Approval is plan-reviews/<slug>.approval.json.
type Approval struct {
	Doc        string    `json:"doc"`
	DocSHA256  string    `json:"doc_sha256"`
	ApprovedBy string    `json:"approved_by"`
	ApprovedAt time.Time `json:"approved_at"`
	Forced     bool      `json:"forced,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

// State is the gate verdict for one task.
type State string

const (
	StateNoDoc      State = "no-plan"
	StateUnreviewed State = "unreviewed"
	StateStale      State = "stale"
	StateFindings   State = "findings"
	StateReviewed   State = "reviewed"
	StateApproved   State = "approved"
	StateForced     State = "forced"
)

// Status is Evaluate's result. Doc/SHA are empty when no doc resolved.
type Status struct {
	State    State
	Doc      string
	SHA      string
	Review   *Review
	Approval *Approval
	Err      error
}

// ErrNoDoc means the task text links no TASK-PLAN doc.
var ErrNoDoc = errors.New("no linked task plan (add `Task plan: docs/TASK-PLAN-<slug>.md` to the spec)")

var linkRE = regexp.MustCompile("(?:^|[\\s\"'(`\\[])((?:[^\\s\"'()`\\[\\]]*/)?TASK-PLAN-[A-Za-z0-9._-]+\\.md)")

// LinkedDoc returns the first TASK-PLAN doc path named in the task's
// Spec, Acceptance, or Notes (in that order), or "".
func LinkedDoc(t *tasks.Task) string {
	for _, text := range []string{t.Spec, t.Acceptance, t.Notes} {
		if m := linkRE.FindStringSubmatch(text); m != nil {
			return m[1]
		}
	}
	return ""
}

// ResolveDoc returns the absolute path of the task's linked plan doc.
// Relative links resolve against the project's repo_path (meta.json),
// falling back to the current directory for unregistered projects.
func ResolveDoc(project string, t *tasks.Task) (string, error) {
	base := ""
	if m, err := projects.Read(project); err == nil {
		base = m.RepoPath
	}
	return resolveDoc(base, t)
}

func resolveDoc(base string, t *tasks.Task) (string, error) {
	rel := LinkedDoc(t)
	if rel == "" {
		return "", ErrNoDoc
	}
	path := rel
	if !filepath.IsAbs(path) {
		if base == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			base = cwd
		}
		path = filepath.Join(base, rel)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("linked task plan %s: %w", rel, err)
	}
	return path, nil
}

// HashFile is the sha256 hex of path's bytes (matches plan_review.doc_sha256).
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func recordPath(project, slug, suffix string) (string, error) {
	d, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	return recordPathIn(d, slug, suffix)
}

func recordPathIn(projectDir, slug, suffix string) (string, error) {
	d := filepath.Join(projectDir, "plan-reviews")
	if slug == "" || slug != filepath.Base(slug) || slug == "." || slug == ".." {
		return "", fmt.Errorf("planreview: invalid slug %q", slug)
	}
	return filepath.Join(d, slug+suffix), nil
}

func readJSON(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	return true, nil
}

// ReadReview returns the task's latest review, or nil when none exists.
func ReadReview(project, slug string) (*Review, error) {
	path, err := recordPath(project, slug, ".json")
	if err != nil {
		return nil, err
	}
	return readReview(path)
}

func readReview(path string) (*Review, error) {
	var r Review
	ok, err := readJSON(path, &r)
	if !ok || err != nil {
		return nil, err
	}
	return &r, nil
}

// ReadApproval returns the task's approval, or nil when none exists.
func ReadApproval(project, slug string) (*Approval, error) {
	path, err := recordPath(project, slug, ".approval.json")
	if err != nil {
		return nil, err
	}
	return readApproval(path)
}

func readApproval(path string) (*Approval, error) {
	var a Approval
	ok, err := readJSON(path, &a)
	if !ok || err != nil {
		return nil, err
	}
	return &a, nil
}

// WriteApproval atomically records a for the task.
func WriteApproval(project, slug string, a Approval) error {
	path, err := recordPath(project, slug, ".approval.json")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteAtomic(path, append(data, '\n'))
}

// Evaluate resolves the task's plan doc, hashes it, and compares it with
// the stored review and approval.
func Evaluate(project string, t *tasks.Task) Status {
	d, err := state.ProjectDir(project)
	if err != nil {
		return Status{State: StateNoDoc, Err: err}
	}
	base := ""
	if m, err := projects.Read(project); err == nil {
		base = m.RepoPath
	}
	return EvaluateIn(d, base, t)
}

// EvaluateIn is Evaluate for a caller that already holds the project's
// state dir and repo path (the TUI scanner walks its own projects root).
func EvaluateIn(projectDir, repoPath string, t *tasks.Task) Status {
	doc, err := resolveDoc(repoPath, t)
	if err != nil {
		return Status{State: StateNoDoc, Err: err}
	}
	sha, err := HashFile(doc)
	if err != nil {
		return Status{State: StateNoDoc, Doc: doc, Err: err}
	}
	st := Status{Doc: doc, SHA: sha}
	reviewPath, err := recordPathIn(projectDir, t.Slug, ".json")
	if err == nil {
		st.Review, err = readReview(reviewPath)
	}
	if err != nil {
		st.State, st.Err = StateUnreviewed, err
		return st
	}
	if approvalPath, err := recordPathIn(projectDir, t.Slug, ".approval.json"); err == nil {
		if st.Approval, err = readApproval(approvalPath); err != nil {
			st.Err = err
		}
	}
	switch {
	case st.Review == nil:
		st.State = StateUnreviewed
	case st.Review.DocSHA256 != sha:
		st.State = StateStale
	case !st.Review.Clean:
		st.State = StateFindings
	case st.Approval == nil || st.Approval.DocSHA256 != sha:
		st.State = StateReviewed
	case st.Approval.Forced:
		st.State = StateForced
	default:
		st.State = StateApproved
	}
	return st
}

// Blocking counts P0/P1 findings in r.
func (r *Review) Blocking() int {
	if r == nil {
		return 0
	}
	n := 0
	for _, f := range r.Findings {
		if f.Severity == "P0" || f.Severity == "P1" {
			n++
		}
	}
	return n
}

// Explain is a one-line, operator-facing reason plus next step.
func (s Status) Explain(project, slug string) string {
	review := fmt.Sprintf("review_slot.py --plan %s --project %s --slug %s", s.Doc, project, slug)
	switch s.State {
	case StateNoDoc:
		return fmt.Sprintf("no plan doc: %v", s.Err)
	case StateUnreviewed:
		if s.Err != nil {
			return fmt.Sprintf("plan review unreadable: %v", s.Err)
		}
		return "plan not reviewed — run " + review
	case StateStale:
		return "plan doc changed since its review — re-run " + review
	case StateFindings:
		return fmt.Sprintf("plan review not clean (%d P0/P1 finding(s), exit %d) — fix the doc and re-run %s",
			s.Review.Blocking(), s.Review.Exit, review)
	case StateReviewed:
		if s.Approval != nil {
			return fmt.Sprintf("approval is for an older revision of the plan — `fleet tasks approve %s --project %s`", slug, project)
		}
		return fmt.Sprintf("plan reviewed clean, awaiting operator approval — `fleet tasks approve %s --project %s`", slug, project)
	case StateForced:
		return "plan gate forced: " + s.Approval.Reason
	default:
		return "plan reviewed clean and approved"
	}
}

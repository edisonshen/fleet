package planreview

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PlanSections are the headings every TASK-PLAN doc must carry, mirroring
// Devin's plan shape: what, where, how, how-verified, what's unresolved.
var PlanSections = []string{"Goal", "Files", "Steps", "Verification", "Open questions"}

// BriefSections are the headings a DISCOVER brief must carry: the
// research the operator decides on before any plan is written.
var BriefSections = []string{"Relevant files", "Current behavior", "Options", "Open questions", "Decision"}

var (
	headingRE   = regexp.MustCompile(`^#{1,6}\s+(.+?)\s*#*\s*$`)
	briefLinkRE = regexp.MustCompile("(?:^|[\\s\"'(`\\[])((?:[^\\s\"'()`\\[\\]]*/)?BRIEF-[A-Za-z0-9._-]+\\.md)")
	emptyBodyRE = regexp.MustCompile(`(?i)^[-*\s]*(none|n/?a|no open questions|-)?[.\s]*$`)
	placeholder = regexp.MustCompile(`(?i)^[-*\s]*(tbd|todo|\?+|pending|<.*>)[.\s]*$`)
)

// Sections splits Markdown into heading -> body (trimmed). Keys are
// lower-cased; the first occurrence of a heading wins.
func Sections(doc []byte) map[string]string {
	out := map[string]string{}
	cur := ""
	var body []string
	flush := func() {
		if cur != "" {
			if _, seen := out[cur]; !seen {
				out[cur] = strings.TrimSpace(strings.Join(body, "\n"))
			}
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(doc))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	inFence := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if m := headingRE.FindStringSubmatch(line); m != nil && !inFence {
			flush()
			cur, body = strings.ToLower(m[1]), nil
			continue
		}
		body = append(body, line)
	}
	flush()
	return out
}

// section finds a required heading by prefix so "Files touched" or
// "Verification (scenario)" satisfy "Files" / "Verification".
func section(secs map[string]string, name string) (string, bool) {
	want := strings.ToLower(name)
	for k, v := range secs {
		if k == want || strings.HasPrefix(k, want+" ") || strings.HasPrefix(k, want+":") || strings.HasPrefix(k, want+" (") {
			return v, true
		}
	}
	return "", false
}

func isEmptyBody(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !emptyBodyRE.MatchString(line) {
			return false
		}
	}
	return true
}

func missing(secs map[string]string, names []string, prefix string) []string {
	var out []string
	for _, n := range names {
		if _, ok := section(secs, n); !ok {
			out = append(out, prefix+"missing section: "+n)
		}
	}
	return out
}

// LinkedBrief returns the first BRIEF-*.md path named in the plan text.
func LinkedBrief(plan []byte) string {
	if m := briefLinkRE.FindSubmatch(plan); m != nil {
		return string(m[1])
	}
	return ""
}

// resolveBrief resolves a brief link against the repo first (links are
// written repo-relative like plan links), then the plan's own dir.
func resolveBrief(link, repoPath, planPath string) string {
	if filepath.IsAbs(link) {
		return link
	}
	var cands []string
	if repoPath != "" {
		cands = append(cands, filepath.Join(repoPath, link))
	}
	cands = append(cands, filepath.Join(filepath.Dir(planPath), link), filepath.Join(filepath.Dir(planPath), filepath.Base(link)))
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return cands[0]
}

// CheckBrief lists what keeps a brief from being decided.
func CheckBrief(data []byte) []string {
	secs := Sections(data)
	probs := missing(secs, BriefSections, "brief ")
	if q, ok := section(secs, "Open questions"); ok && !isEmptyBody(q) {
		probs = append(probs, "brief has unanswered open questions")
	}
	if d, ok := section(secs, "Decision"); ok && (isEmptyBody(d) || placeholder.MatchString(d)) {
		probs = append(probs, "brief has no Decision (operator picks an option)")
	}
	return probs
}

// CheckStructure lists the structural problems that keep a plan from
// being approvable: missing sections, open questions, and a missing or
// undecided DISCOVER brief. It returns the resolved brief path ("" when
// the plan links none).
func CheckStructure(planPath, repoPath string, plan []byte) (brief string, probs []string) {
	secs := Sections(plan)
	probs = missing(secs, PlanSections, "plan ")
	if q, ok := section(secs, "Open questions"); ok && !isEmptyBody(q) {
		probs = append(probs, "plan has unresolved open questions")
	}
	link := LinkedBrief(plan)
	if link == "" {
		return "", append(probs, "plan links no DISCOVER brief (docs/BRIEF-<slug>.md)")
	}
	brief = resolveBrief(link, repoPath, planPath)
	data, err := os.ReadFile(brief)
	if err != nil {
		return brief, append(probs, "brief unreadable: "+err.Error())
	}
	return brief, append(probs, CheckBrief(data)...)
}

// BriefTemplate is the skeleton `fleet tasks brief` writes. A read-only
// research subagent fills the first three sections; the operator answers
// the questions and records the Decision.
func BriefTemplate(slug string) string {
	return "# Brief: " + slug + "\n\n" +
		"DISCOVER output for task `" + slug + "`. Research only — no plan yet.\n\n" +
		"## Relevant files\n\n- `path/to/file.go` — `FuncName`: why it matters\n\n" +
		"## Current behavior\n\nHow the code works today, citing the files above.\n\n" +
		"## Options\n\n### A. <approach>\n\n- Pros:\n- Cons:\n\n### B. <approach>\n\n- Pros:\n- Cons:\n\n" +
		"## Open questions\n\n- <question for the operator>\n\n" +
		"## Decision\n\nTBD\n"
}

// PlanTemplate is the TASK-PLAN skeleton linked to its brief.
func PlanTemplate(slug, briefRel string) string {
	return "# Task plan: " + slug + "\n\n" +
		"Brief: `" + briefRel + "`\n\n" +
		"## Goal\n\n## Files\n\n## Steps\n\n1.\n\n## Verification\n\n## Open questions\n\nNone.\n"
}

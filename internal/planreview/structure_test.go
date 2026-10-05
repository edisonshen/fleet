package planreview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const decidedBrief = "# Brief\n\n## Relevant files\n\n- `a.go`\n\n## Current behavior\n\nx\n\n" +
	"## Options\n\n### A. one\n\n### B. two\n\n## Open questions\n\nNone.\n\n## Decision\n\nOption A.\n"

const validPlan = "# Plan\n\nBrief: `docs/BRIEF-s.md`\n\n## Goal\n\ng\n\n## Files touched\n\n- a.go\n\n" +
	"## Steps\n\n1. do\n\n## Verification (scenario)\n\ntest\n\n## Open questions\n\n- none\n"

func TestSections_SkipsFencedHeadingsAndKeepsFirst(t *testing.T) {
	secs := Sections([]byte("# A\none\n```\n# Not\n```\n## A\ntwo\n## B ##\nthree\n"))
	if secs["a"] != "one\n```\n# Not\n```" {
		t.Errorf("a = %q", secs["a"])
	}
	if _, ok := secs["not"]; ok {
		t.Error("heading inside fence was parsed")
	}
	if secs["b"] != "three" {
		t.Errorf("b = %q", secs["b"])
	}
}

func TestCheckBrief(t *testing.T) {
	if p := CheckBrief([]byte(decidedBrief)); len(p) != 0 {
		t.Fatalf("decided brief: %v", p)
	}
	if p := CheckBrief([]byte(BriefTemplate("s"))); !hasAll(p, "unanswered open questions", "no Decision") {
		t.Errorf("template brief: %v", p)
	}
	if p := CheckBrief([]byte("# Brief\n## Decision\nA\n")); !hasAll(p, "missing section: Relevant files", "missing section: Options") {
		t.Errorf("sparse brief: %v", p)
	}
	for _, d := range []string{"", "TBD", "<pick one>", "???"} {
		b := strings.Replace(decidedBrief, "Option A.", d, 1)
		if p := CheckBrief([]byte(b)); !hasAll(p, "no Decision") {
			t.Errorf("decision %q accepted: %v", d, p)
		}
	}
}

func TestCheckStructure(t *testing.T) {
	repo := t.TempDir()
	plan := filepath.Join(repo, "docs", "TASK-PLAN-s.md")
	brief := filepath.Join(repo, "docs", "BRIEF-s.md")
	if err := os.MkdirAll(filepath.Dir(plan), 0o755); err != nil {
		t.Fatal(err)
	}

	b, p := CheckStructure(plan, repo, []byte(validPlan))
	if !hasAll(p, "brief unreadable") || b != brief {
		t.Fatalf("missing brief file: %q %v", b, p)
	}
	if err := os.WriteFile(brief, []byte(BriefTemplate("s")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, p := CheckStructure(plan, repo, []byte(validPlan)); !hasAll(p, "no Decision") {
		t.Fatalf("undecided brief: %v", p)
	}
	if err := os.WriteFile(brief, []byte(decidedBrief), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, p := CheckStructure(plan, repo, []byte(validPlan)); len(p) != 0 || b != brief {
		t.Fatalf("valid plan: %q %v", b, p)
	}

	open := strings.Replace(validPlan, "- none", "- which table?", 1)
	if _, p := CheckStructure(plan, repo, []byte(open)); !hasAll(p, "unresolved open questions") {
		t.Errorf("open questions: %v", p)
	}
	noLink := strings.Replace(validPlan, "Brief: `docs/BRIEF-s.md`", "", 1)
	if _, p := CheckStructure(plan, repo, []byte(noLink)); !hasAll(p, "links no DISCOVER brief") {
		t.Errorf("no brief link: %v", p)
	}
	if _, p := CheckStructure(plan, repo, []byte("# plan\nBrief: docs/BRIEF-s.md\n")); !hasAll(p,
		"missing section: Goal", "missing section: Files", "missing section: Steps",
		"missing section: Verification", "missing section: Open questions") {
		t.Errorf("bare plan: %v", p)
	}
	if _, p := CheckStructure(plan, repo, []byte(PlanTemplate("s", "docs/BRIEF-s.md"))); len(p) != 0 {
		t.Errorf("plan template with decided brief: %v", p)
	}
}

func hasAll(probs []string, wants ...string) bool {
	joined := strings.Join(probs, "|")
	for _, w := range wants {
		if !strings.Contains(joined, w) {
			return false
		}
	}
	return true
}

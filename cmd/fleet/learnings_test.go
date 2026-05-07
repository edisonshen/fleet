package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/learnings"
)

// TestLearningsAdd_Basic — happy path: tag + body + author.
func TestLearningsAdd_Basic(t *testing.T) {
	_, project := setupTasksHome(t)

	out := &bytes.Buffer{}
	if err := runLearningsAdd(&learningsAddOpts{
		project: project,
		author:  "operator",
		tags:    []string{"testing"},
	}, "use t.TempDir not os.MkdirTemp", out); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "tag=testing") {
		t.Errorf("output missing tag confirmation: %s", out.String())
	}

	entries, err := learnings.Read(project)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Tag != "testing" || entries[0].Author != "operator" {
		t.Errorf("entry fields wrong: %+v", entries[0])
	}
}

// TestLearningsAdd_RequiresTag — empty --tag is rejected before any
// disk write.
func TestLearningsAdd_RequiresTag(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runLearningsAdd(&learningsAddOpts{
		project: project,
		author:  "operator",
	}, "body", &bytes.Buffer{})
	if err == nil {
		t.Error("expected error when --tag missing")
	}
}

// TestLearningsAdd_MultipleTags — extras are folded into the body so
// the storage primitive (one tag in header) doesn't lose information.
func TestLearningsAdd_MultipleTags(t *testing.T) {
	_, project := setupTasksHome(t)
	if err := runLearningsAdd(&learningsAddOpts{
		project: project, author: "operator",
		tags: []string{"testing", "ci", "flake"},
	}, "second case", &bytes.Buffer{}); err != nil {
		t.Fatalf("add: %v", err)
	}
	entries, err := learnings.Read(project)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if entries[0].Tag != "testing" {
		t.Errorf("primary tag should be first: %q", entries[0].Tag)
	}
	if !strings.Contains(entries[0].Body, "additional tags: ci, flake") {
		t.Errorf("extras not folded into body: %q", entries[0].Body)
	}
}

// TestLearningsList_FilterByTag — list narrows by tag substring.
func TestLearningsList_FilterByTag(t *testing.T) {
	_, project := setupTasksHome(t)
	for _, tag := range []string{"testing", "review", "testing-flake"} {
		if err := runLearningsAdd(&learningsAddOpts{
			project: project, author: "operator", tags: []string{tag},
		}, "body for "+tag, &bytes.Buffer{}); err != nil {
			t.Fatalf("add %q: %v", tag, err)
		}
	}

	out := &bytes.Buffer{}
	if err := runLearningsList(&learningsListOpts{project: project, tag: "testing"}, out); err != nil {
		t.Fatalf("list: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "testing") {
		t.Errorf("expected testing rows in filter output: %s", got)
	}
	if strings.Contains(got, "review\t") {
		// review-tagged row must NOT be in the testing-substr output.
		t.Errorf("review row leaked into testing filter: %s", got)
	}
}

// TestLearningsPrune_RequiresBefore — --before is required.
func TestLearningsPrune_RequiresBefore(t *testing.T) {
	_, project := setupTasksHome(t)
	err := runLearningsPrune(&learningsPruneOpts{project: project}, &bytes.Buffer{})
	if err == nil {
		t.Error("expected error when --before missing")
	}
}

// TestLearningsPrune_DurationLoose accepts both Go-stdlib durations and
// the operator-friendly Nd form.
func TestLearningsPrune_DurationLoose(t *testing.T) {
	for _, in := range []string{"1h", "7d", "2w"} {
		if _, err := parseDurationLoose(in); err != nil {
			t.Errorf("parseDurationLoose(%q) failed: %v", in, err)
		}
	}
	if d, err := parseDurationLoose("3d"); err != nil || d != 3*24*time.Hour {
		t.Errorf("3d should parse to 72h, got %s err=%v", d, err)
	}
	if _, err := parseDurationLoose("bogus"); err == nil {
		t.Error("expected error on bogus duration")
	}
}

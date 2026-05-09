package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// withFleetHome points FLEET_HOME at a fresh tmpdir for the duration
// of the test. Returns the dir path so the test body can stat files
// directly when needed.
func withFleetHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	return dir
}

func TestReadHiddenProjects_MissingFile(t *testing.T) {
	withFleetHome(t)
	got, err := ReadHiddenProjects()
	if err != nil {
		t.Fatalf("ReadHiddenProjects: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list on missing file, got %v", got)
	}
}

func TestWriteRead_RoundTrip(t *testing.T) {
	withFleetHome(t)
	in := []string{"projects-fleet", "projects-tatoosh", "side-experiment"}
	if err := WriteHiddenProjects(in); err != nil {
		t.Fatalf("WriteHiddenProjects: %v", err)
	}
	got, err := ReadHiddenProjects()
	if err != nil {
		t.Fatalf("ReadHiddenProjects: %v", err)
	}
	want := append([]string{}, in...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestWriteHiddenProjects_AtomicPublish(t *testing.T) {
	dir := withFleetHome(t)
	if err := WriteHiddenProjects([]string{"foo", "bar"}); err != nil {
		t.Fatalf("WriteHiddenProjects: %v", err)
	}
	path := filepath.Join(dir, "hidden-projects.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	// File content must be parseable JSON — atomic publish guarantees
	// readers see only fully-written content. A torn write would parse
	// to []string with shorter length OR fail to parse at all.
	var got []string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse on-disk JSON: %v\ndata: %s", err, string(data))
	}
	want := []string{"bar", "foo"} // sorted
	if !reflect.DeepEqual(got, want) {
		t.Errorf("on-disk content mismatch: got %v, want %v", got, want)
	}
	// No leftover .tmp files. WriteAtomic cleans these on success; a
	// failed write would leave one and we'd want to know about it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" && e.Name() != "hidden-projects.json" {
			t.Errorf("unexpected leftover file in fleet home: %s", e.Name())
		}
	}
}

func TestWriteHiddenProjects_EmptySlice(t *testing.T) {
	withFleetHome(t)
	if err := WriteHiddenProjects(nil); err != nil {
		t.Fatalf("WriteHiddenProjects(nil): %v", err)
	}
	got, err := ReadHiddenProjects()
	if err != nil {
		t.Fatalf("ReadHiddenProjects: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty after writing nil, got %v", got)
	}
}

func TestWriteHiddenProjects_DedupeAndSort(t *testing.T) {
	withFleetHome(t)
	// Duplicates + unsorted input → on-disk content is deduped + sorted.
	in := []string{"zebra", "apple", "zebra", "mango", "apple"}
	if err := WriteHiddenProjects(in); err != nil {
		t.Fatalf("WriteHiddenProjects: %v", err)
	}
	got, err := ReadHiddenProjects()
	if err != nil {
		t.Fatalf("ReadHiddenProjects: %v", err)
	}
	want := []string{"apple", "mango", "zebra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected dedupe+sort:\n got %v\nwant %v", got, want)
	}
}

func TestWriteHiddenProjects_FiltersEmptyEntries(t *testing.T) {
	withFleetHome(t)
	in := []string{"foo", "", "bar", ""}
	if err := WriteHiddenProjects(in); err != nil {
		t.Fatalf("WriteHiddenProjects: %v", err)
	}
	got, err := ReadHiddenProjects()
	if err != nil {
		t.Fatalf("ReadHiddenProjects: %v", err)
	}
	want := []string{"bar", "foo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("empties should be filtered: got %v, want %v", got, want)
	}
}

func TestReadHiddenProjects_MalformedJSON(t *testing.T) {
	dir := withFleetHome(t)
	// Drop garbage bytes at the path. ReadHiddenProjects must collapse
	// to empty rather than propagate the parse error — operators
	// shouldn't lose their dashboard to a hand-edit gone wrong.
	path := filepath.Join(dir, "hidden-projects.json")
	if err := os.WriteFile(path, []byte("not json {"), 0o644); err != nil {
		t.Fatalf("seed garbage: %v", err)
	}
	got, err := ReadHiddenProjects()
	if err != nil {
		t.Fatalf("ReadHiddenProjects must not error on malformed: got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("malformed JSON should read as empty, got %v", got)
	}
}

func TestToggleHiddenProject_AddRemove(t *testing.T) {
	withFleetHome(t)
	hidden, err := ToggleHiddenProject("projects-fleet")
	if err != nil {
		t.Fatalf("first toggle: %v", err)
	}
	if !hidden {
		t.Errorf("first toggle should hide, got nowHidden=false")
	}
	// On disk?
	got, _ := ReadHiddenProjects()
	if !reflect.DeepEqual(got, []string{"projects-fleet"}) {
		t.Errorf("after first toggle: got %v, want [projects-fleet]", got)
	}
	// Second toggle un-hides.
	hidden, err = ToggleHiddenProject("projects-fleet")
	if err != nil {
		t.Fatalf("second toggle: %v", err)
	}
	if hidden {
		t.Errorf("second toggle should un-hide, got nowHidden=true")
	}
	got, _ = ReadHiddenProjects()
	if len(got) != 0 {
		t.Errorf("after second toggle: got %v, want []", got)
	}
}

func TestToggleHiddenProject_PreservesOthers(t *testing.T) {
	withFleetHome(t)
	if err := WriteHiddenProjects([]string{"alpha", "beta", "gamma"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := ToggleHiddenProject("beta"); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	got, _ := ReadHiddenProjects()
	want := []string{"alpha", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("toggle should leave others intact: got %v, want %v", got, want)
	}
	// Re-add.
	if _, err := ToggleHiddenProject("beta"); err != nil {
		t.Fatalf("re-toggle: %v", err)
	}
	got, _ = ReadHiddenProjects()
	want = []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("re-toggle should restore: got %v, want %v", got, want)
	}
}

func TestToggleHiddenProject_EmptyName(t *testing.T) {
	withFleetHome(t)
	_, err := ToggleHiddenProject("")
	if err == nil {
		t.Errorf("ToggleHiddenProject(\"\") should error, got nil")
	}
}

func TestIsHidden(t *testing.T) {
	withFleetHome(t)
	if IsHidden("foo") {
		t.Errorf("IsHidden on empty list should be false")
	}
	if err := WriteHiddenProjects([]string{"foo", "bar"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !IsHidden("foo") {
		t.Errorf("IsHidden(foo) on [foo bar] should be true")
	}
	if IsHidden("baz") {
		t.Errorf("IsHidden(baz) on [foo bar] should be false")
	}
	if IsHidden("") {
		t.Errorf("IsHidden(\"\") should always be false")
	}
}

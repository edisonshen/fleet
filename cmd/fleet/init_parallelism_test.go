package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readCoordDefaults returns the operator-wide ~/.fleet/coord-config.json
// as a map, or nil when the file does not exist.
func readCoordDefaults(t *testing.T, fleetHome string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fleetHome, "coord-config.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read coord-config.json: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parse coord-config.json: %v (raw=%q)", err, raw)
	}
	return data
}

func TestSeedParallelism_FlagWritesValue(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".fleet")
	t.Setenv("FLEET_HOME", home)

	var out bytes.Buffer
	if err := seedParallelism(&out, nil, 7); err != nil {
		t.Fatalf("seedParallelism: %v", err)
	}
	if got := readCoordDefaults(t, home)["parallelism"]; got != float64(7) {
		t.Errorf("parallelism = %v, want 7", got)
	}
}

// A non-tty stdin with no flag must write nothing and never block —
// `fleet init` runs unattended in provisioning scripts and CI.
func TestSeedParallelism_NoTTYNoFlagWritesNothing(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".fleet")
	t.Setenv("FLEET_HOME", home)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()

	var out bytes.Buffer
	if err := seedParallelism(&out, r, 0); err != nil {
		t.Fatalf("seedParallelism: %v", err)
	}
	if cfg := readCoordDefaults(t, home); cfg != nil {
		t.Errorf("wrote coord-config.json on a non-tty init: %v", cfg)
	}
}

// Re-running init must not re-answer a question the operator answered,
// and must leave unrelated keys alone.
func TestSeedParallelism_ExistingValuePreserved(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".fleet")
	t.Setenv("FLEET_HOME", home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(home, "coord-config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"parallelism": 1, "repo": "/x"}`), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	var out bytes.Buffer
	if err := seedParallelism(&out, nil, 9); err != nil {
		t.Fatalf("seedParallelism: %v", err)
	}
	cfg := readCoordDefaults(t, home)
	if cfg["parallelism"] != float64(1) || cfg["repo"] != "/x" {
		t.Errorf("existing config mutated: %v", cfg)
	}
	if !strings.Contains(out.String(), "skip (existing parallelism)") {
		t.Errorf("no skip line in output: %q", out.String())
	}
}

func TestSeedParallelism_RejectsOutOfRangeFlag(t *testing.T) {
	t.Setenv("FLEET_HOME", filepath.Join(t.TempDir(), ".fleet"))
	if err := seedParallelism(&bytes.Buffer{}, nil, parallelismMax+1); err == nil {
		t.Fatal("out-of-range --parallelism accepted")
	}
}

func TestPromptParallelism(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"5\n", 5},
		{"\n", parallelismDefault},
		{"  2  \n", 2},
		{"banana\n", parallelismDefault},
		{"0\n", parallelismDefault},
		{"999\n", parallelismDefault},
		{"", parallelismDefault}, // stdin closed mid-prompt
	}
	for _, tc := range cases {
		var out bytes.Buffer
		if got := promptParallelism(&out, strings.NewReader(tc.in)); got != tc.want {
			t.Errorf("promptParallelism(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

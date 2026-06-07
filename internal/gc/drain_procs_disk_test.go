package gc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// killDrainGuarded with RequireFingerprint=true MUST fail closed (no
// kill, no Gone) when no start fingerprint is available — even if the pid
// is alive (codex iter-6 [P2]). We point it at our own live test process
// with an empty PidStart: the guard must NOT SIGTERM us.
func TestKillDrainGuarded_RequireFingerprint_FailsClosed(t *testing.T) {
	res, err := killDrainGuarded(DrainKillTarget{
		Pid: os.Getpid(), PidStart: "", RequireFingerprint: true,
	})
	if err != nil {
		t.Fatalf("killDrainGuarded: %v", err)
	}
	if res.Killed {
		t.Fatal("RequireFingerprint with empty PidStart must NEVER kill")
	}
	if res.Gone {
		t.Fatal("a live pid with no fingerprint is AMBIGUOUS, not Gone")
	}
}

// sameFleetExe matches the current process against itself and mismatches
// against pid 1 (launchd/init — definitely not the test binary).
func TestSameFleetExe(t *testing.T) {
	if got := sameFleetExe(os.Getpid()); got != exeProbeMatch {
		t.Fatalf("sameFleetExe(self) = %v, want match", got)
	}
	// pid 1 is init/launchd — a different executable from the test binary.
	// (On the rare host where the exe is unreadable we accept Failed too,
	// since the guard treats both non-Match outcomes as "don't kill".)
	if got := sameFleetExe(1); got == exeProbeMatch {
		t.Fatalf("sameFleetExe(1) must not match the test binary; got %v", got)
	}
}

// removeDrainRunFile must NOT delete a run-record that was overwritten by
// a NEW drain reusing the PID after classification (codex iter-4 [P2]
// TOCTOU). It deletes only when the on-disk pid_start still matches what
// was classified (or there's nothing to compare).
func TestRemoveDrainRunFile_TOCTOU(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "4242.json")
	write := func(pidStart string) {
		data, _ := json.Marshal(DrainRun{Pid: 4242, PidStart: pidStart})
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Case 1: file now belongs to a DIFFERENT run (pid_start changed) →
	// must be KEPT.
	write("NEW-run-fingerprint")
	if err := removeDrainRunFile(DrainRun{Path: path, Pid: 4242, PidStart: "OLD-classified"}); err != nil {
		t.Fatalf("removeDrainRunFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("record overwritten by a new drain must NOT be deleted; got stat err %v", err)
	}

	// Case 2: file still the classified run → deleted.
	write("OLD-classified")
	if err := removeDrainRunFile(DrainRun{Path: path, Pid: 4242, PidStart: "OLD-classified"}); err != nil {
		t.Fatalf("removeDrainRunFile: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("matching record should be deleted; stat err = %v", err)
	}

	// Case 3: already gone → idempotent no-op.
	if err := removeDrainRunFile(DrainRun{Path: path, Pid: 4242, PidStart: "OLD-classified"}); err != nil {
		t.Fatalf("delete of already-gone record must be a no-op; got %v", err)
	}

	// Case 4: empty CLASSIFIED pid_start → bare delete (nothing to compare).
	write("whatever")
	if err := removeDrainRunFile(DrainRun{Path: path, Pid: 4242, PidStart: ""}); err != nil {
		t.Fatalf("removeDrainRunFile (empty classified pid_start): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty-classified-fingerprint record should be deleted; stat err = %v", err)
	}

	// Case 5 (codex iter-9 [P2]): classified pid_start non-empty, but the
	// on-disk record now has an EMPTY pid_start — a new drain that couldn't
	// fingerprint reused the PID. Ambiguous → must KEEP.
	write("") // new record with empty fingerprint
	if err := removeDrainRunFile(DrainRun{Path: path, Pid: 4242, PidStart: "OLD-classified"}); err != nil {
		t.Fatalf("removeDrainRunFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("record overwritten with an empty fingerprint must be KEPT; stat err %v", err)
	}
}

// argvIsFleetDrain must match drain invocations across the root-flag
// syntaxes fleet supports, and must NOT match other subcommands that
// merely carry a literal "drain" arg (codex iter-2 [P2]).
func TestArgvIsFleetDrain(t *testing.T) {
	cases := []struct {
		argv string
		want bool
	}{
		{"fleet drain", true},
		{"/usr/local/bin/fleet drain", true},
		{"fleet drain --grace-ms 500", true},
		{"fleet --engine codex drain", true}, // value-flag + subcommand
		{"fleet --engine=codex drain", true}, // inline value
		{"fleet --codex drain", true},        // bool root flag
		{"fleet -claude drain", true},        // bool shorthand
		{"fleet --engine codex drain --x", true},
		{"fleet", false},                       // no subcommand
		{"fleet status", false},                // different subcommand
		{"fleet tasks add drain", false},       // 'tasks' is the subcommand
		{"fleet --engine codex status", false}, // skips value, status != drain
		{"claude drain", false},                // not the fleet binary
		{"/opt/other/bin/notfleet drain", false},
		{"drain", false}, // bare, no binary
	}
	for _, c := range cases {
		if got := argvIsFleetDrain(c.argv); got != c.want {
			t.Errorf("argvIsFleetDrain(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}

// isSleepingState classifies STAT primary chars (darwin + linux).
func TestIsSleepingState(t *testing.T) {
	for _, c := range []struct {
		stat string
		want bool
	}{
		{"S", true}, {"Ss", true}, {"D", true}, {"I", true}, {"I<", true},
		{"R", false}, {"R+", false}, {"Z", false}, {"T", false}, {"", false},
	} {
		if got := isSleepingState(c.stat); got != c.want {
			t.Errorf("isSleepingState(%q) = %v, want %v", c.stat, got, c.want)
		}
	}
}

// splitLstart peels the fixed 5-token ctime string off the front.
func TestSplitLstart(t *testing.T) {
	ls, args := splitLstart("Wed May 13 17:20:39 2026 /usr/local/bin/fleet drain")
	if ls != "Wed May 13 17:20:39 2026" {
		t.Fatalf("lstart = %q", ls)
	}
	if args != "/usr/local/bin/fleet drain" {
		t.Fatalf("args = %q", args)
	}
	if ls2, a2 := splitLstart("only three tokens here"); ls2 != "" || a2 != "" {
		t.Fatalf("short input should yield empty; got %q / %q", ls2, a2)
	}
}

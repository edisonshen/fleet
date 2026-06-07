package gc

import "testing"

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

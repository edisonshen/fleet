//go:build integration

package lane

import (
	"os"
	"testing"
)

// TestOnlyTagged_Y exists only in the integration build; fails when
// LANE_FIXTURE_FAIL=1.
func TestOnlyTagged_Y(t *testing.T) {
	if os.Getenv("LANE_FIXTURE_FAIL") == "1" {
		t.Fatal("LANE_FIXTURE_FAIL=1")
	}
}

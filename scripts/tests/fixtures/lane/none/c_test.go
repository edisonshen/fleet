package none

import "testing"

// TestDefault_Z: this package has no //go:build integration tests, so the
// lane must exit 0 without running anything.
func TestDefault_Z(t *testing.T) {}

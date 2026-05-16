package dispatch

import "time"

// defaultNow is the production clock. Tests override via setNow().
func defaultNow() time.Time { return time.Now() }

// setNow replaces nowFunc for the duration of the test. The returned
// closure restores the previous value when invoked from t.Cleanup.
//
// Used by store/delivery tests to pin journal timestamps to deterministic
// values so golden-file comparisons don't churn.
func setNow(t time.Time) func() {
	prev := nowFunc
	nowFunc = func() time.Time { return t }
	return func() { nowFunc = prev }
}

package version

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempHome points FLEET_HOME at a fresh temp dir for the duration
// of the test. Returns the dir so tests can poke at the cache file.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	return dir
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.1.1", -1},
		{"v0.1.1", "v0.1.0", 1},
		{"v0.1.0", "v0.1.0", 0},
		{"0.1.0", "v0.1.0", 0},      // tolerate missing v prefix
		{"v0.1.0", "0.1.0", 0},      //
		{"v0.1.10", "v0.1.2", 1},    // numeric compare, not lex
		{"v1.0.0", "v0.99.99", 1},   // major bump
		{"v0.2.0", "v0.1.99", 1},    // minor bump
		{"v0.1.0", "v0.1.0-pre", 1}, // pre-release < release
		{"v0.1.0-pre", "v0.1.0", -1},
		{"", "v0.1.0", -1}, // empty treated as smallest
		{"v0.1.0", "", 1},
		{"garbage", "v0.1.0", -1}, // unparseable treated as smallest
	}
	for _, c := range cases {
		got := compareSemver(c.a, c.b)
		// normalize sign for the assertion
		sign := 0
		switch {
		case got > 0:
			sign = 1
		case got < 0:
			sign = -1
		}
		if sign != c.want {
			t.Errorf("compareSemver(%q,%q) = %d, want sign %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := withTempHome(t)

	want := cacheEntry{
		CheckedAt: time.Now().UTC().Truncate(time.Second),
		Latest:    "v0.1.3",
		Current:   "0.1.2",
	}
	if err := writeCache(want); err != nil {
		t.Fatalf("writeCache: %v", err)
	}

	// File lives at <dir>/version_check.json.
	if _, err := os.Stat(filepath.Join(dir, "version_check.json")); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}

	got, err := readCache()
	if err != nil {
		t.Fatalf("readCache: %v", err)
	}
	if got.Latest != want.Latest || got.Current != want.Current {
		t.Errorf("roundtrip mismatch: got %+v want %+v", got, want)
	}
	if !got.CheckedAt.Equal(want.CheckedAt) {
		t.Errorf("checked_at: got %v want %v", got.CheckedAt, want.CheckedAt)
	}
}

func TestReadCache_Missing(t *testing.T) {
	withTempHome(t)
	_, err := readCache()
	if err == nil {
		t.Fatalf("expected error on missing cache, got nil")
	}
}

func TestReadCache_Corrupt(t *testing.T) {
	dir := withTempHome(t)
	if err := os.WriteFile(filepath.Join(dir, "version_check.json"),
		[]byte("not-json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := readCache(); err == nil {
		t.Fatalf("expected error on corrupt cache, got nil")
	}
}

func TestFetchLatest_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.2.0"})
	}))
	defer srv.Close()

	tag, err := fetchLatest(srv.URL, 2*time.Second)
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if tag != "v0.2.0" {
		t.Errorf("got tag %q, want v0.2.0", tag)
	}
}

func TestFetchLatest_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := fetchLatest(srv.URL, 2*time.Second); err == nil {
		t.Fatalf("expected error on 500, got nil")
	}
}

func TestFetchLatest_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("garbage"))
	}))
	defer srv.Close()

	if _, err := fetchLatest(srv.URL, 2*time.Second); err == nil {
		t.Fatalf("expected error on bad json, got nil")
	}
}

func TestFetchLatest_ConnectionRefused(t *testing.T) {
	// Use a port unlikely to be listening. Failure mode: silent — caller
	// catches the error and the banner doesn't render.
	if _, err := fetchLatest("http://127.0.0.1:1", 200*time.Millisecond); err == nil {
		t.Fatalf("expected dial error, got nil")
	}
}

func TestNudge_FreshCacheWithUpgrade(t *testing.T) {
	withTempHome(t)
	if err := writeCache(cacheEntry{
		CheckedAt: time.Now().UTC(),
		Latest:    "v0.1.3",
		Current:   "0.1.2",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got := Nudge("0.1.2")
	if got == "" {
		t.Fatalf("expected nudge string, got empty")
	}
	if !contains(got, "v0.1.3") || !contains(got, "brew upgrade fleet") {
		t.Errorf("unexpected nudge format: %q", got)
	}
}

func TestNudge_NoUpgrade(t *testing.T) {
	withTempHome(t)
	if err := writeCache(cacheEntry{
		CheckedAt: time.Now().UTC(),
		Latest:    "v0.1.2",
		Current:   "0.1.2",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := Nudge("0.1.2"); got != "" {
		t.Errorf("expected empty nudge when up-to-date, got %q", got)
	}
}

func TestNudge_NoCacheYet(t *testing.T) {
	withTempHome(t)
	// No cache file written. Nudge should silently return empty.
	if got := Nudge("0.1.2"); got != "" {
		t.Errorf("expected empty nudge with no cache, got %q", got)
	}
}

func TestNudge_DevVersionSuppressed(t *testing.T) {
	withTempHome(t)
	if err := writeCache(cacheEntry{
		CheckedAt: time.Now().UTC(),
		Latest:    "v0.1.3",
		Current:   "dev",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// "dev" / unparseable current version → suppress the nudge.
	// Local builds shouldn't be told to "brew upgrade fleet".
	if got := Nudge("dev"); got != "" {
		t.Errorf("dev build should suppress nudge, got %q", got)
	}
}

func TestStaleCache_TriggersRefresh(t *testing.T) {
	withTempHome(t)
	old := cacheEntry{
		CheckedAt: time.Now().UTC().Add(-48 * time.Hour),
		Latest:    "v0.1.0",
		Current:   "0.1.0",
	}
	if err := writeCache(old); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !cacheStale(old) {
		t.Errorf("48h-old cache should be stale")
	}
	fresh := cacheEntry{CheckedAt: time.Now().UTC(), Latest: "v0.1.0", Current: "0.1.0"}
	if cacheStale(fresh) {
		t.Errorf("fresh cache should not be stale")
	}
}

func TestCheckAsync_PopulatesCacheFromServer(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v9.9.9"})
	}))
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		runCheck(srv.URL, "0.1.2")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("runCheck did not finish in time")
	}

	got, err := readCache()
	if err != nil {
		t.Fatalf("readCache after runCheck: %v", err)
	}
	if got.Latest != "v9.9.9" {
		t.Errorf("cache.Latest: got %q want v9.9.9", got.Latest)
	}
	if got.Current != "0.1.2" {
		t.Errorf("cache.Current: got %q want 0.1.2", got.Current)
	}
}

func TestCheckAsync_OfflineSilent(t *testing.T) {
	withTempHome(t)
	// Connection-refused endpoint → runCheck should swallow the error
	// and NOT write a cache file (so we don't poison subsequent reads).
	runCheck("http://127.0.0.1:1", "0.1.2")
	if _, err := readCache(); err == nil {
		t.Fatalf("expected no cache file after failed fetch")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

package version

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	if !strings.Contains(got, "v0.1.3") || !strings.Contains(got, "brew upgrade fleet") {
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

// TestEnsureFresh_PopulatesCacheSynchronously asserts the synchronous
// helper used by `fleet status` (a short-lived CLI command) actually
// blocks until the cache is on disk — async-via-goroutine doesn't work
// there because the binary exits before the goroutine finishes.
func TestEnsureFresh_PopulatesCacheSynchronously(t *testing.T) {
	withTempHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.5.0"})
	}))
	defer srv.Close()

	ensureFresh(srv.URL, "0.1.2", 2*time.Second)

	got, err := readCache()
	if err != nil {
		t.Fatalf("readCache after ensureFresh: %v", err)
	}
	if got.Latest != "v0.5.0" {
		t.Errorf("cache.Latest: got %q want v0.5.0", got.Latest)
	}
}

// TestEnsureFresh_OfflineDoesNotBlock asserts the helper returns
// quickly when the network is dead — short-lived CLI must not hang.
func TestEnsureFresh_OfflineDoesNotBlock(t *testing.T) {
	withTempHome(t)
	start := time.Now()
	ensureFresh("http://127.0.0.1:1", "0.1.2", 500*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Errorf("ensureFresh blocked too long on offline: %v", elapsed)
	}
	// And no cache should be written.
	if _, err := readCache(); err == nil {
		t.Fatalf("offline ensureFresh should not write cache")
	}
}

// TestEnsureFresh_SkipsWhenFresh asserts the helper is a no-op when
// the cache is already fresh — we don't punish CLI users with an HTTP
// hit on every invocation.
func TestEnsureFresh_SkipsWhenFresh(t *testing.T) {
	withTempHome(t)
	if err := writeCache(cacheEntry{
		CheckedAt: time.Now().UTC(),
		Latest:    "v0.1.0",
		Current:   "0.1.2",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Endpoint that would return v9.9.9 if hit. Should NOT be hit
	// because cache is fresh.
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v9.9.9"})
	}))
	defer srv.Close()

	ensureFresh(srv.URL, "0.1.2", 2*time.Second)
	if hit {
		t.Errorf("fresh cache should suppress the network call")
	}
	got, _ := readCache()
	if got.Latest != "v0.1.0" {
		t.Errorf("cache should not have been overwritten; got %q", got.Latest)
	}
}

// TestNudge_DefaultDevVersionSuppressed asserts that the *default*
// build version "dev" (cmd/fleet/main.go's package var) suppresses
// the nudge — go-run / unreleased binaries shouldn't see "brew
// upgrade fleet". Codex review feedback: the previous default
// "0.1.0" parsed as semver and would have nudged side-loaded builds
// the moment v0.1.1 landed.
func TestNudge_DefaultDevVersionSuppressed(t *testing.T) {
	withTempHome(t)
	if err := writeCache(cacheEntry{
		CheckedAt: time.Now().UTC(),
		Latest:    "v0.1.3",
		Current:   "dev",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Default Version is now "dev" — should suppress.
	if got := Nudge("dev"); got != "" {
		t.Errorf("expected dev build suppression, got %q", got)
	}
}

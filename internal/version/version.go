// Package version checks GitHub Releases for a newer Fleet build and
// surfaces a one-line "brew upgrade fleet" nudge.
//
// Design (issue #26):
//
//	┌─ TUI / CLI process ─┐
//	│  Start() goroutine  │ ──> async http GET releases/latest ──> writeCache
//	│  Nudge() (sync)     │ ──> readCache + compareSemver ──> "⬆ vX.Y.Z — brew upgrade fleet"
//	└─────────────────────┘
//
// Cache lives at ~/.fleet/version_check.json with a 24h TTL. Every
// failure mode (no cache, fetch error, parse error, dev build) returns
// an empty string from Nudge so the banner silently disappears.
//
// stdlib only — net/http + encoding/json. Single-binary discipline.
package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// DefaultEndpoint is the GitHub Releases API URL for fleet's latest tag.
// Overridable in tests via runCheck / fetchLatest.
const DefaultEndpoint = "https://api.github.com/repos/edisonshen/fleet/releases/latest"

// cacheTTL controls how often Start refreshes the cache. Issue #26
// requires "at most once per day per process" — the in-flight guard
// AND the on-disk timestamp both enforce this.
const cacheTTL = 24 * time.Hour

// fetchTimeout caps the HTTP call so a slow/hung GitHub doesn't wedge
// the goroutine forever (5s per spec).
const fetchTimeout = 5 * time.Second

// cacheFileName is the leaf filename under ~/.fleet/.
const cacheFileName = "version_check.json"

// banner template — kept here so the format is testable by string
// match in version_test.go and TUI tests reference it without
// re-implementing it.
//
//	⬆ v0.1.3 — brew upgrade fleet
const bannerFormat = "⬆ %s — brew upgrade fleet"

// cacheEntry is the on-disk record at ~/.fleet/version_check.json.
type cacheEntry struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
	Current   string    `json:"current"`
}

// inflight serializes background checks per-process so two callers of
// Start (e.g., TUI startup + a future background ticker) don't fan out
// duplicate HTTP requests.
var inflight sync.Mutex

// Start launches the version check in a background goroutine if the
// cache is missing or stale. Returns immediately — the caller MUST
// NOT wait. Every failure mode is silent (no log, no panic) so this
// path can never break TUI startup.
//
// current is the running build's version (cmd/fleet/main.go's Version
// var, baked in via -ldflags at release). "dev" / "0.1.0" defaults
// suppress the nudge in Nudge() but DON'T skip the fetch — we still
// want the cache populated for the next run.
func Start(current string) {
	go runCheck(DefaultEndpoint, current)
}

// runCheck is the goroutine body extracted for tests. Internal — tests
// in this package call it directly to avoid the goroutine timing dance.
func runCheck(endpoint, current string) {
	if !inflight.TryLock() {
		return // another check already in flight
	}
	defer inflight.Unlock()

	// Skip the network if we already have a fresh cache. This is the
	// "at most once per day per process" guarantee from the spec.
	if existing, err := readCache(); err == nil && !cacheStale(existing) {
		return
	}

	tag, err := fetchLatest(endpoint, fetchTimeout)
	if err != nil {
		// Silent on every failure — offline laptops, GitHub 5xx, DNS
		// blocks. Don't write the cache (a poisoned cache would suppress
		// the next refresh).
		return
	}
	_ = writeCache(cacheEntry{
		CheckedAt: time.Now().UTC(),
		Latest:    tag,
		Current:   current,
	})
}

// Nudge returns the banner string if a newer version is on disk,
// otherwise "". Pure read — never makes a network call. Safe to call
// from render hot paths.
//
// current is what the running binary reports. If current is empty or
// unparseable (e.g. "dev"), we suppress the nudge: a local build
// shouldn't be told to "brew upgrade fleet" since brew didn't install
// it.
func Nudge(current string) string {
	if !looksLikeSemver(current) {
		return ""
	}
	entry, err := readCache()
	if err != nil {
		return ""
	}
	if !looksLikeSemver(entry.Latest) {
		return ""
	}
	if compareSemver(entry.Latest, current) <= 0 {
		return ""
	}
	return fmt.Sprintf(bannerFormat, entry.Latest)
}

// cachePath returns ~/.fleet/version_check.json under the current
// FLEET_HOME (or $HOME/.fleet otherwise). Centralized so tests can
// drive it via FLEET_HOME alone.
func cachePath() (string, error) {
	root, err := state.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, cacheFileName), nil
}

// readCache returns the on-disk entry, or an error if missing or
// corrupt. Callers treat any error as "no cache yet" — never log.
func readCache() (cacheEntry, error) {
	var out cacheEntry
	path, err := cachePath()
	if err != nil {
		return out, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("decode version cache: %w", err)
	}
	return out, nil
}

// writeCache atomically publishes entry to ~/.fleet/version_check.json.
// Reuses state.WriteAtomic so a crashed process never leaves a partial
// JSON file that the next read would mistake for "valid empty cache".
//
// Bootstraps ~/.fleet/ if missing — the version check runs before any
// agent commands on a fresh install.
func writeCache(entry cacheEntry) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap state for version cache: %w", err)
	}
	path, err := cachePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("encode version cache: %w", err)
	}
	return state.WriteAtomic(path, data)
}

// cacheStale returns true when entry.CheckedAt is older than cacheTTL.
// Future timestamps (clock skew) read as fresh — better than refreshing
// every render if the operator's clock jumps.
func cacheStale(e cacheEntry) bool {
	return time.Since(e.CheckedAt) > cacheTTL
}

// fetchLatest GETs the GitHub releases endpoint and returns the
// tag_name field. Fails on non-2xx, network errors, or unparseable
// JSON — caller treats every failure as "no signal".
//
// Sets a User-Agent because GitHub rejects unidentified clients with
// 403, and an Accept header for the v3 JSON API.
func fetchLatest(endpoint string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build version request: %w", err)
	}
	req.Header.Set("User-Agent", "fleet-version-check")
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github releases status %d", resp.StatusCode)
	}

	// Decode only the field we care about — schema is permissive so a
	// future GitHub API addition doesn't break us.
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode releases payload: %w", err)
	}
	if payload.TagName == "" {
		return "", fmt.Errorf("releases payload missing tag_name")
	}
	return payload.TagName, nil
}

// looksLikeSemver returns true when s parses as MAJOR.MINOR.PATCH
// (with optional leading "v" and optional "-prerelease" suffix). Used
// to gate Nudge() so dev / unset versions don't trigger a "brew
// upgrade" suggestion.
func looksLikeSemver(s string) bool {
	major, minor, patch, _, ok := parseSemver(s)
	if !ok {
		return false
	}
	return major >= 0 && minor >= 0 && patch >= 0
}

// compareSemver returns -1 / 0 / +1 when a is less than / equal to /
// greater than b. Tolerates a leading "v" on either side. Pre-release
// suffixes ("-rc1", "-pre") sort BEFORE the corresponding release
// (semver §11). Unparseable input sorts as smallest.
//
// 20-line comparator, stdlib only — issue #26 explicitly allowed this
// in lieu of pulling in golang.org/x/mod just for one helper.
func compareSemver(a, b string) int {
	aMaj, aMin, aPat, aPre, aOK := parseSemver(a)
	bMaj, bMin, bPat, bPre, bOK := parseSemver(b)
	switch {
	case !aOK && !bOK:
		return strings.Compare(a, b)
	case !aOK:
		return -1
	case !bOK:
		return 1
	}
	if aMaj != bMaj {
		return cmpInt(aMaj, bMaj)
	}
	if aMin != bMin {
		return cmpInt(aMin, bMin)
	}
	if aPat != bPat {
		return cmpInt(aPat, bPat)
	}
	// Pre-release tie-break: "v0.1.0" > "v0.1.0-rc1".
	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		return 1
	case bPre == "":
		return -1
	}
	return strings.Compare(aPre, bPre)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// parseSemver pulls major/minor/patch + optional pre-release out of s.
// Tolerates a leading "v" since GitHub tags use "v0.1.0" and the
// ldflags-injected Version is bare "0.1.0". Returns ok=false on any
// parse error so callers can decide the fallback policy.
func parseSemver(s string) (major, minor, patch int, pre string, ok bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return 0, 0, 0, "", false
	}
	// Split off pre-release / build metadata.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, "", false
	}
	var err error
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, 0, "", false
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, 0, "", false
	}
	if patch, err = strconv.Atoi(parts[2]); err != nil {
		return 0, 0, 0, "", false
	}
	return major, minor, patch, pre, true
}

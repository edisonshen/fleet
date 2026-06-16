//go:build integration

package main

import "os"

// assertNoStandbyLaunches is a no-op in the integration lane: standby launches
// ARE expected there. Mirrors the !integration twin's signature so TestMain
// compiles in both builds.
func assertNoStandbyLaunches(code int) { os.Exit(code) }

package fleetlog

// CLIStart logs a cli.start event for an explicitly-traced command and
// returns a finish closure that logs the matching cli.finish (rc +
// duration) when called. Usage in a cobra RunE:
//
//	finish := fleetlog.CLIStart("drain")
//	err := runDrain(...)
//	finish(err)
//	return err
//
// This is an explicit per-command call (argv[0] is the command name) — NOT
// a cobra-hook / main()-wrapper auto-instrumentation, which the design
// deliberately rejects. For `fleet attach` the success path execve-replaces
// the process before finish runs, so only cli.start is emitted on success
// and cli.finish fires only on the error-return path — by design.
func CLIStart(argv ...string) func(err error) {
	start := nowFn()
	name := ""
	if len(argv) > 0 {
		name = argv[0]
	}
	id := Log(CompCLI, "cli.start", "info", Fields{
		Msg:  "cli " + name + " start",
		Data: map[string]any{"argv": argv},
	})
	return func(err error) {
		rc := 0
		if err != nil {
			rc = 1
		}
		Log(CompCLI, "cli.finish", "info", Fields{
			CausedBy: id,
			Msg:      "cli " + name + " finish",
			Data: map[string]any{
				"argv":   argv,
				"rc":     rc,
				"dur_ms": nowFn().Sub(start).Milliseconds(),
			},
		})
	}
}

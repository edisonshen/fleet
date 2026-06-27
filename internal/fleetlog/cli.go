package fleetlog

// CLIStart logs a cli.start event for an explicitly-traced command and
// returns a finish closure that logs the matching cli.finish (rc +
// duration) when called. Usage in a cobra RunE:
//
//	finish := fleetlog.CLIStart(fleetlog.Fields{Proj: opts.project}, "dispatch", opts.taskID)
//	err := runDispatch(...)
//	finish(err)
//	return err
//
// The Fields parameter sets optional correlation keys (Proj, Agent, Slug …)
// on both cli.start and cli.finish. Pass a zero value (fleetlog.Fields{})
// for commands that do not carry a project context.
//
// For commands with typed exit codes (e.g. `fleet attach` exits 64 for usage
// errors, 70 for system errors), pass the real code as the optional second
// argument to the returned closure:
//
//	finish(err, ExitCodeFor(err))
//
// The optional rc overrides the default err→1 mapping so cli.finish records
// the same code that main() passes to os.Exit.
//
// This is an explicit per-command call (argv[0] is the command name) — NOT
// a cobra-hook / main()-wrapper auto-instrumentation, which the design
// deliberately rejects. For `fleet attach` the success path execve-replaces
// the process before finish runs, so only cli.start is emitted on success
// and cli.finish fires only on the error-return path — by design.
func CLIStart(f Fields, argv ...string) func(err error, rc ...int) {
	start := nowFn()
	name := ""
	if len(argv) > 0 {
		name = argv[0]
	}
	f.Msg = "cli " + name + " start"
	if f.Data == nil {
		f.Data = map[string]any{"argv": argv}
	} else {
		f.Data["argv"] = argv
	}
	id := Log(CompCLI, "cli.start", "info", f)
	return func(err error, rc ...int) {
		code := 0
		if len(rc) > 0 {
			// Caller supplied the real exit code (e.g. typed error → ExitCodeFor).
			code = rc[0]
		} else if err != nil {
			code = 1
		}
		fin := Fields{
			Proj:     f.Proj,
			Agent:    f.Agent,
			Slug:     f.Slug,
			CausedBy: id,
			Msg:      "cli " + name + " finish",
			Data: map[string]any{
				"argv":   argv,
				"rc":     code,
				"dur_ms": nowFn().Sub(start).Milliseconds(),
			},
		}
		Log(CompCLI, "cli.finish", "info", fin)
	}
}

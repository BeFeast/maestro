package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/befeast/maestro/internal/selfcheck"
)

// selfcheckCmd runs the bundled behavioral smoke gate (#842) and exits 0 when
// every invariant holds, non-zero otherwise. It is what scripts/self-deploy.sh
// runs against the freshly-installed binary after the version health check and
// before finalizing the deploy: a binary that boots and reports the right
// version but has regressed config parsing, backend resolution, prompt
// assembly, or state persistence fails here and is rolled back to `.prev`.
//
// The gate is deterministic and side-effect-free — it runs against an embedded
// held-out fixture and a throwaway temp dir, makes no GitHub/config-store/
// network calls, and touches no live fleet state — so it is safe to run
// anywhere, including by hand for diagnosis.
func selfcheckCmd(args []string) {
	os.Exit(runSelfCheck(args, os.Stdout))
}

// runSelfCheck is the testable core of selfcheckCmd: it parses flags, runs the
// gate, writes a report to out, and returns the process exit code (0 = all
// checks passed, 1 = one or more failed, 2 = usage error).
func runSelfCheck(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("selfcheck", flag.ContinueOnError)
	fs.SetOutput(out)
	jsonOut := fs.Bool("json", false, "Emit the report as a single JSON object")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// The invariants exercise packages (e.g. the router) that log through the
	// standard logger. Silence it for the duration so the gate's own report is
	// the only output — clean for a human diagnosing a rollback and stable for
	// the deploy script that parses stdout.
	prevOut, prevFlags, prevPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(io.Discard)
	rep := selfcheck.Run()
	log.SetOutput(prevOut)
	log.SetFlags(prevFlags)
	log.SetPrefix(prevPrefix)

	if *jsonOut {
		fmt.Fprintln(out, rep.JSON())
		if rep.OK {
			return 0
		}
		return 1
	}

	for _, c := range rep.Checks {
		status := "PASS"
		if !c.OK {
			status = "FAIL"
		}
		if c.Detail != "" {
			fmt.Fprintf(out, "[selfcheck] %s %s: %s\n", status, c.Name, c.Detail)
		} else {
			fmt.Fprintf(out, "[selfcheck] %s %s\n", status, c.Name)
		}
	}
	if rep.OK {
		fmt.Fprintf(out, "[selfcheck] OK (%d checks passed)\n", len(rep.Checks))
		return 0
	}
	// Final line names the failed checks so self-deploy can attribute a
	// gate-triggered rollback to a specific invariant. Keep this line's format
	// stable — scripts/self-deploy.sh greps for the "FAILED:" prefix.
	fmt.Fprintf(out, "[selfcheck] FAILED: %s\n", strings.Join(rep.FailedNames(), ","))
	return 1
}

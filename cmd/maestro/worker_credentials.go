package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/befeast/maestro/internal/worker"
)

// workerExecCmd implements the private internal boundary used by generated
// worker runners. Credential values are read from the validated service file
// immediately before exec and never accepted as flags or other argv content.
func workerExecCmd(args []string) {
	fs := flag.NewFlagSet("_worker-exec", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	credentialsFile := fs.String("credentials-file", "", "private worker credential file reference")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := worker.RunWorkerWithCredentials(*credentialsFile, fs.Args(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "[maestro] worker exec: %v\n", err)
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}

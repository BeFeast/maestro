package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/befeast/maestro/internal/candidatepreflight"
)

func candidatePreflightCmd(args []string) {
	os.Exit(runCandidatePreflight(args, os.Stdin, os.Stdout, os.Stderr))
}

func runCandidatePreflight(args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("candidate-preflight", flag.ContinueOnError)
	fs.SetOutput(errOut)
	input := fs.String("input", "-", "Supplied JSON snapshots file, or - for stdin; never loads live runtime state")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	if *input != "-" {
		f, err := os.Open(*input)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 2
		}
		defer f.Close()
		in = f
	}
	data, err := io.ReadAll(io.LimitReader(in, 4*1024*1024+1))
	if err != nil || len(data) > 4*1024*1024 {
		fmt.Fprintln(errOut, "input unreadable or exceeds 4 MiB")
		return 2
	}
	report, err := candidatepreflight.Evaluate(data)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	if report.Status != "no_declared_overlap" {
		return 1
	}
	return 0
}

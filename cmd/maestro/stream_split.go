package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/befeast/maestro/internal/worker"
)

// streamSplitCmd implements `maestro stream-split`, the internal filter the
// worker runner pipes a claude stream-json stream through (#737). It reads
// NDJSON on stdin, appends every raw frame to --jsonl (the side-channel the
// usage parser consumes), and renders human-readable text to stdout (tee'd
// into slot.log). It never fails the pipeline on a per-line decode problem —
// unrecognized lines pass through verbatim so the worker log is preserved.
func streamSplitCmd(args []string) {
	fs := flag.NewFlagSet("stream-split", flag.ExitOnError)
	backend := fs.String("backend", "claude", "backend kind for rendering (claude)")
	jsonl := fs.String("jsonl", "", "path to append raw NDJSON frames to (side channel)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := worker.RunStreamSplit(*backend, *jsonl, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "[maestro] stream-split: %v\n", err)
		os.Exit(1)
	}
}

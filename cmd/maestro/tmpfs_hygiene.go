package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/configstore"
	"github.com/befeast/maestro/internal/tmpfshygiene"
)

type tmpfsHygieneDependencies struct {
	defaultStore       string
	options            tmpfshygiene.Options
	loadProtectedPaths func(context.Context, string) ([]string, error)
}

func tmpfsHygieneCmd(args []string) {
	deps := tmpfsHygieneDependencies{
		defaultStore: defaultConfigStorePath(),
		options: tmpfshygiene.Options{
			Root:     "/tmp",
			ProcRoot: "/proc",
		},
		loadProtectedPaths: configuredTmpfsProtectedPaths,
	}
	if err := runTmpfsHygiene(context.Background(), args, os.Stdout, deps); err != nil {
		log.Fatalf("tmpfs-hygiene: %v", err)
	}
}

func runTmpfsHygiene(ctx context.Context, args []string, out io.Writer, deps tmpfsHygieneDependencies) error {
	fs := flag.NewFlagSet("tmpfs-hygiene", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "inspect without deleting")
	apply := fs.Bool("apply", false, "apply the allowlisted sweep")
	storePath := fs.String("store", deps.defaultStore, "config store used to protect local/worktree paths")
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *dryRun == *apply {
		return errorsExactlyOneTmpfsMode()
	}
	if deps.loadProtectedPaths == nil {
		deps.loadProtectedPaths = configuredTmpfsProtectedPaths
	}
	mode := tmpfshygiene.ModeDryRun
	if *apply {
		mode = tmpfshygiene.ModeApply
	}
	paths, err := deps.loadProtectedPaths(ctx, *storePath)
	if err != nil {
		now := time.Now
		if deps.options.Now != nil {
			now = deps.options.Now
		}
		root := strings.TrimSpace(deps.options.Root)
		if root == "" {
			root = "/tmp"
		}
		summary := tmpfshygiene.Summary{
			Timestamp:   now().UTC(),
			Mode:        mode,
			Root:        root,
			Categories:  map[string]tmpfshygiene.CategoryStats{},
			ProtectHits: map[string]int{},
			Error:       "load configured protection paths: " + err.Error(),
		}
		if encodeErr := writeTmpfsHygieneSummary(out, summary); encodeErr != nil {
			return encodeErr
		}
		return fmt.Errorf("load configured protection paths: %w", err)
	}
	opts := deps.options
	opts.Mode = mode
	opts.ProtectedPaths = append(append([]string(nil), opts.ProtectedPaths...), paths...)
	summary, sweepErr := tmpfshygiene.Sweep(ctx, opts)
	if err := writeTmpfsHygieneSummary(out, summary); err != nil {
		return err
	}
	return sweepErr
}

func writeTmpfsHygieneSummary(out io.Writer, summary tmpfshygiene.Summary) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(summary); err != nil {
		return fmt.Errorf("write JSONL summary: %w", err)
	}
	return nil
}

func errorsExactlyOneTmpfsMode() error {
	return fmt.Errorf("exactly one of --dry-run or --apply is required")
}

func configuredTmpfsProtectedPaths(ctx context.Context, storePath string) ([]string, error) {
	store, err := configstore.OpenReadOnly(storePath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	cfgs, err := store.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(cfgs)*2)
	seen := make(map[string]bool, len(cfgs)*2)
	for _, cfg := range cfgs {
		if cfg == nil {
			continue
		}
		for _, path := range []string{cfg.LocalPath, cfg.WorktreeBase} {
			path = strings.TrimSpace(path)
			if path != "" && !seen[path] {
				paths = append(paths, path)
				seen[path] = true
			}
		}
	}
	return paths, nil
}

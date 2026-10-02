package main

import (
	"flag"
	"io"
	"time"

	"github.com/befeast/maestro/internal/controllerowner"
)

// superviseFlags is shared by dispatch classification and command parsing so a
// string-valued flag containing "approve" cannot bypass scheduler ownership.
type superviseFlags struct {
	configPath, storePath, storeProject, approvalsDB *string
	once, jsonOutput, dryRun                         *bool
	interval                                         *time.Duration
}

func addSuperviseFlags(fs *flag.FlagSet, approvalsDefault string) superviseFlags {
	store, project := configStoreFlags(fs)
	return superviseFlags{
		configPath: fs.String("config", "", "Path to config file"),
		storePath:  store, storeProject: project,
		once:        fs.Bool("once", false, "Run once and exit"),
		interval:    fs.Duration("interval", 5*time.Minute, "Loop interval"),
		jsonOutput:  fs.Bool("json", false, "Output decision as JSON"),
		dryRun:      fs.Bool("dry-run", false, "Compute decision without recording state"),
		approvalsDB: fs.String("approvals-db", approvalsDefault, "SQLite approvals db path used for durable delivery claims"),
	}
}

func isSuperviseAdminAction(action string) bool {
	switch action {
	case "approve", "reject", "reconcile-delivery":
		return true
	default:
		return false
	}
}

func requiresControllerOwner(command string, args []string) bool {
	// An exact help request cannot execute the command; other spellings go
	// through normal ownership and parser handling without speculative bypass.
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return false
	}
	switch command {
	case "daemon", "run", "night-start", "spawn":
		return true
	case "supervise":
		if len(args) > 0 && isSuperviseAdminAction(args[0]) {
			return false
		}
		fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		addSuperviseFlags(fs, "") // no config/store/default discovery
		if err := fs.Parse(args); err != nil {
			return true // fail closed; actual parser still reports usage if admitted
		}
		return fs.NArg() == 0 || !isSuperviseAdminAction(fs.Arg(0))
	default:
		return false
	}
}

// claimControllerForCommand is called only by the executable dispatcher. Nested
// runCmd and the daemon's in-process supervisors share that single lifetime.
func claimControllerForCommand(command string, args []string, acquire func() (io.Closer, error)) (io.Closer, error) {
	if !requiresControllerOwner(command, args) {
		return nil, nil
	}
	return acquire()
}

func acquireControllerOwner() (io.Closer, error) {
	return controllerowner.Acquire()
}

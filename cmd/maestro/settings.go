package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configstore"
)

// settingsCmd implements `maestro settings <list|get|set|rm|audit>` — the
// operator surface for the fleet-level cost/LLM control knobs (#839). A fleet
// default (no --project) applies to every project that does not set the key in
// its own config; a per-project override (--project <name>) wins over the fleet
// default. Both write with the same hot-reload semantics as config-store edits
// and are journaled to settings_audit.
func settingsCmd(args []string) {
	if len(args) == 0 {
		settingsUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "list", "ls":
		settingsList(args[1:])
	case "get":
		settingsGet(args[1:])
	case "set":
		settingsSet(args[1:])
	case "rm", "unset", "delete":
		settingsRm(args[1:])
	case "audit", "log":
		settingsAudit(args[1:])
	case "help", "-h", "--help":
		settingsUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown settings command: %s\n\n", args[0])
		settingsUsage()
		os.Exit(1)
	}
}

func settingsUsage() {
	fmt.Fprintln(os.Stderr, "usage: maestro settings <list|get|set|rm|audit> ...")
	fmt.Fprintln(os.Stderr, "  list  [--project <name>]                 show fleet defaults (and per-project effective values with --project)")
	fmt.Fprintln(os.Stderr, "  get   <key> [--project <name>]           show a key's fleet default, or its effective value+source with --project")
	fmt.Fprintln(os.Stderr, "  set   <key>=<value> [--project <name>]   set a fleet default, or a per-project override with --project")
	fmt.Fprintln(os.Stderr, "  rm    <key> [--project <name>]           clear a fleet default (revert to builtin), or a per-project override")
	fmt.Fprintln(os.Stderr, "  audit [--limit N]                        show the settings change journal (who/when/old->new)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "known keys: "+strings.Join(config.FleetSettingKeys(), ", "))
	fmt.Fprintln(os.Stderr, "flags: --db <path> (default ~/.maestro/config.db), --actor <name>")
}

// splitSettingsArgs separates flag tokens from positional arguments so a key /
// key=value can appear before or after flags — Go's flag package otherwise
// stops at the first positional, breaking the natural
// `settings set key=value --project x`. Every settings flag takes a value, so a
// dash token without "=" also consumes the following token as its value.
func splitSettingsArgs(args []string) (flags, positionals []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return flags, positionals
}

// settingsActor resolves the audit identity for a CLI change: --actor if given,
// else "cli:<user>".
func settingsActor(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	user := strings.TrimSpace(os.Getenv("USER"))
	if user == "" {
		user = "unknown"
	}
	return "cli:" + user
}

func openSettingsStore(dbPath string) *configstore.Store {
	store, err := configstore.Open(dbPath)
	if err != nil {
		log.Fatalf("settings: open db: %v", err)
	}
	return store
}

func settingsList(args []string) {
	fs := flag.NewFlagSet("settings list", flag.ExitOnError)
	dbPath := configStoreDBFlag(fs)
	project := fs.String("project", "", "Project name (show per-project effective values + source)")
	fs.Parse(args)

	ctx := context.Background()
	store := openSettingsStore(*dbPath)
	defer store.Close()

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if strings.TrimSpace(*project) == "" {
		fleet, err := store.FleetSettings(ctx)
		if err != nil {
			log.Fatalf("settings list: %v", err)
		}
		fmt.Fprintln(tw, "KEY\tFLEET DEFAULT")
		for _, key := range config.FleetSettingKeys() {
			v, ok := fleet[key]
			if !ok {
				v = "— (built-in)"
			}
			fmt.Fprintf(tw, "%s\t%s\n", key, v)
		}
		tw.Flush()
		return
	}

	cfg, err := store.Load(ctx, *project)
	if err != nil {
		log.Fatalf("settings list: %v", err)
	}
	fmt.Fprintln(tw, "KEY\tVALUE\tSOURCE")
	for _, spec := range config.FleetSettingSpecs() {
		src := cfg.SettingsSources[spec.Key]
		if src == "" {
			src = config.SettingSourceBuiltin
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", spec.Key, spec.Value(cfg), src)
	}
	tw.Flush()
}

func settingsGet(args []string) {
	fs := flag.NewFlagSet("settings get", flag.ExitOnError)
	dbPath := configStoreDBFlag(fs)
	project := fs.String("project", "", "Project name (show effective value + source)")
	flags, rest := splitSettingsArgs(args)
	fs.Parse(flags)

	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		log.Fatalf("settings get: exactly one key is required")
	}
	key := strings.TrimSpace(rest[0])
	if _, ok := config.FleetSettingSpecByKey(key); !ok {
		log.Fatalf("settings get: unknown key %q (known: %s)", key, strings.Join(config.FleetSettingKeys(), ", "))
	}

	ctx := context.Background()
	store := openSettingsStore(*dbPath)
	defer store.Close()

	if strings.TrimSpace(*project) == "" {
		fleet, err := store.FleetSettings(ctx)
		if err != nil {
			log.Fatalf("settings get: %v", err)
		}
		if v, ok := fleet[key]; ok {
			fmt.Printf("%s = %s (fleet default)\n", key, v)
		} else {
			fmt.Printf("%s: unset — the built-in default applies\n", key)
		}
		return
	}

	cfg, err := store.Load(ctx, *project)
	if err != nil {
		log.Fatalf("settings get: %v", err)
	}
	spec, _ := config.FleetSettingSpecByKey(key)
	src := cfg.SettingsSources[key]
	if src == "" {
		src = config.SettingSourceBuiltin
	}
	fmt.Printf("%s = %s (source: %s)\n", key, spec.Value(cfg), src)
}

func settingsSet(args []string) {
	fs := flag.NewFlagSet("settings set", flag.ExitOnError)
	dbPath := configStoreDBFlag(fs)
	project := fs.String("project", "", "Project name (set a per-project override instead of the fleet default)")
	actor := fs.String("actor", "", "Actor recorded in the settings audit log (default: cli:<user>)")
	flags, rest := splitSettingsArgs(args)
	fs.Parse(flags)

	if len(rest) != 1 || !strings.Contains(rest[0], "=") {
		log.Fatalf("settings set: exactly one key=value argument is required")
	}
	key, value, _ := strings.Cut(rest[0], "=")
	key = strings.TrimSpace(key)
	if _, ok := config.FleetSettingSpecByKey(key); !ok {
		log.Fatalf("settings set: unknown key %q (known: %s)", key, strings.Join(config.FleetSettingKeys(), ", "))
	}

	ctx := context.Background()
	store := openSettingsStore(*dbPath)
	defer store.Close()
	who := settingsActor(*actor)

	if p := strings.TrimSpace(*project); p != "" {
		if err := store.SetProjectSetting(ctx, p, key, value, who); err != nil {
			log.Fatalf("settings set: %v", err)
		}
		fmt.Printf("Set %s=%s for project %q (per-project override).\n", key, strings.TrimSpace(value), p)
		return
	}
	if err := store.SetFleetSetting(ctx, key, value, who); err != nil {
		log.Fatalf("settings set: %v", err)
	}
	fmt.Printf("Set fleet default %s=%s (applies to projects without an override).\n", key, strings.TrimSpace(value))
}

func settingsRm(args []string) {
	fs := flag.NewFlagSet("settings rm", flag.ExitOnError)
	dbPath := configStoreDBFlag(fs)
	project := fs.String("project", "", "Project name (clear a per-project override instead of the fleet default)")
	actor := fs.String("actor", "", "Actor recorded in the settings audit log (default: cli:<user>)")
	flags, rest := splitSettingsArgs(args)
	fs.Parse(flags)

	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		log.Fatalf("settings rm: exactly one key is required")
	}
	key := strings.TrimSpace(rest[0])
	if _, ok := config.FleetSettingSpecByKey(key); !ok {
		log.Fatalf("settings rm: unknown key %q (known: %s)", key, strings.Join(config.FleetSettingKeys(), ", "))
	}

	ctx := context.Background()
	store := openSettingsStore(*dbPath)
	defer store.Close()
	who := settingsActor(*actor)

	if p := strings.TrimSpace(*project); p != "" {
		if err := store.DeleteProjectSetting(ctx, p, key, who); err != nil {
			log.Fatalf("settings rm: %v", err)
		}
		fmt.Printf("Cleared %s override for project %q.\n", key, p)
		return
	}
	if err := store.DeleteFleetSetting(ctx, key, who); err != nil {
		log.Fatalf("settings rm: %v", err)
	}
	fmt.Printf("Cleared fleet default %s (projects without an override revert to the built-in value).\n", key)
}

func settingsAudit(args []string) {
	fs := flag.NewFlagSet("settings audit", flag.ExitOnError)
	dbPath := configStoreDBFlag(fs)
	limit := fs.Int("limit", 50, "Max audit rows to show (0 = all)")
	fs.Parse(args)

	ctx := context.Background()
	store := openSettingsStore(*dbPath)
	defer store.Close()

	rows, err := store.SettingsAudit(ctx, *limit)
	if err != nil {
		log.Fatalf("settings audit: %v", err)
	}
	if len(rows) == 0 {
		fmt.Println("No settings changes recorded.")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CHANGED AT\tSCOPE\tKEY\tOLD\tNEW\tACTOR")
	for _, r := range rows {
		old := r.OldValue
		if old == "" {
			old = "—"
		}
		newV := r.NewValue
		if newV == "" {
			newV = "— (cleared)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ChangedAt, r.Scope, r.Key, old, newV, r.Actor)
	}
	tw.Flush()
}

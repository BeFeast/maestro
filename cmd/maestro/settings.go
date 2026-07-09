package main

// `maestro settings` — the operator surface for fleet-level cost-control knobs
// (#839). Before this, flipping e.g. `supervisor.enabled=false` fleet-wide meant
// exporting, editing and re-importing every project row (the 2026-07-09 P0
// mitigation did exactly that under incident pressure). `settings set` writes
// the config store directly — fleet-wide by default, or per-project with
// --project — so a `maestro daemon --watch-store` picks the change up on its
// next poll without a restart, and every change is journaled (who/when/old→new).

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/befeast/maestro/internal/configstore"
)

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
	case "audit", "log":
		settingsAudit(args[1:])
	case "-h", "--help", "help":
		settingsUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown settings command: %s\n\n", args[0])
		settingsUsage()
		os.Exit(1)
	}
}

func settingsUsage() {
	fmt.Fprintln(os.Stderr, "usage: maestro settings <list|get|set|audit> [flags]")
	fmt.Fprintln(os.Stderr, "  list [--project <row>]           show effective values (per project) or fleet defaults")
	fmt.Fprintln(os.Stderr, "  list --keys                      list supported keys and their meaning")
	fmt.Fprintln(os.Stderr, "  get <key> [--project <row>]      print one key's value (resolved when --project)")
	fmt.Fprintln(os.Stderr, "  set <key>=<value> [--project <row>] [--actor <who>]   set a fleet default or per-project override")
	fmt.Fprintln(os.Stderr, "  set <key>= [--unset]             clear a fleet default (revert to built-in)")
	fmt.Fprintln(os.Stderr, "  audit [--limit N]                show the settings change journal")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Precedence: per-project override > fleet default > built-in.")
}

// defaultSettingsActor derives the audit actor string when --actor is omitted:
// the invoking user, falling back to "cli".
func defaultSettingsActor() string {
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		return u + " (cli)"
	}
	return "cli"
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
	project := fs.String("project", "", "Project row to resolve (omit for fleet defaults)")
	keys := fs.Bool("keys", false, "List supported keys and their meaning, then exit")
	fs.Parse(args)

	if *keys {
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "KEY\tTYPE\tDESCRIPTION")
		for _, spec := range configstore.SettingSpecs() {
			fmt.Fprintf(w, "%s\t%s\t%s\n", spec.Name, spec.TypeName(), spec.Doc)
		}
		w.Flush()
		return
	}

	store := openSettingsStore(*dbPath)
	defer store.Close()
	ctx := context.Background()

	if strings.TrimSpace(*project) != "" {
		resolved, err := store.ResolveProjectSettings(ctx, *project)
		if err != nil {
			log.Fatalf("settings list: %v", err)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "KEY\tVALUE\tSOURCE\n")
		for _, r := range resolved {
			fmt.Fprintf(w, "%s\t%s\t%s\n", r.Key, r.Value, r.Source)
		}
		w.Flush()
		return
	}

	// No project: show fleet-level defaults (unset keys render as "(unset)").
	fleet, err := store.FleetSettings(ctx)
	if err != nil {
		log.Fatalf("settings list: %v", err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "KEY\tFLEET DEFAULT\n")
	for _, spec := range configstore.SettingSpecs() {
		val, ok := fleet[spec.Name]
		if !ok {
			val = "(unset)"
		}
		fmt.Fprintf(w, "%s\t%s\n", spec.Name, val)
	}
	w.Flush()
}

func settingsGet(args []string) {
	fs := flag.NewFlagSet("settings get", flag.ExitOnError)
	dbPath := configStoreDBFlag(fs)
	project := fs.String("project", "", "Project row to resolve (omit for the fleet default)")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		log.Fatalf("settings get: exactly one key is required")
	}
	key := strings.TrimSpace(rest[0])
	store := openSettingsStore(*dbPath)
	defer store.Close()
	ctx := context.Background()

	if strings.TrimSpace(*project) != "" {
		resolved, err := store.ResolveProjectSettings(ctx, *project)
		if err != nil {
			log.Fatalf("settings get: %v", err)
		}
		for _, r := range resolved {
			if r.Key == key {
				fmt.Printf("%s\t%s\n", r.Value, r.Source)
				return
			}
		}
		log.Fatalf("settings get: unknown key %q", key)
	}

	val, ok, err := store.FleetSetting(ctx, key)
	if err != nil {
		log.Fatalf("settings get: %v", err)
	}
	if !ok {
		fmt.Println("(unset)")
		return
	}
	fmt.Println(val)
}

func settingsSet(args []string) {
	fs := flag.NewFlagSet("settings set", flag.ExitOnError)
	dbPath := configStoreDBFlag(fs)
	project := fs.String("project", "", "Project row to override (omit to set the fleet default)")
	actor := fs.String("actor", "", "Actor recorded in the audit trail (default: $USER (cli))")
	unset := fs.Bool("unset", false, "Clear the fleet default for the key (revert to built-in)")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) != 1 || strings.TrimSpace(rest[0]) == "" {
		log.Fatalf("settings set: exactly one key=value (or key= with --unset) is required")
	}
	key, value := parseSettingAssignment(rest[0])
	who := strings.TrimSpace(*actor)
	if who == "" {
		who = defaultSettingsActor()
	}
	store := openSettingsStore(*dbPath)
	defer store.Close()
	ctx := context.Background()

	if *unset {
		if strings.TrimSpace(*project) != "" {
			log.Fatalf("settings set: --unset applies to fleet defaults only (a project override lives in its row)")
		}
		if err := store.DeleteFleetSetting(ctx, key, who); err != nil {
			log.Fatalf("settings set: %v", err)
		}
		fmt.Printf("Cleared fleet default %q (projects without an override revert to built-in).\n", key)
		return
	}

	if strings.TrimSpace(*project) != "" {
		if err := store.SetProjectSetting(ctx, *project, key, value, who); err != nil {
			log.Fatalf("settings set: %v", err)
		}
		fmt.Printf("Set %s=%s on project %q (overrides the fleet default).\n", key, value, *project)
		return
	}
	if err := store.SetFleetSetting(ctx, key, value, who); err != nil {
		log.Fatalf("settings set: %v", err)
	}
	fmt.Printf("Set fleet default %s=%s (applies to every project without its own override; hot-reloads next cycle).\n", key, value)
}

// parseSettingAssignment splits a "key=value" argument. A bare "key" (no '=')
// is treated as key with an empty value so `set --unset key` reads naturally.
func parseSettingAssignment(arg string) (string, string) {
	if i := strings.IndexByte(arg, '='); i >= 0 {
		return strings.TrimSpace(arg[:i]), strings.TrimSpace(arg[i+1:])
	}
	return strings.TrimSpace(arg), ""
}

func settingsAudit(args []string) {
	fs := flag.NewFlagSet("settings audit", flag.ExitOnError)
	dbPath := configStoreDBFlag(fs)
	limit := fs.Int("limit", 50, "Max number of journal rows to show (0 = all)")
	fs.Parse(args)

	store := openSettingsStore(*dbPath)
	defer store.Close()
	records, err := store.SettingsAudit(context.Background(), *limit)
	if err != nil {
		log.Fatalf("settings audit: %v", err)
	}
	if len(records) == 0 {
		fmt.Println("No settings changes recorded.")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "WHEN\tSCOPE\tKEY\tOLD\tNEW\tACTOR")
	for _, r := range records {
		old := r.OldValue
		if old == "" {
			old = "-"
		}
		next := r.NewValue
		if next == "" {
			next = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.At, r.Scope, r.Key, old, next, r.Actor)
	}
	w.Flush()
}

package candidatepreflight

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/daemonresources"
)

func fixture(t *testing.T) (Input, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	makeSide := func(name string, port int) *Snapshot {
		base := filepath.Join(root, name)
		opts := daemonresources.Defaults(filepath.Join(base, "maestro.db"))
		opts.Port = port
		opts.ApprovalsDB, opts.StateDB, opts.WebhookDB, opts.EmergencyDB = opts.Store, opts.Store, opts.Store, opts.Store
		data, _ := json.Marshal(opts)
		// Validation forbids /tmp for these roots; they remain absent declarations.
		scratch := filepath.Join("/var/tmp/maestro-preflight-fixture", filepath.Base(root), name)
		cfg := fmt.Sprintf("repo: example/%s\nlocal_path: %q\nstate_dir: %q\nworktree_base: %q\nworker_runtime:\n  mode: isolated\n  scratch_root: %q\nsupervisor:\n  temp_dir: %q\nself_deploy:\n  enabled: true\n  promotion_policy: explicit\n", name, filepath.Join(base, "checkout"), filepath.Join(base, "state"), filepath.Join(base, "worktrees"), filepath.Join(scratch, "workers"), filepath.Join(scratch, "supervisor"))
		return &Snapshot{Options: data, Projects: []Project{{Name: name, ConfigYAML: cfg}}}
	}
	return Input{Version: 1, Stable: makeSide("stable", 8786), Candidate: makeSide("candidate", 8787)}, root
}

func evaluate(t *testing.T, in Input) Report {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Evaluate(data)
	if err != nil {
		t.Fatal(err)
	}
	if r.ActivationAuthorized {
		t.Fatal("offline report authorized activation")
	}
	return r
}

func changeOptions(t *testing.T, s *Snapshot, fn func(*daemonresources.Options)) {
	t.Helper()
	var o daemonresources.Options
	if err := json.Unmarshal(s.Options, &o); err != nil {
		t.Fatal(err)
	}
	fn(&o)
	s.Options, _ = json.Marshal(o)
}

func hasFinding(r Report, code, stable, candidate string) bool {
	for _, f := range r.Findings {
		if f.Code == code && f.Stable == stable && f.Candidate == candidate {
			return true
		}
	}
	return false
}

func TestDistinctDeclarationsDoNotAuthorizeActivation(t *testing.T) {
	in, _ := fixture(t)
	r := evaluate(t, in)
	if r.Status != "no_declared_overlap" || len(r.Findings) != 0 || len(r.Limitations) == 0 {
		t.Fatalf("unexpected report: %+v", r)
	}
}

func TestDefaultAuxiliaryStoresRemainShared(t *testing.T) {
	in, root := fixture(t)
	in.Stable.Options = json.RawMessage(fmt.Sprintf(`{"store":%q,"port":0}`, filepath.Join(root, "stable.db")))
	in.Candidate.Options = json.RawMessage(fmt.Sprintf(`{"store":%q,"port":0}`, filepath.Join(root, "candidate.db")))
	r := evaluate(t, in)
	for _, role := range []string{"approvals_db", "state_db", "webhook_db", "emergency_db", "self_deploy_state_dir"} {
		if !hasFinding(r, "overlap", role, role) {
			t.Errorf("missing shared default %s", role)
		}
	}
	for _, res := range r.Candidate.Resources {
		if res.Role == "approvals_db" && (!res.Defaulted || !res.Active) {
			t.Fatal("JSON approvals mode must still report active delivery database")
		}
	}
}

func TestDatabaseCopiesAndLabelsDoNotPartitionQueue(t *testing.T) {
	in, _ := fixture(t)
	for _, side := range []*Snapshot{in.Stable, in.Candidate} {
		side.Projects[0].ConfigYAML = strings.Replace(side.Projects[0].ConfigYAML, "repo: example/"+side.Projects[0].Name, "repo: Example/Shared", 1) + "review_gate: none\nforge:\n  kind: forgejo\n  base_url: https://forge.example.test/team\n"
	}
	in.Stable.Projects[0].ConfigYAML += "issue_labels: [stable-ready]\nproject_id: 11111111-1111-4111-8111-111111111111\n"
	in.Candidate.Projects[0].ConfigYAML += "issue_labels: [candidate-ready]\nproject_id: 22222222-2222-4222-8222-222222222222\n"
	in.Candidate.Projects[0].ConfigYAML = strings.Replace(in.Candidate.Projects[0].ConfigYAML, "https://forge.example.test/team", "https://FORGE.example.test:443/team/", 1)
	r := evaluate(t, in)
	if !hasFinding(r, "overlap", "project:stable:queue", "project:candidate:queue") {
		t.Fatalf("same queue not detected: %+v", r)
	}
}

func TestQueueLiteralHostAndPortAliases(t *testing.T) {
	for _, pair := range [][2]string{
		{"https://forge.example.test", "https://FORGE.example.test:0443/"},
		{"https://[::1]", "https://[0:0:0:0:0:0:0:1]:443"},
	} {
		a, aErr := queueIdentity(&config.Config{Repo: "example/repo", Forge: config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: pair[0]}})
		b, bErr := queueIdentity(&config.Config{Repo: "example/repo", Forge: config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: pair[1]}})
		if aErr != "" || bErr != "" || a != b {
			t.Fatalf("literal aliases %v produced %q/%q (%q/%q)", pair, a, b, aErr, bErr)
		}
	}
}

func TestFilesystemAliasesAndDirectoryContainment(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "relative", "missing-tail", "symlink-parent", "nested-directory"} {
		t.Run(kind, func(t *testing.T) {
			in, root := fixture(t)
			real := filepath.Join(root, "real")
			if err := os.Mkdir(real, 0700); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(real, "store.db")
			if err := os.WriteFile(db, []byte("not sqlite; must never be opened as a database"), 0600); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(root, "alias.db")
			switch kind {
			case "symlink":
				if err := os.Symlink(db, alias); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(db, alias); err != nil {
					t.Fatal(err)
				}
			case "relative":
				cwd, _ := os.Getwd()
				alias, _ = filepath.Rel(cwd, db)
			case "missing-tail":
				link := filepath.Join(root, "alias-dir")
				if err := os.Symlink(real, link); err != nil {
					t.Fatal(err)
				}
				db, alias = filepath.Join(real, "missing", "db"), filepath.Join(link, "missing", "db")
			case "symlink-parent":
				child := filepath.Join(real, "child")
				if err := os.Mkdir(child, 0700); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(root, "alias-child")
				if err := os.Symlink(child, link); err != nil {
					t.Fatal(err)
				}
				alias = link + "/../store.db"
			case "nested-directory":
				alias = filepath.Join(root, "stable", "state", "sub", "db")
			}
			changeOptions(t, in.Stable, func(o *daemonresources.Options) { o.Store = db })
			changeOptions(t, in.Candidate, func(o *daemonresources.Options) { o.Store = alias })
			r := evaluate(t, in)
			role := "store"
			if kind == "nested-directory" {
				role = "project:stable:state_dir"
			}
			if !hasFinding(r, "overlap", role, "store") {
				t.Fatalf("missing %s overlap: %+v", kind, r)
			}
		})
	}
}

func TestUnknownIdentitiesNeverProduceCleanReport(t *testing.T) {
	for _, kind := range []string{"uri", "unknown-option", "missing-store", "null", "hostname", "legacy-scratch", "invalid-queue", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			in, root := fixture(t)
			switch kind {
			case "uri":
				changeOptions(t, in.Candidate, func(o *daemonresources.Options) { o.Store = "file:/candidate.db?mode=ro" })
			case "unknown-option":
				in.Candidate.Options = json.RawMessage(`{"store":"candidate.db","typo":true}`)
			case "missing-store":
				in.Candidate.Options = json.RawMessage(`{}`)
			case "null":
				in.Candidate.Options = json.RawMessage(`{"store":"candidate.db","port":null}`)
			case "hostname":
				changeOptions(t, in.Candidate, func(o *daemonresources.Options) { o.Host = "alias.example.test" })
			case "legacy-scratch":
				in.Candidate.Projects[0].ConfigYAML = strings.Replace(in.Candidate.Projects[0].ConfigYAML, "mode: isolated", "mode: legacy", 1)
			case "invalid-queue":
				in.Candidate.Projects[0].ConfigYAML = strings.Replace(in.Candidate.Projects[0].ConfigYAML, "example/candidate", "example/a/b", 1)
			case "dangling":
				link := filepath.Join(root, "dangling")
				if err := os.Symlink(filepath.Join(root, "absent"), link); err != nil {
					t.Fatal(err)
				}
				changeOptions(t, in.Candidate, func(o *daemonresources.Options) { o.Store = filepath.Join(link, "db") })
			}
			if r := evaluate(t, in); r.Status != "unresolved" {
				t.Fatalf("unknown identity produced %s: %+v", r.Status, r)
			}
		})
	}
}

func TestEndpointWildcardAndPortZero(t *testing.T) {
	in, _ := fixture(t)
	changeOptions(t, in.Candidate, func(o *daemonresources.Options) { o.Host, o.Port = "0.0.0.0", 8786 })
	if r := evaluate(t, in); !hasFinding(r, "overlap", "http_endpoint", "http_endpoint") {
		t.Fatal("wildcard bind overlap missing")
	}
	changeOptions(t, in.Candidate, func(o *daemonresources.Options) { o.Port = 0 })
	if r := evaluate(t, in); hasFinding(r, "overlap", "http_endpoint", "http_endpoint") {
		t.Fatal("disabled endpoint must not overlap")
	}
}

func TestEndpointEmptyHostMatchesFleetLoopbackDefault(t *testing.T) {
	in, _ := fixture(t)
	changeOptions(t, in.Stable, func(o *daemonresources.Options) { o.Host = "192.0.2.10" })
	changeOptions(t, in.Candidate, func(o *daemonresources.Options) { o.Host, o.Port = "  ", 8786 })
	r := evaluate(t, in)
	if hasFinding(r, "overlap", "http_endpoint", "http_endpoint") {
		t.Fatal("empty host must match FleetServer loopback default, not wildcard")
	}
	for _, res := range r.Candidate.Resources {
		if res.Role == "http_endpoint" && res.Identity != "127.0.0.1:8786" {
			t.Fatalf("effective endpoint = %q", res.Identity)
		}
	}
}

func TestPreflightDoesNotCreateOrModifyResources(t *testing.T) {
	in, root := fixture(t)
	for _, name := range []string{"stable", "candidate"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"maestro.db", "maestro.db-wal", "maestro.db-shm"} {
			if err := os.WriteFile(filepath.Join(dir, file), []byte("sentinel "+file), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A broken legacy DB must not be discovered, read or initialized.
	if err := os.MkdirAll(filepath.Join(root, "home", ".maestro"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "home", ".maestro", "config.db"), []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]string {
		out := map[string]string{}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			var data []byte
			if !entry.IsDir() {
				data, err = os.ReadFile(path)
				if err != nil {
					return err
				}
			}
			out[path] = fmt.Sprintf("%v|%v|%s", info.Mode(), info.ModTime(), data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	t.Setenv("PATH", filepath.Join(root, "no-programs"))
	_ = evaluate(t, in)
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("preflight changed resource tree: before=%v after=%v", before, after)
	}
}

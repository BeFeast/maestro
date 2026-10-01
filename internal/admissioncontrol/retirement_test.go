package admissioncontrol

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type retirementGolden struct {
	SealArgs   SealRequest   `json:"seal_args"`
	RetireArgs RetireRequest `json:"retire_args"`
	Held       NativeOutcome `json:"held"`
	Retired    NativeOutcome `json:"retired"`
}

func TestRetireNativeUsesV2AndChecksExactRequestAndOperator(t *testing.T) {
	for _, mode := range []string{"valid", "other_operator", "other_request", "lost_reply"} {
		t.Run(mode, func(t *testing.T) {
			g := readRetirementGolden(t)
			g.Retired.OperatorRetirement.OperatorUID = uint32(os.Geteuid())
			if mode == "other_operator" {
				g.Retired.OperatorRetirement.OperatorUID++
			}
			if mode == "other_request" {
				g.Retired.OperatorRetirement.Reason = "other_reason"
			}
			refreshRetirementDigests(t, &g.Retired)
			dir, err := os.MkdirTemp("", "retire-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "c")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				var prefix [4]byte
				if _, err = io.ReadFull(conn, prefix[:]); err != nil {
					done <- err
					return
				}
				body := make([]byte, binary.BigEndian.Uint32(prefix[:]))
				if _, err = io.ReadFull(conn, body); err != nil {
					done <- err
					return
				}
				var sent struct {
					Version int
					ID, Op  string
					Args    RetireRequest
				}
				if err = json.Unmarshal(body, &sent); err != nil {
					done <- err
					return
				}
				want, _ := canonicalObjectDigest(g.RetireArgs)
				got, _ := canonicalObjectDigest(sent.Args)
				if sent.Version != 2 || sent.ID != g.SealArgs.NativeSessionID || sent.Op != "retire_native" || want != got {
					done <- io.ErrUnexpectedEOF
					return
				}
				if mode != "lost_reply" {
					response, _ := json.Marshal(map[string]any{"version": 2, "id": sent.ID, "ok": true, "result": g.Retired})
					binary.BigEndian.PutUint32(prefix[:], uint32(len(response)))
					_, err = conn.Write(append(prefix[:], response...))
				}
				done <- err
			}()
			_, err = (Client{SocketPath: socket, AuthorityUID: uint32(os.Geteuid()), Timeout: time.Second}).RetireNative(g.RetireArgs)
			if (mode == "valid") != (err == nil) {
				t.Fatalf("mode %s: %v", mode, err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
			if conn, err := listener.Accept(); err == nil {
				conn.Close()
				t.Fatal("automatic RPC replay")
			}
		})
	}
}

func readRetirementGolden(t *testing.T) retirementGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/native-operator-retirement-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden retirementGolden
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

func TestOperatorRetirementPythonGolden(t *testing.T) {
	g := readRetirementGolden(t)
	if !g.RetireArgs.Valid() {
		t.Fatal("invalid request fixture")
	}
	for _, outcome := range []NativeOutcome{g.Held, g.Retired} {
		if err := ValidateNativeOutcome(outcome, g.SealArgs); err != nil {
			t.Fatal(err)
		}
		wire, _ := json.Marshal(map[string]any{"version": 2, "id": g.SealArgs.NativeSessionID, "ok": true, "result": outcome})
		if _, err := decodeNativeOutcome(wire, g.SealArgs); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := canonicalObjectDigest(g.RetireArgs)
	if err != nil || digest != g.Retired.OperatorRetirement.RequestDigest {
		t.Fatal("cross-language request digest mismatch", err)
	}
}

func refreshRetirementDigests(t *testing.T, o *NativeOutcome) {
	t.Helper()
	p := o.OperatorRetirement
	p.RequestDigest, _ = canonicalObjectDigest(p.Request())
	p.RetirementDigest, _ = canonicalObjectDigest(p, "retirement_digest")
	o.SnapshotDigest, _ = outcomeDigest(*o)
	o.EvidenceID = "native-outcome-v2:" + o.SnapshotDigest
}

func TestOperatorRetirementRejectsChangedAccountingAndProof(t *testing.T) {
	for name, mutate := range map[string]func(*NativeOutcome){
		"resolved liability": func(o *NativeOutcome) { o.UnresolvedAttempts = 0; o.TerminalAttempts = 1 },
		"refunded request":   func(o *NativeOutcome) { o.PhysicalAttempts = 0; o.UnresolvedAttempts = 0 },
		"other run":          func(o *NativeOutcome) { o.Binding.RunID = "other" },
		"other attempt set":  func(o *NativeOutcome) { o.AttemptsDigest = strings.Repeat("f", 64) },
		"money asserted":     func(o *NativeOutcome) { o.MoneyStatus = "known" },
		"no prior hold":      func(o *NativeOutcome) { o.OperatorRetirement.PriorOutcome.HoldCode = nil },
		"cap violation":      func(o *NativeOutcome) { o.OperatorRetirement.PriorOutcome.CapViolations = 1 },
		"wrong native": func(o *NativeOutcome) {
			o.OperatorRetirement.LocalTerminationAttestation.NativeSessionID = "22222222-2222-4222-8222-222222222222"
		},
		"wrong cgroup": func(o *NativeOutcome) {
			o.OperatorRetirement.LocalTerminationAttestation.Cgroup = "/maestro.slice/other.service"
		},
		"no invocation":      func(o *NativeOutcome) { o.OperatorRetirement.LocalTerminationAttestation.InvocationID = "" },
		"empty observations": func(o *NativeOutcome) { o.OperatorRetirement.AttemptObservations = nil },
		"duplicate attempt": func(o *NativeOutcome) {
			o.OperatorRetirement.AttemptObservations = append(o.OperatorRetirement.AttemptObservations, o.OperatorRetirement.AttemptObservations[0])
		},
		"invalid observation digest": func(o *NativeOutcome) { o.OperatorRetirement.AttemptObservations[0].ObservationDigest = "invalid" },
		"invalid reason":             func(o *NativeOutcome) { o.OperatorRetirement.Reason = "" },
	} {
		t.Run(name, func(t *testing.T) {
			g := readRetirementGolden(t)
			mutate(&g.Retired)
			refreshRetirementDigests(t, &g.Retired)
			if ValidateNativeOutcome(g.Retired, g.SealArgs) == nil {
				t.Fatal("accepted changed retirement")
			}
		})
	}
}

func TestOperatorRetirementStrictNestedShape(t *testing.T) {
	g := readRetirementGolden(t)
	valid, _ := json.Marshal(g.Retired)
	for _, change := range [][2]string{
		{`"operator_uid":1000`, `"operator_uid":null`},
		{`"operator_uid":1000`, `"operator_uid":1000.0`},
		{`"operator_uid":1000`, `"operator_uid":4294967296`},
		{`"operator_uid":1000`, `"operator_uid":1000,"operator_uid":1000`},
		{`"operator_uid":1000`, `"operator_uid":1000,"extra":false`},
		{`"invocation_id":`, `"extra":true,"invocation_id":`},
		{`"observation_digest":`, `"extra":true,"observation_digest":`},
		{`"prior_outcome":{`, `"prior_outcome":{"operator_retirement":{},`},
		{`"reason":"operator_reviewed_unknown_usage",`, ``},
	} {
		changed := strings.Replace(string(valid), change[0], change[1], 1)
		if changed == string(valid) {
			t.Fatalf("fixture substitution missed: %s", change[0])
		}
		var outcome NativeOutcome
		if json.Unmarshal([]byte(changed), &outcome) == nil {
			t.Fatalf("accepted invalid proof: %s", change[0])
		}
	}
	// Merely adding an operator proof to an ordinary held outcome must fail.
	g.Held.OperatorRetirement = g.Retired.OperatorRetirement
	changed, _ := json.Marshal(g.Held)
	var outcome NativeOutcome
	if json.Unmarshal(changed, &outcome) == nil {
		t.Fatal("ordinary outcome accepted retirement field")
	}
}

package admissioncontrol

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Produced by the Python authority at ebe057213cee2ae04e85009c10b22398269e9365.
// These include real ledger proofs, rather than Go-generated expected digests.
func TestRequestOutcomePythonGolden(t *testing.T) {
	body, err := os.ReadFile("testdata/native-request-outcomes-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Case    string        `json:"case"`
		Request SealRequest   `json:"request"`
		Result  NativeOutcome `json:"result"`
	}
	if err := json.Unmarshal(body, &fixtures); err != nil || len(fixtures) != 6 {
		t.Fatalf("invalid authority fixtures: %v", err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Case, func(t *testing.T) {
			if err := ValidateNativeOutcome(fixture.Result, fixture.Request); err != nil {
				t.Fatal(err)
			}
			actual, err := decodeNativeOutcome(requestOutcomeEnvelope(fixture.Request, fixture.Result), fixture.Request)
			if err != nil || !reflect.DeepEqual(actual, fixture.Result) {
				t.Fatalf("Python/Go outcome mismatch: %+v %v", actual, err)
			}
			encoded, err := json.Marshal(actual)
			if err != nil {
				t.Fatal(err)
			}
			var saved NativeOutcome
			if err := json.Unmarshal(encoded, &saved); err != nil || !reflect.DeepEqual(saved, actual) {
				t.Fatalf("persisted outcome changed: %+v %v", saved, err)
			}
		})
	}
}

func requestOutcomeFixture(request SealRequest) NativeOutcome {
	o := NativeOutcome{SchemaVersion: 2, AdmissionBasis: "requests", MoneyStatus: "unknown",
		Binding: request.Binding, RegistrationVersion: request.RegistrationVersion, Sealed: true,
		Outcome: "request_accounted", NextGenerationAllowed: true, PhysicalAttempts: 2, TerminalAttempts: 2,
		AttemptsDigest: strings.Repeat("a", 64)}
	o.SnapshotDigest, _ = outcomeDigest(o)
	o.EvidenceID = "native-outcome-v2:" + o.SnapshotDigest
	return o
}

func requestOutcomeEnvelope(request SealRequest, o NativeOutcome) []byte {
	b, _ := json.Marshal(map[string]any{"version": 2, "id": request.NativeSessionID, "ok": true, "result": o})
	return b
}

func TestRequestOutcomeCannotBecomeMonetarySettlement(t *testing.T) {
	r := requestFixture()
	r.AdmissionBasis = "requests"
	seal := SealRequest{Binding: r.Binding, RegistrationVersion: r.ExpectedVersion}
	o := requestOutcomeFixture(seal)
	valid := requestOutcomeEnvelope(seal, o)
	if _, err := decodeNativeOutcome(valid, seal); err != nil {
		t.Fatal(err)
	}
	old := seal
	old.AdmissionBasis = ""
	if _, err := decodeNativeOutcome(valid, old); err == nil || ValidateNativeOutcome(o, old) == nil {
		t.Fatal("request accounting satisfied a monetary request")
	}
	for _, change := range []func(*NativeOutcome){
		func(o *NativeOutcome) { o.Outcome = "settled" },
		func(o *NativeOutcome) { o.MoneyStatus = "zero" },
		func(o *NativeOutcome) { o.SchemaVersion = 1 },
		func(o *NativeOutcome) { o.AdmissionBasis = "" },
		func(o *NativeOutcome) { o.Binding.AdmissionBasis = "" },
		func(o *NativeOutcome) { o.CapViolations = 1 },
		func(o *NativeOutcome) { o.TerminalAttempts--; o.UnresolvedAttempts++ },
	} {
		bad := o
		change(&bad)
		bad.SnapshotDigest, _ = outcomeDigest(bad)
		bad.EvidenceID = "native-outcome-v2:" + bad.SnapshotDigest
		if _, err := decodeNativeOutcome(requestOutcomeEnvelope(seal, bad), seal); err == nil {
			t.Fatalf("accepted downgraded request accounting: %+v", bad)
		}
	}
	for _, pair := range [][2]string{
		{`"version":2`, `"version":1`},
		{`"cap_violations":0`, `"cap_violations":null`},
		{`"cap_violations":0`, `"bound_violations":0`},
		{`"cap_violations":0`, `"cap_violations":0,"cost_microusd":0`},
		{`"money_status":"unknown"`, `"money_status":"unknown","money_status":"zero"`},
	} {
		bad := strings.Replace(string(valid), pair[0], pair[1], 1)
		if bad == string(valid) {
			t.Fatal("missing mutation target", pair[0])
		}
		if _, err := decodeNativeOutcome([]byte(bad), seal); err == nil {
			t.Fatal("accepted ambiguous request outcome", bad)
		}
	}
	// A saved receipt retains its explicit discriminator and rejects money data.
	b, _ := json.Marshal(o)
	var saved NativeOutcome
	if json.Unmarshal(b, &saved) != nil || ValidateNativeOutcome(saved, seal) != nil {
		t.Fatal("could not restore typed request proof")
	}
	b = []byte(strings.Replace(string(b), `"cap_violations":0`, `"cap_violations":0,"bound_violations":0`, 1))
	if json.Unmarshal(b, &saved) == nil {
		t.Fatal("saved request outcome silently ignored monetary fields")
	}
}

func TestRequestRegistrationRejectsDowngradeAndUnknownBasis(t *testing.T) {
	r := requestFixture()
	r.AdmissionBasis = "requests"
	valid, _ := json.Marshal(map[string]any{"version": 2, "id": r.NativeSessionID, "ok": true, "result": Acknowledgement{Binding: r.Binding, RegistrationVersion: r.ExpectedVersion}})
	if _, err := decodeResponse(valid, r); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{`"version":2`, `"version":1`},
		{`"admission_basis":"requests",`, ``},
		{`"admission_basis":"requests"`, `"admission_basis":"money"`},
		{`"admission_basis":"requests"`, `"admission_basis":null`},
	} {
		bad := strings.Replace(string(valid), pair[0], pair[1], 1)
		if bad == string(valid) {
			t.Fatal("fixture mutation missed")
		}
		if _, err := decodeResponse([]byte(bad), r); err == nil {
			t.Fatal("accepted downgraded binding", bad)
		}
	}
	r.AdmissionBasis = "api_equivalent"
	if r.Valid() || r.ProtocolVersion() != 0 {
		t.Fatal("accepted unsupported admission basis")
	}
}

func TestRequestOutcomeHeldAndNoDispatchVariants(t *testing.T) {
	r := requestFixture()
	r.AdmissionBasis = "requests"
	seal := SealRequest{Binding: r.Binding, RegistrationVersion: r.ExpectedVersion}
	for _, code := range []string{"", "outcome_unknown", "cap_violation", "attempt_limit_exceeded"} {
		o := requestOutcomeFixture(seal)
		o.PhysicalAttempts, o.TerminalAttempts, o.Outcome = 0, 0, "no_dispatch"
		if code != "" {
			o.Outcome, o.NextGenerationAllowed, o.HoldCode = "held", false, &code
			o.PhysicalAttempts, o.UnresolvedAttempts = 1, 1
			if code == "cap_violation" {
				o.CapViolations = 1
			}
			if code == "attempt_limit_exceeded" {
				o.PhysicalAttempts, o.UnresolvedAttempts = 4097, 4097
			}
		}
		o.SnapshotDigest, _ = outcomeDigest(o)
		o.EvidenceID = "native-outcome-v2:" + o.SnapshotDigest
		if _, err := decodeNativeOutcome(requestOutcomeEnvelope(seal, o), seal); err != nil {
			t.Fatal(code, err)
		}
	}
}

func TestRequestNativeControlUsesExplicitVersionAndBinding(t *testing.T) {
	dir, err := os.MkdirTemp("", "request-v2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "c")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	r := requestFixture()
	r.AdmissionBasis = "requests"
	seal := SealRequest{Binding: r.Binding, RegistrationVersion: r.ExpectedVersion}
	done := make(chan error, 1)
	go func() {
		for _, op := range []string{"register", "seal_native"} {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			var length [4]byte
			if _, err = io.ReadFull(conn, length[:]); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(length[:]))
			if _, err = io.ReadFull(conn, body); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			var request struct {
				Version int
				Op      string
				Args    RegistrationRequest
			}
			if json.Unmarshal(body, &request) != nil || request.Version != 2 || request.Op != op || request.Args.Binding != r.Binding {
				_ = conn.Close()
				done <- &Hold{Code: "wire_mismatch"}
				return
			}
			var response []byte
			if op == "register" {
				response, _ = json.Marshal(map[string]any{"version": 2, "id": r.NativeSessionID, "ok": true, "result": Acknowledgement{Binding: r.Binding, RegistrationVersion: r.ExpectedVersion}})
			} else {
				response = requestOutcomeEnvelope(seal, requestOutcomeFixture(seal))
			}
			binary.BigEndian.PutUint32(length[:], uint32(len(response)))
			_, err = conn.Write(append(length[:], response...))
			_ = conn.Close()
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	client := Client{SocketPath: socket, AuthorityUID: uint32(os.Geteuid()), Timeout: time.Second}
	ack, err := client.Register(r)
	if err != nil || ack.Binding != r.Binding {
		t.Fatal(ack, err)
	}
	outcome, err := client.SealNative(seal)
	if err != nil || outcome.Outcome != "request_accounted" || outcome.MoneyStatus != "unknown" {
		t.Fatal(outcome, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

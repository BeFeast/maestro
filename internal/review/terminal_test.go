package review

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func terminalBody(reason, scope string, committed bool, retry string) string {
	return fmt.Sprintf(`{"error":{"message":"private-token-do-not-store","terminal":{"correlation_id":"request-123","reason":%q,"attempted":1,"retry_scope":%q,"stream_committed":%t%s}}}`, reason, scope, committed, retry)
}

func TestGatewayTerminalStrictEnvelope(t *testing.T) {
	good := terminalBody("quota_cooldown", "same_request", false, `,"retry_at":"2030-01-01T00:01:00Z"`)
	cases := map[string]string{
		"duplicate control":        strings.Replace(good, `"attempted":1`, `"attempted":1,"attempted":0`, 1),
		"escaped duplicate":        strings.Replace(good, `"reason":"quota_cooldown"`, `"reason":"quota_cooldown","rea\u0073on":"unknown"`, 1),
		"case shadow committed":    strings.Replace(good, `"stream_committed":false`, `"stream_committed":true,"Stream_Committed":false`, 1),
		"case shadow reason":       strings.Replace(good, `"reason":"quota_cooldown"`, `"reason":"unknown","Reason":"upstream_transient"`, 1),
		"Unicode shadow committed": strings.Replace(good, `"stream_committed":false`, `"stream_committed":true,"ſtream_committed":false`, 1),
		"missing boolean":          strings.Replace(good, `"stream_committed":false,`, "", 1),
		"null boolean":             strings.Replace(good, `"stream_committed":false`, `"stream_committed":null`, 1),
		"string boolean":           strings.Replace(good, `"stream_committed":false`, `"stream_committed":"false"`, 1),
		"negative attempts":        strings.Replace(good, `"attempted":1`, `"attempted":-1`, 1),
		"fraction attempts":        strings.Replace(good, `"attempted":1`, `"attempted":1.1`, 1),
		"missing attempts":         strings.Replace(good, `"attempted":1,`, "", 1),
		"unknown reason":           strings.Replace(good, "quota_cooldown", "new_reason", 1),
		"unknown scope":            strings.Replace(good, "same_request", "new_scope", 1),
		"invalid timestamp":        strings.Replace(good, "2030-01-01T00:01:00Z", "later", 1),
		"null timestamp":           strings.Replace(good, `"2030-01-01T00:01:00Z"`, `null`, 1),
		"nonUTC timestamp":         strings.Replace(good, "2030-01-01T00:01:00Z", "2030-01-01T00:01:00+02:00", 1),
		"committed same request":   strings.Replace(good, `"stream_committed":false`, `"stream_committed":true`, 1),
		"trailing data":            good + `{}`,
		"both locations":           strings.Replace(good, `{"error":`, `{"terminal":{},"error":`, 1),
		"quoted envelope":          fmt.Sprintf(`{"error":{"message":%q}}`, good),
		"wrong nested location":    fmt.Sprintf(`{"error":{"metadata":%s}}`, good),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			e := terminalError(429, []byte(body), false)
			if e.Terminal != nil {
				t.Fatalf("untrusted metadata accepted: %+v", e)
			}
		})
	}
	for _, reason := range []string{"quota_cooldown", "credentials_unavailable", "upstream_transient", "request_invalid", "cancelled", "unknown"} {
		e := terminalError(429, []byte(terminalBody(reason, "none", false, ` ,"future":{"extra":true}`)), false)
		if e.Terminal == nil || e.Terminal.Reason != reason {
			t.Fatalf("valid reason lost: %+v", e)
		}
	}
	for _, scope := range []string{"same_request", "new_turn_only", "none"} {
		if terminalError(503, []byte(terminalBody("unknown", scope, false, "")), false).Terminal == nil {
			t.Fatal(scope)
		}
	}
	top := `{"error":"upstream","terminal":{"reason":"unknown","attempted":0,"retry_scope":"none","stream_committed":false}}`
	if terminalError(503, []byte(top), false).Terminal == nil {
		t.Fatal("documented top-level fallback lost")
	}
}

func TestChatLensTerminalBoundaryAndBodyLimit(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"typed", terminalBody("quota_cooldown", "same_request", false, `,"retry_at":"2030-01-01T00:01:00Z"`), 429, "quota_cooldown"},
		{"oversize", strings.Repeat("x", maxErrorBody+1), 503, "response_too_large"},
		{"unsupported", `{"error":{"message":"Bearer private-token"}}`, 429, "terminal_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			_, err := (&ChatLens{BaseURL: srv.URL, Model: "fixture", APIKey: "secret"}).Run(context.Background(), "private prompt")
			var terminal *GatewayTerminalError
			if !errors.As(err, &terminal) || terminal.Code != tc.code {
				t.Fatalf("%+v", err)
			}
			if strings.Contains(err.Error(), "private-token") {
				t.Fatal("raw error leaked")
			}
			if tc.name == "oversize" && !terminal.ResponseTruncated {
				t.Fatal("digest not marked as truncated")
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, terminalBody("quota_cooldown", "same_request", false, ""))
	}))
	defer srv.Close()
	text, err := (&ChatLens{BaseURL: srv.URL, Model: "fixture", APIKey: "secret"}).Run(context.Background(), "p")
	if err != nil || !strings.Contains(text, "terminal") {
		t.Fatalf("model text treated as control: %q %v", text, err)
	}
}

package aiexecution

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// NativeForgejoCreatePullRequest only creates a PR at the fixed Forgejo host.
// Server-side token/branch ACLs remain a separate provisioning invariant.
func NativeForgejoCreatePullRequest(repo, token string, input io.Reader, output io.Writer) error {
	if !strings.HasPrefix(repo, "BeFeast/") || strings.Count(repo, "/") != 1 || strings.ContainsAny(repo, " ?#%\r\n") || strings.Contains(repo, "..") || token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return Held("containment_forgejo_credential_unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
	if err != nil || len(b) > 64<<10 {
		return Held("native_pr_request_invalid")
	}
	var request struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if DecodeStrict(b, &request) != nil || strings.TrimSpace(request.Title) == "" || len(request.Title) > 256 || request.Base != "main" || !strings.HasPrefix(request.Head, "feat/") || strings.ContainsAny(request.Head, " :?#%\r\n") || strings.Contains(request.Head, "..") {
		return Held("native_pr_request_invalid")
	}
	body, _ := json.Marshal(request)
	req, err := http.NewRequest(http.MethodPost, "https://git.oklabs.uk/api/v1/repos/"+repo+"/pulls", bytes.NewReader(body))
	if err != nil {
		return Held("native_pr_request_invalid")
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return Held("native_pr_delivery_unknown")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return Held("native_pr_delivery_rejected")
	}
	b, err = io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(b) > 64<<10 {
		return Held("native_pr_delivery_unknown")
	}
	var result struct {
		Number int    `json:"number"`
		URL    string `json:"html_url"`
	}
	if json.Unmarshal(b, &result) != nil || result.Number <= 0 || !strings.HasPrefix(result.URL, "https://git.oklabs.uk/"+repo+"/pulls/") {
		return Held("native_pr_delivery_unknown")
	}
	return json.NewEncoder(output).Encode(result)
}

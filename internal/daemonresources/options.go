// Package daemonresources describes daemon resource options without opening them.
package daemonresources

import (
	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/emergencystore"
	"github.com/befeast/maestro/internal/statestore"
	"github.com/befeast/maestro/internal/webhookstore"
)

type Options struct {
	Store             string `json:"store"`
	Host              string `json:"host"`
	Port              int    `json:"port"`
	ApprovalsStore    string `json:"approvals_store"`
	ApprovalsDB       string `json:"approvals_db"`
	StateStore        string `json:"state_store"`
	StateDB           string `json:"state_db"`
	WebhookSecretFile string `json:"webhook_secret_file"`
	WebhookDB         string `json:"webhook_db"`
	EmergencyDB       string `json:"emergency_db"`
}

// Defaults shares the CLI's resource defaults without probing the legacy store.
// Callers supply the selected store explicitly; offline callers must not run the
// CLI's compatibility discovery, which inspects local database contents.
func Defaults(store string) Options {
	return Options{
		Store: store, Host: "127.0.0.1", Port: 8786,
		ApprovalsStore: "json", ApprovalsDB: approvalstore.DefaultDBPath(),
		StateStore: "json", StateDB: statestore.DefaultDBPath(),
		WebhookDB: webhookstore.DefaultDBPath(), EmergencyDB: emergencystore.DefaultDBPath(),
	}
}

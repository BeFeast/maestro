package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

type Notifier struct {
	OpenclawURL string
	Target      string
}

func New(openclawURL, target string) *Notifier {
	return &Notifier{
		OpenclawURL: openclawURL,
		Target:      target,
	}
}

type messagePayload struct {
	Channel string `json:"channel"`
	Target  string `json:"target"`
	Message string `json:"message"`
}

func (n *Notifier) Send(msg string) error {
	if n.OpenclawURL == "" || n.Target == "" {
		log.Printf("[notify] skipping notification (no openclaw_url or target): %s", msg)
		return nil
	}

	payload := messagePayload{
		Channel: "telegram",
		Target:  n.Target,
		Message: msg,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	url := n.OpenclawURL + "/api/v1/message"
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("post to openclaw: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("openclaw returned %d", resp.StatusCode)
	}

	return nil
}

func (n *Notifier) Sendf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if err := n.Send(msg); err != nil {
		log.Printf("[notify] failed to send: %v", err)
	}
}

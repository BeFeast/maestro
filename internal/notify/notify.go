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
	BotToken    string
	Target      string
	OpenclawURL string
}

func New(openclawURL, target string) *Notifier {
	return &Notifier{OpenclawURL: openclawURL, Target: target}
}

func NewWithToken(botToken, target, openclawURL string) *Notifier {
	return &Notifier{BotToken: botToken, Target: target, OpenclawURL: openclawURL}
}

func (n *Notifier) Send(msg string) error {
	if n.Target == "" {
		return nil
	}
	if n.BotToken != "" {
		return n.sendTelegram(msg)
	}
	if n.OpenclawURL != "" {
		return n.sendOpenclaw(msg)
	}
	log.Printf("[notify] no transport configured, skipping: %s", msg)
	return nil
}

func (n *Notifier) sendTelegram(msg string) error {
	payload, _ := json.Marshal(map[string]string{
		"chat_id":    n.Target,
		"text":       msg,
		"parse_mode": "Markdown",
	})
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.BotToken)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("telegram api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram returned %d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) sendOpenclaw(msg string) error {
	payload, _ := json.Marshal(map[string]string{
		"channel": "telegram", "target": n.Target, "message": msg,
	})
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(
		n.OpenclawURL+"/api/v1/message/send", "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("openclaw: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("openclaw returned %d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) Sendf(format string, args ...any) {
	if err := n.Send(fmt.Sprintf(format, args...)); err != nil {
		log.Printf("[notify] failed to send: %v", err)
	}
}

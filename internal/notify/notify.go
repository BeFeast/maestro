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
	OpenclawURL string // legacy fallback
}

func New(openclawURL, target string) *Notifier {
	return &Notifier{
		OpenclawURL: openclawURL,
		Target:      target,
	}
}

func NewWithToken(botToken, target, openclawURL string) *Notifier {
	return &Notifier{
		BotToken:    botToken,
		Target:      target,
		OpenclawURL: openclawURL,
	}
}

func (n *Notifier) Send(msg string) error {
	if n.Target == "" {
		log.Printf("[notify] skipping (no target): %s", msg)
		return nil
	}

	if n.BotToken != "" {
		return n.sendViaTelegram(msg)
	}
	if n.OpenclawURL != "" {
		return n.sendViaOpenclaw(msg)
	}

	log.Printf("[notify] skipping (no bot_token or openclaw_url): %s", msg)
	return nil
}

func (n *Notifier) sendViaTelegram(msg string) error {
	type tgPayload struct {
		ChatID    string `json:"chat_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}

	payload := tgPayload{
		ChatID:    n.Target,
		Text:      msg,
		ParseMode: "Markdown",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.BotToken)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("telegram api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram returned %d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) sendViaOpenclaw(msg string) error {
	type ocPayload struct {
		Channel string `json:"channel"`
		Target  string `json:"target"`
		Message string `json:"message"`
	}

	payload := ocPayload{Channel: "telegram", Target: n.Target, Message: msg}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	url := n.OpenclawURL + "/api/v1/message/send"
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
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
	msg := fmt.Sprintf(format, args...)
	if err := n.Send(msg); err != nil {
		log.Printf("[notify] failed to send: %v", err)
	}
}

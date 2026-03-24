package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Notifier struct {
	BotToken    string
	Target      string
	Mode        string // "direct" (Telegram Bot API) or "openclaw" (OpenClaw relay)
	OpenclawURL string

	mu         sync.Mutex
	digestMode bool
	buffer     []string
}

func New(openclawURL, target string) *Notifier {
	return &Notifier{OpenclawURL: openclawURL, Target: target, Mode: "openclaw"}
}

func NewWithToken(botToken, target, mode, openclawURL string) *Notifier {
	if mode == "" {
		mode = "direct"
	}
	return &Notifier{BotToken: botToken, Target: target, Mode: mode, OpenclawURL: openclawURL}
}

// SetDigestMode enables or disables digest mode.
// In digest mode, messages are buffered and sent as a single combined
// message when Flush() is called.
func (n *Notifier) SetDigestMode(enabled bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.digestMode = enabled
}

// Flush sends all buffered messages as a single combined message.
// No-op if buffer is empty or digest mode is off.
func (n *Notifier) Flush() error {
	n.mu.Lock()
	msgs := n.buffer
	n.buffer = nil
	n.mu.Unlock()

	if len(msgs) == 0 {
		return nil
	}

	combined := "📋 *maestro digest:*\n\n" + strings.Join(msgs, "\n\n")
	if err := n.send(combined); err != nil {
		log.Printf("[notify] digest flush failed: %v", err)
		return err
	}
	return nil
}

// Buffered returns the number of buffered messages (for testing).
func (n *Notifier) Buffered() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.buffer)
}

func (n *Notifier) Send(msg string) error {
	if n.Target == "" {
		return nil
	}

	n.mu.Lock()
	digest := n.digestMode
	n.mu.Unlock()

	if digest {
		n.mu.Lock()
		n.buffer = append(n.buffer, msg)
		n.mu.Unlock()
		log.Printf("[notify] buffered (digest mode): %s", msg)
		return nil
	}

	return n.send(msg)
}

func (n *Notifier) send(msg string) error {
	switch n.Mode {
	case "openclaw":
		if n.OpenclawURL != "" {
			return n.sendOpenclaw(msg)
		}
		log.Printf("[notify] mode=openclaw but openclaw_url not configured, skipping: %s", msg)
		return nil
	default: // "direct" or unset
		if n.BotToken != "" {
			return n.sendTelegram(msg)
		}
		// Fallback to openclaw if bot_token is not set but openclaw_url is available
		if n.OpenclawURL != "" {
			return n.sendOpenclaw(msg)
		}
		log.Printf("[notify] no transport configured, skipping: %s", msg)
		return nil
	}
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

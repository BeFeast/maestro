package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Notifier struct {
	BotToken    string
	Target      string
	Mode        string // "direct" (Telegram Bot API) or "openclaw" (OpenClaw relay)
	OpenclawURL string

	// ntfy push transport (#1018). When NtfyBaseURL+NtfyTopic are set, the
	// alert-class router fans out to ntfy via a plain HTTP POST. NtfyToken is
	// resolved from the environment by the caller — never stored in config.
	NtfyBaseURL string
	NtfyTopic   string
	NtfyToken   string

	mu         sync.Mutex
	digestMode bool
	buffer     []string
	// alertState dedups (class,key) alerts: the last body sent per identity.
	// An alert whose body equals the last-sent body is a no-op (no state
	// change), so a supervisor cycle re-emitting the same condition sends once.
	alertState map[string]string
	// alertInFlight reserves an alert that is being sent right now, so
	// concurrent callers with the same (class,key,body) collapse to one send
	// without recording delivery before it succeeds.
	alertInFlight map[string]string
}

// WithNtfy configures the ntfy push transport and returns the notifier for
// chaining. base and topic empty leaves the transport disabled.
func (n *Notifier) WithNtfy(base, topic, token string) *Notifier {
	n.NtfyBaseURL = base
	n.NtfyTopic = topic
	n.NtfyToken = token
	return n
}

// NtfyConfigured reports whether the ntfy transport can POST.
func (n *Notifier) NtfyConfigured() bool {
	return strings.TrimSpace(n.NtfyBaseURL) != "" && strings.TrimSpace(n.NtfyTopic) != ""
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

// Alert emits a classified operator alert (#1018). The class selects the ntfy
// priority + tags; key is the dedup identity (e.g. "project:gate"); title and
// body are the human-readable payload.
//
// Dedup is per-state-change: for a given (class,key) the alert fires once, then
// stays silent until body differs from the last-sent body. This mirrors the
// digest/notify contract so a supervisor loop re-detecting the same condition
// every cycle produces one notification, not one per cycle.
//
// The alert fans out to the ntfy transport when configured. When it is NOT
// configured the alert falls back to the base transport (Telegram/OpenClaw)
// rather than disappearing: an install that never set up ntfy would otherwise
// silently lose every classified alert, including the CRITICAL floor-breach
// page. The fallback loses only the priority/tag routing, which ntfy owns.
func (n *Notifier) Alert(class AlertClass, key, title, body string) error {
	stateKey := string(class) + "\x00" + key

	// Reserve the (class,key,body) under the lock so concurrent callers with an
	// identical alert still collapse to one send, then release it if the send
	// fails so the condition stays eligible for retry. Recording delivery
	// before sending would lose alerts on a transport error; not reserving at
	// all would duplicate them under concurrency.
	n.mu.Lock()
	if last, ok := n.alertState[stateKey]; ok && last == body {
		n.mu.Unlock()
		return nil // no state change since last send — dedup
	}
	if inflight, ok := n.alertInFlight[stateKey]; ok && inflight == body {
		n.mu.Unlock()
		return nil // another caller is sending this exact alert right now
	}
	if n.alertInFlight == nil {
		n.alertInFlight = make(map[string]string)
	}
	n.alertInFlight[stateKey] = body
	// In digest mode the base transport only buffers, so "sent" is not
	// "delivered" until Flush succeeds; skip the delivered-state write and let
	// the next occurrence re-buffer rather than being silenced by dedup.
	digestBuffered := !n.NtfyConfigured() && n.digestMode
	n.mu.Unlock()

	var err error
	if n.NtfyConfigured() {
		err = n.sendNtfy(title, body, RouteFor(class))
	} else {
		err = n.Send(alertFallbackText(class, title, body))
	}

	n.mu.Lock()
	delete(n.alertInFlight, stateKey)
	if err == nil && !digestBuffered {
		if n.alertState == nil {
			n.alertState = make(map[string]string)
		}
		n.alertState[stateKey] = body
	}
	n.mu.Unlock()
	return err
}

// alertFallbackText renders a classified alert for the plain-text transports,
// keeping the class visible since they carry no priority/tag metadata.
func alertFallbackText(class AlertClass, title, body string) string {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	switch {
	case title == "" && body == "":
		return fmt.Sprintf("[%s]", class)
	case title == "":
		return fmt.Sprintf("[%s] %s", class, body)
	case body == "":
		return fmt.Sprintf("[%s] %s", class, title)
	}
	return fmt.Sprintf("[%s] %s — %s", class, title, body)
}

// ResetAlertState clears the dedup memory (test/rotation helper).
func (n *Notifier) ResetAlertState() {
	n.mu.Lock()
	n.alertState = nil
	n.mu.Unlock()
}

func (n *Notifier) sendNtfy(title, body string, route AlertRoute) error {
	base := strings.TrimRight(strings.TrimSpace(n.NtfyBaseURL), "/")
	topic := strings.Trim(strings.TrimSpace(n.NtfyTopic), "/")
	url := base + "/" + topic

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("ntfy request: %w", err)
	}
	if title != "" {
		req.Header.Set("Title", title)
	}
	if route.Priority > 0 {
		req.Header.Set("Priority", strconv.Itoa(route.Priority))
	}
	if len(route.Tags) > 0 {
		req.Header.Set("Tags", strings.Join(route.Tags, ","))
	}
	if tok := strings.TrimSpace(n.NtfyToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("ntfy post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ntfy returned %d", resp.StatusCode)
	}
	return nil
}

package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Event is a control plane notification.
type Event struct {
	Kind      string `json:"kind"` // e.g. "volume_at_risk", "node_down", "repair_needed"
	Summary   string `json:"summary"`
	Detail    string `json:"detail,omitempty"`
	At        time.Time `json:"at"`
}

// Notifier fans an event out to one or more channels with per-key debouncing.
type Notifier struct {
	mu     sync.Mutex
	window time.Duration
	last   map[string]time.Time
	senders []Sender
}

type Sender interface {
	Name() string
	Send(Event) error
}

func New(debounce time.Duration) *Notifier {
	if debounce <= 0 {
		debounce = time.Hour
	}
	return &Notifier{window: debounce, last: make(map[string]time.Time)}
}

func (n *Notifier) Add(s Sender) { n.senders = append(n.senders, s) }

// Notify sends the event unless the same key was sent within the window.
func (n *Notifier) Notify(key string, e Event) {
	n.mu.Lock()
	if t, ok := n.last[key]; ok && time.Since(t) < n.window {
		n.mu.Unlock()
		return
	}
	n.last[key] = time.Now()
	n.mu.Unlock()

	for _, s := range n.senders {
		if err := s.Send(e); err != nil {
			log.Printf("notify: %s failed: %v", s.Name(), err)
		}
	}
}

// LogSender writes events to the server log.
type LogSender struct{}

func (LogSender) Name() string { return "log" }
func (LogSender) Send(e Event) error {
	log.Printf("[notify %s] %s — %s", e.Kind, e.Summary, e.Detail)
	return nil
}

// WebhookSender POSTs events as JSON to a URL.
type WebhookSender struct {
	URL    string
	client *http.Client
}

func NewWebhookSender(url string) *WebhookSender {
	return &WebhookSender{URL: url, client: &http.Client{Timeout: 10 * time.Second}}
}

func (w *WebhookSender) Name() string { return fmt.Sprintf("webhook(%s)", w.URL) }
func (w *WebhookSender) Send(e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	resp, err := w.client.Post(w.URL, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
}

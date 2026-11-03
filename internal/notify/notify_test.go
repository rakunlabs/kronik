package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rakunlabs/kronik/internal/store"
)

type memQueue struct {
	mu     sync.Mutex
	items  []store.Outgoing
	sent   []string
	failed map[string]time.Time
}

func (m *memQueue) TakeNotifications(context.Context, time.Time, int) ([]store.Outgoing, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := m.items
	m.items = nil

	for _, o := range out {
		o.Notification.Attempts++
	}

	return out, nil
}

func (m *memQueue) SentNotification(_ context.Context, id string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sent = append(m.sent, id)

	return nil
}

func (m *memQueue) FailedNotification(_ context.Context, id, _ string, next time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.failed[id] = next

	return nil
}

func TestCheckChannel(t *testing.T) {
	for typ, raw := range map[string]string{
		TypeSlack:   `{"url":"https://hooks.slack.com/services/x"}`,
		TypeWebhook: `{"url":"{{secret.HOOK_URL}}","headers":{"Authorization":"Bearer {{secret.T}}"}}`,
		TypeEmail:   `{"to":["ops@example.com"," "]}`,
	} {
		if _, err := CheckChannel(typ, json.RawMessage(raw)); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}

	for typ, raw := range map[string]string{
		TypeSlack: `{"url":"ftp://x"}`,
		TypeEmail: `{"to":["not an address"]}`,
		"sms":     `{}`,
	} {
		if _, err := CheckChannel(typ, json.RawMessage(raw)); err == nil {
			t.Errorf("%s %s accepted", typ, raw)
		}
	}
}

func TestSend(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
		auth   string
	)

	fail := true

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		mu.Lock()
		defer mu.Unlock()

		if r.URL.Path == "/flaky" && fail {
			fail = false

			w.WriteHeader(http.StatusBadGateway)

			return
		}

		bodies = append(bodies, string(b))
		auth = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	since := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
	ev := func(e string) json.RawMessage {
		b, _ := json.Marshal(store.Event{Event: e, JobID: "j1", JobName: "backup", RunID: "r1", Status: "failed", Error: "exit code 2", Since: &since})

		return b
	}

	slackCfg, _ := CheckChannel(TypeSlack, json.RawMessage(`{"url":"`+srv.URL+`/slack"}`))
	hookCfg, _ := CheckChannel(TypeWebhook, json.RawMessage(`{"url":"`+srv.URL+`/flaky","headers":{"Authorization":"Bearer {{secret.T}}"}}`))
	teamsCfg, _ := CheckChannel(TypeTeams, json.RawMessage(`{"url":"`+srv.URL+`/teams"}`))

	q := &memQueue{failed: map[string]time.Time{}, items: []store.Outgoing{
		{Notification: &store.Notification{ID: "n1", Event: "failure", Payload: ev("failure")}, Channel: &store.Channel{Name: "s", Type: TypeSlack, Config: slackCfg}},
		{Notification: &store.Notification{ID: "n2", Event: "no_run", Payload: ev("no_run")}, Channel: &store.Channel{Name: "w", Type: TypeWebhook, Config: hookCfg}},
		{Notification: &store.Notification{ID: "n3", Event: "recovery", Payload: ev("recovery")}, Channel: &store.Channel{Name: "t", Type: TypeTeams, Config: teamsCfg}},
	}}

	s := NewSender(q, func() Options {
		return Options{Enabled: true, BaseURL: "https://cron.example.com/", Environment: "prod", Secrets: map[string]string{"T": "tok"}}
	})

	if err := s.Round(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(q.sent) != 2 || q.failed["n2"].IsZero() {
		t.Fatalf("sent %v failed %v", q.sent, q.failed)
	}

	all := strings.Join(bodies, "\n")
	for _, want := range []string{"[prod] backup failed", "exit code 2", "https://cron.example.com/#/run/r1", "backup works again", "AdaptiveCard"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in\n%s", want, all)
		}
	}

	// The failed one is tried again and goes through.
	q.items = []store.Outgoing{{Notification: &store.Notification{ID: "n2", Event: "no_run", Payload: ev("no_run")},
		Channel: &store.Channel{Name: "w", Type: TypeWebhook, Config: hookCfg}}}

	if err := s.Round(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(q.sent) != 3 || auth != "Bearer tok" || !strings.Contains(bodies[len(bodies)-1], `"event":"no_run"`) ||
		!strings.Contains(bodies[len(bodies)-1], "did not run") {
		t.Fatalf("retry: sent %v auth %q body %s", q.sent, auth, bodies[len(bodies)-1])
	}

	// Turned off: nothing is taken.
	q.items = []store.Outgoing{{Notification: &store.Notification{ID: "n4", Payload: ev("failure")}, Channel: &store.Channel{Type: TypeSlack, Config: slackCfg}}}
	off := NewSender(q, func() Options { return Options{} })

	if err := off.Round(context.Background()); err != nil || len(q.items) != 1 {
		t.Fatalf("off: %v %d", err, len(q.items))
	}
}

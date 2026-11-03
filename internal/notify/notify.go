// Package notify sends the notifications queued in the store: to Slack,
// Microsoft Teams, any webhook or e-mail. Any replica sends; a notification
// taken by one is left to it for a while, then tried again.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kronik/internal/store"
)

const (
	TypeSlack   = "slack"
	TypeTeams   = "teams"
	TypeWebhook = "webhook"
	TypeEmail   = "email"
)

// Types lists the channel types.
var Types = []string{TypeSlack, TypeTeams, TypeWebhook, TypeEmail}

// ChannelConfig is a channel's settings; which fields count depends on the
// type.
type ChannelConfig struct {
	// URL is the incoming webhook (Slack, Teams) or the address to POST to.
	URL string `json:"url"`
	// Headers are sent with a webhook; values may use {{secret.NAME}}.
	Headers map[string]string `json:"headers"`
	// To are the e-mail recipients.
	To []string `json:"to"`
}

// CheckChannel checks and normalizes a channel's type and config.
func CheckChannel(typ string, raw json.RawMessage) (json.RawMessage, error) {
	var c ChannelConfig
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}

	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	switch typ {
	case TypeSlack, TypeTeams, TypeWebhook:
		c.URL = strings.TrimSpace(c.URL)
		if !strings.Contains(c.URL, "{{") {
			u, err := url.Parse(c.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return nil, errors.New("url must be an http or https URL")
			}
		}

		for k := range c.Headers {
			if strings.TrimSpace(k) == "" || strings.ContainsAny(k, " :\r\n") {
				return nil, fmt.Errorf("header name %q is not valid", k)
			}
		}

		c.To = nil
	case TypeEmail:
		var to []string

		for _, a := range c.To {
			if a = strings.TrimSpace(a); a == "" {
				continue
			}

			if _, err := mail.ParseAddress(a); err != nil {
				return nil, fmt.Errorf("%q is not an e-mail address", a)
			}

			to = append(to, a)
		}

		if len(to) == 0 {
			return nil, errors.New("give at least one recipient")
		}

		c = ChannelConfig{To: to}
	default:
		return nil, fmt.Errorf("type must be one of %s", strings.Join(Types, ", "))
	}

	if c.Headers == nil {
		c.Headers = map[string]string{}
	}

	if c.To == nil {
		c.To = []string{}
	}

	return json.Marshal(c)
}

// SMTP is how e-mail goes out.
type SMTP struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	// TLS is starttls, tls or none.
	TLS string
}

// Options are what the sender reads before every round.
type Options struct {
	Enabled bool
	BaseURL string
	SMTP    SMTP
	Secrets map[string]string
	// Environment names the installation in messages.
	Environment string
}

// Store is what the sender needs from the database.
type Store interface {
	TakeNotifications(ctx context.Context, now time.Time, limit int) ([]store.Outgoing, error)
	SentNotification(ctx context.Context, id string, now time.Time) error
	FailedNotification(ctx context.Context, id, errText string, next time.Time) error
}

// Sender sends due notifications.
type Sender struct {
	st      Store
	options func() Options
	client  *http.Client
	// Observer is told about every notification sent or failed; may be nil.
	Observer func(channelType string, ok bool)
}

func NewSender(st Store, options func() Options) *Sender {
	return &Sender{st: st, options: options, client: &http.Client{Timeout: 20 * time.Second}}
}

const (
	sendEvery   = 2 * time.Second
	sendBatch   = 20
	maxAttempts = 8
)

// Run sends until ctx ends.
func (s *Sender) Run(ctx context.Context) {
	t := time.NewTicker(sendEvery)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		if err := s.Round(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("send notifications", "error", err)
		}
	}
}

// Round sends the notifications due now.
func (s *Sender) Round(ctx context.Context) error {
	o := s.options()
	if !o.Enabled {
		return nil
	}

	due, err := s.st.TakeNotifications(ctx, time.Now().UTC(), sendBatch)
	if err != nil {
		return err
	}

	for _, d := range due {
		err := s.Send(ctx, o, d.Channel, d.Notification)

		if s.Observer != nil {
			s.Observer(d.Channel.Type, err == nil)
		}

		now := time.Now().UTC()

		if err == nil {
			if err := s.st.SentNotification(ctx, d.Notification.ID, now); err != nil {
				slog.Warn("record notification sent", "error", err)
			}

			continue
		}

		var next time.Time
		if d.Notification.Attempts < maxAttempts {
			// 30s, 1m, 2m … up to an hour.
			next = now.Add(min(time.Duration(1<<(d.Notification.Attempts-1))*30*time.Second, time.Hour))
		}

		slog.Warn("notification not sent", "channel", d.Channel.Name, "event", d.Notification.Event, "attempt", d.Notification.Attempts,
			"error", err, "retry", !next.IsZero())

		if err := s.st.FailedNotification(ctx, d.Notification.ID, cut(err.Error(), 1000), next); err != nil {
			slog.Warn("record notification failure", "error", err)
		}
	}

	return nil
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}

	return s
}

// Message is a notification as people read it.
type Message struct {
	Title string
	Text  string
	Color string
	Link  string
}

// Build turns an event into a message.
func Build(o Options, ev store.Event) Message {
	env := ""
	if o.Environment != "" {
		env = "[" + o.Environment + "] "
	}

	m := Message{}

	switch ev.Event {
	case store.NotifyFailure:
		m.Title = fmt.Sprintf("%s%s failed", env, ev.JobName)
		m.Color = "#c4372f"
		m.Text = fmt.Sprintf("Status: %s", ev.Status)

		if ev.Attempt > 1 {
			m.Text += fmt.Sprintf(" after %d attempts", ev.Attempt)
		}

		if ev.Error != "" {
			m.Text += "\nError: " + cut(ev.Error, 500)
		}
	case store.NotifyRecovery:
		m.Title = fmt.Sprintf("%s%s works again", env, ev.JobName)
		m.Color = "#276e45"
		m.Text = "The latest run succeeded."
	case store.NotifySuccess:
		m.Title = fmt.Sprintf("%s%s succeeded", env, ev.JobName)
		m.Color = "#276e45"
		m.Text = "The run succeeded."
	case store.NotifyNoRun:
		m.Title = fmt.Sprintf("%s%s did not run", env, ev.JobName)
		m.Color = "#985b0a"
		m.Text = "No successful run"
		if ev.Since != nil {
			m.Text += " since " + ev.Since.UTC().Format("2006-01-02 15:04 UTC")
		}

		if ev.Schedule != "" {
			m.Text += fmt.Sprintf(" (schedule %s)", ev.Schedule)
		}

		m.Text += "."
	case "test":
		m.Title = env + "kronik test notification"
		m.Color = "#2b5c8a"
		m.Text = "This channel works."
	default:
		m.Title = env + ev.JobName + ": " + ev.Event
	}

	if base := strings.TrimRight(o.BaseURL, "/"); base != "" {
		switch {
		case ev.RunID != "":
			m.Link = base + "/#/run/" + ev.RunID
		case ev.JobID != "":
			m.Link = base + "/#/job/" + ev.JobID
		}
	}

	return m
}

// Send sends one notification now.
func (s *Sender) Send(ctx context.Context, o Options, ch *store.Channel, n *store.Notification) error {
	var cfg ChannelConfig
	if err := json.Unmarshal(ch.Config, &cfg); err != nil {
		return fmt.Errorf("channel config: %w", err)
	}

	var ev store.Event
	if err := json.Unmarshal(n.Payload, &ev); err != nil {
		return fmt.Errorf("payload: %w", err)
	}

	msg := Build(o, ev)

	switch ch.Type {
	case TypeSlack:
		text := "*" + msg.Title + "*\n" + msg.Text
		if msg.Link != "" {
			text += "\n<" + msg.Link + "|Open in kronik>"
		}

		return s.post(ctx, o, cfg, map[string]any{
			"text":        msg.Title,
			"attachments": []map[string]any{{"color": msg.Color, "text": text, "mrkdwn_in": []string{"text"}}},
		})
	case TypeTeams:
		body := []map[string]any{
			{"type": "TextBlock", "text": msg.Title, "weight": "Bolder", "size": "Medium", "wrap": true},
			{"type": "TextBlock", "text": msg.Text, "wrap": true},
		}

		card := map[string]any{
			"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard", "version": "1.4", "body": body,
		}
		if msg.Link != "" {
			card["actions"] = []map[string]any{{"type": "Action.OpenUrl", "title": "Open in kronik", "url": msg.Link}}
		}

		return s.post(ctx, o, cfg, map[string]any{
			"type":        "message",
			"attachments": []map[string]any{{"contentType": "application/vnd.microsoft.card.adaptive", "content": card}},
		})
	case TypeWebhook:
		return s.post(ctx, o, cfg, map[string]any{
			"event": ev.Event, "title": msg.Title, "text": msg.Text, "link": msg.Link, "environment": o.Environment,
			"job": map[string]any{"id": ev.JobID, "name": ev.JobName, "schedule": ev.Schedule},
			"run": map[string]any{"id": ev.RunID, "status": ev.Status, "error": ev.Error, "attempt": ev.Attempt},
			"at":  ev.At, "since": ev.Since,
		})
	case TypeEmail:
		return sendMail(o.SMTP, cfg.To, msg)
	default:
		return fmt.Errorf("unknown channel type %q", ch.Type)
	}
}

var secretRef = regexp.MustCompile(`\{\{\s*secret\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// fill puts secrets into a channel's URL and headers.
func fill(s string, secrets map[string]string) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}

	var missing []string

	out := secretRef.ReplaceAllStringFunc(s, func(m string) string {
		name := secretRef.FindStringSubmatch(m)[1]

		v, ok := secrets[name]
		if !ok {
			missing = append(missing, name)
		}

		return v
	})

	if len(missing) > 0 {
		return "", fmt.Errorf("unknown secret %s", strings.Join(missing, ", "))
	}

	return out, nil
}

func (s *Sender) post(ctx context.Context, o Options, cfg ChannelConfig, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	u, err := fill(cfg.URL, o.Secrets)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "kronik")

	for k, v := range cfg.Headers {
		if v, err = fill(v, o.Secrets); err != nil {
			return fmt.Errorf("header %s: %w", k, err)
		}

		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			// The URL of a Slack or Teams webhook is its credential.
			return fmt.Errorf("post: %w", uerr.Err)
		}

		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))

		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	return nil
}

func sendMail(c SMTP, to []string, msg Message) error {
	if c.Host == "" || c.From == "" {
		return errors.New("e-mail is not set up: set the SMTP server and the from address (Settings → Configuration → Notifications)")
	}

	from, err := mail.ParseAddress(c.From)
	if err != nil {
		return fmt.Errorf("from address: %w", err)
	}

	text := msg.Text
	if msg.Link != "" {
		text += "\n\n" + msg.Link
	}

	var b strings.Builder

	b.WriteString("From: " + from.String() + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mimeHeader(msg.Title) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(text, "\n", "\r\n") + "\r\n")

	port := c.Port
	if port == 0 {
		port = 587
	}

	addr := net.JoinHostPort(c.Host, strconv.Itoa(port))

	var conn net.Conn

	dialer := &net.Dialer{Timeout: 15 * time.Second}

	if c.TLS == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}

	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}

	_ = conn.SetDeadline(time.Now().Add(time.Minute))

	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		_ = conn.Close()

		return err
	}
	defer func() { _ = cl.Close() }()

	if c.TLS == "starttls" || c.TLS == "" {
		if err := cl.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}

	if c.User != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.User, c.Password, c.Host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}

	if err := cl.Mail(from.Address); err != nil {
		return err
	}

	for _, a := range to {
		if err := cl.Rcpt(a); err != nil {
			return fmt.Errorf("recipient %s: %w", a, err)
		}
	}

	w, err := cl.Data()
	if err != nil {
		return err
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}

	if err := w.Close(); err != nil {
		return err
	}

	return cl.Quit()
}

// mimeHeader encodes a subject that is not plain ASCII.
func mimeHeader(s string) string {
	for _, r := range s {
		if r > 126 || r < 32 {
			return "=?utf-8?b?" + b64(s) + "?="
		}
	}

	return s
}

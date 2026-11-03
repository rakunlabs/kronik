package store

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Topics sent between replicas with PostgreSQL's NOTIFY.
const (
	// TopicWake: there is work (a run was queued or a job changed).
	TopicWake = "wake"
	// TopicSettings: settings were changed.
	TopicSettings = "settings"
	// TopicSecrets: secrets were changed.
	TopicSecrets = "secrets"
	// TopicRun: a run changed; the payload is its id.
	TopicRun = "run"
)

// Bus tells the replicas about changes at once, with LISTEN/NOTIFY on one
// connection of its own. Polling stays as it is, so a lost connection only
// makes changes slower to arrive, never lost.
type Bus struct {
	dsn     string
	channel string
	st      *Store

	mu   sync.Mutex
	subs map[string][]func(payload string)
}

func (s *Store) NewBus(dsn, prefix string) *Bus {
	return &Bus{dsn: dsn, channel: prefix + "events", st: s, subs: map[string][]func(string){}}
}

// On calls fn for every message of the topic, from any replica (this one
// too). fn must not block.
func (b *Bus) On(topic string, fn func(payload string)) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.subs[topic] = append(b.subs[topic], fn)
}

// Send tells every replica. A failure is logged: the receivers find the
// change by polling.
func (b *Bus) Send(ctx context.Context, topic, payload string) {
	if b == nil {
		return
	}

	if _, err := b.st.db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, b.channel, topic+":"+payload); err != nil && ctx.Err() == nil {
		slog.Debug("notify replicas", "topic", topic, "error", err)
	}
}

// Run listens until ctx ends, connecting again after a failure.
func (b *Bus) Run(ctx context.Context) {
	wait := time.Second

	for ctx.Err() == nil {
		err := b.listen(ctx)
		if ctx.Err() != nil {
			return
		}

		slog.Warn("listening for changes stopped, polling until it is back", "error", err, "retry_in", wait)

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		wait = min(wait*2, time.Minute)
	}
}

func (b *Bus) listen(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, b.dsn)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{b.channel}.Sanitize()); err != nil {
		return err
	}

	// Anything missed while not listening is picked up now.
	b.deliver(TopicWake, "")
	b.deliver(TopicSettings, "")
	b.deliver(TopicSecrets, "")

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}

		topic, payload, _ := strings.Cut(n.Payload, ":")
		b.deliver(topic, payload)
	}
}

func (b *Bus) deliver(topic, payload string) {
	b.mu.Lock()
	fns := append([]func(string){}, b.subs[topic]...)
	b.mu.Unlock()

	for _, fn := range fns {
		fn(payload)
	}
}

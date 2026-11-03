package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Channel is where notifications go.
type Channel struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
	CreatedBy string          `json:"created_by"`
	UpdatedAt time.Time       `json:"updated_at"`
	UpdatedBy string          `json:"updated_by"`
}

const channelColumns = `id, name, type, config, created_at, created_by, updated_at, updated_by`

func scanChannel(row scanner) (*Channel, error) {
	var (
		c   Channel
		cfg []byte
	)

	if err := row.Scan(&c.ID, &c.Name, &c.Type, &cfg, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy); err != nil {
		return nil, err
	}

	c.Config = cfg
	c.CreatedAt = c.CreatedAt.UTC()
	c.UpdatedAt = c.UpdatedAt.UTC()

	return &c, nil
}

func (s *Store) ListChannels(ctx context.Context) ([]*Channel, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+channelColumns+` FROM {channels} ORDER BY name`))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []*Channel{}

	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, c)
	}

	return out, rows.Err()
}

func (s *Store) GetChannel(ctx context.Context, id string) (*Channel, error) {
	c, err := scanChannel(s.db.QueryRowContext(ctx, s.q(`SELECT `+channelColumns+` FROM {channels} WHERE id = $1`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}

	return c, err
}

func (s *Store) PutChannel(ctx context.Context, c *Channel, create bool) error {
	var (
		res sql.Result
		err error
	)

	if create {
		res, err = s.db.ExecContext(ctx, s.q(`INSERT INTO {channels} (id, name, type, config, created_at, created_by, updated_at, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $5, $6)`), c.ID, c.Name, c.Type, []byte(c.Config), c.CreatedAt, c.CreatedBy)
	} else {
		res, err = s.db.ExecContext(ctx, s.q(`UPDATE {channels} SET name = $2, type = $3, config = $4, updated_at = $5, updated_by = $6 WHERE id = $1`),
			c.ID, c.Name, c.Type, []byte(c.Config), c.UpdatedAt, c.UpdatedBy)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Conflict("a channel named %q already exists", c.Name)
	}

	if err != nil {
		return err
	}

	return affected(res)
}

// DeleteChannel removes a channel, its pending notifications and its place
// in the jobs' notify lists.
func (s *Store) DeleteChannel(ctx context.Context, id string) error {
	return s.inTxRetry(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM {channels} WHERE id = $1`), id)
		if err != nil {
			return err
		}

		if err := affected(res); err != nil {
			return err
		}

		// The path is a parameter: "{channels}" in the query would be taken
		// for the table name.
		_, err = tx.ExecContext(ctx, s.q(`UPDATE {jobs} SET notify = jsonb_set(notify, $2::text[], (notify->'channels') - $1::text)
			WHERE notify->'channels' ? $1`), id, []string{"channels"})

		return err
	})
}

// Notification is one message to send, or sent.
type Notification struct {
	ID          string          `json:"id"`
	ChannelID   string          `json:"channel_id"`
	ChannelName string          `json:"channel_name"`
	JobID       string          `json:"job_id"`
	RunID       string          `json:"run_id"`
	Event       string          `json:"event"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	NextAt      time.Time       `json:"next_at"`
	CreatedAt   time.Time       `json:"created_at"`
	SentAt      *time.Time      `json:"sent_at"`
	Error       string          `json:"error"`
}

const (
	NotificationPending = "pending"
	NotificationSent    = "sent"
	NotificationFailed  = "failed"
)

const notificationColumns = `n.id, n.channel_id, c.name, n.job_id, n.run_id, n.event, n.payload, n.status, n.attempts,
	n.next_at, n.created_at, n.sent_at, n.error`

func scanNotification(row scanner) (*Notification, error) {
	var (
		n       Notification
		payload []byte
		sent    sql.NullTime
	)

	if err := row.Scan(&n.ID, &n.ChannelID, &n.ChannelName, &n.JobID, &n.RunID, &n.Event, &payload, &n.Status, &n.Attempts,
		&n.NextAt, &n.CreatedAt, &sent, &n.Error); err != nil {
		return nil, err
	}

	n.Payload = payload
	n.NextAt = n.NextAt.UTC()
	n.CreatedAt = n.CreatedAt.UTC()
	n.SentAt = timePtr(sent)

	return &n, nil
}

// ListNotifications lists the newest notifications, of one job when jobID
// is set.
func (s *Store) ListNotifications(ctx context.Context, jobID string, limit int) ([]*Notification, error) {
	q := `SELECT ` + notificationColumns + ` FROM {notifications} n JOIN {channels} c ON c.id = n.channel_id`

	args := []any{limit}
	if jobID != "" {
		q += ` WHERE n.job_id = $2`

		args = append(args, jobID)
	}

	rows, err := s.db.QueryContext(ctx, s.q(q+` ORDER BY n.created_at DESC, n.id DESC LIMIT $1`), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []*Notification{}

	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, n)
	}

	return out, rows.Err()
}

// Outgoing is a notification taken for sending, with its channel.
type Outgoing struct {
	Notification *Notification
	Channel      *Channel
}

// lease is how long a taken notification is left to its replica before
// another one may send it again.
const lease = 2 * time.Minute

// TakeNotifications takes up to limit due notifications for sending: their
// next try is moved past the lease, so no other replica sends them meanwhile.
func (s *Store) TakeNotifications(ctx context.Context, now time.Time, limit int) ([]Outgoing, error) {
	var out []Outgoing

	err := s.inTxRetry(ctx, func(tx *sql.Tx) error {
		out = nil

		rows, err := tx.QueryContext(ctx, s.q(`UPDATE {notifications} SET next_at = $3, attempts = attempts + 1
			WHERE id IN (SELECT id FROM {notifications} WHERE status = $1 AND next_at <= $2 ORDER BY next_at LIMIT $4 FOR UPDATE SKIP LOCKED)
			RETURNING id`), NotificationPending, now, now.Add(lease), limit)
		if err != nil {
			return err
		}

		var ids []string

		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()

				return err
			}

			ids = append(ids, id)
		}

		_ = rows.Close()

		if err := rows.Err(); err != nil || len(ids) == 0 {
			return err
		}

		rows, err = tx.QueryContext(ctx, s.q(`SELECT `+notificationColumns+`, c.id, c.name, c.type, c.config, c.created_at, c.created_by, c.updated_at, c.updated_by
			FROM {notifications} n JOIN {channels} c ON c.id = n.channel_id WHERE n.id = ANY($1) ORDER BY n.created_at`), ids)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var (
				n       Notification
				payload []byte
				sent    sql.NullTime
				c       Channel
				cfg     []byte
			)

			if err := rows.Scan(&n.ID, &n.ChannelID, &n.ChannelName, &n.JobID, &n.RunID, &n.Event, &payload, &n.Status, &n.Attempts,
				&n.NextAt, &n.CreatedAt, &sent, &n.Error,
				&c.ID, &c.Name, &c.Type, &cfg, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy); err != nil {
				return err
			}

			n.Payload, c.Config = payload, cfg
			out = append(out, Outgoing{Notification: &n, Channel: &c})
		}

		return rows.Err()
	})

	return out, err
}

// SentNotification records a notification as sent.
func (s *Store) SentNotification(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE {notifications} SET status = $2, sent_at = $3, error = '' WHERE id = $1`),
		id, NotificationSent, now)

	return err
}

// FailedNotification records a failed try: it is tried again at next, or
// given up when next is zero.
func (s *Store) FailedNotification(ctx context.Context, id, errText string, next time.Time) error {
	if next.IsZero() {
		_, err := s.db.ExecContext(ctx, s.q(`UPDATE {notifications} SET status = $2, error = $3 WHERE id = $1`), id, NotificationFailed, errText)

		return err
	}

	_, err := s.db.ExecContext(ctx, s.q(`UPDATE {notifications} SET error = $2, next_at = $3 WHERE id = $1`), id, errText, next)

	return err
}

// AddNotification queues a notification outside a run's result, such as a
// test message from the settings.
func (s *Store) AddNotification(ctx context.Context, channelID string, ev Event, now time.Time) (string, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}

	id := NewID()

	_, err = s.db.ExecContext(ctx, s.q(`INSERT INTO {notifications} (id, channel_id, job_id, run_id, event, payload, next_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`), id, channelID, ev.JobID, ev.RunID, ev.Event, payload, now)

	return id, err
}

// PruneNotifications deletes notifications created before the given time.
func (s *Store) PruneNotifications(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM {notifications} WHERE created_at < $1 AND status <> $2`), before, NotificationPending)
	if err != nil {
		return 0, err
	}

	return res.RowsAffected()
}

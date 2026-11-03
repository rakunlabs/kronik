// Package store keeps jobs, their runs and the replicas in PostgreSQL.
//
// Every replica works on the same tables. Due jobs and queued runs are
// claimed with SELECT ... FOR UPDATE SKIP LOCKED, so each run is taken by
// exactly one replica.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/oklog/ulid/v2"
	"github.com/rakunlabs/muz"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// conflictError is an ErrConflict with a message for the user.
type conflictError struct{ msg string }

func (e conflictError) Error() string        { return e.msg }
func (e conflictError) Is(target error) bool { return target == ErrConflict }

// Conflict is an ErrConflict with a message for the user.
func Conflict(format string, a ...any) error {
	return conflictError{msg: fmt.Sprintf(format, a...)}
}

type Store struct {
	db *sql.DB
	// t holds the prefixed table names.
	t tables
	// bus, when set, tells replicas about changed runs and new work.
	bus *Bus
}

// SetBus makes the store announce changed runs and queued work on bus.
func (s *Store) SetBus(b *Bus) { s.bus = b }

// announce tells the replicas the runs changed, and wakes their schedulers
// when wake is set. It never blocks the caller for long.
func (s *Store) announce(wake bool, runIDs ...string) {
	if s.bus == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, id := range runIDs {
		s.bus.Send(ctx, TopicRun, id)
	}

	if wake {
		s.bus.Send(ctx, TopicWake, "")
	}
}

type tables struct {
	jobs, runs, nodes, audit, settings, secretKeys, secrets, channels, notifications string
}

// Config is how to reach the database.
type Config struct {
	// DSN is the PostgreSQL connection string.
	DSN string `cfg:"dsn" log:"-"`
	// TablePrefix starts every table name, so kronik can share a schema.
	TablePrefix  string `cfg:"table_prefix" default:"kronik_"`
	MaxOpenConns int    `cfg:"max_open_conns" default:"10"`
}

func New(ctx context.Context, cfg Config) (*Store, error) {
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open postgres; %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxOpenConns)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("ping postgres; %w", err)
	}

	s := &Store{
		db: db,
		t: tables{
			jobs:     cfg.TablePrefix + "jobs",
			runs:     cfg.TablePrefix + "runs",
			nodes:    cfg.TablePrefix + "nodes",
			audit:    cfg.TablePrefix + "audit",
			settings: cfg.TablePrefix + "settings",

			secretKeys:    cfg.TablePrefix + "secret_keys",
			secrets:       cfg.TablePrefix + "secrets",
			channels:      cfg.TablePrefix + "channels",
			notifications: cfg.TablePrefix + "notifications",
		},
	}

	if err := s.migrate(ctx, cfg.TablePrefix); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("migrate; %w", err)
	}

	return s, nil
}

func (s *Store) migrate(ctx context.Context, prefix string) error {
	m := muz.Migrate{
		Path:      "migrations",
		FS:        migrationsFS,
		Extension: ".sql",
		Values:    map[string]string{"TABLE_PREFIX": prefix},
	}

	driver := muz.NewPostgresDriver(s.db, prefix+"migrations", slog.Default())
	// Replicas starting together migrate one after another.
	driver.LockKey = "kronik:" + prefix + "migrations"

	return m.Migrate(ctx, driver)
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) Close() error {
	return s.db.Close()
}

// q replaces {jobs}, {runs} and the other table placeholders with the
// prefixed table names.
func (s *Store) q(query string) string {
	return strings.NewReplacer(
		"{jobs}", s.t.jobs,
		"{runs}", s.t.runs,
		"{nodes}", s.t.nodes,
		"{audit}", s.t.audit,
		"{settings}", s.t.settings,
		"{secret_keys}", s.t.secretKeys,
		"{secrets}", s.t.secrets,
		"{channels}", s.t.channels,
		"{notifications}", s.t.notifications,
	).Replace(query)
}

// Now is the current time as PostgreSQL stores it (microseconds, UTC), so a
// value written and read back compares equal.
func Now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

func NewID() string {
	return strings.ToLower(ulid.Make().String())
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	if err := fn(tx); err != nil {
		_ = tx.Rollback()

		return err
	}

	return tx.Commit()
}

// Exclusive runs fn when no other replica runs the task of the same name,
// and skips it otherwise. The lock is a transaction-level advisory lock, so
// it is released when fn returns or the connection drops.
func (s *Store) Exclusive(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var got bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtext($1))`, s.t.jobs+":"+name).Scan(&got); err != nil {
		return err
	}

	if !got {
		return nil
	}

	if err := fn(ctx); err != nil {
		return err
	}

	return tx.Commit()
}

// inTxRetry is inTx, run again when PostgreSQL aborts the transaction for
// a deadlock or a serialization failure. fn must be safe to run again.
func (s *Store) inTxRetry(ctx context.Context, fn func(tx *sql.Tx) error) error {
	var err error

	for attempt := range 4 {
		if err = s.inTx(ctx, fn); err == nil || !isRetryable(err) {
			return err
		}

		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}

	return err
}

func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40P01" || pgErr.Code == "40001"
	}

	return false
}

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}

	return *t
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}

	v := t.Time.UTC()

	return &v
}

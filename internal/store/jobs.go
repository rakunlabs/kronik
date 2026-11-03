package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type Job struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Tags        []string        `json:"tags"`
	Schedule    string          `json:"schedule"`
	Timezone    string          `json:"timezone"`
	Enabled     bool            `json:"enabled"`
	Executor    string          `json:"executor"`
	Config      json.RawMessage `json:"config"`
	// Concurrency is what happens when a run is due while another one of
	// the same job is still queued or running: allow, forbid or replace.
	Concurrency string `json:"concurrency"`
	// CatchUp creates a run for every time missed while no replica was
	// running; otherwise only the latest missed time runs.
	CatchUp           bool       `json:"catch_up"`
	TimeoutSeconds    int        `json:"timeout_seconds"`
	Retries           int        `json:"retries"`
	RetryDelaySeconds int        `json:"retry_delay_seconds"`
	NextRunAt         *time.Time `json:"next_run_at"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	// CreatedBy and UpdatedBy are the users from the proxy's header.
	CreatedBy string `json:"created_by"`
	UpdatedBy string `json:"updated_by"`

	// JitterSeconds delays each scheduled run by a random 0..n seconds, so
	// jobs due at the same minute do not all start at once.
	JitterSeconds int `json:"jitter_seconds"`
	// AlertAfterSeconds raises a "did not run" alert when no run of the
	// job succeeded for this long; 0 is no alert.
	AlertAfterSeconds int        `json:"alert_after_seconds"`
	AlertedAt         *time.Time `json:"alerted_at"`
	// Health is "", "ok" or "failing": what the last final run left.
	Health string `json:"health"`
	Notify Notify `json:"notify"`
	// HookHash is the SHA-256 of the webhook token, "" without one.
	HookHash string `json:"-"`
	HasHook  bool   `json:"has_hook"`
	// AfterJobs runs this job when a run of one of them ends as AfterOn
	// says: succeeded, failed or finished.
	AfterJobs []string `json:"after_jobs"`
	AfterOn   string   `json:"after_on"`

	// LastRun is filled in lists, nil when the job never ran.
	LastRun *RunSummary `json:"last_run,omitempty"`
}

// Notify is where a job's notifications go and on what.
type Notify struct {
	Channels []string `json:"channels"`
	// On lists failure, recovery, no_run and success.
	On []string `json:"on"`
}

const (
	NotifyFailure  = "failure"
	NotifyRecovery = "recovery"
	NotifyNoRun    = "no_run"
	NotifySuccess  = "success"

	HealthOK      = "ok"
	HealthFailing = "failing"

	AfterSucceeded = "succeeded"
	AfterFailed    = "failed"
	AfterFinished  = "finished"
)

type RunSummary struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	// Result is the return value of a script run (JSON), "" otherwise.
	Result string `json:"result"`
}

const jobColumns = `j.id, j.name, j.description, j.tags, j.schedule, j.timezone, j.enabled, j.executor, j.config,
	j.concurrency, j.catch_up, j.timeout_seconds, j.retries, j.retry_delay_seconds, j.next_run_at, j.created_at, j.updated_at, j.created_by, j.updated_by,
	j.jitter_seconds, j.alert_after_seconds, j.alerted_at, j.health, j.notify, j.hook_hash, j.after_jobs, j.after_on`

type scanner interface {
	Scan(dest ...any) error
}

// jobScan holds the scan targets of jobColumns, for scans that read more
// columns in the same row.
type jobScan struct {
	j                        Job
	tags, cfg, notify, after []byte
	next, alerted            sql.NullTime
}

func (s *jobScan) dest() []any {
	j := &s.j

	return []any{
		&j.ID, &j.Name, &j.Description, &s.tags, &j.Schedule, &j.Timezone, &j.Enabled, &j.Executor, &s.cfg,
		&j.Concurrency, &j.CatchUp, &j.TimeoutSeconds, &j.Retries, &j.RetryDelaySeconds, &s.next, &j.CreatedAt, &j.UpdatedAt,
		&j.CreatedBy, &j.UpdatedBy,
		&j.JitterSeconds, &j.AlertAfterSeconds, &s.alerted, &j.Health, &s.notify, &j.HookHash, &s.after, &j.AfterOn,
	}
}

func (s *jobScan) job() (*Job, error) {
	j := s.j

	if err := json.Unmarshal(s.tags, &j.Tags); err != nil {
		return nil, fmt.Errorf("decode tags of job %s; %w", j.ID, err)
	}

	if j.Tags == nil {
		j.Tags = []string{}
	}

	if err := json.Unmarshal(s.notify, &j.Notify); err != nil {
		return nil, fmt.Errorf("decode notify of job %s; %w", j.ID, err)
	}

	if err := json.Unmarshal(s.after, &j.AfterJobs); err != nil {
		return nil, fmt.Errorf("decode after_jobs of job %s; %w", j.ID, err)
	}

	j.Notify = j.Notify.normalized()
	if j.AfterJobs == nil {
		j.AfterJobs = []string{}
	}

	j.HasHook = j.HookHash != ""
	j.Config = json.RawMessage(s.cfg)
	j.NextRunAt = timePtr(s.next)
	j.AlertedAt = timePtr(s.alerted)
	j.CreatedAt = j.CreatedAt.UTC()
	j.UpdatedAt = j.UpdatedAt.UTC()

	return &j, nil
}

func scanJob(row scanner, extra ...any) (*Job, error) {
	var s jobScan

	if err := row.Scan(append(s.dest(), extra...)...); err != nil {
		return nil, err
	}

	return s.job()
}

type JobFilter struct {
	// Search matches the name, description or a tag, case-insensitively.
	Search string
	Tag    string
}

func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]*Job, error) {
	var (
		where []string
		args  []any
	)

	if f.Search != "" {
		args = append(args, "%"+escapeLike(f.Search)+"%")
		where = append(where, fmt.Sprintf(
			"(j.name ILIKE $%[1]d OR j.description ILIKE $%[1]d OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(j.tags) t WHERE t ILIKE $%[1]d))",
			len(args)))
	}

	if f.Tag != "" {
		args = append(args, f.Tag)
		where = append(where, fmt.Sprintf("j.tags ? $%d", len(args)))
	}

	query := `SELECT ` + jobColumns + `, r.id, r.status, r.started_at, r.finished_at, r.result
		FROM {jobs} j
		LEFT JOIN LATERAL (
			SELECT id, status, started_at, finished_at, result FROM {runs}
			WHERE job_id = j.id AND status <> 'queued'
			ORDER BY id DESC LIMIT 1
		) r ON TRUE`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	query += " ORDER BY j.name"

	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	jobs := []*Job{}

	for rows.Next() {
		var (
			runID, runStatus, result sql.NullString
			started, finished        sql.NullTime
		)

		j, err := scanJob(rows, &runID, &runStatus, &started, &finished, &result)
		if err != nil {
			return nil, err
		}

		if runID.Valid {
			j.LastRun = &RunSummary{
				ID:         runID.String,
				Status:     runStatus.String,
				StartedAt:  timePtr(started),
				FinishedAt: timePtr(finished),
				Result:     result.String,
			}
		}

		jobs = append(jobs, j)
	}

	return jobs, rows.Err()
}

func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, s.q(`SELECT `+jobColumns+` FROM {jobs} j WHERE j.id = $1`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}

	return j, err
}

func (n Notify) normalized() Notify {
	if n.Channels == nil {
		n.Channels = []string{}
	}

	if n.On == nil {
		n.On = []string{}
	}

	return n
}

// Wants reports whether the job asked for notifications on event.
func (n Notify) Wants(event string) bool {
	return len(n.Channels) > 0 && slices.Contains(n.On, event)
}

// jobJSON is the JSON columns of a job.
func jobJSON(j *Job) (tags, notify, after []byte, err error) {
	if tags, err = json.Marshal(j.Tags); err != nil {
		return
	}

	if notify, err = json.Marshal(j.Notify.normalized()); err != nil {
		return
	}

	if j.AfterJobs == nil {
		j.AfterJobs = []string{}
	}

	after, err = json.Marshal(j.AfterJobs)

	return
}

func (s *Store) CreateJob(ctx context.Context, j *Job) error {
	tags, notify, after, err := jobJSON(j)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, s.q(`INSERT INTO {jobs} (id, name, description, tags, schedule, timezone, enabled,
		executor, config, concurrency, catch_up, timeout_seconds, retries, retry_delay_seconds, next_run_at, created_at, updated_at,
		created_by, updated_by, jitter_seconds, alert_after_seconds, notify, after_jobs, after_on, hook_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $18, $19, $20, $21, $22, $23, $24)`),
		j.ID, j.Name, j.Description, tags, j.Schedule, j.Timezone, j.Enabled,
		j.Executor, []byte(j.Config), j.Concurrency, j.CatchUp, j.TimeoutSeconds, j.Retries, j.RetryDelaySeconds,
		nullTime(j.NextRunAt), j.CreatedAt, j.UpdatedAt, j.CreatedBy,
		j.JitterSeconds, j.AlertAfterSeconds, notify, after, j.AfterOn, j.HookHash)

	return uniqueErr(err, j.Name)
}

// UpdateJob replaces the job if it is still at version prev (its updated_at).
func (s *Store) UpdateJob(ctx context.Context, j *Job, prev time.Time) error {
	tags, notify, after, err := jobJSON(j)
	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx, s.q(`UPDATE {jobs} SET name = $2, description = $3, tags = $4, schedule = $5,
		timezone = $6, enabled = $7, executor = $8, config = $9, concurrency = $10, catch_up = $11,
		timeout_seconds = $12, retries = $13, retry_delay_seconds = $14, next_run_at = $15, updated_at = $16, updated_by = $18,
		jitter_seconds = $19, alert_after_seconds = $20, notify = $21, after_jobs = $22, after_on = $23, alerted_at = NULL
		WHERE id = $1 AND updated_at = $17`),
		j.ID, j.Name, j.Description, tags, j.Schedule, j.Timezone, j.Enabled, j.Executor, []byte(j.Config),
		j.Concurrency, j.CatchUp, j.TimeoutSeconds, j.Retries, j.RetryDelaySeconds, nullTime(j.NextRunAt), j.UpdatedAt, prev, j.UpdatedBy,
		j.JitterSeconds, j.AlertAfterSeconds, notify, after, j.AfterOn)
	if err != nil {
		return uniqueErr(err, j.Name)
	}

	if err := affected(res); err != nil {
		if _, getErr := s.GetJob(ctx, j.ID); getErr != nil {
			return getErr
		}

		return Conflict("the job was changed at the same time; reload it and apply the change again")
	}

	return nil
}

// SetEnabled switches a job on or off with its new next run time.
func (s *Store) SetEnabled(ctx context.Context, id string, enabled bool, next *time.Time, now time.Time, by string) error {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE {jobs} SET enabled = $2, next_run_at = $3, updated_at = $4, updated_by = $5, alerted_at = NULL WHERE id = $1`),
		id, enabled, nullTime(next), now, by)
	if err != nil {
		return err
	}

	return affected(res)
}

// DeleteJob removes the job and its runs. Runs still going are canceled by
// their replica when it finds them gone.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	return s.inTxRetry(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM {jobs} WHERE id = $1`), id)
		if err != nil {
			return err
		}

		return affected(res)
	})
}

func (s *Store) Tags(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT DISTINCT jsonb_array_elements_text(tags) AS tag FROM {jobs} ORDER BY tag`))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	tags := []string{}

	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}

		tags = append(tags, t)
	}

	return tags, rows.Err()
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if n == 0 {
		return ErrNotFound
	}

	return nil
}

func uniqueErr(err error, name string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Conflict("a job named %q already exists", name)
	}

	return err
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// SetHook stores the hash of a job's new webhook token ("" removes it).
func (s *Store) SetHook(ctx context.Context, id, hash string) error {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE {jobs} SET hook_hash = $2 WHERE id = $1`), id, hash)
	if err != nil {
		return err
	}

	return affected(res)
}

// JobByHook finds the job whose webhook token has this hash.
func (s *Store) JobByHook(ctx context.Context, hash string) (*Job, error) {
	if hash == "" {
		return nil, ErrNotFound
	}

	j, err := scanJob(s.db.QueryRowContext(ctx, s.q(`SELECT `+jobColumns+` FROM {jobs} j WHERE j.hook_hash = $1`), hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}

	return j, err
}

// SetEnabledMany pauses or resumes jobs; next gives the next run of a job
// being resumed. It returns the jobs changed.
func (s *Store) SetEnabledMany(ctx context.Context, ids []string, enabled bool, next func(*Job) *time.Time, now time.Time, by string) ([]*Job, error) {
	var changed []*Job

	err := s.inTxRetry(ctx, func(tx *sql.Tx) error {
		changed = nil

		rows, err := tx.QueryContext(ctx, s.q(`SELECT `+jobColumns+` FROM {jobs} j WHERE j.id = ANY($1) AND j.enabled <> $2 ORDER BY j.id FOR UPDATE`),
			ids, enabled)
		if err != nil {
			return err
		}

		var jobs []*Job

		for rows.Next() {
			j, err := scanJob(rows)
			if err != nil {
				_ = rows.Close()

				return err
			}

			jobs = append(jobs, j)
		}

		_ = rows.Close()

		if err := rows.Err(); err != nil {
			return err
		}

		for _, j := range jobs {
			var n *time.Time
			if enabled {
				n = next(j)
			}

			if _, err := tx.ExecContext(ctx, s.q(`UPDATE {jobs} SET enabled = $2, next_run_at = $3, updated_at = $4, updated_by = $5, alerted_at = NULL WHERE id = $1`),
				j.ID, enabled, nullTime(n), now, by); err != nil {
				return err
			}

			changed = append(changed, j)
		}

		return nil
	})

	return changed, err
}

// JobNames maps job ids to names, for the jobs that exist.
func (s *Store) JobNames(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, name FROM {jobs} WHERE id = ANY($1)`), ids)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}

	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}

		out[id] = name
	}

	return out, rows.Err()
}

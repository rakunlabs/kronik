package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusTimedOut  = "timed_out"
	StatusCanceled  = "canceled"
	StatusSkipped   = "skipped"
	// StatusLost is a run whose replica stopped while running it.
	StatusLost = "lost"

	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerRetry    = "retry"

	ConcurrencyAllow   = "allow"
	ConcurrencyForbid  = "forbid"
	ConcurrencyReplace = "replace"
)

type Run struct {
	ID              string     `json:"id"`
	JobID           string     `json:"job_id"`
	JobName         string     `json:"job_name"`
	Trigger         string     `json:"trigger"`
	Status          string     `json:"status"`
	Attempt         int        `json:"attempt"`
	ScheduledAt     time.Time  `json:"scheduled_at"`
	NotBefore       time.Time  `json:"not_before"`
	Node            string     `json:"node"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	ResultCode      *int       `json:"result_code"`
	Output          string     `json:"output,omitempty"`
	OutputTruncated bool       `json:"output_truncated"`
	Error           string     `json:"error"`
	CancelRequested bool       `json:"cancel_requested"`
	// Result is a script's return value (JSON), "" for other executors.
	Result      string `json:"result"`
	TriggeredBy string `json:"triggered_by"`
	CanceledBy  string `json:"canceled_by"`
}

func (r *Run) Finished() bool {
	return r.Status != StatusQueued && r.Status != StatusRunning
}

const runColumns = `r.id, r.job_id, j.name, r.trigger, r.status, r.attempt, r.scheduled_at, r.not_before, r.node,
	r.created_at, r.started_at, r.finished_at, r.result_code, r.output_truncated, r.error, r.cancel_requested,
	r.result, r.triggered_by, r.canceled_by`

func scanRun(row scanner, extra ...any) (*Run, error) {
	var (
		r                 Run
		started, finished sql.NullTime
		code              sql.NullInt64
	)

	dest := []any{
		&r.ID, &r.JobID, &r.JobName, &r.Trigger, &r.Status, &r.Attempt, &r.ScheduledAt, &r.NotBefore, &r.Node,
		&r.CreatedAt, &started, &finished, &code, &r.OutputTruncated, &r.Error, &r.CancelRequested,
		&r.Result, &r.TriggeredBy, &r.CanceledBy,
	}

	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}

	r.ScheduledAt = r.ScheduledAt.UTC()
	r.NotBefore = r.NotBefore.UTC()
	r.CreatedAt = r.CreatedAt.UTC()
	r.StartedAt = timePtr(started)
	r.FinishedAt = timePtr(finished)

	if code.Valid {
		c := int(code.Int64)
		r.ResultCode = &c
	}

	return &r, nil
}

type RunFilter struct {
	JobID  string
	Status []string
	// Search matches the job's name, the run's error and who started it,
	// case-insensitively.
	Search string
	// TriggeredBy is the user who started the run by hand.
	TriggeredBy string
	// JobsBy keeps runs of jobs this user created or last changed, and runs
	// they started: "my runs".
	JobsBy string
	// From and To bound the scheduled time (To is exclusive).
	From, To time.Time
	// Before pages back: only runs with a smaller id, which are older.
	Before string
	Limit  int
}

func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	var (
		where []string
		args  []any
	)

	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}

	if f.JobID != "" {
		add("r.job_id = $%d", f.JobID)
	}

	if len(f.Status) > 0 {
		add("r.status = ANY($%d)", f.Status)
	}

	if f.Search != "" {
		args = append(args, "%"+escapeLike(f.Search)+"%", strings.ToLower(strings.TrimSpace(f.Search)))
		n := len(args)
		where = append(where, fmt.Sprintf("(j.name ILIKE $%[1]d OR r.error ILIKE $%[1]d OR r.triggered_by ILIKE $%[1]d OR r.id = $%[2]d)", n-1, n))
	}

	if f.TriggeredBy != "" {
		add("r.triggered_by = $%d", f.TriggeredBy)
	}

	if f.JobsBy != "" {
		add("(r.triggered_by = $%[1]d OR j.created_by = $%[1]d OR j.updated_by = $%[1]d)", f.JobsBy)
	}

	if !f.From.IsZero() {
		add("r.scheduled_at >= $%d", f.From)
	}

	if !f.To.IsZero() {
		add("r.scheduled_at < $%d", f.To)
	}

	if f.Before != "" {
		add("r.id < $%d", f.Before)
	}

	query := `SELECT ` + runColumns + ` FROM {runs} r JOIN {jobs} j ON j.id = r.job_id`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	args = append(args, f.Limit)
	query += fmt.Sprintf(" ORDER BY r.id DESC LIMIT $%d", len(args))

	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	runs := []*Run{}

	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}

		runs = append(runs, r)
	}

	return runs, rows.Err()
}

// GetRun returns the run with its output.
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	var output string

	r, err := scanRun(s.db.QueryRowContext(ctx,
		s.q(`SELECT `+runColumns+`, r.output FROM {runs} r JOIN {jobs} j ON j.id = r.job_id WHERE r.id = $1`), id),
		&output)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, err
	}

	r.Output = output

	return r, nil
}

// TriggerRun queues a manual run of the job, to start right away.
// ErrActive is a manual run of a "forbid" job refused while one is active.
var ErrActive = Conflict("a run of this job is still queued or running, and the job does not allow two at once")

func (s *Store) TriggerRun(ctx context.Context, jobID string, now time.Time, by string) (string, error) {
	id := NewID()

	err := s.inTxRetry(ctx, func(tx *sql.Tx) error {
		var concurrency string

		err := tx.QueryRowContext(ctx, s.q(`SELECT concurrency FROM {jobs} WHERE id = $1 FOR UPDATE`), jobID).Scan(&concurrency)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		if err != nil {
			return err
		}

		switch concurrency {
		case ConcurrencyForbid:
			var active bool
			if err := tx.QueryRowContext(ctx, s.q(`SELECT EXISTS (SELECT 1 FROM {runs} WHERE job_id = $1 AND status IN ($2, $3))`),
				jobID, StatusQueued, StatusRunning).Scan(&active); err != nil {
				return err
			}

			if active {
				return ErrActive
			}
		case ConcurrencyReplace:
			if err := s.replaceActive(ctx, tx, jobID, now); err != nil {
				return err
			}
		}

		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO {runs} (id, job_id, trigger, status, scheduled_at, not_before, created_at, triggered_by)
			VALUES ($1, $2, $3, $4, $5, $5, $5, $6)`), id, jobID, TriggerManual, StatusQueued, now, by)

		return err
	})
	if err != nil {
		return "", err
	}

	return id, nil
}

// CancelRun cancels a queued run at once and asks the replica running a
// running one to stop it.
func (s *Store) CancelRun(ctx context.Context, id string, now time.Time, by string) error {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE {runs} SET
			status = CASE WHEN status = $2 THEN $3 ELSE status END,
			finished_at = CASE WHEN status = $2 THEN $4 ELSE finished_at END,
			error = CASE WHEN status = $2 THEN 'canceled before start' ELSE error END,
			cancel_requested = TRUE,
			canceled_by = COALESCE(NULLIF(canceled_by, ''), $6)
		WHERE id = $1 AND status IN ($2, $5)`),
		id, StatusQueued, StatusCanceled, now, StatusRunning, by)
	if err != nil {
		return err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if n == 0 {
		if _, err := s.GetRun(ctx, id); err != nil {
			return err
		}

		return Conflict("the run has already finished")
	}

	return nil
}

// Plan is what a due job gets: run times to create and its next run time.
type Plan struct {
	Times []time.Time
	Next  *time.Time
}

// ScheduleDue takes up to limit due jobs that no other replica is handling,
// asks plan for their run times and creates the runs, applying each job's
// concurrency policy. It returns the number of jobs handled. A job that
// cannot be scheduled is retried a minute later; it does not hold up the
// others of the batch.
func (s *Store) ScheduleDue(ctx context.Context, now time.Time, limit int, plan func(*Job) (Plan, error)) (int, error) {
	n := 0

	err := s.inTxRetry(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, s.q(`SELECT `+jobColumns+` FROM {jobs} j
			WHERE j.enabled AND j.next_run_at <= $1
			ORDER BY j.next_run_at LIMIT $2 FOR UPDATE SKIP LOCKED`), now, limit)
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
			if err := s.scheduleOne(ctx, tx, j, plan, now); err != nil {
				return err
			}
		}

		n = len(jobs)

		return nil
	})

	return n, err
}

// retryLater is how far a job that could not be scheduled is moved.
const retryLater = time.Minute

func (s *Store) scheduleOne(ctx context.Context, tx *sql.Tx, j *Job, plan func(*Job) (Plan, error), now time.Time) error {
	p, err := plan(j)
	if err == nil {
		if _, err = tx.ExecContext(ctx, "SAVEPOINT schedule_job"); err != nil {
			return err
		}

		if err = s.scheduleJob(ctx, tx, j, p, now); err == nil {
			_, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT schedule_job")

			return err
		}

		if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT schedule_job"); rbErr != nil {
			return rbErr
		}

		// A deadlock is retried with the whole batch.
		if isRetryable(err) {
			return err
		}
	}

	slog.Error("job not scheduled, trying again later", "job", j.Name, "job_id", j.ID, "error", err, "retry_in", retryLater)

	_, err = tx.ExecContext(ctx, s.q(`UPDATE {jobs} SET next_run_at = $2 WHERE id = $1`), j.ID, now.Add(retryLater))

	return err
}

func (s *Store) scheduleJob(ctx context.Context, tx *sql.Tx, j *Job, p Plan, now time.Time) error {
	if _, err := tx.ExecContext(ctx, s.q(`UPDATE {jobs} SET next_run_at = $2 WHERE id = $1`), j.ID, nullTime(p.Next)); err != nil {
		return err
	}

	for _, t := range p.Times {
		status, errText := StatusQueued, ""

		switch j.Concurrency {
		case ConcurrencyForbid:
			var active bool
			if err := tx.QueryRowContext(ctx, s.q(`SELECT EXISTS (SELECT 1 FROM {runs} WHERE job_id = $1 AND status IN ($2, $3))`),
				j.ID, StatusQueued, StatusRunning).Scan(&active); err != nil {
				return err
			}

			if active {
				status, errText = StatusSkipped, "previous run still active"
			}
		case ConcurrencyReplace:
			if err := s.replaceActive(ctx, tx, j.ID, now); err != nil {
				return err
			}
		}

		var finished any
		if status == StatusSkipped {
			finished = now
		}

		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO {runs} (id, job_id, trigger, status, scheduled_at, not_before, created_at, finished_at, error)
			VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8)
			ON CONFLICT (job_id, scheduled_at) WHERE trigger = 'schedule' DO NOTHING`),
			NewID(), j.ID, TriggerSchedule, status, t, now, finished, errText); err != nil {
			return err
		}
	}

	return nil
}

// replaceActive cancels the job's queued runs and asks its running ones to stop.
func (s *Store) replaceActive(ctx context.Context, tx *sql.Tx, jobID string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, s.q(`UPDATE {runs} SET status = $3, finished_at = $4, error = 'replaced by a newer run', cancel_requested = TRUE
		WHERE job_id = $1 AND status = $2`), jobID, StatusQueued, StatusCanceled, now); err != nil {
		return err
	}

	_, err := tx.ExecContext(ctx, s.q(`UPDATE {runs} SET cancel_requested = TRUE WHERE job_id = $1 AND status = $2`),
		jobID, StatusRunning)

	return err
}

// Claimed is a run taken by this replica with the job as it was at that moment.
type Claimed struct {
	Run *Run
	Job *Job
}

// ClaimRuns marks up to limit queued runs whose time has come as running on
// node and returns them. Only runs of the given executors are taken, so a
// replica without one leaves its runs to the others.
func (s *Store) ClaimRuns(ctx context.Context, node string, now time.Time, limit int, executors []string) ([]Claimed, error) {
	var claimed []Claimed

	err := s.inTxRetry(ctx, func(tx *sql.Tx) error {
		claimed = nil

		rows, err := tx.QueryContext(ctx, s.q(`UPDATE {runs} SET status = $1, node = $2, started_at = $3
			WHERE id IN (
				SELECT r.id FROM {runs} r JOIN {jobs} j ON j.id = r.job_id
				WHERE r.status = $4 AND r.not_before <= $3 AND j.executor = ANY($6)
				ORDER BY r.not_before, r.id LIMIT $5 FOR UPDATE OF r SKIP LOCKED
			)
			RETURNING id`), StatusRunning, node, now, StatusQueued, limit, executors)
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

		rows, err = tx.QueryContext(ctx, s.q(`SELECT `+runColumns+`, `+jobColumns+`
			FROM {runs} r JOIN {jobs} j ON j.id = r.job_id WHERE r.id = ANY($1) ORDER BY r.not_before, r.id`), ids)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var js jobScan

			r, err := scanRun(rows, js.dest()...)
			if err != nil {
				return err
			}

			j, err := js.job()
			if err != nil {
				return err
			}

			claimed = append(claimed, Claimed{Run: r, Job: j})
		}

		return rows.Err()
	})

	return claimed, err
}

// Result is how a run ended.
type Result struct {
	Status          string
	ResultCode      *int
	Output          string
	OutputTruncated bool
	Error           string
	// Value is a script's return value (JSON).
	Value string
}

// FinishRun records the result of a run this replica ran. A failed or timed
// out run of a job with retries left queues the next attempt. It returns
// false when the run was not running on node anymore (lost, or deleted with
// its job).
func (s *Store) FinishRun(ctx context.Context, id, node string, res Result, now time.Time) (bool, error) {
	var ok bool

	err := s.inTxRetry(ctx, func(tx *sql.Tx) error {
		ok = false

		var (
			jobID       string
			attempt     int
			scheduledAt time.Time
			canceled    bool
		)

		// Lock the job before the run, in the order scheduling and deleting
		// take them, so they cannot deadlock with a finishing run.
		err := tx.QueryRowContext(ctx, s.q(`SELECT j.id FROM {runs} r JOIN {jobs} j ON j.id = r.job_id
			WHERE r.id = $1 FOR KEY SHARE OF j`), id).Scan(&jobID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}

		if err != nil {
			return err
		}

		err = tx.QueryRowContext(ctx, s.q(`UPDATE {runs} SET status = $3, finished_at = $4, result_code = $5,
				output = $6, output_truncated = $7, error = $8, result = $10
			WHERE id = $1 AND node = $2 AND status = $9
			RETURNING job_id, attempt, scheduled_at, cancel_requested`),
			id, node, res.Status, now, res.ResultCode, res.Output, res.OutputTruncated, res.Error, StatusRunning, res.Value).
			Scan(&jobID, &attempt, &scheduledAt, &canceled)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}

		if err != nil {
			return err
		}

		ok = true

		if canceled || (res.Status != StatusFailed && res.Status != StatusTimedOut) {
			return nil
		}

		// Attempt n is followed by n+1 while the job allows n retries or
		// more; a paused job is not retried.
		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO {runs} (id, job_id, trigger, status, attempt, scheduled_at, not_before, created_at)
			SELECT $1, id, $2, $3, $4::integer + 1, $5, $6::timestamptz + make_interval(secs => retry_delay_seconds), $6
			FROM {jobs} WHERE id = $7 AND enabled AND retries >= $4::integer`),
			NewID(), TriggerRetry, StatusQueued, attempt, scheduledAt, now, jobID)

		return err
	})

	return ok, err
}

// RecoverOwn marks this node's running runs that it is not running anymore
// as lost: left by a process that stopped under the same name, or whose
// result could not be recorded. running lists the runs it does run; runs
// started after startedBefore are left alone, as a run claimed a moment ago
// may not be listed yet.
func (s *Store) RecoverOwn(ctx context.Context, node string, running []string, startedBefore, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE {runs} SET status = $1, finished_at = $2,
			error = 'the replica stopped running it (restart, or its result could not be saved)'
		WHERE node = $3 AND status = $4 AND NOT (id = ANY($5)) AND started_at < $6`),
		StatusLost, now, node, StatusRunning, running, startedBefore)
	if err != nil {
		return 0, err
	}

	return res.RowsAffected()
}

// SaveOutput stores the output of a run still going, for the live view.
func (s *Store) SaveOutput(ctx context.Context, id, output string, truncated bool) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE {runs} SET output = $2, output_truncated = $3 WHERE id = $1 AND status = $4`),
		id, output, truncated, StatusRunning)

	return err
}

// StopRequested returns which of the given running runs should stop: the ones
// asked to cancel and the ones that are no longer running here.
func (s *Store) StopRequested(ctx context.Context, node string, ids []string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, status, node, cancel_requested FROM {runs} WHERE id = ANY($1)`), ids)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	stop := make(map[string]string, len(ids))
	for _, id := range ids {
		stop[id] = "the run was deleted"
	}

	for rows.Next() {
		var (
			id, status, runNode string
			cancel              bool
		)

		if err := rows.Scan(&id, &status, &runNode, &cancel); err != nil {
			return nil, err
		}

		switch {
		case status != StatusRunning || runNode != node:
			stop[id] = "the run is no longer running on this replica"
		case cancel:
			stop[id] = "canceled"
		default:
			delete(stop, id)
		}
	}

	return stop, rows.Err()
}

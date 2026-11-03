package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
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
	// TriggerWebhook is a run started through the job's webhook.
	TriggerWebhook = "webhook"
	// TriggerAfter is a run started by the end of a run of another job.
	TriggerAfter = "after"
	// TriggerPing is a result reported from outside through the webhook,
	// with nothing run by kronik.
	TriggerPing = "ping"

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
	// Params are values given to this run by hand, a webhook or the run
	// before it in a chain.
	Params map[string]string `json:"params"`
}

func (r *Run) Finished() bool {
	return r.Status != StatusQueued && r.Status != StatusRunning
}

const runColumns = `r.id, r.job_id, j.name, r.trigger, r.status, r.attempt, r.scheduled_at, r.not_before, r.node,
	r.created_at, r.started_at, r.finished_at, r.result_code, r.output_truncated, r.error, r.cancel_requested,
	r.result, r.triggered_by, r.canceled_by, r.params`

func scanRun(row scanner, extra ...any) (*Run, error) {
	var (
		r                 Run
		started, finished sql.NullTime
		code              sql.NullInt64
		params            []byte
	)

	dest := []any{
		&r.ID, &r.JobID, &r.JobName, &r.Trigger, &r.Status, &r.Attempt, &r.ScheduledAt, &r.NotBefore, &r.Node,
		&r.CreatedAt, &started, &finished, &code, &r.OutputTruncated, &r.Error, &r.CancelRequested,
		&r.Result, &r.TriggeredBy, &r.CanceledBy, &params,
	}

	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}

	if err := json.Unmarshal(params, &r.Params); err != nil || r.Params == nil {
		r.Params = map[string]string{}
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
	return s.StartRun(ctx, StartRun{JobID: jobID, Trigger: TriggerManual, By: by}, now)
}

// StartRun is a run queued by hand or by a webhook.
type StartRun struct {
	JobID   string
	Trigger string
	By      string
	Params  map[string]string
}

// StartRun queues a run of the job, to start right away, keeping to the
// job's concurrency.
func (s *Store) StartRun(ctx context.Context, in StartRun, now time.Time) (string, error) {
	id := NewID()
	jobID := in.JobID

	params, err := jsonParams(in.Params)
	if err != nil {
		return "", err
	}

	err = s.inTxRetry(ctx, func(tx *sql.Tx) error {
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

		_, err = tx.ExecContext(ctx, s.q(`INSERT INTO {runs} (id, job_id, trigger, status, scheduled_at, not_before, created_at, triggered_by, params)
			VALUES ($1, $2, $3, $4, $5, $5, $5, $6, $7)`), id, jobID, in.Trigger, StatusQueued, now, in.By, params)

		return err
	})
	if err != nil {
		return "", err
	}

	s.announce(true, id)

	return id, nil
}

func jsonParams(p map[string]string) ([]byte, error) {
	if p == nil {
		p = map[string]string{}
	}

	return json.Marshal(p)
}

// Report records a result reported from outside through the job's webhook:
// a finished run kronik did not run itself.
func (s *Store) Report(ctx context.Context, jobID string, res Result, by string, now time.Time) (string, error) {
	id := NewID()

	err := s.inTxRetry(ctx, func(tx *sql.Tx) error {
		var name string
		if err := tx.QueryRowContext(ctx, s.q(`SELECT name FROM {jobs} WHERE id = $1 FOR NO KEY UPDATE`), jobID).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}

			return err
		}

		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO {runs} (id, job_id, trigger, status, scheduled_at, not_before, created_at,
				started_at, finished_at, result_code, output, error, triggered_by, node)
			VALUES ($1, $2, $3, $4, $5, $5, $5, $5, $5, $6, $7, $8, $9, '')`),
			id, jobID, TriggerPing, res.Status, now, res.ResultCode, res.Output, res.Error, by); err != nil {
			return err
		}

		return s.finalize(ctx, tx, ended{runID: id, jobID: jobID, status: res.Status, errText: res.Error}, now)
	})
	if err == nil {
		s.announce(true, id)
	}

	return id, err
}

// CancelRun cancels a queued run at once and asks the replica running a
// running one to stop it.
func (s *Store) CancelRun(ctx context.Context, id string, now time.Time, by string) error {
	defer s.announce(false, id)

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
	// Skip, when set, records the runs as skipped with this reason (kronik
	// is paused, or in a maintenance window).
	Skip string
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

		if status == StatusQueued && p.Skip != "" {
			status, errText = StatusSkipped, p.Skip
		}

		var finished any
		if status == StatusSkipped {
			finished = now
		}

		notBefore := t
		if j.JitterSeconds > 0 && status == StatusQueued {
			notBefore = t.Add(time.Duration(rand.Int64N(int64(j.JitterSeconds)*1000+1)) * time.Millisecond) //nolint:gosec // spreading load, not security
		}

		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO {runs} (id, job_id, trigger, status, scheduled_at, not_before, created_at, finished_at, error)
			VALUES ($1, $2, $3, $4, $5, $9, $6, $7, $8)
			ON CONFLICT (job_id, scheduled_at) WHERE trigger = 'schedule' DO NOTHING`),
			NewID(), j.ID, TriggerSchedule, status, t, now, finished, errText, notBefore); err != nil {
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
	return s.ClaimRunsLimited(ctx, node, now, limit, executors, nil)
}

// ClaimRunsLimited is ClaimRuns keeping to tag limits: at most n runs of the
// jobs with a tag running at once, across all replicas. With limits, claims
// of all replicas take one lock, so two cannot both take the last place.
func (s *Store) ClaimRunsLimited(ctx context.Context, node string, now time.Time, limit int, executors []string, tagLimits map[string]int) ([]Claimed, error) {
	var claimed []Claimed

	err := s.inTxRetry(ctx, func(tx *sql.Tx) error {
		claimed = nil

		ids, err := s.claimable(ctx, tx, now, limit, executors, tagLimits)
		if err != nil || len(ids) == 0 {
			return err
		}

		if _, err := tx.ExecContext(ctx, s.q(`UPDATE {runs} SET status = $1, node = $2, started_at = $3 WHERE id = ANY($4)`),
			StatusRunning, node, now, ids); err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx, s.q(`SELECT `+runColumns+`, `+jobColumns+`
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

	if err == nil && len(claimed) > 0 {
		ids := make([]string, 0, len(claimed))
		for _, c := range claimed {
			ids = append(ids, c.Run.ID)
		}

		s.announce(false, ids...)
	}

	return claimed, err
}

// claimable picks the queued runs to take and locks them.
func (s *Store) claimable(ctx context.Context, tx *sql.Tx, now time.Time, limit int, executors []string, tagLimits map[string]int) ([]string, error) {
	look := limit
	if len(tagLimits) > 0 {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, s.t.runs+":tag-limits"); err != nil {
			return nil, err
		}

		// Runs held back by a limit are skipped over, so look further.
		look = limit*5 + 20
	}

	rows, err := tx.QueryContext(ctx, s.q(`SELECT r.id, j.tags FROM {runs} r JOIN {jobs} j ON j.id = r.job_id
		WHERE r.status = $1 AND r.not_before <= $2 AND j.executor = ANY($3)
		ORDER BY r.not_before, r.id LIMIT $4 FOR UPDATE OF r SKIP LOCKED`), StatusQueued, now, executors, look)
	if err != nil {
		return nil, err
	}

	type cand struct {
		id   string
		tags []string
	}

	var cands []cand

	for rows.Next() {
		var (
			c   cand
			raw []byte
		)

		if err := rows.Scan(&c.id, &raw); err != nil {
			_ = rows.Close()

			return nil, err
		}

		_ = json.Unmarshal(raw, &c.tags)
		cands = append(cands, c)
	}

	_ = rows.Close()

	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(tagLimits) == 0 {
		ids := make([]string, 0, len(cands))
		for _, c := range cands {
			ids = append(ids, c.id)
		}

		return ids, nil
	}

	running, err := s.runningByTag(ctx, tx, tagLimits)
	if err != nil {
		return nil, err
	}

	var ids []string

	for _, c := range cands {
		if len(ids) >= limit {
			break
		}

		ok := true

		for _, t := range c.tags {
			if n, limited := tagLimits[t]; limited && running[t] >= n {
				ok = false

				break
			}
		}

		if !ok {
			continue
		}

		for _, t := range c.tags {
			running[t]++
		}

		ids = append(ids, c.id)
	}

	return ids, nil
}

func (s *Store) runningByTag(ctx context.Context, tx *sql.Tx, tagLimits map[string]int) (map[string]int, error) {
	tags := make([]string, 0, len(tagLimits))
	for t := range tagLimits {
		tags = append(tags, t)
	}

	rows, err := tx.QueryContext(ctx, s.q(`SELECT t, COUNT(*) FROM {runs} r JOIN {jobs} j ON j.id = r.job_id,
			jsonb_array_elements_text(j.tags) t
		WHERE r.status = $1 AND t = ANY($2) GROUP BY t`), StatusRunning, tags)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int{}

	for rows.Next() {
		var (
			t string
			n int
		)

		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}

		out[t] = n
	}

	return out, rows.Err()
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
			WHERE r.id = $1 FOR NO KEY UPDATE OF j`), id).Scan(&jobID)
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

		end := ended{runID: id, jobID: jobID, status: res.Status, errText: res.Error, attempt: attempt}

		if canceled || (res.Status != StatusFailed && res.Status != StatusTimedOut) {
			return s.finalize(ctx, tx, end, now)
		}

		// Attempt n is followed by n+1 while the job allows n retries or
		// more; a paused job is not retried. The retry keeps the params.
		res2, err := tx.ExecContext(ctx, s.q(`INSERT INTO {runs} (id, job_id, trigger, status, attempt, scheduled_at, not_before, created_at, params)
			SELECT $1, id, $2, $3, $4::integer + 1, $5, $6::timestamptz + make_interval(secs => retry_delay_seconds), $6,
				(SELECT params FROM {runs} WHERE id = $8)
			FROM {jobs} WHERE id = $7 AND enabled AND retries >= $4::integer`),
			NewID(), TriggerRetry, StatusQueued, attempt, scheduledAt, now, jobID, id)
		if err != nil {
			return err
		}

		if n, _ := res2.RowsAffected(); n > 0 {
			return nil
		}

		return s.finalize(ctx, tx, end, now)
	})
	if ok {
		// A retry or a chained run may be queued: wake the replicas.
		s.announce(true, id)
	}

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
	if err == nil {
		s.announce(false, id)
	}

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

// RunPoint is one finished run for the job page's chart.
type RunPoint struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Trigger     string    `json:"trigger"`
	ScheduledAt time.Time `json:"scheduled_at"`
	// DurationMs is 0 for a run that did not start (skipped, canceled before).
	DurationMs int64 `json:"duration_ms"`
}

// RunPoints lists the last n finished runs of a job, oldest first.
func (s *Store) RunPoints(ctx context.Context, jobID string, n int) ([]RunPoint, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, status, trigger, scheduled_at,
			COALESCE(EXTRACT(EPOCH FROM (finished_at - started_at)) * 1000, 0)::bigint
		FROM {runs} WHERE job_id = $1 AND finished_at IS NOT NULL ORDER BY id DESC LIMIT $2`), jobID, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []RunPoint{}

	for rows.Next() {
		var p RunPoint
		if err := rows.Scan(&p.ID, &p.Status, &p.Trigger, &p.ScheduledAt, &p.DurationMs); err != nil {
			return nil, err
		}

		p.ScheduledAt = p.ScheduledAt.UTC()
		out = append(out, p)
	}

	slices.Reverse(out)

	return out, rows.Err()
}

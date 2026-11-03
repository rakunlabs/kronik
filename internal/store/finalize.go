package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// ended is a run that reached its final result: no retry follows it.
type ended struct {
	runID, jobID string
	status       string
	errText      string
	attempt      int
}

// Event is what a notification is about.
type Event struct {
	Event    string     `json:"event"`
	JobID    string     `json:"job_id"`
	JobName  string     `json:"job_name"`
	RunID    string     `json:"run_id,omitempty"`
	Status   string     `json:"status,omitempty"`
	Error    string     `json:"error,omitempty"`
	Attempt  int        `json:"attempt,omitempty"`
	At       time.Time  `json:"at"`
	Since    *time.Time `json:"since,omitempty"`
	Schedule string     `json:"schedule,omitempty"`
}

// finalize does what a run's final result causes, in the transaction that
// records it: the job's health, its notifications and the runs of the jobs
// that follow it. The job row is locked by the caller.
func (s *Store) finalize(ctx context.Context, tx *sql.Tx, e ended, now time.Time) error {
	var (
		name, health, schedule string
		notifyRaw              []byte
	)

	err := tx.QueryRowContext(ctx, s.q(`SELECT name, health, notify, schedule FROM {jobs} WHERE id = $1`), e.jobID).
		Scan(&name, &health, &notifyRaw, &schedule)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}

	if err != nil {
		return err
	}

	var notify Notify
	_ = json.Unmarshal(notifyRaw, &notify)

	ev := Event{JobID: e.jobID, JobName: name, RunID: e.runID, Status: e.status, Error: e.errText, Attempt: e.attempt, At: now, Schedule: schedule}

	switch e.status {
	case StatusSucceeded:
		if _, err := tx.ExecContext(ctx, s.q(`UPDATE {jobs} SET health = $2, alerted_at = NULL WHERE id = $1`), e.jobID, HealthOK); err != nil {
			return err
		}

		switch {
		case health == HealthFailing && notify.Wants(NotifyRecovery):
			ev.Event = NotifyRecovery
			if err := s.enqueue(ctx, tx, notify.Channels, ev, now); err != nil {
				return err
			}
		case notify.Wants(NotifySuccess):
			ev.Event = NotifySuccess
			if err := s.enqueue(ctx, tx, notify.Channels, ev, now); err != nil {
				return err
			}
		}
	case StatusFailed, StatusTimedOut, StatusLost:
		if _, err := tx.ExecContext(ctx, s.q(`UPDATE {jobs} SET health = $2 WHERE id = $1`), e.jobID, HealthFailing); err != nil {
			return err
		}

		// Only the change from working to failing is told, not every
		// failure of a job that keeps failing.
		if health != HealthFailing && notify.Wants(NotifyFailure) {
			ev.Event = NotifyFailure
			if err := s.enqueue(ctx, tx, notify.Channels, ev, now); err != nil {
				return err
			}
		}
	default:
		// Canceled and skipped runs say nothing about the job's health.
		return nil
	}

	return s.startFollowers(ctx, tx, e, name, now)
}

// startFollowers queues a run of every active job that runs after this one
// on this result.
func (s *Store) startFollowers(ctx context.Context, tx *sql.Tx, e ended, name string, now time.Time) error {
	on := []string{AfterFinished}
	if e.status == StatusSucceeded {
		on = append(on, AfterSucceeded)
	} else {
		on = append(on, AfterFailed)
	}

	rows, err := tx.QueryContext(ctx, s.q(`SELECT id, concurrency FROM {jobs}
		WHERE enabled AND after_jobs ? $1 AND after_on = ANY($2) AND id <> $1 ORDER BY id`), e.jobID, on)
	if err != nil {
		return err
	}

	type follower struct{ id, concurrency string }

	var fs []follower

	for rows.Next() {
		var f follower
		if err := rows.Scan(&f.id, &f.concurrency); err != nil {
			_ = rows.Close()

			return err
		}

		fs = append(fs, f)
	}

	_ = rows.Close()

	if err := rows.Err(); err != nil {
		return err
	}

	params, _ := json.Marshal(map[string]string{"after_job": name, "after_run": e.runID, "after_status": e.status})

	for _, f := range fs {
		status, errText := StatusQueued, ""

		switch f.concurrency {
		case ConcurrencyForbid:
			var active bool
			if err := tx.QueryRowContext(ctx, s.q(`SELECT EXISTS (SELECT 1 FROM {runs} WHERE job_id = $1 AND status IN ($2, $3))`),
				f.id, StatusQueued, StatusRunning).Scan(&active); err != nil {
				return err
			}

			if active {
				status, errText = StatusSkipped, "previous run still active"
			}
		case ConcurrencyReplace:
			if err := s.replaceActive(ctx, tx, f.id, now); err != nil {
				return err
			}
		}

		var finished any
		if status == StatusSkipped {
			finished = now
		}

		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO {runs} (id, job_id, trigger, status, scheduled_at, not_before, created_at,
				finished_at, error, triggered_by, params)
			VALUES ($1, $2, $3, $4, $5, $5, $5, $6, $7, $8, $9)`),
			NewID(), f.id, TriggerAfter, status, now, finished, errText, name, params); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) enqueue(ctx context.Context, tx *sql.Tx, channels []string, ev Event, now time.Time) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	for _, ch := range slices.Compact(slices.Sorted(slices.Values(channels))) {
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO {notifications} (id, channel_id, job_id, run_id, event, payload, next_at, created_at)
			SELECT $1, id, $3, $4, $5, $6, $7, $7 FROM {channels} WHERE id = $2`),
			NewID(), ch, ev.JobID, ev.RunID, ev.Event, payload, now); err != nil {
			return err
		}
	}

	return nil
}

// Silent is a job with an alert that has not had a successful run in its
// alert time.
type Silent struct {
	JobID string
	Since time.Time
}

// AlertSilent raises the "did not run" alert of every active job that had
// no successful run for its alert_after_seconds: it marks the job failing
// and queues its notification. Each job is alerted once until it succeeds
// again. It returns the jobs alerted.
func (s *Store) AlertSilent(ctx context.Context, now time.Time) ([]Silent, error) {
	var out []Silent

	err := s.inTxRetry(ctx, func(tx *sql.Tx) error {
		out = nil

		rows, err := tx.QueryContext(ctx, s.q(`SELECT j.id, j.name, j.health, j.notify, j.schedule, j.alert_after_seconds,
				GREATEST((SELECT MAX(finished_at) FROM {runs} r WHERE r.job_id = j.id AND r.status = $1), j.updated_at) AS since
			FROM {jobs} j
			WHERE j.enabled AND j.alert_after_seconds > 0 AND j.alerted_at IS NULL
			FOR NO KEY UPDATE OF j SKIP LOCKED`), StatusSucceeded)
		if err != nil {
			return err
		}

		type cand struct {
			id, name, health, schedule string
			notify                     []byte
			after                      int
			since                      time.Time
		}

		var cands []cand

		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.id, &c.name, &c.health, &c.notify, &c.schedule, &c.after, &c.since); err != nil {
				_ = rows.Close()

				return err
			}

			cands = append(cands, c)
		}

		_ = rows.Close()

		if err := rows.Err(); err != nil {
			return err
		}

		for _, c := range cands {
			if now.Sub(c.since) < time.Duration(c.after)*time.Second {
				continue
			}

			if _, err := tx.ExecContext(ctx, s.q(`UPDATE {jobs} SET alerted_at = $2, health = $3 WHERE id = $1`), c.id, now, HealthFailing); err != nil {
				return err
			}

			var notify Notify
			_ = json.Unmarshal(c.notify, &notify)

			since := c.since.UTC()
			if notify.Wants(NotifyNoRun) {
				ev := Event{Event: NotifyNoRun, JobID: c.id, JobName: c.name, At: now, Since: &since, Schedule: c.schedule}
				if err := s.enqueue(ctx, tx, notify.Channels, ev, now); err != nil {
					return err
				}
			}

			out = append(out, Silent{JobID: c.id, Since: since})
		}

		return nil
	})

	return out, err
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Node struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	SeenAt    time.Time `json:"seen_at"`
	// Running is the number of runs the replica is running now.
	Running int `json:"running"`
}

func (s *Store) Heartbeat(ctx context.Context, name, version string, startedAt, now time.Time) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO {nodes} (name, version, started_at, seen_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET version = EXCLUDED.version, started_at = EXCLUDED.started_at, seen_at = EXCLUDED.seen_at`),
		name, version, startedAt, now)

	return err
}

// RemoveNode drops this replica from the list. startedAt keeps another
// process that runs under the same name from being removed with it.
func (s *Store) RemoveNode(ctx context.Context, name string, startedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM {nodes} WHERE name = $1 AND started_at = $2`), name, startedAt)

	return err
}

// NodeStartedAt is when the replica named name last started, zero when it
// is not in the list or was not seen since the given time.
func (s *Store) NodeStartedAt(ctx context.Context, name string, since time.Time) (time.Time, error) {
	var t time.Time

	err := s.db.QueryRowContext(ctx, s.q(`SELECT started_at FROM {nodes} WHERE name = $1 AND seen_at >= $2`), name, since).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}

	return t, err
}

// ListNodes lists the replicas seen since the given time.
func (s *Store) ListNodes(ctx context.Context, since time.Time) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT n.name, n.version, n.started_at, n.seen_at,
			(SELECT COUNT(*) FROM {runs} r WHERE r.node = n.name AND r.status = $2)
		FROM {nodes} n WHERE n.seen_at >= $1 ORDER BY n.name`), since, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	nodes := []*Node{}

	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.Name, &n.Version, &n.StartedAt, &n.SeenAt, &n.Running); err != nil {
			return nil, err
		}

		n.StartedAt = n.StartedAt.UTC()
		n.SeenAt = n.SeenAt.UTC()
		nodes = append(nodes, &n)
	}

	return nodes, rows.Err()
}

// RecoverLost marks the running runs of replicas not seen since the given
// time as lost and forgets those replicas. It returns the number of runs.
func (s *Store) RecoverLost(ctx context.Context, since, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE {runs} SET status = $1, finished_at = $2, error = 'the replica running it stopped'
		WHERE status = $3 AND node NOT IN (SELECT name FROM {nodes} WHERE seen_at >= $4)`),
		StatusLost, now, StatusRunning, since)
	if err != nil {
		return 0, err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}

	_, err = s.db.ExecContext(ctx, s.q(`DELETE FROM {nodes} WHERE seen_at < $1`), since)

	return n, err
}

// pruneBatch bounds one delete, so pruning never holds locks for long; what
// is left goes in the next round.
const pruneBatch = 5000

// PruneRuns deletes finished runs older than before (when set) and, per job,
// all but the newest keep finished runs (when keep is above zero). Skipped
// runs count too.
func (s *Store) PruneRuns(ctx context.Context, before time.Time, keep int) (int64, error) {
	var total int64

	if !before.IsZero() {
		res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM {runs} WHERE id IN (
				SELECT id FROM {runs} WHERE finished_at IS NOT NULL AND finished_at < $1 LIMIT $2
			)`), before, pruneBatch)
		if err != nil {
			return 0, err
		}

		n, _ := res.RowsAffected()
		total += n
	}

	if keep > 0 {
		res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM {runs} WHERE id IN (
				SELECT id FROM (
					SELECT id, ROW_NUMBER() OVER (PARTITION BY job_id ORDER BY id DESC) AS n
					FROM {runs} WHERE finished_at IS NOT NULL
				) ranked WHERE n > $1 LIMIT $2
			)`), keep, pruneBatch)
		if err != nil {
			return total, err
		}

		n, _ := res.RowsAffected()
		total += n
	}

	return total, nil
}

type Stats struct {
	Jobs        int `json:"jobs"`
	Enabled     int `json:"enabled"`
	Running     int `json:"running"`
	Queued      int `json:"queued"`
	Failed24h   int `json:"failed_24h"`
	Succeeded24 int `json:"succeeded_24h"`
}

func (s *Store) Stats(ctx context.Context, now time.Time) (*Stats, error) {
	var st Stats

	err := s.db.QueryRowContext(ctx, s.q(`SELECT
			(SELECT COUNT(*) FROM {jobs}),
			(SELECT COUNT(*) FROM {jobs} WHERE enabled),
			(SELECT COUNT(*) FROM {runs} WHERE status = $1),
			(SELECT COUNT(*) FROM {runs} WHERE status = $2),
			(SELECT COUNT(*) FROM {runs} WHERE finished_at >= $3 AND status IN ($4, $5, $6)),
			(SELECT COUNT(*) FROM {runs} WHERE finished_at >= $3 AND status = $7)`),
		StatusRunning, StatusQueued, now.Add(-24*time.Hour), StatusFailed, StatusTimedOut, StatusLost, StatusSucceeded).
		Scan(&st.Jobs, &st.Enabled, &st.Running, &st.Queued, &st.Failed24h, &st.Succeeded24)

	return &st, err
}

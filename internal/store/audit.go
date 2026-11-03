package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AuditEntry is one change made through the API or MCP.
type AuditEntry struct {
	ID      string          `json:"id"`
	At      time.Time       `json:"at"`
	Actor   string          `json:"actor"`
	Via     string          `json:"via"`
	Action  string          `json:"action"`
	JobID   string          `json:"job_id"`
	JobName string          `json:"job_name"`
	RunID   string          `json:"run_id"`
	Detail  json.RawMessage `json:"detail"`
}

func (s *Store) AddAudit(ctx context.Context, e *AuditEntry) error {
	detail := e.Detail
	if len(detail) == 0 {
		detail = json.RawMessage("{}")
	}

	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO {audit} (id, at, actor, via, action, job_id, job_name, run_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`),
		NewID(), e.At, e.Actor, e.Via, e.Action, e.JobID, e.JobName, e.RunID, []byte(detail))

	return err
}

type AuditFilter struct {
	JobID  string
	Actor  string
	Before string
	Limit  int
	// Actions keeps only these actions.
	Actions []string
}

func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]*AuditEntry, error) {
	var (
		where []string
		args  []any
	)

	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}

	if f.JobID != "" {
		add("job_id = $%d", f.JobID)
	}

	if f.Actor != "" {
		add("actor = $%d", f.Actor)
	}

	if f.Before != "" {
		add("id < $%d", f.Before)
	}

	if len(f.Actions) > 0 {
		add("action = ANY($%d)", f.Actions)
	}

	query := `SELECT id, at, actor, via, action, job_id, job_name, run_id, detail FROM {audit}`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	args = append(args, f.Limit)
	query += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))

	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []*AuditEntry{}

	for rows.Next() {
		var (
			e      AuditEntry
			detail []byte
		)

		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Via, &e.Action, &e.JobID, &e.JobName, &e.RunID, &detail); err != nil {
			return nil, err
		}

		e.At = e.At.UTC()
		e.Detail = json.RawMessage(detail)
		out = append(out, &e)
	}

	return out, rows.Err()
}

// PruneAudit deletes entries older than before.
func (s *Store) PruneAudit(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM {audit} WHERE at < $1`), before)
	if err != nil {
		return 0, err
	}

	return res.RowsAffected()
}

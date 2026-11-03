package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Setting is one value changed from the UI.
type Setting struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
	UpdatedBy string          `json:"updated_by"`
}

// Settings returns the stored settings; keys starting with "_" are kronik's
// own bookkeeping and are left out.
func (s *Store) Settings(ctx context.Context) (map[string]Setting, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT key, value, updated_at, updated_by FROM {settings} WHERE key NOT LIKE '\_%'`))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]Setting{}

	for rows.Next() {
		var (
			st  Setting
			raw []byte
		)

		if err := rows.Scan(&st.Key, &raw, &st.UpdatedAt, &st.UpdatedBy); err != nil {
			return nil, err
		}

		st.Value = raw
		st.UpdatedAt = st.UpdatedAt.UTC()
		out[st.Key] = st
	}

	return out, rows.Err()
}

// ClaimDaily reports whether this replica takes the task for the day: the
// first to ask for a day gets it, every other replica is told no.
func (s *Store) ClaimDaily(ctx context.Context, task, day string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`INSERT INTO {settings} (key, value, updated_at, updated_by) VALUES ($1, to_jsonb($2::text), $3, '')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at
		WHERE {settings}.value IS DISTINCT FROM EXCLUDED.value`),
		"_daily."+task, day, Now())
	if err != nil {
		return false, err
	}

	n, err := res.RowsAffected()

	return n == 1, err
}

// PutSettings stores values and drops the keys in reset, in one change.
func (s *Store) PutSettings(ctx context.Context, values map[string]json.RawMessage, reset []string, by string, now time.Time) error {
	return s.inTxRetry(ctx, func(tx *sql.Tx) error {
		for k, v := range values {
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO {settings} (key, value, updated_at, updated_by) VALUES ($1, $2, $3, $4)
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`),
				k, []byte(v), now, by); err != nil {
				return err
			}
		}

		for _, k := range reset {
			if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM {settings} WHERE key = $1`), k); err != nil {
				return err
			}
		}

		return nil
	})
}

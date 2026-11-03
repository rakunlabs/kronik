package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SecretKey is a data key as stored: wrapped by a master key.
type SecretKey struct {
	ID        string    `json:"id"`
	Wrapped   []byte    `json:"-"`
	Master    string    `json:"master"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	// Secrets is the number of secrets encrypted with this key.
	Secrets int `json:"secrets"`
}

func (s *Store) SecretKeys(ctx context.Context) ([]*SecretKey, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT k.id, k.wrapped, k.master, k.active, k.created_at,
			(SELECT COUNT(*) FROM {secrets} s WHERE s.key_id = k.id)
		FROM {secret_keys} k ORDER BY k.created_at DESC, k.id DESC`))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []*SecretKey{}

	for rows.Next() {
		var k SecretKey
		if err := rows.Scan(&k.ID, &k.Wrapped, &k.Master, &k.Active, &k.CreatedAt, &k.Secrets); err != nil {
			return nil, err
		}

		k.CreatedAt = k.CreatedAt.UTC()
		out = append(out, &k)
	}

	return out, rows.Err()
}

// AddSecretKey stores a new data key and makes it the active one.
func (s *Store) AddSecretKey(ctx context.Context, k *SecretKey) error {
	return s.inTxRetry(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.q(`UPDATE {secret_keys} SET active = FALSE WHERE active`)); err != nil {
			return err
		}

		_, err := tx.ExecContext(ctx, s.q(`INSERT INTO {secret_keys} (id, wrapped, master, active, created_at) VALUES ($1, $2, $3, TRUE, $4)`),
			k.ID, k.Wrapped, k.Master, k.CreatedAt)

		return err
	})
}

// RewrapSecretKey stores a data key wrapped by another master key.
func (s *Store) RewrapSecretKey(ctx context.Context, id string, wrapped []byte, master string) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE {secret_keys} SET wrapped = $2, master = $3 WHERE id = $1`), id, wrapped, master)

	return err
}

// DropUnusedSecretKeys deletes the keys that are not active and encrypt no
// secret, created before the given time.
func (s *Store) DropUnusedSecretKeys(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM {secret_keys} k WHERE NOT k.active AND k.created_at < $1
		AND NOT EXISTS (SELECT 1 FROM {secrets} s WHERE s.key_id = k.id)`), before)
	if err != nil {
		return 0, err
	}

	return res.RowsAffected()
}

// Secret is a stored secret; Value is encrypted.
type Secret struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Value       []byte    `json:"-"`
	KeyID       string    `json:"key_id"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by"`
}

const secretColumns = `name, description, value, key_id, created_at, created_by, updated_at, updated_by`

func scanSecret(row scanner) (*Secret, error) {
	var x Secret
	if err := row.Scan(&x.Name, &x.Description, &x.Value, &x.KeyID, &x.CreatedAt, &x.CreatedBy, &x.UpdatedAt, &x.UpdatedBy); err != nil {
		return nil, err
	}

	x.CreatedAt = x.CreatedAt.UTC()
	x.UpdatedAt = x.UpdatedAt.UTC()

	return &x, nil
}

func (s *Store) ListSecrets(ctx context.Context) ([]*Secret, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+secretColumns+` FROM {secrets} ORDER BY name`))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []*Secret{}

	for rows.Next() {
		x, err := scanSecret(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, x)
	}

	return out, rows.Err()
}

func (s *Store) GetSecret(ctx context.Context, name string) (*Secret, error) {
	x, err := scanSecret(s.db.QueryRowContext(ctx, s.q(`SELECT `+secretColumns+` FROM {secrets} WHERE name = $1`), name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}

	return x, err
}

// PutSecret creates or replaces a secret. A nil Value keeps the stored
// value and changes only the description.
func (s *Store) PutSecret(ctx context.Context, x *Secret) error {
	if x.Value == nil {
		res, err := s.db.ExecContext(ctx, s.q(`UPDATE {secrets} SET description = $2, updated_at = $3, updated_by = $4 WHERE name = $1`),
			x.Name, x.Description, x.UpdatedAt, x.UpdatedBy)
		if err != nil {
			return err
		}

		return affected(res)
	}

	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO {secrets} (name, description, value, key_id, created_at, created_by, updated_at, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $6)
		ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description, value = EXCLUDED.value, key_id = EXCLUDED.key_id,
			updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`),
		x.Name, x.Description, x.Value, x.KeyID, x.UpdatedAt, x.UpdatedBy)

	return err
}

// ReencryptSecret replaces a secret's value with one encrypted by another
// key, if the secret was not changed meanwhile.
func (s *Store) ReencryptSecret(ctx context.Context, name string, value []byte, keyID string, prevKey string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE {secrets} SET value = $2, key_id = $3 WHERE name = $1 AND key_id = $4`),
		name, value, keyID, prevKey)
	if err != nil {
		return false, err
	}

	n, err := res.RowsAffected()

	return n == 1, err
}

func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM {secrets} WHERE name = $1`), name)
	if err != nil {
		return err
	}

	return affected(res)
}

// SecretsVersion changes whenever a secret or key is added, changed or
// removed, so replicas reload their copy only then.
func (s *Store) SecretsVersion(ctx context.Context) (string, error) {
	var v sql.NullString

	err := s.db.QueryRowContext(ctx, s.q(`SELECT
			(SELECT COUNT(*) || ':' || COALESCE(MAX(updated_at)::text, '') || ':' || COALESCE(string_agg(name || key_id, ',' ORDER BY name), '') FROM {secrets})
			|| '|' ||
			(SELECT COUNT(*) || ':' || COALESCE(string_agg(id || master || active::text, ',' ORDER BY id), '') FROM {secret_keys})`)).Scan(&v)

	return v.String, err
}

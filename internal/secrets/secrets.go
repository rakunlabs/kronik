// Package secrets keeps the secrets set from the UI, encrypted in
// PostgreSQL.
//
// Envelope encryption: every value is sealed with AES-256-GCM under a data
// key; data keys are sealed under a master key that comes from the
// environment and is never stored. Rotating makes a new data key and
// re-encrypts the secrets under it; a new master key re-wraps the data keys.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kronik/internal/store"
)

// MasterKey is a key from the environment that wraps the data keys.
type MasterKey struct {
	// ID names the key: the first bytes of its hash, so the key it was
	// wrapped with can be told without storing it.
	ID  string
	key []byte
}

// ParseMasterKey reads a 32-byte key given as base64 or hex, or derives one
// from a passphrase of at least 16 characters.
func ParseMasterKey(s string) (MasterKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return MasterKey{}, errors.New("empty")
	}

	var key []byte

	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		key = b
	} else if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		key = b
	} else if len(s) >= 16 {
		sum := sha256.Sum256([]byte("kronik-master-key:" + s))
		key = sum[:]
	} else {
		return MasterKey{}, errors.New("give 32 bytes as base64 or hex (openssl rand -base64 32), or a passphrase of 16 characters or more")
	}

	id := sha256.Sum256(append([]byte("kronik-key-id:"), key...))

	return MasterKey{ID: hex.EncodeToString(id[:4]), key: key}, nil
}

func seal(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func open(key, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("value too short")
	}

	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}

// Store is what the vault needs from the database.
type Store interface {
	SecretKeys(ctx context.Context) ([]*store.SecretKey, error)
	AddSecretKey(ctx context.Context, k *store.SecretKey) error
	RewrapSecretKey(ctx context.Context, id string, wrapped []byte, master string) error
	DropUnusedSecretKeys(ctx context.Context, before time.Time) (int64, error)
	ListSecrets(ctx context.Context) ([]*store.Secret, error)
	GetSecret(ctx context.Context, name string) (*store.Secret, error)
	PutSecret(ctx context.Context, x *store.Secret) error
	ReencryptSecret(ctx context.Context, name string, value []byte, keyID, prevKey string) (bool, error)
	DeleteSecret(ctx context.Context, name string) error
	SecretsVersion(ctx context.Context) (string, error)
}

// Vault holds the secrets for the executors: the ones from the replica's
// configuration and the ones from the database, decrypted in memory.
type Vault struct {
	st      Store
	masters []MasterKey // the first wraps new keys; the rest only open old ones
	static  map[string]string

	mu      sync.RWMutex
	values  map[string]string
	version string
	problem string
}

// ErrNoMaster is a change refused because no master key is set.
var ErrNoMaster = errors.New("secrets cannot be stored: set KRONIK_SECRETS_KEY (openssl rand -base64 32) on every replica")

// NewVault makes a vault. static are the secrets from the configuration;
// a stored secret of the same name wins. masters may be empty: then only
// static secrets are there.
func NewVault(st Store, masters []MasterKey, static map[string]string) *Vault {
	return &Vault{st: st, masters: masters, static: maps.Clone(static), values: maps.Clone(static)}
}

// CanStore reports whether a master key is set.
func (v *Vault) CanStore() bool { return len(v.masters) > 0 }

// MasterID is the id of the master key that wraps new data keys.
func (v *Vault) MasterID() string {
	if len(v.masters) == 0 {
		return ""
	}

	return v.masters[0].ID
}

// Values are the secrets in effect: name → value.
func (v *Vault) Values() map[string]string {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.values
}

// Problem is why stored secrets could not be read, "" when they could.
func (v *Vault) Problem() string {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.problem
}

// StaticNames lists the secrets from the configuration.
func (v *Vault) StaticNames() []string {
	return slices.Sorted(maps.Keys(v.static))
}

// Load reads the stored secrets again when they changed. It returns whether
// the values changed.
func (v *Vault) Load(ctx context.Context) (bool, error) {
	ver, err := v.st.SecretsVersion(ctx)
	if err != nil {
		return false, err
	}

	v.mu.RLock()
	same := ver == v.version
	v.mu.RUnlock()

	if same {
		return false, nil
	}

	keys, err := v.dataKeys(ctx)
	if err != nil {
		return false, err
	}

	stored, err := v.st.ListSecrets(ctx)
	if err != nil {
		return false, err
	}

	values := maps.Clone(v.static)
	if values == nil {
		values = map[string]string{}
	}

	var problems []string

	for _, x := range stored {
		key, ok := keys[x.KeyID]
		if !ok {
			problems = append(problems, x.Name)

			continue
		}

		plain, err := open(key, x.Value)
		if err != nil {
			problems = append(problems, x.Name)

			continue
		}

		values[x.Name] = string(plain)
	}

	problem := ""
	if len(problems) > 0 {
		problem = fmt.Sprintf("cannot decrypt %s: the master key (KRONIK_SECRETS_KEY) of this replica is not the one they were stored with",
			strings.Join(problems, ", "))
		slog.Error("secrets not readable", "secrets", problems)
	}

	v.mu.Lock()
	v.values, v.version, v.problem = values, ver, problem
	v.mu.Unlock()

	return true, nil
}

// dataKeys opens every data key this replica's master keys can open.
func (v *Vault) dataKeys(ctx context.Context) (map[string][]byte, error) {
	ks, err := v.st.SecretKeys(ctx)
	if err != nil {
		return nil, err
	}

	out := map[string][]byte{}

	for _, k := range ks {
		for _, m := range v.masters {
			if m.ID != k.Master {
				continue
			}

			if key, err := open(m.key, k.Wrapped); err == nil {
				out[k.ID] = key
			}
		}
	}

	return out, nil
}

// activeKey returns the data key new values are sealed with, making one if
// there is none (or the active one is not readable here).
func (v *Vault) activeKey(ctx context.Context) (string, []byte, error) {
	if !v.CanStore() {
		return "", nil, ErrNoMaster
	}

	ks, err := v.st.SecretKeys(ctx)
	if err != nil {
		return "", nil, err
	}

	for _, k := range ks {
		if !k.Active {
			continue
		}

		for _, m := range v.masters {
			if m.ID == k.Master {
				if key, err := open(m.key, k.Wrapped); err == nil {
					return k.ID, key, nil
				}
			}
		}

		return "", nil, fmt.Errorf("the active data key is wrapped by master key %s, which this replica does not have", k.Master)
	}

	return v.newKey(ctx)
}

func (v *Vault) newKey(ctx context.Context) (string, []byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", nil, err
	}

	wrapped, err := seal(v.masters[0].key, key)
	if err != nil {
		return "", nil, err
	}

	k := &store.SecretKey{ID: store.NewID(), Wrapped: wrapped, Master: v.masters[0].ID, CreatedAt: store.Now()}
	if err := v.st.AddSecretKey(ctx, k); err != nil {
		return "", nil, err
	}

	return k.ID, key, nil
}

var secretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)

// ValidName reports whether name can be a secret's name (secrets.NAME).
func ValidName(name string) bool { return secretName.MatchString(name) }

// maxValue bounds a secret's value.
const maxValue = 64 << 10

// Put stores a secret. A nil value keeps the stored value (the description
// changes).
func (v *Vault) Put(ctx context.Context, name, description string, value *string, by string) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: a name is letters, digits and _ and does not start with a digit", ErrInvalid)
	}

	now := store.Now()
	x := &store.Secret{Name: name, Description: strings.TrimSpace(description), CreatedAt: now, CreatedBy: by, UpdatedAt: now, UpdatedBy: by}

	if value != nil {
		if len(*value) > maxValue {
			return fmt.Errorf("%w: a value is at most %d KiB", ErrInvalid, maxValue>>10)
		}

		id, key, err := v.activeKey(ctx)
		if err != nil {
			return err
		}

		sealed, err := seal(key, []byte(*value))
		if err != nil {
			return err
		}

		x.Value, x.KeyID = sealed, id
	}

	if err := v.st.PutSecret(ctx, x); err != nil {
		return err
	}

	_, err := v.Load(ctx)

	return err
}

func (v *Vault) Delete(ctx context.Context, name string) error {
	if err := v.st.DeleteSecret(ctx, name); err != nil {
		return err
	}

	_, err := v.Load(ctx)

	return err
}

// ErrInvalid marks an input error.
var ErrInvalid = errors.New("invalid")

// RotateStats is what a rotation did.
type RotateStats struct {
	Key         string `json:"key"`
	Reencrypted int    `json:"reencrypted"`
	Rewrapped   int    `json:"rewrapped"`
	Dropped     int64  `json:"dropped"`
}

// Rotate makes a new data key, re-encrypts every secret with it, re-wraps
// the data keys still around under the current master key and drops the
// old keys no secret uses anymore.
func (v *Vault) Rotate(ctx context.Context) (RotateStats, error) {
	var st RotateStats

	if !v.CanStore() {
		return st, ErrNoMaster
	}

	keys, err := v.dataKeys(ctx)
	if err != nil {
		return st, err
	}

	id, key, err := v.newKey(ctx)
	if err != nil {
		return st, err
	}

	st.Key = id

	stored, err := v.st.ListSecrets(ctx)
	if err != nil {
		return st, err
	}

	for _, x := range stored {
		old, ok := keys[x.KeyID]
		if !ok {
			return st, fmt.Errorf("cannot decrypt secret %s: its key is wrapped by a master key this replica does not have", x.Name)
		}

		plain, err := open(old, x.Value)
		if err != nil {
			return st, fmt.Errorf("cannot decrypt secret %s: %w", x.Name, err)
		}

		sealed, err := seal(key, plain)
		if err != nil {
			return st, err
		}

		if ok, err := v.st.ReencryptSecret(ctx, x.Name, sealed, id, x.KeyID); err != nil {
			return st, err
		} else if ok {
			st.Reencrypted++
		}
	}

	// Keys left (in use by a secret changed meanwhile) move to the current
	// master key, so an old master key can be taken away.
	ks, err := v.st.SecretKeys(ctx)
	if err != nil {
		return st, err
	}

	for _, k := range ks {
		plain, ok := keys[k.ID]
		if !ok || k.Master == v.masters[0].ID {
			continue
		}

		wrapped, err := seal(v.masters[0].key, plain)
		if err != nil {
			return st, err
		}

		if err := v.st.RewrapSecretKey(ctx, k.ID, wrapped, v.masters[0].ID); err != nil {
			return st, err
		}

		st.Rewrapped++
	}

	if st.Dropped, err = v.st.DropUnusedSecretKeys(ctx, store.Now()); err != nil {
		return st, err
	}

	_, err = v.Load(ctx)

	return st, err
}

// KeyAge is how old the active data key is, zero without one.
func (v *Vault) KeyAge(ctx context.Context) (time.Duration, error) {
	ks, err := v.st.SecretKeys(ctx)
	if err != nil {
		return 0, err
	}

	for _, k := range ks {
		if k.Active {
			return time.Since(k.CreatedAt), nil
		}
	}

	return 0, nil
}

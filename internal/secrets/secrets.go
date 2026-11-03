// Package secrets keeps the secrets set from the UI, encrypted in
// PostgreSQL.
//
// Envelope encryption: every value is sealed with AES-256-GCM under a data
// key; data keys are sealed under a master key made from the configured
// secret and never stored. Changing the master key (from the UI) re-wraps
// the data keys; the values stay as they are.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/rakunlabs/kronik/internal/store"
)

// MasterKey wraps the data keys. It is made from any secret, even an empty
// one: the secret's SHA-256 is the key.
type MasterKey struct {
	// ID names the key: the first bytes of its hash, so the key it was
	// wrapped with can be told without storing it.
	ID  string
	key []byte
}

// NewMasterKey makes the master key of a secret.
func NewMasterKey(secret string) MasterKey {
	key := sha256.Sum256([]byte(secret))
	id := sha256.Sum256(append([]byte("kronik-key-id:"), key[:]...))

	return MasterKey{ID: hex.EncodeToString(id[:4]), key: key[:]}
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
	RewrapSecretKeys(ctx context.Context, keys []store.Rewrap) error
	ListSecrets(ctx context.Context) ([]*store.Secret, error)
	GetSecret(ctx context.Context, name string) (*store.Secret, error)
	PutSecret(ctx context.Context, x *store.Secret) error
	DeleteSecret(ctx context.Context, name string) error
	SecretsVersion(ctx context.Context) (string, error)
}

// Vault holds the secrets for the executors: the ones from the replica's
// configuration and the ones from the database, decrypted in memory.
type Vault struct {
	st Store
	// configured is the master key from the configuration; the next start
	// uses it, whatever was changed from the UI meanwhile.
	configured string
	static     map[string]string

	mu     sync.RWMutex
	master MasterKey
	// dataKeys are the data keys opened so far. A data key never changes,
	// so a replica keeps reading the secrets after another replica changed
	// the master key, until it restarts.
	dataKeys map[string][]byte
	values   map[string]string
	version  string
	problem  string
	stored   string // master key id the stored data keys are wrapped with
}

// NewVault makes a vault. static are the secrets from the configuration;
// a stored secret of the same name wins.
func NewVault(st Store, master MasterKey, static map[string]string) *Vault {
	return &Vault{st: st, configured: master.ID, master: master, static: maps.Clone(static),
		dataKeys: map[string][]byte{}, values: maps.Clone(static)}
}

// Status is the state of the master key on this replica.
type Status struct {
	// MasterID is the key this replica wraps new data keys with.
	MasterID string `json:"master_id"`
	// ConfiguredID is the key from this replica's configuration.
	ConfiguredID string `json:"configured_id"`
	// StoredID is the key the stored data keys are wrapped with, "" without
	// data keys.
	StoredID string `json:"stored_id"`
	// Problem is why stored secrets could not be read, "" when they could.
	Problem string `json:"problem"`
	// ConfigOutdated is set when the configured key is not the one the
	// stored data keys are wrapped with: the next start cannot read them.
	ConfigOutdated bool `json:"config_outdated"`
}

func (v *Vault) Status() Status {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return Status{MasterID: v.master.ID, ConfiguredID: v.configured, StoredID: v.stored, Problem: v.problem,
		ConfigOutdated: v.stored != "" && v.stored != v.configured}
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

	ks, err := v.st.SecretKeys(ctx)
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

	keys := map[string][]byte{}
	storedID := ""

	for _, k := range ks {
		if key, ok := v.dataKey(k); ok {
			keys[k.ID] = key
		}

		if k.Active {
			storedID = k.Master
		}
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
		problem = fmt.Sprintf("cannot decrypt %s: they are encrypted with key %s, the key of this replica (secrets.key) is %s",
			strings.Join(problems, ", "), storedID, v.Status().MasterID)
		slog.Error("secrets not readable", "secrets", problems)
	}

	v.mu.Lock()
	v.values, v.version, v.problem, v.stored = values, ver, problem, storedID
	v.mu.Unlock()

	return true, nil
}

// dataKey opens a stored data key, or takes it from the ones opened before.
func (v *Vault) dataKey(k *store.SecretKey) ([]byte, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if key, ok := v.dataKeys[k.ID]; ok {
		return key, true
	}

	if k.Master != v.master.ID {
		return nil, false
	}

	key, err := open(v.master.key, k.Wrapped)
	if err != nil {
		return nil, false
	}

	v.dataKeys[k.ID] = key

	return key, true
}

// activeKey returns the data key new values are sealed with, making one if
// there is none.
func (v *Vault) activeKey(ctx context.Context) (string, []byte, error) {
	ks, err := v.st.SecretKeys(ctx)
	if err != nil {
		return "", nil, err
	}

	for _, k := range ks {
		if !k.Active {
			continue
		}

		if key, ok := v.dataKey(k); ok {
			return k.ID, key, nil
		}

		return "", nil, fmt.Errorf("the secrets are encrypted with key %s, which this replica does not have: set it as secrets.key", k.Master)
	}

	return v.newKey(ctx)
}

func (v *Vault) newKey(ctx context.Context) (string, []byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", nil, err
	}

	v.mu.RLock()
	master := v.master
	v.mu.RUnlock()

	wrapped, err := seal(master.key, key)
	if err != nil {
		return "", nil, err
	}

	k := &store.SecretKey{ID: store.NewID(), Wrapped: wrapped, Master: master.ID, CreatedAt: store.Now()}
	if err := v.st.AddSecretKey(ctx, k); err != nil {
		return "", nil, err
	}

	v.mu.Lock()
	v.dataKeys[k.ID] = key
	v.mu.Unlock()

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

// ChangeStats is what a key change did.
type ChangeStats struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Rewrapped int    `json:"rewrapped"`
}

// ChangeKey moves the data keys to the master key made from secret. The
// values are not touched: other replicas keep reading them with the data
// keys they opened, but each needs the new secret in its configuration
// before it starts again.
func (v *Vault) ChangeKey(ctx context.Context, secret string) (ChangeStats, error) {
	v.mu.RLock()
	from := v.master
	v.mu.RUnlock()

	to := NewMasterKey(secret)
	st := ChangeStats{From: from.ID, To: to.ID}

	ks, err := v.st.SecretKeys(ctx)
	if err != nil {
		return st, err
	}

	for _, k := range ks {
		if k.Active {
			st.From = k.Master
		}
	}

	if st.From == to.ID {
		return st, fmt.Errorf("%w: the new key is the same as the one in use", ErrInvalid)
	}

	rewrapped := make([]store.Rewrap, 0, len(ks))

	for _, k := range ks {
		plain, ok := v.dataKey(k)
		if !ok {
			return st, fmt.Errorf("the secrets are encrypted with key %s, not with the key of this replica (%s)", k.Master, from.ID)
		}

		wrapped, err := seal(to.key, plain)
		if err != nil {
			return st, err
		}

		rewrapped = append(rewrapped, store.Rewrap{ID: k.ID, Wrapped: wrapped, Master: to.ID, From: k.Master})
	}

	if err := v.st.RewrapSecretKeys(ctx, rewrapped); err != nil {
		return st, err
	}

	st.Rewrapped = len(rewrapped)

	v.mu.Lock()
	v.master = to
	v.version = "" // read again: the key in use changed
	v.mu.Unlock()

	// Without a data key nothing tells the other replicas which key is in
	// use: make one now, under the new key.
	if len(ks) == 0 {
		if _, _, err := v.newKey(ctx); err != nil {
			return st, err
		}
	}

	_, err = v.Load(ctx)

	return st, err
}

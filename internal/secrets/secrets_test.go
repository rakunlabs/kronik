package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rakunlabs/kronik/internal/store"
)

type memStore struct {
	mu      sync.Mutex
	keys    map[string]*store.SecretKey
	secrets map[string]*store.Secret
	n       int
}

func newMem() *memStore {
	return &memStore{keys: map[string]*store.SecretKey{}, secrets: map[string]*store.Secret{}}
}

func (m *memStore) SecretKeys(context.Context) ([]*store.SecretKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []*store.SecretKey
	for _, k := range m.keys {
		c := *k
		for _, s := range m.secrets {
			if s.KeyID == k.ID {
				c.Secrets++
			}
		}
		out = append(out, &c)
	}

	return out, nil
}

func (m *memStore) AddSecretKey(_ context.Context, k *store.SecretKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, o := range m.keys {
		if o.Active && o.Master != k.Master {
			return store.ErrConflict
		}
	}

	for _, o := range m.keys {
		o.Active = false
	}

	c := *k
	c.Active = true
	m.keys[k.ID] = &c
	m.n++

	return nil
}

func (m *memStore) RewrapSecretKeys(_ context.Context, keys []store.Rewrap) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(keys) != len(m.keys) {
		return store.ErrConflict
	}

	for _, k := range keys {
		if m.keys[k.ID] == nil || m.keys[k.ID].Master != k.From {
			return store.ErrConflict
		}
	}

	for _, k := range keys {
		m.keys[k.ID].Wrapped, m.keys[k.ID].Master = k.Wrapped, k.Master
	}

	m.n++

	return nil
}

func (m *memStore) ListSecrets(context.Context) ([]*store.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []*store.Secret
	for _, s := range m.secrets {
		c := *s
		out = append(out, &c)
	}

	return out, nil
}

func (m *memStore) GetSecret(_ context.Context, name string) (*store.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.secrets[name]; ok {
		c := *s

		return &c, nil
	}

	return nil, store.ErrNotFound
}

func (m *memStore) PutSecret(_ context.Context, x *store.Secret) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.n++

	if x.Value == nil {
		s, ok := m.secrets[x.Name]
		if !ok {
			return store.ErrNotFound
		}

		s.Description = x.Description

		return nil
	}

	c := *x
	m.secrets[x.Name] = &c

	return nil
}

func (m *memStore) DeleteSecret(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.secrets[name]; !ok {
		return store.ErrNotFound
	}

	delete(m.secrets, name)
	m.n++

	return nil
}

func (m *memStore) SecretsVersion(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return fmt.Sprint(m.n), nil
}

func TestNewMasterKey(t *testing.T) {
	a, b, empty := NewMasterKey("first"), NewMasterKey("second"), NewMasterKey("")
	if a.ID == b.ID || a.ID == empty.ID || len(a.ID) != 8 || len(a.key) != 32 || len(empty.key) != 32 {
		t.Fatalf("ids %q %q %q", a.ID, b.ID, empty.ID)
	}

	if NewMasterKey("first").ID != a.ID {
		t.Fatal("not stable")
	}
}

func TestVault(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	k1 := NewMasterKey("the first master key")

	v := NewVault(st, k1, map[string]string{"FROM_ENV": "env-value", "TOKEN": "env-token"})

	val := "s3cret"
	if err := v.Put(ctx, "TOKEN", "API token", &val, "alice"); err != nil {
		t.Fatal(err)
	}

	if got := v.Values(); got["TOKEN"] != "s3cret" || got["FROM_ENV"] != "env-value" {
		t.Fatalf("values %v", got)
	}

	// Stored encrypted.
	if x, _ := st.GetSecret(ctx, "TOKEN"); strings.Contains(string(x.Value), "s3cret") {
		t.Fatal("stored in clear")
	}

	// Another replica with the same key reads it.
	other := NewVault(st, k1, nil)
	if _, err := other.Load(ctx); err != nil || other.Values()["TOKEN"] != "s3cret" {
		t.Fatalf("other replica: %v %v", other.Values(), err)
	}

	// A replica with another key cannot, and says so.
	wrong := NewVault(st, NewMasterKey("a different master key"), nil)
	if _, err := wrong.Load(ctx); err != nil || wrong.Problem() == "" || wrong.Values()["TOKEN"] != "" || !wrong.Status().ConfigOutdated {
		t.Fatalf("wrong key: %v %q", wrong.Values(), wrong.Problem())
	}

	if err := wrong.Put(ctx, "OTHER", "", &val, "x"); err == nil {
		t.Fatal("stored with the wrong key")
	}

	// Description only keeps the value.
	if err := v.Put(ctx, "TOKEN", "renamed", nil, "bob"); err != nil || v.Values()["TOKEN"] != "s3cret" {
		t.Fatalf("description change: %v", err)
	}

	if err := v.Put(ctx, "1BAD", "", &val, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}

	// The key is changed from the UI: the data keys move to the new one.
	if _, err := v.ChangeKey(ctx, "the first master key"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("same key: %v", err)
	}

	if _, err := wrong.ChangeKey(ctx, "whatever"); err == nil {
		t.Fatal("a replica without the key changed it")
	}

	stats, err := v.ChangeKey(ctx, "")
	if err != nil || stats.Rewrapped != 1 || stats.From != k1.ID {
		t.Fatalf("change: %+v %v", stats, err)
	}

	if s := v.Status(); s.MasterID != stats.To || !s.ConfigOutdated || v.Values()["TOKEN"] != "s3cret" {
		t.Fatalf("after change: %+v %v", s, v.Values())
	}

	// A running replica keeps reading; new values go under the new key.
	if _, err := other.Load(ctx); err != nil || other.Values()["TOKEN"] != "s3cret" || other.Problem() != "" {
		t.Fatalf("running replica: %v %q", other.Values(), other.Problem())
	}

	val2 := "v2"
	if err := v.Put(ctx, "SECOND", "", &val2, "x"); err != nil {
		t.Fatal(err)
	}

	// Started again with the old key: cannot read. With the new one: can.
	if old := NewVault(st, k1, nil); func() bool { _, _ = old.Load(ctx); return old.Problem() == "" }() {
		t.Fatal("the old key still reads")
	}

	empty := NewVault(st, NewMasterKey(""), nil)
	if _, err := empty.Load(ctx); err != nil || empty.Values()["TOKEN"] != "s3cret" || empty.Values()["SECOND"] != "v2" ||
		empty.Problem() != "" || empty.Status().ConfigOutdated {
		t.Fatalf("new key: %v %q", empty.Values(), empty.Problem())
	}

	if err := v.Delete(ctx, "TOKEN"); err != nil || v.Values()["TOKEN"] != "env-token" {
		t.Fatalf("delete: %v", err)
	}
}

func TestChangeKeyWithoutSecrets(t *testing.T) {
	ctx := context.Background()
	st := newMem()

	v := NewVault(st, NewMasterKey("a"), nil)
	if _, err := v.ChangeKey(ctx, "b"); err != nil {
		t.Fatal(err)
	}

	// The new key is recorded, so a replica with the old one sees it is behind.
	old := NewVault(st, NewMasterKey("a"), nil)
	if _, err := old.Load(ctx); err != nil || !old.Status().ConfigOutdated || old.Status().StoredID != NewMasterKey("b").ID {
		t.Fatalf("status %+v %v", old.Status(), err)
	}
}

package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
		o.Active = false
	}

	c := *k
	c.Active = true
	m.keys[k.ID] = &c
	m.n++

	return nil
}

func (m *memStore) RewrapSecretKey(_ context.Context, id string, wrapped []byte, master string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.keys[id].Wrapped, m.keys[id].Master = wrapped, master
	m.n++

	return nil
}

func (m *memStore) DropUnusedSecretKeys(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var n int64

outer:
	for id, k := range m.keys {
		if k.Active || !k.CreatedAt.Before(before.Add(time.Second)) {
			continue
		}

		for _, s := range m.secrets {
			if s.KeyID == id {
				continue outer
			}
		}

		delete(m.keys, id)
		n++
	}

	m.n++

	return n, nil
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

func (m *memStore) ReencryptSecret(_ context.Context, name string, value []byte, keyID, prev string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.secrets[name]
	if !ok || s.KeyID != prev {
		return false, nil
	}

	s.Value, s.KeyID = value, keyID
	m.n++

	return true, nil
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

func master(t *testing.T, s string) MasterKey {
	t.Helper()

	k, err := ParseMasterKey(s)
	if err != nil {
		t.Fatal(err)
	}

	return k
}

func TestParseMasterKey(t *testing.T) {
	for _, ok := range []string{
		"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
		strings.Repeat("ab", 32),
		"a long passphrase here",
	} {
		if _, err := ParseMasterKey(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}

	for _, bad := range []string{"", "short"} {
		if _, err := ParseMasterKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}

	a, b := master(t, "first passphrase 123"), master(t, "second passphrase 456")
	if a.ID == b.ID || len(a.ID) != 8 {
		t.Fatalf("ids %q %q", a.ID, b.ID)
	}
}

func TestVault(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	k1 := master(t, "the first master key")

	v := NewVault(st, []MasterKey{k1}, map[string]string{"FROM_ENV": "env-value", "TOKEN": "env-token"})

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
	other := NewVault(st, []MasterKey{k1}, nil)
	if _, err := other.Load(ctx); err != nil || other.Values()["TOKEN"] != "s3cret" {
		t.Fatalf("other replica: %v %v", other.Values(), err)
	}

	// A replica with another key cannot, and says so.
	wrong := NewVault(st, []MasterKey{master(t, "a different master key")}, nil)
	if _, err := wrong.Load(ctx); err != nil || wrong.Problem() == "" || wrong.Values()["TOKEN"] != "" {
		t.Fatalf("wrong key: %v %q", wrong.Values(), wrong.Problem())
	}

	// Description only keeps the value.
	if err := v.Put(ctx, "TOKEN", "renamed", nil, "bob"); err != nil || v.Values()["TOKEN"] != "s3cret" {
		t.Fatalf("description change: %v", err)
	}

	if err := v.Put(ctx, "1BAD", "", &val, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}

	// Rotation: new data key, secrets moved to it, old key dropped.
	time.Sleep(10 * time.Millisecond)

	ks, _ := st.SecretKeys(ctx)
	oldKey := ks[0].ID

	stats, err := v.Rotate(ctx)
	if err != nil || stats.Reencrypted != 1 || stats.Key == oldKey {
		t.Fatalf("rotate: %+v %v", stats, err)
	}

	if x, _ := st.GetSecret(ctx, "TOKEN"); x.KeyID != stats.Key {
		t.Fatalf("secret on key %s, want %s", x.KeyID, stats.Key)
	}

	if _, err := other.Load(ctx); err != nil || other.Values()["TOKEN"] != "s3cret" {
		t.Fatal("after rotation the other replica lost the secret")
	}

	// New master key, the old one kept to open: rotation moves everything.
	k2 := master(t, "the second master key")
	v2 := NewVault(st, []MasterKey{k2, k1}, nil)

	if _, err := v2.Rotate(ctx); err != nil {
		t.Fatal(err)
	}

	only2 := NewVault(st, []MasterKey{k2}, nil)
	if _, err := only2.Load(ctx); err != nil || only2.Values()["TOKEN"] != "s3cret" || only2.Problem() != "" {
		t.Fatalf("after master change: %v %q", only2.Values(), only2.Problem())
	}

	// No master key: nothing can be stored.
	none := NewVault(st, nil, nil)
	if err := none.Put(ctx, "X", "", &val, "x"); !errors.Is(err, ErrNoMaster) {
		t.Fatalf("no master: %v", err)
	}

	if err := v2.Delete(ctx, "TOKEN"); err != nil || v2.Values()["TOKEN"] != "" {
		t.Fatalf("delete: %v", err)
	}
}

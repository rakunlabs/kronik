package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"
)

// Stored is one value changed from the UI.
type Stored struct {
	Value     json.RawMessage
	UpdatedAt time.Time
	UpdatedBy string
}

// Source is where the overrides live.
type Source interface {
	LoadSettings(ctx context.Context) (map[string]Stored, error)
	PutSettings(ctx context.Context, values map[string]json.RawMessage, reset []string, by string, now time.Time) error
}

// Manager keeps the settings in effect: the base (defaults and the
// configuration) with the overrides from the database on top. It reads the
// database every few seconds, so a change made on one replica reaches the
// others, and calls the listeners when the values change.
type Manager struct {
	src  Source
	base Values

	refresh chan struct{}

	mu        sync.RWMutex
	current   Values
	overrides map[string]Stored
	listeners []func(Values)
}

// refreshEvery is how often the overrides are read from the database.
const refreshEvery = 5 * time.Second

func NewManager(src Source, base Values) *Manager {
	return &Manager{src: src, base: base, current: base.Clone(), overrides: map[string]Stored{}, refresh: make(chan struct{}, 1)}
}

// Refresh makes Run read the overrides now.
func (m *Manager) Refresh() {
	select {
	case m.refresh <- struct{}{}:
	default:
	}
}

// Load reads the overrides once.
func (m *Manager) Load(ctx context.Context) error {
	ov, err := m.src.LoadSettings(ctx)
	if err != nil {
		return err
	}

	m.apply(ov)

	return nil
}

// Run reads the overrides until ctx ends: every few seconds, and at once
// when Refresh is called (another replica said they changed).
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.refresh:
		case <-t.C:
			if err := m.Load(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("read settings", "error", err)
			}
		}
	}
}

func (m *Manager) apply(ov map[string]Stored) {
	next := m.base.Clone()

	for k, s := range ov {
		d, ok := Find(k)
		if !ok {
			continue
		}

		v, err := d.Parse(s.Value)
		if err == nil {
			err = next.Set(k, v)
		}

		if err != nil {
			slog.Warn("setting ignored, its stored value is not valid", "key", k, "error", err)
		}
	}

	m.mu.Lock()
	changed := !reflect.DeepEqual(next, m.current)
	m.current = next
	m.overrides = ov
	listeners := append([]func(Values){}, m.listeners...)
	m.mu.Unlock()

	if changed {
		for _, fn := range listeners {
			fn(next)
		}
	}
}

// OnChange calls fn with the new values whenever they change.
func (m *Manager) OnChange(fn func(Values)) {
	m.mu.Lock()
	m.listeners = append(m.listeners, fn)
	m.mu.Unlock()
}

// Values are the settings in effect now.
func (m *Manager) Values() Values {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.current.Clone()
}

// Entry is one setting as the settings page shows it.
type Entry struct {
	Def
	Value json.RawMessage `json:"value"`
	// Base is the value of the configuration (or the default).
	Base json.RawMessage `json:"base"`
	// Overridden is set when the value was changed from the UI.
	Overridden bool       `json:"overridden"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	UpdatedBy  string     `json:"updated_by,omitempty"`
}

// Entries lists every setting with its value.
func (m *Manager) Entries() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Entry, 0, len(Defs))

	for _, d := range Defs {
		e := Entry{Def: d, Value: m.current.Get(d.Key), Base: m.base.Get(d.Key)}

		if ov, ok := m.overrides[d.Key]; ok {
			e.Overridden = true
			t := ov.UpdatedAt
			e.UpdatedAt = &t
			e.UpdatedBy = ov.UpdatedBy
		}

		out = append(out, e)
	}

	return out
}

// Change is a set of values to store and keys to reset to the base.
type Change struct {
	Values map[string]json.RawMessage `json:"values"`
	Reset  []string                   `json:"reset"`
}

// Apply checks and stores a change, and returns the keys that changed.
func (m *Manager) Apply(ctx context.Context, c Change, by string) ([]string, error) {
	values := make(map[string]json.RawMessage, len(c.Values))

	trial := m.Values()

	for k, raw := range c.Values {
		d, ok := Find(k)
		if !ok {
			return nil, fmt.Errorf("unknown setting %q", k)
		}

		v, err := d.Parse(raw)
		if err != nil {
			return nil, err
		}

		values[k] = v
		_ = trial.Set(k, v)
	}

	for _, k := range c.Reset {
		if _, ok := Find(k); !ok {
			return nil, fmt.Errorf("unknown setting %q", k)
		}

		_ = trial.Set(k, m.base.Get(k))
	}

	if err := trial.check(); err != nil {
		return nil, err
	}

	if err := m.src.PutSettings(ctx, values, c.Reset, by, time.Now().UTC().Truncate(time.Microsecond)); err != nil {
		return nil, err
	}

	if err := m.Load(ctx); err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(values)+len(c.Reset))
	for k := range values {
		keys = append(keys, k)
	}

	return append(keys, c.Reset...), nil
}

// check refuses a combination that cannot work.
func (v *Values) check() error {
	if v.CleanupZone != "" {
		if _, err := time.LoadLocation(v.CleanupZone); err != nil {
			return fmt.Errorf("unknown cleanup time zone %q", v.CleanupZone)
		}
	}

	if v.JSMaxDuration <= 0 {
		return fmt.Errorf("the longest script run must be positive")
	}

	if !v.HTTP && !v.JS && !v.Command && !v.Docker && !v.Swarm && !v.Kubernetes {
		return fmt.Errorf("turn on at least one executor")
	}

	if err := v.checkScheduling(); err != nil {
		return err
	}

	return nil
}

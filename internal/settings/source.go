package settings

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rakunlabs/kronik/internal/store"
)

// StoreSource reads and writes the overrides in the store.
type StoreSource struct{ Store *store.Store }

func (s StoreSource) LoadSettings(ctx context.Context) (map[string]Stored, error) {
	all, err := s.Store.Settings(ctx)
	if err != nil {
		return nil, err
	}

	out := make(map[string]Stored, len(all))
	for k, v := range all {
		out[k] = Stored{Value: v.Value, UpdatedAt: v.UpdatedAt, UpdatedBy: v.UpdatedBy}
	}

	return out, nil
}

func (s StoreSource) PutSettings(ctx context.Context, values map[string]json.RawMessage, reset []string, by string, now time.Time) error {
	return s.Store.PutSettings(ctx, values, reset, by, now)
}

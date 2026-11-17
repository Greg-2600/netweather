package board

import (
	"context"

	"github.com/Greg-2600/netweather/internal/store"
)

// StoreSource adapts the SQLite store to the UI's read model. It is the
// production Source; Memory remains for tests and demos.
type StoreSource struct {
	Store   *store.Store
	Vantage string
	Changes *Broadcast
}

// Snapshot returns the board as the store sees it.
func (s *StoreSource) Snapshot(ctx context.Context) ([]Target, error) {
	rows, err := s.Store.TargetsWithState(ctx, s.Vantage)
	if err != nil {
		return nil, err
	}

	out := make([]Target, 0, len(rows))
	for _, r := range rows {
		out = append(out, Target{
			ID:      r.ID,
			Name:    r.Name,
			Host:    r.Host,
			Sector:  r.Sector,
			Country: r.Country,
			State:   r.State,
			Since:   r.Since,
		})
	}
	return out, nil
}

// Subscribe implements Watcher. Without a broadcaster the board still renders;
// it simply does not receive live deltas.
func (s *StoreSource) Subscribe() (<-chan struct{}, func()) {
	if s.Changes == nil {
		ch := make(chan struct{})
		return ch, func() { close(ch) }
	}
	return s.Changes.Subscribe()
}

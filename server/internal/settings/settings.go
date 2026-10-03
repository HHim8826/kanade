// Package settings keeps what the settings page changes for the whole service (reviews #74, #75,
// #77): each group is stored in the settings table as JSON, checked when saved, and handed to
// whoever applies it, at once where it can be.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Resources are the service's disk budgets (plan §6). The serve flags of the same names, when
// given, win for that run.
type Resources struct {
	CacheMiB   int64 `json:"cache_mib"`   // the stream cache
	StagingMiB int64 `json:"staging_mib"` // downloads, uploads and import work together
	ReserveGiB int64 `json:"reserve_gib"` // free space kept on the disk
}

// Downloads is how BitTorrent downloads use the network and seed (decision D5).
type Downloads struct {
	DownKiB    int64 `json:"down_kib"`   // overall download speed limit, KiB/s; 0: none
	UpKiB      int64 `json:"up_kib"`     // overall upload speed limit, KiB/s; 0: none
	Concurrent int   `json:"concurrent"` // downloads fetching at the same time
	MaxPeers   int   `json:"max_peers"`  // connections of each torrent
	// Seeding once a download is complete: until either limit is reached (0: that one does not
	// apply); without Seed, not at all.
	Seed      bool    `json:"seed"`
	SeedRatio float64 `json:"seed_ratio"`
	SeedHours int     `json:"seed_hours"`
}

// Drive is how the library keeps up with Drive (P2-6, decision D6).
type Drive struct {
	AutoInbox     bool `json:"auto_inbox"`     // import new files of the inbox by itself
	CheckMinutes  int  `json:"check_minutes"`  // how often the change feed and inbox are looked at
	SettleMinutes int  `json:"settle_minutes"` // how long an inbox folder must go without new files
}

func DefaultResources() Resources { return Resources{CacheMiB: 512, StagingMiB: 2048, ReserveGiB: 4} }

func DefaultDownloads() Downloads {
	return Downloads{Concurrent: 1, MaxPeers: 30, Seed: true, SeedRatio: 1, SeedHours: 72}
}

func DefaultDrive() Drive { return Drive{AutoInbox: true, CheckMinutes: 10, SettleMinutes: 5} }

// ErrInvalid is a value out of range; the message says which.
var ErrInvalid = errors.New("invalid setting")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func (r Resources) Check() error {
	switch {
	case r.CacheMiB < 64 || r.CacheMiB > 1<<20:
		return invalid("the stream cache must be 64 MiB to 1 TiB")
	case r.StagingMiB < 256 || r.StagingMiB > 1<<24:
		return invalid("staging must be 256 MiB to 16 TiB")
	case r.ReserveGiB < 1 || r.ReserveGiB > 1<<14:
		return invalid("the free space to keep must be 1 to 16384 GiB")
	}
	return nil
}

func (d Downloads) Check() error {
	switch {
	case d.DownKiB < 0 || d.DownKiB > 1<<24 || d.UpKiB < 0 || d.UpKiB > 1<<24:
		return invalid("speed limits must be 0 (none) to 16 GiB/s")
	case d.Concurrent < 1 || d.Concurrent > 5:
		return invalid("1 to 5 downloads at a time")
	case d.MaxPeers < 1 || d.MaxPeers > 500:
		return invalid("1 to 500 connections per torrent")
	case d.SeedRatio < 0 || d.SeedRatio > 1000:
		return invalid("a share ratio of 0 (no limit) to 1000")
	case d.SeedHours < 0 || d.SeedHours > 24*365:
		return invalid("0 (no limit) to 8760 hours of seeding")
	}
	return nil
}

func (d Drive) Check() error {
	switch {
	case d.CheckMinutes < 1 || d.CheckMinutes > 24*60:
		return invalid("check every 1 to 1440 minutes")
	case d.SettleMinutes < 0 || d.SettleMinutes > 24*60:
		return invalid("wait 0 to 1440 minutes for an inbox folder to settle")
	}
	return nil
}

const (
	keyResources = "settings.resources"
	keyDownloads = "settings.downloads"
	keyDrive     = "settings.drive"
)

// Store reads and saves the settings; On* run after a save, with the saved value, to apply it.
type Store struct {
	DB *sql.DB

	mu          sync.Mutex
	OnResources func(Resources)
	OnDownloads func(Downloads)
	OnDrive     func(Drive)
}

// load reads a group over its defaults, so a field added later keeps its default.
func load[T any](ctx context.Context, d *sql.DB, key string, v T) (T, error) {
	raw, err := db.GetSetting(ctx, d, key)
	if err != nil || raw == "" {
		return v, err
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return v, fmt.Errorf("setting %s: %w", key, err)
	}
	return v, nil
}

func save(ctx context.Context, d *sql.DB, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return db.SetSetting(ctx, d, key, string(raw))
}

func (s *Store) Resources(ctx context.Context) (Resources, error) {
	return load(ctx, s.DB, keyResources, DefaultResources())
}

func (s *Store) Downloads(ctx context.Context) (Downloads, error) {
	return load(ctx, s.DB, keyDownloads, DefaultDownloads())
}

func (s *Store) Drive(ctx context.Context) (Drive, error) {
	return load(ctx, s.DB, keyDrive, DefaultDrive())
}

func (s *Store) SetResources(ctx context.Context, v Resources) error {
	return set(ctx, s, keyResources, v, v.Check, s.OnResources)
}

func (s *Store) SetDownloads(ctx context.Context, v Downloads) error {
	return set(ctx, s, keyDownloads, v, v.Check, s.OnDownloads)
}

func (s *Store) SetDrive(ctx context.Context, v Drive) error {
	return set(ctx, s, keyDrive, v, v.Check, s.OnDrive)
}

func set[T any](ctx context.Context, s *Store, key string, v T, check func() error, apply func(T)) error {
	if err := check(); err != nil {
		return err
	}
	s.mu.Lock() // saves apply in the order they are made
	defer s.mu.Unlock()
	if err := save(ctx, s.DB, key, v); err != nil {
		return err
	}
	if apply != nil {
		apply(v)
	}
	return nil
}

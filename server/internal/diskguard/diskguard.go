// Package diskguard watches free disk space (plan §6, P2-6). Below the reserve it first empties the
// stream cache; if that is not enough it pauses downloads and refuses new downloads and uploads.
// Data not yet in Drive is never deleted. Once space is back (with some margin) everything resumes
// by itself.
package diskguard

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

type Cache interface {
	Trim() int64
	SetLean(on bool)
}

type Downloads interface {
	SetLowDisk(on bool)
	PauseForDisk(ctx context.Context) (int, error)
	ResumeAfterDisk(ctx context.Context) (int, error)
}

type Uploads interface {
	SetLowDisk(on bool)
}

// margin is how far above the reserve free space must climb before the guard lets go, so it does
// not flap around the line.
const margin = 512 << 20

type Guard struct {
	Dir     string
	Reserve int64
	Free    func(dir string) int64 // free bytes, -1 when unknown
	Cache   Cache
	DL      Downloads
	UL      Uploads
	Log     *slog.Logger

	mu     sync.Mutex
	status Status
}

// Status is what the task page shows.
type Status struct {
	Low          bool  `json:"low"`
	FreeBytes    int64 `json:"free_bytes"`
	ReserveBytes int64 `json:"reserve_bytes"`
	Since        int64 `json:"since,omitempty"`  // when space got low
	Paused       int   `json:"paused,omitempty"` // downloads paused for it
	Stopped      bool  `json:"stopped"`          // downloads and uploads are held, not just the cache trimmed
}

func (g *Guard) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.status
}

// Run checks every 30 seconds until ctx ends.
func (g *Guard) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		g.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check looks at free space once and acts on it.
func (g *Guard) Check(ctx context.Context) {
	free := g.Free(g.Dir)
	if free < 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := &g.status
	st.FreeBytes, st.ReserveBytes = free, g.Reserve
	switch {
	case free < g.Reserve:
		if !st.Low {
			st.Low, st.Since = true, db.Now()
			g.Log.Warn("disk low", "free_mb", free>>20, "reserve_mb", g.Reserve>>20)
		}
		if freed := g.Cache.Trim(); freed > 0 {
			g.Log.Info("disk low: stream cache trimmed", "freed_mb", freed>>20)
			free = g.Free(g.Dir)
			st.FreeBytes = free
		}
		g.Cache.SetLean(true)
		if free < g.Reserve {
			g.DL.SetLowDisk(true)
			g.UL.SetLowDisk(true)
			n, err := g.DL.PauseForDisk(ctx)
			if err != nil {
				g.Log.Warn("disk low: pausing downloads", "err", err)
			}
			if n > 0 {
				g.Log.Warn("disk low: downloads paused", "count", n)
			}
			st.Paused += n
			st.Stopped = true
		}
	case free >= g.Reserve+margin || !st.Low:
		if st.Low {
			g.Log.Info("disk space back", "free_mb", free>>20)
		}
		*st = Status{FreeBytes: free, ReserveBytes: g.Reserve}
		g.Cache.SetLean(false)
		g.DL.SetLowDisk(false)
		g.UL.SetLowDisk(false)
		// Also after a restart: downloads the guard paused before it continue now.
		if n, err := g.DL.ResumeAfterDisk(ctx); err != nil {
			g.Log.Warn("resuming downloads", "err", err)
		} else if n > 0 {
			g.Log.Info("downloads resumed after low disk", "count", n)
		}
	}
}

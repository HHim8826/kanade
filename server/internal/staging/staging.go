// Package staging is the one staging budget (plan §6) that downloads, client uploads and the
// importer's work folder share, together with the filesystem's free-space reserve. Checking that
// something fits and recording that it is taken happen under one lock, so two requests can never
// both get the last free space (review #4).
package staging

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrOverBudget = errors.New("not enough staging space")
	ErrReserve    = errors.New("the disk would drop below its free-space reserve")
)

type Budget struct {
	Limit   int64              // bytes all users may hold or have promised together
	Reserve int64              // free space to keep on the filesystem
	Dir     string             // where the staging data lives (for free space)
	Free    func(string) int64 // free bytes on Dir's filesystem, -1 when unknown; nil: not checked

	mu    sync.Mutex
	users []func(context.Context) int64
	held  int64
}

// Use registers what a user holds or has promised (files on disk plus bytes still to come).
func (b *Budget) Use(committed func(context.Context) int64) {
	b.mu.Lock()
	b.users = append(b.users, committed)
	b.mu.Unlock()
}

func (b *Budget) usedLocked(ctx context.Context) int64 {
	n := b.held
	for _, u := range b.users {
		n += u(ctx)
	}
	return n
}

// Used is everything held or promised now.
func (b *Budget) Used(ctx context.Context) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.usedLocked(ctx)
}

// Request is one reservation.
type Request struct {
	Need int64
	// Record makes the reservation part of a user's committed bytes (an upload row, a download
	// round); it runs only when Need fits, with the lock held.
	Record func() error
	// MakeRoom may free space (stopping a finished seed) when Need does not fit; it reports
	// whether it freed anything, and is called again until Need fits or it frees nothing.
	MakeRoom func(ctx context.Context) (bool, error)
	// Alone allows a single item larger than the whole budget, when staging holds little else (a
	// quarter of the budget at most) and the disk has the room: a file bigger than the budget can
	// still be downloaded, by itself.
	Alone bool
}

// Take reserves r.Need if it fits the budget and the free-space reserve.
func (b *Budget) Take(ctx context.Context, r Request) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fitsLocked(ctx, r); err != nil {
		return err
	}
	if r.Record != nil {
		return r.Record()
	}
	return nil
}

func (b *Budget) fitsLocked(ctx context.Context, r Request) error {
	used := b.usedLocked(ctx)
	for used+r.Need > b.Limit && r.MakeRoom != nil {
		freed, err := r.MakeRoom(ctx)
		if err != nil {
			return err
		}
		if !freed {
			break
		}
		used = b.usedLocked(ctx)
	}
	if used+r.Need > b.Limit && !(r.Alone && used <= b.Limit/4) {
		if r.Need > b.Limit {
			return fmt.Errorf("%w: %d MB needed, more than the %d MB staging budget", ErrOverBudget, r.Need>>20, b.Limit>>20)
		}
		return fmt.Errorf("%w: %d MB in use or reserved, %d MB more needed, budget %d MB", ErrOverBudget, used>>20, r.Need>>20, b.Limit>>20)
	}
	if b.Free != nil {
		if free := b.Free(b.Dir); free >= 0 && free-r.Need < b.Reserve {
			return fmt.Errorf("%w (%d GB)", ErrReserve, b.Reserve>>30)
		}
	}
	return nil
}

// Hold reserves need until release is called, for data about to be written that no user counts
// yet (FFmpeg output, an archive being unpacked). Release once the data is on disk, where its user
// counts it, or gone.
func (b *Budget) Hold(ctx context.Context, need int64) (release func(), err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fitsLocked(ctx, Request{Need: need}); err != nil {
		return nil, err
	}
	b.held += need
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.held -= need
			b.mu.Unlock()
		})
	}, nil
}

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
	Limit   int64              // bytes all users may hold or have promised together (set before use; then SetLimits)
	Reserve int64              // free space to keep on the filesystem
	Dir     string             // where the staging data lives (for free space)
	Free    func(string) int64 // free bytes on Dir's filesystem, -1 when unknown; nil: not checked
	// Reclaim may free space for a request that does not fit (stopping a finished seed); it reports
	// whether it freed anything, and is called again until the request fits or it frees nothing.
	Reclaim func(ctx context.Context) (bool, error)

	mu    sync.Mutex
	users []func(context.Context) Usage
	held  int64
}

// Usage is what one user of the budget has: bytes on disk, and bytes promised that are not on disk
// yet (the rest of an upload, what a download round still has to fetch).
type Usage struct{ Disk, Pending int64 }

// Use registers a user's usage.
func (b *Budget) Use(u func(context.Context) Usage) {
	b.mu.Lock()
	b.users = append(b.users, u)
	b.mu.Unlock()
}

// OnDisk is the usage of a user whose bytes are all on disk.
func OnDisk(f func(context.Context) int64) func(context.Context) Usage {
	return func(ctx context.Context) Usage { return Usage{Disk: f(ctx)} }
}

// usageLocked is everything held or promised, and the part of it not written yet: held space and
// promises do not show in the filesystem's free space (review #4).
func (b *Budget) usageLocked(ctx context.Context) (used, unwritten int64) {
	used, unwritten = b.held, b.held
	for _, u := range b.users {
		n := u(ctx)
		used += n.Disk + n.Pending
		unwritten += n.Pending
	}
	return used, unwritten
}

// Limits are the budget and the free-space reserve now.
func (b *Budget) Limits() (limit, reserve int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Limit, b.Reserve
}

// SetLimits changes the budget and the reserve (review #74). What is held or promised stays; new
// requests are measured against the new values, and wait when what is held is already more.
func (b *Budget) SetLimits(limit, reserve int64) {
	b.mu.Lock()
	b.Limit, b.Reserve = limit, reserve
	b.mu.Unlock()
}

// Used is everything held or promised now.
func (b *Budget) Used(ctx context.Context) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	used, _ := b.usageLocked(ctx)
	return used
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
	// still be downloaded, or converted, by itself.
	Alone bool
	// Own is what the requester already has in staging and that does not count as something else
	// (the file being converted), for Alone.
	Own int64
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
	used, unwritten := b.usageLocked(ctx)
	for _, makeRoom := range []func(context.Context) (bool, error){r.MakeRoom, b.Reclaim} {
		for used+r.Need > b.Limit && makeRoom != nil {
			freed, err := makeRoom(ctx)
			if err != nil {
				return err
			}
			if !freed {
				break
			}
			used, unwritten = b.usageLocked(ctx)
		}
	}
	if used+r.Need > b.Limit && !(r.Alone && used-r.Own <= b.Limit/4) {
		if r.Need > b.Limit {
			return fmt.Errorf("%w: %d MB needed, more than the %d MB staging budget", ErrOverBudget, r.Need>>20, b.Limit>>20)
		}
		return fmt.Errorf("%w: %d MB in use or reserved, %d MB more needed, budget %d MB", ErrOverBudget, used>>20, r.Need>>20, b.Limit>>20)
	}
	// What is promised but not written yet will take free space too.
	if b.Free != nil {
		if free := b.Free(b.Dir); free >= 0 && free-unwritten-r.Need < b.Reserve {
			return fmt.Errorf("%w (%d GB)", ErrReserve, b.Reserve>>30)
		}
	}
	return nil
}

// Hold reserves need until release is called, for data about to be written that no user counts
// yet (FFmpeg output, an archive being unpacked). Release once the data is on disk, where its user
// counts it, or gone.
func (b *Budget) Hold(ctx context.Context, need int64) (release func(), err error) {
	return b.HoldRequest(ctx, Request{Need: need})
}

// HoldRequest is Hold for r.Need, with r.Alone and r.Own (r.Record is not used).
func (b *Budget) HoldRequest(ctx context.Context, r Request) (release func(), err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.fitsLocked(ctx, r); err != nil {
		return nil, err
	}
	need := r.Need
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

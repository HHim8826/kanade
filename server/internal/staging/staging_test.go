package staging

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// Two requests that each fit alone never both get in (review #4).
func TestConcurrentReservationsStayWithinBudget(t *testing.T) {
	ctx := context.Background()
	var committed atomic.Int64
	b := &Budget{Limit: 1000}
	b.Use(OnDisk(func(context.Context) int64 { return committed.Load() }))
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Take(ctx, Request{Need: 700, Record: func() error { committed.Add(700); return nil }}) == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || committed.Load() != 700 {
		t.Fatalf("granted %d, committed %d", ok.Load(), committed.Load())
	}
}

func TestHoldMakeRoomAloneAndReserve(t *testing.T) {
	ctx := context.Background()
	var used int64 = 600
	free := int64(10 << 30)
	b := &Budget{Limit: 1000, Reserve: 4 << 30, Free: func(string) int64 { return free }}
	b.Use(OnDisk(func(context.Context) int64 { return used }))
	release, err := b.Hold(ctx, 300)
	if err != nil || b.Used(ctx) != 900 {
		t.Fatalf("hold: %v %d", err, b.Used(ctx))
	}
	if err := b.Take(ctx, Request{Need: 200}); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("over the budget with a hold: %v", err)
	}
	release()
	release() // once only
	if b.Used(ctx) != 600 {
		t.Fatalf("after release %d", b.Used(ctx))
	}
	// Room is made by the caller's callback until the request fits.
	calls := 0
	err = b.Take(ctx, Request{Need: 600, MakeRoom: func(context.Context) (bool, error) { calls++; used -= 200; return true, nil }})
	if err != nil || calls != 1 {
		t.Fatalf("make room: %v after %d", err, calls)
	}
	// Larger than the budget: only alone, and only with the disk room.
	if err := b.Take(ctx, Request{Need: 5000, Alone: true}); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("alone with others in staging: %v", err)
	}
	used = 250
	if err := b.Take(ctx, Request{Need: 5000, Alone: true}); err != nil {
		t.Fatalf("alone: %v", err)
	}
	free = 4<<30 + 100
	if err := b.Take(ctx, Request{Need: 200}); !errors.Is(err, ErrReserve) {
		t.Fatalf("reserve: %v", err)
	}
}

// Promises not written yet count against the free-space reserve: two reservations cannot both
// use the same free space (review #4).
func TestReserveCountsUnwrittenPromises(t *testing.T) {
	ctx := context.Background()
	var pending int64
	b := &Budget{Limit: 2 << 30, Reserve: 4 << 30, Free: func(string) int64 { return 5 << 30 }}
	b.Use(func(context.Context) Usage { return Usage{Disk: 0, Pending: pending} })
	take := func() error {
		return b.Take(ctx, Request{Need: 800 << 20, Record: func() error { pending += 800 << 20; return nil }})
	}
	if err := take(); err != nil {
		t.Fatal(err)
	}
	if err := take(); !errors.Is(err, ErrReserve) {
		t.Fatalf("second promise of the same free space: %v", err)
	}
	// A hold not written yet counts too.
	pending = 0
	release, err := b.Hold(ctx, 800<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Hold(ctx, 800<<20); !errors.Is(err, ErrReserve) {
		t.Fatalf("second hold: %v", err)
	}
	release()
	// What is on disk already shows in the free space and is not subtracted twice.
	disk := int64(900 << 20)
	b2 := &Budget{Limit: 2 << 30, Reserve: 4 << 30, Free: func(string) int64 { return 5<<30 - disk }}
	b2.Use(func(context.Context) Usage { return Usage{Disk: disk} })
	if err := b2.Take(ctx, Request{Need: 100 << 20}); err != nil {
		t.Fatalf("written data subtracted twice: %v", err)
	}
}

// Reclaim frees space for holds as well as reservations; a conversion larger than the budget may
// run alone, its own source not counting as something else.
func TestReclaimAndHoldAlone(t *testing.T) {
	ctx := context.Background()
	used := int64(900)
	b := &Budget{Limit: 1000}
	b.Use(OnDisk(func(context.Context) int64 { return used }))
	b.Reclaim = func(context.Context) (bool, error) {
		if used < 500 {
			return false, nil
		}
		used -= 400
		return true, nil
	}
	release, err := b.Hold(ctx, 300)
	if err != nil || used != 500 {
		t.Fatalf("reclaim for a hold: %v %d", err, used)
	}
	release()
	b.Reclaim = nil
	used = 600 // the source being converted
	if _, err := b.HoldRequest(ctx, Request{Need: 1500, Alone: true}); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("alone, counting its own source as others: %v", err)
	}
	if _, err := b.HoldRequest(ctx, Request{Need: 1500, Alone: true, Own: 600}); err != nil {
		t.Fatalf("alone with its own source: %v", err)
	}
}

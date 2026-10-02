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
	b.Use(func(context.Context) int64 { return committed.Load() })
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
	b.Use(func(context.Context) int64 { return used })
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

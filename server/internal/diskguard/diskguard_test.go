package diskguard

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

type fake struct {
	free             int64
	trimFrees        int64
	lean, dlLow, low bool
	paused, resumed  int
	diskPaused       int // left paused by a previous run
}

func (f *fake) Trim() int64 {
	f.free += f.trimFrees
	n := f.trimFrees
	f.trimFrees = 0
	return n
}
func (f *fake) SetLean(on bool)    { f.lean = on }
func (f *fake) SetLowDisk(on bool) { f.dlLow, f.low = on, on }
func (f *fake) PauseForDisk(context.Context) (int, error) {
	f.paused++
	return 2, nil
}
func (f *fake) ResumeAfterDisk(context.Context) (int, error) {
	f.resumed++
	n := f.diskPaused
	f.diskPaused = 0
	return n, nil
}
func (f *fake) DiskPaused(context.Context) (int, error) { return f.diskPaused, nil }

func newGuard(f *fake) *Guard {
	return &Guard{Dir: "/", Reserve: 4 << 30, Free: func(string) int64 { return f.free }, Cache: f, DL: f, UL: f,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// A guard started after a restart with downloads still paused for disk keeps holding them until
// space is back past the margin, like the guard that paused them would have.
func TestRestartKeepsMargin(t *testing.T) {
	ctx := context.Background()
	f := &fake{free: 4<<30 + 100<<20, diskPaused: 3}
	g := newGuard(f)
	g.Check(ctx)
	if st := g.Status(); !st.Low || !st.Stopped || !f.dlLow || !f.lean || f.resumed != 0 {
		t.Fatalf("let go after a restart inside the margin: %+v %+v", st, f)
	}
	f.free = 6 << 30
	g.Check(ctx)
	if st := g.Status(); st.Low || f.dlLow || f.resumed != 1 || f.diskPaused != 0 {
		t.Fatalf("recovered: %+v %+v", st, f)
	}
	// A normal first start with nothing paused does not hold anything.
	f2 := &fake{free: 4<<30 + 100<<20}
	g2 := newGuard(f2)
	g2.Check(ctx)
	if st := g2.Status(); st.Low || f2.dlLow {
		t.Fatalf("fresh start: %+v %+v", st, f2)
	}
}

func TestGuard(t *testing.T) {
	ctx := context.Background()
	f := &fake{free: 10 << 30}
	g := newGuard(f)
	g.Check(ctx)
	if g.Status().Low || f.lean || f.resumed != 1 {
		t.Fatalf("plenty of space: %+v %+v", g.Status(), f)
	}
	// Low, but trimming the cache is enough: downloads go on.
	f.free, f.trimFrees = 3<<30, 2<<30
	g.Check(ctx)
	if st := g.Status(); !st.Low || st.Stopped || !f.lean || f.paused != 0 || f.dlLow {
		t.Fatalf("trim was enough: %+v %+v", st, f)
	}
	// Still low after trimming: downloads paused, new ones refused.
	f.free = 3 << 30
	g.Check(ctx)
	if st := g.Status(); !st.Stopped || st.Paused != 2 || !f.dlLow || f.paused != 1 {
		t.Fatalf("stopped: %+v %+v", st, f)
	}
	// Just above the reserve is not enough to let go (margin) ...
	f.free = 4<<30 + 100<<20
	g.Check(ctx)
	if !g.Status().Low || !f.dlLow {
		t.Fatal("let go inside the margin")
	}
	// ... well above it is.
	f.free = 6 << 30
	g.Check(ctx)
	if st := g.Status(); st.Low || f.dlLow || f.lean || f.resumed != 2 {
		t.Fatalf("recovered: %+v %+v", st, f)
	}
}

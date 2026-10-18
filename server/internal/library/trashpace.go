package library

import (
	"context"
	"strings"
	"sync"
	"time"
)

// TrashPacer moves files owed to the trash a location at a time (review #245): a file is given at
// most Wait, so that a location that does not answer holds up the others for that long; a storage
// location whose try failed is not tried again before a while (a minute, doubling, at most an
// hour), and one known not to be reachable now not before a minute: its files wait their turn, put
// off in the database (review #253: left due, they were the first of each batch again, and those
// after them never had a turn). Drive's files wait each their own (TrashFailed). The zero value is
// ready to use.
type TrashPacer struct {
	Wait time.Duration // the most a file is given; 0 is two minutes

	mu   sync.Mutex
	held map[int64]pace // a store -> when it is tried again
}

type pace struct {
	until time.Time
	fails int
}

const (
	trashGive    = 2 * time.Minute
	trashHoldMin = time.Minute
	trashHoldMax = time.Hour
)

// Trash moves refs to their trash with trash, as TrashFile does, those of a location held back
// skipped, or of a store offline says is known not to be reachable now (its last check; nil: none).
// It returns how many went, and when the first location held back that had files here is tried
// again (zero: none).
func (p *TrashPacer) Trash(ctx context.Context, lib *Store, refs []string, trash func(context.Context, string) error,
	offline func(ctx context.Context, store int64) bool) (done int, next time.Time) {
	give := p.Wait
	if give <= 0 {
		give = trashGive
	}
	put := map[int64]bool{} // stores whose files were put off
	held := func(ref string, store int64, until time.Time) {
		if next.IsZero() || until.Before(next) {
			next = until
		}
		if !put[store] {
			put[store] = true
			lib.putOffTrash(ctx, ref, until)
		}
	}
	for _, ref := range refs {
		if ctx.Err() != nil {
			return done, next
		}
		store, _ := refStore(ref)
		if until, ok := p.heldUntil(store); ok {
			held(ref, store, until)
			continue
		}
		if store != 0 && offline != nil && offline(ctx, store) { // looked at again in a minute
			held(ref, store, p.hold(store, false))
			continue
		}
		tctx, cancel := context.WithTimeout(ctx, give)
		n, _ := lib.TrashFile(tctx, ref, trash)
		cancel()
		switch n {
		case Trashed:
			done++
			p.clear(store)
		case TrashFailedTry:
			if store == 0 { // Drive: each file waits its own turn, as it did
				continue
			}
			held(ref, store, p.hold(store, true))
		}
	}
	return done, next
}

// putOffTrash puts off until then the files owed to the trash in the storage location of ref, those
// due before.
func (s *Store) putOffTrash(ctx context.Context, ref string, until time.Time) {
	kind, _, _ := strings.Cut(ref, ":")
	store, ok := refStore(ref)
	if !ok {
		return
	}
	from, to := refRange(kind, store)
	s.db.ExecContext(ctx, `UPDATE file_trash SET next_at = ?3 WHERE `+likeRefs("file_id")+` AND done_at = 0 AND next_at < ?3`,
		from, to, until.UnixMilli())
}

func (p *TrashPacer) heldUntil(store int64) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.held[store]
	if !ok || !time.Now().Before(h.until) {
		return time.Time{}, false
	}
	return h.until, true
}

// hold holds store back, and says until when: after a failed try longer each time in a row, else
// a minute.
func (p *TrashPacer) hold(store int64, failed bool) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil {
		p.held = map[int64]pace{}
	}
	h := p.held[store]
	h.until = time.Now().Add(trashHoldMin)
	if failed {
		h.until = time.Now().Add(min(trashHoldMin<<min(h.fails, 6), trashHoldMax))
		h.fails++
	}
	p.held[store] = h
	return h.until
}

func (p *TrashPacer) clear(store int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.held, store)
}

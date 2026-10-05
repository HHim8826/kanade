package loudness

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/staging"
)

func waitScan(t *testing.T, s *Service) ScanState {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for s.State().Running && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return s.State()
}

// The library is scanned by itself only once that is turned on (review #155); turned off, a scan
// running stops and none starts again.
func TestScanByItselfOnlyWhenOn(t *testing.T) {
	s, lib, _, add := setup(t)
	old := firstScan
	firstScan = 10 * time.Millisecond
	defer func() { firstScan = old }()
	s.Every = time.Hour
	id := add("a.flac", "d1", testdata(t, "tone.flac"))
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	cancel()
	if s.Auto(context.Background()) != AutoUnset || len(measured(t, lib, id)) != 0 {
		t.Fatal("scanned before anyone asked")
	}

	if err := s.SetAuto(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if st := waitScan(t, s); st.Done != 1 || s.Auto(context.Background()) != AutoOn {
		t.Fatalf("turned on: %+v", st)
	}
	if err := s.SetAuto(context.Background(), false); err != nil || s.Auto(context.Background()) != AutoOff {
		t.Fatalf("turned off: %v", err)
	}
}

// failing serves every file with an error.
type failing struct{ err error }

func (f failing) OpenRange(context.Context, string, int64, int64) (*http.Response, error) {
	return nil, f.err
}

// counting counts the records logged at Warn and above.
type counting struct {
	slog.Handler
	n *int
}

func (c counting) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		*c.n++
	}
	return nil
}

// Drive not connected ends a scan at the first file, and Drive failing file after file ends it
// after a few, each with one line in the log (review #155).
func TestDriveDownEndsTheScan(t *testing.T) {
	for _, c := range []struct {
		err    error
		failed int
		reason string
	}{
		{gdrive.ErrNotConnected, 1, "not_connected"},
		{io.ErrUnexpectedEOF, maxDriveFailures, "drive"},
	} {
		s, _, _, add := setup(t)
		for i := range 20 {
			add(filepath.Join(string(rune('a'+i))+".flac"), string(rune('a'+i)), testdata(t, "tone.flac"))
		}
		warns := 0
		s.Log = slog.New(counting{slog.NewTextHandler(io.Discard, nil), &warns})
		s.Source = failing{c.err}
		s.Scan(false)
		st := waitScan(t, s)
		if st.Failed != c.failed || st.Done != 0 || st.Reason != c.reason || warns != 1 {
			t.Fatalf("%v: %+v, %d warnings", c.err, st, warns)
		}
	}
}

// slowDrive holds every file until let go.
type slowDrive struct {
	data   []byte
	opened chan struct{}
	go_    chan struct{}
}

type held struct {
	r   io.Reader
	go_ chan struct{}
}

func (h held) Read(p []byte) (int, error) {
	<-h.go_
	return h.r.Read(p)
}

func (d *slowDrive) OpenRange(context.Context, string, int64, int64) (*http.Response, error) {
	select {
	case d.opened <- struct{}{}:
	default:
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(held{bytes.NewReader(d.data), d.go_})}, nil
}

// A song being imported is measured while the scan waits for Drive (review #145).
func TestImportDoesNotWaitForDrive(t *testing.T) {
	s, lib, _, add := setup(t)
	add("a.flac", "d1", testdata(t, "tone.flac"))
	slow := &slowDrive{data: testdata(t, "tone.flac"), opened: make(chan struct{}, 1), go_: make(chan struct{})}
	s.Source = slow
	s.Scan(false)
	<-slow.opened
	imported := add("i.mp3", "di", testdata(t, "tone-vbr.mp3"))
	done := make(chan struct{})
	go func() {
		s.File(context.Background(), imported, filepath.Join("..", "media", "testdata", "tone-vbr.mp3"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the import waited for the scan")
	}
	if len(measured(t, lib, imported)) != 1 {
		t.Fatal("not measured")
	}
	close(slow.go_)
	waitScan(t, s)
}

// A copy that would leave the disk below its reserve is not made (review #144): the file waits for
// another scan.
func TestCopyTakesStagingRoom(t *testing.T) {
	s, lib, _, add := setup(t)
	m4a := add("a.m4a", "d1", testdata(t, "tone.m4a"))
	s.Budget = &staging.Budget{Limit: 1 << 30, Reserve: 1 << 30, Dir: s.Temp, Free: func(string) int64 { return 0 }}
	s.Scan(false)
	st := waitScan(t, s)
	if st.Reason != "room" || st.Failed != 0 || len(measured(t, lib, m4a)) != 0 {
		t.Fatalf("no room: %+v", st)
	}
	if u, _ := lib.Unmeasured(context.Background(), 0, 10, false); len(u) != 1 {
		t.Fatalf("left for another scan: %+v", u)
	}
	s.Budget.Free = func(string) int64 { return 4 << 30 }
	s.Scan(false)
	if st := waitScan(t, s); st.Done != 1 || s.Budget.Used(context.Background()) != 0 {
		t.Fatalf("with room: %+v, %d held", st, s.Budget.Used(context.Background()))
	}
}

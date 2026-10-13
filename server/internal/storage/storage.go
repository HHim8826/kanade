// Package storage keeps the library's files in places besides Google Drive (#95, #99, #100): a
// folder on the server (or a NAS mounted there), an S3-compatible bucket such as Cloudflare R2, a
// OneDrive. Each place is a store, either managed (Kanade keeps the files it imports there, under
// library/, covers/ and sidecars/, as it does in Drive) or a source (a collection that is already
// there: imported where it is, and never written to).
//
// A library file's place is a reference (assets.file_ref, covers, sidecars): a Drive file ID as
// before, or "<kind>:<store id>:<key>". The Set reads every kind of reference, so the stream cache,
// lite mode, loudness and covers read every place the same way.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/HHim8826/kanade/server/internal/gdrive"
)

const (
	KindDrive    = "drive"
	KindLocal    = "local"
	KindS3       = "s3"
	KindOneDrive = "onedrive"

	// Managed: Kanade keeps imported files there. Source: a collection that is already there.
	Managed = "managed"
	Source  = "source"

	// DriveID is how Google Drive is named where a store's ID is expected (the default location,
	// an upload's destination).
	DriveID = "drive"
)

// Ref is the reference to a store's file.
func Ref(kind string, store int64, key string) string {
	return kind + ":" + strconv.FormatInt(store, 10) + ":" + key
}

// Parse splits a reference made by Ref; ok is false for a Drive file ID (and for "").
func Parse(ref string) (kind string, store int64, key string, ok bool) {
	kind, rest, found := strings.Cut(ref, ":")
	if !found {
		return "", 0, "", false
	}
	id, key, found := strings.Cut(rest, ":")
	if !found {
		return "", 0, "", false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || key == "" {
		return "", 0, "", false
	}
	return kind, n, key, true
}

// IsDrive says whether a reference is a Drive file ID (Drive's IDs never hold a colon).
func IsDrive(ref string) bool { return ref != "" && !strings.Contains(ref, ":") }

// Object is a file in a store.
type Object struct {
	Key     string
	Path    string // below the root, slash-separated
	Size    int64
	Version string // changes when the content may have: the modification time, an ETag, a cTag
	Hash    string // the place's own checksum when it keeps one (OneDrive's quickXorHash); "" otherwise
}

// Usage is the room a store has, as far as it says: 0 is not known.
type Usage struct {
	Total int64 `json:"total,omitempty"`
	Free  int64 `json:"free,omitempty"`
	Used  int64 `json:"used,omitempty"`  // what is kept there (a bucket, which has no size of its own)
	Files int   `json:"files,omitempty"` // and in how many files
}

// Sessions keeps an upload's resumable session (one per file being imported) across restarts.
// *gdrive.Client's uploads take the same.
type Sessions interface {
	LoadSession(ctx context.Context) (string, error)
	SaveSession(ctx context.Context, session string) error
	DeleteSession(ctx context.Context) error
}

// PutFile is a local file to keep in a managed store, at Dir/Name (or a name next to it, when that
// is taken by other content).
type PutFile struct {
	Path     string // the local file
	Dir      string // "library/<album artist>/<album>", "covers", "sidecars/..."
	Name     string
	MIME     string
	Size     int64
	SHA256   string // hex: what the stored copy is checked against
	Sessions Sessions
	Progress func(sent, total int64)
}

// Backend reads a store.
type Backend interface {
	// Open reads bytes [start, end] of key; end < 0 reads to the end.
	Open(ctx context.Context, key string, start, end int64) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (Object, error)
	// Walk calls fn for every file below the root, Kanade's own folders (trash, temporary) left out.
	Walk(ctx context.Context, fn func(Object) error) error
	// Check says whether the store can be used now (nil), and how much room it has.
	Check(ctx context.Context) (Usage, error)
}

// Writer is what a managed store adds.
type Writer interface {
	// Put keeps f and returns its key once the stored copy is known to hold f's bytes.
	Put(ctx context.Context, f PutFile) (key string, err error)
	// Trash moves key to the store's trash (or deletes it where there is none); a file already gone
	// is no error.
	Trash(ctx context.Context, key string) error
}

var (
	// ErrOffline is a store that cannot be reached now: a folder not mounted, a network down. Its
	// files are not marked missing for it.
	ErrOffline = errors.New("the storage location cannot be reached")
	// ErrNotFound is a file the store does not have.
	ErrNotFound = errors.New("the file is not in the storage location")
	// ErrAuth is a store that refuses the keys or the sign-in it has.
	ErrAuth = errors.New("the storage location refused its keys or sign-in")
	// ErrNoRoom is a store without the space a file needs.
	ErrNoRoom = errors.New("not enough free space in the storage location")
	// ErrNoStore is a reference to a store that was removed, or a destination that is not there.
	ErrNoStore = errors.New("no such storage location")
	// ErrReadOnly is a write to a source.
	ErrReadOnly = errors.New("this storage location is a source: Kanade does not write to it")
)

// Error is a store's failure, with the store's name for people to read.
type Error struct {
	Store string
	Err   error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %v", e.Store, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// offline wraps why a store cannot be reached.
func offline(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrOffline, fmt.Sprintf(format, a...))
}

// Explain names why reading a file failed, for a player to say: for a store's, offline,
// store_auth, gone or store; for Drive's, what gdrive.Explain says. lasting when trying again soon
// cannot help.
func Explain(err error) (reason string, lasting bool) {
	var e *Error
	switch {
	case errors.Is(err, ErrOffline):
		return "offline", false
	case errors.Is(err, ErrAuth), errors.Is(err, ErrNotSignedIn):
		return "store_auth", true
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoStore):
		return "gone", true
	case errors.As(err, &e):
		return "store", false
	}
	return gdrive.Explain(err)
}

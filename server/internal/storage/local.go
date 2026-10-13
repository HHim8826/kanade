package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A folder on the server, or a NAS mounted there (#99). Files are read and written through os.Root,
// so no key reaches outside the folder, symbolic links included.
//
// A folder that is not there now must not look like one whose files were all deleted, and a NAS
// that is not mounted must not be written to on the server's own disk: a folder that was a mount
// point when it was added has to be one still, a managed folder has to hold the mark Kanade left in
// it (.kanade-store), and a source that is empty is offline.

// LocalConfig is a folder store's config.
type LocalConfig struct {
	Root  string `json:"root"`
	Mount bool   `json:"mount,omitempty"` // the root was a mount point when added: it must still be one
	Mark  string `json:"mark,omitempty"`  // a managed folder: the ID in its .kanade-store file
}

const (
	markFile    = ".kanade-store"
	trashDir    = ".kanade-trash" // a managed folder's trash, emptied after trashDays
	tempDir     = ".kanade-tmp"   // files being written, renamed into place once checked
	trashDays   = 30
	smallMargin = 256 << 20 // free space a write leaves on a disk that is not the data directory's
)

type Local struct {
	name    string
	mode    string
	cfg     LocalConfig
	reserve func(dir string) int64 // the free space a write must leave there
}

func newLocal(name, mode string, cfg LocalConfig, reserve func(string) int64) *Local {
	return &Local{name: name, mode: mode, cfg: cfg, reserve: reserve}
}

// isMount says whether dir is a mount point: on another device than the folder above it.
func isMount(dir string) (bool, error) {
	var st, up syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		return false, err
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return true, nil // the root of the file system
	}
	if err := syscall.Stat(parent, &up); err != nil {
		return false, err
	}
	return st.Dev != up.Dev, nil
}

// sameDevice says whether a and b are on the same file system.
func sameDevice(a, b string) bool {
	var x, y syscall.Stat_t
	return syscall.Stat(a, &x) == nil && syscall.Stat(b, &y) == nil && x.Dev == y.Dev
}

func freeSpace(dir string) (free, total int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1, 0
	}
	return int64(st.Bavail) * st.Bsize, int64(st.Blocks) * st.Bsize
}

// PrepareLocal checks a folder about to be added as a store and returns its config: it must exist
// and be readable, and a managed one writable, where it gets its mark (an existing mark is kept,
// so a folder added again is the same store's).
func PrepareLocal(root, mode string) (LocalConfig, error) {
	if !filepath.IsAbs(root) {
		return LocalConfig{}, errors.New("the folder must be an absolute path (starting with /)")
	}
	root = filepath.Clean(root)
	st, err := os.Stat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return LocalConfig{}, fmt.Errorf("%s does not exist; create it (or mount the NAS there) first", root)
	}
	if err != nil {
		return LocalConfig{}, err
	}
	if !st.IsDir() {
		return LocalConfig{}, fmt.Errorf("%s is not a folder", root)
	}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != root {
		return LocalConfig{}, fmt.Errorf("%s is a symbolic link; add the folder it points to (%s)", root, real)
	}
	f, err := os.Open(root)
	if err != nil {
		return LocalConfig{}, fmt.Errorf("cannot read %s: %w", root, err)
	}
	f.Close()
	cfg := LocalConfig{Root: root}
	if cfg.Mount, err = isMount(root); err != nil {
		return LocalConfig{}, err
	}
	if mode != Managed {
		return cfg, nil
	}
	if b, err := os.ReadFile(filepath.Join(root, markFile)); err == nil {
		var m struct{ ID string }
		if json.Unmarshal(b, &m) == nil && m.ID != "" {
			cfg.Mark = m.ID
		}
	}
	if cfg.Mark == "" {
		cfg.Mark = randomID()
		b, _ := json.Marshal(map[string]string{"id": cfg.Mark,
			"note": "Kanade keeps music here. This file tells it the folder is there (mounted); do not delete it."})
		if err := os.WriteFile(filepath.Join(root, markFile), b, 0o644); err != nil {
			return LocalConfig{}, fmt.Errorf("cannot write to %s: %w", root, err)
		}
	}
	return cfg, nil
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// available says whether the folder is there as it was added (nil), without reading it all.
func (l *Local) available() error {
	st, err := os.Stat(l.cfg.Root)
	if err != nil {
		return offline("%s is not there", l.cfg.Root)
	}
	if !st.IsDir() {
		return offline("%s is not a folder", l.cfg.Root)
	}
	if l.cfg.Mount {
		if m, err := isMount(l.cfg.Root); err != nil || !m {
			return offline("nothing is mounted at %s", l.cfg.Root)
		}
	}
	if l.mode == Managed {
		b, err := os.ReadFile(filepath.Join(l.cfg.Root, markFile))
		if err != nil {
			return offline("%s has no %s file: not mounted, or not the folder that was added", l.cfg.Root, markFile)
		}
		var m struct{ ID string }
		if json.Unmarshal(b, &m) != nil || m.ID != l.cfg.Mark {
			return offline("%s holds another store's %s file", l.cfg.Root, markFile)
		}
		return nil
	}
	f, err := os.Open(l.cfg.Root)
	if err != nil {
		return offline("cannot read %s: %v", l.cfg.Root, err)
	}
	defer f.Close()
	if names, _ := f.Readdirnames(1); len(names) == 0 {
		return offline("%s is empty: not mounted?", l.cfg.Root)
	}
	return nil
}

func (l *Local) Check(ctx context.Context) (Usage, error) {
	if err := l.available(); err != nil {
		return Usage{}, err
	}
	free, total := freeSpace(l.cfg.Root)
	u := Usage{Total: total, Free: max(free, 0)}
	if l.mode == Managed {
		r, err := os.OpenRoot(l.cfg.Root)
		if err != nil {
			return u, err
		}
		defer r.Close()
		if err := r.MkdirAll(tempDir, 0o755); err != nil {
			return u, fmt.Errorf("cannot write to %s: %w", l.cfg.Root, err)
		}
		probe, err := r.OpenFile(path.Join(tempDir, "check-"+randomID()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return u, fmt.Errorf("cannot write to %s: %w", l.cfg.Root, err)
		}
		name := probe.Name()
		probe.Close()
		os.Remove(name)
		l.sweep(r)
	}
	return u, nil
}

// sweep empties what is old in the trash (trashDays) and leftovers of writes that stopped.
func (l *Local) sweep(r *os.Root) {
	cut := time.Now().AddDate(0, 0, -trashDays).Format("20060102")
	if days, err := fs.ReadDir(r.FS(), trashDir); err == nil {
		for _, d := range days {
			if d.IsDir() && len(d.Name()) == 8 && d.Name() < cut {
				r.RemoveAll(path.Join(trashDir, d.Name()))
			}
		}
	}
	if tmp, err := fs.ReadDir(r.FS(), tempDir); err == nil {
		for _, d := range tmp {
			if info, err := d.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				r.Remove(path.Join(tempDir, d.Name()))
			}
		}
	}
}

// checkKey refuses keys that are not a plain path below the root.
func checkKey(key string) error {
	if key == "" || strings.Contains(key, "\\") || !filepath.IsLocal(filepath.FromSlash(key)) {
		return fmt.Errorf("bad file key %q", key)
	}
	return nil
}

// missing turns a file that is not there into ErrNotFound, or ErrOffline when it is the whole
// folder that is not.
func (l *Local) missing(err error) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if off := l.available(); off != nil {
		return off
	}
	return ErrNotFound
}

// File opens key for reading.
func (l *Local) File(key string) (*os.File, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(l.cfg.Root)
	if err != nil {
		return nil, l.missing(err)
	}
	defer r.Close()
	f, err := r.Open(filepath.FromSlash(key))
	if err != nil {
		return nil, l.missing(err)
	}
	return f, nil
}

// Path is key's path on the server, for FFmpeg to read: there and inside the folder.
func (l *Local) Path(key string) (string, error) {
	f, err := l.File(key)
	if err != nil {
		return "", err
	}
	f.Close()
	return filepath.Join(l.cfg.Root, filepath.FromSlash(key)), nil
}

type sectionFile struct {
	io.Reader
	f *os.File
}

func (s sectionFile) Close() error { return s.f.Close() }

func (l *Local) Open(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	f, err := l.File(key)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if end < 0 || end >= st.Size() {
		end = st.Size() - 1
	}
	return sectionFile{io.NewSectionReader(f, start, max(end-start+1, 0)), f}, nil
}

func localObject(key string, info fs.FileInfo) Object {
	return Object{Key: key, Path: key, Size: info.Size(), Version: strconv.FormatInt(info.ModTime().UnixNano(), 10)}
}

func (l *Local) Stat(ctx context.Context, key string) (Object, error) {
	f, err := l.File(key)
	if err != nil {
		return Object{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Object{}, err
	}
	if !info.Mode().IsRegular() {
		return Object{}, ErrNotFound
	}
	return localObject(key, info), nil
}

// systemDirs are the folders NAS systems and desktops keep their own things in (thumbnails, recycle
// bins, snapshots, metadata), lowercased. They are named one by one: music is in folders whose names
// start with ".", "@", "#" or "$" too (.hack//G.U., @onefive, $uicideboy$), and Kanade keeps an
// album where its artist and title say (review #223).
var systemDirs = map[string]bool{
	"@eadir": true, "@tmp": true, "@recycle": true, "@sharebin": true, "@recently-snapshot": true, // Synology, QNAP
	"#recycle": true, "#snapshot": true, ".@__thumb": true, ".@upload_cache": true,
	"$recycle.bin": true, "recycler": true, "system volume information": true, // Windows
	".trashes": true, ".spotlight-v100": true, ".fseventsd": true, ".temporaryitems": true, // macOS
	".documentrevisions-v100": true, ".appledouble": true,
	".trash": true, "lost+found": true, ".snapshot": true, ".snapshots": true, // Linux, btrfs, NetApp
	".stversions": true, // Syncthing's old versions
}

// skipDir leaves out Kanade's own folders (.kanade-trash, .kanade-tmp) and the systems' above.
func skipDir(name string) bool {
	n := strings.ToLower(name)
	return systemDirs[n] || strings.HasPrefix(n, ".kanade-") || strings.HasPrefix(n, ".trash-")
}

// skipPath says whether a key (folders split by "/") is in a folder left out.
func skipPath(key string) bool {
	dirs := strings.Split(key, "/")
	return slices.ContainsFunc(dirs[:len(dirs)-1], skipDir)
}

// skipFile leaves out the mark and macOS's ._ files (resource forks that copy along with music).
func skipFile(name string) bool {
	return name == markFile || strings.HasPrefix(name, "._") || name == ".DS_Store"
}

// Walk lists every file below the root: subfolders that cannot be read are left out (and named in
// the error returned at the end, with the rest walked); symbolic links are not followed.
func (l *Local) Walk(ctx context.Context, fn func(Object) error) error {
	if err := l.available(); err != nil {
		return err
	}
	var unreadable []string
	err := filepath.WalkDir(l.cfg.Root, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if p == l.cfg.Root {
				return l.missing(err)
			}
			rel, _ := filepath.Rel(l.cfg.Root, p)
			unreadable = append(unreadable, filepath.ToSlash(rel))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != l.cfg.Root && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || skipFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // gone meanwhile
		}
		rel, err := filepath.Rel(l.cfg.Root, p)
		if err != nil {
			return err
		}
		return fn(localObject(filepath.ToSlash(rel), info))
	})
	if err == nil && len(unreadable) > 0 {
		err = &PartialError{Unreadable: unreadable}
	}
	return err
}

// PartialError is a walk that could not read some folders (paths below the root); the rest was
// walked.
type PartialError struct{ Unreadable []string }

func (e *PartialError) Error() string {
	list := e.Unreadable
	if len(list) > 5 {
		list = append(list[:5:5], "…")
	}
	return "some folders could not be read: " + strings.Join(list, ", ")
}

// Covers says whether path (below the root) is in a folder the walk could not read.
func (e *PartialError) Covers(path string) bool {
	for _, u := range e.Unreadable {
		if path == u || strings.HasPrefix(path, u+"/") {
			return true
		}
	}
	return false
}

// Root is the folder on the server.
func (l *Local) Root() string { return l.cfg.Root }

// Put copies f into the folder: written beside, checked, then given its name, so a file that is
// there is always whole. A name taken by other content gets " (2)" and so on; one that already
// holds the same content (an earlier try that stopped before it was recorded) is used as it is.
func (l *Local) Put(ctx context.Context, f PutFile) (string, error) {
	if l.mode != Managed {
		return "", ErrReadOnly
	}
	if err := l.available(); err != nil {
		return "", err
	}
	if err := checkKey(path.Join(f.Dir, f.Name)); err != nil {
		return "", err
	}
	if free, _ := freeSpace(l.cfg.Root); free >= 0 && free-f.Size < l.reserve(l.cfg.Root) {
		return "", fmt.Errorf("%w: %s free, %s needed", ErrNoRoom, mib(free), mib(f.Size+l.reserve(l.cfg.Root)))
	}
	r, err := os.OpenRoot(l.cfg.Root)
	if err != nil {
		return "", l.missing(err)
	}
	defer r.Close()
	dir := filepath.FromSlash(f.Dir)
	if err := r.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := r.MkdirAll(tempDir, 0o755); err != nil {
		return "", err
	}
	tmp := path.Join(tempDir, randomID()+".part")
	defer r.Remove(tmp)
	if err := l.write(ctx, r, tmp, f); err != nil {
		return "", err
	}
	ext := path.Ext(f.Name)
	base := strings.TrimSuffix(f.Name, ext)
	for n := 1; n < 1000; n++ {
		name := f.Name
		if n > 1 {
			name = fmt.Sprintf("%s (%d)%s", base, n, ext)
		}
		key := path.Join(f.Dir, name)
		if same, err := sameContent(r, key, f.Size, f.SHA256); err != nil {
			return "", err
		} else if same {
			return key, nil
		}
		if _, err := r.Lstat(filepath.FromSlash(key)); err == nil {
			continue // other content
		}
		// A link does not replace a file that appeared meanwhile; file systems without links
		// (some network shares) take a rename.
		err := r.Link(tmp, filepath.FromSlash(key))
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			if _, statErr := r.Lstat(filepath.FromSlash(key)); statErr == nil {
				continue
			}
			if err = r.Rename(tmp, filepath.FromSlash(key)); err != nil {
				return "", err
			}
		}
		syncDir(r, dir)
		return key, nil
	}
	return "", fmt.Errorf("too many files named like %s in %s", f.Name, f.Dir)
}

// write copies f to tmp, synced, and reads it back: the copy must hold the bytes f.SHA256 names.
func (l *Local) write(ctx context.Context, r *os.Root, tmp string, f PutFile) error {
	src, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer src.Close()
	if st, err := src.Stat(); err != nil || st.Size() != f.Size {
		return fmt.Errorf("%s changed since it was hashed", f.Path)
	}
	dst, err := r.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	var sent int64
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			dst.Close()
			return err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			if _, err := dst.Write(buf[:n]); err != nil {
				dst.Close()
				if errors.Is(err, syscall.ENOSPC) {
					return fmt.Errorf("%w: %v", ErrNoRoom, err)
				}
				return err
			}
			sent += int64(n)
			if f.Progress != nil {
				f.Progress(sent, f.Size)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			dst.Close()
			return rerr
		}
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if sent != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return fmt.Errorf("%s changed since it was hashed", f.Path)
	}
	if same, err := sameContent(r, tmp, f.Size, f.SHA256); err != nil || !same {
		return fmt.Errorf("the copy written to %s does not read back as the file: %v", l.cfg.Root, err)
	}
	return nil
}

// sameContent says whether key holds exactly size bytes hashing to sha (false when it is not there).
func sameContent(r *os.Root, key string, size int64, sha string) (bool, error) {
	f, err := r.Open(filepath.FromSlash(key))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != size {
		return false, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == sha, nil
}

func syncDir(r *os.Root, dir string) {
	if d, err := r.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

func mib(n int64) string { return fmt.Sprintf("%d MiB", n>>20) }

// Trash moves key into the folder's trash, under the day it went there.
func (l *Local) Trash(ctx context.Context, key string) error {
	if l.mode != Managed {
		return ErrReadOnly
	}
	if err := checkKey(key); err != nil {
		return err
	}
	if err := l.available(); err != nil {
		return err
	}
	r, err := os.OpenRoot(l.cfg.Root)
	if err != nil {
		return l.missing(err)
	}
	defer r.Close()
	from := filepath.FromSlash(key)
	if _, err := r.Lstat(from); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	to := filepath.Join(trashDir, time.Now().Format("20060102"), from)
	if err := r.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if _, err := r.Lstat(to); err == nil {
		to += "." + randomID()[:6]
	}
	return r.Rename(from, to)
}

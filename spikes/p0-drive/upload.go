package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Google requires every chunk except the last to be a multiple of 256 KiB.
const chunkAlign = 256 << 10

type uploadSession struct {
	// URI lets anyone holding it upload to this session for about a week; it lives in the 0600 state file.
	URI     string    `json:"uri"`
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	Chunk   int64     `json:"chunk"`
	Started time.Time `json:"started"`
}

func cmdGenfile(_ context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: genfile PATH SIZE_MB")
	}
	mb, err := strconv.Atoi(args[1])
	if err != nil || mb <= 0 {
		return fmt.Errorf("bad size %q", args[1])
	}
	f, err := os.Create(args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.CopyN(f, rand.Reader, int64(mb)<<20); err != nil {
		return err
	}
	fmt.Printf("wrote %d MiB of random data to %s\n", mb, args[0])
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func cmdUpload(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("upload", flag.ExitOnError)
	chunkMiB := fs.Float64("chunk", 8, "chunk size in MiB (rounded to 256 KiB)")
	stopAfter := fs.Int("stop-after", 0, "exit after N chunks to simulate a crash")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: upload [-chunk MiB] [-stop-after N] PATH")
	}
	path, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return err
	}
	st, err := loadState()
	if err != nil {
		return err
	}
	if err := st.requireFolder(); err != nil {
		return err
	}
	if st.Upload != nil {
		return fmt.Errorf("an interrupted upload of %s is pending; run `resume` first", st.Upload.Name)
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	chunk := int64(*chunkMiB*(1<<20)) / chunkAlign * chunkAlign
	if chunk < chunkAlign {
		chunk = chunkAlign
	}

	t0 := time.Now()
	sum, err := sha256File(path)
	if err != nil {
		return err
	}
	fmt.Printf("local sha256 %s (%s, %.1f MiB)\n", sum, time.Since(t0).Round(time.Millisecond), float64(info.Size())/(1<<20))

	// fields= on the initiation URL shapes the final response of the upload.
	initURL := uploadBase + "/files?" + url.Values{
		"uploadType": {"resumable"},
		"fields":     {"id,name,size,md5Checksum,sha256Checksum"},
	}.Encode()
	meta, _ := json.Marshal(map[string]any{"name": filepath.Base(path), "parents": []string{st.FolderID}})
	header := http.Header{}
	header.Set("Content-Type", "application/json; charset=UTF-8")
	header.Set("X-Upload-Content-Type", "application/octet-stream")
	header.Set("X-Upload-Content-Length", strconv.FormatInt(info.Size(), 10))
	resp, err := d.do(ctx, http.MethodPost, initURL, meta, header)
	if err != nil {
		return err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return errors.New("no session URI in the Location header")
	}
	s := &uploadSession{URI: loc, Path: path, Name: filepath.Base(path), Size: info.Size(), SHA256: sum, Chunk: chunk, Started: time.Now()}
	st.Upload = s
	if err := st.save(); err != nil {
		return err
	}
	fmt.Printf("session saved; uploading in %.2f MiB chunks\n", float64(chunk)/(1<<20))
	return d.runUpload(ctx, st, 0, *stopAfter)
}

func cmdResume(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	stopAfter := fs.Int("stop-after", 0, "exit after N chunks to simulate a crash")
	fs.Parse(args)
	st, err := loadState()
	if err != nil {
		return err
	}
	if st.Upload == nil {
		return errors.New("no interrupted upload")
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	off, done, err := d.uploadStatus(ctx, st.Upload)
	if err != nil {
		return err
	}
	if done != nil {
		fmt.Println("server already has the whole file")
		return d.finishUpload(ctx, st, *done, time.Now(), 0)
	}
	fmt.Printf("server has %d of %d bytes (%.1f%%); resuming\n", off, st.Upload.Size, 100*float64(off)/float64(st.Upload.Size))
	return d.runUpload(ctx, st, off, *stopAfter)
}

// uploadStatus asks how many bytes Google has persisted for the session.
func (d *drive) uploadStatus(ctx context.Context, s *uploadSession) (int64, *driveFile, error) {
	auth, err := d.bearer(ctx)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.URI, nil)
	if err != nil {
		return 0, nil, err
	}
	req.ContentLength = 0
	req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", s.Size))
	req.Header.Set("Authorization", auth)
	resp, err := d.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	return d.chunkResult(resp, s)
}

func (d *drive) chunkResult(resp *http.Response, s *uploadSession) (int64, *driveFile, error) {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var f driveFile
		if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
			return 0, nil, err
		}
		return s.Size, &f, nil
	case 308: // Resume Incomplete
		return persistedEnd(resp.Header.Get("Range")), nil, nil
	case http.StatusNotFound, http.StatusGone:
		return 0, nil, errors.New("upload session expired; delete it from the state file and start over")
	case http.StatusUnauthorized:
		d.tok.Expiry = time.Time{}
		return 0, nil, errors.New("401 during upload; token will be refreshed")
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return 0, nil, parseAPIError(resp.StatusCode, data)
	}
}

// persistedEnd turns a 308 Range header ("bytes=0-N") into the next offset; no header means nothing persisted.
func persistedEnd(r string) int64 {
	_, end, ok := strings.Cut(strings.TrimPrefix(r, "bytes="), "-")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(end, 10, 64)
	if err != nil {
		return 0
	}
	return n + 1
}

func (d *drive) putChunk(ctx context.Context, s *uploadSession, f *os.File, off, n int64) (int64, *driveFile, error) {
	auth, err := d.bearer(ctx)
	if err != nil {
		return off, nil, err
	}
	// The body streams straight from disk: no chunk is ever held in memory.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.URI, io.NewSectionReader(f, off, n))
	if err != nil {
		return off, nil, err
	}
	req.ContentLength = n
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, s.Size))
	req.Header.Set("Authorization", auth)
	resp, err := d.http.Do(req)
	if err != nil {
		return off, nil, err
	}
	defer resp.Body.Close()
	return d.chunkResult(resp, s)
}

func (d *drive) runUpload(ctx context.Context, st *state, off int64, stopAfter int) error {
	s := st.Upload
	f, err := os.Open(s.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() != s.Size {
		return fmt.Errorf("%s changed since the upload started", s.Path)
	}
	t0, startOff, chunks := time.Now(), off, 0
	for {
		n := min(s.Chunk, s.Size-off)
		next, done, err := d.putChunk(ctx, s, f, off, n)
		for attempt := 0; err != nil; attempt++ {
			if attempt >= 5 {
				return fmt.Errorf("giving up at byte %d: %w (state kept; run `resume` later)", off, err)
			}
			wait := backoff(attempt)
			fmt.Printf("\n  chunk at %d failed: %v; asking the server for its offset in %s\n", off, err, wait.Round(time.Millisecond))
			time.Sleep(wait)
			next, done, err = d.uploadStatus(ctx, s)
		}
		if done != nil {
			fmt.Println()
			return d.finishUpload(ctx, st, *done, t0, s.Size-startOff)
		}
		off = next
		chunks++
		elapsed := time.Since(t0).Seconds()
		fmt.Printf("\r  %5.1f%%  %7.1f / %.1f MiB  %.2f MiB/s   ", 100*float64(off)/float64(s.Size),
			float64(off)/(1<<20), float64(s.Size)/(1<<20), float64(off-startOff)/(1<<20)/elapsed)
		if stopAfter > 0 && chunks >= stopAfter {
			fmt.Printf("\nstopping after %d chunks at byte %d to simulate a crash; run `resume`\n", chunks, off)
			return nil
		}
	}
}

func (d *drive) finishUpload(ctx context.Context, st *state, f driveFile, t0 time.Time, sent int64) error {
	s := st.Upload
	if sent > 0 {
		secs := time.Since(t0).Seconds()
		fmt.Printf("upload complete: %d bytes this run in %.1fs (%.2f MiB/s)\n", sent, secs, float64(sent)/(1<<20)/secs)
	}
	fmt.Printf("file id %s, size %s\n", f.ID, f.Size)
	if f.SHA256Checksum != "" {
		fmt.Println("sha256Checksum was already present in the upload response")
	}
	st.Upload = nil
	if err := st.save(); err != nil {
		return err
	}
	return d.verify(ctx, f.ID, s.SHA256, s.Size)
}

func cmdVerify(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: verify FILE_ID [SHA256]")
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	want := ""
	if len(args) > 1 {
		want = args[1]
	}
	return d.verify(ctx, args[0], want, -1)
}

// verify polls until Drive reports sha256Checksum, then compares it with the local hash and size.
func (d *drive) verify(ctx context.Context, id, wantSHA string, wantSize int64) error {
	t0 := time.Now()
	for {
		f, err := d.getFile(ctx, id, "id,name,size,md5Checksum,sha1Checksum,sha256Checksum")
		if err != nil {
			return err
		}
		if f.SHA256Checksum != "" {
			fmt.Printf("sha256Checksum available after %s: %s\n", time.Since(t0).Round(time.Millisecond), f.SHA256Checksum)
			ok := wantSHA == "" || strings.EqualFold(f.SHA256Checksum, wantSHA)
			if wantSize >= 0 && f.size() != wantSize {
				ok = false
				fmt.Printf("size mismatch: drive %d, local %d\n", f.size(), wantSize)
			}
			switch {
			case wantSHA == "":
				fmt.Println("no local hash given; nothing to compare")
			case ok:
				fmt.Println("MATCH: remote sha256 and size equal the local file")
			default:
				return fmt.Errorf("MISMATCH: local sha256 %s", wantSHA)
			}
			return nil
		}
		if time.Since(t0) > 2*time.Minute {
			return fmt.Errorf("sha256Checksum still empty after %s (md5=%q)", time.Since(t0).Round(time.Second), f.MD5Checksum)
		}
		time.Sleep(2 * time.Second)
	}
}

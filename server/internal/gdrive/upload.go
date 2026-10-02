package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Every chunk except the last must be a multiple of 256 KiB.
const (
	chunkAlign = 256 << 10
	chunkSize  = 32 * chunkAlign // 8 MiB, streamed from disk (P0 §1: RSS stays flat)
	fileFields = "id,name,size,parents,md5Checksum,sha256Checksum"
)

// SessionStore persists resumable-upload session URIs so uploads survive restarts.
type SessionStore interface {
	LoadSession(ctx context.Context) (string, error)
	SaveSession(ctx context.Context, uri string) error
	DeleteSession(ctx context.Context) error
}

type Upload struct {
	Path     string // local file
	Name     string // name in Drive
	ParentID string
	MIME     string
	Size     int64
	SHA256   string // hex, verified against Drive's sha256Checksum
	Sessions SessionStore
	Progress func(sent, total int64)
}

var ErrChecksumMismatch = errors.New("drive copy does not match the local file")

// Upload sends a file with the resumable protocol and returns the verified Drive file.
func (c *Client) Upload(ctx context.Context, u Upload) (File, error) {
	if u.Size <= 0 {
		return File{}, errors.New("refusing to upload an empty file")
	}
	f, err := os.Open(u.Path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() != u.Size {
		return File{}, fmt.Errorf("%s changed since it was hashed", u.Path)
	}

	uri, err := u.Sessions.LoadSession(ctx)
	if err != nil {
		return File{}, err
	}
	var offset int64
	if uri != "" {
		off, done, err := c.uploadStatus(ctx, uri, u.Size)
		switch {
		case err == nil && done != nil:
			return c.finish(ctx, u, *done)
		case err == nil:
			offset = off
		case errors.Is(err, errSessionGone):
			uri = ""
		default:
			return File{}, err
		}
	}
	if uri == "" {
		// A previous attempt may have finished uploading but crashed before recording it.
		if existing, err := c.findUploaded(ctx, u); err != nil {
			return File{}, err
		} else if existing != nil {
			u.Sessions.DeleteSession(ctx)
			return *existing, nil
		}
		if uri, err = c.startSession(ctx, u); err != nil {
			return File{}, err
		}
		if err := u.Sessions.SaveSession(ctx, uri); err != nil {
			return File{}, err
		}
		offset = 0
	}

	for attempt := 0; ; {
		n := min(int64(chunkSize), u.Size-offset)
		next, done, err := c.putChunk(ctx, uri, f, offset, n, u.Size)
		if err != nil {
			if ctx.Err() != nil {
				return File{}, ctx.Err()
			}
			if attempt >= 5 {
				return File{}, fmt.Errorf("upload stalled at byte %d: %w", offset, err)
			}
			if !sleepCtx(ctx, backoff(attempt)) {
				return File{}, ctx.Err()
			}
			attempt++
			if next, done, err = c.uploadStatus(ctx, uri, u.Size); err != nil {
				if errors.Is(err, errSessionGone) {
					u.Sessions.DeleteSession(ctx)
				}
				continue
			}
		} else {
			attempt = 0
		}
		if done != nil {
			if u.Progress != nil {
				u.Progress(u.Size, u.Size)
			}
			return c.finish(ctx, u, *done)
		}
		offset = next
		if u.Progress != nil {
			u.Progress(offset, u.Size)
		}
	}
}

func (c *Client) finish(ctx context.Context, u Upload, f File) (File, error) {
	u.Sessions.DeleteSession(ctx)
	if f.SHA256Checksum == "" { // P0 found it always present; be safe anyway
		var err error
		if f, err = c.GetFile(ctx, f.ID, fileFields); err != nil {
			return File{}, err
		}
	}
	if !strings.EqualFold(f.SHA256Checksum, u.SHA256) || f.SizeBytes() != u.Size {
		c.Delete(ctx, f.ID)
		return File{}, fmt.Errorf("%w (local %s, drive %q)", ErrChecksumMismatch, u.SHA256, f.SHA256Checksum)
	}
	return f, nil
}

// findUploaded looks for a finished copy of this exact file in the target folder.
func (c *Client) findUploaded(ctx context.Context, u Upload) (*File, error) {
	q := fmt.Sprintf("name = %s and %s in parents and trashed = false", quote(u.Name), quote(u.ParentID))
	var list struct{ Files []File }
	if err := c.Call(ctx, http.MethodGet, apiBase+"/files?"+url.Values{"q": {q}, "fields": {"files(" + fileFields + ")"}}.Encode(), nil, &list); err != nil {
		return nil, err
	}
	for _, f := range list.Files {
		if strings.EqualFold(f.SHA256Checksum, u.SHA256) && f.SizeBytes() == u.Size {
			return &f, nil
		}
	}
	return nil, nil
}

func (c *Client) startSession(ctx context.Context, u Upload) (string, error) {
	meta, _ := json.Marshal(map[string]any{"name": u.Name, "parents": []string{u.ParentID}})
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=UTF-8")
	h.Set("X-Upload-Content-Type", u.MIME)
	h.Set("X-Upload-Content-Length", strconv.FormatInt(u.Size, 10))
	resp, err := c.Do(ctx, http.MethodPost, uploadBase+"/files?"+url.Values{"uploadType": {"resumable"}, "fields": {fileFields}}.Encode(), meta, h)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("drive did not return an upload session")
	}
	return loc, nil
}

var errSessionGone = errors.New("upload session expired")

func (c *Client) putChunk(ctx context.Context, uri string, f *os.File, off, n, total int64) (int64, *File, error) {
	auth, err := c.bearer(ctx, false)
	if err != nil {
		return off, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uri, io.NewSectionReader(f, off, n))
	if err != nil {
		return off, nil, err
	}
	req.ContentLength = n
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, total))
	req.Header.Set("Authorization", auth)
	resp, err := c.http.Do(req)
	if err != nil {
		return off, nil, err
	}
	defer resp.Body.Close()
	return c.sessionResult(ctx, resp, total)
}

// uploadStatus asks how many bytes Drive holds for the session.
func (c *Client) uploadStatus(ctx context.Context, uri string, total int64) (int64, *File, error) {
	auth, err := c.bearer(ctx, false)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uri, nil)
	if err != nil {
		return 0, nil, err
	}
	req.ContentLength = 0
	req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", total))
	req.Header.Set("Authorization", auth)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	return c.sessionResult(ctx, resp, total)
}

func (c *Client) sessionResult(ctx context.Context, resp *http.Response, total int64) (int64, *File, error) {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var f File
		if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
			return 0, nil, err
		}
		return total, &f, nil
	case 308: // Resume Incomplete
		return persistedEnd(resp.Header.Get("Range")), nil, nil
	case http.StatusNotFound, http.StatusGone:
		return 0, nil, errSessionGone
	case http.StatusUnauthorized:
		c.bearer(ctx, true)
		return 0, nil, errors.New("access token expired during upload")
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return 0, nil, parseAPIError(resp.StatusCode, body)
	}
}

// persistedEnd turns "bytes=0-N" into N+1; without a header nothing was persisted.
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

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.Call(ctx, http.MethodDelete, apiBase+"/files/"+url.PathEscape(id), nil, nil)
}

// Trash moves a file to the Drive trash, where it can be restored for 30 days.
func (c *Client) Trash(ctx context.Context, id string) error {
	return c.Call(ctx, http.MethodPatch, apiBase+"/files/"+url.PathEscape(id)+"?fields=id", map[string]bool{"trashed": true}, nil)
}

// OpenRange streams bytes [start, end] (end < 0 means to the end of the file).
func (c *Client) OpenRange(ctx context.Context, id string, start, end int64) (*http.Response, error) {
	h := http.Header{}
	if end >= 0 {
		h.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	} else {
		h.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	return c.Do(ctx, http.MethodGet, MediaURL(id), nil, h)
}

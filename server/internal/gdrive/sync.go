package gdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Changes, listing, moving and random access, for reconciling the library with Drive and for the
// inbox (P2-6, decision D6).

// StartPageToken is where the change feed starts from now on.
func (c *Client) StartPageToken(ctx context.Context) (string, error) {
	var r struct {
		StartPageToken string `json:"startPageToken"`
	}
	err := c.Call(ctx, http.MethodGet, apiBase+"/changes/startPageToken", nil, &r)
	return r.StartPageToken, err
}

type Change struct {
	FileID  string `json:"fileId"`
	Removed bool   `json:"removed"` // deleted for good, or no longer visible
	File    *File  `json:"file"`
}

const listFields = "id,name,mimeType,size,parents,trashed,md5Checksum,sha256Checksum,createdTime"

// Changes lists what changed in the Drive since token and returns the token to continue from.
func (c *Client) Changes(ctx context.Context, token string) ([]Change, string, error) {
	var out []Change
	for token != "" {
		var r struct {
			Changes           []Change `json:"changes"`
			NextPageToken     string   `json:"nextPageToken"`
			NewStartPageToken string   `json:"newStartPageToken"`
		}
		v := url.Values{"pageToken": {token}, "pageSize": {"1000"}, "includeRemoved": {"true"}, "spaces": {"drive"},
			"fields": {"nextPageToken,newStartPageToken,changes(fileId,removed,file(" + listFields + "))"}}
		if err := c.Call(ctx, http.MethodGet, apiBase+"/changes?"+v.Encode(), nil, &r); err != nil {
			return out, "", err
		}
		out = append(out, r.Changes...)
		if r.NewStartPageToken != "" {
			return out, r.NewStartPageToken, nil
		}
		token = r.NextPageToken
	}
	return out, "", errors.New("the change list ended without a new token")
}

// List calls fn with each page of files matching a Drive query.
func (c *Client) List(ctx context.Context, q string, fn func([]File) error) error {
	page := ""
	for {
		v := url.Values{"q": {q}, "pageSize": {"1000"}, "fields": {"nextPageToken,files(" + listFields + ")"}}
		if page != "" {
			v.Set("pageToken", page)
		}
		var r struct {
			Files         []File `json:"files"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := c.Call(ctx, http.MethodGet, apiBase+"/files?"+v.Encode(), nil, &r); err != nil {
			return err
		}
		if err := fn(r.Files); err != nil {
			return err
		}
		if r.NextPageToken == "" {
			return nil
		}
		page = r.NextPageToken
	}
}

// Children lists a folder's files and folders that are not in the trash.
func (c *Client) Children(ctx context.Context, folder string) ([]File, error) {
	var out []File
	err := c.List(ctx, fmt.Sprintf("%s in parents and trashed = false", quote(folder)), func(fs []File) error {
		out = append(out, fs...)
		return nil
	})
	return out, err
}

// Move puts a file into another folder on the Drive side: nothing is downloaded or uploaded.
func (c *Client) Move(ctx context.Context, id, to string, from []string) error {
	v := url.Values{"addParents": {to}, "fields": {"id"}}
	var rm []string
	for _, p := range from {
		if p != to {
			rm = append(rm, p)
		}
	}
	if len(rm) > 0 {
		v.Set("removeParents", strings.Join(rm, ","))
	}
	return c.Call(ctx, http.MethodPatch, apiBase+"/files/"+url.PathEscape(id)+"?"+v.Encode(), map[string]any{}, nil)
}

// ReaderAt reads a Drive file at random positions through Range requests, a block at a time with
// a few blocks kept, so parsing tags reads kilobytes instead of whole files.
type ReaderAt struct {
	c    RangeOpener
	ctx  context.Context
	id   string
	size int64

	mu     sync.Mutex
	blocks map[int64][]byte
	order  []int64
}

const raBlock = 256 << 10

// RangeOpener reads byte ranges of Drive files; *Client is one.
type RangeOpener interface {
	OpenRange(ctx context.Context, id string, start, end int64) (*http.Response, error)
}

func NewReaderAt(ctx context.Context, c RangeOpener, id string, size int64) *ReaderAt {
	return &ReaderAt{c: c, ctx: ctx, id: id, size: size, blocks: map[int64][]byte{}}
}

func (r *ReaderAt) block(n int64) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.blocks[n]; ok {
		return b, nil
	}
	start := n * raBlock
	end := min(start+raBlock, r.size) - 1
	resp, err := r.c.OpenRange(r.ctx, r.id, start, end)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, raBlock))
	if err != nil {
		return nil, err
	}
	if len(r.order) >= 16 {
		delete(r.blocks, r.order[0])
		r.order = r.order[1:]
	}
	r.blocks[n] = b
	r.order = append(r.order, n)
	return b, nil
}

func (r *ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < r.size {
		b, err := r.block(off / raBlock)
		if err != nil {
			return n, err
		}
		i := int(off % raBlock)
		if i >= len(b) {
			return n, io.ErrUnexpectedEOF
		}
		c := copy(p[n:], b[i:])
		n += c
		off += int64(c)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

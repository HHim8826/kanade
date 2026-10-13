package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A OneDrive (#95), through Microsoft Graph with the administrator's own app registration (as
// Google Drive is reached with their own OAuth client). A source is imported where it is: its
// files listed folder by folder (their IDs are their keys, so a file renamed or moved in OneDrive
// stays the library's song), read with the short-lived download links Graph hands out (asked
// again when one expires), each checked against OneDrive's quickXorHash when fetched. A managed
// OneDrive keeps imports in a folder of it, sent in upload sessions that resume after a restart,
// and checked against the quickXorHash OneDrive computes of what it got. Deleted files go to
// OneDrive's recycle bin; a source's are never touched. When Graph says it is busy (429, 503), the
// request waits as long as it says.

// The Microsoft endpoints of its global cloud; variables for tests.
var (
	msLogin = "https://login.microsoftonline.com"
	msGraph = "https://graph.microsoft.com/v1.0"
)

// odClouds are Microsoft's other national clouds, as OneDriveConfig.Region names them: China's
// (operated by 21Vianet) and the US government's. Accounts and apps of one are not the others'.
var odClouds = map[string][2]string{
	"cn": {"https://login.chinacloudapi.cn", "https://microsoftgraph.chinacloudapi.cn/v1.0"},
	"us": {"https://login.microsoftonline.us", "https://graph.microsoft.us/v1.0"},
}

// hosts are the sign-in and Graph endpoints of cfg's cloud.
func (cfg OneDriveConfig) hosts() (login, graph string) {
	if c, ok := odClouds[cfg.Region]; ok {
		return c[0], c[1]
	}
	return msLogin, msGraph
}

// OneDriveConfig is a OneDrive store's app registration, account and folder.
type OneDriveConfig struct {
	ClientID string `json:"client_id"`
	Region   string `json:"region,omitempty"` // Microsoft's cloud: "" (global), "cn" or "us" (odClouds)
	Tenant   string `json:"tenant"`           // "common" (personal and work accounts), "consumers", "organizations" or a tenant's ID
	DriveID  string `json:"drive_id,omitempty"`
	Account  string `json:"account,omitempty"` // who signed in, for people
	Folder   string `json:"folder,omitempty"`  // the chosen folder's item ID
	Path     string `json:"path,omitempty"`    // and its path, for people
}

// OneDriveSecret is the app registration's secret and the sign-in's refresh token.
type OneDriveSecret struct {
	ClientSecret string `json:"client_secret"`
	Refresh      string `json:"refresh_token,omitempty"`
}

// odChunk is the size of an upload session's pieces: a multiple of 320 KiB, as Graph asks; a
// variable for tests.
var odChunk int64 = 10 << 20

const (
	odLinkTTL     = 20 * time.Minute
	odQuotaEvery  = 30 * time.Minute
	odMaxWait     = 2 * time.Minute // the longest a throttled request waits at once
	odTries       = 5
	odStateMaxAge = 10 * time.Minute
)

// odBackoff is how long a request waits before it is tried again the nth time when Graph said
// nothing of how long; a variable for tests.
var odBackoff = func(n int) time.Duration { return time.Second << (n - 1) }

// UseMicrosoft has OneDrive stores sign in and reach Graph at other addresses (a test's fake ones,
// which also wait a millisecond when throttled); what it returns puts them back.
func UseMicrosoft(login, graph string) (restore func()) {
	l, g, b := msLogin, msGraph, odBackoff
	msLogin, msGraph, odBackoff = login, graph, func(int) time.Duration { return time.Millisecond }
	return func() { msLogin, msGraph, odBackoff = l, g, b }
}

// ErrNotSignedIn is a OneDrive store not signed in to yet, or whose folder is not chosen yet.
var ErrNotSignedIn = errors.New("OneDrive is not connected yet")

func odScope(mode string) string {
	if mode == Managed {
		return "offline_access Files.ReadWrite"
	}
	return "offline_access Files.Read"
}

type OneDrive struct {
	name, mode string
	cfg        OneDriveConfig
	client     *http.Client
	load       func(ctx context.Context) (OneDriveSecret, error) // the secret as kept now
	// rotate keeps the refresh token a refresh gave back in place of the one it used, and only that:
	// false when the one kept is another by now (signed in again, a new client secret tried).
	rotate func(ctx context.Context, old, new string) (bool, error)
	now    func() time.Time

	refresh sync.Mutex // one refresh at a time
	mu      sync.Mutex
	access  string
	expires time.Time
	links   map[string]odLink // download links by item
	bare    atomic.Bool       // Graph gave a file of this drive without its link: /content is asked at once
	dirs    map[string]string // folder IDs by path below the root, for writes
	quota   Usage
	quotaAt time.Time
}

type odLink struct {
	url string
	at  time.Time
}

func newOneDrive(name, mode string, cfg OneDriveConfig, client *http.Client, load func(context.Context) (OneDriveSecret, error),
	rotate func(ctx context.Context, old, new string) (bool, error)) *OneDrive {
	if cfg.Tenant == "" {
		cfg.Tenant = "common"
	}
	return &OneDrive{name: name, mode: mode, cfg: cfg, client: client, load: load, rotate: rotate, now: time.Now,
		links: map[string]odLink{}, dirs: map[string]string{}}
}

// ---- signing in ----

type odToken struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Expires int64  `json:"expires_in"`
	Scope   string `json:"scope"`
	Error   string `json:"error"`
	Desc    string `json:"error_description"`
}

// odExchange asks the token endpoint for a token (a code exchanged, or a refresh).
func odExchange(ctx context.Context, client *http.Client, cfg OneDriveConfig, secret string, form url.Values) (odToken, error) {
	tenant := cfg.Tenant
	if tenant == "" {
		tenant = "common"
	}
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", secret)
	login, _ := cfg.hosts()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, login+"/"+url.PathEscape(tenant)+"/oauth2/v2.0/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return odToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return odToken{}, offline("signing in to Microsoft: %v", unwrapNet(err))
	}
	defer resp.Body.Close()
	var tok odToken
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok)
	if resp.StatusCode == http.StatusOK && tok.Access != "" {
		return tok, nil
	}
	desc, _, _ := strings.Cut(tok.Desc, "\r\n") // the rest is trace and correlation IDs
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return tok, offline("signing in to Microsoft: %s", resp.Status)
	case tok.Error != "":
		return tok, fmt.Errorf("%w: %s: %s", ErrAuth, tok.Error, desc)
	}
	return tok, fmt.Errorf("%w: %s", ErrAuth, resp.Status)
}

// token is an access token, refreshed when it is about to expire (and kept when the refresh
// token given back is a new one). A refresh that comes back after the sign-in it was made with was
// replaced (signed in again, a new client secret) keeps nothing: the access token serves the
// request that asked, and the next asks again with what is kept now (review #215).
func (o *OneDrive) token(ctx context.Context, stale string) (string, error) {
	o.mu.Lock()
	if o.access != "" && o.access != stale && o.now().Before(o.expires.Add(-2*time.Minute)) {
		t := o.access
		o.mu.Unlock()
		return t, nil
	}
	o.mu.Unlock()
	o.refresh.Lock()
	defer o.refresh.Unlock()
	o.mu.Lock()
	if o.access != "" && o.access != stale && o.now().Before(o.expires.Add(-2*time.Minute)) {
		t := o.access
		o.mu.Unlock()
		return t, nil
	}
	o.mu.Unlock()
	sec, err := o.load(ctx)
	if err != nil {
		return "", err
	}
	if sec.Refresh == "" {
		return "", ErrNotSignedIn
	}
	tok, err := odExchange(ctx, o.client, o.cfg, sec.ClientSecret, url.Values{"grant_type": {"refresh_token"},
		"refresh_token": {sec.Refresh}, "scope": {odScope(o.mode)}})
	if err != nil {
		return "", err
	}
	if tok.Refresh != "" && tok.Refresh != sec.Refresh {
		kept, err := o.rotate(ctx, sec.Refresh, tok.Refresh)
		if err != nil {
			return "", err
		}
		if !kept {
			return tok.Access, nil
		}
	} else if now, err := o.load(ctx); err != nil || now.Refresh != sec.Refresh || now.ClientSecret != sec.ClientSecret {
		return tok.Access, nil // the sign-in was replaced meanwhile
	}
	o.mu.Lock()
	o.access, o.expires = tok.Access, o.now().Add(time.Duration(tok.Expires)*time.Second)
	o.mu.Unlock()
	return tok.Access, nil
}

// ---- requests ----

type odError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// errConflict is a name taken in a folder.
var errConflict = errors.New("the name is taken")

// retryAfter is how long Graph asked to wait, or the backoff.
func retryAfter(resp *http.Response, n int) time.Duration {
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, odMaxWait)
	}
	return min(odBackoff(n), odMaxWait)
}

func wait(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// call sends a Graph request (rel: below Graph's endpoint, or a whole link Graph gave) and decodes its
// answer into out. A token refused is refreshed once; throttling is waited out.
func (o *OneDrive) call(ctx context.Context, method, rel string, body any, out any) error {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	target := rel
	if strings.HasPrefix(rel, "/") {
		_, graph := o.cfg.hosts()
		target = graph + rel
	}
	stale := ""
	for n := 1; ; n++ {
		tok, err := o.token(ctx, stale)
		if err != nil {
			return err
		}
		var rd io.Reader
		if raw != nil {
			rd = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if raw != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := o.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if n >= 3 {
				return offline("OneDrive: %v", unwrapNet(err))
			}
			if err := wait(ctx, odBackoff(n)); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode < 300 {
			defer resp.Body.Close()
			if out != nil {
				if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
					return offline("reading OneDrive's answer: %v", err)
				}
			}
			return nil
		}
		var e odError
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		resp.Body.Close()
		msg := strings.TrimSpace(e.Error.Code + ": " + e.Error.Message)
		switch {
		case resp.StatusCode == http.StatusUnauthorized && stale == "":
			stale = tok // expired or revoked early: refreshed once
			continue
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == 509 || resp.StatusCode >= 500:
			if n >= odTries {
				return offline("OneDrive is busy (%s); try again later", resp.Status)
			}
			if err := wait(ctx, retryAfter(resp, n)); err != nil {
				return err
			}
			continue
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrNotFound, msg)
		case resp.StatusCode == http.StatusConflict:
			return fmt.Errorf("%w: %s", errConflict, msg)
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return fmt.Errorf("%w: %s", ErrAuth, msg)
		case resp.StatusCode == http.StatusInsufficientStorage || e.Error.Code == "quotaLimitReached":
			return fmt.Errorf("%w: %s", ErrNoRoom, msg)
		}
		return fmt.Errorf("OneDrive: %s %s", resp.Status, msg)
	}
}

type odItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	CTag string `json:"cTag"`
	ETag string `json:"eTag"`
	File *struct {
		Hashes struct {
			QuickXor string `json:"quickXorHash"`
		} `json:"hashes"`
	} `json:"file"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	Deleted *struct{} `json:"deleted"`
	Parent  struct {
		DriveID string `json:"driveId"`
		ID      string `json:"id"`
		Path    string `json:"path"`
	} `json:"parentReference"`
	Link string `json:"@microsoft.graph.downloadUrl"`
}

func (it odItem) object(rel string) Object {
	o := Object{Key: it.ID, Path: rel, Size: it.Size, Version: it.CTag}
	if o.Version == "" {
		o.Version = it.ETag
	}
	if it.File != nil && it.File.Hashes.QuickXor != "" {
		o.Hash = hashQuickXor + it.File.Hashes.QuickXor
	}
	return o
}

// path is where an item is in the drive, for people: "/Music/Album".
func (it odItem) path() string {
	p := it.Parent.Path
	if i := strings.Index(p, ":"); i >= 0 {
		p = p[i+1:]
	}
	if it.Parent.Path == "" { // the root
		return "/"
	}
	return strings.TrimRight(p, "/") + "/" + it.Name
}

func (o *OneDrive) item(id string) string {
	return "/drives/" + url.PathEscape(o.cfg.DriveID) + "/items/" + url.PathEscape(id)
}

const odFields = "id,name,size,cTag,eTag,file,folder,deleted,parentReference"

func (o *OneDrive) ready() error {
	if o.cfg.DriveID == "" || o.cfg.Folder == "" {
		return fmt.Errorf("%w: sign in and choose a folder", ErrNotSignedIn)
	}
	return nil
}

// ---- reading ----

func (o *OneDrive) Stat(ctx context.Context, key string) (Object, error) {
	if err := o.ready(); err != nil {
		return Object{}, err
	}
	var it odItem
	if err := o.call(ctx, http.MethodGet, o.item(key)+"?$select="+odFields, nil, &it); err != nil {
		return Object{}, err
	}
	if it.Deleted != nil || it.File == nil {
		return Object{}, ErrNotFound
	}
	return it.object(it.Name), nil
}

// errNoLink is a file Graph gave without its download link: its /content is asked instead.
var errNoLink = errors.New("OneDrive gave no download link for the file")

// link is a download link of an item: one Graph gave in the last minutes, or a new one. A file Graph
// gives without one is errNoLink, not gone: Graph has left the link out of a personal OneDrive's
// files, which were then taken for gone though they were there (their songs and covers 503). Only
// an item Graph does not have (404), or a folder, is not there.
func (o *OneDrive) link(ctx context.Context, key string, fresh bool) (string, error) {
	o.mu.Lock()
	l, ok := o.links[key]
	o.mu.Unlock()
	if ok && !fresh && o.now().Sub(l.at) < odLinkTTL {
		return l.url, nil
	}
	if o.bare.Load() {
		return "", errNoLink // one request, not two: the item would come without it
	}
	var it odItem
	if err := o.call(ctx, http.MethodGet, o.item(key)+"?$select=id,deleted,file,folder,@microsoft.graph.downloadUrl", nil, &it); err != nil {
		return "", err
	}
	if it.Folder != nil {
		return "", ErrNotFound
	}
	if it.Link == "" {
		o.bare.Store(true)
	}
	if it.Deleted != nil || it.File == nil || it.Link == "" {
		return "", errNoLink // not what Graph says of a file there: its /content says what it is
	}
	o.keepLink(key, it.Link)
	return it.Link, nil
}

func (o *OneDrive) keepLink(key, link string) {
	o.mu.Lock()
	if len(o.links) > 2000 {
		clear(o.links)
	}
	o.links[key] = odLink{link, o.now()}
	o.mu.Unlock()
}

// setRange asks for bytes [start, end] (end < 0: to the end) of a file.
func setRange(req *http.Request, start, end int64) {
	if start > 0 || end >= 0 {
		r := fmt.Sprintf("bytes=%d-", start)
		if end >= 0 {
			r += strconv.FormatInt(end, 10)
		}
		req.Header.Set("Range", r)
	}
}

// served is the file's bytes from start in an answer to a read: one not honouring the range sent
// the whole file, skipped to start. ok is false for an answer that is not the file's bytes.
func served(resp *http.Response, start int64) (rc io.ReadCloser, ok bool, err error) {
	switch {
	case resp.StatusCode == http.StatusPartialContent || (resp.StatusCode == http.StatusOK && start == 0):
		return resp.Body, true, nil
	case resp.StatusCode == http.StatusOK: // the range not honoured: skipped to
		if _, err := io.CopyN(io.Discard, resp.Body, start); err != nil {
			resp.Body.Close()
			return nil, true, offline("OneDrive: %v", err)
		}
		return resp.Body, true, nil
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		resp.Body.Close()
		return nil, true, fmt.Errorf("OneDrive: %s", resp.Status)
	}
	return nil, false, nil
}

// fromContent reads a file Graph gave without its download link through its /content, which
// redirects to the link (kept as the file's for the next reads), or sends bytes [start, end] itself.
// A token refused is refreshed once; throttling is waited out.
func (o *OneDrive) fromContent(ctx context.Context, key string, start, end int64) (string, io.ReadCloser, error) {
	_, graph := o.cfg.hosts()
	client := *o.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	stale := ""
	for n := 1; ; n++ {
		tok, err := o.token(ctx, stale)
		if err != nil {
			return "", nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, graph+o.item(key)+"/content", nil)
		if err != nil {
			return "", nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		setRange(req, start, end)
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", nil, ctx.Err()
			}
			if n >= 3 {
				return "", nil, offline("OneDrive: %v", unwrapNet(err))
			}
			if err := wait(ctx, odBackoff(n)); err != nil {
				return "", nil, err
			}
			continue
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			resp.Body.Close()
			loc, err := resp.Location()
			if err != nil {
				return "", nil, fmt.Errorf("%w (%s)", errNoLink, resp.Status)
			}
			o.keepLink(key, loc.String())
			return loc.String(), nil, nil
		}
		if rc, ok, err := served(resp, start); ok {
			return "", rc, err
		}
		var e odError
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		resp.Body.Close()
		msg := strings.TrimSpace(e.Error.Code + ": " + e.Error.Message)
		switch {
		case resp.StatusCode == http.StatusUnauthorized && stale == "":
			stale = tok // expired or revoked early: refreshed once
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			return "", nil, fmt.Errorf("%w: %s", ErrNotFound, msg)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 509 || resp.StatusCode >= 500:
			if n >= odTries {
				return "", nil, offline("OneDrive is busy (%s); try again later", resp.Status)
			}
			if err := wait(ctx, retryAfter(resp, n)); err != nil {
				return "", nil, err
			}
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return "", nil, fmt.Errorf("%w: %s", ErrAuth, msg)
		default:
			return "", nil, fmt.Errorf("OneDrive: reading the file: %s %s", resp.Status, msg)
		}
	}
}

// Open reads with the item's download link (no token: the link is its own), asking for another
// when it has expired; a file Graph gives without one is read through its /content.
func (o *OneDrive) Open(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	if err := o.ready(); err != nil {
		return nil, err
	}
	fresh := false
	for n := 1; ; n++ {
		link, err := o.link(ctx, key, fresh)
		if errors.Is(err, errNoLink) {
			var rc io.ReadCloser
			if link, rc, err = o.fromContent(ctx, key, start, end); rc != nil {
				return rc, nil
			}
		}
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if err != nil {
			return nil, err
		}
		setRange(req, start, end)
		resp, err := o.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if n >= 3 {
				return nil, offline("OneDrive: %v", unwrapNet(err))
			}
			wait(ctx, odBackoff(n))
			continue
		}
		if rc, ok, err := served(resp, start); ok {
			return rc, err
		}
		resp.Body.Close()
		if n >= odTries {
			return nil, offline("OneDrive: reading the file: %s", resp.Status)
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
			fresh = true // the link expired: another is asked for (a file gone is said so then)
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			if err := wait(ctx, retryAfter(resp, n)); err != nil {
				return nil, err
			}
		default:
			if resp.StatusCode < 500 {
				return nil, fmt.Errorf("OneDrive: reading the file: %s", resp.Status)
			}
			wait(ctx, odBackoff(n))
		}
	}
}

// children calls fn for every item in a folder.
func (o *OneDrive) children(ctx context.Context, id string, fn func(odItem) error) error {
	next := o.item(id) + "/children?$top=1000&$select=" + odFields
	for next != "" {
		var page struct {
			Value []odItem `json:"value"`
			Next  string   `json:"@odata.nextLink"`
		}
		if err := o.call(ctx, http.MethodGet, next, nil, &page); err != nil {
			return err
		}
		for _, it := range page.Value {
			if err := fn(it); err != nil {
				return err
			}
		}
		next = page.Next
	}
	return nil
}

// Walk lists every file below the folder, folder by folder. A listing that fails fails the walk:
// a partial one would say files are gone that are not.
func (o *OneDrive) Walk(ctx context.Context, fn func(Object) error) error {
	if err := o.ready(); err != nil {
		return err
	}
	type dir struct{ id, rel string }
	queue := []dir{{o.cfg.Folder, ""}}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		err := o.children(ctx, d.id, func(it odItem) error {
			if it.Deleted != nil {
				return nil
			}
			rel := it.Name
			if d.rel != "" {
				rel = d.rel + "/" + it.Name
			}
			switch {
			case it.Folder != nil:
				if !skipDir(it.Name) {
					queue = append(queue, dir{it.ID, rel})
				}
			case it.File != nil && !skipFile(it.Name):
				return fn(it.object(rel))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Check reads the folder (and the drive's room, now and then).
func (o *OneDrive) Check(ctx context.Context) (Usage, error) {
	if err := o.ready(); err != nil {
		return Usage{}, err
	}
	var it odItem
	err := o.call(ctx, http.MethodGet, o.item(o.cfg.Folder)+"?$select="+odFields, nil, &it)
	if errors.Is(err, ErrNotFound) || (err == nil && (it.Deleted != nil || it.Folder == nil)) {
		return Usage{}, fmt.Errorf("the folder %s is no longer in this OneDrive; choose it again", o.cfg.Path)
	}
	if err != nil {
		return Usage{}, err
	}
	o.mu.Lock()
	u, due := o.quota, o.now().Sub(o.quotaAt) >= odQuotaEvery
	o.mu.Unlock()
	if due {
		var d struct {
			Quota struct {
				Total     int64 `json:"total"`
				Remaining int64 `json:"remaining"`
			} `json:"quota"`
		}
		if err := o.call(ctx, http.MethodGet, "/drives/"+url.PathEscape(o.cfg.DriveID)+"?$select=id,quota", nil, &d); err == nil {
			u = Usage{Total: d.Quota.Total, Free: d.Quota.Remaining}
			o.mu.Lock()
			o.quota, o.quotaAt = u, o.now()
			o.mu.Unlock()
		}
	}
	return u, nil
}

// ---- folders, for choosing one ----

// Folder is a OneDrive folder.
type Folder struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Children int    `json:"children"`
}

// Folders lists the folders in parent ("" for the drive's root), and parent itself.
func (o *OneDrive) Folders(ctx context.Context, parent string) (Folder, []Folder, error) {
	if o.cfg.DriveID == "" {
		return Folder{}, nil, ErrNotSignedIn
	}
	id := parent
	if id == "" {
		id = "root"
	}
	var here odItem
	if err := o.call(ctx, http.MethodGet, o.item(id)+"?$select="+odFields, nil, &here); err != nil {
		return Folder{}, nil, err
	}
	if here.Folder == nil {
		return Folder{}, nil, fmt.Errorf("%w: not a folder", ErrNotFound)
	}
	h := Folder{ID: here.ID, Name: here.Name, Path: here.path()}
	var list []Folder
	err := o.children(ctx, here.ID, func(it odItem) error {
		if it.Folder != nil && it.Deleted == nil {
			list = append(list, Folder{ID: it.ID, Name: it.Name, Path: strings.TrimRight(h.Path, "/") + "/" + it.Name, Children: it.Folder.ChildCount})
		}
		return nil
	})
	return h, list, err
}

// MakeFolder makes a folder named name in parent ("" for the root), or finds the one there.
func (o *OneDrive) MakeFolder(ctx context.Context, parent, name string) (Folder, error) {
	if o.cfg.DriveID == "" {
		return Folder{}, ErrNotSignedIn
	}
	if parent == "" {
		parent = "root"
	}
	id, err := o.child(ctx, parent, odName(name), true)
	if err != nil {
		return Folder{}, err
	}
	var it odItem
	if err := o.call(ctx, http.MethodGet, o.item(id)+"?$select="+odFields, nil, &it); err != nil {
		return Folder{}, err
	}
	return Folder{ID: it.ID, Name: it.Name, Path: it.path()}, nil
}

// ---- writing ----

// odName is a name OneDrive takes: the characters it refuses written as their full-width forms
// (as Japanese and Chinese titles often have them already), no dot or space at its end.
func odName(s string) string {
	s = strings.NewReplacer(`"`, "＂", "*", "＊", ":", "：", "<", "＜", ">", "＞", "?", "？", `\`, "＼", "|", "｜", "/", "／").Replace(s)
	s = strings.TrimRight(strings.TrimSpace(s), ". ")
	if s == "" {
		s = "_"
	}
	return s
}

// child is the item named name in folder parent (a folder of that name made when dir is true and
// there is none).
func (o *OneDrive) child(ctx context.Context, parent, name string, dir bool) (string, error) {
	var it odItem
	err := o.call(ctx, http.MethodGet, o.item(parent)+":/"+url.PathEscape(name)+":?$select=id,folder,deleted", nil, &it)
	if err == nil && it.Deleted == nil {
		if dir && it.Folder == nil {
			return "", fmt.Errorf("%s is a file, not a folder", name)
		}
		return it.ID, nil
	}
	if !errors.Is(err, ErrNotFound) && err != nil {
		return "", err
	}
	if !dir {
		return "", ErrNotFound
	}
	err = o.call(ctx, http.MethodPost, o.item(parent)+"/children", map[string]any{"name": name, "folder": map[string]any{},
		"@microsoft.graph.conflictBehavior": "fail"}, &it)
	if errors.Is(err, errConflict) { // made meanwhile
		return o.child(ctx, parent, name, false)
	}
	return it.ID, err
}

// dir is the folder at rel below the root, made as needed.
func (o *OneDrive) dir(ctx context.Context, rel string) (string, error) {
	id, at := o.cfg.Folder, ""
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		seg = odName(seg)
		at += "/" + seg
		o.mu.Lock()
		known, ok := o.dirs[at]
		o.mu.Unlock()
		if ok {
			id = known
			continue
		}
		next, err := o.child(ctx, id, seg, true)
		if err != nil {
			return "", err
		}
		o.mu.Lock()
		o.dirs[at] = next
		o.mu.Unlock()
		id = next
	}
	return id, nil
}

// odSession is an upload session kept across restarts.
type odSession struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func quickXorOf(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := NewQuickXorHash()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// Put keeps f at Dir/Name in the folder, or at "Name (2)" and so on when that holds other
// content; a file there with f's size and quickXorHash is f's.
func (o *OneDrive) Put(ctx context.Context, f PutFile) (string, error) {
	if o.mode != Managed {
		return "", ErrReadOnly
	}
	if err := o.ready(); err != nil {
		return "", err
	}
	qx, err := quickXorOf(f.Path)
	if err != nil {
		return "", err
	}
	if f.Sessions != nil {
		if raw, err := f.Sessions.LoadSession(ctx); err == nil && raw != "" {
			var ses odSession
			if json.Unmarshal([]byte(raw), &ses) == nil && ses.URL != "" && ses.SHA256 == f.SHA256 && ses.Size == f.Size {
				key, err := o.send(ctx, f, qx, &ses)
				if err == nil || !errors.Is(err, errUploadGone) {
					return key, err
				}
			}
			f.Sessions.DeleteSession(ctx)
		}
	}
	parent, err := o.dir(ctx, f.Dir)
	if err != nil {
		return "", err
	}
	name := odName(f.Name)
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for n := 1; n < 100; n++ {
		try := name
		if n > 1 {
			try = fmt.Sprintf("%s (%d)%s", base, n, ext)
		}
		var it odItem
		err := o.call(ctx, http.MethodGet, o.item(parent)+":/"+url.PathEscape(try)+":?$select="+odFields, nil, &it)
		switch {
		case err == nil && it.Deleted == nil:
			if it.File != nil && it.Size == f.Size && it.File.Hashes.QuickXor == qx {
				return it.ID, nil // there already: an earlier attempt, or the same file again
			}
			continue
		case err != nil && !errors.Is(err, ErrNotFound):
			return "", err
		}
		var s struct {
			URL string `json:"uploadUrl"`
		}
		err = o.call(ctx, http.MethodPost, o.item(parent)+":/"+url.PathEscape(try)+":/createUploadSession",
			map[string]any{"item": map[string]any{"@microsoft.graph.conflictBehavior": "fail"}}, &s)
		if errors.Is(err, errConflict) {
			continue
		}
		if err != nil {
			return "", err
		}
		ses := &odSession{URL: s.URL, SHA256: f.SHA256, Size: f.Size}
		if f.Sessions != nil {
			raw, _ := json.Marshal(ses)
			if err := f.Sessions.SaveSession(ctx, string(raw)); err != nil {
				return "", err
			}
		}
		key, err := o.send(ctx, f, qx, ses)
		if errors.Is(err, errConflict) { // taken while it was sent
			if f.Sessions != nil {
				f.Sessions.DeleteSession(ctx)
			}
			continue
		}
		return key, err
	}
	return "", fmt.Errorf("no free name for %s in %s", f.Name, f.Dir)
}

// expected is where an upload session goes on from: the start of what it says it still needs.
func (o *OneDrive) expected(ctx context.Context, link string) (int64, error) {
	resp, err := o.upload(ctx, http.MethodGet, link, nil, 0, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return 0, errUploadGone
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("OneDrive: the upload session: %s", resp.Status)
	}
	var st struct {
		Next []string `json:"nextExpectedRanges"`
	}
	json.NewDecoder(resp.Body).Decode(&st)
	return firstRange(st.Next), nil
}

func firstRange(next []string) int64 {
	if len(next) == 0 {
		return 0
	}
	a, _, _ := strings.Cut(next[0], "-")
	n, _ := strconv.ParseInt(a, 10, 64)
	return n
}

// send sends what the upload session has not got yet (asked of it), and checks the file OneDrive
// made of it against f's quickXorHash.
func (o *OneDrive) send(ctx context.Context, f PutFile, qx string, ses *odSession) (string, error) {
	if f.Size <= 0 {
		return "", errors.New("an empty file is not uploaded")
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	start, err := o.expected(ctx, ses.URL)
	if err != nil {
		return "", err
	}
	var made odItem
sending:
	for range 100000 {
		if start >= f.Size {
			if start, err = o.expected(ctx, ses.URL); err != nil {
				return "", err
			}
			if start >= f.Size {
				return "", errors.New("OneDrive took every piece but made no file of them")
			}
		}
		end := min(start+odChunk, f.Size) - 1
		size := end - start + 1
		from := start
		resp, err := o.upload(ctx, http.MethodPut, ses.URL, func() io.Reader {
			return &progressReader{r: io.NewSectionReader(file, from, size), base: from, total: f.Size, fn: f.Progress}
		}, size, fmt.Sprintf("bytes %d-%d/%d", from, end, f.Size))
		if err != nil {
			return "", err
		}
		switch resp.StatusCode {
		case http.StatusAccepted:
			var st struct {
				Next []string `json:"nextExpectedRanges"`
			}
			json.NewDecoder(resp.Body).Decode(&st)
			resp.Body.Close()
			start = end + 1
			if len(st.Next) > 0 {
				start = firstRange(st.Next)
			}
		case http.StatusOK, http.StatusCreated:
			err := json.NewDecoder(resp.Body).Decode(&made)
			resp.Body.Close()
			if err != nil {
				return "", offline("reading OneDrive's answer: %v", err)
			}
			break sending
		case http.StatusRequestedRangeNotSatisfiable: // a piece it had already: asked where it is
			resp.Body.Close()
			if start, err = o.expected(ctx, ses.URL); err != nil {
				return "", err
			}
		case http.StatusConflict:
			resp.Body.Close()
			return "", errConflict
		case http.StatusNotFound, http.StatusGone:
			resp.Body.Close()
			return "", errUploadGone
		default:
			var e odError
			json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
			resp.Body.Close()
			if resp.StatusCode == http.StatusInsufficientStorage || e.Error.Code == "quotaLimitReached" {
				return "", fmt.Errorf("%w: %s", ErrNoRoom, e.Error.Message)
			}
			return "", fmt.Errorf("OneDrive: uploading: %s %s", resp.Status, e.Error.Message)
		}
	}
	if made.ID == "" {
		return "", errors.New("OneDrive did not say what it made of the upload")
	}
	if f.Sessions != nil {
		f.Sessions.DeleteSession(ctx)
	}
	if made.Size != f.Size || made.File == nil || made.File.Hashes.QuickXor != qx {
		got := ""
		if made.File != nil {
			got = made.File.Hashes.QuickXor
		}
		o.call(ctx, http.MethodDelete, o.item(made.ID), nil, nil) // just made under a free name
		return "", fmt.Errorf("OneDrive's copy is not the file (%d bytes, quickXorHash %q); it was removed", made.Size, got)
	}
	return made.ID, nil
}

// upload sends one request of an upload session (its link is its own: no token). Throttling and
// the network failing are waited out.
func (o *OneDrive) upload(ctx context.Context, method, link string, body func() io.Reader, size int64, contentRange string) (*http.Response, error) {
	for n := 1; ; n++ {
		var rd io.Reader
		if body != nil {
			rd = body()
		}
		req, err := http.NewRequestWithContext(ctx, method, link, rd)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.ContentLength = size
			req.Header.Set("Content-Range", contentRange)
		}
		resp, err := o.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if n >= odTries {
				return nil, offline("OneDrive: %v", unwrapNet(err))
			}
			if err := wait(ctx, odBackoff(n)); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			resp.Body.Close()
			if n >= odTries {
				return nil, offline("OneDrive is busy (%s); try again later", resp.Status)
			}
			if err := wait(ctx, retryAfter(resp, n)); err != nil {
				return nil, err
			}
			continue
		}
		return resp, nil
	}
}

// Trash moves key to OneDrive's recycle bin.
func (o *OneDrive) Trash(ctx context.Context, key string) error {
	if o.mode != Managed {
		return ErrReadOnly
	}
	if err := o.ready(); err != nil {
		return err
	}
	return o.call(ctx, http.MethodDelete, o.item(key), nil, nil)
}

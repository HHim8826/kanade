package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	apiBase    = "https://www.googleapis.com/drive/v3"
	uploadBase = "https://www.googleapis.com/upload/drive/v3"
	folderMime = "application/vnd.google-apps.folder"
)

type driveFile struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	MimeType       string   `json:"mimeType"`
	Size           string   `json:"size"`
	Parents        []string `json:"parents"`
	MD5Checksum    string   `json:"md5Checksum"`
	SHA1Checksum   string   `json:"sha1Checksum"`
	SHA256Checksum string   `json:"sha256Checksum"`
}

func (f driveFile) size() int64 { n, _ := strconv.ParseInt(f.Size, 10, 64); return n }

type drive struct {
	client oauthClient
	tok    token
	http   *http.Client
}

func newDrive() (*drive, error) {
	client, err := loadClient()
	if err != nil {
		return nil, err
	}
	var tok token
	if err := loadJSON(tokenFile, &tok); err != nil || tok.RefreshToken == "" {
		return nil, errors.New("no saved token; run `p0drive auth` first")
	}
	return &drive{client: client, tok: tok, http: &http.Client{}}, nil
}

func (d *drive) refresh(ctx context.Context) error {
	t, keys, err := exchange(ctx, d.client, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {d.tok.RefreshToken},
	})
	if err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	d.tok.AccessToken, d.tok.Expiry = t.AccessToken, t.Expiry
	if t.RefreshToken != "" {
		d.tok.RefreshToken = t.RefreshToken
	}
	for _, k := range keys {
		if k == "refresh_token_expires_in" {
			d.tok.RefreshTokenExpiresIn = t.RefreshTokenExpiresIn
		}
	}
	return saveJSON(tokenFile, d.tok)
}

func (d *drive) bearer(ctx context.Context) (string, error) {
	if time.Until(d.tok.Expiry) < time.Minute {
		if err := d.refresh(ctx); err != nil {
			return "", err
		}
	}
	return "Bearer " + d.tok.AccessToken, nil
}

type apiError struct {
	Status  int
	Reason  string
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("drive %d %s: %s", e.Status, e.Reason, e.Message)
}

func parseAPIError(status int, body []byte) *apiError {
	var v struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	e := &apiError{Status: status, Message: strings.TrimSpace(string(body))}
	if json.Unmarshal(body, &v) == nil && v.Error.Message != "" {
		e.Message = v.Error.Message
		if len(v.Error.Errors) > 0 {
			e.Reason = v.Error.Errors[0].Reason
		}
	}
	return e
}

func retryable(e *apiError) bool {
	switch e.Status {
	case 429, 500, 502, 503, 504:
		return true
	case 403:
		return e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded"
	}
	return false
}

func backoff(attempt int) time.Duration {
	return time.Duration(1<<attempt)*time.Second + time.Duration(rand.IntN(1000))*time.Millisecond
}

// do sends a request with a replayable body, refreshing the token once on 401
// and backing off on rate limits and 5xx. The caller closes the response body.
func (d *drive) do(ctx context.Context, method, rawURL string, body []byte, header http.Header) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		auth, err := d.bearer(ctx)
		if err != nil {
			return nil, err
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
		if err != nil {
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		req.Header.Set("Authorization", auth)
		resp, err := d.http.Do(req)
		if err != nil {
			if attempt < 4 {
				time.Sleep(backoff(attempt))
				continue
			}
			return nil, err
		}
		if resp.StatusCode < 400 {
			return resp, nil
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		e := parseAPIError(resp.StatusCode, data)
		switch {
		case e.Status == http.StatusUnauthorized && attempt == 0:
			d.tok.Expiry = time.Time{}
		case retryable(e) && attempt < 5:
			wait := backoff(attempt)
			fmt.Printf("  %v; retrying in %s\n", e, wait.Round(time.Millisecond))
			time.Sleep(wait)
		default:
			return nil, e
		}
	}
}

// call is do() for JSON APIs.
func (d *drive) call(ctx context.Context, method, rawURL string, in, out any) error {
	var body []byte
	header := http.Header{}
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
		header.Set("Content-Type", "application/json; charset=UTF-8")
	}
	resp, err := d.do(ctx, method, rawURL, body, header)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (d *drive) getFile(ctx context.Context, id, fields string) (driveFile, error) {
	var f driveFile
	err := d.call(ctx, http.MethodGet, apiBase+"/files/"+url.PathEscape(id)+"?"+url.Values{"fields": {fields}}.Encode(), nil, &f)
	return f, err
}

// inTestTree reports whether id is the test folder or lies beneath it.
// All spike writes are limited to that subtree (D1: writes stay inside the platform folder).
func (d *drive) inTestTree(ctx context.Context, st *state, id string) (bool, error) {
	for depth := 0; depth < 16; depth++ {
		if id == st.FolderID {
			return true, nil
		}
		f, err := d.getFile(ctx, id, "id,parents")
		if err != nil {
			return false, err
		}
		if len(f.Parents) == 0 {
			return false, nil
		}
		id = f.Parents[0]
	}
	return false, nil
}

func cmdAbout(ctx context.Context, _ []string) error {
	d, err := newDrive()
	if err != nil {
		return err
	}
	var about struct {
		User struct {
			DisplayName  string `json:"displayName"`
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
		StorageQuota struct {
			Limit             string `json:"limit"`
			Usage             string `json:"usage"`
			UsageInDrive      string `json:"usageInDrive"`
			UsageInDriveTrash string `json:"usageInDriveTrash"`
		} `json:"storageQuota"`
	}
	if err := d.call(ctx, http.MethodGet, apiBase+"/about?fields=user(displayName,emailAddress),storageQuota", nil, &about); err != nil {
		return err
	}
	gib := func(s string) string {
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return "unlimited"
		}
		return fmt.Sprintf("%.2f GiB", n/(1<<30))
	}
	q := about.StorageQuota
	fmt.Printf("account: %s (%s)\n", about.User.EmailAddress, about.User.DisplayName)
	fmt.Printf("storage: limit %s, used %s (Drive %s, trash %s)\n", gib(q.Limit), gib(q.Usage), gib(q.UsageInDrive), gib(q.UsageInDriveTrash))
	if d.tok.RefreshTokenExpiresIn > 0 {
		fmt.Printf("token: Testing mode, refresh token valid until %s\n",
			d.tok.ObtainedAt.Add(time.Duration(d.tok.RefreshTokenExpiresIn)*time.Second).Format(time.RFC3339))
	}
	return nil
}

func cmdInit(ctx context.Context, _ []string) error {
	st, err := loadState()
	if err != nil {
		return err
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	q := fmt.Sprintf("name = '%s' and mimeType = '%s' and 'root' in parents and trashed = false", testFolderName, folderMime)
	var list struct{ Files []driveFile }
	if err := d.call(ctx, http.MethodGet, apiBase+"/files?"+url.Values{"q": {q}, "fields": {"files(id,name)"}}.Encode(), nil, &list); err != nil {
		return err
	}
	if len(list.Files) > 0 {
		st.FolderID = list.Files[0].ID
		fmt.Printf("found test folder %s (%s)\n", testFolderName, st.FolderID)
	} else {
		var f driveFile
		if err := d.call(ctx, http.MethodPost, apiBase+"/files?fields=id,name",
			map[string]any{"name": testFolderName, "mimeType": folderMime, "parents": []string{"root"}}, &f); err != nil {
			return err
		}
		st.FolderID = f.ID
		fmt.Printf("created test folder %s (%s)\n", testFolderName, st.FolderID)
	}
	return st.save()
}

func cmdMkdir(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mkdir NAME")
	}
	st, err := loadState()
	if err != nil {
		return err
	}
	if err := st.requireFolder(); err != nil {
		return err
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	var f driveFile
	if err := d.call(ctx, http.MethodPost, apiBase+"/files?fields=id,name",
		map[string]any{"name": args[0], "mimeType": folderMime, "parents": []string{st.FolderID}}, &f); err != nil {
		return err
	}
	fmt.Printf("created %s/%s (%s)\n", testFolderName, f.Name, f.ID)
	return nil
}

func cmdList(ctx context.Context, args []string) error {
	st, err := loadState()
	if err != nil {
		return err
	}
	if err := st.requireFolder(); err != nil {
		return err
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	folder := st.FolderID
	if len(args) > 0 {
		folder = args[0]
	}
	q := fmt.Sprintf("'%s' in parents and trashed = false", folder)
	pageToken := ""
	for {
		var list struct {
			Files         []driveFile
			NextPageToken string
		}
		v := url.Values{"q": {q}, "pageSize": {"1000"}, "orderBy": {"folder,name"},
			"fields": {"nextPageToken,files(id,name,mimeType,size,sha256Checksum)"}}
		if pageToken != "" {
			v.Set("pageToken", pageToken)
		}
		if err := d.call(ctx, http.MethodGet, apiBase+"/files?"+v.Encode(), nil, &list); err != nil {
			return err
		}
		for _, f := range list.Files {
			kind := "file"
			if f.MimeType == folderMime {
				kind = "dir "
			}
			sha := "-"
			if f.SHA256Checksum != "" {
				sha = f.SHA256Checksum[:12] + "…"
			}
			fmt.Printf("%s  %-33s  %12s  sha256=%-13s  %s\n", kind, f.ID, f.Size, sha, f.Name)
		}
		if pageToken = list.NextPageToken; pageToken == "" {
			return nil
		}
	}
}

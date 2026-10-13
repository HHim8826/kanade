package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Signing a OneDrive store in (#95): the administrator's browser goes to Microsoft with a state
// made here, and comes back to the callback (or the address it ends on is pasted, when Microsoft
// cannot send it back to this server); the code it brings is exchanged for a refresh token, kept in
// the store's secret. Then a folder is chosen.

// ErrBadState is a sign-in that was not started here, or too long ago.
var ErrBadState = errors.New("unknown or expired sign-in; start again")

// OneDriveCallbackPath is where Microsoft sends the browser back to.
const OneDriveCallbackPath = "/oauth/onedrive/callback"

func (s *Set) oneDriveSecret(ctx context.Context, id int64) (OneDriveSecret, error) {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT secret FROM stores WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return OneDriveSecret{}, ErrNoStore
	}
	var sec OneDriveSecret
	json.Unmarshal([]byte(raw), &sec)
	return sec, err
}

func (s *Set) saveSecret(ctx context.Context, id int64, v any) error {
	raw, _ := json.Marshal(v)
	_, err := s.DB.ExecContext(ctx, `UPDATE stores SET secret = ? WHERE id = ?`, string(raw), id)
	return err
}

func (s *Set) setConfig(ctx context.Context, id int64, v any) error {
	raw, _ := json.Marshal(v)
	_, err := s.DB.ExecContext(ctx, `UPDATE stores SET config = ? WHERE id = ?`, string(raw), id)
	return err
}

// rebuild has a store's backend made again on its next use.
func (s *Set) rebuild(id int64) {
	s.mu.Lock()
	delete(s.built, id)
	s.mu.Unlock()
}

func (s *Set) oneDrive(ctx context.Context, id int64) (*OneDrive, *Store, OneDriveConfig, error) {
	st, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, OneDriveConfig{}, err
	}
	if st == nil {
		return nil, nil, OneDriveConfig{}, ErrNoStore
	}
	if st.Kind != KindOneDrive {
		return nil, st, OneDriveConfig{}, invalid("not a OneDrive")
	}
	var cfg OneDriveConfig
	json.Unmarshal(st.Config, &cfg)
	b, _, err := s.Backend(ctx, id)
	if err != nil {
		return nil, st, cfg, err
	}
	return b.(*OneDrive), st, cfg, nil
}

// SignedIn says whether a OneDrive store has a sign-in (a refresh token); true for other kinds.
func (st *Store) SignedIn() bool {
	if st.Kind != KindOneDrive {
		return true
	}
	var sec OneDriveSecret
	json.Unmarshal([]byte(st.secret), &sec)
	return sec.Refresh != ""
}

// OneDriveAuthURL starts signing a OneDrive store in; redirect is where Microsoft sends the browser
// back to (as registered with the app).
func (s *Set) OneDriveAuthURL(ctx context.Context, id int64, redirect string) (string, error) {
	_, st, cfg, err := s.oneDrive(ctx, id)
	if st == nil {
		return "", err
	}
	if cfg.ClientID == "" {
		return "", invalid("not a OneDrive")
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	state := "onedrive." + strconv.FormatInt(id, 10) + "." + hex.EncodeToString(raw)
	now := db.Now()
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM oauth_states WHERE created_at < ?`, now-odStateMaxAge.Milliseconds()); err != nil {
		return "", err
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO oauth_states (state, created_at) VALUES (?, ?)`, state, now); err != nil {
		return "", err
	}
	q := url.Values{"client_id": {cfg.ClientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "response_mode": {"query"},
		"scope": {odScope(st.Mode)}, "state": {state}, "prompt": {"select_account"}}
	login, _ := cfg.hosts()
	return login + "/" + url.PathEscape(cfg.Tenant) + "/oauth2/v2.0/authorize?" + q.Encode(), nil
}

// OneDriveComplete finishes a sign-in with what the browser brought back (the callback's query),
// keeps its refresh token and says who signed in. Signing in to another OneDrive than the one a
// chosen folder is in is refused: the library's files are in that one.
func (s *Set) OneDriveComplete(ctx context.Context, q url.Values, redirect string) (*Store, error) {
	state := q.Get("state")
	parts := strings.Split(state, ".")
	if len(parts) != 3 || parts[0] != "onedrive" {
		return nil, ErrBadState
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, ErrBadState
	}
	var created int64
	err = s.DB.QueryRowContext(ctx, `DELETE FROM oauth_states WHERE state = ? RETURNING created_at`, state).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && db.Now()-created > odStateMaxAge.Milliseconds()) {
		return nil, ErrBadState
	}
	if err != nil {
		return nil, err
	}
	if e := q.Get("error"); e != "" {
		desc, _, _ := strings.Cut(q.Get("error_description"), "\r\n")
		return nil, invalid("the sign-in was not completed: %s %s", e, desc)
	}
	code := q.Get("code")
	if code == "" {
		return nil, invalid("the sign-in brought back no code")
	}
	_, st, cfg, err := s.oneDrive(ctx, id)
	if st == nil {
		return nil, err
	}
	sec, err := s.oneDriveSecret(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	tok, err := odExchange(ctx, s.httpClient(), cfg, sec.ClientSecret, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirect}, "scope": {odScope(st.Mode)}})
	if err != nil {
		return nil, err
	}
	if tok.Refresh == "" {
		return nil, invalid("microsoft gave no refresh token; the app must allow offline_access")
	}
	want := "Files.Read"
	if st.Mode == Managed {
		want = "Files.ReadWrite"
	}
	if tok.Scope != "" && !strings.Contains(tok.Scope, want) {
		return nil, invalid("microsoft did not grant %s; allow it on the consent screen", want)
	}
	_, graph := cfg.hosts()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, graph+"/me/drive?$select=id,driveType,owner", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Access)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, offline("OneDrive: %v", unwrapNet(err))
	}
	defer resp.Body.Close()
	var drive struct {
		ID    string `json:"id"`
		Owner struct {
			User struct {
				Name  string `json:"displayName"`
				Email string `json:"email"`
			} `json:"user"`
		} `json:"owner"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&drive) != nil || drive.ID == "" {
		return nil, invalid("this account has no OneDrive to use (%s)", resp.Status)
	}
	if cfg.Folder != "" && cfg.DriveID != "" && cfg.DriveID != drive.ID {
		return nil, invalid("this is another OneDrive than the one 「%s」 is in (%s); sign in with that account", st.Name, cfg.Account)
	}
	cfg.DriveID = drive.ID
	cfg.Account = strings.TrimSpace(drive.Owner.User.Name)
	if e := drive.Owner.User.Email; e != "" {
		cfg.Account = strings.TrimSpace(cfg.Account + " " + "<" + e + ">")
	}
	sec.Refresh = tok.Refresh
	if err := s.saveSecret(ctx, id, sec); err != nil {
		return nil, err
	}
	if err := s.setConfig(ctx, id, cfg); err != nil {
		return nil, err
	}
	s.rebuild(id)
	return s.Check(ctx, id)
}

// OneDriveFolders lists the folders of a signed-in OneDrive store's drive in parent ("": its root).
func (s *Set) OneDriveFolders(ctx context.Context, id int64, parent string) (Folder, []Folder, error) {
	o, st, _, err := s.oneDrive(ctx, id)
	if o == nil {
		return Folder{}, nil, named(st, err)
	}
	here, list, err := o.Folders(ctx, parent)
	return here, list, named(st, err)
}

// OneDriveMakeFolder makes a folder in a managed OneDrive store's drive.
func (s *Set) OneDriveMakeFolder(ctx context.Context, id int64, parent, name string) (Folder, error) {
	o, st, _, err := s.oneDrive(ctx, id)
	if o == nil {
		return Folder{}, named(st, err)
	}
	if st.Mode != Managed {
		return Folder{}, ErrReadOnly
	}
	if strings.TrimSpace(name) == "" {
		return Folder{}, invalid("a folder needs a name")
	}
	f, err := o.MakeFolder(ctx, parent, name)
	return f, named(st, err)
}

// SetFolder chooses a OneDrive store's folder. The caller has made sure the library has no files
// in the one chosen before (moving them is a feature of its own).
func (s *Set) SetFolder(ctx context.Context, id int64, item string) (*Store, error) {
	o, st, cfg, err := s.oneDrive(ctx, id)
	if o == nil {
		return nil, named(st, err)
	}
	if cfg.DriveID == "" {
		return nil, ErrNotSignedIn
	}
	here, _, err := o.Folders(ctx, item)
	if err != nil {
		return nil, named(st, err)
	}
	stores, err := s.Stores(ctx)
	if err != nil {
		return nil, err
	}
	for _, other := range stores {
		if other.Kind != KindOneDrive || other.ID == id {
			continue
		}
		var oc OneDriveConfig
		json.Unmarshal(other.Config, &oc)
		if oc.DriveID != cfg.DriveID || oc.Folder == "" {
			continue
		}
		a, b := strings.TrimRight(here.Path, "/")+"/", strings.TrimRight(oc.Path, "/")+"/"
		if strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
			return nil, invalid("%s overlaps the folder of 「%s」 (%s)", here.Path, other.Name, oc.Path)
		}
	}
	cfg.Folder, cfg.Path = here.ID, here.Path
	if err := s.setConfig(ctx, id, cfg); err != nil {
		return nil, err
	}
	s.rebuild(id)
	return s.Check(ctx, id)
}

// setClientSecret gives a OneDrive store's app a new client secret (Microsoft's expire). It is
// tried with the store's sign-in first: one Microsoft refuses is not kept; a sign-in that ended
// does not stop it, the store is then connected again.
func (s *Set) setClientSecret(ctx context.Context, st *Store, raw json.RawMessage) (*Store, error) {
	var n OneDriveSecret
	json.Unmarshal(raw, &n)
	if n.ClientSecret = strings.TrimSpace(n.ClientSecret); n.ClientSecret == "" {
		return nil, invalid("a client secret is needed")
	}
	sec, err := s.oneDriveSecret(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	sec.ClientSecret = n.ClientSecret
	if sec.Refresh != "" {
		var cfg OneDriveConfig
		json.Unmarshal(st.Config, &cfg)
		try, cancel := context.WithTimeout(ctx, tryFor)
		tok, err := odExchange(try, s.httpClient(), cfg, sec.ClientSecret, url.Values{"grant_type": {"refresh_token"},
			"refresh_token": {sec.Refresh}, "scope": {odScope(st.Mode)}})
		cancel()
		switch {
		case err == nil:
			if tok.Refresh != "" {
				sec.Refresh = tok.Refresh
			}
		case tok.Error == "invalid_grant":
		default:
			return nil, tried(err, "Microsoft")
		}
	}
	if err := s.saveSecret(ctx, st.ID, sec); err != nil {
		return nil, err
	}
	s.rebuild(st.ID)
	return s.Check(ctx, st.ID)
}

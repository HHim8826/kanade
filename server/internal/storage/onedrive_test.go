package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/HHim8826/kanade/server/internal/storage/storagetest"
)

const odRedirect = "https://music.example/oauth/onedrive/callback"

func fakeGraph(t *testing.T) *storagetest.Graph {
	t.Helper()
	g := storagetest.NewGraph(t)
	g.QuickXor = func(b []byte) string {
		h := NewQuickXorHash()
		h.Write(b)
		return base64.StdEncoding.EncodeToString(h.Sum(nil))
	}
	t.Cleanup(UseMicrosoft(g.Login(), g.API()))
	chunk := odChunk
	t.Cleanup(func() { odChunk = chunk })
	return g
}

// signIn adds a OneDrive store and signs it in as the browser would.
func signIn(t *testing.T, s *Set, g *storagetest.Graph, mode string) *Store {
	t.Helper()
	ctx := context.Background()
	cfg, _ := json.Marshal(OneDriveConfig{ClientID: g.ClientID})
	sec, _ := json.Marshal(OneDriveSecret{ClientSecret: g.ClientSecret})
	st, err := s.Add(ctx, NewStore{Kind: KindOneDrive, Name: "OneDrive", Mode: mode, Config: cfg, Secret: sec})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateError || st.SignedIn() {
		t.Fatalf("not signed in yet: %+v", st)
	}
	raw, err := s.OneDriveAuthURL(ctx, st.ID, odRedirect)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	scope := "offline_access Files.Read"
	if mode == Managed {
		scope = "offline_access Files.ReadWrite"
	}
	if u.Path != "/login/common/oauth2/v2.0/authorize" || q.Get("client_id") != g.ClientID || q.Get("redirect_uri") != odRedirect || q.Get("scope") != scope {
		t.Fatalf("auth URL %s", raw)
	}
	if _, err := s.OneDriveComplete(ctx, url.Values{"state": {"onedrive.1.forged"}, "code": {g.Code}}, odRedirect); !errors.Is(err, ErrBadState) {
		t.Fatalf("a state not made here: %v", err)
	}
	back := url.Values{"state": {q.Get("state")}, "code": {g.Code}}
	if _, err := s.OneDriveComplete(ctx, back, odRedirect); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OneDriveComplete(ctx, back, odRedirect); !errors.Is(err, ErrBadState) {
		t.Fatalf("a state used twice: %v", err)
	}
	st, _ = s.Get(ctx, st.ID)
	var c OneDriveConfig
	json.Unmarshal(st.Config, &c)
	if !st.SignedIn() || c.Account != "Ann" || c.DriveID == "" {
		t.Fatalf("signed in: %+v %+v", st, c)
	}
	return st
}

// A OneDrive as a source: signed in, a folder chosen, listed folder by folder (pages of them, the
// hidden left out), read with download links asked for again when they expire, tokens refreshed
// (and the new refresh token kept), throttling waited out, never written; a sign-in revoked is
// said so.
func TestOneDriveSource(t *testing.T) {
	ctx := context.Background()
	g := fakeGraph(t)
	music := g.Add("Music", nil)
	one := g.Add("Music/Album/01 One.flac", []byte("fLaC-one-0123456789"))
	g.Add("Music/Album/02 Two.flac", []byte("fLaC-two"))
	g.Add("Music/Album/cover.jpg", []byte("jpg"))
	g.Add("Music/@eaDir/x.flac", []byte("x"))
	g.Add("Music/#recycle/x.flac", []byte("x"))
	g.Add("Music/@onefive/01.flac", []byte("fLaC-at")) // music, not a system's folder (review #223)
	g.Add("Other/y.flac", []byte("y"))
	s, _ := newSet(t)
	st := signIn(t, s, g, Source)
	here, list, err := s.OneDriveFolders(ctx, st.ID, "")
	if err != nil || here.Path != "/" || len(list) != 2 || list[0].Name != "Music" || list[0].Path != "/Music" {
		t.Fatalf("the root's folders: %+v %+v %v", here, list, err)
	}
	if _, err := s.OneDriveMakeFolder(ctx, st.ID, "", "New"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a folder made for a source: %v", err)
	}
	if st, err = s.SetFolder(ctx, st.ID, music); err != nil || st.State != StateOK || st.Usage == nil || st.Usage.Free != 4<<30 {
		t.Fatalf("the folder chosen: %+v %v", st, err)
	}
	b, _, _ := s.Backend(ctx, st.ID)
	g.Fail = func(r *http.Request) int { // throttled twice
		if strings.HasSuffix(r.URL.Path, "/children") && g.Requests["children"] < 2 {
			g.Requests["children"]++
			return http.StatusTooManyRequests
		}
		return 0
	}
	var objs []Object
	if err := b.Walk(ctx, func(o Object) error { objs = append(objs, o); return nil }); err != nil {
		t.Fatal(err)
	}
	g.Fail = nil
	var paths []string
	for _, o := range objs {
		paths = append(paths, o.Path)
	}
	slices.Sort(paths)
	i := slices.IndexFunc(objs, func(o Object) bool { return o.Path == "Album/01 One.flac" })
	if strings.Join(paths, ",") != "@onefive/01.flac,Album/01 One.flac,Album/02 Two.flac,Album/cover.jpg" || i < 0 ||
		objs[i].Key != one || !strings.HasPrefix(objs[i].Hash, "quickxor:") || objs[i].Version == "" {
		t.Fatalf("walked %+v", objs)
	}
	ref := Ref(KindOneDrive, st.ID, one)
	if got := read(t, s, ref, 5, 7); got != "one" {
		t.Fatalf("range: %q", got)
	}
	links := g.Requests["item"]
	read(t, s, ref, 0, 3)
	if g.Requests["item"] != links {
		t.Fatal("a download link asked for again while it works")
	}
	g.ExpireLinks()
	if got := read(t, s, ref, 0, -1); got != "fLaC-one-0123456789" {
		t.Fatalf("after the link expired: %q", got)
	}
	// The fetched copy checks against OneDrive's own checksum.
	c := NewChecker(objs[i])
	c.Write([]byte("fLaC-one-0123456789"))
	if !c.Matches() {
		t.Fatal("quickXorHash does not match")
	}
	// An access token ended: refreshed, and the refresh token given back kept.
	before := g.Refresh()
	g.Expire()
	if _, err := s.Stat(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if after := g.Refresh(); after == before {
		t.Fatal("not refreshed")
	} else if sec, _ := s.oneDriveSecret(ctx, st.ID); sec.Refresh != after {
		t.Fatalf("kept refresh token %q, want %q", sec.Refresh, after)
	}
	// Never written; a source's file let go of stays.
	if _, err := b.(Writer).Put(ctx, PutFile{}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("put: %v", err)
	}
	if err := s.Trash(ctx, ref); err != nil || g.Get(one).Deleted {
		t.Fatalf("a source's file deleted: %v", err)
	}
	// Moved in OneDrive: the same key, its new path.
	g.Move(one, g.Find("Music").ID, "01 One (moved).flac")
	objs = nil
	b.Walk(ctx, func(o Object) error { objs = append(objs, o); return nil })
	if objs[0].Key != one || objs[0].Path != "01 One (moved).flac" {
		t.Fatalf("after the move %+v", objs)
	}
	// Gone: not found.
	g.Delete(one)
	if _, err := s.Stat(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted in OneDrive: %v", err)
	}
	// Signed out elsewhere: the refresh token refused.
	g.Expire()
	s.DB.Exec(`UPDATE stores SET secret = json_set(secret, '$.refresh_token', 'revoked') WHERE id = ?`, st.ID)
	_, err = s.Open(ctx, Ref(KindOneDrive, st.ID, g.Find("Music/Album/02 Two.flac").ID), 0, -1)
	if reason, lasting := Explain(err); !errors.Is(err, ErrAuth) || reason != "store_auth" || !lasting {
		t.Fatalf("revoked: %v (%s)", err, reason)
	}
	if st, _ := s.Check(ctx, st.ID); st.State != StateError || !strings.Contains(st.Error, "invalid_grant") {
		t.Fatalf("recorded %+v", st)
	}
}

// A file Graph gives without its download link (it has left it out of a personal OneDrive's files,
// which were then taken for gone: their songs and covers 503) is read through its /content: from the
// link it redirects to, kept for the next reads and asked for again when it expires, or from the
// bytes it sends itself. One gone is not there still.
func TestOneDriveNoLinks(t *testing.T) {
	ctx := context.Background()
	g := fakeGraph(t)
	music := g.Add("Music", nil)
	one := g.Add("Music/Album/01 One.flac", []byte("fLaC-one-0123456789"))
	two := g.Add("Music/Album/02 Two.flac", []byte("fLaC-two"))
	s, _ := newSet(t)
	st := signIn(t, s, g, Source)
	if _, err := s.SetFolder(ctx, st.ID, music); err != nil {
		t.Fatal(err)
	}
	g.NoLinks = true
	ref := Ref(KindOneDrive, st.ID, one)
	if got := read(t, s, ref, 5, 7); got != "one" || g.Requests["content"] != 1 {
		t.Fatalf("a range without a link: %q (%d asked)", got, g.Requests["content"])
	}
	if got := read(t, s, ref, 0, -1); got != "fLaC-one-0123456789" || g.Requests["content"] != 1 {
		t.Fatalf("the link it redirected to not kept: %q (%d asked)", got, g.Requests["content"])
	}
	g.ExpireLinks()
	if got := read(t, s, ref, 9, -1); got != "0123456789" || g.Requests["content"] != 2 {
		t.Fatalf("after the link expired: %q (%d asked)", got, g.Requests["content"])
	}
	g.ContentServes = true
	g.ExpireLinks()
	if got := read(t, s, ref, 5, 7); got != "one" {
		t.Fatalf("a range sent by /content: %q", got)
	}
	if got := read(t, s, Ref(KindOneDrive, st.ID, two), 0, -1); got != "fLaC-two" {
		t.Fatalf("a file sent by /content: %q", got)
	}
	for _, links := range []bool{true, false} {
		g.NoLinks, g.ContentServes = links, false
		g.Delete(two)
		g.ExpireLinks()
		if _, err := s.Open(ctx, Ref(KindOneDrive, st.ID, two), 0, -1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted in OneDrive (no links %v): %v", links, err)
		}
	}
}

// A OneDrive as a managed store: a folder made and chosen; files kept with names OneDrive takes,
// the same file once, another under a free name, large ones in upload sessions that resume, each
// checked against OneDrive's quickXorHash of it; deleted to the recycle bin.
func TestOneDriveManaged(t *testing.T) {
	ctx := context.Background()
	g := fakeGraph(t)
	odChunk = 10
	s, data := newSet(t)
	st := signIn(t, s, g, Managed)
	f, err := s.OneDriveMakeFolder(ctx, st.ID, "", "Kanade")
	if err != nil || f.Path != "/Kanade" {
		t.Fatalf("made %+v %v", f, err)
	}
	if again, err := s.OneDriveMakeFolder(ctx, st.ID, "", "Kanade"); err != nil || again.ID != f.ID {
		t.Fatalf("made again: %+v %v", again, err)
	}
	if _, err := s.SetFolder(ctx, st.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	dest, err := s.Destination(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	p, sha := file(t, data, "a.flac", "fLaC-small")
	ref, err := dest.Put(ctx, PutFile{Path: p, Dir: "library/Re:Zero/Album?", Name: "01 \"One\".flac", Size: 10, SHA256: sha})
	if err != nil {
		t.Fatal(err)
	}
	it := g.Find("Kanade/library/Re：Zero/Album？/01 ＂One＂.flac")
	if it == nil || string(it.Data) != "fLaC-small" || ref != Ref(KindOneDrive, st.ID, it.ID) {
		t.Fatalf("kept as %q: %+v", ref, it)
	}
	sessions := g.Requests["create session"]
	if again, err := dest.Put(ctx, PutFile{Path: p, Dir: "library/Re:Zero/Album?", Name: "01 \"One\".flac", Size: 10, SHA256: sha}); err != nil ||
		again != ref || g.Requests["create session"] != sessions {
		t.Fatalf("the same again: %q %v", again, err)
	}
	p2, sha2 := file(t, data, "b.flac", "fLaC-other")
	if other, err := dest.Put(ctx, PutFile{Path: p2, Dir: "library/Re:Zero/Album?", Name: "01 \"One\".flac", Size: 10, SHA256: sha2}); err != nil ||
		g.Find("Kanade/library/Re：Zero/Album？/01 ＂One＂ (2).flac") == nil || other == ref {
		t.Fatalf("other content: %q %v", other, err)
	}

	// In pieces: the third refused, then resumed from where OneDrive says.
	big := strings.Repeat("0123456789", 5) + "end"
	p3, sha3 := file(t, data, "c.flac", big)
	ses := &testSessions{}
	failed := false
	g.Fail = func(r *http.Request) int {
		if strings.HasPrefix(r.URL.Path, "/upload/") && r.Method == http.MethodPut && strings.HasPrefix(r.Header.Get("Content-Range"), "bytes 20-") && !failed {
			failed = true
			return http.StatusBadRequest
		}
		return 0
	}
	big3 := PutFile{Path: p3, Dir: "library/Band/Album", Name: "02.flac", Size: int64(len(big)), SHA256: sha3, Sessions: ses}
	if _, err := dest.Put(ctx, big3); err == nil || ses.v == "" {
		t.Fatalf("a piece refused: %v, session %q", err, ses.v)
	}
	uploads := g.Requests["upload"]
	var sent []int64
	big3.Progress = func(n, _ int64) { sent = append(sent, n) }
	ref3, err := dest.Put(ctx, big3)
	if err != nil {
		t.Fatalf("resumed: %v", err)
	}
	if it := g.Get(strings.TrimPrefix(ref3, "onedrive:1:")); it == nil || string(it.Data) != big {
		t.Fatalf("the large file: %+v", it)
	}
	if n := g.Requests["upload"] - uploads; n != 5 || sent[0] <= 20 { // the status, then pieces 20-29 to 50-52
		t.Fatalf("resumed with %d requests, progress %v", n, sent)
	}
	if ses.v != "" || g.Sessions() != 0 {
		t.Fatalf("left: %q, %d sessions", ses.v, g.Sessions())
	}
	// A session OneDrive let go of: started again.
	ses.v = `{"url":"` + g.URL + `/upload/999","sha256":"` + sha3 + `","size":53}`
	big3.Name = "03.flac"
	if _, err := dest.Put(ctx, big3); err != nil || g.Find("Kanade/library/Band/Album/03.flac") == nil {
		t.Fatalf("after the session was gone: %v", err)
	}

	// A new client secret: tried before it is kept (and the refresh token it brings kept).
	if _, err := s.SetKeys(ctx, st.ID, json.RawMessage(`{"client_secret":"wrong"}`)); !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("a wrong client secret: %v", err)
	}
	g.ClientSecret = "new-secret"
	before := g.Refresh()
	if st, err := s.SetKeys(ctx, st.ID, json.RawMessage(`{"client_secret":" new-secret "}`)); err != nil || st.State != StateOK {
		t.Fatalf("a new client secret: %+v %v", st, err)
	}
	if sec, _ := s.oneDriveSecret(ctx, st.ID); sec.ClientSecret != "new-secret" || sec.Refresh == before || sec.Refresh != g.Refresh() {
		t.Fatalf("kept %+v", sec)
	}

	// Deleted: to the recycle bin.
	if err := s.Trash(ctx, ref); err != nil || !g.Get(strings.TrimPrefix(ref, "onedrive:1:")).Deleted {
		t.Fatalf("trash: %v", err)
	}
	if err := s.Trash(ctx, ref); err != nil {
		t.Fatalf("trashed again: %v", err)
	}
	var keys []string
	b, _, _ := s.Backend(ctx, st.ID)
	b.Walk(ctx, func(o Object) error { keys = append(keys, o.Path); return nil })
	if strings.Join(keys, ",") != "library/Band/Album/02.flac,library/Band/Album/03.flac,library/Re：Zero/Album？/01 ＂One＂ (2).flac" {
		t.Fatalf("walked %v", keys)
	}
}

// Microsoft's national clouds: a store of China's (21Vianet) signs in there; one not known is
// refused.
func TestOneDriveClouds(t *testing.T) {
	ctx := context.Background()
	s, _ := newSet(t)
	add := func(region string) (*Store, error) {
		cfg, _ := json.Marshal(OneDriveConfig{ClientID: "app", Region: region})
		return s.Add(ctx, NewStore{Kind: KindOneDrive, Name: "OneDrive " + region, Mode: Source, Config: cfg, Secret: json.RawMessage(`{"client_secret":"x"}`)})
	}
	st, err := add("cn")
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := s.OneDriveAuthURL(ctx, st.ID, odRedirect); err != nil || !strings.HasPrefix(raw, "https://login.chinacloudapi.cn/common/oauth2/v2.0/authorize?") {
		t.Fatalf("China's: %s %v", raw, err)
	}
	if st, err := add("global"); err != nil || strings.Contains(string(st.Config), "region") {
		t.Fatalf("global: %+v %v", st, err)
	}
	if _, err := add("de"); err == nil {
		t.Fatal("Germany's cloud (closed in 2021) taken")
	}
}

// A refresh that comes back after the sign-in it used was replaced keeps nothing of its own: a
// client secret given meanwhile, or a sign-in made meanwhile, stays (review #215).
func TestOneDriveLateRefresh(t *testing.T) {
	ctx := context.Background()
	g := fakeGraph(t)
	music := g.Add("Music", nil)
	one := g.Add("Music/01.flac", []byte("fLaC-one"))
	s, _ := newSet(t)
	st := signIn(t, s, g, Source)
	if _, err := s.SetFolder(ctx, st.ID, music); err != nil {
		t.Fatal(err)
	}
	ref := Ref(KindOneDrive, st.ID, one)
	kept := func() OneDriveSecret {
		t.Helper()
		sec, err := s.oneDriveSecret(ctx, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		return sec
	}
	// late has a refresh go out (the access token ended) and come back only after during.
	late := func(during func()) {
		t.Helper()
		held, release := make(chan struct{}), make(chan struct{})
		var first atomic.Bool // only the first refresh is held: the others (a backend signed in again) go on
		g.Hold = func(r *http.Request) {
			if r.Form.Get("grant_type") == "refresh_token" && first.CompareAndSwap(false, true) {
				close(held)
				<-release
			}
		}
		defer func() { g.Hold = nil }()
		g.Expire()
		done := make(chan error, 1)
		go func() { _, err := s.Stat(ctx, ref); done <- err }()
		<-held
		during()
		close(release)
		if err := <-done; err != nil {
			t.Fatal("the request that asked:", err)
		}
	}

	// The app's client secret replaced while a refresh is out.
	late(func() {
		g.ClientSecret = "the-new-secret"
		if _, err := s.SetKeys(ctx, st.ID, json.RawMessage(`{"client_secret":"the-new-secret"}`)); err != nil {
			t.Fatal(err)
		}
	})
	if sec := kept(); sec.ClientSecret != "the-new-secret" || sec.Refresh != g.Refresh() {
		t.Fatalf("after a late refresh: the client secret %q, the refresh token the last given %v", sec.ClientSecret, sec.Refresh == g.Refresh())
	}
	g.Expire()
	if _, err := s.Stat(ctx, ref); err != nil {
		t.Fatal("refreshed with the new secret:", err)
	}

	// Signed in again while a refresh is out.
	var again string
	late(func() {
		g.Code = "the-code-again"
		raw, err := s.OneDriveAuthURL(ctx, st.ID, odRedirect)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(raw)
		if _, err := s.OneDriveComplete(ctx, url.Values{"state": {u.Query().Get("state")}, "code": {g.Code}}, odRedirect); err != nil {
			t.Fatal(err)
		}
		again = kept().Refresh
	})
	if sec := kept(); sec.Refresh != again || sec.ClientSecret != "the-new-secret" {
		t.Fatalf("after a late refresh: the sign-in made meanwhile replaced (%v)", sec.Refresh == again)
	}
	g.Expire()
	if _, err := s.Stat(ctx, ref); err != nil {
		t.Fatal("refreshed with the sign-in made meanwhile:", err)
	}
}

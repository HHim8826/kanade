package bangumi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

func TestRef(t *testing.T) {
	for in, want := range map[string]int64{
		"531": 531, " 531 ": 531, "https://bgm.tv/subject/531": 531, "http://bangumi.tv/subject/1269?x=1": 1269,
		"chii.in/subject/12/": 12, "https://www.bgm.tv/subject/7#a": 7,
		"ARIA": 0, "https://example.com/subject/531": 0, "0": 0, "-3": 0, "https://bgm.tv/person/531": 0,
	} {
		got, ok := Ref(in)
		if (want == 0) == ok || (ok && got != want) {
			t.Errorf("Ref(%q) = %d %v, want %d", in, got, ok, want)
		}
	}
}

// Only Bangumi's pictures are fetched: an address a client names cannot make the server reach
// another host.
func TestOpenImageOnlyBangumi(t *testing.T) {
	c := New("test")
	for _, u := range []string{"http://lain.bgm.tv/a.jpg", "https://example.com/a.jpg", "https://bgm.tv.example.com/a.jpg", "file:///etc/passwd",
		"https://127.0.0.1/a.jpg", "https://evilbgm.tv/a.jpg"} {
		if _, err := c.OpenImage(context.Background(), u); !errors.Is(err, ErrImage) {
			t.Errorf("%s: %v", u, err)
		}
	}
}

// An error page sent as a success is not kept: asked again once Bangumi is back, the answer is
// right away (review #179).
func TestBadAnswerNotKept(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch asked.Add(1) {
		case 1:
			w.Write([]byte("<html>busy</html>"))
		case 2:
			w.Write([]byte(`{"id":532,"type":2,"name":"Another"}`)) // not the one asked for
		default:
			if r.Method == http.MethodPost {
				w.Write([]byte(`{"total":0,"data":[]}`))
				return
			}
			w.Write([]byte(`{"id":531,"type":2,"name":"ARIA"}`))
		}
	}))
	defer srv.Close()
	c := New("test")
	c.Base, c.Gap = srv.URL, 0
	ctx := context.Background()
	for range 2 {
		if _, err := c.Subject(ctx, 531); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("a bad answer: %v", err)
		}
	}
	if s, err := c.Subject(ctx, 531); err != nil || s.Name != "ARIA" {
		t.Fatalf("back: %v %v", s, err)
	}
	if _, err := c.Subject(ctx, 531); err != nil || asked.Load() != 3 {
		t.Fatalf("a good answer is kept: %v, asked %d times", err, asked.Load())
	}
	// No result is an answer, kept as one.
	for range 2 {
		if p, err := c.Search(ctx, "nothing", nil, 0); err != nil || p.Total != 0 || len(p.Subjects) != 0 {
			t.Fatalf("search %+v %v", p, err)
		}
	}
	if asked.Load() != 4 {
		t.Fatalf("asked %d times", asked.Load())
	}
}

// A renewal that answers after the account was linked again leaves the new link alone, and does
// not mark it refused (review #172).
func TestLateRenewalAfterLinkingAgain(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES (1, 'a', 'x', 0)`)
	gate, asked := make(chan struct{}), make(chan struct{}, 1)
	refuse := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- struct{}{}
		<-gate
		if refuse.Load() {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		w.Write([]byte(`{"access_token":"old-acc2","refresh_token":"old-ref2","expires_in":604800,"user_id":7}`))
	}))
	defer srv.Close()
	c := New("test")
	c.Site, c.Gap = srv.URL, 0
	a := NewAccounts(d, c, "https://music.example")
	a.SetApp(ctx, "bgm1", "sec")
	for _, refused := range []bool{false, true} {
		refuse.Store(refused)
		d.Exec(`DELETE FROM bangumi_links`)
		d.Exec(`INSERT INTO bangumi_links (user_id, bgm_id, username, nickname, access_token, refresh_token, expires_at, linked_at)
			VALUES (1, 7, 'old', '', 'old-acc', 'old-ref', 0, 1)`)
		got := make(chan error, 1)
		go func() {
			_, err := a.Session(ctx, 1)
			got <- err
		}()
		<-asked
		d.Exec(`UPDATE bangumi_links SET bgm_id = 8, username = 'new', access_token = 'new-acc', refresh_token = 'new-ref', expires_at = ?, linked_at = 2`,
			time.Now().Add(72*time.Hour).UnixMilli())
		gate <- struct{}{}
		if err := <-got; err != nil {
			t.Fatalf("refused %v: %v", refused, err)
		}
		l, _ := a.Link(ctx, 1)
		if l.Username != "new" || l.Access != "new-acc" || l.Refresh != "new-ref" || l.Error != "" {
			t.Fatalf("refused %v: the old renewal changed the new link: %+v", refused, l)
		}
		a.Failed(1, &Session{Access: "old-acc"})
		if l, _ := a.Link(ctx, 1); l.Error != "" {
			t.Fatal("an old token refused marked the new link")
		}
	}
}

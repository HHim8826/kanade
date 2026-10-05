package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
)

// fakeBgmAccount is Bangumi's OAuth and the API of one person (ser1ka) and their collections.
type fakeBgmAccount struct {
	mu       sync.Mutex
	access   string
	expired  bool
	collects map[string]map[string]any // by subject
	posted   []map[string]any
}

func (f *fakeBgmAccount) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		authed := r.Header.Get("Authorization") == "Bearer "+f.access && !f.expired
		switch {
		case r.URL.Path == "/oauth/access_token":
			r.ParseForm()
			if r.Form.Get("client_secret") != "sec" || r.Form.Get("code") != "good" || !strings.HasSuffix(r.Form.Get("redirect_uri"), "/oauth/bangumi/callback") {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			w.Write([]byte(`{"access_token":"tok","expires_in":604800,"token_type":"Bearer","refresh_token":"ref","user_id":7}`))
		case r.URL.Path == "/v0/me":
			if !authed {
				http.Error(w, "{}", 401)
				return
			}
			w.Write([]byte(`{"id":7,"username":"ser1ka","nickname":"Seri"}`))
		case r.URL.Path == "/v0/subjects/531":
			w.Write([]byte(`{"id":531,"type":2,"name":"ARIA The ANIMATION","images":null}`))
		case r.URL.Path == "/v0/subjects/9001":
			w.Write([]byte(`{"id":9001,"type":3,"name":"Rainbow","name_cn":"","date":"2005-11-23","platform":"Single","images":null,
				"rating":{"rank":0,"total":12,"score":7.9},"tags":[{"name":"ARIA","count":9},{"name":"OP ED","count":3},{"name":"2005","count":2}]}`))
		case r.URL.Path == "/v0/users/ser1ka/collections/9001":
			if !authed {
				http.Error(w, "{}", 401)
				return
			}
			c, ok := f.collects["9001"]
			if !ok {
				http.Error(w, `{"title":"Not Found"}`, 404)
				return
			}
			json.NewEncoder(w).Encode(c)
		case r.URL.Path == "/v0/users/-/collections/9001" && r.Method == "POST":
			if !authed {
				http.Error(w, "{}", 401)
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.posted = append(f.posted, body)
			body["subject_id"], body["subject_type"] = 9001, 3
			f.collects["9001"] = body
			w.WriteHeader(204)
		case r.URL.Path == "/v0/users/ser1ka/collections":
			if !authed {
				http.Error(w, "{}", 401)
				return
			}
			if r.URL.Query().Get("subject_type") != "3" {
				t.Errorf("collections of %s", r.URL.RawQuery)
			}
			data := []map[string]any{{"subject_id": 9001, "type": 2, "rate": 9, "tags": []string{"鱼韵", "OST"}, "subject": map[string]any{"id": 9001, "type": 3, "name": "Rainbow", "score": 7.9}},
				{"subject_id": 9002, "type": 1, "rate": 0, "tags": []string{"OST"}, "subject": map[string]any{"id": 9002, "type": 3, "name": "Not here"}}}
			json.NewEncoder(w).Encode(map[string]any{"total": 2, "limit": 50, "offset": 0, "data": data})
		default:
			http.NotFound(w, r)
		}
	})
}

// A Bangumi account (review #94): the application set without the secret ever coming back; linked
// by OAuth with a state only it knows; an album made a music subject (not an anime); its collection
// read and written only as asked, with the subject's and the owner's tags to pick from; the owner's
// collections listed with the library's albums of them; a token no longer accepted says so.
func TestBangumiAccount(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	f := &fakeBgmAccount{access: "tok", collects: map[string]map[string]any{}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	s.bgm.Base, s.bgm.Site, s.bgm.Gap = srv.URL, srv.URL, 0

	call := func(method, path string, body any, out any) int {
		t.Helper()
		rec := do(t, h, method, path, tok, body)
		if out != nil {
			json.Unmarshal(rec.Body.Bytes(), out)
		}
		return rec.Code
	}
	if code := call("POST", "/api/v1/bangumi/link", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("no application: %d", code)
	}
	call("PUT", "/api/v1/bangumi/app", map[string]string{"app_id": "bgm123", "app_secret": "sec"}, nil)
	rec := do(t, h, "GET", "/api/v1/bangumi/account", tok, nil)
	if strings.Contains(rec.Body.String(), `"sec"`) || !strings.Contains(rec.Body.String(), `"has_secret":true`) {
		t.Fatalf("account %s", rec.Body)
	}
	var begun struct{ URL string }
	call("POST", "/api/v1/bangumi/link", nil, &begun)
	u, _ := url.Parse(begun.URL)
	if u.Path != "/oauth/authorize" || u.Query().Get("client_id") != "bgm123" || u.Query().Get("state") == "" {
		t.Fatalf("authorize %s", begun.URL)
	}
	for q, want := range map[string]string{"?state=forged&code=good": "expired", "?error=access_denied": "denied"} {
		if rec := do(t, h, "GET", "/oauth/bangumi/callback"+q, "", nil); rec.Header().Get("Location") != "/app/#/settings?bangumi="+want {
			t.Fatalf("%s: %s", q, rec.Header().Get("Location"))
		}
	}
	if rec := do(t, h, "GET", "/oauth/bangumi/callback?state="+u.Query().Get("state")+"&code=good", "", nil); rec.Header().Get("Location") != "/app/#/settings?bangumi=linked" {
		t.Fatalf("linked: %s", rec.Header().Get("Location"))
	}
	var acct struct {
		Link struct{ Username, Nickname string }
	}
	if call("GET", "/api/v1/bangumi/account", nil, &acct); acct.Link.Username != "ser1ka" {
		t.Fatalf("link %+v", acct)
	}

	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "r", Size: 1, Format: "flac", Codec: "flac"})
	s.lib.MarkVerified(ctx, a.ID, "d-r")
	pub, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "Rainbow", Artist: "Round Table", Album: "Rainbow", AlbumArtist: "Round Table"})
	var album int64
	s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, pub.EntryID).Scan(&album)
	path := "/api/v1/albums/" + itoa(album)
	if code := call("GET", path+"/collection", nil, nil); code != http.StatusNotFound {
		t.Fatalf("no subject yet: %d", code)
	}
	if code := call("PUT", path+"/subject", map[string]string{"source_id": "531"}, nil); code != http.StatusBadRequest {
		t.Fatalf("an anime as the album's subject: %d", code)
	}
	var set struct{ Group int64 }
	if code := call("PUT", path+"/subject", map[string]string{"source_id": "https://bgm.tv/subject/9001"}, &set); code != 200 || set.Group == 0 {
		t.Fatalf("subject: %d", code)
	}
	var detail library.AlbumDetail
	if call("GET", path, nil, &detail); detail.Subject == nil || detail.Subject.Name != "Rainbow" || detail.Subject.Score != 7.9 {
		t.Fatalf("album subject %+v", detail.Subject)
	}
	var col struct {
		Linked     bool
		Collection *struct {
			Type, Rate int
			Tags       []string
		}
		SubjectTags []string `json:"subject_tags"`
		MyTags      []string `json:"my_tags"`
	}
	if call("GET", path+"/collection", nil, &col); !col.Linked || col.Collection != nil || strings.Join(col.SubjectTags, ",") != "ARIA,2005" ||
		strings.Join(col.MyTags, ",") != "OST,鱼韵" {
		t.Fatalf("collection %+v", col)
	}
	if len(f.posted) != 0 {
		t.Fatal("reading wrote")
	}
	for _, bad := range []map[string]any{{"type": 0}, {"type": 2, "rate": 11}, {"type": 2, "tags": []string{"has space"}}} {
		if code := call("PUT", path+"/collection", bad, nil); code != http.StatusBadRequest {
			t.Fatalf("%v: %d", bad, code)
		}
	}
	if code := call("PUT", path+"/collection", map[string]any{"type": 2, "rate": 9, "comment": " 好聽 ", "private": true, "tags": []string{"ARIA", "鱼韵", "ARIA"}}, nil); code != 204 {
		t.Fatalf("write: %d", code)
	}
	if len(f.posted) != 1 || f.posted[0]["type"] != 2.0 || f.posted[0]["comment"] != "好聽" || f.posted[0]["private"] != true ||
		len(f.posted[0]["tags"].([]any)) != 2 {
		t.Fatalf("posted %v", f.posted)
	}
	if call("GET", path+"/collection", nil, &col); col.Collection == nil || col.Collection.Rate != 9 {
		t.Fatalf("read back %+v", col)
	}
	var list struct {
		Total int
		Items []struct {
			SubjectID int64 `json:"subject_id"`
			Name      string
			Albums    []library.AlbumSummary
		}
	}
	if call("GET", "/api/v1/bangumi/collections?type=2", nil, &list); list.Total != 2 || len(list.Items) != 2 || list.Items[0].Name != "Rainbow" ||
		len(list.Items[0].Albums) != 1 || list.Items[0].Albums[0].ID != album || len(list.Items[1].Albums) != 0 {
		t.Fatalf("collections %+v", list)
	}
	// Bound by an edit: undone.
	if code := call("POST", "/api/v1/edits/"+itoa(set.Group)+"/undo", nil, nil); code != 200 {
		t.Fatalf("undo: %d", code)
	}
	if call("GET", path, nil, &detail); detail.Subject != nil {
		t.Fatal("undone subject")
	}
	call("PUT", path+"/subject", map[string]string{"source_id": "9001"}, nil)
	// A token no longer accepted (and not renewed: it is not near its end): the link says so.
	f.mu.Lock()
	f.expired = true
	f.mu.Unlock()
	var e struct{ Reason string }
	if code := call("GET", path+"/collection", nil, &e); code != http.StatusConflict || e.Reason != "not_linked" {
		t.Fatalf("expired: %d %+v", code, e)
	}
	var after struct{ Link struct{ Error string } }
	if call("GET", "/api/v1/bangumi/account", nil, &after); after.Link.Error == "" {
		t.Fatal("the link does not say it failed")
	}
}

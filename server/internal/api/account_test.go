package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// An account sees and ends its own logins only, never their tokens; it changes its password with
// the current one, which ends every login and keeps its passkeys (review #76).
func TestAccountLoginsAndPassword(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	login := func(user, password, device string) string {
		t.Helper()
		rec := do(t, h, "POST", "/api/v1/login", "", map[string]string{"username": user, "password": password, "device": device})
		var lr struct{ Token string }
		json.Unmarshal(rec.Body.Bytes(), &lr)
		return lr.Token
	}
	for _, u := range []string{"admin", "other"} {
		if err := s.auth.CreateUser(ctx, u, "correct horse battery", false); err != nil {
			t.Fatal(err)
		}
	}
	here := login("admin", "correct horse battery", "web: here")
	phone := login("admin", "correct horse battery", "phone")
	tablet := login("admin", "correct horse battery", "tablet")
	theirs := login("other", "correct horse battery", "theirs")
	s.db.Exec(`INSERT INTO passkeys (user_id, credential_id, public_key, name, created_at) VALUES (1, x'01', x'02', 'key', 0)`)

	type session struct {
		ID      int64
		Name    string
		Current bool
	}
	list := func(token string) ([]session, string) {
		t.Helper()
		rec := do(t, h, "GET", "/api/v1/sessions", token, nil)
		var l []session
		json.Unmarshal(rec.Body.Bytes(), &l)
		return l, rec.Body.String()
	}
	mine, raw := list(here)
	if len(mine) != 3 || strings.Contains(raw, "token") || strings.Contains(raw, here) {
		t.Fatalf("sessions %s", raw)
	}
	ids := map[string]int64{}
	for _, v := range mine {
		ids[v.Name] = v.ID
		if v.Current != (v.Name == "web: here") {
			t.Fatalf("current: %+v", v)
		}
	}
	other, _ := list(theirs)
	end := func(token string, id int64) int {
		return do(t, h, "DELETE", "/api/v1/sessions/"+strconv.FormatInt(id, 10), token, nil).Code
	}
	if code := end(here, other[0].ID); code != http.StatusNotFound {
		t.Fatalf("another account's login: %d", code)
	}
	if code := end(here, ids["web: here"]); code != http.StatusConflict {
		t.Fatalf("this login: %d", code)
	}
	if code := end(here, ids["phone"]); code != http.StatusNoContent {
		t.Fatalf("end phone: %d", code)
	}
	works := func(token string) bool { return do(t, h, "GET", "/api/v1/sessions", token, nil).Code == http.StatusOK }
	if works(phone) || !works(tablet) || !works(here) || !works(theirs) {
		t.Fatal("ending one login")
	}
	login("admin", "correct horse battery", "laptop")
	rec := do(t, h, "POST", "/api/v1/sessions/end-others", here, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ended":2`) || works(tablet) || !works(here) || !works(theirs) {
		t.Fatalf("end others: %d %s", rec.Code, rec.Body)
	}

	change := func(current, next string) int {
		return do(t, h, "POST", "/api/v1/account/password", here, map[string]string{"current": current, "password": next}).Code
	}
	if code := change("wrong password!", "a new password 1"); code != http.StatusForbidden {
		t.Fatalf("wrong current: %d", code)
	}
	if code := change("correct horse battery", "short"); code != http.StatusBadRequest {
		t.Fatalf("short: %d", code)
	}
	if code := change("correct horse battery", "correct horse battery"); code != http.StatusBadRequest {
		t.Fatalf("same: %d", code)
	}
	second := login("admin", "correct horse battery", "second")
	if code := change("correct horse battery", "a new password 1"); code != http.StatusNoContent {
		t.Fatalf("change: %d", code)
	}
	if works(here) || works(second) || !works(theirs) {
		t.Fatal("logins after the change")
	}
	if login("admin", "correct horse battery", "x") != "" || login("admin", "a new password 1", "x") == "" {
		t.Fatal("logging in after the change")
	}
	var keys int
	s.db.QueryRow(`SELECT count(*) FROM passkeys WHERE user_id = 1`).Scan(&keys)
	if keys != 1 {
		t.Fatal("passkeys gone")
	}
	// Wrong current passwords are throttled like logins.
	fresh := login("admin", "a new password 1", "y")
	codes := []int{}
	for i := 0; i < 6; i++ {
		codes = append(codes, do(t, h, "POST", "/api/v1/account/password", fresh, map[string]string{"current": "nope nope nope", "password": "another one 22"}).Code)
	}
	if codes[5] != http.StatusTooManyRequests {
		t.Fatalf("throttle: %v", codes)
	}
}

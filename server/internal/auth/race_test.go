package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/webauthn"
	"github.com/HHim8826/kanade/server/internal/webauthn/webauthntest"
)

// Logins and the changes that end them, in the orders a race could put them: a login is checked
// (checkLogin, checkAssertion) and then made (passwordSession, passkeySession); the tests put the
// change in between.

const password = "correct horse battery"

var rp = webauthn.RP{ID: "music.example", Origin: "https://music.example"}

func sessions(t *testing.T, s *Service) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// withPasskey makes the account admin (ID 1) with a passkey from dev.
func withPasskey(t *testing.T, s *Service, dev *webauthntest.Authenticator) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateUser(ctx, "admin", password, false); err != nil {
		t.Fatal(err)
	}
	ch, err := s.RegisterChallenge(ctx, 1, password, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	cd, att := dev.Create(rp.ID, rp.Origin, ch)
	if _, err := s.AddPasskey(ctx, rp, 1, "Phone", cd, att); err != nil {
		t.Fatal(err)
	}
}

// answer is dev's answer to a new login challenge.
func answer(t *testing.T, s *Service, dev *webauthntest.Authenticator) (cd, ad, sig []byte) {
	t.Helper()
	ch, err := s.LoginChallenge("1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	return dev.Get(rp.ID, rp.Origin, ch)
}

func TestSessionsEndAfterTheirLifetime(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", password, false)
	token, err := s.Login(ctx, "admin", password, "", "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	// Used a minute ago, made just past the lifetime: still over.
	now := time.Now().UnixMilli()
	s.db.Exec(`UPDATE sessions SET created_at = ?, last_used_at = ?`, now-SessionLifetime.Milliseconds()-1000, now-60000)
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an expired login still works: %v", err)
	}
	if n := sessions(t, s); n != 0 {
		t.Fatalf("expired login kept: %d", n)
	}
	token, _ = s.Login(ctx, "admin", password, "", "1.1.1.1")
	s.db.Exec(`UPDATE sessions SET created_at = ?`, now-SessionLifetime.Milliseconds()+60000)
	if _, err := s.Authenticate(ctx, token); err != nil {
		t.Fatalf("a login a minute before its end: %v", err)
	}
}

// Guesses sent at once count while they are checked: only maxFailures get checked.
func TestThrottleCountsChecksUnderWay(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", password, false)
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Go(func() { _, errs[i] = s.Login(ctx, "admin", "wrong password!", "", "9.9.9.9") })
	}
	wg.Wait()
	checked := 0
	for _, err := range errs {
		switch {
		case errors.Is(err, ErrBadCredentials):
			checked++
		case !errors.Is(err, ErrThrottled):
			t.Fatal(err)
		}
	}
	if checked != maxFailures {
		t.Fatalf("%d of 20 guesses were checked, want %d", checked, maxFailures)
	}
	// The same budget covers the passkey's password confirmation and passkey logins.
	if _, err := s.RegisterChallenge(ctx, 1, password, "9.9.9.9"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("password confirmation: %v", err)
	}
	if _, err := s.PasskeyLogin(ctx, rp, nil, nil, nil, nil, nil, "", "9.9.9.9"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("passkey login: %v", err)
	}

	// A right password is not a failure, and a check under way is held only while it runs.
	done, err := s.attempt("7.7.7.7")
	if err != nil {
		t.Fatal(err)
	}
	done(nil)
	if s.checking["7.7.7.7"] != 0 || len(s.failures["7.7.7.7"]) != 0 {
		t.Fatalf("checking %v failures %v", s.checking, s.failures)
	}
}

func TestNewPasswordRefusesALoginCheckedBefore(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", password, false)
	id, hash, err := s.checkLogin(ctx, "admin", password)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "admin", "another long password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.passwordSession(ctx, id, hash, ""); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("login with the old password after the change: %v", err)
	}
	if n := sessions(t, s); n != 0 {
		t.Fatalf("%d sessions", n)
	}
	if _, err := s.Login(ctx, "admin", "another long password", "", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
}

func TestNewPasswordVoidsPasskeyRequests(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", password, false)
	ch, err := s.RegisterChallenge(ctx, 1, password, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "admin", "another long password"); err != nil {
		t.Fatal(err)
	}
	dev := webauthntest.New(webauthntest.ES256)
	cd, att := dev.Create(rp.ID, rp.Origin, ch)
	if _, err := s.AddPasskey(ctx, rp, 1, "Phone", cd, att); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("passkey confirmed with the old password: %v", err)
	}
	if has, _ := s.HasPasskeys(ctx); has {
		t.Fatal("passkey stored")
	}
}

func TestPasskeyChallenges(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", password, false)
	s.CreateUser(ctx, "other", password, false)
	dev := webauthntest.New(webauthntest.ES256)
	add := func(user int64, ch []byte) error {
		cd, att := dev.Create(rp.ID, rp.Origin, ch)
		_, err := s.AddPasskey(ctx, rp, user, "Phone", cd, att)
		return err
	}
	ch, _ := s.RegisterChallenge(ctx, 1, password, "1.1.1.1")
	if err := add(2, ch); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("another account's request: %v", err)
	}
	if err := add(1, ch); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("a request used by the try above: %v", err)
	}
	login, _ := s.LoginChallenge("1.1.1.1")
	if err := add(1, login); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("a login challenge for adding: %v", err)
	}
	ch, _ = s.RegisterChallenge(ctx, 1, password, "1.1.1.1")
	s.mu.Lock()
	c := s.challenges[string(ch)]
	c.expires = time.Now().Add(-time.Second)
	s.challenges[string(ch)] = c
	s.mu.Unlock()
	if err := add(1, ch); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("an expired request: %v", err)
	}
	ch, _ = s.RegisterChallenge(ctx, 1, password, "1.1.1.1")
	if err := add(1, ch); err != nil {
		t.Fatal(err)
	}
	if err := add(1, ch); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("the same request twice: %v", err)
	}
	// A registration challenge does not log in.
	ch, _ = s.RegisterChallenge(ctx, 1, password, "1.1.1.1")
	cd, ad, sig := dev.Get(rp.ID, rp.Origin, ch)
	if _, err := s.PasskeyLogin(ctx, rp, dev.ID, cd, ad, sig, nil, "", "1.1.1.1"); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("login with a registration challenge: %v", err)
	}
}

func TestRemovedPasskeyGetsNoSession(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	dev := webauthntest.New(webauthntest.ES256)
	withPasskey(t, s, dev)
	cd, ad, sig := answer(t, s, dev)
	a, err := s.checkAssertion(ctx, rp, dev.ID, cd, ad, sig, UserHandle(1))
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := s.Passkeys(ctx, 1)
	if err := s.DeletePasskey(ctx, 1, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.passkeySession(ctx, a, ""); !errors.Is(err, ErrNoPasskey) {
		t.Fatalf("login with a removed passkey: %v", err)
	}
	if n := sessions(t, s); n != 0 {
		t.Fatalf("%d sessions", n)
	}
}

// Two answers with one counter value, checked before either made its session: only one logs in.
// Passkeys that keep no counter (synced ones say 0) keep working.
func TestPasskeyCounterRace(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	dev := webauthntest.New(webauthntest.ES256)
	dev.Count = 4
	withPasskey(t, s, dev)
	dev.Count = 5
	var as []*assertion
	for range 2 {
		cd, ad, sig := answer(t, s, dev)
		a, err := s.checkAssertion(ctx, rp, dev.ID, cd, ad, sig, nil)
		if err != nil {
			t.Fatal(err)
		}
		as = append(as, a)
	}
	if _, err := s.passkeySession(ctx, as[0], ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.passkeySession(ctx, as[1], ""); !errors.Is(err, webauthn.ErrCloned) {
		t.Fatalf("second login with the same counter: %v", err)
	}
	dev.Count = 6
	cd, ad, sig := answer(t, s, dev)
	if _, err := s.PasskeyLogin(ctx, rp, dev.ID, cd, ad, sig, nil, "", "1.1.1.1"); err != nil {
		t.Fatalf("counter going up: %v", err)
	}

	synced := newService(t)
	dev = webauthntest.New(webauthntest.ES256)
	withPasskey(t, synced, dev)
	for range 2 {
		cd, ad, sig := answer(t, synced, dev)
		if _, err := synced.PasskeyLogin(ctx, rp, dev.ID, cd, ad, sig, nil, "", "1.1.1.1"); err != nil {
			t.Fatalf("passkey without a counter: %v", err)
		}
	}
}

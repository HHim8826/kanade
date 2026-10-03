package auth

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/webauthn"
	"github.com/HHim8826/kanade/server/internal/webauthn/webauthntest"
)

// Pause after a real SQLite read has finished, before the caller receives its result.
// This makes the credential-change races deterministic without production test hooks.
type readConnector struct {
	dsn  string
	hook func(string)
}

func (c readConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c readConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &readConn{Conn: conn, hook: c.hook}, nil
}

type readConn struct {
	driver.Conn
	hook func(string)
}

func (c *readConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *readConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &readRows{Rows: rows, done: func() { c.hook(query) }}, nil
}

type readRows struct {
	driver.Rows
	done func()
	once sync.Once
}

func (r *readRows) Close() error {
	err := r.Rows.Close()
	r.once.Do(r.done)
	return err
}

func controlledService(t *testing.T, hook func(string)) *Service {
	t.Helper()
	p := filepath.Join(t.TempDir(), "auth.sqlite")
	d, err := db.Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d = sql.OpenDB(readConnector{dsn: "file:" + p + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)", hook: hook})
	d.SetMaxOpenConns(32)
	t.Cleanup(func() { d.Close() })
	return New(d)
}

func waitRead(t *testing.T, read <-chan struct{}) {
	t.Helper()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("credential read did not reach the barrier")
	}
}

func TestSessionsExpireOnServer(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	if err := s.CreateUser(ctx, "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	token, err := s.Login(ctx, "admin", "correct horse battery", "test", "client")
	if err != nil {
		t.Fatal(err)
	}
	// A fresh last_used_at must not extend the absolute lifetime.
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET created_at = ?, last_used_at = ?`, db.Now()-int64(91*24*time.Hour/time.Millisecond), db.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired bearer/cookie token still authenticates: %v", err)
	}
}

func TestPasswordResetCannotBeRacedByOldPasswordLogin(t *testing.T) {
	ctx := context.Background()
	read, resume := make(chan struct{}), make(chan struct{})
	var pause, release sync.Once
	s := controlledService(t, func(q string) {
		if strings.Contains(q, "SELECT id, password_hash FROM users") {
			pause.Do(func() { close(read); <-resume })
		}
	})
	defer release.Do(func() { close(resume) })
	if err := s.CreateUser(ctx, "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	type result struct {
		token string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		token, err := s.Login(ctx, "admin", "correct horse battery", "old proof", "client")
		done <- result{token, err}
	}()
	waitRead(t, read)
	if err := s.SetPassword(ctx, "admin", "a different password"); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(resume) })
	r := <-done
	if r.token != "" || !errors.Is(r.err, ErrBadCredentials) {
		t.Fatalf("old password minted a session after reset: token issued=%v error=%v", r.token != "", r.err)
	}
}

func registeredDevice(t *testing.T, s *Service) (webauthn.RP, *webauthntest.Authenticator) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateUser(ctx, "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	rp := webauthn.RP{ID: "music.example", Origin: "https://music.example"}
	a := webauthntest.New(webauthntest.ES256)
	ch, err := s.NewRegistrationChallenge(ctx, 1, "correct horse battery", "registration")
	if err != nil {
		t.Fatal(err)
	}
	cd, att := a.Create(rp.ID, rp.Origin, ch)
	if _, err := s.AddPasskey(ctx, rp, 1, "test", cd, att); err != nil {
		t.Fatal(err)
	}
	return rp, a
}

func TestConcurrentPasskeyCountersAreCheckedAtSessionCreation(t *testing.T) {
	ctx := context.Background()
	read, resume := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var release sync.Once
	n := 0
	s := controlledService(t, func(q string) {
		if strings.Contains(q, "SELECT id, user_id, credential_id, public_key, sign_count") {
			mu.Lock()
			n++
			if n == 2 {
				close(read)
			}
			mu.Unlock()
			<-resume
		}
	})
	defer release.Do(func() { close(resume) })
	rp, a := registeredDevice(t, s)
	a.Count = 1 // two copies of the key answer different challenges with the same counter
	done := make(chan error, 2)
	for _, ip := range []string{"client1", "client2"} {
		ch, err := s.NewLoginChallenge(ip)
		if err != nil {
			t.Fatal(err)
		}
		cd, ad, sig := a.Get(rp.ID, rp.Origin, ch)
		go func() { _, err := s.PasskeyLogin(ctx, rp, a.ID, cd, ad, sig, UserHandle(1), "test", ip); done <- err }()
	}
	waitRead(t, read)
	release.Do(func() { close(resume) })
	success, cloned := 0, 0
	for range 2 {
		err := <-done
		switch {
		case err == nil:
			success++
		case errors.Is(err, webauthn.ErrCloned):
			cloned++
		default:
			t.Errorf("login: %v", err)
		}
	}
	if success != 1 || cloned != 1 {
		t.Fatalf("same counter accepted concurrently: success=%d cloned=%d", success, cloned)
	}
}

func TestDeletedPasskeyCannotFinishAnInFlightLogin(t *testing.T) {
	ctx := context.Background()
	read, resume := make(chan struct{}), make(chan struct{})
	var pause, release sync.Once
	s := controlledService(t, func(q string) {
		if strings.Contains(q, "SELECT id, user_id, credential_id, public_key, sign_count") {
			pause.Do(func() { close(read); <-resume })
		}
	})
	defer release.Do(func() { close(resume) })
	rp, a := registeredDevice(t, s)
	ch, err := s.NewLoginChallenge("client")
	if err != nil {
		t.Fatal(err)
	}
	cd, ad, sig := a.Get(rp.ID, rp.Origin, ch)
	done := make(chan error, 1)
	go func() {
		_, err := s.PasskeyLogin(ctx, rp, a.ID, cd, ad, sig, UserHandle(1), "test", "client")
		done <- err
	}()
	waitRead(t, read)
	if err := s.DeletePasskey(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(resume) })
	if err := <-done; !errors.Is(err, ErrNoPasskey) {
		t.Fatalf("deleted passkey completed login: %v", err)
	}
}

func TestParallelPasswordGuessesCannotBypassThrottle(t *testing.T) {
	ctx := context.Background()
	read, resume := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var release sync.Once
	n := 0
	s := controlledService(t, func(q string) {
		if strings.Contains(q, "SELECT id, password_hash FROM users") {
			mu.Lock()
			n++
			if n == maxFailures {
				close(read)
			}
			mu.Unlock()
			<-resume
		}
	})
	defer release.Do(func() { close(resume) })
	if err := s.CreateUser(ctx, "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 20)
	for range 20 {
		go func() { _, err := s.Login(ctx, "admin", "wrong password", "", "one address"); done <- err }()
	}
	waitRead(t, read)
	release.Do(func() { close(resume) })
	guesses, throttled := 0, 0
	for range 20 {
		err := <-done
		switch {
		case errors.Is(err, ErrBadCredentials):
			guesses++
		case errors.Is(err, ErrThrottled):
			throttled++
		default:
			t.Errorf("guess: %v", err)
		}
	}
	if guesses != maxFailures || throttled != 20-maxFailures {
		t.Fatalf("parallel throttle bypass: guesses=%d throttled=%d", guesses, throttled)
	}
}

func TestPasswordResetInvalidatesInFlightPasskeyRegistration(t *testing.T) {
	ctx := context.Background()
	read, resume := make(chan struct{}), make(chan struct{})
	var pause, release sync.Once
	s := controlledService(t, func(q string) {
		if strings.Contains(q, "SELECT count(*) FROM passkeys WHERE user_id") {
			pause.Do(func() { close(read); <-resume })
		}
	})
	defer release.Do(func() { close(resume) })
	if err := s.CreateUser(ctx, "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	ch, err := s.NewRegistrationChallenge(ctx, 1, "correct horse battery", "client")
	if err != nil {
		t.Fatal(err)
	}
	rp := webauthn.RP{ID: "music.example", Origin: "https://music.example"}
	a := webauthntest.New(webauthntest.ES256)
	cd, att := a.Create(rp.ID, rp.Origin, ch)
	done := make(chan error, 1)
	go func() { _, err := s.AddPasskey(ctx, rp, 1, "old confirmation", cd, att); done <- err }()
	waitRead(t, read)
	if err := s.SetPassword(ctx, "admin", "a different password"); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(resume) })
	if err := <-done; !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("registration authorized by the old password survived reset: %v", err)
	}
	keys, err := s.Passkeys(ctx, 1)
	if err != nil || len(keys) != 0 {
		t.Fatalf("unauthorized key stored: count=%d error=%v", len(keys), err)
	}
}

func TestPasskeyChallengesAreOneTimeExpiringAndAccountBound(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	rp, a := registeredDevice(t, s)
	ch, err := s.NewLoginChallenge("client")
	if err != nil {
		t.Fatal(err)
	}
	cd, ad, sig := a.Get(rp.ID, rp.Origin, ch)
	if _, err := s.PasskeyLogin(ctx, rp, a.ID, cd, ad, sig, UserHandle(1), "test", "client"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PasskeyLogin(ctx, rp, a.ID, cd, ad, sig, UserHandle(1), "test", "client"); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("assertion replay accepted: %v", err)
	}
	ch, err = s.NewLoginChallenge("client")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	c := s.challenges[string(ch)]
	c.expires = time.Now().Add(-time.Second)
	s.challenges[string(ch)] = c
	s.mu.Unlock()
	cd, ad, sig = a.Get(rp.ID, rp.Origin, ch)
	if _, err := s.PasskeyLogin(ctx, rp, a.ID, cd, ad, sig, UserHandle(1), "test", "client"); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("expired assertion accepted: %v", err)
	}
	if err := s.CreateUser(ctx, "second", "another long password", false); err != nil {
		t.Fatal(err)
	}
	ch, err = s.NewRegistrationChallenge(ctx, 1, "correct horse battery", "client")
	if err != nil {
		t.Fatal(err)
	}
	other := webauthntest.New(webauthntest.ES256)
	cd, att := other.Create(rp.ID, rp.Origin, ch)
	if _, err := s.AddPasskey(ctx, rp, 2, "wrong owner", cd, att); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("registration moved to another account: %v", err)
	}
	ch, err = s.NewLoginChallenge("client")
	if err != nil {
		t.Fatal(err)
	}
	cd, att = other.Create(rp.ID, rp.Origin, ch)
	if _, err := s.AddPasskey(ctx, rp, 1, "wrong purpose", cd, att); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("login challenge authorized registration: %v", err)
	}
}

func TestPasswordAndPasskeyChecksShareConcurrentLimit(t *testing.T) {
	ctx := context.Background()
	read, resume := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var release sync.Once
	n := 0
	s := controlledService(t, func(q string) {
		if strings.Contains(q, "SELECT id, password_hash FROM users") {
			mu.Lock()
			n++
			if n == maxFailures {
				close(read)
			}
			mu.Unlock()
			<-resume
		}
	})
	defer release.Do(func() { close(resume) })
	rp, a := registeredDevice(t, s)
	done := make(chan error, maxFailures)
	for range maxFailures {
		go func() { _, err := s.Login(ctx, "admin", "wrong password", "", "client"); done <- err }()
	}
	waitRead(t, read)
	ch, err := s.NewLoginChallenge("client")
	if err != nil {
		t.Fatal(err)
	}
	cd, ad, sig := a.Get(rp.ID, rp.Origin, ch)
	if _, err := s.PasskeyLogin(ctx, rp, a.ID, cd, ad, sig, UserHandle(1), "test", "client"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("passkey bypassed active password checks: %v", err)
	}
	if _, err := s.NewRegistrationChallenge(ctx, 1, "correct horse battery", "client"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("registration password bypassed active checks: %v", err)
	}
	release.Do(func() { close(resume) })
	for range maxFailures {
		if err := <-done; !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("password check: %v", err)
		}
	}
}

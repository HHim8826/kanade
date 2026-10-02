package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HHim8826/kanade/server/internal/db"
)

func newService(t *testing.T) *Service {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d)
}

func TestDatabaseFileIsPrivateAndMigrationsRerun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	for i := 0; i < 2; i++ { // second open must not reapply migrations
		d, err := db.Open(context.Background(), path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		d.Close()
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db mode = %o, want 600", perm)
	}
}

func TestSetupOnlyOnce(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	if err := s.CreateUser(ctx, "admin", "short", true); err == nil {
		t.Fatal("short password accepted")
	}
	if err := s.CreateUser(ctx, "admin", "correct horse battery", true); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, "intruder", "correct horse battery", true); !errors.Is(err, ErrUsersExist) {
		t.Fatalf("second setup: err = %v, want ErrUsersExist", err)
	}
}

func TestLoginAuthenticateLogout(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	if err := s.CreateUser(ctx, "admin", "correct horse battery", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "admin", "wrong password!", "", "1.2.3.4"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := s.Login(ctx, "nobody", "correct horse battery", "", "1.2.3.4"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	token, err := s.Login(ctx, "admin", "correct horse battery", "phone", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, err := s.Authenticate(ctx, token+"x"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("tampered token: %v", err)
	}
	if err := s.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after logout: %v", err)
	}
}

func TestThrottleAfterRepeatedFailures(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", "correct horse battery", false)
	for i := 0; i < maxFailures; i++ {
		s.Login(ctx, "admin", "wrong password!", "", "9.9.9.9")
	}
	if _, err := s.Login(ctx, "admin", "correct horse battery", "", "9.9.9.9"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("throttled IP with right password: %v", err)
	}
	if _, err := s.Login(ctx, "admin", "correct horse battery", "", "8.8.8.8"); err != nil {
		t.Fatalf("other IP should be unaffected: %v", err)
	}
}

func TestNewPasswordEndsSessions(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", "correct horse battery", false)
	token, _ := s.Login(ctx, "admin", "correct horse battery", "", "1.1.1.1")
	if err := s.SetPassword(ctx, "admin", "another long password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("old session survived a password change: %v", err)
	}
}

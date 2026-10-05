package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/webauthn/webauthntest"
)

// Guesses from ever new addresses (review #148) are held to a limit for everyone together; a
// passkey still logs in meanwhile, and so does a browser that logged in before; one logged in
// still confirms its password, to change it or add a passkey (review #182).
func TestThrottleAcrossAddresses(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	dev := webauthntest.New(webauthntest.ES256)
	withPasskey(t, s, dev)
	for i := range maxAllFailures {
		if _, err := s.Login(ctx, "admin", "wrong password!", "", fmt.Sprintf("192.0.2.%d", i)); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	if _, err := s.Login(ctx, "admin", password, "", "198.51.100.1"); !errors.Is(err, ErrThrottledAll) {
		t.Fatalf("a new address after %d wrong passwords: %v", maxAllFailures, err)
	}
	if _, err := s.Login(KnownDevice(ctx), "admin", password, "", "198.51.100.1"); err != nil {
		t.Fatalf("a browser that logged in before: %v", err)
	}
	if _, err := s.RegisterChallenge(ctx, 1, password, "198.51.100.2"); err != nil {
		t.Fatalf("adding a passkey: %v", err)
	}
	if err := s.ChangePassword(ctx, 1, "wrong password!", "another password", "198.51.100.2"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("changing the password: %v", err)
	}
	cd, ad, sig := answer(t, s, dev)
	if _, err := s.PasskeyLogin(ctx, rp, dev.ID, cd, ad, sig, nil, "", "198.51.100.3"); err != nil {
		t.Fatalf("passkey login: %v", err)
	}
	// They pass with the window.
	s.mu.Lock()
	for i := range s.wrong {
		s.wrong[i] = s.wrong[i].Add(-failureWindow)
	}
	s.mu.Unlock()
	if _, err := s.Login(ctx, "admin", password, "", "198.51.100.1"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// An account's own password confirmations are held to its own limit (review #182).
func TestConfirmationsHaveTheirOwnLimit(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", password, false)
	for i := range maxFailures {
		if err := s.ChangePassword(ctx, 1, "wrong password!", "another password", fmt.Sprintf("192.0.2.%d", i)); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if _, err := s.RegisterChallenge(ctx, 1, password, "203.0.113.1"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("after %d wrong confirmations: %v", maxFailures, err)
	}
	if _, err := s.Login(ctx, "admin", password, "", "203.0.113.1"); err != nil {
		t.Fatalf("logging in is another limit: %v", err)
	}
}

// The addresses of one IPv6 /64 are one machine's: they share its limit (review #182).
func TestIPv6NetworksShareALimit(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", password, false)
	for i := range maxFailures {
		if _, err := s.Login(ctx, "admin", "wrong password!", "", fmt.Sprintf("2001:db8::%x", i+1)); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if _, err := s.Login(ctx, "admin", password, "", "2001:db8::ffff"); !errors.Is(err, ErrThrottled) || errors.Is(err, ErrThrottledAll) {
		t.Fatalf("another address of the /64: %v", err)
	}
	if _, err := s.Login(ctx, "admin", password, "", "2001:db8:0:1::1"); err != nil {
		t.Fatalf("another /64: %v", err)
	}
	if network("192.0.2.7") != "192.0.2.7" || network("::ffff:192.0.2.7") != "::ffff:192.0.2.7" || network("2001:db8::1") != "2001:db8::/64" {
		t.Fatal(network("2001:db8::1"))
	}
}

// Addresses whose failures passed are forgotten, though they never come back.
func TestFailuresAreSwept(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", password, false)
	for i := range 3 {
		s.Login(ctx, "admin", "wrong password!", "", fmt.Sprintf("192.0.2.%d", i))
	}
	s.mu.Lock()
	for ip, ts := range s.failures {
		s.failures[ip] = []time.Time{ts[0].Add(-failureWindow)}
	}
	s.swept = time.Time{}
	s.mu.Unlock()
	s.Login(ctx, "admin", password, "", "203.0.113.1")
	if len(s.failures) != 0 {
		t.Fatalf("kept %v", s.failures)
	}
}

// bcrypt runs maxChecks at a time; one waiting for its turn gives up with its request, and that is
// no failure.
func TestPasswordChecksTakeTurns(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	s.CreateUser(ctx, "admin", password, false)
	for range maxChecks {
		s.turns <- struct{}{}
	}
	wait, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := s.Login(wait, "admin", "wrong password!", "", "192.0.2.1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no turn: %v", err)
	}
	if len(s.failures) != 0 || len(s.wrong) != 0 || s.passwords != 0 {
		t.Fatalf("failures %v wrong %v checking %d", s.failures, s.wrong, s.passwords)
	}
	<-s.turns
	if _, err := s.Login(ctx, "admin", password, "", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
}

// Login challenges take no room (review #148): any number can be asked for, and each is good once,
// until it expires, and only as made here.
func TestLoginChallenges(t *testing.T) {
	ctx := context.Background()
	s := newService(t)
	dev := webauthntest.New(webauthntest.ES256)
	withPasskey(t, s, dev)
	for range 10 * maxChallenges {
		s.LoginChallenge()
	}
	cd, ad, sig := answer(t, s, dev)
	if _, err := s.PasskeyLogin(ctx, rp, dev.ID, cd, ad, sig, nil, "", "1.1.1.1"); err != nil {
		t.Fatalf("after a flood of requests: %v", err)
	}
	if _, err := s.PasskeyLogin(ctx, rp, dev.ID, cd, ad, sig, nil, "", "1.1.1.1"); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("the same answer again: %v", err)
	}

	forged := s.LoginChallenge()
	forged[0] ^= 1
	cd, ad, sig = dev.Get(rp.ID, rp.Origin, forged)
	if _, err := s.PasskeyLogin(ctx, rp, dev.ID, cd, ad, sig, nil, "", "1.1.1.1"); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("a challenge not made here: %v", err)
	}
	other := New(s.db)
	cd, ad, sig = dev.Get(rp.ID, rp.Origin, other.LoginChallenge())
	if _, err := s.PasskeyLogin(ctx, rp, dev.ID, cd, ad, sig, nil, "", "1.1.1.1"); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("another key's challenge: %v", err)
	}

	raw := make([]byte, 16, 56)
	raw = append(raw, 0, 0, 0, 0, 0, 0, 0, 1) // expired in 1970
	cd, ad, sig = dev.Get(rp.ID, rp.Origin, s.loginMAC(raw))
	if _, err := s.PasskeyLogin(ctx, rp, dev.ID, cd, ad, sig, nil, "", "1.1.1.1"); !errors.Is(err, ErrPasskeyRequest) {
		t.Fatalf("an expired challenge: %v", err)
	}
	// Used ones are forgotten once they expire.
	s.mu.Lock()
	for k := range s.used {
		s.used[k] = time.Now().Add(-time.Second)
	}
	s.mu.Unlock()
	cd, ad, sig = answer(t, s, dev)
	if _, err := s.PasskeyLogin(ctx, rp, dev.ID, cd, ad, sig, nil, "", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if len(s.used) != 1 {
		t.Fatalf("%d used challenges kept", len(s.used))
	}
}

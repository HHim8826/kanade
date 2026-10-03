// Package auth manages the administrator account and login tokens.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/webauthn"
)

var (
	ErrBadCredentials = errors.New("wrong username or password")
	ErrThrottled      = errors.New("too many failed logins; try again later")
	ErrNoSession      = errors.New("not logged in")
	ErrUsersExist     = errors.New("an account already exists")
)

const (
	minPasswordLen = 10
	maxFailures    = 5
	failureWindow  = 15 * time.Minute
	// SessionLifetime is how long a login lasts from when it was made, however often it is used;
	// the web client's cookie lasts as long.
	SessionLifetime = 90 * 24 * time.Hour
)

var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("timing-equalizer"), bcrypt.DefaultCost)
	return h
})

type Service struct {
	db *sql.DB

	mu         sync.Mutex
	failures   map[string][]time.Time // client IP -> recent failed logins
	checking   map[string]int         // client IP -> passwords and passkeys being checked now
	challenges map[string]challenge   // pending passkey requests
}

func New(d *sql.DB) *Service {
	return &Service{db: d, failures: map[string][]time.Time{}, checking: map[string]int{}, challenges: map[string]challenge{}}
}

func (s *Service) HasUsers(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n > 0, err
}

// CreateUser adds an account. With onlyIfNone it refuses once any account exists,
// which is how first-run setup works.
func (s *Service) CreateUser(ctx context.Context, username, password string, onlyIfNone bool) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("username is empty")
	}
	if len(password) < minPasswordLen {
		return errors.New("password must be at least 10 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if onlyIfNone {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrUsersExist
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)`,
		username, string(hash), db.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) SetPassword(ctx context.Context, username, password string) error {
	if len(password) < minPasswordLen {
		return errors.New("password must be at least 10 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	// One transaction: a login checked against the old password either made its session before
	// this (and loses it here) or finds the password changed when it goes to make one.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE username = ?`, string(hash), username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such user")
	}
	// A new password ends every existing login.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = (SELECT id FROM users WHERE username = ?)`, username); err != nil {
		return err
	}
	return tx.Commit()
}

// attempt lets a client check a password or passkey, unless its recent failures and the checks it
// has under way reach the limit: checks still running count, so a burst of guesses sent at once is
// held to the limit too. done ends the check; a refused one is remembered as a failure.
func (s *Service) attempt(ip string) (done func(err error), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recent := s.failures[ip][:0]
	for _, t := range s.failures[ip] {
		if time.Since(t) < failureWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(s.failures, ip)
	} else {
		s.failures[ip] = recent
	}
	if len(recent)+s.checking[ip] >= maxFailures {
		return nil, ErrThrottled
	}
	s.checking[ip]++
	return func(err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.checking[ip]--; s.checking[ip] <= 0 {
			delete(s.checking, ip)
		}
		if refused(err) {
			s.failures[ip] = append(s.failures[ip], time.Now())
		}
	}, nil
}

// refused tells a wrong password or passkey (a failure for the throttle) from a failure here.
func refused(err error) bool {
	return errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrNoPasskey) || errors.Is(err, ErrPasskeyRequest) ||
		errors.Is(err, webauthn.ErrInvalid) || errors.Is(err, webauthn.ErrCloned)
}

// Login checks the password and returns a new login token. clientIP is used for throttling.
func (s *Service) Login(ctx context.Context, username, password, deviceName, clientIP string) (token string, err error) {
	done, err := s.attempt(clientIP)
	if err != nil {
		return "", err
	}
	defer func() { done(err) }()
	id, hash, err := s.checkLogin(ctx, username, password)
	if err != nil {
		return "", err
	}
	return s.passwordSession(ctx, id, hash, deviceName)
}

// checkLogin checks a password, and returns the account and the password hash it matched.
func (s *Service) checkLogin(ctx context.Context, username, password string) (int64, string, error) {
	var id int64
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT id, password_hash FROM users WHERE username = ?`, username).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Spend the same time as a real comparison so usernames cannot be probed by timing.
		bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
		return 0, "", ErrBadCredentials
	}
	if err != nil {
		return 0, "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return 0, "", ErrBadCredentials
	}
	return id, hash, nil
}

// passwordSession starts a login checked against the password hash, if it is still the account's
// password: a new password set during the check refuses it.
func (s *Service) passwordSession(ctx context.Context, id int64, hash, deviceName string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current != hash) {
		return "", ErrBadCredentials
	}
	if err != nil {
		return "", err
	}
	token, err := newSession(ctx, tx, id, deviceName)
	if err != nil {
		return "", err
	}
	return token, tx.Commit()
}

// newSession starts a login for the user and returns its token; only the token's hash is kept.
func newSession(ctx context.Context, tx *sql.Tx, id int64, deviceName string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	now := db.Now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (user_id, token_hash, name, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?)`, id, sum[:], deviceName, now, now); err != nil {
		return "", err
	}
	return token, nil
}

// Authenticate resolves a login token to a user ID. A login ends SessionLifetime after it was made.
func (s *Service) Authenticate(ctx context.Context, token string) (int64, error) {
	if token == "" {
		return 0, ErrNoSession
	}
	sum := sha256.Sum256([]byte(token))
	var userID, sessionID, created, lastUsed int64
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, created_at, last_used_at FROM sessions WHERE token_hash = ?`, sum[:]).
		Scan(&sessionID, &userID, &created, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoSession
	}
	if err != nil {
		return 0, err
	}
	now := db.Now()
	if now-created >= SessionLifetime.Milliseconds() {
		s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
		return 0, ErrNoSession
	}
	if now-lastUsed > int64(time.Hour/time.Millisecond) { // avoid a write on every request
		s.db.ExecContext(ctx, `UPDATE sessions SET last_used_at = ? WHERE id = ?`, now, sessionID)
	}
	return userID, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	sum := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, sum[:])
	return err
}

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
	// SessionLifetime is the absolute lifetime shared by server tokens and web cookies.
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
	inFlight   map[string]int         // reserved checks count towards the same limit
	challenges map[string]challenge   // pending passkey requests
}

func New(d *sql.DB) *Service {
	return &Service{db: d, failures: map[string][]time.Time{}, inFlight: map[string]int{}, challenges: map[string]challenge{}}
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = (SELECT id FROM users WHERE username = ?)`, username); err != nil {
		return err
	}
	return tx.Commit()
}

// Reserve a check before doing expensive verification. Concurrent password/passkey
// requests cannot all pass the limit while their earlier failures are still pending.
// finish must be called once; only failed credentials remain in the rolling window.
func (s *Service) beginAttempt(ip string) (finish func(bool), err error) {
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
	if len(recent)+s.inFlight[ip] >= maxFailures {
		return nil, ErrThrottled
	}
	s.inFlight[ip]++
	return func(failed bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.inFlight[ip]--
		if s.inFlight[ip] == 0 {
			delete(s.inFlight, ip)
		}
		if failed {
			s.failures[ip] = append(s.failures[ip], time.Now())
		}
	}, nil
}

// Login checks the password and returns a new login token. clientIP is used for throttling.
func (s *Service) Login(ctx context.Context, username, password, deviceName, clientIP string) (string, error) {
	finish, err := s.beginAttempt(clientIP)
	if err != nil {
		return "", err
	}
	failed := false
	defer func() { finish(failed) }()
	var id int64
	var hash string
	err = s.db.QueryRowContext(ctx, `SELECT id, password_hash FROM users WHERE username = ?`, username).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Spend the same time as a real comparison so usernames cannot be probed by timing.
		bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
		failed = true
		return "", ErrBadCredentials
	}
	if err != nil {
		return "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		failed = true
		return "", ErrBadCredentials
	}
	// Recheck the password version under the same write lock as session creation.
	// A reset either rejects this old proof or revokes the session after it commits.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current != hash) {
		failed = true
		return "", ErrBadCredentials
	}
	if err != nil {
		return "", err
	}
	token, err := newSession(ctx, tx, id, deviceName)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
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

// Authenticate resolves a login token to a user ID.
func (s *Service) Authenticate(ctx context.Context, token string) (int64, error) {
	if token == "" {
		return 0, ErrNoSession
	}
	sum := sha256.Sum256([]byte(token))
	var userID, sessionID, lastUsed int64
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, last_used_at FROM sessions WHERE token_hash = ? AND created_at > ?`, sum[:], db.Now()-SessionLifetime.Milliseconds()).
		Scan(&sessionID, &userID, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoSession
	}
	if err != nil {
		return 0, err
	}
	if now := db.Now(); now-lastUsed > int64(time.Hour/time.Millisecond) { // avoid a write on every request
		s.db.ExecContext(ctx, `UPDATE sessions SET last_used_at = ? WHERE id = ?`, now, sessionID)
	}
	return userID, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	sum := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, sum[:])
	return err
}

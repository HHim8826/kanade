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
	ErrShortPassword  = errors.New("password must be at least 10 characters")
)

const (
	minPasswordLen = 10
	maxFailures    = 5
	failureWindow  = 15 * time.Minute
	// From all addresses together (they can change at will, or all be a proxy's), passwords are
	// checked no more than this (review #148): failures within the window, and checks at a time
	// (bcrypt takes the CPU; more wait their turn).
	maxAllFailures = 30
	maxChecks      = 4
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
	swept      time.Time              // when failures was last cleared of those past the window
	wrong      []time.Time            // recent wrong passwords, from anyone
	passwords  int                    // passwords being checked now
	turns      chan struct{}          // a turn to run bcrypt
	challenges map[string]challenge   // pending passkey requests to add one
	key        []byte                 // signs the challenges to log in with a passkey
	used       map[string]time.Time   // those used, until they expire
}

func New(d *sql.DB) *Service {
	key := make([]byte, 32)
	rand.Read(key)
	return &Service{db: d, failures: map[string][]time.Time{}, checking: map[string]int{}, turns: make(chan struct{}, maxChecks),
		challenges: map[string]challenge{}, key: key, used: map[string]time.Time{}}
}

// compare checks a password against a hash, when there is a turn for it.
func (s *Service) compare(ctx context.Context, hash []byte, password string) error {
	select {
	case s.turns <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.turns }()
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		return ErrBadCredentials
	}
	return nil
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
		return ErrShortPassword
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
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("no such user")
	}
	if err != nil {
		return err
	}
	return s.setPassword(ctx, id, "", password)
}

// setPassword sets an account's password; with was, only if its password hash is still that one
// (no other change came in between).
func (s *Service) setPassword(ctx context.Context, id int64, was, password string) error {
	if len(password) < minPasswordLen {
		return ErrShortPassword
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
	res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ? AND (? = '' OR password_hash = ?)`,
		string(hash), id, was, was)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if was != "" {
			return ErrBadCredentials // changed in between
		}
		return errors.New("no such user")
	}
	// A new password ends every existing login.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrSamePassword is a new password that is the current one.
var ErrSamePassword = errors.New("the new password is the current one")

// ChangePassword is an account changing its own password, which it confirms with the current one
// (throttled like a login, review #76). As with SetPassword, every login of the account ends,
// this one too; passkeys stay.
func (s *Service) ChangePassword(ctx context.Context, userID int64, current, password, clientIP string) (err error) {
	done, err := s.attempt(clientIP, true)
	if err != nil {
		return err
	}
	defer func() { done(err) }()
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash); err != nil {
		return err
	}
	if err := s.compare(ctx, []byte(hash), current); err != nil {
		return err
	}
	if password == current {
		return ErrSamePassword
	}
	return s.setPassword(ctx, userID, hash, password)
}

// Session is a login of an account, as its owner sees it: never its token.
type Session struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"` // what the client said it is when logging in
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"` // kept to the hour
	Current    bool   `json:"current"`      // the login asking
}

var (
	ErrNoSuchSession  = errors.New("no such login")
	ErrCurrentSession = errors.New("this is the login in use: log out instead")
)

// Sessions lists an account's logins, the one with token marked, most recently used first.
func (s *Service) Sessions(ctx context.Context, userID int64, token string) ([]Session, error) {
	sum := sha256.Sum256([]byte(token))
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, last_used_at, token_hash = ? FROM sessions
		WHERE user_id = ? AND created_at > ? ORDER BY last_used_at DESC, id DESC`, sum[:], userID, db.Now()-SessionLifetime.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var v Session
		if err := rows.Scan(&v.ID, &v.Name, &v.CreatedAt, &v.LastUsedAt, &v.Current); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// EndSession ends one of an account's logins other than the one with token (that one logs out).
func (s *Service) EndSession(ctx context.Context, userID, id int64, token string) error {
	sum := sha256.Sum256([]byte(token))
	var current bool
	err := s.db.QueryRowContext(ctx, `SELECT token_hash = ? FROM sessions WHERE id = ? AND user_id = ?`, sum[:], id, userID).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNoSuchSession
	case err != nil:
		return err
	case current:
		return ErrCurrentSession
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// EndOtherSessions ends every login of the account but the one with token, and says how many.
func (s *Service) EndOtherSessions(ctx context.Context, userID int64, token string) (int, error) {
	sum := sha256.Sum256([]byte(token))
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, userID, sum[:])
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// attempt lets a client check a password or passkey, unless its recent failures and the checks it
// has under way reach the limit: checks still running count, so a burst of guesses sent at once is
// held to the limit too. Passwords are also held to a limit for everyone together. done ends the
// check; a refused one is remembered as a failure.
func (s *Service) attempt(ip string, password bool) (done func(err error), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.swept) > time.Minute {
		for k, ts := range s.failures {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= failureWindow {
				delete(s.failures, k)
			}
		}
		s.swept = now
	}
	recent := within(s.failures[ip], now)
	if len(recent) == 0 {
		delete(s.failures, ip)
	} else {
		s.failures[ip] = recent
	}
	s.wrong = within(s.wrong, now)
	if len(recent)+s.checking[ip] >= maxFailures || (password && len(s.wrong)+s.passwords >= maxAllFailures) {
		return nil, ErrThrottled
	}
	s.checking[ip]++
	if password {
		s.passwords++
	}
	return func(err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.checking[ip]--; s.checking[ip] <= 0 {
			delete(s.checking, ip)
		}
		if password {
			s.passwords--
		}
		if refused(err) {
			s.failures[ip] = append(s.failures[ip], time.Now())
			if password {
				s.wrong = append(s.wrong, time.Now())
			}
		}
	}, nil
}

// within keeps the times still in the failure window (in place).
func within(ts []time.Time, now time.Time) []time.Time {
	recent := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < failureWindow {
			recent = append(recent, t)
		}
	}
	return recent
}

// refused tells a wrong password or passkey (a failure for the throttle) from a failure here.
func refused(err error) bool {
	return errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrNoPasskey) || errors.Is(err, ErrPasskeyRequest) ||
		errors.Is(err, webauthn.ErrInvalid) || errors.Is(err, webauthn.ErrCloned)
}

// Login checks the password and returns a new login token. clientIP is used for throttling.
func (s *Service) Login(ctx context.Context, username, password, deviceName, clientIP string) (token string, err error) {
	done, err := s.attempt(clientIP, true)
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
		if err := s.compare(ctx, dummyHash(), password); !errors.Is(err, ErrBadCredentials) && err != nil {
			return 0, "", err
		}
		return 0, "", ErrBadCredentials
	}
	if err != nil {
		return 0, "", err
	}
	if err := s.compare(ctx, []byte(hash), password); err != nil {
		return 0, "", err
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
	user, _, err := s.SessionOf(ctx, token)
	return user, err
}

// SessionOf resolves a login token to its user and the login's ID.
func (s *Service) SessionOf(ctx context.Context, token string) (user, session int64, err error) {
	if token == "" {
		return 0, 0, ErrNoSession
	}
	sum := sha256.Sum256([]byte(token))
	var userID, sessionID, created, lastUsed int64
	err = s.db.QueryRowContext(ctx, `SELECT id, user_id, created_at, last_used_at FROM sessions WHERE token_hash = ?`, sum[:]).
		Scan(&sessionID, &userID, &created, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNoSession
	}
	if err != nil {
		return 0, 0, err
	}
	now := db.Now()
	if now-created >= SessionLifetime.Milliseconds() {
		s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
		return 0, 0, ErrNoSession
	}
	if now-lastUsed > int64(time.Hour/time.Millisecond) { // avoid a write on every request
		s.db.ExecContext(ctx, `UPDATE sessions SET last_used_at = ? WHERE id = ?`, now, sessionID)
	}
	return userID, sessionID, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	sum := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, sum[:])
	return err
}

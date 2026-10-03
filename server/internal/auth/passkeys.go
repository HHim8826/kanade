package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/webauthn"
)

// Passkeys: an account can add passkeys (after typing its password) and then log in with one
// instead of the password. Each request for a passkey gets a one-time challenge, kept here for
// five minutes.

type Passkey struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
}

var (
	ErrNoPasskey       = errors.New("this passkey is not registered here")
	ErrPasskeyRequest  = errors.New("the passkey request expired or was already used; try again")
	ErrPasskeyExists   = errors.New("this passkey is already added")
	ErrTooManyRequests = errors.New("too many passkey requests; try again later")
)

const (
	challengeTTL  = 5 * time.Minute
	maxChallenges = 256
	maxPerClient  = 8 // pending requests from one address, so one client cannot crowd out the rest
	maxPasskeys   = 20
)

type challenge struct {
	purpose string // "register" or "login"
	userID  int64
	client  string
	expires time.Time
}

// NewChallenge makes a one-time challenge for registering a passkey for userID, or for logging in
// (userID 0), asked from clientIP.
func (s *Service) NewChallenge(purpose string, userID int64, clientIP string) ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	mine := 0
	for k, c := range s.challenges {
		if now.After(c.expires) {
			delete(s.challenges, k)
		} else if c.client == clientIP {
			mine++
		}
	}
	if len(s.challenges) >= maxChallenges || mine >= maxPerClient {
		return nil, ErrTooManyRequests
	}
	s.challenges[string(raw)] = challenge{purpose, userID, clientIP, now.Add(challengeTTL)}
	return raw, nil
}

// takeChallenge uses up a challenge; it reports the user it was made for.
func (s *Service) takeChallenge(raw []byte, purpose string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.challenges[string(raw)]
	delete(s.challenges, string(raw))
	if !ok || c.purpose != purpose || time.Now().After(c.expires) {
		return 0, false
	}
	return c.userID, true
}

// UserHandle is the passkey user ID of an account (no name in it, as WebAuthn asks).
func UserHandle(userID int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(userID)) }

// User is an account's name, for the passkey a device shows.
func (s *Service) User(ctx context.Context, userID int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, userID).Scan(&name)
	return name, err
}

// CheckPassword confirms the account's password before something sensitive (adding a passkey);
// failures count towards the login throttle.
func (s *Service) CheckPassword(ctx context.Context, userID int64, password, clientIP string) error {
	if s.throttled(clientIP) {
		return ErrThrottled
	}
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.recordFailure(clientIP)
		return ErrBadCredentials
	}
	return nil
}

// Passkeys lists an account's passkeys, newest first.
func (s *Service) Passkeys(ctx context.Context, userID int64) ([]Passkey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, last_used_at FROM passkeys WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Passkey{}
	for rows.Next() {
		var p Passkey
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CredentialIDs are an account's passkey IDs, so a device does not add the same one twice.
func (s *Service) CredentialIDs(ctx context.Context, userID int64) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT credential_id FROM passkeys WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var id []byte
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// HasPasskeys reports whether any account has a passkey: only then does the login page offer it.
func (s *Service) HasPasskeys(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM passkeys`).Scan(&n)
	return n > 0, err
}

// AddPasskey checks a new passkey made for the challenge given to userID, and stores it.
func (s *Service) AddPasskey(ctx context.Context, rp webauthn.RP, userID int64, name string, clientDataJSON, attestationObject []byte) (*Passkey, error) {
	ch, err := webauthn.Challenge(clientDataJSON)
	if err != nil {
		return nil, err
	}
	if owner, ok := s.takeChallenge(ch, "register"); !ok || owner != userID {
		return nil, ErrPasskeyRequest
	}
	cred, err := rp.Register(ch, clientDataJSON, attestationObject)
	if err != nil {
		return nil, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM passkeys WHERE user_id = ?`, userID).Scan(&n); err != nil {
		return nil, err
	}
	if n >= maxPasskeys {
		return nil, errors.New("an account can have at most 20 passkeys")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	name = string([]rune(name)[:min(len([]rune(name)), 60)])
	now := db.Now()
	r, err := s.db.ExecContext(ctx, `INSERT INTO passkeys (user_id, credential_id, public_key, sign_count, name, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (credential_id) DO NOTHING`, userID, cred.ID, cred.PublicKey, cred.SignCount, name, now)
	if err != nil {
		return nil, err
	}
	if k, _ := r.RowsAffected(); k == 0 {
		return nil, ErrPasskeyExists
	}
	id, _ := r.LastInsertId()
	return &Passkey{ID: id, Name: name, CreatedAt: now}, nil
}

// RenamePasskey and DeletePasskey change an account's own passkeys only.
func (s *Service) RenamePasskey(ctx context.Context, userID, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("the name is empty")
	}
	name = string([]rune(name)[:min(len([]rune(name)), 60)])
	r, err := s.db.ExecContext(ctx, `UPDATE passkeys SET name = ? WHERE id = ? AND user_id = ?`, name, id, userID)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNoPasskey
	}
	return nil
}

func (s *Service) DeletePasskey(ctx context.Context, userID, id int64) error {
	r, err := s.db.ExecContext(ctx, `DELETE FROM passkeys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNoPasskey
	}
	return nil
}

// PasskeyLogin checks an assertion for a login challenge and returns a new login token, like
// Login with the password. Failures count towards the same throttle.
func (s *Service) PasskeyLogin(ctx context.Context, rp webauthn.RP, credentialID, clientDataJSON, authenticatorData, signature,
	userHandle []byte, deviceName, clientIP string) (string, error) {
	if s.throttled(clientIP) {
		return "", ErrThrottled
	}
	fail := func(err error) (string, error) {
		s.recordFailure(clientIP)
		return "", err
	}
	ch, err := webauthn.Challenge(clientDataJSON)
	if err != nil {
		return fail(err)
	}
	if _, ok := s.takeChallenge(ch, "login"); !ok {
		return fail(ErrPasskeyRequest)
	}
	var id, userID int64
	var cred webauthn.Credential
	err = s.db.QueryRowContext(ctx, `SELECT id, user_id, credential_id, public_key, sign_count FROM passkeys WHERE credential_id = ?`,
		credentialID).Scan(&id, &userID, &cred.ID, &cred.PublicKey, &cred.SignCount)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrNoPasskey)
	}
	if err != nil {
		return "", err
	}
	if len(userHandle) > 0 && string(userHandle) != string(UserHandle(userID)) {
		return fail(webauthn.ErrInvalid)
	}
	count, err := rp.Login(cred, ch, clientDataJSON, authenticatorData, signature)
	if err != nil {
		return fail(err)
	}
	// The counter only moves forward, even when two logins with one passkey race.
	if _, err := s.db.ExecContext(ctx, `UPDATE passkeys SET sign_count = max(sign_count, ?), last_used_at = ? WHERE id = ?`,
		count, db.Now(), id); err != nil {
		return "", err
	}
	return s.newSession(ctx, userID, deviceName)
}

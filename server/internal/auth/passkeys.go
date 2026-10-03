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
	purpose      string // "register" or "login"
	userID       int64
	client       string
	expires      time.Time
	passwordHash string // the password version that authorized registration
}

func (s *Service) NewLoginChallenge(clientIP string) ([]byte, error) {
	return s.newChallenge("login", 0, clientIP, "")
}

// Registration retains the exact password version that was verified, so resetting
// the password invalidates the authorization even if enrollment is already in flight.
func (s *Service) NewRegistrationChallenge(ctx context.Context, userID int64, password, clientIP string) ([]byte, error) {
	hash, err := s.checkPassword(ctx, userID, password, clientIP)
	if err != nil {
		return nil, err
	}
	return s.newChallenge("register", userID, clientIP, hash)
}

func (s *Service) newChallenge(purpose string, userID int64, clientIP, passwordHash string) ([]byte, error) {
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
	s.challenges[string(raw)] = challenge{purpose: purpose, userID: userID, client: clientIP, expires: now.Add(challengeTTL), passwordHash: passwordHash}
	return raw, nil
}

// takeChallenge uses up a challenge and returns its authorization, including its owner.
func (s *Service) takeChallenge(raw []byte, purpose string) (challenge, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.challenges[string(raw)]
	delete(s.challenges, string(raw))
	if !ok || c.purpose != purpose || time.Now().After(c.expires) {
		return challenge{}, false
	}
	return c, true
}

// UserHandle is the passkey user ID of an account (no name in it, as WebAuthn asks).
func UserHandle(userID int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(userID)) }

// User is an account's name, for the passkey a device shows.
func (s *Service) User(ctx context.Context, userID int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, userID).Scan(&name)
	return name, err
}

// checkPassword returns the verified password version; failures share the login throttle.
func (s *Service) checkPassword(ctx context.Context, userID int64, password, clientIP string) (string, error) {
	finish, err := s.beginAttempt(clientIP)
	if err != nil {
		return "", err
	}
	failed := false
	defer func() { finish(failed) }()
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash); err != nil {
		return "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		failed = true
		return "", ErrBadCredentials
	}
	return hash, nil
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
	proof, ok := s.takeChallenge(ch, "register")
	if !ok || proof.userID != userID {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (proof.passwordHash == "" || current != proof.passwordHash)) {
		return nil, ErrPasskeyRequest
	}
	if err != nil {
		return nil, err
	}
	// Enforce the account limit again under the write lock, alongside enrollment.
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM passkeys WHERE user_id = ?`, userID).Scan(&n); err != nil {
		return nil, err
	}
	if n >= maxPasskeys {
		return nil, errors.New("an account can have at most 20 passkeys")
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO passkeys (user_id, credential_id, public_key, sign_count, name, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (credential_id) DO NOTHING`, userID, cred.ID, cred.PublicKey, cred.SignCount, name, now)
	if err != nil {
		return nil, err
	}
	if k, _ := r.RowsAffected(); k == 0 {
		return nil, ErrPasskeyExists
	}
	id, _ := r.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
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
	finish, err := s.beginAttempt(clientIP)
	if err != nil {
		return "", err
	}
	failed := false
	defer func() { finish(failed) }()
	fail := func(err error) (string, error) {
		failed = true
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
	// The credential must still exist and its counter must still advance when the
	// session is created, even if another login or deletion happened during verification.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var current uint32
	err = tx.QueryRowContext(ctx, `SELECT sign_count FROM passkeys WHERE id = ? AND user_id = ? AND credential_id = ? AND public_key = ?`,
		id, userID, cred.ID, cred.PublicKey).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(ErrNoPasskey)
	}
	if err != nil {
		return "", err
	}
	if (count != 0 || current != 0) && count <= current {
		return fail(webauthn.ErrCloned)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE passkeys SET sign_count = ?, last_used_at = ? WHERE id = ?`,
		count, db.Now(), id); err != nil {
		return "", err
	}
	token, err := newSession(ctx, tx, userID, deviceName)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

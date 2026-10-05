package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/webauthn"
)

// Passkeys: an account can add passkeys (after typing its password) and then log in with one
// instead of the password. Each request for a passkey gets a one-time challenge, good for five
// minutes. Those to add a passkey are kept here; those to log in are not (anyone can ask for
// them, from any number of addresses, review #148): they carry when they expire and a MAC, and
// only those used are remembered.

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
	maxChallenges = 256 // pending requests to add a passkey
	maxPerClient  = 8   // of them from one address, so one client cannot crowd out the rest
	maxPasskeys   = 20
)

type challenge struct {
	purpose string // "register" or "login"
	userID  int64
	client  string
	expires time.Time
	// password is the password hash that was confirmed to register a passkey: a new password set
	// meanwhile voids the request.
	password string
}

// LoginChallenge makes a one-time challenge for logging in with a passkey: 16 random bytes, when it
// expires, and their MAC.
func (s *Service) LoginChallenge() []byte {
	raw := make([]byte, 16, 16+8+sha256.Size)
	rand.Read(raw)
	raw = binary.BigEndian.AppendUint64(raw, uint64(time.Now().Add(challengeTTL).Unix()))
	return s.loginMAC(raw)
}

func (s *Service) loginMAC(raw []byte) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("login"))
	mac.Write(raw)
	return mac.Sum(raw)
}

// loginChallengeOK reports whether ch is a login challenge made here that has not expired.
func (s *Service) loginChallengeOK(ch []byte) bool {
	if len(ch) != 16+8+sha256.Size || !hmac.Equal(s.loginMAC(ch[:24:24]), ch) {
		return false
	}
	return time.Now().Unix() < int64(binary.BigEndian.Uint64(ch[16:24]))
}

// useLoginChallenge uses up a login challenge, once a passkey answered it: false if it was used.
func (s *Service) useLoginChallenge(ch []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, exp := range s.used {
		if now.After(exp) {
			delete(s.used, k)
		}
	}
	if _, ok := s.used[string(ch)]; ok {
		return false
	}
	s.used[string(ch)] = time.Unix(int64(binary.BigEndian.Uint64(ch[16:24])), 0)
	return true
}

// RegisterChallenge confirms the account's password, then makes a one-time challenge for adding a
// passkey to it. Wrong passwords count towards the login throttle.
func (s *Service) RegisterChallenge(ctx context.Context, userID int64, password, clientIP string) (ch []byte, err error) {
	done, err := s.attempt(clientIP, true)
	if err != nil {
		return nil, err
	}
	defer func() { done(err) }()
	var hash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash); err != nil {
		return nil, err
	}
	if err := s.compare(ctx, []byte(hash), password); err != nil {
		return nil, err
	}
	return s.newChallenge(challenge{purpose: "register", userID: userID, client: clientIP, password: hash})
}

func (s *Service) newChallenge(c challenge) ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	mine := 0
	for k, old := range s.challenges {
		if now.After(old.expires) {
			delete(s.challenges, k)
		} else if old.client == c.client {
			mine++
		}
	}
	if len(s.challenges) >= maxChallenges || mine >= maxPerClient {
		return nil, ErrTooManyRequests
	}
	c.expires = now.Add(challengeTTL)
	s.challenges[string(raw)] = c
	return raw, nil
}

// takeChallenge uses up a challenge; it reports what it was made for.
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
	c, ok := s.takeChallenge(ch, "register")
	if !ok || c.userID != userID {
		return nil, ErrPasskeyRequest
	}
	cred, err := rp.Register(ch, clientDataJSON, attestationObject)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	name = string([]rune(name)[:min(len([]rune(name)), 60)])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var hash string
	var n int
	err = tx.QueryRowContext(ctx, `SELECT password_hash, (SELECT count(*) FROM passkeys WHERE user_id = users.id) FROM users WHERE id = ?`,
		userID).Scan(&hash, &n)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && hash != c.password) {
		return nil, ErrPasskeyRequest // the password changed since it was confirmed
	}
	if err != nil {
		return nil, err
	}
	if n >= maxPasskeys {
		return nil, errors.New("an account can have at most 20 passkeys")
	}
	now := db.Now()
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
	userHandle []byte, deviceName, clientIP string) (token string, err error) {
	done, err := s.attempt(clientIP, false)
	if err != nil {
		return "", err
	}
	defer func() { done(err) }()
	a, err := s.checkAssertion(ctx, rp, credentialID, clientDataJSON, authenticatorData, signature, userHandle)
	if err != nil {
		return "", err
	}
	return s.passkeySession(ctx, a, deviceName)
}

// assertion is a passkey's answer that was checked, with the passkey as it was then.
type assertion struct {
	id, userID int64
	cred       webauthn.Credential
	count      uint32
}

func (s *Service) checkAssertion(ctx context.Context, rp webauthn.RP, credentialID, clientDataJSON, authenticatorData, signature,
	userHandle []byte) (*assertion, error) {
	ch, err := webauthn.Challenge(clientDataJSON)
	if err != nil {
		return nil, err
	}
	if !s.loginChallengeOK(ch) {
		return nil, ErrPasskeyRequest
	}
	var a assertion
	err = s.db.QueryRowContext(ctx, `SELECT id, user_id, credential_id, public_key, sign_count FROM passkeys WHERE credential_id = ?`,
		credentialID).Scan(&a.id, &a.userID, &a.cred.ID, &a.cred.PublicKey, &a.cred.SignCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoPasskey
	}
	if err != nil {
		return nil, err
	}
	if len(userHandle) > 0 && string(userHandle) != string(UserHandle(a.userID)) {
		return nil, webauthn.ErrInvalid
	}
	if a.count, err = rp.Login(a.cred, ch, clientDataJSON, authenticatorData, signature); err != nil {
		return nil, err
	}
	if !s.useLoginChallenge(ch) {
		return nil, ErrPasskeyRequest
	}
	return &a, nil
}

// passkeySession starts a login for a checked assertion, if the passkey is still there as it was
// checked, and its counter (when it keeps one) still goes up: a passkey removed meanwhile, or a
// second login with the same counter, gets no session.
func (s *Service) passkeySession(ctx context.Context, a *assertion, deviceName string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var count uint32
	err = tx.QueryRowContext(ctx, `SELECT sign_count FROM passkeys WHERE id = ? AND user_id = ? AND credential_id = ? AND public_key = ?`,
		a.id, a.userID, a.cred.ID, a.cred.PublicKey).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoPasskey
	}
	if err != nil {
		return "", err
	}
	if (a.count != 0 || count != 0) && a.count <= count {
		return "", webauthn.ErrCloned
	}
	if _, err := tx.ExecContext(ctx, `UPDATE passkeys SET sign_count = ?, last_used_at = ? WHERE id = ?`, a.count, db.Now(), a.id); err != nil {
		return "", err
	}
	token, err := newSession(ctx, tx, a.userID, deviceName)
	if err != nil {
		return "", err
	}
	return token, tx.Commit()
}

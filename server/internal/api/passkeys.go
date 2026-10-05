package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/HHim8826/kanade/server/internal/auth"
	"github.com/HHim8826/kanade/server/internal/webauthn"
)

// Passkeys (WebAuthn): added in the settings after typing the password, then offered on the login
// page. The site is the relying party named by its public URL.

// b64 is binary data in JSON as base64url without padding, as WebAuthn's JSON forms have it.
type b64 []byte

func (b b64) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.RawURLEncoding.EncodeToString(b))
}

func (b *b64) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	*b = raw
	return err
}

// rp is this site as a relying party, from its public URL.
func (s *Server) rp() (webauthn.RP, bool) {
	u, err := url.Parse(s.cfg.PublicURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Hostname() != "localhost") {
		return webauthn.RP{}, false
	}
	return webauthn.RP{ID: u.Hostname(), Origin: u.Scheme + "://" + u.Host}, true
}

func (s *Server) passkeyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrThrottled), errors.Is(err, auth.ErrTooManyRequests):
		writeError(w, http.StatusTooManyRequests, err)
	case errors.Is(err, auth.ErrBadCredentials): // not 401: the login itself is fine
		writeError(w, http.StatusForbidden, errors.New("wrong password"))
	case errors.Is(err, auth.ErrNoPasskey):
		writeError(w, http.StatusUnauthorized, err)
	case errors.Is(err, auth.ErrPasskeyExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, auth.ErrPasskeyRequest), errors.Is(err, webauthn.ErrInvalid), errors.Is(err, webauthn.ErrCloned):
		s.log.Warn("passkey refused", "path", r.URL.Path, "ip", s.clientIP(r), "err", err)
		writeError(w, http.StatusUnauthorized, err)
	default:
		s.internal(w, r, err)
	}
}

type credParam struct {
	Type string `json:"type"`
	Alg  int64  `json:"alg"`
}

type credRef struct {
	Type string `json:"type"`
	ID   b64    `json:"id"`
}

// passkeysAvailable tells the login page whether to offer a passkey (some account added one).
func (s *Server) passkeysAvailable(w http.ResponseWriter, r *http.Request) {
	_, ok := s.rp()
	has, err := s.auth.HasPasskeys(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"available": ok && has})
}

// passkeyLoginOptions starts a passkey login: the device offers the passkeys it has for this site.
func (s *Server) passkeyLoginOptions(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.rp()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("passkeys need the site's https address"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"challenge": b64(s.auth.LoginChallenge()), "rpId": rp.ID, "timeout": 300000,
		"userVerification": "required", "allowCredentials": []credRef{}})
}

// passkeyLogin logs in with a passkey's answer, like login with the password.
func (s *Server) passkeyLogin(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.rp()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("passkeys need the site's https address"))
		return
	}
	var req struct {
		ID                b64    `json:"id"`
		ClientDataJSON    b64    `json:"clientDataJSON"`
		AuthenticatorData b64    `json:"authenticatorData"`
		Signature         b64    `json:"signature"`
		UserHandle        b64    `json:"userHandle"`
		Device            string `json:"device"`
		Cookie            bool   `json:"cookie"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !cookieLogin(w, r, req.Cookie) {
		return
	}
	token, err := s.auth.PasskeyLogin(r.Context(), rp, req.ID, req.ClientDataJSON, req.AuthenticatorData, req.Signature,
		req.UserHandle, req.Device, s.clientIP(r))
	if err != nil {
		s.passkeyError(w, r, err)
		return
	}
	s.loggedIn(w, token, req.Cookie)
}

func userID(r *http.Request) int64 {
	id, _ := r.Context().Value(userKey).(int64)
	return id
}

// sessionID is the ID of the login a request was made with.
func sessionID(r *http.Request) int64 {
	id, _ := r.Context().Value(sessionKey).(int64)
	return id
}

func (s *Server) listPasskeys(w http.ResponseWriter, r *http.Request) {
	list, err := s.auth.Passkeys(r.Context(), userID(r))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// passkeyOptions starts adding a passkey, after the account's password is confirmed: a stolen login
// alone cannot add a passkey of its own.
func (s *Server) passkeyOptions(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.rp()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("passkeys need the site's https address"))
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	uid := userID(r)
	ch, err := s.auth.RegisterChallenge(r.Context(), uid, req.Password, s.clientIP(r))
	if err != nil {
		s.passkeyError(w, r, err)
		return
	}
	name, err := s.auth.User(r.Context(), uid)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	have, err := s.auth.CredentialIDs(r.Context(), uid)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	exclude := []credRef{}
	for _, id := range have {
		exclude = append(exclude, credRef{"public-key", id})
	}
	params := []credParam{}
	for _, alg := range webauthn.Algorithms {
		params = append(params, credParam{"public-key", alg})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"challenge": b64(ch), "rp": map[string]string{"id": rp.ID, "name": "Kanade"},
		"user":             map[string]any{"id": b64(auth.UserHandle(uid)), "name": name, "displayName": name},
		"pubKeyCredParams": params, "timeout": 300000, "excludeCredentials": exclude, "attestation": "none",
		"authenticatorSelection": map[string]any{"residentKey": "required", "requireResidentKey": true, "userVerification": "required"},
	})
}

func (s *Server) addPasskey(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.rp()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("passkeys need the site's https address"))
		return
	}
	var req struct {
		Name              string `json:"name"`
		ClientDataJSON    b64    `json:"clientDataJSON"`
		AttestationObject b64    `json:"attestationObject"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.auth.AddPasskey(r.Context(), rp, userID(r), req.Name, req.ClientDataJSON, req.AttestationObject)
	if err != nil {
		s.passkeyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) renamePasskey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.auth.RenamePasskey(r.Context(), userID(r), id, req.Name); err != nil {
		if errors.Is(err, auth.ErrNoPasskey) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deletePasskey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.auth.DeletePasskey(r.Context(), userID(r), id); err != nil {
		if errors.Is(err, auth.ErrNoPasskey) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

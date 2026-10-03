// Package webauthn checks passkeys (Web Authentication, W3C level 2) for this one site: a new
// credential when it is registered, and an assertion when someone logs in with it. Attestation
// is not checked (the site asks for "none": any authenticator may be used); user verification
// (a fingerprint, face or PIN on the device) is required.
package webauthn

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// RP is the relying party: this site. ID is its host name; Origin the exact origin pages are
// served from, such as https://music.example.
type RP struct {
	ID     string
	Origin string
}

// Credential is a registered passkey: its ID, its public key as the authenticator gave it (a
// COSE key), and the authenticator's signature count when last used.
type Credential struct {
	ID        []byte
	PublicKey []byte
	SignCount uint32
}

// Algorithms the site accepts, in the order it asks for them (COSE numbers).
var Algorithms = []int64{algES256, algEdDSA, algRS256}

const (
	algES256 = -7
	algEdDSA = -8
	algRS256 = -257
)

const (
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified
	flagAT = 0x40 // attested credential data included
	flagED = 0x80 // extension data included
)

var (
	ErrInvalid = errors.New("the passkey response is not valid")
	ErrCloned  = errors.New("the passkey's counter went back: the authenticator may have been copied")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

// Challenge reads the challenge a client's data answers, to find which request it belongs to.
func Challenge(clientDataJSON []byte) ([]byte, error) {
	var cd clientData
	if err := json.Unmarshal(clientDataJSON, &cd); err != nil {
		return nil, invalid("client data: %v", err)
	}
	c, err := base64.RawURLEncoding.DecodeString(cd.Challenge)
	if err != nil {
		return nil, invalid("challenge: %v", err)
	}
	return c, nil
}

func (rp RP) checkClient(raw []byte, typ string, challenge []byte) error {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return invalid("client data: %v", err)
	}
	got, err := base64.RawURLEncoding.DecodeString(cd.Challenge)
	switch {
	case cd.Type != typ:
		return invalid("type %q", cd.Type)
	case err != nil || len(challenge) == 0 || subtle.ConstantTimeCompare(got, challenge) != 1:
		return invalid("the challenge does not match")
	case cd.Origin != rp.Origin:
		return invalid("origin %q", cd.Origin)
	case cd.CrossOrigin:
		return invalid("made in a frame of another site")
	}
	return nil
}

type authData struct {
	flags  byte
	count  uint32
	credID []byte
	key    []byte // COSE key, as given
}

// parseAuthData reads authenticator data, checks it is for this site and that the user was
// present and verified.
func (rp RP) parseAuthData(b []byte, attested bool) (*authData, error) {
	if len(b) < 37 {
		return nil, invalid("authenticator data too short")
	}
	want := sha256.Sum256([]byte(rp.ID))
	if subtle.ConstantTimeCompare(b[:32], want[:]) != 1 {
		return nil, invalid("made for another site")
	}
	ad := &authData{flags: b[32], count: binary.BigEndian.Uint32(b[33:37])}
	if ad.flags&flagUP == 0 {
		return nil, invalid("the user was not present")
	}
	if ad.flags&flagUV == 0 {
		return nil, invalid("the user was not verified on the device (fingerprint, face or PIN)")
	}
	rest := b[37:]
	if ad.flags&flagAT != 0 {
		if len(rest) < 18 {
			return nil, invalid("credential data too short")
		}
		n := int(binary.BigEndian.Uint16(rest[16:18]))
		rest = rest[18:]
		if n == 0 || n > 1023 || len(rest) < n {
			return nil, invalid("credential ID length")
		}
		ad.credID, rest = rest[:n:n], rest[n:]
		_, after, err := decode(rest, 0)
		if err != nil {
			return nil, invalid("public key: %v", err)
		}
		ad.key, rest = rest[:len(rest)-len(after):len(rest)-len(after)], after
	}
	if ad.flags&flagED != 0 {
		_, after, err := decode(rest, 0)
		if err != nil {
			return nil, invalid("extensions: %v", err)
		}
		rest = after
	}
	if len(rest) != 0 {
		return nil, invalid("trailing authenticator data")
	}
	if attested && ad.credID == nil {
		return nil, invalid("no credential in the registration")
	}
	return ad, nil
}

// Register checks a new credential answering challenge, and returns it to be stored.
func (rp RP) Register(challenge, clientDataJSON, attestationObject []byte) (*Credential, error) {
	if err := rp.checkClient(clientDataJSON, "webauthn.create", challenge); err != nil {
		return nil, err
	}
	v, rest, err := decode(attestationObject, 0)
	if err != nil || len(rest) != 0 {
		return nil, invalid("attestation object")
	}
	m, _ := v.(map[any]any)
	raw, ok := m["authData"].([]byte)
	if !ok {
		return nil, invalid("attestation object has no authenticator data")
	}
	ad, err := rp.parseAuthData(raw, true)
	if err != nil {
		return nil, err
	}
	if _, err := parseKey(ad.key); err != nil {
		return nil, err
	}
	return &Credential{ID: bytes.Clone(ad.credID), PublicKey: bytes.Clone(ad.key), SignCount: ad.count}, nil
}

// Login checks an assertion made with cred answering challenge, and returns the authenticator's
// new signature count, to be stored.
func (rp RP) Login(cred Credential, challenge, clientDataJSON, authenticatorData, signature []byte) (uint32, error) {
	if err := rp.checkClient(clientDataJSON, "webauthn.get", challenge); err != nil {
		return 0, err
	}
	ad, err := rp.parseAuthData(authenticatorData, false)
	if err != nil {
		return 0, err
	}
	key, err := parseKey(cred.PublicKey)
	if err != nil {
		return 0, err
	}
	hash := sha256.Sum256(clientDataJSON)
	if err := key.verify(append(bytes.Clone(authenticatorData), hash[:]...), signature); err != nil {
		return 0, err
	}
	// Many passkeys (synced ones) always say 0; a counter that is used must go up.
	if (ad.count != 0 || cred.SignCount != 0) && ad.count <= cred.SignCount {
		return 0, ErrCloned
	}
	return ad.count, nil
}

type publicKey struct {
	alg int64
	ec  *ecdsa.PublicKey
	ed  ed25519.PublicKey
	rsa *rsa.PublicKey
}

// parseKey reads a COSE public key (RFC 9053) of an accepted algorithm.
func parseKey(raw []byte) (*publicKey, error) {
	v, rest, err := decode(raw, 0)
	if err != nil || len(rest) != 0 {
		return nil, invalid("public key")
	}
	m, _ := v.(map[any]any)
	num := func(k int64) int64 { n, _ := m[k].(int64); return n }
	bin := func(k int64) []byte { b, _ := m[k].([]byte); return b }
	k := &publicKey{alg: num(3)}
	switch {
	case k.alg == algES256 && num(1) == 2 && num(-1) == 1 && len(bin(-2)) == 32 && len(bin(-3)) == 32:
		point := append(append([]byte{4}, bin(-2)...), bin(-3)...)
		if k.ec, err = ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point); err != nil {
			return nil, invalid("P-256 key: %v", err)
		}
	case k.alg == algEdDSA && num(1) == 1 && num(-1) == 6 && len(bin(-2)) == ed25519.PublicKeySize:
		k.ed = ed25519.PublicKey(bin(-2))
	case k.alg == algRS256 && num(1) == 3 && len(bin(-1)) >= 256 && len(bin(-2)) > 0 && len(bin(-2)) <= 4:
		e := new(big.Int).SetBytes(bin(-2))
		if e.Int64() < 3 || e.Int64()%2 == 0 {
			return nil, invalid("RSA exponent")
		}
		k.rsa = &rsa.PublicKey{N: new(big.Int).SetBytes(bin(-1)), E: int(e.Int64())}
	default:
		return nil, invalid("unsupported key (algorithm %d)", k.alg)
	}
	return k, nil
}

func (k *publicKey) verify(msg, sig []byte) error {
	ok := false
	switch k.alg {
	case algES256:
		h := sha256.Sum256(msg)
		ok = ecdsa.VerifyASN1(k.ec, h[:], sig)
	case algEdDSA:
		ok = ed25519.Verify(k.ed, msg, sig)
	case algRS256:
		h := sha256.Sum256(msg)
		ok = rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, h[:], sig) == nil
	}
	if !ok {
		return invalid("the signature does not verify")
	}
	return nil
}

// Package webauthntest is a software passkey for tests: it makes registrations and assertions as a
// device would.
package webauthntest

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"sort"
)

// COSE algorithms.
const (
	ES256 = -7
	EdDSA = -8
	RS256 = -257
)

// Authenticator flags.
const (
	UP = 0x01
	UV = 0x04
	AT = 0x40
)

// Encode is a small CBOR encoder (integers, byte and text strings, maps) with canonical key order.
func Encode(v any) []byte {
	var b bytes.Buffer
	head := func(major byte, n uint64) {
		switch {
		case n < 24:
			b.WriteByte(major<<5 | byte(n))
		case n < 1<<8:
			b.Write([]byte{major<<5 | 24, byte(n)})
		case n < 1<<16:
			b.WriteByte(major<<5 | 25)
			binary.Write(&b, binary.BigEndian, uint16(n))
		default:
			b.WriteByte(major<<5 | 26)
			binary.Write(&b, binary.BigEndian, uint32(n))
		}
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case int:
			if x >= 0 {
				head(0, uint64(x))
			} else {
				head(1, uint64(-1-x))
			}
		case []byte:
			head(2, uint64(len(x)))
			b.Write(x)
		case string:
			head(3, uint64(len(x)))
			b.WriteString(x)
		case map[any]any:
			head(5, uint64(len(x)))
			keys := make([][]byte, 0, len(x))
			byKey := map[string]any{}
			for k, v := range x {
				e := Encode(k)
				keys, byKey[string(e)] = append(keys, e), v
			}
			sort.Slice(keys, func(i, j int) bool {
				if len(keys[i]) != len(keys[j]) {
					return len(keys[i]) < len(keys[j])
				}
				return bytes.Compare(keys[i], keys[j]) < 0
			})
			for _, k := range keys {
				b.Write(k)
				walk(byKey[string(k)])
			}
		}
	}
	walk(v)
	return b.Bytes()
}

// Authenticator is a software passkey. Flags are what it reports (user present and verified by
// default); Count its signature counter.
type Authenticator struct {
	Alg   int
	ID    []byte
	Count uint32
	Flags byte
	ec    *ecdsa.PrivateKey
	ed    ed25519.PrivateKey
	rsa   *rsa.PrivateKey
}

func New(alg int) *Authenticator {
	id := make([]byte, 16)
	rand.Read(id)
	a := &Authenticator{Alg: alg, ID: id, Flags: UP | UV}
	switch alg {
	case ES256:
		a.ec, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case EdDSA:
		_, a.ed, _ = ed25519.GenerateKey(rand.Reader)
	default:
		a.rsa, _ = rsa.GenerateKey(rand.Reader, 2048)
	}
	return a
}

// Key is its public key as a COSE key.
func (a *Authenticator) Key() []byte {
	switch a.Alg {
	case ES256:
		raw, _ := a.ec.PublicKey.Bytes()
		return Encode(map[any]any{1: 2, 3: ES256, -1: 1, -2: raw[1:33], -3: raw[33:]})
	case EdDSA:
		return Encode(map[any]any{1: 1, 3: EdDSA, -1: 6, -2: []byte(a.ed.Public().(ed25519.PublicKey))})
	default:
		return Encode(map[any]any{1: 3, 3: RS256, -1: a.rsa.N.Bytes(), -2: big.NewInt(int64(a.rsa.E)).Bytes()})
	}
}

// AuthData is authenticator data for the site rpID; attested includes the credential.
func (a *Authenticator) AuthData(rpID string, attested bool) []byte {
	h := sha256.Sum256([]byte(rpID))
	flags := a.Flags
	if attested {
		flags |= AT
	}
	b := append(h[:], flags)
	b = binary.BigEndian.AppendUint32(b, a.Count)
	if attested {
		b = append(b, make([]byte, 16)...) // AAGUID
		b = binary.BigEndian.AppendUint16(b, uint16(len(a.ID)))
		b = append(append(b, a.ID...), a.Key()...)
	}
	return b
}

// ClientData is a client's data for a request.
func ClientData(typ string, challenge []byte, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": base64.RawURLEncoding.EncodeToString(challenge), "origin": origin})
	return b
}

// Create answers a registration: client data and attestation object.
func (a *Authenticator) Create(rpID, origin string, challenge []byte) (clientData, attestation []byte) {
	clientData = ClientData("webauthn.create", challenge, origin)
	attestation = Encode(map[any]any{"fmt": "none", "attStmt": map[any]any{}, "authData": a.AuthData(rpID, true)})
	return
}

// Get answers a login: client data, authenticator data and signature.
func (a *Authenticator) Get(rpID, origin string, challenge []byte) (clientData, authData, sig []byte) {
	clientData = ClientData("webauthn.get", challenge, origin)
	authData = a.AuthData(rpID, false)
	return clientData, authData, a.Sign(authData, clientData)
}

// Sign signs authenticator data and client data as an assertion.
func (a *Authenticator) Sign(authData, clientData []byte) []byte {
	h := sha256.Sum256(clientData)
	msg := append(bytes.Clone(authData), h[:]...)
	digest := sha256.Sum256(msg)
	switch a.Alg {
	case ES256:
		sig, _ := ecdsa.SignASN1(rand.Reader, a.ec, digest[:])
		return sig
	case EdDSA:
		return ed25519.Sign(a.ed, msg)
	default:
		sig, _ := rsa.SignPKCS1v15(rand.Reader, a.rsa, crypto.SHA256, digest[:])
		return sig
	}
}

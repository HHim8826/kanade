package webauthn

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/HHim8826/kanade/server/internal/webauthn/webauthntest"
)

var enc = webauthntest.Encode

type authenticator = webauthntest.Authenticator

func newAuthenticator(_ *testing.T, alg int) *authenticator { return webauthntest.New(alg) }

func clientJSON(typ string, challenge []byte, origin string) []byte {
	return webauthntest.ClientData(typ, challenge, origin)
}

var rp = RP{ID: "music.example", Origin: "https://music.example"}

func TestRegisterAndLogin(t *testing.T) {
	for _, alg := range []int{algES256, algEdDSA, algRS256} {
		a := newAuthenticator(t, alg)
		ch := []byte("registration challenge 0123456789")
		cd, att := a.Create(rp.ID, rp.Origin, ch)
		cred, err := rp.Register(ch, cd, att)
		if err != nil || !bytes.Equal(cred.ID, a.ID) {
			t.Fatalf("alg %d register: %v", alg, err)
		}
		if got, _ := Challenge(cd); !bytes.Equal(got, ch) {
			t.Fatal("challenge not read back")
		}
		login := []byte("login challenge 9876543210")
		cd, ad, sig := a.Get(rp.ID, rp.Origin, login)
		if n, err := rp.Login(*cred, login, cd, ad, sig); err != nil || n != 0 {
			t.Fatalf("alg %d login: %d %v", alg, n, err)
		}
	}
}

func TestCounter(t *testing.T) {
	a := newAuthenticator(t, algES256)
	ch := []byte("challenge-for-the-counter")
	cd, att := a.Create(rp.ID, rp.Origin, ch)
	cred, _ := rp.Register(ch, cd, att)
	a.Count = 5
	cd, ad, sig := a.Get(rp.ID, rp.Origin, ch)
	n, err := rp.Login(*cred, ch, cd, ad, sig)
	if err != nil || n != 5 {
		t.Fatalf("count 5: %d %v", n, err)
	}
	cred.SignCount = 5
	a.Count = 5 // a copy that signs with the same counter
	cd, ad, sig = a.Get(rp.ID, rp.Origin, ch)
	if _, err := rp.Login(*cred, ch, cd, ad, sig); !errors.Is(err, ErrCloned) {
		t.Fatalf("counter not going up: %v", err)
	}
	a.Count = 6
	cd, ad, sig = a.Get(rp.ID, rp.Origin, ch)
	if _, err := rp.Login(*cred, ch, cd, ad, sig); err != nil {
		t.Fatal(err)
	}
}

func TestForgeries(t *testing.T) {
	a := newAuthenticator(t, algES256)
	other := newAuthenticator(t, algES256)
	ch := []byte("the-challenge-of-this-request")
	cd, att := a.Create(rp.ID, rp.Origin, ch)
	cred, err := rp.Register(ch, cd, att)
	if err != nil {
		t.Fatal(err)
	}
	reg := func(name string, cd, att []byte) {
		t.Helper()
		if _, err := rp.Register(ch, cd, att); !errors.Is(err, ErrInvalid) {
			t.Errorf("register %s: %v", name, err)
		}
	}
	reg("another origin", clientJSON("webauthn.create", ch, "https://evil.example"), att)
	reg("a get answer", clientJSON("webauthn.get", ch, rp.Origin), att)
	reg("another challenge", clientJSON("webauthn.create", []byte("old challenge"), rp.Origin), att)
	cross, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": base64.RawURLEncoding.EncodeToString(ch),
		"origin": rp.Origin, "crossOrigin": true})
	reg("a cross-origin frame", cross, att)
	_, wrongSite := a.Create("evil.example", rp.Origin, ch)
	reg("another site's key", cd, wrongSite)
	a.Flags = flagUP
	_, noUV := a.Create(rp.ID, rp.Origin, ch)
	reg("no user verification", cd, noUV)
	a.Flags = flagUV
	_, noUP := a.Create(rp.ID, rp.Origin, ch)
	reg("no user presence", cd, noUP)
	a.Flags = flagUP | flagUV
	reg("trailing bytes", cd, append(bytes.Clone(att), 0))
	reg0 := a.AuthData(rp.ID, true)
	reg("trailing auth data", cd, enc(map[any]any{"fmt": "none", "authData": append(bytes.Clone(reg0), 1, 2)}))
	reg("cut credential", cd, enc(map[any]any{"fmt": "none", "authData": reg0[:60]}))
	es384 := enc(map[any]any{1: 2, 3: -35, -1: 2, -2: make([]byte, 48), -3: make([]byte, 48)})
	bad := append(bytes.Clone(reg0[:55+len(a.ID)]), es384...)
	reg("an unsupported algorithm", cd, enc(map[any]any{"fmt": "none", "authData": bad}))
	reg("no auth data", cd, enc(map[any]any{"fmt": "none"}))
	reg("indefinite CBOR", cd, []byte{0xbf, 0x63, 'f', 'm', 't', 0xff})
	reg("duplicate keys", cd, []byte{0xa2, 0x01, 0x01, 0x01, 0x02})
	reg("a huge length", cd, []byte{0x5b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	reg("a huge map", cd, []byte{0xbb, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	login := func(name string, cd, ad, sig []byte) {
		t.Helper()
		if _, err := rp.Login(*cred, ch, cd, ad, sig); !errors.Is(err, ErrInvalid) {
			t.Errorf("login %s: %v", name, err)
		}
	}
	cd, ad, sig := a.Get(rp.ID, rp.Origin, ch)
	login("another key's signature", cd, ad, other.Sign(ad, cd))
	tampered := bytes.Clone(ad)
	tampered[33] = 9 // the counter changed after signing
	login("changed data", cd, tampered, sig)
	login("another origin", clientJSON("webauthn.get", ch, "https://evil.example"), ad, a.Sign(ad, clientJSON("webauthn.get", ch, "https://evil.example")))
	login("another challenge", clientJSON("webauthn.get", []byte("x"), rp.Origin), ad, sig)
	evil := a.AuthData("evil.example", false)
	login("another site", cd, evil, a.Sign(evil, cd))
	a.Flags = flagUP
	unverified := a.AuthData(rp.ID, false)
	login("no user verification", cd, unverified, a.Sign(unverified, cd))
	login("an empty signature", cd, ad, nil)
}

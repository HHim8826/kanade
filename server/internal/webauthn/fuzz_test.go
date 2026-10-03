package webauthn

import "testing"

// Untrusted input never makes the parsers panic.
func FuzzParse(f *testing.F) {
	f.Add([]byte{0xa1, 0x68, 'a', 'u', 't', 'h', 'D', 'a', 't', 'a', 0x40})
	f.Add(enc(map[any]any{1: 2, 3: algES256, -1: 1, -2: make([]byte, 32), -3: make([]byte, 32)}))
	f.Add(append(make([]byte, 32), flagUP|flagUV|flagAT, 0, 0, 0, 1))
	f.Fuzz(func(t *testing.T, b []byte) {
		decode(b, 0)
		parseKey(b)
		rp.parseAuthData(b, true)
		rp.Register([]byte("c"), []byte(`{"type":"webauthn.create","challenge":"Yw","origin":"https://music.example"}`), b)
	})
}

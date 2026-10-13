package storage

import (
	"encoding/base64"
	"testing"
)

// Vectors of OneDrive's quickXorHash (as rclone's tests of it have them): input and sum in base64.
var quickXorVectors = []struct {
	size    int
	in, out string
}{
	{0, "", "AAAAAAAAAAAAAAAAAAAAAAAAAAA="},
	{1, "Sg==", "SgAAAAAAAAAAAAAAAQAAAAAAAAA="},
	{2, "tbQ=", "taAFAAAAAAAAAAAAAgAAAAAAAAA="},
	{7, "qaApEj66lw==", "qQBFCiTgA11cAgAABwAAAAAAAAA="},
	{8, "/aNzzCFPS/A=", "/RjFHJgRgicsAR4ACAAAAAAAAAA="},
	{16, "2fuxhBJXtpWFe8dOfdGeHw==", "O9tHLAghgSvYohKFyMMxnNCHaHg="},
	{20, "T6LYJIfDh81JrAK309H2JMJTXis=", "zBTHrspn3mEcohlJdIUAbjGNaNg="},
	{21, "DWAAX5/CIfrmErgZa8ot6ZraeSbu", "LR2Z0PjuRYGKQB/mhQAuMrAGZbQ="},
	{41, "0n7nl3YJtipy6yeUbVPWtc2h45WbF9u8hTz5tNwj3dZZwfXWkk+GN3g=", "YJLNK7JR64j9aODWfqDvEe/u6NU="},
	{64, "Mb7EGva2rEE5fENDL85P+BsapHEEjv2/siVhKjvAQe02feExVOQSkfmuYzU/kTF1MaKjPmKF/w+cbvwfdWL8aQ==", "n1anP5NfvD4XDYWIeRPW3ZkPv1Y="},
	{222, "PfxMcUd0vIW6VbHG/uj/Y0W6qEoKmyBD0nYebEKazKaKG+UaDqBEcmQjbfQeVnVLuodMoPp7P7TR1htX5n2VnkHh22xDyoJ8C/ZQKiSNqQfXvh83judf4RVr9exJCud8Uvgip6aVZTaPrJHVjQhMCp/dEnGvqg0oN5OVkM2qqAXvA0teKUDhgNM71sDBVBCGXxNOR2bpbD1iM4dnuT0ey4L+loXEHTL0fqMeUcEi2asgImnlNakwenDzz0x57aBwyq3AspCFGB1ncX4yYCr/OaCcS5OKi/00WH+wNQU3", "QX/YEpG0gDsmhEpCdWhsxDzsfVE="},
	{256, "qwGf2ESubE5jOUHHyc94ORczFYYbc2OmEzo+hBIyzJiNwAzC8PvJqtTzwkWkSslgHFGWQZR2BV5+uYTrYT7HVwRM40vqfj0dBgeDENyTenIOL1LHkjtDKoXEnQ0mXAHoJ8PjbNC93zi5TovVRXTNzfGEs5dpWVqxUzb5lc7dwkyvOluBw482mQ4xrzYyIY1t+//OrNi1ObGXuUw2jBQOFfJVj2Y6BOyYmfB1y36eBxi3zxeG5d5NYjm2GSh6e08QMAwu3zrINcqIzLOuNIiGXBtl7DjKt7b5wqi4oFiRpZsCyx2smhSrdrtK/CkdU6nDN+34vSR/M8rZpWQdBE7a8g==", "WYT9JY3JIo/pEBp+tIM6Gt2nyTM="},
	{333, "w0LGhqU1WXFbdavqDE4kAjEzWLGGzmTNikzqnsiXHx2KRReKVTxkv27u3UcEz9+lbMvYl4xFf2Z4aE1xRBBNd1Ke5C0zToSaYw5o4B/7X99nKK2/XaUX1byLow2aju2XJl2OpKpJg+tSJ2fmjIJTkfuYUz574dFX6/VXxSxwGH/xQEAKS5TCsBK3CwnuG1p5SAsQq3gGVozDWyjEBcWDMdy8/AIFrj/y03Lfc/RNRCQTAfZbnf2QwV7sluw4fH3XJr07UoD0YqN+7XZzidtrwqMY26fpLZnyZjnBEt1FAZWO7RnKG5asg8xRk9YaDdedXdQSJAOy6bWEWlABj+tVAigBxavaluUH8LOj+yfCFldJjNLdi90fVHkUD/m4Mr5OtmupNMXPwuG3EQlqWUVpQoYpUYKLsk7a5Mvg6UFkiH596y5IbJEVCI1Kb3D1", "e3+wo77iKcILiZegnzyUNcjCdoQ="},
}

func TestQuickXorHash(t *testing.T) {
	for _, v := range quickXorVectors {
		in, err := base64.StdEncoding.DecodeString(v.in)
		if err != nil || len(in) != v.size {
			t.Fatalf("vector %d: %v (%d bytes)", v.size, err, len(in))
		}
		for _, block := range []int{1, 3, 7, 64, 159, 160, 161, 1 << 20} {
			h := NewQuickXorHash()
			for i := 0; i < len(in); i += block {
				h.Write(in[i:min(i+block, len(in))])
			}
			if got := base64.StdEncoding.EncodeToString(h.Sum(nil)); got != v.out {
				t.Fatalf("%d bytes in blocks of %d: %s, want %s", v.size, block, got, v.out)
			}
		}
		c := NewChecker(Object{Hash: "quickxor:" + v.out})
		c.Write(in)
		if !c.Matches() {
			t.Fatalf("%d bytes: the checker does not match", v.size)
		}
	}
	var none *Checker
	if !none.Matches() || NewChecker(Object{Hash: ""}) != nil {
		t.Fatal("no checksum: nothing to check")
	}
	c := NewChecker(Object{Hash: "quickxor:" + quickXorVectors[1].out})
	c.Write([]byte("other"))
	if c.Matches() {
		t.Fatal("other bytes match")
	}
}

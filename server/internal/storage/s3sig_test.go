package storage

import (
	"net/http"
	"testing"
	"time"
)

// Signature Version 4 as AWS's own examples of it (the S3 documentation's "GET Object", "GET
// Bucket lifecycle" and "Get Bucket (List Objects)", for examplebucket in us-east-1).
func TestSignV4(t *testing.T) {
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		url, rng, want string
	}{
		{"https://examplebucket.s3.amazonaws.com/test.txt", "bytes=0-9", "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
		{"https://examplebucket.s3.amazonaws.com/?lifecycle", "", "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"},
		{"https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", "", "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	} {
		req, _ := http.NewRequest("GET", c.url, nil)
		if c.rng != "" {
			req.Header.Set("Range", c.rng)
		}
		signV4(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", emptySHA256, at)
		auth := req.Header.Get("Authorization")
		if want := "Signature=" + c.want; len(auth) < len(want) || auth[len(auth)-len(want):] != want {
			t.Errorf("%s: %s", c.url, auth)
		}
	}
}

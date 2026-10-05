package bangumi

import (
	"context"
	"errors"
	"testing"
)

func TestRef(t *testing.T) {
	for in, want := range map[string]int64{
		"531": 531, " 531 ": 531, "https://bgm.tv/subject/531": 531, "http://bangumi.tv/subject/1269?x=1": 1269,
		"chii.in/subject/12/": 12, "https://www.bgm.tv/subject/7#a": 7,
		"ARIA": 0, "https://example.com/subject/531": 0, "0": 0, "-3": 0, "https://bgm.tv/person/531": 0,
	} {
		got, ok := Ref(in)
		if (want == 0) == ok || (ok && got != want) {
			t.Errorf("Ref(%q) = %d %v, want %d", in, got, ok, want)
		}
	}
}

// Only Bangumi's pictures are fetched: an address a client names cannot make the server reach
// another host.
func TestOpenImageOnlyBangumi(t *testing.T) {
	c := New("test")
	for _, u := range []string{"http://lain.bgm.tv/a.jpg", "https://example.com/a.jpg", "https://bgm.tv.example.com/a.jpg", "file:///etc/passwd",
		"https://127.0.0.1/a.jpg", "https://evilbgm.tv/a.jpg"} {
		if _, err := c.OpenImage(context.Background(), u); !errors.Is(err, ErrImage) {
			t.Errorf("%s: %v", u, err)
		}
	}
}

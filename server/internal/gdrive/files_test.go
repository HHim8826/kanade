package gdrive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFolders is a Drive holding folders only, reached through the client's transport.
type fakeFolders struct {
	mu      sync.Mutex
	files   map[string]*File
	next    int
	created int
}

func (f *fakeFolders) add(name, parent string) string {
	f.next++
	id := fmt.Sprintf("f%d", f.next)
	f.files[id] = &File{ID: id, Name: name, MimeType: FolderMime, Parents: []string{parent}}
	return id
}

var queryName = regexp.MustCompile(`name = '([^']*)'.* '([^']*)' in parents`)

func (f *fakeFolders) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files")
	id = strings.TrimPrefix(id, "/")
	switch {
	case r.Method == http.MethodGet && id != "":
		file, ok := f.files[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"code":404,"message":"File not found"}}`))
			return
		}
		json.NewEncoder(w).Encode(file)
	case r.Method == http.MethodGet: // search by name and parent
		var out []File
		if m := queryName.FindStringSubmatch(r.URL.Query().Get("q")); m != nil {
			for _, file := range f.files {
				if file.Name == m[1] && len(file.Parents) > 0 && file.Parents[0] == m[2] && !file.Trashed {
					out = append(out, *file)
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"files": out})
	case r.Method == http.MethodPost:
		var body struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.created++
		json.NewEncoder(w).Encode(map[string]string{"id": f.add(body.Name, body.Parents[0])})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

type rewrite struct{ to string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.Scheme, u.Host = "http", strings.TrimPrefix(r.to, "http://")
	out := req.Clone(req.Context())
	out.URL = &u
	return http.DefaultTransport.RoundTrip(out)
}

// A cached folder moved out of the platform folder, trashed or deleted is not written to again: the
// path is made anew under the platform folder (review #7).
func TestCachedFolderDoesNotEscapeRoot(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	fake := &fakeFolders{files: map[string]*File{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c.http = &http.Client{Transport: rewrite{srv.URL}}
	c.ImportToken(ctx, Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)})
	root := fake.add(RootFolderName, "root")
	elsewhere := fake.add("Shared stuff", "root")

	lib, err := c.Folder(ctx, "library/A")
	if err != nil {
		t.Fatal(err)
	}
	libID := fake.files[lib].Parents[0]
	if fake.files[libID].Parents[0] != root {
		t.Fatal("library is not under the platform folder")
	}
	// Moved out of the platform folder in Drive: library and everything below is made anew.
	fake.files[libID].Parents = []string{elsewhere}
	c.checked = nil // the five-minute check is due
	again, err := c.Folder(ctx, "library/A")
	if err != nil {
		t.Fatal(err)
	}
	newLib := fake.files[again].Parents[0]
	if again == lib || newLib == libID || fake.files[newLib].Parents[0] != root {
		t.Fatalf("wrote into the moved folder: %s under %s", again, newLib)
	}
	// Trashed, and deleted.
	fake.files[again].Trashed = true
	c.checked = nil
	third, _ := c.Folder(ctx, "library/A")
	if third == again {
		t.Fatal("wrote into a trashed folder")
	}
	delete(fake.files, third)
	c.checked = nil
	fourth, _ := c.Folder(ctx, "library/A")
	if fourth == third || fake.files[fourth] == nil || fake.files[fourth].Parents[0] != newLib {
		t.Fatalf("after deletion: %s", fourth)
	}
	// Recently checked folders are not asked about again.
	before := fake.created
	c.Folder(ctx, "library/A")
	if fake.created != before {
		t.Fatal("made a folder that exists")
	}
}

package downloader

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Direct downloads: one file fetched over HTTP or HTTPS, an album's zip or a song, beside the
// torrents. It needs no file list and seeds nothing; otherwise it goes the way a torrent's one round
// does: queued, given staging space, downloaded by aria2, imported, cleared.

// Download kinds.
const (
	KindTorrent = "bt"   // a torrent or magnet
	KindDirect  = "http" // the one file a web link gives
)

// direct is a file a web link gives: its name and size.
type direct struct {
	name string
	size int64
}

// A bencoded dictionary starts with its first key's length: what a .torrent begins with.
var bencoded = regexp.MustCompile(`^d[0-9]+:`)

// typeExt is the extension a file of a type is saved with when its name has none Kanade imports.
var typeExt = map[string]string{
	"audio/flac": ".flac", "audio/x-flac": ".flac", "audio/mpeg": ".mp3", "audio/mp3": ".mp3", "audio/mp4": ".m4a",
	"audio/x-m4a": ".m4a", "audio/aac": ".aac", "audio/ogg": ".ogg", "audio/opus": ".opus", "audio/wav": ".wav",
	"audio/x-wav": ".wav", "audio/wave": ".wav", "application/zip": ".zip", "application/x-zip-compressed": ".zip",
}

// directExt: what the importer takes from a direct download, audio or a zip of it.
func directExt(ext string) bool { return audioExt[ext] || ext == ".zip" }

// probe looks at what a web link gives, reading no more than the start of a large answer: a
// .torrent, returned whole for Add to check, or a file to download directly. That file must be
// something the importer takes, audio or a zip, and the server must say how large it is, so that
// staging space can be set aside for it.
func probe(ctx context.Context, uri string) ([]byte, *direct, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching the link: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("the link answered HTTP %d", resp.StatusCode)
	}
	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	body := bufio.NewReaderSize(resp.Body, 64)
	head, _ := body.Peek(16)
	if ctype == "application/x-bittorrent" || bencoded.Match(head) {
		b, err := io.ReadAll(io.LimitReader(body, maxTorrent+1))
		if err != nil {
			return nil, nil, fmt.Errorf("fetching the torrent: %w", err)
		}
		return b, nil, nil
	}
	name := fileName(resp, ctype)
	switch ext := strings.ToLower(path.Ext(name)); {
	case directExt(ext):
	case ctype == "text/html" || ctype == "application/xhtml+xml":
		return nil, nil, errors.New("the link opens a web page, not a file: use the link of the file itself (or of its .torrent)")
	default:
		return nil, nil, fmt.Errorf("%q is not a file Kanade imports: a direct download is an audio file or a .zip of them", name)
	}
	if resp.ContentLength <= 0 {
		return nil, nil, errors.New("the server does not say how large the file is, so no staging space can be set aside for it")
	}
	return nil, &direct{name: name, size: resp.ContentLength}, nil
}

// fileName is what a direct download is saved as: the name the server gives (Content-Disposition),
// else the last part of the address it came from, after redirects; with the extension of its type
// when its own is not one Kanade imports.
func fileName(resp *http.Response, ctype string) string {
	var name string
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		name = params["filename"]
	}
	if name == "" && resp.Request != nil && resp.Request.URL != nil {
		name, _ = url.PathUnescape(path.Base(resp.Request.URL.Path))
	}
	name = cleanName(name)
	if e := typeExt[ctype]; e != "" && !directExt(strings.ToLower(path.Ext(name))) {
		name += e
	}
	return name
}

// cleanName makes a name safe as a file in the download's folder: no folders, control characters or
// leading dots, at most 200 bytes with its extension kept; "download" when nothing is left.
func cleanName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	if len(name) > 200 {
		ext := path.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		stem := name[:len(name)-len(ext)]
		for len(stem)+len(ext) > 200 { // whole characters
			_, size := utf8.DecodeLastRuneInString(stem)
			stem = stem[:len(stem)-size]
		}
		name = stem + ext
	}
	if name == "" {
		return "download"
	}
	return name
}

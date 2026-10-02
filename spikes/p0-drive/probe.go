package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func mediaURL(id string) string {
	return apiBase + "/files/" + url.PathEscape(id) + "?alt=media&acknowledgeAbuse=true"
}

// fetchRange returns the bytes [off, off+n) and the time to response headers.
func (d *drive) fetchRange(ctx context.Context, id string, off, n int64) ([]byte, time.Duration, *http.Response, error) {
	h := http.Header{}
	h.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	t0 := time.Now()
	resp, err := d.do(ctx, http.MethodGet, mediaURL(id), nil, h)
	if err != nil {
		return nil, 0, nil, err
	}
	ttfb := time.Since(t0)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, ttfb, resp, err
}

func cmdRange(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: range FILE_ID")
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	f, err := d.getFile(ctx, args[0], "id,name,size")
	if err != nil {
		return err
	}
	size := f.size()
	const n = 64 << 10
	if size < 2*n {
		return fmt.Errorf("%s is too small for this test (%d bytes)", f.Name, size)
	}
	fmt.Printf("%s, %.1f MiB; reading 64 KiB at each offset\n", f.Name, float64(size)/(1<<20))
	offsets := []int64{0, size / 10, size / 2, size * 9 / 10, size - n, 0, size / 2}
	for _, off := range offsets {
		t0 := time.Now()
		data, ttfb, resp, err := d.fetchRange(ctx, f.ID, off, n)
		if err != nil {
			return err
		}
		fmt.Printf("  offset %12d  status %d  %-34s  headers %4dms  total %4dms  got %d B\n",
			off, resp.StatusCode, resp.Header.Get("Content-Range"), ttfb.Milliseconds(), time.Since(t0).Milliseconds(), len(data))
	}
	return nil
}

func cmdChanges(ctx context.Context, _ []string) error {
	st, err := loadState()
	if err != nil {
		return err
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	if st.PageToken == "" {
		var r struct{ StartPageToken string }
		if err := d.call(ctx, http.MethodGet, apiBase+"/changes/startPageToken", nil, &r); err != nil {
			return err
		}
		st.PageToken = r.StartPageToken
		fmt.Printf("baseline page token %s saved; change files in Drive, then run `changes` again\n", st.PageToken)
		return st.save()
	}
	token, count := st.PageToken, 0
	for {
		var r struct {
			NextPageToken     string
			NewStartPageToken string
			Changes           []struct {
				ChangeType string
				Removed    bool
				FileID     string
				Time       string
				File       *struct {
					Name     string
					MimeType string
					Parents  []string
					Trashed  bool
				}
			}
		}
		v := url.Values{"pageToken": {token}, "pageSize": {"1000"}, "includeRemoved": {"true"}, "spaces": {"drive"},
			"fields": {"nextPageToken,newStartPageToken,changes(changeType,removed,fileId,time,file(name,mimeType,parents,trashed))"}}
		if err := d.call(ctx, http.MethodGet, apiBase+"/changes?"+v.Encode(), nil, &r); err != nil {
			return err
		}
		for _, c := range r.Changes {
			count++
			desc := "removed"
			if c.File != nil {
				desc = fmt.Sprintf("%q parents=%v trashed=%v", c.File.Name, c.File.Parents, c.File.Trashed)
			}
			fmt.Printf("  %s  %s  %s\n", c.Time, c.FileID, desc)
		}
		if r.NextPageToken != "" {
			token = r.NextPageToken
			continue
		}
		fmt.Printf("%d changes; new page token %s saved\n", count, r.NewStartPageToken)
		st.PageToken = r.NewStartPageToken
		return st.save()
	}
}

func cmdMove(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: move FILE_ID FOLDER_ID")
	}
	st, err := loadState()
	if err != nil {
		return err
	}
	if err := st.requireFolder(); err != nil {
		return err
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	for _, id := range args {
		ok, err := d.inTestTree(ctx, st, id)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s is outside %s; refusing to touch it", id, testFolderName)
		}
	}
	f, err := d.getFile(ctx, args[0], "id,name,parents")
	if err != nil {
		return err
	}
	t0 := time.Now()
	v := url.Values{"addParents": {args[1]}, "removeParents": {strings.Join(f.Parents, ",")}, "fields": {"id,name,parents"}}
	var moved driveFile
	if err := d.call(ctx, http.MethodPatch, apiBase+"/files/"+url.PathEscape(f.ID)+"?"+v.Encode(), map[string]any{}, &moved); err != nil {
		return err
	}
	fmt.Printf("moved %q to %v in %s (server side, no data transfer; file id unchanged: %v)\n",
		moved.Name, moved.Parents, time.Since(t0).Round(time.Millisecond), moved.ID == f.ID)
	return nil
}

// rangeReader serves byte ranges from a Drive file, fetching at least 64 KiB per request
// and counting requests and bytes, so we can see what a header-only scan costs.
type rangeReader struct {
	d        *drive
	ctx      context.Context
	id       string
	size     int64
	base     int64
	buf      []byte
	requests int
	fetched  int64
}

func (r *rangeReader) read(off int64, n int) ([]byte, error) {
	if off >= r.base && off+int64(n) <= r.base+int64(len(r.buf)) {
		return r.buf[off-r.base : off-r.base+int64(n)], nil
	}
	want := min(max(int64(n), 64<<10), r.size-off)
	if want < int64(n) {
		return nil, io.ErrUnexpectedEOF
	}
	data, _, _, err := r.d.fetchRange(r.ctx, r.id, off, want)
	if err != nil {
		return nil, err
	}
	r.requests++
	r.fetched += int64(len(data))
	r.base, r.buf = off, data
	if len(data) < n {
		return nil, io.ErrUnexpectedEOF
	}
	return data[:n], nil
}

func cmdHead(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: head FILE_ID")
	}
	d, err := newDrive()
	if err != nil {
		return err
	}
	f, err := d.getFile(ctx, args[0], "id,name,size,sha256Checksum")
	if err != nil {
		return err
	}
	r := &rangeReader{d: d, ctx: ctx, id: f.ID, size: f.size()}
	t0 := time.Now()
	fmt.Printf("%s (%.1f MiB), drive sha256 %q\n", f.Name, float64(r.size)/(1<<20), f.SHA256Checksum)

	hdr, err := r.read(0, 10)
	if err != nil {
		return err
	}
	pos := int64(0)
	if string(hdr[:3]) == "ID3" {
		tagSize := int64(hdr[6])<<21 | int64(hdr[7])<<14 | int64(hdr[8])<<7 | int64(hdr[9])
		fmt.Printf("ID3v2.%d tag, %d bytes\n", hdr[3], tagSize+10)
		pos = tagSize + 10
	}
	magic, err := r.read(pos, 4)
	if err != nil {
		return err
	}
	if string(magic) == "fLaC" {
		if err := readFLAC(r, pos+4); err != nil {
			return err
		}
	} else if pos == 0 {
		fmt.Println("neither ID3 nor FLAC; not an audio format this probe understands")
	}
	fmt.Printf("cost: %d Range requests, %d bytes fetched (%.3f%% of the file), %s\n",
		r.requests, r.fetched, 100*float64(r.fetched)/float64(r.size), time.Since(t0).Round(time.Millisecond))
	return nil
}

func readFLAC(r *rangeReader, pos int64) error {
	blockNames := map[byte]string{0: "STREAMINFO", 1: "PADDING", 2: "APPLICATION", 3: "SEEKTABLE", 4: "VORBIS_COMMENT", 5: "CUESHEET", 6: "PICTURE"}
	for {
		bh, err := r.read(pos, 4)
		if err != nil {
			return err
		}
		last, typ := bh[0]&0x80 != 0, bh[0]&0x7f
		length := int64(bh[1])<<16 | int64(bh[2])<<8 | int64(bh[3])
		pos += 4
		switch typ {
		case 0:
			b, err := r.read(pos, int(length))
			if err != nil {
				return err
			}
			x := binary.BigEndian.Uint64(b[10:18])
			sr, ch, bps, total := x>>44, (x>>41)&7+1, (x>>36)&31+1, x&(1<<36-1)
			md5set := strings.Trim(fmt.Sprintf("%x", b[18:34]), "0") != ""
			fmt.Printf("STREAMINFO: %d Hz, %d ch, %d bit, %.1fs, audio MD5 present=%v\n", sr, ch, bps, float64(total)/float64(max(sr, 1)), md5set)
		case 3:
			fmt.Printf("SEEKTABLE: %d points\n", length/18)
		case 4:
			b, err := r.read(pos, int(length))
			if err != nil {
				return err
			}
			printVorbisComment(b)
		case 6:
			fmt.Printf("PICTURE: %d bytes (skipped, not fetched)\n", length)
		default:
			fmt.Printf("%s: %d bytes\n", blockNames[typ], length)
		}
		pos += length
		if last || pos >= r.size {
			return nil
		}
	}
}

func printVorbisComment(b []byte) {
	le := binary.LittleEndian
	if len(b) < 8 {
		return
	}
	vlen := int(le.Uint32(b))
	q := 4 + vlen
	if q+4 > len(b) {
		return
	}
	count := int(le.Uint32(b[q:]))
	q += 4
	want := map[string]bool{"TITLE": true, "ALBUM": true, "ARTIST": true, "ALBUMARTIST": true, "ALBUM ARTIST": true, "TRACKNUMBER": true, "DISCNUMBER": true, "DATE": true}
	fmt.Printf("VORBIS_COMMENT: vendor %q, %d fields\n", b[4:4+vlen], count)
	for i := 0; i < count && q+4 <= len(b); i++ {
		l := int(le.Uint32(b[q:]))
		q += 4
		if q+l > len(b) {
			return
		}
		k, v, _ := strings.Cut(string(b[q:q+l]), "=")
		if want[strings.ToUpper(k)] {
			fmt.Printf("  %s=%s\n", strings.ToUpper(k), v)
		}
		q += l
	}
}

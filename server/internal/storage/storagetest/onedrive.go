package storagetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Graph is Microsoft's sign-in and a OneDrive in memory: what Kanade asks of them. Its token
// endpoint takes ClientID and ClientSecret, gives access tokens that Expire can end and refresh
// tokens that change each time they are used; its download links stop working when ExpireLinks
// says; its upload sessions take pieces with Content-Range. Fail can throttle requests.
type Graph struct {
	*httptest.Server
	ClientID, ClientSecret string
	Code                   string // the code a sign-in brings back; each works once
	Name                   string // the account's
	QuickXor               func([]byte) string

	mu       sync.Mutex
	items    map[string]*Item
	next     int
	access   map[string]bool
	refresh  string          // the last refresh token given
	live     map[string]bool // the refresh tokens that work: each sign-in's last
	used     map[string]bool // codes exchanged
	links    int             // the generation of download links
	sessions map[string]*upload
	// Fail answers a request with a status of its own and a Retry-After of 0 (0: as usual).
	Fail func(r *http.Request) int
	// Hold is given a token request answered (its tokens given out) before the answer is sent: a
	// test holds it back to have it come late.
	Hold func(r *http.Request)
	// Damage has a download of the item deliver other bytes than its own (its last one changed).
	Damage func(id string) bool
	// NoLinks leaves the download link out of the items it gives, as Graph has done for files of a
	// personal OneDrive: their /content is asked instead.
	NoLinks bool
	// ContentServes has /content send the bytes itself instead of redirecting to the download link.
	ContentServes bool
	// Requests counts requests by what they are ("token", "children", "download", "upload", ...).
	Requests map[string]int
}

// Item is a file or folder of the drive.
type Item struct {
	ID, Name, Parent string
	Folder           bool
	Data             []byte
	CTag             string
	Deleted          bool
}

type upload struct {
	parent, name string
	size         int64
	data         []byte
}

const driveID = "drive-1"

func NewGraph(t *testing.T) *Graph {
	g := &Graph{ClientID: "app-id", ClientSecret: "app-secret", Code: "the-code", Name: "Ann",
		items: map[string]*Item{"root": {ID: "ROOT!1", Name: "root", Folder: true}}, access: map[string]bool{},
		live: map[string]bool{}, used: map[string]bool{},
		sessions: map[string]*upload{}, Requests: map[string]int{}}
	g.items["ROOT!1"] = g.items["root"]
	delete(g.items, "root")
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	return g
}

// Login and API are what the storage package's endpoints are set to.
func (g *Graph) Login() string { return g.URL + "/login" }
func (g *Graph) API() string   { return g.URL + "/v1.0" }

func (g *Graph) id() string {
	g.next++
	return fmt.Sprintf("ITEM!%d", g.next)
}

// Add makes a file (data != nil) or folder at path ("Music/Album/01.flac"), with the folders above.
func (g *Graph) Add(path string, data []byte) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	parent := "ROOT!1"
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		last := i == len(segs)-1
		var found *Item
		for _, it := range g.items {
			if it.Parent == parent && it.Name == seg && !it.Deleted {
				found = it
			}
		}
		if found == nil {
			found = &Item{ID: g.id(), Name: seg, Parent: parent, Folder: !last || data == nil}
			g.items[found.ID] = found
		}
		if last && data != nil {
			found.Data, found.Folder = data, false
			found.CTag = fmt.Sprintf("ctag-%s-%d", found.ID, g.next)
			g.next++
		}
		parent = found.ID
	}
	return parent
}

// Get is the item with an ID; nil when none.
func (g *Graph) Get(id string) *Item {
	g.mu.Lock()
	defer g.mu.Unlock()
	if it := g.items[id]; it != nil {
		cp := *it
		return &cp
	}
	return nil
}

// Find is the item at path; nil when none.
func (g *Graph) Find(path string) *Item {
	g.mu.Lock()
	defer g.mu.Unlock()
	parent := "ROOT!1"
	var found *Item
	for _, seg := range strings.Split(path, "/") {
		found = nil
		for _, it := range g.items {
			if it.Parent == parent && it.Name == seg && !it.Deleted {
				found = it
			}
		}
		if found == nil {
			return nil
		}
		parent = found.ID
	}
	cp := *found
	return &cp
}

// Move renames or moves an item (its ID stays), as the OneDrive app would.
func (g *Graph) Move(id, parent, name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.items[id].Parent, g.items[id].Name = parent, name
}

// Delete moves an item to the recycle bin, as the OneDrive app would.
func (g *Graph) Delete(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.items[id].Deleted = true
}

// Expire ends the access tokens given so far.
func (g *Graph) Expire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	clear(g.access)
}

// ExpireLinks ends the download links given so far.
func (g *Graph) ExpireLinks() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.links++
}

// Refresh is the refresh token given last.
func (g *Graph) Refresh() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refresh
}

// Sessions counts the upload sessions not finished.
func (g *Graph) Sessions() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sessions)
}

func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func graphError(w http.ResponseWriter, status int, code string) {
	jsonOut(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}

func (g *Graph) count(kind string) {
	g.mu.Lock()
	g.Requests[kind]++
	g.mu.Unlock()
}

func (g *Graph) serve(w http.ResponseWriter, r *http.Request) {
	if g.Fail != nil {
		if code := g.Fail(r); code != 0 {
			w.Header().Set("Retry-After", "0")
			graphError(w, code, "activityLimitReached")
			return
		}
	}
	p := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(p, "/login/"):
		g.token(w, r)
	case strings.HasPrefix(p, "/download/"):
		g.download(w, r, strings.TrimPrefix(p, "/download/"))
	case strings.HasPrefix(p, "/upload/"):
		g.upload(w, r, strings.TrimPrefix(p, "/upload/"))
	case strings.HasPrefix(p, "/v1.0/"):
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		g.mu.Lock()
		ok := g.access[auth]
		g.mu.Unlock()
		if !ok {
			graphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
			return
		}
		g.api(w, r, strings.TrimPrefix(p, "/v1.0"))
	default:
		http.NotFound(w, r)
	}
}

func (g *Graph) token(w http.ResponseWriter, r *http.Request) {
	g.count("token")
	r.ParseForm()
	if r.Form.Get("client_id") != g.ClientID || r.Form.Get("client_secret") != g.ClientSecret {
		jsonOut(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret provided.\r\nTrace ID: x"})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		code := r.Form.Get("code")
		if code != g.Code || g.used[code] {
			jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "AADSTS70000: bad code"})
			return
		}
		g.used[code] = true
	case "refresh_token":
		if !g.live[r.Form.Get("refresh_token")] {
			jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "AADSTS70008: The refresh token has expired.\r\nTrace ID: x"})
			return
		}
		delete(g.live, r.Form.Get("refresh_token"))
	default:
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	g.next++
	access := fmt.Sprintf("access-%d", g.next)
	g.refresh = fmt.Sprintf("refresh-%d", g.next)
	g.live[g.refresh] = true
	g.access[access] = true
	scope := "Files.Read"
	if strings.Contains(r.Form.Get("scope"), "Files.ReadWrite") {
		scope = "Files.ReadWrite"
	}
	answer := map[string]any{"access_token": access, "refresh_token": g.refresh, "expires_in": 3600,
		"scope": "https://graph.microsoft.com/" + scope, "token_type": "Bearer"}
	if g.Hold != nil {
		g.mu.Unlock()
		g.Hold(r)
		g.mu.Lock()
	}
	jsonOut(w, http.StatusOK, answer)
}

// view is an item as Graph says it.
func (g *Graph) view(it *Item) map[string]any {
	v := map[string]any{"id": it.ID, "name": it.Name, "eTag": "etag-" + it.ID + "-" + it.CTag}
	if it.Parent != "" {
		v["parentReference"] = map[string]string{"driveId": driveID, "id": it.Parent, "path": "/drive/root:" + g.pathOf(it.Parent)}
	} else {
		v["parentReference"] = map[string]string{"driveId": driveID}
	}
	if it.Deleted {
		v["deleted"] = map[string]string{"state": "deleted"}
	}
	if it.Folder {
		n := 0
		for _, c := range g.items {
			if c.Parent == it.ID && !c.Deleted {
				n++
			}
		}
		v["folder"] = map[string]int{"childCount": n}
		return v
	}
	v["size"] = len(it.Data)
	v["cTag"] = it.CTag
	file := map[string]any{"mimeType": "application/octet-stream", "hashes": map[string]string{}}
	if g.QuickXor != nil {
		file["hashes"] = map[string]string{"quickXorHash": g.QuickXor(it.Data)}
	}
	v["file"] = file
	if !g.NoLinks {
		v["@microsoft.graph.downloadUrl"] = g.link(it)
	}
	return v
}

// link is the item's download link now.
func (g *Graph) link(it *Item) string {
	return fmt.Sprintf("%s/download/%s?gen=%d", g.URL, url.PathEscape(it.ID), g.links)
}

// pathOf is a folder's path below the root ("" for the root).
func (g *Graph) pathOf(id string) string {
	var segs []string
	for id != "ROOT!1" && id != "" {
		it := g.items[id]
		if it == nil {
			break
		}
		segs = append([]string{it.Name}, segs...)
		id = it.Parent
	}
	if len(segs) == 0 {
		return ""
	}
	return "/" + strings.Join(segs, "/")
}

func (g *Graph) child(parent, name string) *Item {
	for _, it := range g.items {
		if it.Parent == parent && it.Name == name && !it.Deleted {
			return it
		}
	}
	return nil
}

func (g *Graph) api(w http.ResponseWriter, r *http.Request, p string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case p == "/me/drive":
		g.Requests["drive"]++
		jsonOut(w, http.StatusOK, map[string]any{"id": driveID, "driveType": "personal", "owner": map[string]any{"user": map[string]string{"displayName": g.Name}}})
		return
	case p == "/drives/"+driveID:
		g.Requests["quota"]++
		jsonOut(w, http.StatusOK, map[string]any{"id": driveID, "quota": map[string]int64{"total": 5 << 30, "remaining": 4 << 30, "used": 1 << 30}})
		return
	}
	rest, ok := strings.CutPrefix(p, "/drives/"+driveID+"/items/")
	if !ok {
		graphError(w, http.StatusNotFound, "itemNotFound")
		return
	}
	// items/{id}, items/{id}/children, items/{id}:/{name}:, items/{id}:/{name}:/createUploadSession
	idPart, tail, _ := strings.Cut(rest, "/")
	if i := strings.Index(rest, ":"); i >= 0 && (strings.Index(rest, "/") < 0 || i < strings.Index(rest, "/")) {
		idPart, tail = rest[:i], rest[i:]
	}
	id, _ := url.PathUnescape(idPart)
	if id == "root" {
		id = "ROOT!1"
	}
	it := g.items[id]
	if it == nil {
		graphError(w, http.StatusNotFound, "itemNotFound")
		return
	}
	if strings.HasPrefix(tail, ":/") { // by name below it
		named, after, _ := strings.Cut(strings.TrimPrefix(tail, ":/"), ":")
		name, _ := url.PathUnescape(named)
		if after == "/createUploadSession" && r.Method == http.MethodPost {
			g.Requests["create session"]++
			if g.child(it.ID, name) != nil {
				graphError(w, http.StatusConflict, "nameAlreadyExists")
				return
			}
			g.next++
			sid := strconv.Itoa(g.next)
			g.sessions[sid] = &upload{parent: it.ID, name: name, size: -1}
			jsonOut(w, http.StatusOK, map[string]any{"uploadUrl": g.URL + "/upload/" + sid, "expirationDateTime": "2099-01-01T00:00:00Z"})
			return
		}
		c := g.child(it.ID, name)
		if c == nil {
			graphError(w, http.StatusNotFound, "itemNotFound")
			return
		}
		jsonOut(w, http.StatusOK, g.view(c))
		return
	}
	switch {
	case tail == "" && r.Method == http.MethodGet:
		g.Requests["item"]++
		jsonOut(w, http.StatusOK, g.view(it))
	case tail == "" && r.Method == http.MethodDelete:
		g.Requests["delete"]++
		if it.Deleted {
			graphError(w, http.StatusNotFound, "itemNotFound")
			return
		}
		it.Deleted = true
		w.WriteHeader(http.StatusNoContent)
	case tail == "content" && r.Method == http.MethodGet:
		g.Requests["content"]++
		if it.Deleted || it.Folder {
			graphError(w, http.StatusNotFound, "itemNotFound")
			return
		}
		if g.ContentServes {
			sendRange(w, r, it.Data)
			return
		}
		w.Header().Set("Location", g.link(it))
		w.WriteHeader(http.StatusFound)
	case tail == "children" && r.Method == http.MethodGet:
		g.Requests["children"]++
		var kids []*Item
		for _, c := range g.items {
			if c.Parent == it.ID {
				kids = append(kids, c)
			}
		}
		sortItems(kids)
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skiptoken"))
		page := kids[min(skip, len(kids)):]
		out := map[string]any{}
		if len(page) > 2 { // pages of 2 make next links show up
			page = page[:2]
			q := r.URL.Query()
			q.Set("$skiptoken", strconv.Itoa(skip+2))
			out["@odata.nextLink"] = g.URL + "/v1.0" + "/drives/" + driveID + "/items/" + url.PathEscape(it.ID) + "/children?" + q.Encode()
		}
		vals := []any{}
		for _, c := range page {
			vals = append(vals, g.view(c))
		}
		out["value"] = vals
		jsonOut(w, http.StatusOK, out)
	case tail == "children" && r.Method == http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if g.child(it.ID, req.Name) != nil {
			graphError(w, http.StatusConflict, "nameAlreadyExists")
			return
		}
		c := &Item{ID: g.id(), Name: req.Name, Parent: it.ID, Folder: true}
		g.items[c.ID] = c
		jsonOut(w, http.StatusCreated, g.view(c))
	default:
		graphError(w, http.StatusBadRequest, "invalidRequest")
	}
}

func sortItems(list []*Item) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Name < list[j-1].Name; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func (g *Graph) download(w http.ResponseWriter, r *http.Request, escaped string) {
	g.count("download")
	id, _ := url.PathUnescape(escaped)
	g.mu.Lock()
	it := g.items[id]
	gen := g.links
	g.mu.Unlock()
	if r.URL.Query().Get("gen") != strconv.Itoa(gen) {
		w.WriteHeader(http.StatusUnauthorized) // the link expired
		return
	}
	if it == nil || it.Deleted || it.Folder {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	data := it.Data
	if g.Damage != nil && g.Damage(id) && len(data) > 0 {
		data = append(append([]byte{}, data[:len(data)-1]...), data[len(data)-1]^0xff)
	}
	sendRange(w, r, data)
}

// sendRange sends data, or the range of it asked for.
func sendRange(w http.ResponseWriter, r *http.Request, data []byte) {
	if rng := r.Header.Get("Range"); rng != "" {
		a, b, _ := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
		start, _ := strconv.Atoi(a)
		end := len(data) - 1
		if b != "" {
			end, _ = strconv.Atoi(b)
			end = min(end, len(data)-1)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
		return
	}
	w.Write(data)
}

func (g *Graph) upload(w http.ResponseWriter, r *http.Request, sid string) {
	g.count("upload")
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.sessions[sid]
	if u == nil {
		graphError(w, http.StatusNotFound, "itemNotFound")
		return
	}
	if r.Header.Get("Authorization") != "" {
		graphError(w, http.StatusUnauthorized, "unauthenticated") // as Graph: an upload link carries its own
		return
	}
	next := func() []string { return []string{fmt.Sprintf("%d-", len(u.data))} }
	if r.Method == http.MethodGet {
		jsonOut(w, http.StatusOK, map[string]any{"nextExpectedRanges": next()})
		return
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
		graphError(w, http.StatusBadRequest, "invalidRange")
		return
	}
	if start != int64(len(u.data)) {
		graphError(w, http.StatusRequestedRangeNotSatisfiable, "invalidRange")
		return
	}
	body, _ := io.ReadAll(r.Body)
	if int64(len(body)) != end-start+1 {
		graphError(w, http.StatusBadRequest, "invalidRange")
		return
	}
	u.data = append(u.data, body...)
	if int64(len(u.data)) < total {
		jsonOut(w, http.StatusAccepted, map[string]any{"nextExpectedRanges": next()})
		return
	}
	delete(g.sessions, sid)
	if g.child(u.parent, u.name) != nil {
		graphError(w, http.StatusConflict, "nameAlreadyExists")
		return
	}
	it := &Item{ID: g.id(), Name: u.name, Parent: u.parent, Data: u.data}
	it.CTag = "ctag-" + it.ID
	g.items[it.ID] = it
	jsonOut(w, http.StatusCreated, g.view(it))
}

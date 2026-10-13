package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/storage"
	"github.com/HHim8826/kanade/server/internal/storesync"
)

// Storage locations besides Google Drive (#95, #99, #100): folders on the server or a NAS mounted
// there, S3-compatible buckets, OneDrives; managed (imports are kept there) or sources (imported
// where they are).

type storeInfo struct {
	storage.Store
	Use      library.StoreUse `json:"use"`
	Running  bool             `json:"running,omitempty"` // a scan or check now
	Location string           `json:"location"`          // where it is, for people
	SignedIn bool             `json:"signed_in"`         // a OneDrive's sign-in is kept (true for others)
}

// location says where a store is: its folder, bucket or OneDrive folder.
func location(st storage.Store) string {
	var v struct {
		Root, Bucket, Prefix, Endpoint, Path, Account string
	}
	json.Unmarshal(st.Config, &v)
	switch st.Kind {
	case storage.KindLocal:
		return v.Root
	case storage.KindS3:
		return strings.TrimSuffix(v.Bucket+"/"+v.Prefix, "/")
	}
	if v.Path == "" {
		return "OneDrive"
	}
	return "OneDrive " + v.Path
}

func (s *Server) storeInfo(ctx context.Context, st storage.Store) (storeInfo, error) {
	use, err := s.lib.StoreUse(ctx, st.Kind, st.ID)
	info := storeInfo{Store: st, Use: use, Location: location(st), SignedIn: st.SignedIn()}
	s.storeMu.Lock()
	info.Running = s.storeRunning[st.ID]
	s.storeMu.Unlock()
	return info, err
}

func (s *Server) storageInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	def, err := s.files.Default(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	stores, err := s.files.Stores(ctx)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	list := make([]storeInfo, 0, len(stores))
	for _, st := range stores {
		info, err := s.storeInfo(ctx, st)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		list = append(list, info)
	}
	drive := map[string]any{"connected": false}
	if s.drive != nil {
		if st, err := s.drive.Status(ctx); err == nil {
			drive["connected"] = st.Connected
		}
	}
	var driveSongs int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM assets WHERE file_ref IS NOT NULL AND instr(file_ref, ':') = 0`).Scan(&driveSongs)
	drive["songs"] = driveSongs
	writeJSON(w, http.StatusOK, map[string]any{"default": def, "drive": drive, "stores": list, "data_dir": s.cfg.DataDir,
		"onedrive_redirect": s.oneDriveRedirect(), "onedrive_paste": s.oneDrivePaste()})
}

// storeError answers for a store operation that failed.
func (s *Server) storeError(w http.ResponseWriter, r *http.Request, err error) {
	var bad *storage.ErrInvalid
	switch {
	case errors.As(err, &bad), errors.Is(err, storage.ErrBadState):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, storage.ErrNotSignedIn):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, storage.ErrNoStore):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, storage.ErrReadOnly), errors.Is(err, library.ErrStoreInUse), errors.Is(err, importer.ErrScanning),
		errors.Is(err, storesync.ErrRunning):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, storage.ErrOffline), errors.Is(err, storage.ErrAuth):
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		var se *storage.Error
		if errors.As(err, &se) {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		s.internal(w, r, err)
	}
}

func (s *Server) addStore(w http.ResponseWriter, r *http.Request) {
	var req storage.NewStore
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st, err := s.files.Add(r.Context(), req)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	info, err := s.storeInfo(r.Context(), *st)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) store(w http.ResponseWriter, r *http.Request) (*storage.Store, bool) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil, false
	}
	st, err := s.files.Get(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return nil, false
	}
	if st == nil {
		writeError(w, http.StatusNotFound, storage.ErrNoStore)
		return nil, false
	}
	return st, true
}

// changeStore renames a store, says whether a source's new files are imported by themselves,
// changes its keys, or chooses a OneDrive's folder.
func (s *Server) changeStore(w http.ResponseWriter, r *http.Request) {
	st, ok := s.store(w, r)
	if !ok {
		return
	}
	var req struct {
		Name     *string         `json:"name"`
		AutoScan *bool           `json:"auto_scan"`
		Secret   json.RawMessage `json:"secret"` // a bucket's new keys, a OneDrive app's client secret
		Folder   *string         `json:"folder"` // a OneDrive's folder (its item ID)
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Name != nil {
		if err := s.files.Rename(r.Context(), st.ID, *req.Name); err != nil {
			s.storeError(w, r, err)
			return
		}
	}
	if req.AutoScan != nil {
		if err := s.files.SetAutoScan(r.Context(), st.ID, *req.AutoScan); err != nil {
			s.internal(w, r, err)
			return
		}
	}
	if req.Folder != nil && !s.setStoreFolder(w, r, st, *req.Folder) {
		return
	}
	if len(req.Secret) > 0 {
		if _, err := s.files.SetKeys(r.Context(), st.ID, req.Secret); err != nil {
			s.storeError(w, r, err)
			return
		}
	}
	st, err := s.files.Get(r.Context(), st.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	info, err := s.storeInfo(r.Context(), *st)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// removeStore removes a store. A source's songs leave the library with it (its files stay where
// they are); a managed store must hold none of the library's files: moving them elsewhere is a
// feature of its own.
func (s *Server) removeStore(w http.ResponseWriter, r *http.Request) {
	st, ok := s.store(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if s.busy(st.ID) {
		writeError(w, http.StatusConflict, errors.New("this storage location is being scanned or checked"))
		return
	}
	songs := 0
	if st.Mode == storage.Managed {
		use, err := s.lib.StoreUse(ctx, st.Kind, st.ID)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if use.Songs+use.Others > 0 {
			s.storeError(w, r, fmt.Errorf("%w: %d songs and %d covers or sidecars", library.ErrStoreInUse, use.Songs, use.Others))
			return
		}
	} else {
		var err error
		if songs, err = s.lib.ForgetStore(ctx, st.Kind, st.ID); err != nil {
			s.internal(w, r, err)
			return
		}
	}
	if err := s.files.Remove(ctx, st.ID); err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"songs": songs})
}

func (s *Server) checkStore(w http.ResponseWriter, r *http.Request) {
	st, ok := s.store(w, r)
	if !ok {
		return
	}
	st, err := s.files.Check(r.Context(), st.ID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	info, err := s.storeInfo(r.Context(), *st)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) busy(id int64) bool {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	return s.storeRunning[id]
}

// scanStore scans a source ({"preview": true} has its new files wait in the import review), or
// checks that a managed location holds the library's files, in the background: GET /storage says
// when it is done and what it found.
func (s *Server) scanStore(w http.ResponseWriter, r *http.Request) {
	st, ok := s.store(w, r)
	if !ok {
		return
	}
	var req struct {
		Preview bool `json:"preview"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if st.Mode == storage.Managed && s.storeSync == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("storage checks are not running"))
		return
	}
	s.storeMu.Lock()
	if s.storeRunning[st.ID] {
		s.storeMu.Unlock()
		writeError(w, http.StatusConflict, importer.ErrScanning)
		return
	}
	s.storeRunning[st.ID] = true
	s.storeMu.Unlock()
	go func() {
		defer func() {
			s.storeMu.Lock()
			delete(s.storeRunning, st.ID)
			s.storeMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		var err error
		if st.Mode == storage.Source {
			_, err = s.importer.ScanSource(ctx, s.files, st.ID, req.Preview)
		} else {
			_, err = s.storeSync.Reconcile(ctx, st.ID)
		}
		if err != nil {
			s.log.Warn("storage scan", "store", st.Name, "err", err)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
}

// setDefaultStore sends new imports to Google Drive ("drive") or a managed store (its ID).
func (s *Server) setDefaultStore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var id string
	if json.Unmarshal(req.ID, &id) != nil {
		var n int64
		if err := json.Unmarshal(req.ID, &n); err != nil {
			writeError(w, http.StatusBadRequest, errors.New(`id: "drive" or a storage location's ID`))
			return
		}
		id = strconv.FormatInt(n, 10)
	}
	if err := s.files.SetDefault(r.Context(), id); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

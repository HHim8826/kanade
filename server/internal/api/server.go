// Package api is the HTTP surface: public pages, the OAuth callback and the JSON API.
package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/auth"
	"github.com/HHim8826/kanade/server/internal/backup"
	"github.com/HHim8826/kanade/server/internal/bangumi"
	"github.com/HHim8826/kanade/server/internal/clientip"
	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/discord"
	"github.com/HHim8826/kanade/server/internal/diskguard"
	"github.com/HHim8826/kanade/server/internal/downloader"
	"github.com/HHim8826/kanade/server/internal/drivesync"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/identify"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/loudness"
	"github.com/HHim8826/kanade/server/internal/lrclib"
	"github.com/HHim8826/kanade/server/internal/presence"
	"github.com/HHim8826/kanade/server/internal/rss"
	"github.com/HHim8826/kanade/server/internal/settings"
	"github.com/HHim8826/kanade/server/internal/staging"
	"github.com/HHim8826/kanade/server/internal/stream"
	"github.com/HHim8826/kanade/server/internal/thumbs"
	"github.com/HHim8826/kanade/server/internal/uploads"
	"github.com/HHim8826/kanade/server/internal/web"
)

//go:embed pages/*.html
var pages embed.FS

// Deps are the services the HTTP layer exposes.
type Deps struct {
	Config    config.Config
	DB        *sql.DB
	Auth      *auth.Service
	Drive     *gdrive.Client
	Library   *library.Store
	Importer  *importer.Importer
	Cache     *stream.Cache
	Downloads *downloader.Service
	Aria2     *downloader.Aria2
	Uploads   *uploads.Store
	Identify  *identify.MusicBrainz
	Lyrics    *lrclib.Client
	RSS       *rss.Service
	Disk      *diskguard.Guard
	Sync      *drivesync.Syncer
	Loudness  *loudness.Service
	Thumbs    *thumbs.Store    // covers made for the web client; nil: one in the data directory
	Backup    *backup.Service  // copies of the database in Drive; nil: none
	Bangumi   *bangumi.Client  // works' descriptions (review #94); nil: one for this version
	Presence  *presence.Hub    // what web players play (review #135); nil: one of its own
	Discord   *discord.Service // shows it as the Discord status; nil: one that is not run
	Settings  *settings.Store  // the service settings the settings page changes
	Staging   *staging.Budget  // shared by downloads, uploads and the importer
	Pinned    map[string]bool  // resources given as serve flags: they win until the next start
	StreamKey []byte           // HMAC key for signed stream URLs
	Log       *slog.Logger
	Version   string
}

type Server struct {
	cfg         config.Config
	db          *sql.DB
	auth        *auth.Service
	drive       *gdrive.Client
	lib         *library.Store
	importer    *importer.Importer
	cache       *stream.Cache
	downloads   *downloader.Service
	aria2       *downloader.Aria2
	settings    *settings.Store
	staging     *staging.Budget
	pinned      map[string]bool
	uploads     *uploads.Store
	mb          *identify.MusicBrainz
	lrclib      *lrclib.Client
	rss         *rss.Service
	disk        *diskguard.Guard
	sync        *drivesync.Syncer
	loudness    *loudness.Service
	thumbs      *thumbs.Store
	backup      *backup.Service
	bgm         *bangumi.Client
	bgmAccounts *bangumi.Accounts
	presence    *presence.Hub
	discord     *discord.Service
	proxy       clientip.Policy
	streamKey   []byte
	log         *slog.Logger
	version     string
	started     time.Time
	unproxied   sync.Once // the log said a proxy's headers are not believed

	refreshing sync.Map // works whose description is being read again
}

// ThumbsBudget is the most the covers made for the web client take on disk.
const ThumbsBudget = 256 << 20

// New makes the server; d.Config.TrustedProxy was checked (clientip.Parse) by the caller.
func New(d Deps) *Server {
	proxy, _ := clientip.Parse(d.Config.TrustedProxy)
	if d.Thumbs == nil {
		d.Thumbs = thumbs.New(d.Config.Path(config.DirThumbs), ThumbsBudget, d.Log)
	}
	if d.Bangumi == nil {
		d.Bangumi = bangumi.New(d.Version)
	}
	if d.Presence == nil {
		d.Presence = presence.NewHub()
	}
	if d.Discord == nil {
		d.Discord = discord.New(d.DB, d.Presence, d.Config.PublicURL, d.Log)
	}
	srv := &Server{proxy: proxy, bgm: d.Bangumi, bgmAccounts: bangumi.NewAccounts(d.DB, d.Bangumi, d.Config.PublicURL), presence: d.Presence, discord: d.Discord, thumbs: d.Thumbs, backup: d.Backup, cfg: d.Config, db: d.DB, auth: d.Auth, drive: d.Drive, lib: d.Library, importer: d.Importer,
		cache: d.Cache, downloads: d.Downloads, aria2: d.Aria2, uploads: d.Uploads, mb: d.Identify, lrclib: d.Lyrics, rss: d.RSS, disk: d.Disk, sync: d.Sync, streamKey: d.StreamKey, log: d.Log, version: d.Version, started: time.Now(),
		settings: d.Settings, staging: d.Staging, pinned: d.Pinned, loudness: d.Loudness}
	d.Discord.CoverURL = srv.publicCoverURL
	return srv
}

type ctxKey int

const (
	userKey    ctxKey = 0
	sessionKey ctxKey = 1 // the login's ID
)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page("pages/index.html"))
	mux.HandleFunc("GET /privacy", s.page("pages/privacy.html"))
	mux.HandleFunc("GET /oauth/google/callback", s.oauthCallback)
	mux.HandleFunc("GET /oauth/discord/callback", s.discordCallback)
	mux.HandleFunc("GET /oauth/bangumi/callback", s.bgmCallback)
	mux.HandleFunc("GET /pub/covers/{id}/{file}", s.publicCover)
	mux.Handle("GET /app/", web.Handler())
	mux.Handle("GET /app", http.RedirectHandler("/app/", http.StatusMovedPermanently))

	mux.Handle("POST /api/v1/setup", sameOrigin(s.setup))
	mux.Handle("POST /api/v1/login", sameOrigin(s.login))
	mux.HandleFunc("GET /api/v1/passkeys/available", s.passkeysAvailable)
	mux.Handle("POST /api/v1/passkeys/login/options", sameOrigin(s.passkeyLoginOptions))
	mux.Handle("POST /api/v1/passkeys/login", sameOrigin(s.passkeyLogin))
	mux.Handle("GET /api/v1/passkeys", s.authed(s.listPasskeys))
	mux.Handle("POST /api/v1/passkeys/options", s.authed(s.passkeyOptions))
	mux.Handle("POST /api/v1/passkeys", s.authed(s.addPasskey))
	mux.Handle("PATCH /api/v1/passkeys/{id}", s.authed(s.renamePasskey))
	mux.Handle("DELETE /api/v1/passkeys/{id}", s.authed(s.deletePasskey))
	mux.Handle("POST /api/v1/logout", s.authed(s.logout))
	mux.Handle("POST /api/v1/account/password", s.authed(s.changePassword))
	mux.Handle("GET /api/v1/sessions", s.authed(s.sessions))
	mux.Handle("DELETE /api/v1/sessions/{id}", s.authed(s.endSession))
	mux.Handle("POST /api/v1/sessions/end-others", s.authed(s.endOtherSessions))
	mux.Handle("GET /api/v1/settings", s.authed(s.serviceSettings))
	mux.Handle("PUT /api/v1/settings/resources", s.authed(s.setResources))
	mux.Handle("PUT /api/v1/settings/downloads", s.authed(s.setDownloads))
	mux.Handle("PUT /api/v1/settings/drive", s.authed(s.setDriveSync))
	mux.Handle("POST /api/v1/cache/trim", s.authed(s.trimCache))
	mux.Handle("GET /api/v1/status", s.authed(s.status))
	mux.Handle("GET /api/v1/drive", s.authed(s.driveInfo))
	mux.Handle("POST /api/v1/drive/client", s.authed(s.driveSetClient))
	mux.Handle("POST /api/v1/drive/auth", s.authed(s.driveAuth))
	mux.Handle("POST /api/v1/drive/auth/paste", s.authed(s.driveAuthPaste))
	mux.Handle("GET /api/v1/drive/sync", s.authed(s.driveSync))
	mux.Handle("POST /api/v1/drive/reconcile", s.authed(s.driveReconcile))
	mux.Handle("POST /api/v1/drive/inbox", s.authed(s.driveInbox))
	mux.Handle("GET /api/v1/library/missing", s.authed(s.missing))

	mux.Handle("GET /api/v1/home", s.authed(s.home))
	mux.Handle("POST /api/v1/plays", s.authed(s.recordPlay))
	mux.Handle("GET /api/v1/assets/{id}/resume", s.authed(s.resumePosition))
	mux.Handle("GET /api/v1/albums/random", s.authed(s.randomAlbum))
	mux.Handle("GET /api/v1/albums", s.authed(s.albums))
	mux.Handle("GET /api/v1/albums/{id}", s.authed(s.album))
	mux.Handle("GET /api/v1/tracks", s.authed(s.tracks))
	mux.Handle("GET /api/v1/tracks/random", s.authed(s.randomTracks))
	mux.Handle("GET /api/v1/artists", s.authed(s.artists))
	mux.Handle("GET /api/v1/artists/{id}", s.authed(s.artist))
	mux.Handle("GET /api/v1/search", s.authed(s.search))

	mux.Handle("GET /api/v1/favorites", s.authed(s.favorites))
	mux.Handle("GET /api/v1/favorites/ids", s.authed(s.favoriteIDs))
	mux.Handle("PUT /api/v1/favorites/tracks/{id}", s.authed(s.setFavorite("track")))
	mux.Handle("DELETE /api/v1/favorites/tracks/{id}", s.authed(s.setFavorite("track")))
	mux.Handle("PUT /api/v1/favorites/albums/{id}", s.authed(s.setFavorite("album")))
	mux.Handle("DELETE /api/v1/favorites/albums/{id}", s.authed(s.setFavorite("album")))
	mux.Handle("GET /api/v1/playlists", s.authed(s.playlists))
	mux.Handle("POST /api/v1/playlists", s.authed(s.createPlaylist))
	mux.Handle("GET /api/v1/playlists/{id}", s.authed(s.playlist))
	mux.Handle("PATCH /api/v1/playlists/{id}", s.authed(s.updatePlaylist))
	mux.Handle("DELETE /api/v1/playlists/{id}", s.authed(s.deletePlaylist))
	mux.Handle("POST /api/v1/playlists/{id}/items", s.authed(s.addPlaylistItems))
	mux.Handle("DELETE /api/v1/playlists/{id}/items/{item}", s.authed(s.removePlaylistItem))
	mux.Handle("PUT /api/v1/playlists/{id}/order", s.authed(s.reorderPlaylist))
	mux.Handle("POST /api/v1/playlists/preview", s.authed(s.previewRules))
	mux.Handle("PUT /api/v1/playlists/{id}/rules", s.authed(s.setPlaylistRules))
	mux.Handle("GET /api/v1/playlists/{id}/next", s.authed(s.smartNext))
	mux.Handle("GET /api/v1/tracks/{id}/lyrics", s.authed(s.lyrics))
	mux.Handle("PUT /api/v1/tracks/{id}/lyrics", s.authed(s.setLyrics))
	mux.Handle("DELETE /api/v1/tracks/{id}/lyrics", s.authed(s.setLyrics))
	mux.Handle("GET /api/v1/loudness", s.authed(s.getLoudness))
	mux.Handle("GET /api/v1/loudness/scan", s.authed(s.loudnessScan))
	mux.Handle("POST /api/v1/loudness/scan", s.authed(s.runLoudnessScan))
	mux.Handle("GET /api/v1/backup", s.authed(s.backupState))
	mux.Handle("POST /api/v1/backup", s.authed(s.backupNow))
	mux.Handle("GET /api/v1/tracks/{id}/lyrics/online", s.authed(s.findLyrics))
	mux.Handle("POST /api/v1/tracks/{id}/lyrics/online", s.authed(s.useFoundLyrics))
	mux.Handle("GET /api/v1/history", s.authed(s.history))
	mux.Handle("GET /api/v1/history/top", s.authed(s.topTracks))

	mux.Handle("GET /api/v1/tracks/{id}", s.authed(s.track))
	mux.Handle("PATCH /api/v1/tracks/{id}", s.authed(s.editTrack))
	mux.Handle("DELETE /api/v1/tracks/{id}", s.authed(s.deleteTrack))
	mux.Handle("POST /api/v1/tracks/{id}/restore", s.authed(s.restoreTrack))
	mux.Handle("PATCH /api/v1/albums/{id}", s.authed(s.editAlbum))
	mux.Handle("DELETE /api/v1/albums/{id}", s.authed(s.deleteAlbum))
	mux.Handle("PUT /api/v1/albums/{id}/cover", s.authed(s.setAlbumCover))
	mux.Handle("POST /api/v1/albums/{id}/merge", s.authed(s.mergeAlbum))
	mux.Handle("POST /api/v1/albums/{id}/split", s.authed(s.splitAlbum))
	mux.Handle("POST /api/v1/albums/{id}/remove", s.authed(s.removeEntries))
	mux.Handle("POST /api/v1/albums/{id}/restore", s.authed(s.restoreAlbum))
	mux.Handle("PUT /api/v1/albums/{id}/sections", s.authed(s.setSections))
	mux.Handle("POST /api/v1/albums/merge", s.authed(s.mergeAlbums))
	mux.Handle("GET /api/v1/bookmarks", s.authed(s.bookmarks))
	mux.Handle("POST /api/v1/bookmarks", s.authed(s.addBookmark))
	mux.Handle("PATCH /api/v1/bookmarks/{id}", s.authed(s.updateBookmark))
	mux.Handle("DELETE /api/v1/bookmarks/{id}", s.authed(s.deleteBookmark))
	mux.Handle("GET /api/v1/stats/days", s.authed(s.statsDays))
	mux.Handle("GET /api/v1/stats/day", s.authed(s.statsDay))
	mux.Handle("GET /api/v1/stats/summary", s.authed(s.statsSummary))
	mux.Handle("GET /api/v1/stats/top", s.authed(s.statsTop))
	mux.Handle("GET /api/v1/stats/trends", s.authed(s.statsTrends))
	mux.Handle("GET /api/v1/stats/export", s.authed(s.statsExport))
	mux.Handle("DELETE /api/v1/stats", s.authed(s.statsClear))
	mux.Handle("GET /api/v1/categories", s.authed(s.categories))
	mux.Handle("POST /api/v1/categories", s.authed(s.createCategory))
	mux.Handle("PATCH /api/v1/categories/{id}", s.authed(s.renameCategory))
	mux.Handle("DELETE /api/v1/categories/{id}", s.authed(s.deleteCategory))
	mux.Handle("POST /api/v1/albums/categorize", s.authed(s.categorize))
	mux.Handle("POST /api/v1/albums/edit", s.authed(s.editAlbums))
	mux.Handle("POST /api/v1/albums/remove", s.authed(s.removeAlbums))
	mux.Handle("POST /api/v1/tracks/edit", s.authed(s.editTracks))
	mux.Handle("POST /api/v1/tracks/place", s.authed(s.placeSongs))
	mux.Handle("POST /api/v1/tracks/delete", s.authed(s.removeTracks))
	mux.Handle("POST /api/v1/favorites/batch", s.authed(s.setFavorites))
	mux.Handle("GET /api/v1/albums/{id}/identify", s.authed(s.identifySearch))
	mux.Handle("GET /api/v1/albums/{id}/identify/{release}", s.authed(s.identifyPropose))
	mux.Handle("POST /api/v1/albums/{id}/identify/{release}", s.authed(s.identifyApply))
	mux.Handle("POST /api/v1/albums/{id}/vgmdb", s.authed(s.vgmdbPropose))
	mux.Handle("POST /api/v1/albums/{id}/vgmdb/apply", s.authed(s.vgmdbApply))
	mux.Handle("PATCH /api/v1/artists/{id}", s.authed(s.editArtist))
	mux.Handle("GET /api/v1/organize/folders", s.authed(s.folderAlbums))
	mux.Handle("POST /api/v1/organize/folders", s.authed(s.makeFolderAlbums))
	mux.Handle("GET /api/v1/edits", s.authed(s.editGroups))
	mux.Handle("GET /api/v1/edits/{id}", s.authed(s.editGroup))
	mux.Handle("POST /api/v1/edits/{id}/undo", s.authed(s.undo))
	mux.Handle("GET /api/v1/covers/{id}", s.authed(s.cover))
	mux.Handle("GET /api/v1/bangumi/search", s.authed(s.bangumiSearch))
	mux.Handle("GET /api/v1/bangumi/subjects/{sid}/image", s.authed(s.bangumiImage))
	mux.Handle("GET /api/v1/works", s.authed(s.works))
	mux.Handle("GET /api/v1/works/{id}", s.authed(s.work))
	mux.Handle("GET /api/v1/works/{id}/image", s.authed(s.workImage))
	mux.Handle("POST /api/v1/works/{id}/refresh", s.authed(s.refreshWorkNow))
	mux.Handle("POST /api/v1/albums/{id}/works", s.authed(s.linkWork))
	mux.Handle("POST /api/v1/albums/works", s.authed(s.linkAlbums))
	mux.Handle("DELETE /api/v1/albums/{id}/works/{work}", s.authed(s.unlinkWork))
	mux.Handle("PUT /api/v1/tracks/{id}/works", s.authed(s.setTrackWorks))
	mux.Handle("PUT /api/v1/albums/{id}/subject", s.authed(s.setAlbumSubject))
	mux.Handle("DELETE /api/v1/albums/{id}/subject", s.authed(s.clearAlbumSubject))
	mux.Handle("GET /api/v1/albums/{id}/collection", s.authed(s.albumCollection))
	mux.Handle("PUT /api/v1/albums/{id}/collection", s.authed(s.setAlbumCollection))
	mux.Handle("GET /api/v1/bangumi/account", s.authed(s.bgmAccountInfo))
	mux.Handle("PUT /api/v1/bangumi/app", s.authed(s.setBgmApp))
	mux.Handle("POST /api/v1/bangumi/link", s.authed(s.beginBgmLink))
	mux.Handle("DELETE /api/v1/bangumi/link", s.authed(s.unlinkBgm))
	mux.Handle("GET /api/v1/bangumi/collections", s.authed(s.bgmCollections))
	mux.Handle("GET /api/v1/bangumi/tags", s.authed(s.bgmTags))
	mux.Handle("GET /api/v1/presence", s.authed(s.presenceInfo))
	mux.Handle("PUT /api/v1/presence/players/{pid}", s.authed(s.reportPlayer))
	mux.Handle("DELETE /api/v1/presence/players/{pid}", s.authed(s.removePlayer))
	mux.Handle("GET /api/v1/discord", s.authed(s.discordInfo))
	mux.Handle("PUT /api/v1/discord/app", s.authed(s.setDiscordApp))
	mux.Handle("POST /api/v1/discord/link", s.authed(s.beginDiscordLink))
	mux.Handle("PATCH /api/v1/discord/link", s.authed(s.changeDiscordLink))
	mux.Handle("DELETE /api/v1/discord/link", s.authed(s.unlinkDiscord))
	mux.Handle("POST /api/v1/stream/{id}/url", s.authed(s.streamURL))
	mux.Handle("POST /api/v1/stream/{id}/prefetch", s.authed(s.prefetchStream))
	mux.HandleFunc("GET /api/v1/stream/{id}", s.stream) // header token or signed URL, checked inside
	mux.HandleFunc("HEAD /api/v1/stream/{id}", s.stream)

	mux.Handle("POST /api/v1/imports", s.authed(s.createImport))
	mux.Handle("GET /api/v1/imports", s.authed(s.imports))
	mux.Handle("GET /api/v1/imports/{id}", s.authed(s.importBatch))
	mux.Handle("POST /api/v1/imports/{id}/retry", s.authed(s.retryImport))
	mux.Handle("POST /api/v1/imports/{id}/discard", s.authed(s.discardImport))
	mux.Handle("GET /api/v1/imports/{id}/preview", s.authed(s.importPreview))
	mux.Handle("POST /api/v1/imports/{id}/plan", s.authed(s.editImportPlan))
	mux.Handle("POST /api/v1/imports/{id}/start", s.authed(s.startImport))
	mux.Handle("POST /api/v1/imports/{id}/cancel", s.authed(s.cancelImport))
	mux.Handle("GET /api/v1/sidecars/{id}", s.authed(s.sidecar))

	mux.Handle("POST /api/v1/downloads", s.authed(s.createDownload))
	mux.Handle("GET /api/v1/downloads", s.authed(s.listDownloads))
	mux.Handle("GET /api/v1/downloads/{id}", s.authed(s.getDownload))
	mux.Handle("POST /api/v1/downloads/{id}/select", s.authed(s.selectFiles))
	mux.Handle("PUT /api/v1/downloads/{id}/grouping", s.authed(s.setGrouping))
	mux.Handle("GET /api/v1/downloads/{id}/collection", s.authed(s.collectionPlan))
	mux.Handle("POST /api/v1/downloads/{id}/collection", s.authed(s.makeCollection))
	mux.Handle("POST /api/v1/downloads/{id}/pause", s.authed(s.downloadAction(func(d *downloader.Service, r *http.Request, id int64) error { return d.Pause(r.Context(), id) })))
	mux.Handle("POST /api/v1/downloads/{id}/resume", s.authed(s.downloadAction(func(d *downloader.Service, r *http.Request, id int64) error { return d.Resume(r.Context(), id) })))
	mux.Handle("POST /api/v1/downloads/{id}/cancel", s.authed(s.downloadAction(func(d *downloader.Service, r *http.Request, id int64) error { return d.Cancel(r.Context(), id) })))
	mux.Handle("POST /api/v1/downloads/{id}/retry", s.authed(s.downloadAction(func(d *downloader.Service, r *http.Request, id int64) error { return d.Retry(r.Context(), id) })))
	mux.Handle("GET /api/v1/tasks", s.authed(s.tasks))
	mux.Handle("GET /api/v1/tasks/older", s.authed(s.tasksOlder))
	mux.Handle("POST /api/v1/tasks/clear", s.authed(s.clearTasks))
	mux.Handle("POST /api/v1/downloads/{id}/clear", s.authed(s.clearDownload))
	mux.Handle("POST /api/v1/imports/{id}/clear", s.authed(s.clearImport))

	mux.Handle("GET /api/v1/rss/sources", s.authed(s.rssSources))
	mux.Handle("POST /api/v1/rss/sources", s.authed(s.createRSSSource))
	mux.Handle("PATCH /api/v1/rss/sources/{id}", s.authed(s.updateRSSSource))
	mux.Handle("DELETE /api/v1/rss/sources/{id}", s.authed(s.deleteRSSSource))
	mux.Handle("POST /api/v1/rss/sources/{id}/refresh", s.authed(s.refreshRSSSource))
	mux.Handle("GET /api/v1/rss/sources/{id}/search", s.authed(s.searchRSSSource))
	mux.Handle("POST /api/v1/rss/sources/{id}/download", s.authed(s.downloadRSSLink))
	mux.Handle("GET /api/v1/rss/items", s.authed(s.rssItems))
	mux.Handle("POST /api/v1/rss/items/{id}/download", s.authed(s.downloadRSSItem))

	mux.Handle("POST /api/v1/uploads", s.authed(s.createUpload))
	mux.Handle("GET /api/v1/uploads", s.authed(s.uploadGroups))
	mux.Handle("DELETE /api/v1/uploads/groups/{group}", s.authed(s.cancelUploadGroup))
	mux.Handle("GET /api/v1/uploads/{id}", s.authed(s.getUpload))
	mux.Handle("PUT /api/v1/uploads/{id}", s.authed(s.appendUpload))
	mux.Handle("POST /api/v1/uploads/{id}/complete", s.authed(s.completeUpload))
	return s.logRequests(mux)
}

func (s *Server) diskStatus() any {
	if s.disk == nil {
		return nil
	}
	return s.disk.Status()
}

// ---- helpers ----

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, apiError{Error: err.Error()})
}

// upstreamError says a service Kanade asked (Drive, LRCLIB, MusicBrainz, a feed) failed. Never as
// 502 or 504: Cloudflare puts its own "Bad gateway" page in place of an origin's, which reads as
// Kanade being down and loses the message. retry > 0 asks the client to wait that many seconds.
func upstreamError(w http.ResponseWriter, err error, retry int) {
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
	}
	writeError(w, http.StatusServiceUnavailable, err)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

// clientIP is the address r comes from: a proxy's forwarding headers are believed only as the
// trusted_proxy setting says (review #148).
func (s *Server) clientIP(r *http.Request) string {
	return s.proxy.Of(r)
}

// sessionCookie carries the login token for the web client. It is HttpOnly, so page scripts
// cannot read it, and SameSite=Strict.
const sessionCookie = "kanade_session"

// csrfHeader must accompany cookie-authenticated requests that change state. A cross-site page
// cannot add a custom header without a CORS preflight, which this server never grants.
const csrfHeader = "X-Requested-With"

// sameOrigin guards the requests made before logging in, which csrfHeader cannot: a page of another
// site must not log the browser in (to an account of its choosing) or use up the login throttle and
// passkey requests of its address. Browsers say where a request comes from (Sec-Fetch-Site, else
// Origin); requests that say nothing, as the apps' do, pass.
var crossOrigin = http.NewCrossOriginProtection()

func sameOrigin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := crossOrigin.Check(r); err != nil {
			writeError(w, http.StatusForbidden, errors.New("requests from another site are not accepted here"))
			return
		}
		next(w, r)
	})
}

// cookieLogin refuses a login that asks for the session cookie without csrfHeader, which the web
// client always sends and a form of another site cannot.
func cookieLogin(w http.ResponseWriter, r *http.Request, cookie bool) bool {
	if cookie && r.Header.Get(csrfHeader) != "kanade" {
		writeError(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
		return false
	}
	return true
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

// sessionToken returns the Bearer token, or else the session cookie.
func sessionToken(r *http.Request) (token string, fromCookie bool) {
	if t := bearerToken(r); t != "" {
		return t, false
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value, true
	}
	return "", false
}

func (s *Server) authed(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, fromCookie := sessionToken(r)
		if fromCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "kanade" {
			writeError(w, http.StatusForbidden, errors.New("missing "+csrfHeader+" header"))
			return
		}
		uid, sid, err := s.auth.SessionOf(r.Context(), token)
		if errors.Is(err, auth.ErrNoSession) {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		if err != nil {
			s.internal(w, r, err)
			return
		}
		next(w, r.WithContext(context.WithValue(context.WithValue(r.Context(), userKey, uid), sessionKey, sid)))
	})
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, errors.New("internal error"))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests never logs query strings: they can carry OAuth codes or tokens.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if !s.proxy.Set() && clientip.Forwarded(r) {
			s.unproxied.Do(func() {
				s.log.Warn("requests come through a proxy on this machine, whose forwarding headers are not believed: every client " +
					"counts as this machine for the login throttle; say which proxy it is with `kanade config set trusted_proxy " +
					"cloudflare` (a Cloudflare Tunnel) or `loopback` (Caddy, Nginx), then restart")
			})
		}
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"ms", time.Since(t0).Milliseconds(), "ip", s.clientIP(r), "ua", r.UserAgent())
	})
}

// page serves an embedded page, naming this site where it says {{SITE}}.
func (s *Server) page(name string) http.HandlerFunc {
	body, err := pages.ReadFile(name)
	if err != nil {
		panic(err)
	}
	site := strings.TrimRight(s.cfg.PublicURL, "/")
	if u, err := url.Parse(site); err == nil && u.Host != "" {
		site = u.Host
	}
	body = bytes.ReplaceAll(body, []byte("{{SITE}}"), []byte(html.EscapeString(site)))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(body)
	}
}

// ---- accounts ----

// SetupCodePath holds the one-time code required to create the first account, so nobody who
// merely reaches the public URL can claim the server before its owner does.
func (s *Server) SetupCodePath() string { return s.cfg.Path("setup-code") }

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SetupCode string `json:"setup_code"`
		Username  string `json:"username"`
		Password  string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	code, err := os.ReadFile(s.SetupCodePath())
	if err != nil {
		writeError(w, http.StatusForbidden, errors.New("setup is not open"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(code))), []byte(req.SetupCode)) != 1 {
		writeError(w, http.StatusForbidden, errors.New("wrong setup code"))
		return
	}
	if err := s.auth.CreateUser(r.Context(), req.Username, req.Password, true); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	os.Remove(s.SetupCodePath())
	writeJSON(w, http.StatusCreated, map[string]string{"username": req.Username})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Device   string `json:"device"`
		Cookie   bool   `json:"cookie"` // web client: set an HttpOnly cookie instead of returning the token
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !cookieLogin(w, r, req.Cookie) {
		return
	}
	ctx := r.Context()
	if s.knownDevice(r) {
		ctx = auth.KnownDevice(ctx)
	}
	token, err := s.auth.Login(ctx, req.Username, req.Password, req.Device, s.clientIP(r))
	switch {
	case errors.Is(err, auth.ErrThrottledAll):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error(), "reason": "throttled_all"})
	case errors.Is(err, auth.ErrThrottled):
		writeError(w, http.StatusTooManyRequests, err)
	case errors.Is(err, auth.ErrBadCredentials):
		writeError(w, http.StatusUnauthorized, err)
	case err != nil:
		s.internal(w, r, err)
	default:
		s.loggedIn(w, token, req.Cookie)
	}
}

// loggedIn answers a successful login: the web client gets an HttpOnly cookie instead of the token,
// and one that marks the browser as known.
func (s *Server) loggedIn(w http.ResponseWriter, token string, cookie bool) {
	if !cookie {
		writeJSON(w, http.StatusOK, map[string]string{"token": token})
		return
	}
	secure := strings.HasPrefix(s.cfg.PublicURL, "https://")
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(auth.SessionLifetime / time.Second),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	now := time.Now().Unix()
	http.SetCookie(w, &http.Cookie{Name: knownCookie, Value: fmt.Sprintf("%d.%s", now, s.knownSig(now)), Path: "/api/v1/",
		MaxAge: int(knownLifetime / time.Second), HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// knownCookie marks a browser that logged in here (review #182): its logins are not held to the
// limit for everyone together, so someone guessing passwords cannot keep the owner out. It is no
// login and says nothing else; only this server can make one.
const (
	knownCookie   = "kanade_known"
	knownLifetime = 400 * 24 * time.Hour
)

func (s *Server) knownSig(issued int64) string {
	m := hmac.New(sha256.New, s.streamKey)
	fmt.Fprintf(m, "known browser %d", issued)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// knownDevice says whether the request comes from a browser that logged in here.
func (s *Server) knownDevice(r *http.Request) bool {
	c, err := r.Cookie(knownCookie)
	if err != nil {
		return false
	}
	at, sig, ok := strings.Cut(c.Value, ".")
	issued, err := strconv.ParseInt(at, 10, 64)
	return ok && err == nil && time.Since(time.Unix(issued, 0)) < knownLifetime && hmac.Equal([]byte(sig), []byte(s.knownSig(issued)))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	token, _ := sessionToken(r)
	if err := s.auth.Logout(r.Context(), token); err != nil {
		s.internal(w, r, err)
		return
	}
	s.presence.EndSessions(userID(r), 0, sessionID(r))
	s.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"), SameSite: http.SameSiteStrictMode})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ds, err := s.drive.Status(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.version,
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"drive":          ds,
		"aria2_ready":    s.aria2 != nil && s.aria2.Ready(),
		"ffmpeg":         s.importer != nil && s.importer.FFmpeg != nil, // converting and splitting (D2)
		"disk":           s.diskStatus(),
	})
}

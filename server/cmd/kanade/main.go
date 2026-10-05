// Command kanade is the Kanade music service.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // listening statistics by the listener's time zone, also where the system has none

	"github.com/HHim8826/kanade/server/internal/api"
	"github.com/HHim8826/kanade/server/internal/auth"
	"github.com/HHim8826/kanade/server/internal/backup"
	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/discord"
	"github.com/HHim8826/kanade/server/internal/diskguard"
	"github.com/HHim8826/kanade/server/internal/downloader"
	"github.com/HHim8826/kanade/server/internal/drivesync"
	"github.com/HHim8826/kanade/server/internal/ffmpeg"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/identify"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/logfile"
	"github.com/HHim8826/kanade/server/internal/loudness"
	"github.com/HHim8826/kanade/server/internal/lrclib"
	"github.com/HHim8826/kanade/server/internal/presence"
	"github.com/HHim8826/kanade/server/internal/rss"
	"github.com/HHim8826/kanade/server/internal/settings"
	"github.com/HHim8826/kanade/server/internal/staging"
	"github.com/HHim8826/kanade/server/internal/stream"
	"github.com/HHim8826/kanade/server/internal/thumbs"
	"github.com/HHim8826/kanade/server/internal/uploads"
)

// version is set at build time (-ldflags "-X main.version=v1.2.3"); releases are tagged so.
var version = "dev"

const usage = `usage: kanade [-data DIR] <command>

  serve [-listen ADDR] [-public-url URL]   run the service
  user add NAME                             create an account (password on stdin)
  user passwd NAME                          set a password (password on stdin)
  config                                    show the settings kept in the data directory
  config set KEY VALUE                      change one: listen, public_url, aria2 or trusted_proxy
                                            ("" for the default)
  backup FILE                               copy the database to FILE while the service runs
  google client FILE                        load the OAuth client JSON from Google Cloud
  google token FILE                         load an existing OAuth token
  version                                   print the version

The data directory defaults to $KANADE_DATA, then ./data. Its config.json holds the settings;
the flags of serve override them for one run. trusted_proxy says which proxy in front tells the
client's address (for the login throttle): cloudflare (a Cloudflare Tunnel on this machine),
loopback (Caddy or Nginx on this machine, by X-Forwarded-For), or the proxies' addresses; by
default none is believed.
`

func main() {
	defaultData := os.Getenv("KANADE_DATA")
	if defaultData == "" {
		defaultData = os.Getenv("SER1KA_DATA") // the name before the project became Kanade
	}
	if defaultData == "" {
		defaultData = "data"
	}
	global := flag.NewFlagSet("kanade", flag.ExitOnError)
	dataDir := global.String("data", defaultData, "data directory")
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	global.Parse(os.Args[1:])
	args := global.Args()
	if len(args) == 0 {
		global.Usage()
		os.Exit(2)
	}

	cfg := config.Config{DataDir: *dataDir, Listen: "127.0.0.1:8080", PublicURL: "http://localhost:8080",
		Aria2Path: defaultAria2()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "serve":
		err = serve(ctx, cfg, args[1:])
	case "user":
		err = userCmd(ctx, cfg, args[1:])
	case "google":
		err = googleCmd(ctx, cfg, args[1:])
	case "config":
		err = configCmd(cfg, args[1:])
	case "backup":
		err = backupCmd(ctx, cfg, args[1:])
	case "version":
		fmt.Println("kanade", version)
	default:
		global.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, cfg config.Config, args []string) error {
	if err := cfg.Apply(); err != nil { // config.json, then the flags over it
		return err
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "listen address")
	fs.StringVar(&cfg.PublicURL, "public-url", cfg.PublicURL, "public base URL (OAuth redirect)")
	cacheMiB := fs.Int64("cache-mib", 512, "stream cache budget in MiB (plan §6)")
	stagingMiB := fs.Int64("staging-mib", 2048, "download staging budget in MiB (plan §6)")
	reserveGiB := fs.Int64("reserve-gib", 4, "free space to keep on the filesystem in GiB (plan §6)")
	fs.StringVar(&cfg.Aria2Path, "aria2", cfg.Aria2Path, "path to aria2c")
	fs.StringVar(&cfg.TrustedProxy, "trusted-proxy", cfg.TrustedProxy, "whose forwarding headers tell a client's address: cloudflare, loopback, or proxy addresses")
	logPath := fs.String("log", "", `log file, rotated at 10 MB with 5 kept (default "<data>/logs/kanade.log"; "-" for stderr)`)
	fs.Parse(args)
	// Resources given as flags win for this run over the settings page's (review #74).
	pinned := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "cache-mib", "staging-mib", "reserve-gib":
			pinned[strings.ReplaceAll(f.Name, "-", "_")] = true
		}
	})

	if err := cfg.Prepare(); err != nil {
		return err
	}
	var logOut io.Writer = os.Stderr
	if *logPath != "-" {
		if *logPath == "" {
			*logPath = cfg.Path("logs", "kanade.log")
		}
		lf, err := logfile.Open(*logPath, 10<<20, 5)
		if err != nil {
			return err
		}
		defer lf.Close()
		logOut = lf
		fmt.Fprintln(os.Stderr, "logging to", *logPath) // stderr keeps only this and crashes
	}
	log := slog.New(slog.NewTextHandler(logOut, nil))
	d, err := db.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer d.Close()
	authSvc := auth.New(d)
	authSvc.Log = log
	drive := gdrive.New(d, gdrive.RedirectFor(cfg.PublicURL))
	lib := library.New(d)
	if err := lib.EnsureSearchIndex(ctx); err != nil {
		return err
	}
	if err := lib.BackfillListening(ctx); err != nil { // plays from before the listening spans (review #93)
		return err
	}
	store := &settings.Store{DB: d}
	res, err := store.Resources(ctx)
	if err != nil {
		return err
	}
	if !pinned["cache_mib"] {
		*cacheMiB = res.CacheMiB
	}
	if !pinned["staging_mib"] {
		*stagingMiB = res.StagingMiB
	}
	if !pinned["reserve_gib"] {
		*reserveGiB = res.ReserveGiB
	}
	imp := importer.New(d, lib, drive, cfg.Path(config.DirStaging), log)
	if imp.FFmpeg = ffmpeg.Find(cfg.DataDir); imp.FFmpeg != nil {
		log.Info("ffmpeg ready", "path", imp.FFmpeg.Path())
	} else {
		log.Warn("no ffmpeg: APE, TAK, WavPack, TTA, ALAC, WAV and AIFF files and CUE images will wait (set KANADE_FFMPEG)")
	}
	cache, err := stream.NewCache(drive, cfg.Path(config.DirCache), *cacheMiB<<20, log)
	if err != nil {
		return err
	}
	// Songs' loudness for the volume balance (review #136): measured as they are imported, once
	// playing has cached them whole, and the rest by a scan of the library when asked, or by itself
	// (a minute after starting, then every six hours) once the balance was turned on (review #155).
	loud := &loudness.Service{Lib: lib, DB: d, FF: imp.FFmpeg, Source: drive, Hold: cache.Hold, Temp: cfg.Path(config.DirStaging, "loudness"),
		Every: 6 * time.Hour, Log: log}
	cache.OnWhole = loud.Cached
	cache.Explain = gdrive.Explain
	imp.Measure = loud.File
	streamKey, err := db.Secret(ctx, d, "stream_signing_key", 32)
	if err != nil {
		return err
	}
	aria, err := downloader.NewAria2(cfg.Aria2Path, cfg.Path(config.DirAria2), cfg.Path(config.DirDownloads), log)
	if err != nil {
		return err
	}
	if cfg.Aria2Path == "" { // everything else works; downloads say the downloader is not running
		log.Error("aria2c not found: downloads are off. Put aria2c next to kanade, set KANADE_ARIA2 or -aria2, " +
			"install it in tools/aria2/ of the repository, or on PATH")
	}
	downloads := downloader.NewService(d, aria, imp, cfg.Path(config.DirDownloads), *stagingMiB<<20, *reserveGiB<<30, log)
	ups := uploads.New(d, cfg.Path(config.DirStaging, "uploads"), *stagingMiB<<20)
	if n, err := ups.Recover(ctx); err != nil {
		log.Warn("recovering uploads", "err", err)
	} else if n > 0 {
		log.Info("uploads completed after a restart", "count", n)
	}
	go func() { // interrupted uploads left for a week give their staging space back (review #6)
		for {
			if n, err := ups.Expire(ctx, 7*24*time.Hour); err != nil {
				log.Warn("expiring uploads", "err", err)
			} else if n > 0 {
				log.Info("abandoned uploads removed", "groups", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(6 * time.Hour):
			}
		}
	}()
	// One shared staging budget (plan §6) for downloads, client uploads and the importer's work
	// folder, with the free-space reserve; checks and reservations are atomic across them (review #4).
	budget := &staging.Budget{Limit: *stagingMiB << 20, Reserve: *reserveGiB << 30, Dir: cfg.DataDir, Free: downloader.FreeSpace}
	loud.Budget = budget
	// A copy of the database in Drive every day, the 14 latest kept (review #161).
	backups := &backup.Service{DB: d, Drive: drive, Dir: cfg.Path(config.DirStaging, "backup"), Budget: budget, Keep: 14,
		Every: 24 * time.Hour, Log: log}
	downloads.ShareBudget(budget)
	ups.ShareBudget(budget)
	budget.Use(staging.OnDisk(imp.WorkCommitted))
	imp.Budget = budget
	imp.SpaceWait = 10 * time.Minute // FFmpeg output waits for space others hold (review #46)
	imp.Refetch = downloads.Refetch  // files lost before their import are fetched again (review #57)
	imp.OnBatchDone = func(ctx context.Context, kind, source string, unsaved int) {
		if kind == "upload" && unsaved == 0 { // files not in the library stay until imported or discarded
			if err := ups.RemoveGroup(ctx, source); err != nil {
				log.Warn("clear upload staging", "group", source, "err", err)
			}
		}
	}
	mb := identify.New(strings.TrimRight(cfg.PublicURL, "/") + "/")
	mb.Log = log
	feeds := rss.New(d, downloads, strings.TrimRight(cfg.PublicURL, "/")+"/", log)
	covers := thumbs.New(cfg.Path(config.DirThumbs), api.ThumbsBudget, log)
	guard := &diskguard.Guard{Dir: cfg.DataDir, Reserve: *reserveGiB << 30, Free: downloader.FreeSpace,
		Cache: trimmed{cache, covers}, DL: downloads, UL: ups, Log: log}
	syncer := &drivesync.Syncer{DB: d, Drive: drive, Lib: lib, Log: log, Inbox: imp.ScanInbox, Forget: cache.Forget}

	// The settings page's settings (reviews #74, #75, #77): applied now, and again when saved.
	store.OnResources = func(r settings.Resources) {
		if !pinned["cache_mib"] {
			cache.SetBudget(r.CacheMiB << 20)
		}
		limit, reserve := budget.Limits()
		if !pinned["staging_mib"] {
			limit = r.StagingMiB << 20
		}
		if !pinned["reserve_gib"] {
			reserve = r.ReserveGiB << 30
		}
		budget.SetLimits(limit, reserve)
		guard.SetReserve(reserve)
		downloads.Poke() // a larger budget may let a waiting round start
	}
	dl, err := store.Downloads(ctx)
	if err != nil {
		return err
	}
	downloads.SetPolicy(ctx, dl)
	store.OnDownloads = func(p settings.Downloads) { downloads.SetPolicy(context.Background(), p) }
	dr, err := store.Drive(ctx)
	if err != nil {
		return err
	}
	applyDrive := func(p settings.Drive) {
		syncer.Configure(time.Duration(p.CheckMinutes)*time.Minute, p.AutoInbox)
		imp.SetInboxSettle(time.Duration(p.SettleMinutes) * time.Minute)
	}
	applyDrive(dr)
	store.OnDrive = applyDrive

	players := presence.NewHub() // what web players play, shown as the Discord status (review #135)
	go players.Run(ctx)
	status := discord.New(d, players, cfg.PublicURL, log)
	go status.Run(ctx)
	srv := api.New(api.Deps{Config: cfg, DB: d, Auth: authSvc, Drive: drive, Library: lib, Importer: imp,
		Cache: cache, Downloads: downloads, Aria2: aria, Uploads: ups, StreamKey: streamKey, Log: log, Version: version,
		Identify: mb, Lyrics: lrclib.New(strings.TrimRight(cfg.PublicURL, "/") + "/"), RSS: feeds, Disk: guard, Sync: syncer,
		Settings: store, Staging: budget, Pinned: pinned, Loudness: loud, Thumbs: covers, Backup: backups, Presence: players, Discord: status})
	go imp.Run(ctx)
	ariaDone := make(chan struct{})
	go func() { aria.Run(ctx); close(ariaDone) }()
	go downloads.Run(ctx)
	go feeds.Run(ctx)
	go guard.Run(ctx)
	go syncer.Run(ctx)
	go loud.Run(ctx)
	go backups.Run(ctx)

	has, err := authSvc.HasUsers(ctx)
	if err != nil {
		return err
	}
	if !has {
		if _, err := os.Stat(srv.SetupCodePath()); errors.Is(err, os.ErrNotExist) {
			raw := make([]byte, 12)
			rand.Read(raw)
			if err := os.WriteFile(srv.SetupCodePath(), []byte(hex.EncodeToString(raw)+"\n"), 0o600); err != nil {
				return err
			}
		}
		log.Warn("no account yet: create one with `user add`, or POST /api/v1/setup with the code in " + srv.SetupCodePath())
	}

	// Requests have a context of their own that stopping ends (review #166).
	reqCtx, endRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer endRequests()
	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return reqCtx }}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen, "public_url", cfg.PublicURL, "data", cfg.DataDir)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	// Short requests finish. Long ones (a song streaming, an upload) are why stopping was asked
	// for, not a failure (review #166): their context ends after a moment, then their connections.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	time.AfterFunc(2*time.Second, endRequests)
	if err := hs.Shutdown(shutdownCtx); err != nil {
		log.Info("requests still open were cut", "err", err)
		hs.Close()
	}
	// Wait for aria2 to save its session and exit; an orphan would linger as a zombie
	// on hosts whose init does not reap (this container's PID 1 does not).
	select {
	case <-ariaDone:
	case <-time.After(15 * time.Second):
		log.Warn("aria2 did not stop in time")
	}
	return nil
}

// trimmed is what the disk guard empties when space runs low: the stream cache, then the covers
// made for the web client (review #157).
type trimmed struct {
	*stream.Cache
	covers *thumbs.Store
}

func (t trimmed) Trim() int64 { return t.Cache.Trim() + t.covers.Trim() }

func readPassword() (string, error) {
	fmt.Fprint(os.Stderr, "password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no password on stdin")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func userCmd(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 2 || (args[0] != "add" && args[0] != "passwd") {
		return errors.New("usage: user add|passwd NAME")
	}
	if err := cfg.Prepare(); err != nil {
		return err
	}
	d, err := db.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer d.Close()
	pw, err := readPassword()
	if err != nil {
		return err
	}
	a := auth.New(d)
	if args[0] == "add" {
		if err := a.CreateUser(ctx, args[1], pw, false); err != nil {
			return err
		}
		os.Remove(cfg.Path("setup-code")) // setup is no longer needed
		fmt.Println("created", args[1])
		return nil
	}
	if err := a.SetPassword(ctx, args[1], pw); err != nil {
		return err
	}
	fmt.Println("password updated; existing logins were ended")
	return nil
}

// configCmd shows or changes the settings in the data directory's config.json.
func configCmd(cfg config.Config, args []string) error {
	switch {
	case len(args) == 0:
		if err := cfg.Apply(); err != nil {
			return err
		}
		out, _ := json.MarshalIndent(map[string]string{"data": cfg.DataDir, "listen": cfg.Listen, "public_url": cfg.PublicURL,
			"aria2": cfg.Aria2Path, "trusted_proxy": cfg.TrustedProxy}, "", "  ")
		fmt.Println(string(out))
		return nil
	case len(args) == 3 && args[0] == "set":
		if err := cfg.Set(args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("%s set; restart the service to use it\n", args[1])
		return nil
	}
	return errors.New("usage: config [set KEY VALUE]")
}

// backupCmd copies the database, consistent while the service runs (VACUUM INTO). The Google
// account, the OAuth client and the signing keys are in it; the music is in Drive.
func backupCmd(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: backup FILE")
	}
	dst, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s exists", dst)
	}
	if _, err := os.Stat(cfg.DBPath()); err != nil {
		return fmt.Errorf("no database in %s: %w", cfg.DataDir, err)
	}
	d, err := db.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := d.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return err
	}
	fmt.Println("database copied to", dst)
	return nil
}

func googleCmd(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 2 || (args[0] != "client" && args[0] != "token") {
		return errors.New("usage: google client|token FILE")
	}
	raw, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	if err := cfg.Prepare(); err != nil {
		return err
	}
	d, err := db.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer d.Close()
	drive := gdrive.New(d, "")
	if args[0] == "client" {
		if err := drive.SetClientConfig(ctx, raw); err != nil {
			return err
		}
		fmt.Println("OAuth client stored")
		return nil
	}
	var tok gdrive.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return err
	}
	if err := drive.ImportToken(ctx, tok); err != nil {
		return err
	}
	fmt.Println("token stored")
	return nil
}

// defaultAria2 looks for aria2c (review #10): next to this binary, then $KANADE_ARIA2, then
// tools/aria2/aria2c in the repository the binary or the working directory is in (searching up a
// few levels), then on PATH. "" when there is none: the server runs without downloads.
func defaultAria2() string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), "aria2c"); isExecutable(p) {
			return p
		}
		dirs = append(dirs, filepath.Dir(exe))
	}
	if p := os.Getenv("KANADE_ARIA2"); p != "" {
		return p
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, d := range dirs {
		for i := 0; i < 4; i++ {
			if p := filepath.Join(d, "tools", "aria2", "aria2c"); isExecutable(p) {
				return p
			}
			d = filepath.Dir(d)
		}
	}
	if p, err := exec.LookPath("aria2c"); err == nil {
		return p
	}
	return ""
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/HHim8826/kanade/server/internal/api"
	"github.com/HHim8826/kanade/server/internal/auth"
	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/diskguard"
	"github.com/HHim8826/kanade/server/internal/downloader"
	"github.com/HHim8826/kanade/server/internal/drivesync"
	"github.com/HHim8826/kanade/server/internal/ffmpeg"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/identify"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/logfile"
	"github.com/HHim8826/kanade/server/internal/lrclib"
	"github.com/HHim8826/kanade/server/internal/rss"
	"github.com/HHim8826/kanade/server/internal/staging"
	"github.com/HHim8826/kanade/server/internal/stream"
	"github.com/HHim8826/kanade/server/internal/uploads"
)

const usage = `usage: kanade [-data DIR] <command>

  serve [-listen ADDR] [-public-url URL]   run the service
  user add NAME                             create an account (password on stdin)
  user passwd NAME                          set a password (password on stdin)
  google client FILE                        load the OAuth client JSON from Google Cloud
  google token FILE                         load an existing token (from the P0 spike)

The data directory defaults to $KANADE_DATA, then ./data.
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

	cfg := config.Config{DataDir: *dataDir, Listen: "127.0.0.1:8080", PublicURL: "https://music.ser1ka.com",
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
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "listen address")
	fs.StringVar(&cfg.PublicURL, "public-url", cfg.PublicURL, "public base URL (OAuth redirect)")
	cacheMiB := fs.Int64("cache-mib", 512, "stream cache budget in MiB (plan §6)")
	stagingMiB := fs.Int64("staging-mib", 2048, "download staging budget in MiB (plan §6)")
	reserveGiB := fs.Int64("reserve-gib", 4, "free space to keep on the filesystem in GiB (plan §6)")
	fs.StringVar(&cfg.Aria2Path, "aria2", cfg.Aria2Path, "path to aria2c")
	logPath := fs.String("log", "", `log file, rotated at 10 MB with 5 kept (default "<data>/logs/kanade.log"; "-" for stderr)`)
	fs.Parse(args)

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
	drive := gdrive.New(d, strings.TrimRight(cfg.PublicURL, "/")+"/oauth/google/callback")
	lib := library.New(d)
	if err := lib.EnsureSearchIndex(ctx); err != nil {
		return err
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
	guard := &diskguard.Guard{Dir: cfg.DataDir, Reserve: *reserveGiB << 30, Free: downloader.FreeSpace, Cache: cache,
		DL: downloads, UL: ups, Log: log}
	syncer := &drivesync.Syncer{DB: d, Drive: drive, Lib: lib, Log: log, Inbox: imp.ScanInbox, Forget: cache.Forget}
	srv := api.New(api.Deps{Config: cfg, DB: d, Auth: authSvc, Drive: drive, Library: lib, Importer: imp,
		Cache: cache, Downloads: downloads, Aria2: aria, Uploads: ups, StreamKey: streamKey, Log: log,
		Identify: mb, Lyrics: lrclib.New(strings.TrimRight(cfg.PublicURL, "/") + "/"), RSS: feeds, Disk: guard, Sync: syncer})
	go imp.Run(ctx)
	ariaDone := make(chan struct{})
	go func() { aria.Run(ctx); close(ariaDone) }()
	go downloads.Run(ctx)
	go feeds.Run(ctx)
	go guard.Run(ctx)
	go syncer.Run(ctx)

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

	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen, "public_url", cfg.PublicURL, "data", cfg.DataDir)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = hs.Shutdown(shutdownCtx)
	// Wait for aria2 to save its session and exit; an orphan would linger as a zombie
	// on hosts whose init does not reap (this container's PID 1 does not).
	select {
	case <-ariaDone:
	case <-time.After(15 * time.Second):
		log.Warn("aria2 did not stop in time")
	}
	return err
}

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

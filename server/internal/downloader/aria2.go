// Package downloader runs the bundled aria2 and manages BitTorrent downloads (decision D5).
package downloader

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/HHim8826/kanade/server/internal/proc"
)

// RPC is a minimal aria2 JSON-RPC client.
type RPC struct {
	url    string
	secret string
	http   *http.Client
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("aria2 error %d: %s", e.Code, e.Message) }

// IsNotFound reports aria2's "GID ... is not found".
func IsNotFound(err error) bool {
	var e *rpcError
	return errors.As(err, &e) && strings.Contains(e.Message, "not found")
}

func (c *RPC) Call(ctx context.Context, method string, out any, params ...any) error {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "1", "method": "aria2." + method,
		"params": append([]any{"token:" + c.secret}, params...),
	})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&r); err != nil {
		return fmt.Errorf("aria2 %s: %w", method, err)
	}
	if r.Error != nil {
		return r.Error
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

// Status is the subset of aria2.tellStatus we use; aria2 reports numbers as strings.
type Status struct {
	GID             string   `json:"gid"`
	Status          string   `json:"status"` // active waiting paused error complete removed
	TotalLength     string   `json:"totalLength"`
	CompletedLength string   `json:"completedLength"`
	UploadLength    string   `json:"uploadLength"`
	DownloadSpeed   string   `json:"downloadSpeed"`
	UploadSpeed     string   `json:"uploadSpeed"`
	Connections     string   `json:"connections"`
	Seeder          string   `json:"seeder"`
	InfoHash        string   `json:"infoHash"`
	FollowedBy      []string `json:"followedBy"`
	ErrorMessage    string   `json:"errorMessage"`
	Bittorrent      struct {
		Info struct {
			Name string `json:"name"`
		} `json:"info"`
	} `json:"bittorrent"`
}

func num(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

type File struct {
	Index    string `json:"index"`
	Path     string `json:"path"`
	Length   string `json:"length"`
	Selected string `json:"selected"`
}

// Aria2 supervises the aria2c process: it writes the config, starts it, and restarts it with
// backoff if it exits. aria2 also exits by itself if this process dies (--stop-with-process).
type Aria2 struct {
	bin, dir, downloads string
	log                 *slog.Logger
	RPC                 *RPC
	ready               atomic.Bool
}

// NewAria2 prepares aria2 at bin. With bin "" there is none: Run returns at once and the
// downloader never becomes ready (downloads are off; the rest of the server works).
func NewAria2(bin, dataDir, downloadDir string, log *slog.Logger) (*Aria2, error) {
	if bin != "" {
		if _, err := os.Stat(bin); err != nil {
			return nil, fmt.Errorf("aria2c not found at %s", bin)
		}
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	return &Aria2{bin: bin, dir: dataDir, downloads: downloadDir, log: log,
		RPC: &RPC{secret: hex.EncodeToString(raw), http: &http.Client{}}}, nil
}

func (a *Aria2) Ready() bool { return a.ready.Load() }

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// writeConfig keeps the RPC secret out of the process list by putting it in a 0600 file.
func (a *Aria2) writeConfig(port int) (string, error) {
	session := filepath.Join(a.dir, "session")
	if f, err := os.OpenFile(session, os.O_CREATE|os.O_RDONLY, 0o600); err != nil {
		return "", err
	} else {
		f.Close()
	}
	conf := strings.Join([]string{
		"enable-rpc=true",
		"rpc-listen-all=false",
		"rpc-listen-port=" + strconv.Itoa(port),
		"rpc-secret=" + a.RPC.secret,
		"dir=" + a.downloads,
		"input-file=" + session,
		"save-session=" + session,
		"save-session-interval=30",
		"auto-save-interval=60",
		// The service keeps only one task downloading at a time (plan §6); aria2 needs more slots
		// so that seeding tasks and metadata fetches do not block a new download.
		"max-concurrent-downloads=5",
		// BitTorrent tasks are always added from a .torrent (addTorrent, paused) so that the
		// session file stores the torrent itself and the task keeps its GID and file selection
		// across restarts. Never let aria2 start downloading from a link on its own.
		"follow-torrent=false",
		"rpc-save-upload-metadata=true",
		"bt-max-peers=30", // D5
		"seed-ratio=1.0",  // D5: ratio 1.0 or 72 hours, whichever first
		"seed-time=4320",
		"bt-remove-unselected-file=true",
		"file-allocation=none",
		// One unresponsive web seed mirror stalled a real download for 5 minutes with the
		// defaults (60 s × 5 tries); fail over faster.
		"connect-timeout=15",
		"timeout=30",
		"continue=true",
		"enable-dht=true",
		"dht-file-path=" + filepath.Join(a.dir, "dht.dat"),
		"dht-file-path6=" + filepath.Join(a.dir, "dht6.dat"),
		"bt-enable-lpd=false",
		"log=" + filepath.Join(a.dir, "aria2.log"),
		"log-level=notice",
		"console-log-level=warn",
		"summary-interval=0",
		"",
	}, "\n")
	path := filepath.Join(a.dir, "aria2.conf")
	return path, os.WriteFile(path, []byte(conf), 0o600)
}

// Run keeps aria2 running until ctx ends.
func (a *Aria2) Run(ctx context.Context) {
	if a.bin == "" {
		return
	}
	for attempt := 0; ctx.Err() == nil; attempt++ {
		started := time.Now()
		err := a.runOnce(ctx)
		a.ready.Store(false)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			attempt = 0
		}
		wait := min(time.Duration(1<<min(attempt, 6))*time.Second, time.Minute)
		a.log.Error("aria2 exited; restarting", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (a *Aria2) runOnce(ctx context.Context) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	conf, err := a.writeConfig(port)
	if err != nil {
		return err
	}
	a.RPC.url = fmt.Sprintf("http://127.0.0.1:%d/jsonrpc", port)
	cmd := exec.Command(a.bin, "--conf-path="+conf, "--stop-with-process="+strconv.Itoa(os.Getpid()))
	proc.DieWithParent(cmd, syscall.SIGTERM) // also when the server is killed outright: aria2 saves its session and exits
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	for i := 0; i < 50; i++ { // wait up to 5 s for RPC
		var v struct{ Version string }
		if err := a.RPC.Call(ctx, "getVersion", &v); err == nil {
			a.ready.Store(true)
			a.log.Info("aria2 ready", "version", v.Version, "pid", cmd.Process.Pid)
			break
		}
		select {
		case err := <-exited:
			return fmt.Errorf("aria2 failed to start: %v %s", err, strings.TrimSpace(stderr.String()))
		case <-time.After(100 * time.Millisecond):
		}
	}
	select {
	case err := <-exited:
		return fmt.Errorf("%v %s", err, strings.TrimSpace(stderr.String()))
	case <-ctx.Done():
		// Save the session so paused and seeding tasks come back after a restart.
		a.RPC.Call(context.Background(), "saveSession", nil)
		a.RPC.Call(context.Background(), "shutdown", nil)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
		}
		return ctx.Err()
	}
}

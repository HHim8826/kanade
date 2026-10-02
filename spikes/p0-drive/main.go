// Command p0drive is the P0 spike for Google Drive access (docs/p0-checklist.md §1).
// It is throwaway code: findings go back into the checklist, not into the product.
//
// Every write stays inside the test folder "ser1ka-music-p0" in the authorized Drive.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	projectDir     = "/data/music-platform"
	clientFile     = projectDir + "/secrets/google-oauth-client.json"
	tokenFile      = projectDir + "/secrets/google-token.json"
	stateFile      = projectDir + "/secrets/p0-drive-state.json"
	testFolderName = "ser1ka-music-p0"
)

// state is what the spike remembers between runs.
type state struct {
	FolderID         string         `json:"folder_id,omitempty"`
	PendingAuthState string         `json:"pending_auth_state,omitempty"`
	PageToken        string         `json:"page_token,omitempty"`
	Upload           *uploadSession `json:"upload,omitempty"`
}

const usage = `usage: p0drive <command> [args]

  auth                      authorize via https://music.ser1ka.com and save the token
  about                     account and storage quota
  init                      create or find the test folder
  mkdir NAME                create a subfolder inside the test folder
  ls [FOLDER_ID]            list the test folder (or a subfolder)
  genfile PATH SIZE_MB      write a random test file
  upload [-chunk MiB] [-stop-after N] PATH
                            resumable upload into the test folder
  resume [-stop-after N]    continue the last interrupted upload
  verify FILE_ID [SHA256]   poll sha256Checksum and compare
  range FILE_ID             time Range reads at several offsets
  changes                   list changes since the saved page token
  move FILE_ID FOLDER_ID    server-side move inside the test folder
  head FILE_ID              read audio metadata through Range requests only
  download FILE_ID          full download speed from Drive
  serve [-listen ADDR] [-cache-mib N]
                            streaming proxy: /stream/{id}?mode=direct|cache&k=KEY
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx := context.Background()
	cmd, args := os.Args[1], os.Args[2:]
	commands := map[string]func(context.Context, []string) error{
		"auth": cmdAuth, "about": cmdAbout, "init": cmdInit, "mkdir": cmdMkdir, "ls": cmdList,
		"genfile": cmdGenfile, "upload": cmdUpload, "resume": cmdResume, "verify": cmdVerify,
		"range": cmdRange, "changes": cmdChanges, "move": cmdMove, "head": cmdHead,
		"download": cmdDownload, "serve": cmdServe,
	}
	run, ok := commands[cmd]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	err := run(ctx, args)
	reportPeakRSS()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// saveJSON writes atomically with 0600: these files hold tokens and upload session URIs.
func saveJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadState() (*state, error) {
	var st state
	if err := loadJSON(stateFile, &st); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return &st, nil
}

func (st *state) save() error { return saveJSON(stateFile, st) }

func (st *state) requireFolder() error {
	if st.FolderID == "" {
		return fmt.Errorf("no test folder yet; run `p0drive init` first")
	}
	return nil
}

// reportPeakRSS prints VmHWM, the checklist's measure for "RAM does not grow with file size".
func reportPeakRSS() {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "VmHWM:") {
			fmt.Fprintf(os.Stderr, "[%s] peak RSS %s\n", filepath.Base(os.Args[0]), strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:")))
		}
	}
}

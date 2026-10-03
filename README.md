# Kanade（奏）

A private, self-hosted music library: find releases through BitTorrent, choose the files, download, tidy, store them in your own Google Drive, and play them from a browser (an Android app comes later).

## Layout

| Path | What it is |
|---|---|
| `server/` | The Go service (`kanade`): API, importer, Drive storage, aria2 downloads, streaming cache, embedded web client |
| `server/internal/web/static/` | Browser client (Preact + htm, no build step), served at `/app/` |
| `server/scripts/upload-folder.py` | Command-line uploader, and the reference for how clients upload |
| `docs/` | Plan, decisions (D1–D8, these override the plan), backend design, P0 validation results, library survey |
| `spikes/p0-drive/` | Throwaway P0 experiments for Drive access and streaming; results are in `docs/p0-checklist.md` |

## Build and run

Requires Go 1.27+. Pure Go (no cgo), so it cross-compiles for linux/amd64 and linux/arm64.

```bash
cd server
go test ./...
go build -o bin/kanade ./cmd/kanade

export KANADE_DATA=/path/to/data          # database, staging, cache; created with mode 0700
./bin/kanade google client client.json    # OAuth client JSON from Google Cloud ("Web application")
printf '%s\n' 'a long password' | ./bin/kanade user add admin
./bin/kanade serve -listen 127.0.0.1:8080 -public-url https://music.example.com
```

`serve` looks for `aria2c` next to the binary, at `$KANADE_ARIA2` (or `-aria2`), in `tools/aria2/` of the checkout the binary or the working directory is in, and on `PATH`; without it the server runs with downloads off. FFmpeg (`$KANADE_FFMPEG`, `tools/ffmpeg/bin/` beside the data directory, or `PATH`, with `ffprobe` next to it) converts lossless formats to FLAC and splits disc images by their CUE sheets; without it those files are skipped. Connect Google Drive from the web client's settings page.

## Browser checks

The loading regressions serve the real browser client with controlled API responses; no running Kanade server or Google account is needed. With Node.js, install Playwright and its browser in a separate test directory, then run from the checkout root:

```bash
npm install --prefix /tmp/kanade-ui-tests playwright
/tmp/kanade-ui-tests/node_modules/.bin/playwright install chromium
NODE_PATH=/tmp/kanade-ui-tests/node_modules node server/scripts/test-ui-loading.cjs
```

Set `CHROMIUM_PATH` to use an installed Chromium. An optional argument filters test names, such as `node server/scripts/test-ui-loading.cjs settings`. `KANADE_UI_ROOT` selects another `static/` tree for checking a previous version.

## Design notes

Start with `docs/decisions.md` and `docs/backend-design.md`. In short: Drive is accessed directly through its REST API (D1); every file is verified by SHA-256 after upload; playback downloads the whole track into a local cache in the background, because each Drive request costs about 0.6 s (D7); everything is sized for a 1.5 GB RAM VPS.

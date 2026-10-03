# Kanade（奏）

A private, self-hosted music library: find releases through BitTorrent, choose the files, download, tidy, store them in your own Google Drive, and play them from a browser (an Android app comes later).

## Install

On a Linux server (amd64 or arm64), as root:

```bash
curl -fsSL https://raw.githubusercontent.com/HHim8826/kanade/main/scripts/kanade.sh -o kanade.sh && sudo bash kanade.sh
```

Where GitHub is slow (mainland China), put a proxy in front of the address, for example `https://ghproxy.net/https://raw.githubusercontent.com/…`; the script asks for the same proxy for its downloads.

The script installs the release for the machine into `/opt/kanade` (data in `/opt/kanade/data`), runs it as a `kanade` system account under systemd or OpenRC (in the background where there is neither, as in a container), installs aria2 and FFmpeg from the system's packages if you agree, creates an administrator with a random password and prints it. Afterwards `sudo kanade-manager` opens the same menu:

| Command | |
|---|---|
| `kanade-manager install` / `update` / `uninstall` | Update checks the download against `SHA256SUMS`, backs up the database first, and goes back to the old version if the new one does not start. Uninstall keeps the data unless you type `DELETE`. |
| `kanade-manager status` / `start` / `stop` / `restart` / `log [-f]` | |
| `kanade-manager password` | Reset a password (random or your own); every login of that account ends. |
| `kanade-manager config` | Change the public address and the listen address. |
| `kanade-manager backup` / `restore` | Backups go to `data/backups/`; a restore saves the current database first. |

Without questions (for automation): `sudo env KANADE_YES=1 KANADE_PUBLIC_URL=https://music.example.com bash kanade.sh install`; the header of `scripts/kanade.sh` lists the other settings.

**Google Drive and passkeys need an HTTPS domain.** Google accepts an OAuth redirect only to https (or localhost), so put Kanade behind Cloudflare Tunnel, Caddy or Nginx with a domain, set it as the public address, and in Google Cloud create an OAuth client ("Web application") whose redirect URI is `https://<your domain>/oauth/google/callback`. Then connect Drive from the web client's settings page.

## Layout

| Path | What it is |
|---|---|
| `server/` | The Go service (`kanade`): API, importer, Drive storage, aria2 downloads, streaming cache, embedded web client |
| `server/internal/web/static/` | Browser client (Preact + htm, no build step), served at `/app/` |
| `server/scripts/upload-folder.py` | Command-line uploader, and the reference for how clients upload |
| `scripts/kanade.sh` | Install and manage script |
| `.github/workflows/ci.yml` | CI for pushes to main and pull requests: gofmt, vet, tests with the race detector (with aria2 and FFmpeg), the web client check (`server/scripts/check-web.mjs`), shellcheck and actionlint |
| `.github/workflows/release.yml` | Runs CI, then builds and publishes a release for each version tag |
| `docs/` | Plan, decisions (D1–D10, these override the plan), backend design, P0 validation results, library survey |

## Build and run

Requires Go 1.27+. Pure Go (no cgo), so it cross-compiles for linux/amd64 and linux/arm64.

```bash
cd server
go test ./...
go build -o bin/kanade ./cmd/kanade

export KANADE_DATA=/path/to/data          # database, settings, staging, cache; created with mode 0700
./bin/kanade config set public_url https://music.example.com
./bin/kanade config set listen 127.0.0.1:8080
printf '%s\n' 'a long password' | ./bin/kanade user add admin
./bin/kanade serve
```

Settings live in the data directory's `config.json` (`kanade config` shows them); the flags of `serve` (`-listen`, `-public-url`, `-aria2`) override them for one run. `kanade backup FILE` copies the database while the service runs.

`serve` looks for `aria2c` next to the binary, at `$KANADE_ARIA2` (or the `aria2` setting), in `tools/aria2/` of the checkout the binary or the working directory is in, and on `PATH`; without it the server runs with downloads off. FFmpeg (`$KANADE_FFMPEG`, `tools/ffmpeg/bin/` beside the data directory, or `PATH`, with `ffprobe` next to it) converts lossless formats to FLAC and splits disc images by their CUE sheets; without it those files wait.

## Releases

Push a version tag and GitHub Actions runs CI, then builds and publishes the release the install script downloads (`kanade-linux-amd64.tar.gz`, `kanade-linux-arm64.tar.gz`, `SHA256SUMS`, `VERSION`, `kanade.sh`):

```bash
git tag v0.1.0 && git push origin v0.1.0
```

A tag with a hyphen (`v0.2.0-rc1`) is published as a pre-release, which `latest` skips. Running the workflow by hand builds the same files as an artifact without releasing them.

## License

[AGPL-3.0](LICENSE). If you run a modified Kanade for other people, offer them its source. The bundled Preact and htm keep their own licenses (`server/internal/web/static/vendor/`).

## Design notes

Start with `docs/decisions.md` and `docs/backend-design.md`. In short: Drive is accessed directly through its REST API (D1); every file is verified by SHA-256 after upload; playback downloads the whole track into a local cache in the background, because each Drive request costs about 0.6 s (D7); everything is sized for a 1.5 GB RAM VPS.

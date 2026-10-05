[English](README.md) | **繁體中文**

# Kanade（奏）

私人、自架的音樂庫：透過 BitTorrent 找到資源、選好檔案、下載、整理，存進你自己的 Google Drive，再從電腦或手機的瀏覽器播放（Android App 之後才做）。介面是繁體中文。

![首頁：繼續播放、最近播放、未聽完的廣播劇](docs/images/home.webp)

## 功能

- **取得音樂**：透過 aria2 下載磁力連結、`.torrent` 檔與 RSS 訂閱（可設規則、自動下載），也可以直接下載音檔或 zip 的 HTTP/HTTPS 連結。種子的檔案由你挑選；下載比伺服器的暫存空間大時分批進行，每批匯入並清掉後才下載下一批。做種依分享率或時間停止。
- **匯入與整理**：讀取標籤（並判斷文字編碼），依 CUE 切割整軌映像、把其他無損格式轉成 FLAC（FFmpeg），CUE 與 log 檔跟著專輯保存，標籤沒寫專輯時以資料夾為一張專輯；整份下載也可以變成一張專輯，每個資料夾一個分區。另外有 MusicBrainz 查詢、貼上 VGMdb 頁面讀取資料、單張或多張專輯一起編輯、分類、關聯專輯所屬的作品（動畫、遊戲、書籍，以 [Bangumi](https://bgm.tv) 的資料為準，並標註每首歌的用途：片頭曲、片尾曲、插曲…）、專輯自己的 Bangumi 條目（連結 Bangumi 帳號後，可以在專輯頁管理你的收藏：狀態、評分、標籤、吐槽），以及每一筆修改都能撤回的修改紀錄。
- **儲存**：每個檔案都存進你自己的 Google Drive，上傳後以 SHA-256 核對；丟進 Drive 收件匣資料夾的檔案會自動匯入。伺服器只保留播放快取，小 VPS 就夠用。
- **播放**：播放佇列、隨機、重複、全曲庫隨機或自動接續播放、廣播劇從上次停下的地方繼續、同步歌詞（來自檔案、LRC，或從 LRCLIB 找）、書籤、睡眠定時、鎖定畫面控制，以及把正在聽的歌顯示成你的 Discord 狀態（「正在聽 Kanade」）：連結 Discord 帳號後由伺服器回報，電腦和手機都不用另外安裝程式。
- **收藏**：我的最愛、歌單、依規則挑歌的智慧歌單（分類、歌手、播放次數、上次播放…）。
- **我的聆聽**：每天實際聽了多久的年度熱圖、任意期間的排行與趨勢，可匯出 CSV 或 JSON。
- **登入**：密碼或 passkey；單一個 Go 執行檔，以 1.5 GB 記憶體的 VPS 為準設計。

| | |
|---|---|
| ![專輯頁：音質、分類、作品與 Bangumi 收藏](docs/images/album.webp) | ![正在播放與同步歌詞（深色主題）](docs/images/now-playing.webp) |
| ![我的聆聽：熱圖與期間統計](docs/images/stats.webp) | ![智慧歌單與預覽](docs/images/smart-playlist.webp) |
| ![任務：下載、做種與匯入](docs/images/tasks.webp) | ![選擇要下載的檔案](docs/images/downloads.webp) |

![手機：首頁、正在播放、專輯（玫瑰色深色主題）](docs/images/phone.webp)

截圖用的是虛構的示範曲庫：專輯、歌手、歌詞、封面和作品都是為了截圖編出來的。

## 安裝

在 Linux 伺服器（amd64 或 arm64）上以 root 執行：

```bash
curl -fsSL https://raw.githubusercontent.com/HHim8826/kanade/main/scripts/kanade.sh -o kanade.sh && sudo bash kanade.sh
```

連 GitHub 很慢的地方（中國大陸），在網址前面加上代理，例如 `https://ghproxy.net/https://raw.githubusercontent.com/…`；腳本下載其他檔案時會詢問要不要用同一個代理。

腳本會：

- 把適合這台機器的版本裝到 `/opt/kanade`（資料在 `/opt/kanade/data`）；
- 以系統帳號 `kanade` 在 systemd、OpenRC 或 SysV init 下執行，並設定開機啟動。這些都沒有時（例如容器裡），就在背景執行，有 cron 時用 `@reboot` 開機啟動；
- 經你同意後從系統套件安裝 aria2 與 FFmpeg。系統套件裡都沒有時（RHEL 系列、Amazon Linux），改用 Kanade 開發時用的靜態版本，放在 `/opt/kanade/tools`；FFmpeg 約 150 MB；
- 建立一個隨機密碼的管理員帳號並顯示密碼。

之後執行 `sudo kanade-manager` 會開啟同一個選單：

| 指令 | |
|---|---|
| `kanade-manager install` / `update` / `uninstall` | 更新時會以 `SHA256SUMS` 核對下載的檔案、先備份資料庫，新版啟動不了就退回舊版；即使 Kanade 已是最新，也會把 `kanade-manager` 本身換成該版的腳本（同樣核對）。解除安裝會保留資料，除非你輸入 `DELETE`。 |
| `kanade-manager status` / `start` / `stop` / `restart` / `log [-f]` | status 也會顯示是否開機啟動。 |
| `kanade-manager autostart [on\|off]` | 是否開機啟動（依機器使用 systemd、OpenRC、SysV 或 cron）；不加參數時顯示目前設定並詢問。 |
| `kanade-manager password` | 重設密碼（隨機或自訂），該帳號的所有登入都會結束。 |
| `kanade-manager config` | 修改公開網址、監聽位址，以及前面的代理（Cloudflare，或同一台機器上的 Nginx、Caddy）：只相信這裡指定的代理所轉送的用戶端位址，登入節流依這個位址計算。 |
| `kanade-manager backup` / `restore` | 備份放在 `data/backups/`；還原前會先備份目前的資料庫。連上 Drive 後，伺服器每天也會把資料庫複製到 Drive 的 `Kanade/backups`（保留最近 14 份）；要還原其中一份，下載到 `data/backups/` 再執行 `restore`。更新前自動做的備份保留最近 5 份。 |
| `kanade-manager tools` | 之後再安裝 aria2 與 FFmpeg（安裝時跳過，或系統套件裡沒有時）。 |

不詢問的安裝（自動化用）：`sudo env KANADE_YES=1 KANADE_PUBLIC_URL=https://music.example.com KANADE_TRUSTED_PROXY=cloudflare bash kanade.sh install`；其他設定（例如 `KANADE_INIT=systemd|openrc|sysv|none`）列在 `scripts/kanade.sh` 的開頭。

**連線 Google Drive。** Kanade 用你自己在 Google Cloud 建立的 OAuth 用戶端存取 Drive；設定頁會一步步帶你建立，並顯示要登記的重新導向 URI。Google 只會把瀏覽器導回 https 網域（或 localhost）：網站在有 https 的網域後面（Cloudflare Tunnel、Caddy、Nginx）時會自動回到 Kanade；用 IP 位址連線時，Kanade 會登記 `http://localhost/oauth/google/callback`，你再把瀏覽器最後停住的網址貼回來。passkey 一定要 https，用純 http 時密碼也是明文傳送。

**登入。** 同一個位址 15 分鐘內最多輸錯 5 次密碼（IPv6 整個 /64 算一個位址），所有位址合計最多 30 次。達到合計上限表示有人在猜密碼：在上限解除前，只有曾經在這裡登入過的瀏覽器能用密碼登入；passkey 照常可用，已登入的頁面也仍可改密碼或新增 passkey。位址依 `kanade-manager config` 設定的代理判斷。

**Discord 狀態。** 在 [Discord Developer Portal](https://discord.com/developers/applications) 建立應用程式，名稱填 Kanade（狀態會顯示「正在聽 Kanade」），為它開啟 Discord Social SDK（填 Getting Started 表單即可，不用下載任何東西），並在 OAuth2 → Redirects 加入 `https://<你的網址>/oauth/discord/callback`。把它的 Client ID 與 Client Secret 填到 Kanade 的設定頁，再連結你的 Discord 帳號。播放時伺服器會更新你的狀態：歌名，以及你選擇顯示的歌手、專輯、進度和專輯封面。封面是專輯綁定的 Bangumi 條目圖片；選擇的話，也可以用 Kanade 自己的封面，透過一個專為 Discord 產生的公開網址提供。暫停最多顯示 10 分鐘，沒有播放時 Kanade 不會連上 Discord。Discord 只為 Social SDK 公開說明這個狀態權限，所以這個做法之後可能因 Discord 改動而失效。

**Bangumi。** 在 [bgm.tv/dev/app](https://bgm.tv/dev/app) 建立應用程式，回調地址填 `https://<你的網址>/oauth/bangumi/callback`，把 App ID 與 App Secret 填到設定頁，再連結你的 Bangumi 帳號。只有在專輯的收藏對話框按下儲存，或綁定條目時勾選「綁定時加入我的 Bangumi 收藏」，Kanade 才會改動你的 Bangumi 收藏；播放和瀏覽都不會。

每個服務的回調網址都不同（`/oauth/google/…`、`/oauth/discord/…`、`/oauth/bangumi/…`），設定頁會分別顯示，可以直接複製。

## 目錄

| 路徑 | 內容 |
|---|---|
| `server/` | Go 服務（`kanade`）：API、匯入、Drive 儲存、aria2 下載、串流快取、聆聽統計、內嵌的網頁端 |
| `server/internal/web/static/` | 瀏覽器端（Preact + htm，不需要建置），網址在 `/app/` |
| `server/scripts/upload-folder.py` | 命令列上傳工具，也是用戶端上傳方式的參考 |
| `scripts/kanade.sh` | 安裝與管理腳本（`kanade-manager`） |
| `.github/workflows/ci.yml` | 推送到 main 與 pull request 的 CI：gofmt、vet、開 race detector 的測試（含 aria2 與 FFmpeg）、網頁端檢查（`server/scripts/check-web.mjs`）、shellcheck 與 actionlint |
| `.github/workflows/release.yml` | 每個版本標籤先跑 CI，再建置並發佈 release |
| `docs/` | 計畫、決策（D1–D11，優先於計畫）、含 API 與實作紀錄的後端設計、P0 驗證結果、曲庫調查；`docs/images/` 是上面的截圖 |

## 建置與執行

需要 Go 1.27 以上。純 Go（不用 cgo），可以交叉編譯成 linux/amd64 與 linux/arm64。

```bash
cd server
go test ./...
go build -o bin/kanade ./cmd/kanade

export KANADE_DATA=/path/to/data          # 資料庫、設定、暫存、快取；以 0700 權限建立
./bin/kanade config set public_url https://music.example.com
./bin/kanade config set listen 127.0.0.1:8080
printf '%s\n' 'a long password' | ./bin/kanade user add admin
./bin/kanade serve
```

設定存在資料目錄的 `config.json`（`kanade config` 可以查看）；`serve` 的參數（`-listen`、`-public-url`、`-aria2`）只對這次執行覆蓋設定。網頁上改的設定（快取與暫存大小、下載限制、做種、Drive 同步）存在資料庫，立即生效。服務執行中也可以用 `kanade backup FILE` 複製資料庫。

`serve` 依序在執行檔旁邊、`$KANADE_ARIA2`（或 `aria2` 設定）、執行檔或工作目錄所在原始碼目錄的 `tools/aria2/`，以及 `PATH` 尋找 `aria2c`；找不到時伺服器照常執行，只是不能下載。FFmpeg（`$KANADE_FFMPEG`、資料目錄旁的 `tools/ffmpeg/bin/` 或 `PATH`，旁邊要有 `ffprobe`）用來把無損格式轉成 FLAC，並依 CUE 切割整軌映像；沒有的話這些檔案會等著。

## 發佈

推送版本標籤後，GitHub Actions 會先跑 CI，再建置並發佈安裝腳本下載的 release（`kanade-linux-amd64.tar.gz`、`kanade-linux-arm64.tar.gz`、`kanade.sh`、涵蓋這三個檔案的 `SHA256SUMS`，以及 `VERSION`）：

```bash
git tag v0.1.0 && git push origin v0.1.0
```

標籤含連字號（`v0.2.0-rc1`）時發佈成 pre-release，`latest` 會略過它。手動執行 workflow 會建置同樣的檔案，但只當成 artifact，不發佈。被標記的 commit 必須通過 CI（包括 gofmt），否則不會發佈。

## 授權

[AGPL-3.0](LICENSE)。如果你提供修改過的 Kanade 給其他人使用，請一併提供它的原始碼。內附的 Preact 與 htm 維持各自的授權（`server/internal/web/static/vendor/`）。

## 設計說明

從 `docs/decisions.md` 與 `docs/backend-design.md` 開始看。簡單來說：直接透過 REST API 存取 Drive（D1）；每個檔案上傳後都以 SHA-256 核對；因為每次向 Drive 請求約要 0.6 秒，播放時會在背景把整首歌下載到本機快取（D7）；一切以 1.5 GB 記憶體的 VPS 為準。聆聽統計計算實際聽到的時間，切成 15 分鐘的區段，所以任何時區的「一天」都加得起來。

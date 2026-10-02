# 後端設計

依據 `plan-v0.5.md` 與 `decisions.md` D1–D7。範圍是計畫書 P1「最小閉環」的後端，加上 D7 串流。客戶端（Android）之後再做，這裡只定義它要用的 API。

## 程式與部署形態

- 單一 Go 執行檔 `kanade`（`server/`），純 Go、不需 cgo，可交叉編譯 amd64／arm64。
- `kanade serve --data DIR --listen 127.0.0.1:8080`：主服務。開發期經 Cloudflare Tunnel 對外為 `music.ser1ka.com`。
- 主服務自行管理 aria2 子程序（D5）：產生設定與 RPC 密鑰、只綁 loopback、異常退出時退避重啟。
- 首頁 `/` 與 `/privacy` 由主服務提供（D4），取代 `deploy/landing`。

### 資料目錄

```
DIR/
  db.sqlite           曲庫、任務、設定、憑證（檔案權限 600）
  staging/            匯入暫存（上傳中的分塊、整理副本）
  downloads/          aria2 下載與做種資料
  cache/              串流快取（D7）
  thumbs/             封面縮圖
  aria2/              aria2 設定、session 檔、日誌
```

### Drive 內的位置

所有寫入都在平台根資料夾 `ser1ka Music` 之下（D1）：

- `library/<專輯歌手>/<專輯>/`：音檔。路徑取第一次匯入時的資料，之後不因修改資料而搬移；資料庫才是唯一依據。
- `covers/`：封面。
- `sidecars/`：CUE、LOG 等旁附檔。

## 套件分層（`server/internal/`）

| 套件 | 責任 |
|---|---|
| `config` | 設定與資料目錄 |
| `db` | SQLite 連線、內嵌遷移 |
| `auth` | 管理員帳號、登入 token |
| `gdrive` | OAuth、Drive API、可續傳上傳（session 存資料庫） |
| `media` | 格式辨識、標籤與時長解析、文字編碼偵測 |
| `library` | 曲庫資料模型與查詢、搜尋 |
| `importer` | 匯入管線：解析 → 雜湊 → 去重 → 上傳 → 驗證 → 入庫 |
| `downloader` | aria2 子程序與 JSON-RPC |
| `stream` | D7 播放快取 |
| `api` | HTTP 路由、驗證中介層、頁面 |

## 資料模型（計畫書 §4 三層）

| 表 | 內容 |
|---|---|
| `assets` | 音檔：`sha256`＋`size` 唯一、格式、取樣率、位元深度、聲道、時長、音訊 MD5、Drive file ID、狀態 |
| `tracks` | 錄音版本：曲名、顯示用歌手、版本說明 |
| `track_assets` | 錄音版本與音檔的多對多（同錄音的不同格式） |
| `albums` | 專輯版本：名稱、專輯歌手、日期、型號、封面 |
| `album_entries` | 專輯收錄：專輯、track、asset、碟號、曲序；收錄專屬資料存在這裡，不改寫共用音檔 |
| `artists`、`track_artists` | 歌手；第一版照原字串，不拆分多人名單 |
| `import_batches`、`import_items` | 每次匯入與每個檔案：原始路徑、原始標籤（保留原始位元組）、偵測到的編碼、狀態、錯誤 |
| `downloads` | BT 任務：aria2 gid、infohash、狀態、檔案清單與選取、做種資訊 |
| `drive_uploads` | 進行中的可續傳上傳 session |
| `drive_folders` | 路徑到 Drive 資料夾 ID 的快取 |
| `search_index` | FTS5 trigram：曲名、歌手、專輯名 |
| `users`、`sessions`、`settings`、`credentials` | 帳號、登入 token（只存雜湊）、設定、OAuth token |
| `plays` | 播放紀錄（D9）：session、音檔、歌曲、從哪張專輯播放、位置、實際聽的時間、是否計次、是否聽完 |
| `favorite_tracks`、`favorite_albums` | 收藏 |
| `playlists`、`playlist_items` | 歌單；條目有獨立 ID、歌曲、可選專輯與音檔版本、位置，允許重複 |
| `lyrics` | 每首歌曲一份歌詞：來源（內嵌／LRC／手動）、是否有時間軸 |
| `aliases` | 歌手、專輯、歌曲的其他名稱，建入搜尋索引 |
| `edit_groups`、`edits` | 修改紀錄：每個操作一組，逐欄記舊值與新值，供撤回 |

精確去重鍵是 `sha256`＋`size`（唯一約束），上傳完成以 Drive 回傳的 `sha256Checksum` 驗證（P0 第 1 節）。

## 匯入狀態

`import_items.state`：`pending → parsing → hashing → uploading → verifying → published`，另有 `duplicate`（音檔已存在，只建立收錄）、`failed`、`skipped`（不支援的格式）。每一步完成即寫入資料庫；重啟後從最後完成的步驟繼續，上傳以保存的 session 續傳。

## API（`/api/v1`，JSON，`Authorization: Bearer <token>`）

以下為已實作的端點（2026-10-02）。

| 方法與路徑 | 用途 |
|---|---|
| `POST /setup` | 首次建立管理員；需資料目錄 `setup-code` 檔內的一次性設定碼 |
| `POST /login`、`POST /logout` | 登入取得 token（失敗 5 次／15 分鐘依 IP 節流）、登出 |
| `GET /status` | Drive 連線、aria2 是否就緒 |
| `GET /drive`、`POST /drive/client` | Drive 帳號與容量；載入 OAuth client JSON |
| `POST /drive/auth`、`POST /drive/auth/paste` | 產生授權網址（回呼 `GET /oauth/google/callback`，公開但只接受本服務產生的 state）；貼上回呼網址完成授權 |
| `GET /albums`、`GET /albums/{id}` | 專輯清單（`limit`、`offset`）與收錄 |
| `GET /tracks`、`GET /artists`、`GET /search?q=` | 歌曲、歌手（只列有歌曲的）、搜尋 |
| `GET`／`HEAD /stream/{asset_id}` | 播放（Range、D7 快取）。驗證：`Authorization` header，或 `POST /stream/{id}/url` 取得的 12 小時簽名網址 |
| `GET /covers/{id}?size=N` | 封面縮圖（預設 300，`size=0` 為原圖） |
| `POST /imports` | `{"path": "..."}` 匯入伺服器上 `imports/`、`downloads/`、`staging/` 內的資料夾；或 `{"upload_group": "..."}` 匯入一組客戶端上傳 |
| `GET /imports`、`GET /imports/{id}`、`POST /imports/{id}/retry` | 匯入批次、逐檔狀態與上傳進度、重試失敗項目 |
| `POST /downloads` | `{"uri": "magnet:..."}`／`{"uri": "https://.../x.torrent"}`，或以 `Content-Type: application/x-bittorrent` 直接送 .torrent |
| `GET /downloads`、`GET /downloads/{id}` | 下載清單；單一下載含檔案清單與預設勾選（`suggested`） |
| `POST /downloads/{id}/select` | `{"files": [索引...]}`；省略則採預設勾選 |
| `POST /downloads/{id}/pause`、`/resume`、`/cancel` | 控制 |
| `POST /uploads` | `{"group", "path", "size", "sha256"}` 建立或續接上傳 |
| `PUT /uploads/{id}?offset=N` | 送一個分塊（最多 32 MB）；位移不符回 409 與伺服器已收到的位元組數 |
| `GET /uploads/{id}`、`POST /uploads/{id}/complete` | 進度；完成（驗證大小與 SHA-256） |
| `GET /tasks` | 任務中心：下載與匯入批次 |
| `GET /home` | 首頁資料：繼續播放、最近播放、最近加入、未聽完的廣播劇、任務摘要、待整理（D9） |
| `POST /plays` | 回報播放：`session`、`asset_id`、`album_id`、`position_ms`、`listened_ms`、`finished` |
| `GET /assets/{id}/resume`、`GET /albums/random` | 續播位置；隨機一張音樂專輯 |
| `GET /favorites`、`GET /favorites/ids` | 收藏的歌曲與專輯；只取 ID（客戶端據此標示愛心） |
| `PUT`／`DELETE /favorites/tracks/{id}`、`/favorites/albums/{id}` | 收藏、取消收藏 |
| `GET`／`POST /playlists` | 歌單清單；建立（`name`、`description`、可帶初始 `items`） |
| `GET`／`PATCH`／`DELETE /playlists/{id}` | 歌單內容、改名與說明、刪除 |
| `POST /playlists/{id}/items`、`DELETE /playlists/{id}/items/{item}` | 加入 `{"items": [{"track_id", "album_id", "asset_id"}]}`；移除一個條目 |
| `PUT /playlists/{id}/order` | `{"items": [條目 ID...]}`，必須剛好是歌單現有的全部條目 |
| `GET`／`PUT`／`DELETE /tracks/{id}/lyrics` | 歌詞；手動輸入或刪除 |
| `GET /history?limit&before`、`GET /history/top?days` | 播放記錄（略過不到 10 秒的跳過）；最常播放 |
| `GET`／`PATCH`／`DELETE /tracks/{id}` | 歌曲資訊（含別名、收錄於哪些專輯）；編輯（`title`、`artist`、`version`、`kind`、`aliases`）；永久刪除（音檔移到 Drive 垃圾桶） |
| `POST /tracks/{id}/restore`、`POST /albums/{id}/restore` | 恢復原標籤 |
| `PATCH /albums/{id}` | 編輯專輯與其收錄（`title`、`album_artist`、`date`、`catalog`、`edition`、`kind`、`aliases`、`entries`） |
| `PUT /albums/{id}/cover` | 以 JPEG／PNG 本體更換封面 |
| `POST /albums/{id}/merge`、`/split`、`/remove` | 合併到 `into`；拆分 `entries` 為 `title`；移除收錄 |
| `DELETE /albums/{id}[?tracks=1]` | 移除專輯（可撤回）；`tracks=1` 另永久刪除只在這張專輯的歌曲 |
| `GET /albums/{id}/identify`、`GET`／`POST /albums/{id}/identify/{release}` | MusicBrainz 候選；差異；套用勾選的 `keys` |
| `GET`／`PATCH /artists/{id}` | 歌手與歌曲、別名；改名（`name`）、別名（`aliases`） |
| `GET /edits?limit&before`、`GET /edits/{id}`、`POST /edits/{id}/undo` | 修改紀錄、單筆明細、撤回（部分欄位保留時回傳 `conflicts`；全部無法撤回為 409） |

## 階段

| 階段 | 內容 | 驗收 |
|---|---|---|
| B1 骨架 ✅ | 設定、資料庫、帳號、頁面、Drive 授權 | 服務接手 8080；能登入；Drive 顯示已連線 |
| B2 曲庫與匯入 ✅ | 解析、去重、上傳驗證、入庫、串流 | 匯入本機資料夾的音檔後可列出並播放；重複匯入不產生第二份 |
| B3 BT 下載 ✅ | aria2、選檔、下載後自動匯入 | 以合法測試 torrent 走完整個流程 |
| B4 客戶端上傳 ✅ | 分塊上傳 API | 中斷後續傳，完成後入庫 |

## 實作紀錄

### B1、B2（2026-10-02）

- 服務在開發工作區 `127.0.0.1:8080`，資料目錄 `var/`，經 `music.ser1ka.com` 隧道對外。管理員帳號 `admin`，密碼在 `secrets/admin-password`（權限 600）。
- 媒體解析為純 Go：FLAC、MP3（ID3v1／v2.2–2.4，Xing／VBRI）、MP4／M4A、Ogg Vorbis／Opus，以及辨識 WAV、AIFF、APE、TAK、WavPack、TTA、DSD、WMA、ALAC 以便回報原因。宣告為 ISO-8859-1 的欄位依序試 UTF-8 → CP932（含合理性檢查）→ CP1252，並保留原始位元組。
- 端到端實測（真實檔案 7 首、約 300 MB）：全部入庫；同一批再匯入一次判為重複，未再上傳；Rainbow 單曲的封面依 D2 規則取自 `Scans/VICL-35899_01.jpg`；登入 token 與簽名網址都能播放、可 seek。
- 發現並修正：專輯沒有專輯歌手、各曲歌手不同時（動畫單曲的伴奏曲標作曲者、OST），原本會拆成每位演出者一張專輯。現在以同一專輯資料夾內同名專輯的檔案共同決定：有明確專輯歌手者優先，僅一位演出者用該名，否則為「Various Artists」。
- 已知限制：
  - 搜尋只做 NFKC 與大小寫正規化；平假名／片假名、羅馬字互通留待 P2。
  - Drive 內的資料夾路徑取自第一次匯入，之後不搬移；修正前拆開匯入的 Rainbow 檔案仍留在兩個歌手資料夾。
  - 串流快取的區塊表不持久化，服務重啟會清空快取。

### B3（2026-10-02）

- 下載狀態：`metadata → selecting → queued → downloading → seeding → completed`，另有 `paused`、`failed`、`canceled`。同時只有一個任務在下載（計畫書 §6），做種任務不佔下載名額。
- 所有 BT 任務都以 `.torrent` 透過 `addTorrent` 加入（暫停狀態），讓 aria2 的 session 保存 torrent、任務 ID 與選檔；重啟後可接續（見 P0 第 4 節的發現）。
- 選檔時檢查 2 GB 暫存預算與 4 GB 檔案系統保留空間；不足時先停止最早完成、且匯入全部成功的做種任務（D5），仍不足則拒絕並說明。
- 下載完成後只匯入使用者勾選的檔案；做種結束、且匯入沒有失敗項目時才刪除本地檔案（失敗的項目保留檔案以便重試）。
- 主服務結束時會等 aria2 存好 session 並退出，避免孤兒程序。
- 端到端實測：archive.org 上 CC BY-NC-SA 的 badpanda041，以網址加入 → 等待選檔時重啟服務 → 選 FLAC 與一張圖 → 下載 → 做種 → 自動匯入入庫。
- 測試：`internal/downloader` 以本地 HTTP web seed（BEP 19）產生的 torrent 驅動真的 aria2，不需外網。

### B4（2026-10-02）

- 客戶端以「分組＋相對路徑」上傳，暫存於 `staging/uploads/<分組>/<路徑>`；同一分組匯入時沿用 Disc 資料夾、封面、專輯歌手的分組規則。
- 分塊最多 32 MB；中斷的分塊不計入，位移不符時回報伺服器已收到的量；同一分組與路徑重新建立時接續原上傳。
- 暫存預算與 BT 下載共用 2 GB。匯入成功的檔案立即刪除；整批沒有失敗時清除整個分組（含只為挑封面而上傳的圖片）與上傳紀錄。
- 參考客戶端：`server/scripts/upload-folder.py`（可從電腦上傳資料夾，也是 App 的行為範本）。實測經 `music.ser1ka.com` 隧道上傳 106 MiB 約 10.8 MiB/s，中斷後續傳、匯入、封面選擇與暫存清除皆正確。
- 發現：網域上的 Cloudflare 機器人防護會擋 Python 預設 User-Agent（回傳非 JSON 錯誤頁）；客戶端需帶自己的 User-Agent。Android App 開發時需確認其 User-Agent 不被擋。

### P2-1 個人化（2026-10-02）

- 遷移 6：收藏、歌單、歌詞三組表。部署前備份資料庫到 `var/backups/`。
- 歌詞來源與優先序：手動 > 有時間軸的匯入 > 純文字的匯入；匯入不會覆蓋手動輸入，手動清空即刪除。內嵌歌詞取自 Vorbis `LYRICS`／`UNSYNCEDLYRICS`、ID3 `USLT`、MP4 `©lyr`；之後找同主檔名的 `.lrc`（≤ 256 KB，編碼依 D2 第 4 點）。遷移時從 `import_items.info` 回填已入庫歌曲的內嵌歌詞（現有曲庫沒有）。
- 網頁端：歌曲列「更多」選單（下一首播放、加入佇列、加入歌單、收藏、前往專輯）；專輯頁收藏與整張加入歌單；曲庫新增「歌單」「收藏」分頁；歌單頁可改名、刪除、拖曳或用選單排序；正在播放頁可收藏、切換「播放佇列／歌詞」，有時間軸的歌詞逐行同步、點一行跳到該處；播放記錄頁（近 30 天最常播放、依日期分組的最近播放）。網頁上傳會一併送出 `.lrc`、`.cue`、`.log`。
- 實測（Chrome，桌面與手機寬度）：上述操作逐一走過，重新整理後排序保留，無主控台錯誤；測試資料與測試帳號已刪除。

### P2-2 整理（2026-10-02）

- 遷移 7：修改紀錄、別名、專輯與收錄的匯入身分（origin）、型號、版本、MusicBrainz ID。部署前備份到 `var/backups/db-20261002-pre-0007.sqlite`；在資料庫副本先試過遷移與索引重建。
- 新套件 `internal/identify`（MusicBrainz 用戶端與差異比對）；`gdrive.Client.Trash`。
- 網頁端：歌曲「更多」選單的「編輯資訊…」（含恢復原標籤、永久刪除）；專輯頁「更多」選單的編輯專輯資訊（含各曲曲名、歌手、碟號、曲序）、從 MusicBrainz 辨識、更換封面、合併、拆分、恢復原標籤、移除專輯；歌曲列「從專輯移除」；歌手頁改名與別名；曲庫頁的「修改紀錄」（#/edits）可展開明細與撤回。每次修改的提示都有「撤回」按鈕。
- 實測（Chrome，桌面與 390 寬手機）：上述操作逐一走過；MusicBrainz 實際查詢「Aria The Natural Op - Euforia」，經歌手別名找到「ユーフォリア」（VICL-35986），套用 17 個欄位後撤回；永久刪除用 ffmpeg 產生的測試檔，確認 Drive 檔案進了垃圾桶。測試後曲庫的歌曲、專輯、收錄、音檔、播放記錄與測試前備份逐表比對一致，測試留下的空專輯、歌手、別名、修改紀錄與測試帳號已刪除。
- 實測中修正：
  - MusicBrainz 以整句片語搜尋時，翻錄檔常見的標籤（「Aria The Natural Op - Euforia」）完全找不到，改為逐字比對，並加上「經歌手別名」的查詢；
  - 歌手改名後別名留在舊歌手、搜尋不到；
  - 編輯對話框在窄欄時輸入框超出邊界；
  - 修改後重新載入時整頁閃成載入中，捲動位置跑掉（`useLoad` 的重新整理改為保留目前資料）。
- 已知限制：歌手字串不拆分多人（「A feat. B」是一位歌手）；羅馬字與假名之間的自動互通只靠別名；AcoustID 未實作。

## 使用方式（開發環境）

```bash
# 啟動（工作目錄 server/）
KANADE_DATA=/data/music-platform/var nohup ./bin/kanade serve >> /data/music-platform/var/server.log 2>&1 &
# 帳號
printf '%s\n' '新密碼至少10字' | KANADE_DATA=... ./bin/kanade user passwd admin
# 從電腦上傳資料夾並匯入
python3 scripts/upload-folder.py "某專輯資料夾" --token <登入取得的 token>
```

### 網頁端（2026-10-02，D8）

- 頁面：首頁（最近加入）、搜尋、曲庫（專輯／歌手／歌曲）、專輯、歌手、正在播放與佇列、任務中心（下載：新增、選檔、暫停、繼續、取消；匯入：逐檔狀態、重試）、上傳音樂（檔案或資料夾）、設定（Drive 狀態與重新連線、登出）。
- 版面：寬螢幕用左側導覽列，手機用底部導覽列；迷你播放列固定在下方；深淺色跟隨系統。
- 實測（Chrome，桌面與 390×844 手機模擬）：登入、播放與 seek、下一首預載、Media Session、全螢幕播放頁、上傳（判為重複）、新增下載→選檔→下載→匯入、全形字搜尋。
- 實測中修正：
  - 換頁時全螢幕播放頁不會關閉；
  - 手機上長專輯名把格狀版面撐破；
  - Cloudflare 改寫快取標頭導致部署後仍拿到舊檔（改用版本路徑）；
  - 下載進度超過 100%。
- 使用者回報（2026-10-02）：曲庫切換分頁時沿用上一頁的資料，「歌手」顯示成空白名字（其實是專輯資料），切到「歌曲」時因資料形狀不符而整個前端當掉。修正：資料讀取改以請求參數為鍵，參數一變就不再沿用舊資料；頁面外加錯誤邊界，單頁出錯不再拖垮導覽與播放器。已在瀏覽器重現使用者操作並驗證，含快速連續切換。
- 使用者回報（2026-10-02）：首頁卡片固定顯示同一首（舊的廣播劇），放其他歌時仍顯示「繼續收聽」，重新整理後也不會換成新的。原因：卡片只取「聽超過 30 秒且未聽完」的播放，之後聽的歌（剛開始或已聽完）都不符合；卡片也不看播放器狀態。修正：見 D9「首頁最上方的卡片」。另外發現暫停中的分頁在關閉或切到背景時會再回報一次，把舊播放推成「最近」；改為沒有變化就不回報。已在瀏覽器驗證：播放中、暫停、整首放完接下一首、重新整理後顯示最近一首、聽完的曲目顯示「上次聽完」，以及另一台裝置後來播放時會以那台為準。

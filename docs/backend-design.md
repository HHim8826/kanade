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
  logs/               服務日誌 kanade.log（10 MB 輪替、保留 5 份，P2-6）
```

### Drive 內的位置

所有寫入都在平台根資料夾 `Kanade` 之下（D1）：

- `library/<專輯歌手>/<專輯>/`：音檔。路徑取第一次匯入時的資料，之後不因修改資料而搬移；資料庫才是唯一依據。
- `covers/`：封面。
- `sidecars/`：CUE、LOG 等旁附檔。
- `inbox/`：收件匣（D6，P2-6）。放進來的音樂就地歸檔進 `library/`；曲庫已有的移到 `inbox/重複`，處理完剩下的移到 `inbox/已處理`。

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
| `ffmpeg` | FFmpeg 子程序：轉 FLAC、依 CUE 分軌、PCM MD5 驗證（P2-4） |
| `identify` | MusicBrainz 查詢與差異比對（P2-2） |
| `rss` | RSS／Atom 解析、輪詢、規則、自動下載（P2-5） |
| `stream` | D7 播放快取（硬性容量上限，放不下時直接轉送） |
| `drivesync` | Drive 變更與完整對帳、基準對帳、收件匣排程（P2-6） |
| `diskguard` | 低磁碟監控：清快取、暫停下載、拒絕新上傳／下載（P2-6） |
| `logfile` | 日誌檔輪替（P2-6） |
| `proc` | 讓 aria2、FFmpeg 子程序隨主程序結束（P2-6） |
| `settings` | 網頁設定頁可改的服務設定（資源、下載、Drive 同步），存在 settings 表、立即套用（#74、#75、#77） |
| `staging` | 匯入暫存的容量預算：上傳、下載與匯入共用，放不下時排隊 |
| `uploads` | 客戶端分塊上傳（B4） |
| `lrclib` | LRCLIB 線上歌詞：搜尋、排序、快取、忙碌時重試 |
| `webauthn` | passkey 註冊與登入 |
| `web` | 內嵌的網頁端（`static/`，Preact＋htm，不需建置），於 `/app/` 提供 |
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
| `playlists`、`playlist_items` | 歌單；條目有獨立 ID、歌曲、可選專輯與音檔版本、位置，允許重複。智慧歌單的 `rules`（JSON）不為空，內容由規則即時挑選（#96） |
| `lyrics` | 每首歌曲一份歌詞：來源（內嵌／LRC／手動）、是否有時間軸 |
| `aliases` | 歌手、專輯、歌曲的其他名稱，建入搜尋索引 |
| `edit_groups`、`edits` | 修改紀錄：每個操作一組，逐欄記舊值與新值，供撤回 |
| `sidecars` | 與專輯一起保存的 CUE、LOG：Drive file ID、SHA-256、所屬專輯 |
| `import_sources` | 轉換或分軌的來源檔（SHA-256＋大小）與由它產生的音檔，用於再次匯入時略過 |
| `rss_sources`、`rss_items` | RSS 來源（網址、間隔、規則、自動下載、認證、輪詢狀態）與條目（GUID、下載連結、info hash、大小、做種數、對應的下載） |
| `passkeys`、`oauth_states` | passkey（公開金鑰、名稱、最後使用）；OAuth 授權進行中的 state |
| `uploads` | 客戶端上傳：群組、路徑、大小、已收到的位元組、SHA-256、狀態 |
| `drive_trash` | 移到 Drive 垃圾桶的檔案，供撤回時找回 |
| `album_sections` | 專輯的命名區段（例如合集裡的 Episode 1），依碟號（#82） |
| `album_scopes` | 下載的「專輯資料夾＋專輯標籤」對應的專輯，跨批次維持同一張；`derived` 表示專輯歌手是推導的（#81、#85、#86、#88） |
| `categories`、`album_categories` | 分類（作品、系列或任何分組）與專輯的多對多（#92） |
| `listening` | 實際聽的時間，每次播放依 15 分鐘分桶：歌曲、從哪張專輯、類型、毫秒、該桶是否計次、是否為舊紀錄推算（#93） |
| `bookmarks` | 書籤：音檔、位置、名稱、備註；音檔換版本時標示 `moved`（#98） |

精確去重鍵是 `sha256`＋`size`（唯一約束），上傳完成以 Drive 回傳的 `sha256Checksum` 驗證（P0 第 1 節）。

## 匯入狀態

`import_items.state`：`pending → parsing → hashing → uploading → verifying → published`，另有 `duplicate`（音檔已存在，只建立收錄）、`failed`、`skipped`（不支援的格式）、`excluded`（預覽中排除）、`expanded`（已展開的 ZIP）、`split`（已依 CUE 分軌的整軌）、`discarded`（使用者捨棄的未保存檔案）。每一步完成即寫入資料庫；重啟後從最後完成的步驟繼續，上傳以保存的 session 續傳。

來源清理（審查 #1、#2）：只有結果已寫入資料庫、且狀態為 `published`／`duplicate`（Drive 上有已驗證的副本）的檔案才刪除其暫存來源；`failed`、被略過的音檔與 ZIP 保留來源（上傳暫存、下載檔案、工作資料夾中解出或轉出的檔案），直到重試成功或使用者「捨棄」。預覽中排除的檔案視為使用者已決定，不保留。重試（#18）讓批次重新經過分析：重新取得收件匣檔案、展開 ZIP、轉檔、依（修正後的）CUE 分軌；已有計畫的檔案保留計畫（含預覽修改），新產生的歌曲用新的分組編號。

`import_batches.state`（P2-3）：`analyzing → review → running → done`，另有 `canceled`。分析（展開 ZIP、讀標籤、定計畫）完成後，要預覽的批次停在 `review`，其餘直接 `running`；工作程序只處理 `running` 批次的檔案。

## API（`/api/v1`，JSON，`Authorization: Bearer <token>`）

以下為已實作的端點（2026-10-04 更新）。

| 方法與路徑 | 用途 |
|---|---|
| `POST /setup` | 首次建立管理員；需資料目錄 `setup-code` 檔內的一次性設定碼 |
| `POST /login`、`POST /logout` | 登入取得 token（失敗 5 次／15 分鐘依 IP 節流，驗證中的請求也算）、登出。要 cookie 的登入（`"cookie": true`）需 `X-Requested-With: kanade`；登入路由拒絕瀏覽器標示為跨站的請求（審查 #60） |
| `GET /passkeys/available`、`POST /passkeys/login/options`、`POST /passkeys/login` | 不需登入：是否有任何 passkey（登入頁據此顯示按鈕）、取得一次性 challenge、以 passkey 的回應登入（同密碼登入的節流與 cookie） |
| `GET /passkeys`、`POST /passkeys/options`、`POST /passkeys`、`PATCH`／`DELETE /passkeys/{id}` | 列出自己的 passkey；`{"password"}` 確認密碼後取得建立選項（密碼錯回 403）；儲存裝置建立的 passkey；改名、移除 |
| `GET /status` | Drive 連線、aria2 是否就緒 |
| `GET /drive`、`POST /drive/client` | Drive 帳號與容量；載入 OAuth client JSON |
| `POST /drive/auth`、`POST /drive/auth/paste` | 產生授權網址（回呼 `GET /oauth/google/callback`，公開但只接受本服務產生的 state）；貼上回呼網址完成授權 |
| `GET /albums`、`GET /albums/{id}` | 專輯清單（`limit`、`offset`）與收錄 |
| `GET /tracks`、`GET /artists`、`GET /search?q=` | 歌曲、歌手（只列有歌曲的）、搜尋；`/tracks?filter=no_album`／`no_artist` 只列沒有專輯／沒有歌手的歌曲 |
| `GET`／`HEAD /stream/{asset_id}` | 播放（Range、D7 快取）。驗證：`Authorization` header，或 `POST /stream/{id}/url` 取得的 12 小時簽名網址 |
| `GET /covers/{id}?size=N` | 封面縮圖（預設 300，`size=0` 為原圖） |
| `POST /imports` | `{"path": "..."}` 匯入伺服器上 `imports/`、`downloads/`、`staging/` 內的資料夾；或 `{"upload_group": "..."}` 匯入一組客戶端上傳；`"preview": true` 時分析後等待確認 |
| `GET /imports/{id}/preview`、`POST /imports/{id}/plan` | 預覽（各組、單曲、其他檔案、偵測到的編碼）；修改計畫（`op`：`group`、`items`、`move`、`standalone`、`folders`、`encoding`、`exclude`、`include`），回傳新的預覽 |
| `POST /imports/{id}/start`、`POST /imports/{id}/cancel` | 開始執行；取消（只限等待確認或分析中） |
| `GET /sidecars/{id}` | 下載與專輯一起保存的 CUE／LOG |
| `GET /imports`、`GET /imports/{id}`、`POST /imports/{id}/retry` | 匯入批次、逐檔狀態與上傳進度（完成的批次另有 `unsaved`：沒存進曲庫、來源保留中的檔案數）、重試失敗項目：回應 `{requeued, saved, fetching, lost}`（重新排入、同一檔案已由其他批次入庫、正由下載重新取得、已無法取得，審查 #57） |
| `POST /imports/{id}/discard` | 捨棄完成批次中沒存進曲庫的檔案（失敗、被略過的音檔），讓來源可以清理：`{"discarded": n}` |
| `POST /downloads` | `{"uri": "magnet:..."}`／`{"uri": "https://.../x.torrent"}`／`{"uri": "https://.../album.zip"}`（直接下載），或以 `Content-Type: application/x-bittorrent` 直接送 .torrent；回傳 `{id, kind}`（`bt` 或 `http`） |
| `GET /downloads`、`GET /downloads/{id}` | 下載清單；單一下載含檔案清單與預設勾選（`suggested`） |
| `POST /downloads/{id}/select` | `{"files": [索引...]}`；省略則採預設勾選。總量超過暫存預算也接受，分批下載（審查 #28）；下載回應含 `round`、`rounds`、`left`（還沒輪到的檔案數）、`waiting_space`、`budget` |
| `POST /downloads/{id}/pause`、`/resume`、`/cancel` | 控制；`cancel` 也用於放棄失敗的下載（清除還沒匯入的檔案，已入庫的歌曲保留） |
| `POST /downloads/{id}/retry` | 重新開始失敗的下載（審查 #49）：已匯入的批次保留，其餘照常分批；種子從資料夾找回或由原連結重新取得。同一個種子另有進行中的下載時拒絕。回應中的 `can_retry` 表示有檔案可重試 |
| `POST /uploads` | `{"group", "path", "size", "sha256"}` 建立或續接上傳 |
| `PUT /uploads/{id}?offset=N` | 送一個分塊（最多 32 MB）；位移不符回 409 與伺服器已收到的位元組數 |
| `GET /uploads/{id}`、`POST /uploads/{id}/complete` | 進度；完成（驗證大小與 SHA-256；可重送） |
| `GET /uploads`、`DELETE /uploads/groups/{group}` | 未完成的上傳群組；取消一個群組（正在匯入時 409） |
| `GET /tasks?history=N` | 任務中心：所有進行中、等待使用者、失敗或還有檔案沒存進曲庫的下載與匯入，加上最近 N 筆（預設 50）已結束的；`more_downloads`／`more_imports` 表示還有更早的（審查 #58） |
| `POST /downloads/{id}/clear`、`POST /imports/{id}/clear`、`POST /tasks/clear` | 從任務中心移除已結束的記錄（單筆、全部）：只標記 `cleared_at`，曲庫、檔案與匯入紀錄不動；還在進行、做種、有檔案可重試或沒存進曲庫的不能移除（409，審查 #54） |
| `GET`／`POST /rss/sources`、`PATCH`／`DELETE /rss/sources/{id}` | RSS 來源（密碼與 Cookie 只回報是否已設定；`auth_origins`：其他也要收到帳密與 Cookie 的網站，每行一個） |
| `POST /rss/sources/{id}/refresh`、`GET /rss/sources/{id}/search?q=` | 立即更新；站內搜尋（不入庫） |
| `GET /rss/items?source&q&only=included&before`、`POST /rss/items/{id}/download`、`POST /rss/sources/{id}/download` | 條目（含是否符合規則、關聯下載的 `download_id` 與 `download_state`、只在完成時為真的 `downloaded`、自動下載的 `auto_state`：`pending`／`done`／`failed` 與 `auto_error`）；從條目或站內搜尋結果開始下載，失敗的下載改為重試，進行中的不重複開始 |
| `GET /home` | 首頁資料：繼續播放、最近播放、最近加入、未聽完的廣播劇、任務摘要、待整理（D9） |
| `POST /plays` | 回報播放：`session`、`asset_id`、`album_id`、`position_ms`、`listened_ms`、`finished`、`seq`（同一次播放遞增）、`at`（用戶端時間，毫秒） |
| `GET /assets/{id}/resume`、`GET /albums/random` | 續播位置；隨機一張音樂專輯 |
| `GET /favorites`、`GET /favorites/ids` | 收藏的歌曲與專輯；只取 ID（客戶端據此標示愛心） |
| `PUT`／`DELETE /favorites/tracks/{id}`、`/favorites/albums/{id}` | 收藏、取消收藏 |
| `GET`／`POST /playlists` | 歌單清單；建立（`name`、`description`、可帶初始 `items`） |
| `GET`／`PATCH`／`DELETE /playlists/{id}` | 歌單內容、改名與說明、刪除 |
| `POST /playlists/{id}/items`、`DELETE /playlists/{id}/items/{item}` | 加入 `{"items": [{"track_id", "album_id", "asset_id"}]}`；移除一個條目 |
| `PUT /playlists/{id}/order` | `{"items": [條目 ID...]}`，必須剛好是歌單現有的全部條目 |
| `GET`／`PUT`／`DELETE /tracks/{id}/lyrics` | 歌詞；手動輸入或刪除 |
| `GET`／`POST /tracks/{id}/lyrics/online` | 在 LRCLIB 尋找這首歌的歌詞（只送標題與歌手，依吻合程度排序，`exact` 為標題、歌手相同且長度差 2 秒內）；`{"id"}` 套用使用者選的（取代現有歌詞），加上 `"auto": true` 則只在沒有歌詞時套用 |
| `GET /history?limit&before&before_id`、`GET /history/top?days` | 播放記錄（略過不到 10 秒的跳過；游標為上一頁最後一筆的時間與 `play_id`）；最常播放 |
| `GET`／`PATCH`／`DELETE /tracks/{id}` | 歌曲資訊（含別名、收錄於哪些專輯）；編輯（`title`、`artist`、`version`、`kind`、`aliases`）；永久刪除（音檔移到 Drive 垃圾桶） |
| `POST /tracks/{id}/restore`、`POST /albums/{id}/restore` | 恢復原標籤 |
| `PATCH /albums/{id}` | 編輯專輯與其收錄（`title`、`album_artist`、`date`、`catalog`、`edition`、`kind`、`aliases`、`entries`） |
| `PUT /albums/{id}/cover` | 以 JPEG／PNG 本體更換封面 |
| `POST /albums/{id}/merge`、`/split`、`/remove` | 合併到 `into`；拆分 `entries` 為 `title`；移除收錄 |
| `DELETE /albums/{id}[?tracks=1]` | 移除專輯（可撤回）；`tracks=1` 另永久刪除只在這張專輯的歌曲 |
| `GET /albums/{id}/identify`、`GET`／`POST /albums/{id}/identify/{release}` | MusicBrainz 候選；差異；套用勾選的 `keys` |
| `POST /albums/{id}/vgmdb`、`POST /albums/{id}/vgmdb/apply` | 用戶端從貼上的 VGMdb 專輯頁讀出的 `album`（名稱、專輯歌手、日期、型號、封面網址、各碟曲名與長度）和這張專輯比對，回傳同 MusicBrainz 的差異；套用勾選的 `keys`，勾了封面才從 media.vgm.io 下載 |
| `GET`／`POST /organize/folders` | 沒有專輯的歌曲中，資料夾說明了專輯的（見下；每個資料夾有 `key` 與來源 `source`）；`{"folders": [{key, title, album_artist}]}` 依資料夾整理，一次可撤回 |
| `GET`／`PATCH /artists/{id}` | 歌手與歌曲、別名；改名（`name`）、別名（`aliases`） |
| `GET /edits?limit&before`、`GET /edits/{id}`、`POST /edits/{id}/undo` | 修改紀錄、單筆明細、撤回（部分欄位保留時回傳 `conflicts`；全部無法撤回為 409） |
| `GET /drive/sync` | 同步狀態：上次變更檢查、上次完整對帳與結果、是否進行中、基準對帳是否未完成、收件匣上次檢查 |
| `POST /drive/reconcile` | 在背景開始完整對帳（202；已在進行時 409） |
| `POST /drive/inbox` | 立即檢查收件匣：`{"files": 排入匯入的數量, "waiting": 剛放進來、等下次的數量}` |
| `GET /library/missing` | 音檔在 Drive 遺失的歌曲 |
| `POST /account/password` | `{"current", "password"}` 改自己的密碼：結束所有登入、保留 passkey（#76） |
| `GET /sessions`、`DELETE /sessions/{id}`、`POST /sessions/end-others` | 自己的登入（裝置、最後使用）；結束一個；結束其他全部 |
| `GET /settings`、`PUT /settings/resources`、`/settings/downloads`、`/settings/drive` | 服務設定：播放快取與暫存容量、磁碟保留空間；下載與上傳限速、同時下載數、每個種子的連線數、是否做種與分享率、時數；收件匣自動匯入、檢查間隔、資料夾靜置時間。儲存時檢查並立即套用 |
| `POST /cache/trim` | 清空播放快取（播放中的檔案保留） |
| `GET /tracks/random?n&kind&not` | 從全曲庫隨機挑歌（以歌曲為單位、只挑可播放的；`kind` 為 `music`、`spoken` 或 `all`）；`not` 由新到舊列出不要再挑的歌，全部挑過時從最久以前的再繞一輪（#72、#105） |
| `GET /tasks/older?kind&before` | 更早的已結束任務，一次一頁（#68） |
| `POST /albums/merge` | 合併多張專輯到 `into` 或新專輯（`title`、`album_artist`），`sections` 讓每張成為一個區段；`"preview": true` 只回傳計畫 |
| `POST /albums/edit`、`/albums/remove`、`/tracks/edit`、`/tracks/place`、`/tracks/delete`、`/favorites/batch` | 批次操作（#83）：先檢查所有 ID，一次一筆修改紀錄 |
| `PUT /albums/{id}/sections` | `{"sections": {"碟號": "名稱"}}` 命名區段 |
| `PUT /downloads/{id}/grouping`、`GET`／`POST /downloads/{id}/collection` | 下載的專輯分組（依標籤、每個資料夾、合集）；已入庫的下載整理成合集：預覽計畫、套用（#82、#87） |
| `GET /categories[?albums=]`、`POST /categories`、`PATCH`／`DELETE /categories/{id}` | 分類與各自的專輯數、封面；帶 `albums` 時另回每個分類含其中幾張（批次分類用）；新增、改名、刪除（專輯不受影響，可撤回） |
| `POST /albums/categorize` | `{"albums", "add", "remove", "create"}` 一次把專輯放入、移出分類並可新增一個；同一分類不能同時放入與移出（#92、#114） |
| `GET /albums?category=N\|none&q&sort` | 某分類（或未分類）的專輯，可搜尋、依名稱或最近入庫排序 |
| `GET /stats/summary`、`/stats/days?year`、`/stats/day?date`、`/stats/top?from&to&group&by&limit`、`/stats/trends?from&to` | 我的聆聽（#93）：今天／本週／本月／今年；一年的每日時間（熱力圖）；某天聽了什麼；歌曲、歌手、專輯排行（依次數或時間）；期間的每日、時段、星期分布與連續天數。都帶 `tz`（IANA 時區）與可選的 `kind` |
| `GET /stats/export?format=csv\|json`、`DELETE /stats` | 匯出聆聽紀錄；清除（播放紀錄保留） |
| `GET /bookmarks?track`、`POST /bookmarks`、`PATCH`／`DELETE /bookmarks/{id}` | 書籤（#98）：全部或某首歌的；`{"asset_id", "position_ms", "name", "note"}` 新增；改名與備註；刪除 |
| `POST /playlists/preview`、`PUT /playlists/{id}/rules`、`GET /playlists/{id}/next?n&not` | 智慧歌單（#96）：預覽規則挑出的歌；修改規則；依規則一直播時的下一批（`not` 同 `/tracks/random`，只剩排除的歌時繞一輪） |
| `GET /tracks/{id}/lyrics/online?title&artist`、`?q` | 線上歌詞：預設用歌曲自己的標題與歌手，依序試標題＋歌手、只用標題、「曲名 / 歌手」拆開、關鍵字；也可手動指定標題與歌手，或只用關鍵字（#122、#123） |

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

### P2-3 匯入預覽（2026-10-02）

- 遷移 8：批次的預覽旗標與選項（文字編碼）、檔案的角色（音檔／附屬檔／ZIP）與計畫、`sidecars`。部署前備份到 `var/backups/db-20261002-pre-0008.sqlite`。
- 匯入流程改為「分析 → 確認 → 執行」，原有的單元測試（專輯歌手推定、廣播劇判斷、歌詞、恢復原標籤）照舊通過；新增預覽編輯、依資料夾分組、排除與取消、GBK 重新解碼、ZIP（CP932 檔名、封面、CUE、清理）與 ZIP 檢查（路徑逃逸、壓縮比、預算、沒有音檔）的測試。
- 暫存預算由下載、上傳與展開的 ZIP 共用（`Other`／`Space`）。
- 網頁端：上傳頁可選 ZIP，上傳完直接進入預覽頁（`#/import/<id>`）；任務頁顯示「分析中／等待確認／已取消」並可進入預覽；專輯頁列出附屬檔案。
- 實測（Chrome，桌面與手機寬度）：伺服器資料夾（兩碟、CUE、LOG、封面、一首沒有專輯標籤的歌）預覽 → 改專輯名稱與曲名、把單曲移進專輯 → 匯入，專輯、歌曲、附屬檔案下載正確；上傳 ZIP → 伺服器展開 → 預覽 → 取消，暫存與展開的檔案都已刪除。測試資料以永久刪除移到 Drive 垃圾桶，測試用的 Drive 資料夾與附屬檔也移到垃圾桶；曲庫與測試前備份逐表比對一致。

### P2-4 轉換與分軌（2026-10-02）

- 遷移 9：匯入項目記錄來源（路徑、轉換或分軌、SHA-256、大小）；`import_sources`。部署前備份到 `var/backups/db-20261002-pre-0009.sqlite`。
- 新套件 `internal/ffmpeg`。開發環境的 FFmpeg 在 `tools/ffmpeg`（n8.1 靜態版），解碼器含 APE、TAK、WavPack、TTA、ALAC。
- 事前驗證（實際執行）：24-bit WAV／ALAC 轉 FLAC 後仍為 24-bit、PCM MD5 相同；以原位元深度解碼的 PCM MD5 等於 FLAC STREAMINFO 的 MD5；`atrim` 依取樣數切割、各段長度相加等於原檔、串接 MD5 等於原檔。
- 測試：轉換（WAV 24-bit、WavPack、ALAC、浮點 WAV 被拒）、分軌（EAC 式 .wav 指向 .flac、pregap 歸屬、封面取自原資料夾）、時間對不上的 CUE 讓整軌失敗而不是整張匯成一首、同一來源再次匯入被略過。
- 實測（Chrome）：伺服器資料夾內放 WavPack 整軌＋CP932 編碼的 CUE（FILE 寫 .wav）、24-bit/96 kHz WAV、浮點 WAV：預覽正確顯示日文曲名、分軌與轉換標示與略過原因；匯入後為 FLAC 44.1/16 三首與 FLAC 96/24 一首，CUE 存為附屬檔，分出的歌曲可以連續播放。測試資料已刪除（Drive 檔案移到垃圾桶），曲庫與測試前備份逐表比對一致。

### P2-5 RSS（2026-10-02）

- 遷移 10：`rss_sources`、`rss_items`、`downloads.auto_select`。部署前備份到 `var/backups/db-20261002-pre-0010.sqlite`。
- 下載器：`Add` 多了「自動選檔」；取得檔案清單後若為自動下載，直接以預設勾選開始，失敗時留在選檔並寫明原因（以真的 aria2 測試）。
- 測試：以真實 Nyaa feed（2026-10-02 取得、裁成兩條）為解析樣本；Atom、Shift-JIS feed、base32 magnet、只有 info hash、只有網頁、不是 feed 的 HTML；規則（全半形、平片假名、排除優先）；基準、自動下載只對之後的新條目、條件式請求 304、手動下載、關閉再開啟自動下載重設基準、站內搜尋、失敗退避與恢復。
- 實測（Chrome，桌面與手機寬度）：以 Nyaa 範本（無損、關鍵字 ARIA）新增來源，第一次更新取得 75 條；篩選、站內搜尋「Euforia」、來源頁立即更新、編輯規則與自動下載選項都正常。沒有按「下載」、也沒有儲存自動下載，避免實際下載受版權保護的內容；下載流程由單元測試與先前的合法測試 torrent（B3）涵蓋。測試來源已刪除。

### P2-6 Drive 與資源（2026-10-02）

- 遷移 11：`downloads.paused_by`（因磁碟暫停的標記）、`import_items.drive_id`／`drive_parent`／`drive_size`（收件匣項目）。部署前備份到 `var/backups/db-20261002-pre-0011.sqlite`。
- 新套件 `drivesync`、`diskguard`、`logfile`、`proc`；`gdrive` 加上 Changes、清單、移動與 Range 讀取（`ReaderAt`）；`library.ObserveDriveFile` 依 checksum 與大小決定 verified／missing。
- 設定 `drive_changes_token`（變更位置）、`drive_sync_baseline`（`done` 表示已有基準對帳）。
- 測試：變更與完整對帳、基準對帳（首次與位置過期）、完整與增量對帳交錯時保留較新的觀測、同 ID 內容改寫不被信任；收件匣就地歸檔、重複移出、等待新檔案、上限只算可匯入的新檔案、掃描後被替換的檔案以新內容匯入、整理到 `inbox/已處理`；磁碟保護（快取足夠、暫停、邊界、重啟後維持）、暫停寫入失敗時回報且不啟動下載；快取硬性上限、轉送、Forget、並行使用（`-race`）。
- 實測與重啟恢復結果見 `p2-design.md` P2-6。
- 審查 issue #3、#33–#39 的修正包含在本階段。

### 審查修正：匯入、暫存與分批下載（2026-10-02）

- 遷移 12：`import_sources.piece`、`import_items.source_piece`。來源去重（#19）改為「每個預期產物（轉檔為 0、分軌為各 CUE 軌號）都還是被歌曲使用的已驗證檔案」才跳過；部分匯入、產物被刪或在 Drive 遺失時重新處理，曲庫已有的歌曲以 checksum 判為已存在。分軌寫入的標籤改為固定順序，同一來源每次切出相同位元組。
- 遷移 13：`downloads.round`、`round_bytes`、`done_before`、`note`；下載狀態多了 `importing`。
- 共用暫存預算 `internal/staging`（#4）：上傳、下載、匯入工作資料夾共用一把鎖，「是否放得下」與「記錄預留」在同一步完成，並一併檢查磁碟保留空間。上傳建立時預留整個檔案；下載每一批開始時預留該批；匯入在寫入工作資料夾前（解 ZIP、取收件匣檔案、FFmpeg 輸出）先 hold 上界，寫完後由實際檔案計入。FFmpeg 輸出的上界為原始 PCM 大小加 1/64、每個檔案 256 KB 與內嵌圖片，並以 `-fs` 截斷超出的輸出（截斷後 PCM 驗證失敗，不會入庫）。下載資料夾以實際配置的區塊計算（aria2 留下的未選檔案片段是稀疏檔）。
- 分批下載（#28）：選取不再因總量超過預算被拒。排程器在下載輪到時依剩餘的暫存空間規劃一批：整個資料夾優先（first-fit），資料夾太大時先帶它的小附件（CUE、LOG、歌詞、封面）再盡量放音檔；單一檔案大於整個預算時，在暫存幾乎淨空（≤ 預算 1/4）且磁碟足夠時單獨一批。每批下載完移除 aria2 任務（批與批之間不做種），匯入該批；匯入完成後清掉已入庫的檔案與其他片段，保留後續批次需要的附件與沒存進曲庫的檔案；下一批從保存的 `task.torrent` 重新加入，並以 `check-integrity` 驗證既有資料。最後一批照常做種。沒有空間時任務留在排隊並顯示「等待暫存空間」，有空間就自動開始。
- 上傳完成（#5）：`Complete` 可重送；若 rename 後、寫入資料庫前中斷，已就位的檔案以大小與 SHA-256 檢查後完成；內容不符則重新上傳。啟動時 `Recover` 自動完成這類上傳。
- 測試：上傳略過檔案保留、ZIP 內略過檔案保留工作資料夾、結果寫入失敗保留來源且重啟後不重複入庫、重試轉檔與修正後的 CUE 與收件匣 LOG、部分分軌補齊與遺失的轉檔重新匯入、並行預留不超額（staging 與 uploads）、分軌的上界與預算不足時失敗、上傳 rename 後恢復、分批規劃（含資料夾拆分與附件）、真的 aria2 分三批下載（含大於預算的單檔）。
- 實測：暫存預算暫設 50 MB，以本機 web seed 的 76 MB 測試 torrent（兩個資料夾）下載：第 1 批（38 MB）下載、匯入、清除後才開始第 2 批，兩張專輯都入庫；測試資料已刪除。

### 審查修正：RSS（2026-10-02）

- 遷移 14：`rss_sources.auth_origins`；`rss_items.auto_state`、`auto_tries`、`auto_next`、`auto_error`。
- 認證範圍（#17）：帳密與 Cookie 只送到 RSS 網址的同一個網站（協定、主機、連接埠都相同），以及來源明列的其他網站；轉址時重新判斷（Go 預設會把它們帶到子網域、也會從 https 帶到 http）。跨站的 .torrent 連結預設不帶。
- 輪詢（#22）：同一個來源的背景輪詢與「立即更新」輪流執行；抓取回來後以當下的設定判斷：網址改了就捨棄這次的結果並立刻重抓，自動下載在抓取中被關閉就不下載，抓取中重新開啟則基準留給下一次。設定每次修改都會讓版本（`updated_at`）前進。
- 自動下載佇列（#23）：符合規則的新條目標為 `pending`，每次輪詢（包括 304 與抓取失敗時）最多開始 5 個，最舊的優先；失敗時保留在佇列，從 10 分鐘起加倍等待（最長 6 小時），8 次後標為 `failed`。規則改了不再符合的離開佇列；關閉自動下載時清空佇列；排隊中的條目不會被條目數上限刪掉。條目列表顯示「等待自動下載」或失敗原因。
- 站內搜尋（#24）：結果列以來源與 GUID（或 info hash、下載連結）為 key，「已下載」狀態不會留給另一個 torrent；較慢的舊搜尋不會覆蓋新的結果。
- 測試：認證只到自己的網站與明列網站（含轉址、不同協定或連接埠）、抓取中關閉自動下載、抓取中重新開啟、抓取中換網址、超過每次上限與暫時失敗的佇列、關閉自動下載清空佇列。

### 審查修正：網頁播放器（2026-10-02）

- 播放控制（#16）：常駐列有可拖曳的播放位置（上緣細線，懸停或聚焦時出現把手）、時間、隨機與循環（桌面）、收藏、歌詞、佇列、音量與靜音；手機版保留上一首、播放、下一首與佇列，其餘在全畫面播放頁。隨機開關時目前的歌不中斷，關閉後回到原本順序；循環為關閉、整個佇列、單曲三態；音量、靜音、隨機、循環記在瀏覽器（`localStorage`，不可用時只在本頁有效）。佇列可移到下一首、上移、下移、移除，「清除待播」保留到目前這首，「停止並清空」停止播放；移除正在播放的歌接著播下一首（原本暫停則維持暫停），移除最後一首則停止。同一首歌重複加入時以佇列 ID 區分。
- 封面（#12）：載入失敗只記在那張圖片上，換曲後重新嘗試新的封面。
- 多碟專輯（#13）：點任一碟的歌，佇列是整張專輯，從那首開始，跨碟接續。
- 登出（#14）：登出前先回報播放進度，再停止音訊、清空佇列與鎖定畫面資訊；在別處登出（API 回 401）時同樣停止。
- 隨便聽一張（#32）：直接開始播放並留在原頁，提示訊息提供「前往專輯」。

### 審查修正：雜項（2026-10-02）

- 首頁 ETag（#11）：以實際送出的首頁內容（含資產版本路徑）計算，只改 JS／CSS 的版本也會讓舊的驗證值得到 200 與新入口。
- aria2 路徑（#10）：依序找執行檔旁、`$KANADE_ARIA2`／`-aria2`、執行檔或工作目錄所在 checkout 的 `tools/aria2/aria2c`（往上找幾層）、`PATH`；都沒有時服務照常啟動、只是不能下載，日誌說明安裝方式。
- 播放回報順序（#8，遷移 15：`plays.seq`、`plays.skew`）：用戶端每次回報帶遞增的 `seq` 與自己的時間 `at`；較舊的回報不改位置與時間，聽了多久、是否計次、是否播完只會增加。時間用用戶端產生回報的時刻（以該次播放第一個回報量得的時差換算成伺服器時間），不晚於收到時、不早於上一次，所以在網路上延遲的回報不會把舊的播放頂成「最近播放」。舊版用戶端（沒有 `seq`）照舊處理。
- 播放記錄分頁（#25）：游標是最後一筆的時間與播放 ID（`before`、`before_id`），排序同樣以兩者為準。
- Drive 資料夾快取（#7）：快取的資料夾 ID 使用前確認仍在預期的父資料夾、沒有被丟進垃圾桶（同一個資料夾 5 分鐘內只問一次）；被移走、丟進垃圾桶或刪除時，忘掉它與底下所有快取，在正確的父資料夾重新尋找或建立。

### 審查修正：整理與上傳（2026-10-02）

- 合併與附屬檔案（#27）：專輯顯示自己以及所有（直接或間接）合併進來的專輯的 CUE／LOG，同一個檔案只列一次；重新匯入時也在這些專輯裡找已存在的附屬檔；撤回合併後自動回到原專輯。
- 撤回（#20）：除了比對目前的值，也看這次修改之後是否有其他仍有效的修改碰過同一個欄位（未被撤回、也不是「撤回之後的修改」）；有就保留並列為衝突，即使值改回相同（A→B→C→B）。被撤回的後續修改與其撤回互相抵銷。
- 預覽中選擇另建專輯（#21）：預覽對分組設定「另建專輯」會標為使用者的選擇（`chosen`），這些檔案即使以前匯入過，也在新專輯建立自己的收錄（共用音檔）；沒有這個選擇的重匯入仍回到原本的收錄。同一批次重試以分組已建立的專輯為準，不會重複建立。
- 永久刪除與 Drive 垃圾桶（#26，遷移 16：`drive_trash`）：刪除歌曲的交易中同時記下要移到垃圾桶的 Drive 檔案；請求當下成功（或 Drive 已沒有該檔）就刪掉記錄，失敗則由同步工作每 10 分鐘重試（等待時間從 1 分鐘起加倍，最長 1 天）；設定頁顯示待處理數量與最近的錯誤。
- 未完成的上傳（#6）：網頁以「檔案路徑、大小、修改時間」辨認同一份選取，重試或重新整理後重選相同檔案會沿用同一個上傳群組並從中斷處續傳，不會再預留一次空間。`GET /uploads` 列出未完成的群組，`DELETE /uploads/groups/{group}` 取消（刪除暫存、釋出預留；正在匯入的群組拒絕），一週沒有動靜且未匯入的群組自動清除。上傳頁顯示「未完成的上傳」與取消按鈕。

### 審查修正：網頁版面、外觀與播放模式（2026-10-03）

- 版面（#48、#15）：外框鋪滿視窗，內容區自己捲動，播放欄與導覽列（手機在下、桌面為左側欄）各佔一格；捲軸停在播放欄上方，播放欄延伸到右邊緣。換頁時重設內容區的捲動位置。桌面內容在側欄右邊的空間置中，最寬 1280px。
- 曲庫分頁（#9）：專輯、歌手、歌曲每次載入 200 筆，捲到底（或按「載入更多」）載入下一頁；排序加上 ID 作為決勝（`title, id`、`name, id`、`album_artist, title, id`），換頁不會重複或漏掉。
- 卡片（#30）：專輯卡與續播卡的作者最多兩行，完整文字放在 `title`，專輯頁顯示全部。
- 對話框（#29）：選檔與 RSS 來源使用寬版（最寬 1120px）；捲動區保留穩定的捲軸空間與內距，焦點外框不被裁切；RSS 表單的「取消／新增」固定在捲動區下方。
- 選單（#47）：依按鈕上下的實際空間決定位置，兩邊都放不下時取較大的一邊並在選單內捲動；在選單內滾動不會關閉選單（在外面滾動才關閉）。打開時焦點在第一項，方向鍵、Home、End 移動，Esc 關閉並把焦點還給按鈕。
- 外觀（#31）：設定頁「外觀」可選配色（藍紫、青綠、玫瑰）與明暗（跟隨系統、淺色、深色），記在瀏覽器（`localStorage` 的 `kanade.theme`）。`theme-init.js` 在畫面繪製前套用，並同步瀏覽器的 `theme-color`；讀不到或內容損壞時用預設值。
- 播放模式（#40）：一個按鈕依序切換順序播放、列表循環、單曲循環、隨機播放，四者互斥；隨機播放在最後一首之後以新的順序繼續。舊的隨機／循環偏好轉成對應的模式。
- 拖動進度（#41）：拖動時只更新顯示的位置、填色與時間，放開才 seek 一次；鍵盤方向鍵每次 5 秒、Page Up／Down 30 秒，立即生效。
- 循環從頭播放（#42）：「繼續播放」的位置只用於那一次；單曲循環、列表或隨機重新開始時從頭播放，也不再查詢廣播劇的續播位置。

### 審查修正：下載種子、交接與暫存（2026-10-03）

- 種子（#49）：分批之間需要重新加入任務時，用 `torrentFor` 取得這個下載自己的種子，並核對 info hash：先看 `task.torrent`，再找資料夾裡任何對得上的 .torrent（aria2 以整份檔案的 SHA-1 命名 RPC 加入的種子，以 info hash 命名 magnet 的 metadata），最後從 .torrent 連結重新下載；找到就存成 `task.torrent`（先寫暫存檔再改名）。服務啟動時先替進行中的下載補存。
- 失敗不再清掉資料（#49、#43）：只有每個選取的檔案都交給了匯入、且匯入都已入庫或被放棄，才清除下載資料夾；失敗的下載保留檔案與種子。任務頁對失敗的下載提供「重試」（`POST /downloads/{id}/retry`）與「放棄並清除」。
- 交接給匯入（#43）：建立匯入批次與寫入下載的關聯在同一個交易（`importer.CreateBatchLinked`）；任一失敗時，下載停在原狀態、記下錯誤，一分鐘後再試，重啟後也會繼續。中間批在交接成功後才移除 aria2 任務。沒有可匯入檔案的批次（例如只有掃描圖）視為已交接。
- 最後一批的 CUE（#45）：每一批都帶上同資料夾在前幾批下載、仍留在磁碟上的附屬檔（CUE、LOG、封面），最後一批也一樣，所以整軌音檔能照 CUE 分軌。
- 轉檔工作空間（#46，遷移 17：`downloads.round_work`）：規劃分批時，每個檔案再加上匯入時需要的工作空間：WAV／AIFF 約為本身大小，APE／TAK／WavPack／TTA 與帶 CUE 的整軌 FLAC 以 2.5 倍估算，ZIP 以 2 倍估算。這部分在下載期間一起預留，進入匯入時釋出給匯入使用。匯入的 FFmpeg 輸出若暫時沒有空間，會等其他用途釋出（最多 10 分鐘，項目上顯示「等待暫存空間」）；暫存區除了自己的原檔幾乎是空的、而且磁碟夠時，大於整個預算的輸出也可以單獨進行。其他用途空間不足時，可以停止已完整入庫的做種（`Budget.Reclaim`）。
- 磁碟保留（#4）：每個使用者回報「已在磁碟」與「尚待寫入」兩部分（`staging.Usage`）；判斷保留空間時，從目前可用空間再扣掉所有尚待寫入的預留與 hold，已經寫到磁碟的不重複扣。
- RSS（#50、#22）：條目回傳關聯下載的實際狀態（`download_state`），只有做種中或完成才算「已下載」；進行中或已完成的下載優先於失敗或取消的。失敗的條目按「重試」會重試原本的下載（保留已入庫的部分），取消的可以重新下載；同一個種子正在下載時不會再開一個。自動下載佇列遇到失敗的下載會重試，使用者取消的則離開佇列；佇列每開始一個下載前都重新讀取來源設定，抓取中途關閉自動下載、停用或刪除來源後，不再開始後面的下載。
- 測試：
  - 真 aria2：舊任務只有 aria2 自存的種子時，三批全部完成；找不到種子時任務失敗但保留資料，恢復連結後重試完成，第一批不重複入庫，同一個種子另有下載時拒絕重試；建立匯入失敗時，中間批與最後一批都不前進、不刪檔，恢復後完成。
  - 規劃與匯入：WAV 依轉檔空間分開兩批、整軌 FLAC 與 APE 的估算、最後一批帶上 CUE、info hash 解析。
  - 預算：未寫入的預留不可重複使用保留空間；`Reclaim` 也作用於 hold；單獨進行大型輸出；轉檔等待空間後完成。
  - RSS：四種下載狀態的顯示、失敗重試與取消重新下載、早先成功不被後來的失敗蓋過、佇列遇到失敗與取消、佇列中途關閉自動下載。

### 審查修正：垃圾桶、播放時間、CUE 切點與重做（2026-10-03）

- Drive 垃圾桶與重新匯入（#44，遷移 18：`drive_trash.done_at`）：「移到垃圾桶」（背景重試與刪除當下的即時處理都走 `library.TrashFile`）與「重新引用同一個 Drive 檔案」（`MarkVerified`）共用一把鎖。
  - 移到垃圾桶前，在鎖內確認這筆待處理仍在、而且沒有資產再用這個檔案；有的話直接取消，不刪。
  - 重新引用時取消待處理的刪除；如果檔案在一小時內才被移到垃圾桶，拒絕引用（`ErrInTrash`），該項目重試時會重新上傳。
  - 移到垃圾桶的記錄保留一小時。之後再刪除同一個檔案時會重新記為待處理。
- 播放時間（#8，遷移 19：`plays.client`）：每個回報帶上登入 session 的雜湊（不存 token）。
  - 裝置的時鐘時差，取這台裝置近 12 小時內回報「收到時間減產生時間」的最小值，包含這次的回報。延遲不會是負的，所以最小值最接近真實時差。
  - 回報時間 = 產生時間 + 時差，不晚於收到時。一次播放的第一個回報即使在網路上延遲，也保留它產生的時間，不會變成「最近播放」。
  - 晚到但較早的回報（seq 較小）不改位置和更新時間，但可以把開始時間往前修正。
- CUE 切點（#19，遷移 20：`import_sources.cut`、`import_items.source_cut`）：分軌的每首以「CUE 軌號＋在整軌中的取樣範圍」作為來源身分。CUE 修正切點後會重新切；切點相同的歌曲仍判為已存在；同一份 CUE 再匯入仍是重複。這次之前記錄的來源沒有切點，第一次重匯入時會切一次並補上記錄。
- 重做與部分撤回（#20）：判斷撤回是否會蓋掉較新的修改時，逐欄位計算每筆修改是否仍有效。
  - 從最新的修改往回看：有效的撤回會讓它撤回的那筆修改失效；被重做（撤回的撤回）的撤回不再有效，原修改恢復有效。
  - 部分撤回沒寫到的欄位，原修改仍有效。
- 測試：
  - 重新匯入後不刪檔；舊的待處理在沒有資產時取消；移到垃圾桶進行中時，重新引用會等待並被拒；過期後可以再次記為待處理。
  - 延遲的首個回報不會變成最近播放；seq 2 先到時開始時間會修正；時鐘快 5 分鐘的裝置不會跑到未來，也不會蓋過其他裝置較新的回報。
  - 真 FFmpeg：同一份 CUE 重複；修正切點後第 1、2 首重新入庫、第 3 首重複；修正後的 CUE 再匯入為重複。
  - 部分撤回、撤回再重做之後，撤回較早的修改仍保留更正；連續撤回與重做後可以正常撤回。

### 使用者回饋：passkey、線上歌詞、佇列拖曳、捲動條、版面（2026-10-03）

- Passkey（遷移 21：`passkeys`；新套件 `internal/webauthn`）：
  - 設定頁「帳號 → Passkey」先輸入目前的密碼，再由裝置建立 passkey（可探索憑證、要求使用者驗證：指紋、臉部或螢幕鎖定）。之後登入頁才會出現「使用 passkey 登入」，密碼仍可使用。
  - 驗證全部自行實作，沒有新增依賴：CBOR（只接受定長、限制深度與長度、拒絕重複鍵）、authenticator data（rpId 雜湊、使用者在場與驗證旗標）、COSE 公鑰（ES256、EdDSA、RS256 ≥ 2048 位元）、clientData 的 type、challenge、origin 與跨站框架、簽名與計數器（使用中的計數器必須遞增）。
  - 不檢查 attestation（要求 `none`）。challenge 只能用一次、5 分鐘失效，同一位址最多 8 個待處理。登入失敗與密碼登入共用節流。RP 是公開網址的主機（music.ser1ka.com）。
  - 測試包含軟體驗證器（`webauthn/webauthntest`）的三種演算法、各種偽造情況、模糊測試，以及 API 的完整流程；Chromium 的虛擬驗證器實測過加入、登出、用 passkey 登入。
- 線上歌詞（新套件 `internal/lrclib`，歌詞來源多了 `lrclib`）：
  - 原本的自動關聯維持：音檔內嵌歌詞與同名 .lrc。沒有歌詞時，播放頁的歌詞分頁會向 LRCLIB 查詢這首歌，只送標題與歌手，廣播劇要按一下才查。
  - 完全吻合（標題、歌手相同，長度差 2 秒內，有歌詞）就自動套用，只在還沒有歌詞時寫入；否則列出候選讓使用者選。
  - 編輯歌詞的對話框可以「線上尋找」；來自 LRCLIB 的歌詞可以「換一個」。查詢結果在伺服器快取一小時。
- 播放佇列拖曳：每列有拖曳把手，滑鼠、觸控筆、手指都可以拖到新位置；靠近捲動區邊緣時自動捲動，觸控被取消時放回原處。歌單的拖曳排序改用同一個 `useReorder`。
- 捲動條：全站改成細的、使用主題顏色的捲動條，平常透明。區域捲動時（約一秒）或滑鼠靠近捲動條時才顯示（`scrollbars.js`）；手機保留系統的浮動捲動條；Safari 用 `::-webkit-scrollbar` 做同樣的效果。
- 首頁「未聽完的廣播劇」：列表改為固定的格線（封面、標題／專輯／進度、固定寬度的剩餘時間），每列等高、進度條等長、時間對齊；沒有專輯也沒有歌手時顯示「沒有專輯」。原因是 `button.plain` 的 `padding: 0` 蓋掉了 `.row` 的內距，所有按鈕形式的列（播放記錄、加入歌單、整理頁）都受影響，一併修正。

### 使用者回饋：VGMdb、未分類篩選、資料夾即專輯（2026-10-03）

- 起因：DUE01 等廣播 MP3 完全沒有標籤，專輯只寫在資料夾名稱裡（`ARIA/Drama CD/ARIA The STATION Due COUR.1/Disc1/DUE01.mp3`），匯入時成了沒有專輯、沒有歌手的單曲（125 首）。
- 匯入規則（計畫 §4「標籤 → 資料夾 → 檔名」補上資料夾這一層）：沒有專輯標籤的檔案依所在的專輯資料夾（Disc 1/ 等歸上一層）決定：
  - 資料夾裡的其他檔案都屬於同一張專輯 → 加入那張（例：同一套的 Disc2 已有標籤，Disc1 的廣播就成為它的第 1 碟）；
  - 整個資料夾都沒有專輯標籤 → 以資料夾名稱為專輯（去掉前面的日期 `[2024.09.30]` 和後面的格式說明 `[FLAC …]`；批次最上層與 `Music`、`新しいフォルダー` 這類名稱不算），專輯歌手由檔案的歌手決定（一位就是他，多位為 Various Artists）；
  - 資料夾裡有多張專輯 → 維持單曲。
  - 沒有歌手的檔案以專輯歌手為歌手（Various Artists 除外）。檔名沒有前置數字時，`DUE01`、`tri14` 這類「字母＋數字」取尾數為曲序。
- 已入庫的歌曲：`FolderGroups` 依匯入紀錄用同樣的規則找出可以整理的資料夾（加入同資料夾已有檔案所在的專輯，或建立以資料夾命名的新專輯，新專輯的 origin 與之後匯入同資料夾時相同，所以後續下載會進同一張）。曲庫「歌曲」分頁新增篩選「全部／未分類（沒有專輯）／沒有歌手」，首頁「待整理」直接連到篩選；在「未分類」上方提示「依資料夾整理…」，對話框可逐一勾選、修改新專輯的名稱與歌手。整理是一筆修改紀錄（收錄的新增與歌手的填入），可撤回。以正式資料的複本實測：125 首全部歸入專輯（9 個資料夾加入既有的 STATION 專輯作為 Disc 1，2 個 NATURAL 廣播劇建立新專輯），沒有歌手的歌從 125 首降到 10 首；撤回後 240 個欄位全數還原。
- VGMdb：沒有 API，且對非瀏覽器的請求一律回 Cloudflare 驗證頁（2026-10-03 實測 robots.txt 也是），非官方的 vgmdb.info 也已無法連線。不繞過驗證，所以伺服器不連 vgmdb.net：使用者在自己的瀏覽器打開專輯頁、全選複製，貼到專輯選單的「從 VGMdb 匯入…」。
  - 用戶端（`vgmdb.js`）讀取剪貼簿的 HTML（專輯名稱與其他語言名稱、型號、發行日期、製作人員、目前顯示的曲目表、封面網址），只有純文字（手機）時從文字讀出名稱、型號、日期、製作人員與曲目表。瀏覽器只複製畫面上顯示的曲目表分頁，所以要日文曲名就先切到 Japanese。
  - 使用者選擇專輯名稱（預設日文）與作為專輯歌手的製作人員（預設演出者，否則作曲者），伺服器檢查內容後產生與 MusicBrainz 相同的差異清單：曲目依碟號與曲序配對，曲序對不上但數量相同時依順序（例如 DUE14 是第二套的第 1 首），長度相差 5 秒以上的不預設勾選；多數曲目長度不符時，專輯欄位也不預設勾選（MusicBrainz 同樣適用）。VGMdb 沒有每首的歌手，專輯歌手只填到沒有歌手的歌曲。
  - 封面網址只接受 https 的 media.vgm.io（含 medium／thumb）`/albums/…`，套用時下載原尺寸，與其他封面一樣存到 Drive。修改紀錄的來源標為「VGMdb」。
- 對話框在手機上被不換行的文字撐出畫面：`.scrim` 的格線改為一欄 `minmax(0, 1fr)`，所有對話框都不會超出螢幕寬度。

### 審查修正：登入安全、任務記錄、遺失的附屬檔、資料夾來源與前端（2026-10-03，#51–#63）

- 登入（#60）：
  - 登入、passkey 登入與其 options、首次設定的路由以 Go 的 `http.CrossOriginProtection` 拒絕瀏覽器標示為跨站的請求（Sec-Fetch-Site，沒有時比對 Origin 與 Host），沒有這些標頭的原生客戶端照常；要 cookie 的登入還需 `X-Requested-With: kanade`。被拒的請求不建立 session、不用掉 challenge 或節流額度。
  - Session 自建立起 90 天到期（`auth.SessionLifetime`，cookie 同長），使用不延長；到期的在下次驗證時刪除。
  - 節流計入驗證中的請求：同一位址「近 15 分鐘的失敗＋正在驗證的」達 5 次就拒絕，密碼、passkey 登入與加入 passkey 前的密碼確認共用。
  - 密碼登入先比對，再在建立 session 的寫入交易內確認密碼雜湊沒變；改密碼與撤銷 sessions 在同一個交易。加入 passkey 的 challenge 記住確認時的密碼雜湊，儲存時在交易內再比對，並在同一交易檢查 20 個的上限。Passkey 登入在交易內重新讀取憑證（仍存在、同一帳號與公鑰），計數器有在用時必須大於目前值，更新計數器與建立 session 一起提交；同時用同一個計數器值的兩次登入只有一次成功，不用計數器（一直是 0）的同步 passkey 不受影響。
  - 撤銷政策：移除 passkey 之後不能再用它登入，但不撤銷它先前登入的 sessions；改密碼撤銷所有 sessions，並使還沒完成的、以舊密碼確認的 passkey 加入失效，既有的 passkey 保留。
- 任務中心（#58、#54，遷移 22：`downloads.cleared_at`、`import_batches.cleared_at`）：未完成的任務（進行中、等待選檔或確認、失敗、還有檔案沒存進曲庫）一定列出，已結束的分頁載入（「顯示更早的記錄」）。已結束的記錄可單筆「移除記錄」或「清除已結束的記錄」，只是不再列出：曲庫、Drive 檔案、匯入紀錄（原標籤恢復與 Drive 對帳用得到）都保留。還持有檔案的下載（失敗後保留以便重試，或匯入還有檔案沒存進曲庫）要先取消或處理匯入，才能移除。
- 遺失的檔案（#57）：匯入重試先看每個失敗檔案的來源。還在的照常重排；不在（或匯入當時就不在，之後出現的內容沒有驗證過）而另一批已有同一個檔案入庫的，標為「已存在」，下載 #8 第 3 批重複帶入的 CD1 CUE／LOG 就屬此類；來自下載的，交給下載器（`Refetch`）：檔案的 `again` 記下要重試的批次，下一批從保存的種子取回（加入時檢查既有資料），交接時不另開新的匯入，而是重試原本那一批，所以 CUE／LOG 會進它們音檔的專輯；做種中或已完成的下載先清掉已入庫的檔案再開始這一批。取不回來的（上傳暫存已刪、下載已取消、種子找不到）留在失敗，說明原因。下載的「重試」也會重試它的匯入中失敗的檔案。
- 資料夾整理（#52、#53）：資料夾依「來源」分開，來源是匯入的實際根目錄（`local_path` 去掉相對路徑：同一個下載的各批共用 `downloads/N/`，每次上傳各自一個，Drive 收件匣算一個），同名的相對資料夾不再混成一張專輯。對話框顯示來源，以 `key` 選擇。匯入時，批次最上層與名稱籠統的資料夾不是專輯資料夾：散放的檔案保留各自的標籤，不會加入同層另一首歌的專輯。
- 拖曳排序（#51）：拖曳時不再重排 DOM（原本重排會讓把手失去 pointer capture，放開事件送不回來），改為被拖的列以 transform 跟著指標、經過的列讓位，放開才改順序；事件掛在 window 上。滑鼠與觸控筆可直接拖整列（移動 6px 以上才開始，放開不會觸發播放），手指用把手。Esc、觸控取消、失去 capture、清單改變或元件卸載時放回原處並清掉計時器與 listener。
- 前端：
  - 登入頁確定能否使用 passkey（最多等 3 秒）後才一次顯示完整表單；設定頁同時讀取服務、Drive、passkey（各最多 10 秒，Drive 已連線才讀同步狀態），確定後依固定順序一次顯示，之後的重試與輪詢只更新各自的區塊（#62）。`api()` 新增 `timeout`。
  - RSS 尚未下載的列不再顯示「0」（#55）；外觀設定移除「只記在這個瀏覽器。」（#56）；VGMdb 匯入的搜尋步驟加上「用 Google 搜尋 VGMdb」（`site:vgmdb.net`，以 URLSearchParams 編碼，#63）。

### 發佈與安裝腳本（2026-10-03）

- 設定改放資料目錄的 `config.json`（`listen`、`public_url`、`aria2`），由 `kanade config set` 寫入並檢查格式；`serve` 依「內建預設 → config.json → 參數」決定。內建的公開網址改為 `http://localhost:8080`，不再寫死 music.ser1ka.com。正式環境的設定已寫進 `var/config.json`。首頁與隱私權頁以設定的網址顯示站名（`{{SITE}}`）。
- 新指令：`kanade version`（版本在編譯時以 `-X main.version` 帶入，`/status` 也回報）、`kanade backup FILE`（`VACUUM INTO`，服務執行中也一致，0600）、`kanade config`。
- CI（`.github/workflows/ci.yml`）：推送到 main 與 pull request 時執行三組檢查：Go（gofmt、vet、裝好 aria2 與 FFmpeg 後以 race detector 跑全部測試，下載與轉檔的測試不會被略過）、網頁端（`server/scripts/check-web.mjs`：每個模組能解析、具名 import 都存在且有用到；網頁端沒有建置步驟，這是唯一的把關）、腳本與 workflow（shellcheck、actionlint）。同一分支有新的推送時取消舊的執行。
- 發佈（`.github/workflows/release.yml`）：推送 `v*` 標籤時先跑 CI（workflow_call），通過後以 `CGO_ENABLED=0 -trimpath` 編譯 linux/amd64、arm64，發佈 `kanade-linux-<arch>.tar.gz`、`SHA256SUMS`、`VERSION`、`kanade.sh`；含連字號的標籤為 pre-release。手動執行只產生 artifact。
- 安裝與管理腳本（`scripts/kanade.sh`，安裝後為 `kanade-manager`），仿 OpenList 的一鍵腳本：
  - 需要 root、curl、tar、sha256sum；支援 amd64、arm64；服務用 systemd（`kanade` 系統帳號、`ProtectSystem=full`、資料目錄可寫、`KillMode=mixed` 讓 Kanade 自己停 aria2）或 OpenRC，兩者都沒有時在背景執行（以 `exec` 脫離腳本，不占住終端或管線），有 crontab 時可加 @reboot。
  - 安裝：詢問安裝位置、連線方式（網域＋反向代理，只聽 127.0.0.1；或直接用 IP，聽 0.0.0.0，但 Google 授權與 passkey 需要 https 網域）、連接埠、管理員帳號、GitHub 代理；以套件管理員安裝 aria2 與 FFmpeg（可略過）；下載後核對 SHA-256；程式屬於 root，只有資料目錄屬於服務帳號（0700）；隨機產生管理員密碼並只顯示一次；在保留資料的位置重裝時沿用原有帳號。
  - 更新：比對 `VERSION`，先備份資料庫，換上新程式後 30 秒內沒有回應就退回舊版。解除安裝預設保留資料，輸入 DELETE 才刪除（連同系統帳號）。另有狀態、啟停、記錄、重設密碼、修改網址、備份與還原（還原前先另存目前的資料庫）。
  - `KANADE_YES` 等環境變數可不經詢問安裝；`KANADE_RELEASE_URL` 可改從其他位置下載（測試用）。
  - 驗證：shellcheck、actionlint；本機以 root 在背景模式實測安裝（自動與互動）、啟動、狀態、以印出的密碼登入、重設密碼、備份、更新、更新失敗自動退回、在保留資料的位置重裝、從選單還原、解除安裝（保留與刪除資料）；systemd 單元以 `systemd-analyze verify` 檢查（這台容器沒有 systemd，未實際啟動）；OpenRC 未測。
- 重啟服務會讓做種中的任務結束（aria2 的 session 不保存做種中的任務），更新時的提示已說明；尚未修正。
- 刪除 `spikes/p0-drive`（P0 的一次性實驗，結果在 `docs/p0-checklist.md`）。

### 安裝腳本：沒有 aria2／FFmpeg 套件的系統（2026-10-03）

- 回報：在 dnf 系統（RHEL 系）上安裝時，`dnf install aria2 ffmpeg` 找不到套件（aria2 在 EPEL、FFmpeg 在 RPM Fusion，預設都沒啟用），兩者都沒裝；Amazon Linux 2023 也一樣。
- 先用套件管理員逐一安裝（一個找不到不再拖累另一個，例如 Fedora 有 aria2 沒有 FFmpeg）；仍缺的改下載靜態版到 `<安裝位置>/tools`，正是 Kanade 本來就會找的位置（`tools/aria2/aria2c`、資料目錄旁的 `tools/ffmpeg/bin`），不必另外設定：
  - aria2：abcfy2/aria2-static-build 1.37.0（musl 靜態），與正式環境同一個檔案；aria2 官方沒有 Linux 版，SHA-256 寫死在腳本裡。
  - FFmpeg：BtbN/FFmpeg-Builds 的 8.1 分支最新版（glibc 2.28 以上），以同一 release 的 `checksums.sha256` 核對；每天重建，無法固定版本。下載約 150 MB，只取出 ffmpeg、ffprobe 與授權檔（約 330 MB）；剩不到 600 MB 時不下載。
  - 在安裝位置裡下載與解壓縮（`/tmp` 可能在記憶體裡）；沒有 xz 時用 python3 解 .tar.xz，沒有 unzip 時用 bsdtar 或 python3；下載後先試執行，不能執行就不裝。解除安裝時一併移除。
- 新指令 `kanade-manager tools`（選單 14）：安裝時略過或當時沒裝成功的，之後補裝，再詢問是否重新啟動；同時把 kanade-manager 換成執行的這份腳本，讓舊版裝好的機器也知道這些檔案。狀態頁顯示實際使用的路徑。
- 驗證：本機以背景模式實測安裝時改用靜態版（套件管理員那題答否）、Kanade 記錄 `aria2 ready`／`ffmpeg ready` 指向 tools、`tools` 在兩者都有時與缺 aria2 時的行為與重新啟動、kanade-manager 由 v0.1.0 換成新版、解除安裝後 tools 與安裝位置都已刪除。xz 的路徑（`tar -xJ`）與 arm64 未實測。

### 網頁端設定 OAuth 用戶端（2026-10-03）

- 回報：新安裝的機器在設定頁按「連線 Google Drive」只得到 `no OAuth client configured`，沒有任何指引。後端早有 `POST /drive/client`，但網頁端沒有入口，只能用指令列。
- 設定頁的 Drive 區塊依狀態顯示：
  - 沒有用戶端：說明為什麼需要，按鈕開啟「設定 OAuth 用戶端」。對話框列出 Google Cloud 的步驟並附上連結：建立專案、啟用 Drive API、Google Auth Platform、目標對象改正式版、建立「網頁應用程式」用戶端。顯示這台應登記的重新導向 URI，可以複製；http 下沒有剪貼簿 API，改用 `execCommand`。可以選擇 Google 下載的 JSON 自動填入，也可以手動貼上 ID 和密鑰；ID 結尾不是 `.apps.googleusercontent.com` 時先擋下。
  - 有用戶端：顯示用戶端 ID，提供連線與「更換用戶端」，並先說明「Google 尚未驗證這個應用程式」要怎麼繼續。
- 沒有 https 網域時也能連 Drive：Google 只把瀏覽器導回 https 網域或 localhost。`gdrive.RedirectFor` 依公開網址決定：
  - https 加網域名稱，或本機：用自己的 `/oauth/google/callback`，維持原本的流程。
  - 其他情況（http 或 IP）：用 `http://localhost/oauth/google/callback`。連線改在對話框完成：在新分頁開 Google 授權頁，授權後把瀏覽器停住的 localhost 網址貼回（`/drive/auth/paste`）。這和 P0 驗證過的做法相同。
  - `/drive` 的狀態加上 `redirect_uri`、`paste`、`client_id`。
- 換成另一個用戶端時刪除舊的 token：token 只能由取得它的用戶端刷新，留著只會讓背景刷新一直失敗；改為顯示「尚未連線」，請使用者重新連線。存回同一個用戶端不受影響。
- 安裝腳本與 README 的說明隨之修改：IP 模式不再說「要設好網域才能連 Drive」，改為說明要貼回網址，以及密碼以 http 傳送、不能用 passkey。修改公開網址後提示到設定頁查看新的重新導向 URI。
- 驗證：
  - gdrive 測試：各種公開網址的重新導向、狀態欄位、換用戶端後要重連（重新開啟後也是）。
  - 用 headless Chromium 在公開網址為 IP 的測試實例走完整流程（桌面與手機寬度）：錯誤的 ID 被擋、選 JSON 自動填入、授權連結帶 localhost 的 redirect_uri 與用戶端 ID、貼上偽造的網址顯示中文錯誤。
  - 沒有用真的 Google 用戶端走完授權：`http://localhost/...`（不帶連接埠）的登記沒有實測；P0 實測過的是 `http://localhost:8080/...`。

### 審查修正與使用者要求的功能（2026-10-04，#64–#83）

- Drive（#64、#70）：`/drive` 讀不到帳號時仍回本地狀態與 `account_error`、`reconnect`（Google 回 `invalid_grant`），設定頁保留重新連線與更換用戶端；更換用戶端與刪除舊 token 在同一交易。
- 線上歌詞與代理錯誤（#79、#80）：`api.js` 把非 JSON 的回應（Cloudflare 的 HTML 502）轉成「暫時連不上伺服器（HTTP 502）」並保留 `Retry-After`，`ErrorBox` 倒數；LRCLIB 只快取通過解析與結構檢查的回應。
- 安裝腳本（#69、#71）：更新先把新程式寫成 `kanade.new` 並確認版本，每一步檢查結果，rename 換入，回不去就退回；還原先複製並比對、停好服務才 rename，之後才刪 WAL。暫存目錄記在全域陣列，由每個 shell 自己的 EXIT trap 清除。
- 任務記錄（#68）：最新 50 筆定時更新；更早的用 `GET /tasks/older?kind&before=ID` 一次讀一頁；之後的更新改帶 `since_*`＋`changed`，不會縮回或出現缺口。
- 匯入重試（#65–#67）：重試找出遺失檔案的來源（轉檔的原始檔、切軌的映像、解壓的壓縮檔），來源還在就在下一次分析重做（產物回到原本的項目、保留方案），來源也不在就請下載重新抓取來源。重新抓回的檔案保持 `Again` 標記（`fetched`），直到原批次真的排回佇列；失敗或重新啟動後由輪詢重做交接。「已由其他匯入存進曲庫」改為檢查曲庫現在仍有可用的副本。
- 專輯身分（#81）：`album_scopes`（遷移 0023）記住每個下載的「專輯資料夾＋專輯標籤」對應哪張專輯，之後的批次加入它並更新推導的專輯歌手（不同的推導 → Various Artists；標籤寫明的優先）；沒有 ALBUMARTIST 的檔案重匯入時以專輯標籤、碟號、曲號找回原本的收錄。`import_batches.root` 記下批次的根目錄。
- 服務設定（#74、#75、#77）：`internal/settings` 把資源、下載、Drive 同步三組設定以 JSON 存在 settings 表，儲存時檢查並立即套用（快取 `SetBudget`、暫存 `SetLimits`、磁碟守衛 `SetReserve`、aria2 `changeGlobalOption` 並寫進每次啟動的設定、同步 `Configure`、收件匣 `SetInboxSettle`）；`serve` 的資源參數若有指定，該次啟動以參數為準。
- 做種（#75）：做種何時停止改由 Kanade 依設定判斷（分享率以累計上傳量計算，`uploaded_before`，遷移 0024）。aria2 重新啟動後沒有做種中的任務時，若仍要做種，用保存的 torrent 重新加入並驗證後繼續——重新啟動服務不再讓做種結束。同時下載數 1–5，仍經過共用暫存預算。
- 帳號（#76）：本人改密碼（以目前密碼確認、與登入共用節流、結束所有登入、保留 passkey），列出／結束自己的登入；回應不含 token。
- 播放（#72、#73、#78）：`GET /tracks/random` 以歌曲為單位從全曲庫隨機挑選（只挑可播放的、音樂與廣播劇分開、避開剛播的）。佇列可以「自己往下接」（`radio`）：全曲庫隨機，或播完後自動接續；挑來的歌標示 `auto`，使用者加入的排在前面，晚到的回應以世代號丟棄。播放設定（模式、隨機範圍、自動接續、各類型點歌是否續播、預載）存在瀏覽器。
- 曲庫整理（#82、#83）：專輯可命名區段（`album_sections`，遷移 0025；修改紀錄欄位 `sections`）。批次操作（合併、批次修改、移除、放進專輯、收藏、永久刪除）在 `library/batch.go`，先檢查所有 ID、一次一筆修改紀錄；計畫（`Plan`：搬移、新增、分區、清空的專輯）先預覽再套用。下載可選分組（`downloads.grouping`：依標籤、每個資料夾、合集）；合集的分區與曲序由整個選取清單算出（`CollectionLayout`），每一批帶著自己檔案的位置入庫；已入庫的下載用 `CollectionPlan` 整理成合集。網頁端：`selection.js`（選取、Ctrl／Shift、全選已載入）、`views/batch.js`。
- 驗證：每項都有以真實 SQLite、Importer、aria2、FFmpeg 的測試（修正前的程式會失敗的都確認過），完整測試含 `-race` 通過；網頁端以 headless Chromium 在正式曲庫的複本上走過（桌面與 390px），包括把 Umineko 這個下載整理成 208 首、9 個分區的合集。遷移 0023 也在正式資料庫的複本上跑過。

### 分類、我的聆聽、智慧歌單、書籤與睡眠定時（2026-10-04，#84–#98）

- Bug（#84–#91）：做種中的任務不佔下載名額（aria2 `bt-detach-seed-only`）；舊的 `album_scopes` 補上 `derived`（遷移 0026）；「每個資料夾一張專輯」不受分批影響（遷移 0027）；分組與合集設定進修改紀錄、可撤回（遷移 0028，`album_scopes` 改用 AUTOINCREMENT 以免撤回後 ID 重用）；批次操作記住選取時的項目；補歌等待時暫停不被蓋掉；安裝腳本的更新與退回確認是新程式在回應。
- 分類（#92，遷移 0029）：`categories`、`album_categories`；曲庫「分類」分頁、分類頁（搜尋、排序、選取、移出）、專輯頁的分類標籤、批次分類。
- 我的聆聽（#93，遷移 0030）：每次播放回報增加的聆聽時間，往回分到它所在的 15 分鐘區間（`listening`），所以任何時區的「一天」都能正確加總；舊的播放紀錄啟動時回填一次，標為推算。頁面：概覽、年度熱力圖（單色色階，經過對比與色覺檢查）、期間排行與趨勢、每天明細、匯出與清除；每張圖都有表格檢視。
- 書籤與睡眠定時（#98，遷移 0031）：書籤記在音檔與位置上，音檔換版本時提示；睡眠定時（分鐘或播完這首），最後 20 秒漸弱，只在這台裝置有效。
- 智慧歌單（#96，遷移 0032）：規則（分類、專輯、歌手、類型、收藏、播放次數、最近播過、從沒播過、聽完過；全部或任一符合）、排序、首數或分鐘上限；可照目前結果播，或依規則一直播。
- 驗證：每項都有真實 SQLite 的測試（含遷移在 v22 資料庫上升級），`-race` 通過；網頁端以 headless Chromium 在正式曲庫的複本上走過桌面與手機寬度。依序部署為 v0.1.5–v0.1.9。

### 審查修正（2026-10-04，#101–#124）

- 聆聽回報（#102）：單次回報超過 24 小時直接拒絕；同一次播放的聆聽時間不超過開始播放以來的實際時間（容許 2 分鐘時鐘誤差），首次回報不超過音檔長度，播放紀錄與分桶總和保持一致。
- 統計（#108、#109）：歌手排行依歌曲連結的歌手身分分組，可開歌手頁；專輯排行用遞迴查詢解完整合併鏈（有循環與缺失防護），撤回合併後恢復。
- 續播（#105）：`/playlists/{id}/next` 與 `/tracks/random` 的 `not` 改為由新到舊（排隊中、正在播、最近播過）；規則只符合排除的歌時繞一輪，最近那首只在唯一時重複。
- 播放器（#103、#104、#106、#110）：睡眠定時到期會讓等待中的補歌不開播；舊的重試只對同一個佇列、沒有暫停過時接著播；`resumeMs: 0` 是明確的開頭，不被自動續播覆蓋；手機播放欄的定時只顯示圖示或短倒數。
- 線上歌詞（#122、#123）：依序查詢、找到有文字的歌詞才停；拆「曲名 / 歌手」與關鍵字查詢；只有空白的記錄不算候選也不算吻合；對話框可調整搜尋。
- 介面（#101、#107、#111–#121、#124）：選取按鈕與分類頁首預留位置；預覽只接受目前條件的回應；分類載入失敗有錯誤與重試；批次分類改為「原樣 → 全部放入 → 全部移出」並列出差異；核取框跟隨主題；熱力圖依寬度調整格子、提示移到不捲動的外層、換年保留鍵盤入口；時區改在設定頁（跟隨瀏覽器或手動）；選檔按鈕改用可聚焦的 button（`FilePick`）；placeholder 用主題色；上傳中每個檔名只出現一次。
- 驗證：新增的單元測試與修正前會失敗的確認；瀏覽器照各 issue 的重現步驟實測。部署為 v0.1.10；因 v0.1.10 標籤的 CI 格式檢查失敗，release 由 v0.1.11 發佈（程式相同）。

### 安裝腳本：SysV、開機自動啟動、管理腳本更新（2026-10-04）

- 服務管理多了 SysV：PID 1 是 init、有 `/etc/init.d` 與 `rc2.d` 的機器（sysvinit，或開機時執行 rc 連結的虛擬機）寫入 `/etc/init.d/kanade` 與 rc 連結；腳本以服務帳號在背景啟動、pid 存在 `/run`，沒有被回收的殭屍程序算已停止。
- `kanade-manager autostart [on|off]`（選單 15）：顯示並開關開機自動啟動（systemd enable、OpenRC rc-update、SysV rc 連結、cron @reboot）；服務檔被刪掉時重新寫入。安裝與狀態會顯示是否已開啟。
- `update` 會一併更新 kanade-manager：先比對 release 的 `SHA256SUMS`（從 v0.1.12 起列入 `kanade.sh`）並確認 bash 能完整讀取；Kanade 已是同一版時只更新腳本、不重新啟動服務。
- 驗證：以腳本自己的函式在暫存目錄測試（SysV 啟動、停止、重複啟動、殭屍、rc 連結、autostart 開關與重寫服務檔、管理腳本的校驗與截斷），以 root 實測 runuser 與 su 兩種啟動方式；原有的更新與退回測試照樣通過。

### 審查修正（2026-10-04，#125–#133）

- 播放器（#125、#126、#127）：`load` 累加載入次數，等待補歌的舊回應在期間換過歌時不再前進或暫停；保存的佇列帶實例 id，進度記錄 id、qid、asset，佇列也附寫入當下的進度，恢復時三者都符合才用進度 key，否則用佇列自帶的那份（跨分頁 localStorage 寫入有延遲，兩個 key 可能分屬兩個分頁）；最後有動作的分頁擁有保存狀態，清空佇列只刪自己的、登出一律刪；音訊真的開始播放才上報 `/plays`，恢復後沒播放不影響首頁續播。
- 線上歌詞（#128）：斜線後綴只是猜測，只有等於歌曲的 artist 時才把去掉後綴的曲名當成精確比對。
- 歌詞分頁（#132、#133）：已快取的歌詞直接顯示；等待區塊與歌詞框同高；搜尋、自動套用與取回歌詞是同一段等待，只有需要手選、沒找到或錯誤時才顯示「沒有歌詞」；自動套用每頁每筆一次，失敗留在清單。
- 統計（#129）：專輯合併鏈只從期間內出現的專輯開始解（走 bucket 索引）；日統計與時段趨勢不解專輯。
- 管理腳本（#130、#131）：SysV 的 `alive()` 比對 `/proc/PID/cmdline`，PID 被重用時不送信號；`install_manager` 本身驗證腳本（校驗碼、語法與入口），所有安裝入口共用，不通過就保留原本的 manager。
- 部署為 v0.1.17；`TestHeardTimeIsBounded` 在整刻鐘附近執行時聆聽分成兩列而失敗，release 由 v0.1.18 發佈（程式相同，測試已修）。

### HTTP/HTTPS 直接下載（2026-10-04，遷移 33：`downloads.kind`）

- 新增下載的網址是 http(s) 時先探測（只讀開頭）：`application/x-bittorrent` 或以 bencode 字典開頭的照舊當 .torrent；其他當成直接下載的檔案。
- 直接下載只收匯入吃得下的東西：音檔或 .zip（副檔名，或依 Content-Type 補上）；網頁（text/html）、其他類型、沒有 Content-Length 的都在加入時拒絕並說明原因，不留記錄。檔名取 Content-Disposition，否則取轉址後網址的最後一段，去掉路徑、控制字元與開頭的點，最長 200 bytes。
- 加入後直接排隊（只有一個檔案，沒有選檔）；同一網址還在進行時不能重複加入。和 torrent 的一輪相同：排程取得暫存空間後才以 `addUri`（暫停、指定檔名、`continue`、不能續傳時從頭、不改名）加入 aria2，下載完交給匯入，沒有做種，匯入完成後清掉檔案。
- aria2 弄丟任務時依連結重新加入並續傳；失敗可以重試（從連結重新加入）。重試與重抓的重複檢查改用 `twin`：torrent 依 info hash，直接下載依網址（避免空的 info hash 互相比對）。
- 暫停恢復：只有已交給匯入的下載才回到做種；在 100% 但還沒交接時暫停的，恢復後回到這一輪，交接後才結束。
- RSS 不受影響：RSS 自己抓連結並以 .torrent 內容加入。
- 驗證：真實 aria2＋本機 HTTP 伺服器：zip 與單一音檔入庫並清除檔案、Content-Disposition 的中文檔名、重複網址、沒有副檔名的 .torrent 連結仍走 torrent、網頁／不支援的類型／沒有大小／404 被拒絕且不留記錄、限速下載中弄丟 aria2 任務後以 Range 續傳、失敗後重試。

### 封面原圖檢視與鍵盤快捷鍵（2026-10-04）

- 封面（#134 第一部分）：專輯頁與正在播放頁的封面（以及兩者的「更多」選單）開啟全螢幕檢視：不裁切、適合視窗；先顯示 600 px 的圖，原圖（`/covers/{id}?size=0`）解碼完才替換並顯示尺寸；滾輪、雙指、按鈕或 + − 縮放，雙擊／雙點或 0、1 在適合視窗與 1:1 之間切換，拖曳移動且不會移出畫面；深淺背景（記在瀏覽器）。Esc、關閉鈕或返回（手機返回手勢，`history.pushState`，不換頁）關閉，頁面捲動與播放不受影響；原圖抓不到時保留小圖並可重試。沒有封面的專輯在原位提供「加上封面」。內頁圖庫（第二部分）尚未做。
- 鍵盤快捷鍵（`js/shortcuts.js`）：空白鍵／K 播放暫停、← → 倒退快轉 5 秒、Shift+← → 上下一首、↑ ↓ 音量 5%（靜音時 ↑ 回到原音量、↓ 不動）、M 靜音、Esc 收起正在播放、? 顯示說明（設定 → 播放也有入口）。輸入文字、對話框、選單或封面開著時不作用；焦點在按鈕時空白鍵與 Enter 留給按鈕，在滑桿時方向鍵留給滑桿。

## 使用方式（開發環境）

```bash
# 啟動（工作目錄 server/）；日誌寫到 var/logs/kanade.log，server.log 只留啟動前的輸出。
# 公開網址與監聽位址在 var/config.json（kanade config 查看、kanade config set 修改）。
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

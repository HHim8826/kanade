# 後端設計

現在的架構：程式怎麼分、資料怎麼存、API 有什麼。依據 `plan-v0.5.md` 與 `decisions.md`。每一版做了什麼、為什麼那樣改、怎麼驗證的，記在 [`changelog.md`](changelog.md)。

## 程式與部署形態

- 單一 Go 執行檔 `kanade`（`server/`），純 Go、不需 cgo，可交叉編譯 amd64／arm64。
- `kanade serve --data DIR --listen 127.0.0.1:8080`：主服務。開發期經 Cloudflare Tunnel 對外為 `music.ser1ka.com`。
- 主服務自行管理 aria2 子程序（D5）：產生設定與 RPC 密鑰、只綁 loopback、異常退出時退避重啟。
- 首頁 `/` 與 `/privacy` 由主服務提供（D4），取代 `deploy/landing`；名稱依站名產生（見「站名、圖示與分頁標題」）。

### 資料目錄

```
DIR/
  db.sqlite           曲庫、任務、設定、憑證（檔案權限 600）
  staging/            匯入暫存（上傳中的分塊、整理副本）
  downloads/          aria2 下載與做種資料
  cache/              串流快取（D7）
  thumbs/             封面縮圖
  lite/               省流模式的 Opus 檔（上限 2 GB，以原檔的 SHA-256 命名，重啟後保留）
  aria2/              aria2 設定、session 檔、日誌
  logs/               服務日誌 kanade.log（10 MB 輪替、保留 5 份，P2-6）
```

### Drive 內的位置

所有寫入都在平台根資料夾 `Kanade` 之下（D1）：

- `library/<專輯歌手>/<專輯>/`：音檔。路徑取第一次匯入時的資料，之後不因修改資料而搬移；資料庫才是唯一依據。
- `covers/`：封面。
- `sidecars/`：CUE、LOG 等旁附檔。
- `inbox/`：收件匣（D6，P2-6）。放進來的音樂就地歸檔進 `library/`；曲庫已有的移到 `inbox/重複`，處理完剩下的移到 `inbox/已處理`。

其他存放位置（#99、#100、#95，見「儲存位置」）用同樣的 `library/`、`covers/`、`sidecars/`，放在資料夾、bucket 的 prefix 或選定的 OneDrive 資料夾之下。資料夾與 bucket 另有 `.kanade-store`（確認是同一個位置、NAS 有掛載）與 `.kanade-trash/YYYYMMDD/`（刪除的檔案，30 天後清掉）；OneDrive 刪除時進它自己的回收筒。曲庫裡檔案的位置（`file_ref`）是 Drive 的 file ID，或 `<種類>:<位置 ID>:<鍵>`：資料夾與 bucket 的鍵是相對路徑，OneDrive 的鍵是 item ID（改名、搬移後不變）。

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
| `ffmpeg` | FFmpeg 子程序：轉 FLAC、依 CUE 分軌、PCM MD5 驗證（P2-4）、省流模式的 Opus |
| `identify` | MusicBrainz 查詢與差異比對（P2-2） |
| `bangumi` | Bangumi（bgm.tv）作品與音樂條目查詢、帳號連結與收藏（#94） |
| `rss` | RSS／Atom 解析、輪詢、規則、自動下載（P2-5） |
| `netguard` | 外來網址（RSS、其中的連結、貼上的下載連結）不連到本機與區網：檢查解析出的每個位址並直接連那個位址（防 DNS rebinding）；區網位址可在下載設定開放，本機與 link-local 一律不行 |
| `stream` | D7 播放快取（硬性容量上限，放不下時直接轉送） |
| `lite` | 省流模式：播放時把原檔轉成 Opus 128k 存起來，預載下一首時先轉好 |
| `drivesync` | Drive 變更與完整對帳、基準對帳、收件匣排程（P2-6） |
| `storage` | Drive 以外的儲存位置：資料夾與 NAS、S3／R2（自己做 SigV4）、OneDrive（Graph）；讀、寫、垃圾桶、用量（D12） |
| `storesync` | 和儲存位置對帳：只讀來源的掃描與自動匯入、存放位置的檢查 |
| `mover` | 在 Google Drive 與存放位置之間搬移檔案：讀出、核對 SHA-256、寫入、再核對，舊的之後才進垃圾桶 |
| `backup` | 資料庫備份：每天一份，存到 Google Drive 或任一存放位置，保留最近幾份（#161） |
| `thumbs` | 封面縮圖與原圖 |
| `loudness` | 以 FFmpeg 量每首歌的響度（EBU R128），供音量平衡（#136） |
| `presence`、`discord` | 各瀏覽器正在播什麼（接續播放與遙控，#138）；Discord 狀態（#135） |
| `clientip` | 請求的來源位址（登入節流用），只在設定的反向代理之後才相信轉送標頭 |
| `diskguard` | 低磁碟監控：清快取、暫停下載、拒絕新上傳／下載（P2-6） |
| `logfile` | 日誌檔輪替（P2-6） |
| `proc` | 讓 aria2、FFmpeg 子程序隨主程序結束（P2-6） |
| `settings` | 網頁設定頁可改的服務設定（資源、下載、Drive 同步），存在 settings 表、立即套用（#74、#75、#77） |
| `site` | 站名與圖示：存在 settings 表（圖示是 512px PNG），產生分頁、iPhone、PWA 用的各種大小 |
| `staging` | 匯入暫存的容量預算：上傳、下載與匯入共用，放不下時排隊 |
| `uploads` | 客戶端分塊上傳（B4） |
| `lrclib` | LRCLIB 線上歌詞：搜尋、排序、快取、忙碌時重試 |
| `webauthn` | passkey 註冊與登入 |
| `web` | 內嵌的網頁端（`static/`，Preact＋htm，不需建置），於 `/app/` 提供；首頁 HTML 與 manifest 依站名與圖示產生 |
| `api` | HTTP 路由、驗證中介層、頁面 |

## 資料模型（計畫書 §4 三層）

| 表 | 內容 |
|---|---|
| `assets` | 音檔：`sha256`＋`size` 唯一、格式、取樣率、位元深度、聲道、時長、音訊 MD5、檔案位置 `file_ref`（Drive file ID 或 `<種類>:<位置>:<鍵>`）、狀態 |
| `tracks` | 錄音版本：曲名、顯示用歌手、版本說明；作曲、作詞、編曲的原文（D13）；`variant`（伴奏或短版，從曲名與版本說明判斷，隨機播放時可略過） |
| `track_assets` | 錄音版本與音檔的多對多（同錄音的不同格式） |
| `albums` | 專輯版本：名稱、專輯歌手、日期、型號、封面 |
| `album_entries` | 專輯收錄：專輯、track、asset、碟號、曲序；收錄專屬資料存在這裡，不改寫共用音檔 |
| `artists` | 歌手，依名稱唯一；`kind` 是人或角色（拆過「角色（CV：聲優）」才知道） |
| `credits` | 歌曲的每個名字（D13）：角色（演唱、配音演唱、作曲、作詞、編曲）、順序、歌手、配音時飾演的角色；由歌曲的原文和 `artist_splits` 推導，原文改了就重算 |
| `artist_splits` | 使用者對一個名稱字串怎麼拆的決定：`parts`（JSON；`[]` 是保持完整，NULL 是沒決定或已撤回）；之後匯入的同名字串照做 |
| `import_batches`、`import_items` | 每次匯入與每個檔案：原始路徑、原始標籤（保留原始位元組）、偵測到的編碼、狀態、錯誤；來源的檔案另有 `ref`（原處的位置）與 `fetch`（雲端來源：掃描時看到的版本、雜湊與同資料夾的圖片和 LRC） |
| `downloads` | BT 任務：aria2 gid、infohash、狀態、檔案清單與選取、做種資訊 |
| `upload_sessions` | 進行中的可續傳上傳：Drive 的 session URI、bucket 的分段上傳（upload ID 與已完成的分段）、OneDrive 的上傳工作階段；`store` 是去處 |
| `drive_folders` | 路徑到 Drive 資料夾 ID 的快取 |
| `search_index` | FTS5 trigram：曲名、歌手、作曲、作詞、編曲、專輯名、歌手名、別名；`rowid` 是 ID×4＋種類。版本變了（`indexVersion`）啟動時重建 |
| `users`、`sessions`、`settings`、`credentials` | 帳號、登入 token（只存雜湊）、設定、OAuth token |
| `plays` | 播放紀錄（D9）：session、音檔、歌曲、從哪張專輯播放、位置、實際聽的時間、是否計次、是否聽完 |
| `favorite_tracks`、`favorite_albums` | 收藏 |
| `playlists`、`playlist_items` | 歌單；條目有獨立 ID、歌曲、可選專輯與音檔版本、位置，允許重複。智慧歌單的 `rules`（JSON）不為空，內容由規則即時挑選（#96） |
| `lyrics` | 每首歌曲一份歌詞：來源（內嵌／LRC／手動）、是否有時間軸 |
| `aliases` | 歌手、專輯、歌曲的其他名稱，建入搜尋索引 |
| `edit_groups`、`edits` | 修改紀錄：每個操作一組，逐欄記舊值與新值，供撤回 |
| `sidecars` | 與專輯一起保存的 CUE、LOG：檔案位置、SHA-256、所屬專輯 |
| `import_sources` | 轉換或分軌的來源檔（SHA-256＋大小）與由它產生的音檔，用於再次匯入時略過 |
| `rss_sources`、`rss_items` | RSS 來源（網址、間隔、規則、自動下載、認證、輪詢狀態）與條目（GUID、下載連結、info hash、大小、做種數、對應的下載） |
| `passkeys`、`oauth_states` | passkey（公開金鑰、名稱、最後使用）；OAuth 授權進行中的 state |
| `uploads` | 客戶端上傳：群組、路徑、大小、已收到的位元組、SHA-256、狀態 |
| `file_trash` | 移到垃圾桶（Drive、OneDrive 的回收筒，或資料夾與 bucket 的 `.kanade-trash`）的檔案，供撤回時找回；還沒移成功的在背景重試 |
| `stores` | 儲存位置（#99）：種類（`local`、`s3`、`onedrive`）、名稱、用途（`managed` 存放位置、`source` 只讀來源）、設定（JSON，不含祕密）、`secret`（金鑰、用戶端密碼與 refresh token）、上次檢查的狀態、錯誤與用量、來源是否自動匯入、上次掃描、來源匯入的專輯放進的分類（`category`，分類刪除時清空）。ID 是 AUTOINCREMENT，絕不重複使用 |
| `source_files` | 只讀來源上次掃描看到的檔案：鍵、路徑、大小、版本（修改時間、ETag 或 cTag）、讀過的 SHA-256、狀態（排入、已匯入、略過、失敗、曲庫不要了）、不見的時間 |
| `source_covers` | 只讀來源裡被當成封面的圖片（#221）：鍵、路徑、大小、版本、SHA-256；掃描時和歌曲一樣追蹤搬動、換內容與刪除 |
| `album_sections` | 專輯的命名區段（例如合集裡的 Episode 1），依碟號（#82） |
| `album_scopes` | 下載的「專輯資料夾＋專輯標籤」對應的專輯，跨批次維持同一張；`derived` 表示專輯歌手是推導的（#81、#85、#86、#88） |
| `categories`、`album_categories` | 分類（作品、系列或任何分組）與專輯的多對多（#92） |
| `listening` | 實際聽的時間，每次播放依 15 分鐘分桶：歌曲、從哪張專輯、類型、毫秒、該桶是否計次、是否為舊紀錄推算（#93） |
| `bookmarks` | 書籤：音檔、位置、名稱、備註；音檔換版本時標示 `moved`（#98） |
| `covers` | 封面圖：SHA-256、大小、檔案位置 |
| `album_images` | 專輯的內頁圖庫（#134）：掃圖、背面、歌詞本、光碟、側標，種類、順序、所屬碟；隱藏的保留，免得再次匯入時又出現 |
| `works`、`album_works`、`track_works` | 作品（動畫、遊戲、書，資料來自 Bangumi）與專輯、歌曲的關聯（#94） |
| `album_subjects`、`bangumi_links` | 專輯在 Bangumi 的音樂條目；連結的 Bangumi 帳號（#94） |
| `loudness` | 每個音檔的響度與峰值（#136） |
| `devices`、`discord_links` | 瀏覽器的名稱（#138）；連結的 Discord 帳號與要顯示的狀態（#135） |
| `moves`、`move_items` | 檔案搬移的工作與逐檔進度 |

精確去重鍵是 `sha256`＋`size`（唯一約束），上傳完成以 Drive 回傳的 `sha256Checksum` 驗證（P0 第 1 節）。

## 匯入狀態

`import_items.state`：`pending → parsing → hashing → uploading → verifying → published`，另有 `duplicate`（音檔已存在，只建立收錄）、`failed`、`skipped`（不支援的格式）、`excluded`（預覽中排除）、`expanded`（已展開的 ZIP）、`split`（已依 CUE 分軌的整軌）、`discarded`（使用者捨棄的未保存檔案）。每一步完成即寫入資料庫；重啟後從最後完成的步驟繼續，上傳以保存的 session 續傳。

來源清理（審查 #1、#2）：只有結果已寫入資料庫、且狀態為 `published`／`duplicate`（Drive 上有已驗證的副本）的檔案才刪除其暫存來源；`failed`、被略過的音檔與 ZIP 保留來源（上傳暫存、下載檔案、工作資料夾中解出或轉出的檔案），直到重試成功或使用者「捨棄」。預覽中排除的檔案視為使用者已決定，不保留。重試（#18）讓批次重新經過分析：重新取得收件匣檔案、展開 ZIP、轉檔、依（修正後的）CUE 分軌；已有計畫的檔案保留計畫（含預覽修改），新產生的歌曲用新的分組編號。

`import_batches.state`（P2-3）：`analyzing → review → running → done`，另有 `canceled`。分析（展開 ZIP、讀標籤、定計畫）完成後，要預覽的批次停在 `review`，其餘直接 `running`；工作程序只處理 `running` 批次的檔案。

## API（`/api/v1`，JSON，`Authorization: Bearer <token>`）

以下是主要的端點；完整的路由在 `server/internal/api/server.go`。

| 方法與路徑 | 用途 |
|---|---|
| `POST /setup` | 首次建立管理員；需資料目錄 `setup-code` 檔內的一次性設定碼 |
| `POST /login`、`POST /logout` | 登入取得 token（失敗 5 次／15 分鐘依 IP 節流，IPv6 依 /64，驗證中的請求也算；沒在這裡登入過的瀏覽器另受全體上限，回 429 `reason: throttled_all`，#148、#182）、登出。網頁登入成功時另發 `kanade_known` cookie，標示這個瀏覽器登入過。要 cookie 的登入（`"cookie": true`）需 `X-Requested-With: kanade`；登入路由拒絕瀏覽器標示為跨站的請求（審查 #60） |
| `GET /passkeys/available`、`POST /passkeys/login/options`、`POST /passkeys/login` | 不需登入：是否有任何 passkey（登入頁據此顯示按鈕）、取得一次性 challenge、以 passkey 的回應登入（同密碼登入的節流與 cookie） |
| `GET /passkeys`、`POST /passkeys/options`、`POST /passkeys`、`PATCH`／`DELETE /passkeys/{id}` | 列出自己的 passkey；`{"password"}` 確認密碼後取得建立選項（密碼錯回 403）；儲存裝置建立的 passkey；改名、移除 |
| `GET /status` | Drive 連線、aria2 是否就緒、`lite`（能否轉省流檔） |
| `GET /drive`、`POST /drive/client` | Drive 帳號與容量；載入 OAuth client JSON |
| `POST /drive/auth`、`POST /drive/auth/paste` | 產生授權網址（回呼 `GET /oauth/google/callback`，公開但只接受本服務產生的 state）；貼上回呼網址完成授權 |
| `GET /albums`、`GET /albums/{id}` | 專輯清單（`limit`、`offset`）與收錄 |
| `GET /tracks`、`GET /artists`、`GET /search?q=` | 歌曲、歌手（只列有歌曲的）、搜尋；`/tracks?filter=no_album`／`no_artist` 只列沒有專輯／沒有歌手的歌曲 |
| `GET`／`HEAD /stream/{asset_id}` | 播放（Range、D7 快取）。驗證：`Authorization` header，或 `POST /stream/{id}/url` 取得的 12 小時簽名網址。`?q=opus128`：省流模式，會轉檔的歌送轉好的 Ogg Opus（`audio/ogg`，ETag 為原檔 SHA-256 加 `-opus128`），其餘照原檔送 |
| `GET /stream/{asset_id}/lite` | 省流模式下這首實際怎麼送：`{"lite": true}` 為轉好的 Opus（省流檔在這裡），`false` 為照原檔送（低碼率、轉了沒變小、轉檔失敗，或伺服器不能轉）；`later: true` 為這次照原檔送、省流版在背景轉，下次播放就是省流（10 分鐘以上或從中間開始播放、還沒轉好的歌，#226）。播放器在串流回應後才問（#209） |
| `POST /stream/{asset_id}/prefetch` | 預載下一首：記成快取的下一首並開始下載，不算播放；下載多或磁碟低時略過，回 204（#175）。`?q=opus128`：改成先轉好省流檔，並放掉原檔的預載 |
| `DELETE /stream/prefetch` | 放掉預載：已經沒有下一首（或關閉預載、停止並清空），原本預載的那首若沒有請求在讀就停止下載，回 204（#193、#198） |
| `GET /players`、`GET /players/{pid}/events` | 這個帳號的播放裝置（#138）；分頁的事件流（SSE：`hello` 伺服器時間、`players` 裝置清單、`command`、`ack`），參數 `device`、`name` |
| `POST /players/{pid}/commands`、`POST /players/{pid}/acks` | 送指令給另一個分頁（`{from, op, args}`，對方沒連線回 409）；分頁回覆收到的指令（`{seq, ok, error, data}`），回覆轉給送出的分頁 |
| `PUT /devices/{device}` | 幫自己的瀏覽器取名字（`{name}`，空字串改回瀏覽器自己的名稱） |
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
| `GET`／`PATCH`／`DELETE /tracks/{id}` | 歌曲資訊（含別名、收錄於哪些專輯）；編輯（`title`、`artist`、`version`、`kind`、`composer`、`lyricist`、`arranger`、`aliases`）；回應附 `credits`；永久刪除（音檔移到 Drive 或存放位置的垃圾桶；最多等 5 秒，其餘在背景繼續，#225） |
| `POST /tracks/{id}/restore`、`POST /albums/{id}/restore` | 恢復原標籤 |
| `PATCH /albums/{id}` | 編輯專輯與其收錄（`title`、`album_artist`、`date`、`catalog`、`edition`、`kind`、`aliases`、`entries`） |
| `PUT /albums/{id}/cover` | 以 JPEG／PNG 本體更換封面 |
| `POST /albums/{id}/merge`、`/split`、`/remove` | 合併到 `into`；拆分 `entries` 為 `title`；移除收錄 |
| `DELETE /albums/{id}[?tracks=1]` | 移除專輯（可撤回）；`tracks=1` 另永久刪除只在這張專輯的歌曲 |
| `GET /albums/{id}/identify`、`GET`／`POST /albums/{id}/identify/{release}` | MusicBrainz 候選；差異；套用勾選的 `keys` |
| `POST /albums/{id}/vgmdb`、`POST /albums/{id}/vgmdb/apply` | 用戶端從貼上的 VGMdb 專輯頁讀出的 `album`（名稱、專輯歌手、日期、型號、封面網址、各碟曲名與長度）和這張專輯比對，回傳同 MusicBrainz 的差異；套用勾選的 `keys`，勾了封面才從 media.vgm.io 下載 |
| `GET`／`POST /organize/folders` | 沒有專輯的歌曲中，資料夾說明了專輯的（見下；每個資料夾有 `key` 與來源 `source`）；`{"folders": [{key, title, album_artist}]}` 依資料夾整理，一次可撤回 |
| `GET`／`PATCH /artists/{id}` | 歌手：類型、別名、演唱的歌，以及配音演唱、作曲、作詞、編曲的歌（配音的歌帶 `as`：飾演的角色）、角色的聲優或聲優飾演的角色；改名（`name`，所有角色的原文與拆分決定一起改）、別名（`aliases`） |
| `GET`／`POST /artists/splits` | 看起來是多位歌手的名稱（`proposals`，附建議的拆法）與已決定的（`decided`）；`{"items": [{"name", "parts"}]}` 決定怎麼拆（`parts: []` 保持完整、`null` 取消），一次是一筆可撤回的修改（D13） |
| `GET /edits?limit&before`、`GET /edits/{id}`、`POST /edits/{id}/undo` | 修改紀錄、單筆明細、撤回（部分欄位保留時回傳 `conflicts`；全部無法撤回為 409） |
| `GET /drive/sync` | 同步狀態：上次變更檢查、上次完整對帳與結果、是否進行中、基準對帳是否未完成、收件匣上次檢查 |
| `POST /drive/reconcile` | 在背景開始完整對帳（202；已在進行時 409） |
| `POST /drive/inbox` | 立即檢查收件匣：`{"files": 排入匯入的數量, "waiting": 剛放進來、等下次的數量}` |
| `GET /library/missing` | 音檔遺失的歌曲；每首附 `place`（原本在哪：`kind`、`name`、`mode`） |
| `GET /storage` | 新匯入的去處（`default`：`drive` 或位置 ID）、Drive 是否連線與歌曲數、每個儲存位置（狀態、用量、`use`：曲庫有幾首在這裡、遺失幾首；`location`；OneDrive 的 `signed_in`）、資料目錄、OneDrive 要登記的 `onedrive_redirect` 與是否要貼上網址（`onedrive_paste`） |
| `POST /storage` | `{"kind", "name", "mode", "config", "secret", "category"}` 新增（`category` 只有只讀來源可以）：資料夾檢查可讀寫（資料目錄裡的自動建立）與不和其他位置、暫存、快取重疊；bucket 先用金鑰連線測試（最多 45 秒）；OneDrive 只檢查用戶端資料，之後登入。201 回傳位置 |
| `PATCH /storage/{id}` | `name`、`auto_scan`；`category`：只讀來源之後匯入的專輯放進的分類（0 不放）；`secret`：bucket 的新金鑰或 OneDrive 的新用戶端密碼，先試過能用才換上；`folder`：OneDrive 的資料夾（item ID），曲庫還有檔案在原本的資料夾時 409 |
| `DELETE /storage/{id}` | 移除：只讀來源的歌曲一起離開曲庫（原檔不動，回傳 `songs`），還在預覽或匯入中的檔案一起取消，同一個交易（#222、#229）；掃描或檢查中、存放位置還有曲庫的檔案時 409 |
| `PUT /storage/default` | `{"id": "drive" 或位置 ID}` 新匯入的去處；只讀來源不行 |
| `POST /storage/{id}/check`、`POST /storage/{id}/scan` | 檢查連線與用量；在背景掃描來源（`{"preview": true}` 時新檔案先停在預覽）或檢查存放位置裡曲庫的檔案（202，進行中時 409）。掃描結果記在位置的 `scan`：新加入、內容改變、搬動、不見、回來的數目，以及跟著更新的封面 `covers`（#221） |
| `POST /storage/{id}/connect` | OneDrive 登入：`{}` 回傳 `{"url", "redirect_uri", "paste"}`（回呼 `GET /oauth/onedrive/callback`，公開但只接受本服務為這個位置產生、10 分鐘內的 state）；`{"url"}` 貼上瀏覽器最後停住的網址完成登入 |
| `GET /storage/{id}/folders?parent=`、`POST /storage/{id}/folders` | OneDrive 的資料夾：`{"here", "folders"}`；`{"parent", "name"}` 新增（只有存放位置可以） |
| `POST /account/password` | `{"current", "password"}` 改自己的密碼：結束所有登入、保留 passkey（#76） |
| `GET /sessions`、`DELETE /sessions/{id}`、`POST /sessions/end-others` | 自己的登入（裝置、最後使用）；結束一個；結束其他全部 |
| `GET /site`、`PUT /site`、`PUT`／`DELETE /site/icon` | 站名與圖示：`{"name","default","icon"}`（icon 是上傳圖示的版本，空字串為內建圖示）；改名送 `{"name"}`（空字串恢復預設）；圖示以請求本文上傳 PNG、JPEG 或 WebP。錯誤回 400 與 `reason`：`name`、`format`、`small`、`large` |
| `GET /settings`、`PUT /settings/resources`、`/settings/downloads`、`/settings/drive` | 服務設定：播放快取與暫存容量、磁碟保留空間；下載與上傳限速、同時下載數、每個種子的連線數、是否做種與分享率、時數；收件匣自動匯入、檢查間隔、資料夾靜置時間。儲存時檢查並立即套用 |
| `POST /cache/trim` | 清空播放快取與省流檔（播放中的檔案、省流的目前與下一首保留） |
| `GET /tracks/random?n&kind&not&plain` | 從全曲庫隨機挑歌（以歌曲為單位、只挑可播放的；`kind` 為 `music`、`spoken` 或 `all`；`plain=1` 略過伴奏與短版，沒有別的歌時才挑它們）；`not` 由新到舊列出不要再挑的歌，全部挑過時從最久以前的再繞一輪（#72、#105） |
| `GET /tasks/older?kind&before` | 更早的已結束任務，一次一頁（#68） |
| `POST /albums/merge` | 合併多張專輯到 `into` 或新專輯（`title`、`album_artist`），`sections` 讓每張成為一個區段；`"preview": true` 只回傳計畫 |
| `POST /albums/edit`、`/albums/remove`、`/tracks/edit`、`/tracks/place`、`/tracks/delete`、`/favorites/batch` | 批次操作（#83）：先檢查所有 ID，一次一筆修改紀錄 |
| `PUT /albums/{id}/sections` | `{"sections": {"碟號": "名稱"}}` 命名區段 |
| `PUT /downloads/{id}/grouping`、`GET`／`POST /downloads/{id}/collection` | 下載的專輯分組（依標籤、每個資料夾、合集）；已入庫的下載整理成合集：預覽計畫、套用（#82、#87） |
| `GET /categories[?albums=]`、`POST /categories`、`PATCH`／`DELETE /categories/{id}` | 分類與各自的專輯數、封面；帶 `albums` 時另回每個分類含其中幾張（批次分類用）；新增、改名、刪除（專輯不受影響，可撤回） |
| `POST /albums/categorize` | `{"albums", "add", "remove", "create"}` 一次把專輯放入、移出分類並可新增一個；同一分類不能同時放入與移出（#92、#114） |
| `GET /albums?category=N\|none&q&sort` | 某分類（或未分類）的專輯，可搜尋、依名稱或最近入庫排序；`place=drive` 或位置 ID：檔案在 Google Drive 或某個儲存位置的專輯（#95，其他值 400） |
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

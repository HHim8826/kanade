# P0 技術驗證清單

依據 `plan-v0.5.md` §8 與 `decisions.md` D1–D5 整理。依風險高低排序：越前面的項目一旦失敗，越可能推翻架構。

每項驗證以獨立的小型 spike 程式完成；spike 程式碼不直接進入正式專案，結論寫回本文件。

## 1. Google Drive 存取（D1）

| 驗證 | 通過標準 |
|---|---|
| 自有 Web client 的 OAuth 流程 | 取得 refresh token 並以 Google token endpoint 刷新成功。2026-10-01 已收到 client 設定（專案 `drive-510320`，Web 類型），存於 `secrets/google-oauth-client.json`（權限 600，已列入 `.gitignore`），目前只登記了正式網址的 redirect URI。P0 需在 GCP 再加入 `http://localhost:8080/oauth/google/callback`：瀏覽器導回 localhost 時會打不開，把網址列整串貼回開發程式即可，與 rclone 的做法相同 |
| refresh token 不會 7 天過期 | 首次授權的 token 回應中沒有 `refresh_token_expires_in` 欄位（「測試中」狀態會帶這個欄位，從 7 天倒數），且第 8 天後同一 token 仍能刷新（背景長時間觀察，不阻擋其他項目）。若暫時停在「測試中」，平台專用的 Google 帳號 必須列在測試使用者名單，否則授權會被擋 |
| redirect URI 登記 | 2026-10-01 已驗證：`http://localhost:8080/oauth/google/callback` 與 `https://music.ser1ka.com/oauth/google/callback` 都會導向 Google 登入頁；未登記的對照網址則導向錯誤頁 |
| 貼上外部 token | 以 rclone 取得的 token＋對應 client 能正常刷新 |
| resumable upload 續傳 | 上傳中途終止程序，重啟後以保存的 session URL 查詢進度（`Content-Range: bytes */總長`）並續傳完成；遠端只有一個檔案 |
| `sha256Checksum` 時效 | 上傳完成回應或其後立即查詢即有值，且等於本地 SHA-256；若有延遲，記錄延遲時間 |
| Range 讀取 | `alt=media` 帶 Range 回 206；記錄首位元組延遲與連續 seek 的延遲 |
| Changes API | 在 Drive 網頁上改名、移動、刪除檔案後，以 page token 增量取得對應變更 |
| 上傳 RAM | 分塊 8 MB 時，上傳 1 GB 檔案的程序 RSS 不隨檔案大小增長 |
| 從 Drive 收件匣讀取（D6 選項 1 的前置） | 對 Drive 上已存在的測試檔只用 Range 讀檔頭取得標籤、時長與封面，搭配 `sha256Checksum` 去重，再以伺服器端移動歸入平台資料夾；記錄每首的請求數與傳輸量（目標遠小於檔案大小） |
| 容量 | 以 `about.storageQuota` 確認平台帳號的實際容量（使用者印象約 5 TB） |

### 第 1 節結果（2026-10-02，spike：`spikes/p0-drive`，在開發工作區執行；spike 已於 2026-10-03 刪除，程式碼留在 git 歷史中）

| 驗證 | 結果 | 實測 |
|---|---|---|
| OAuth 流程 | ✅ | 經 `music.ser1ka.com` 隧道回呼取得 refresh token，scope 為完整 `drive`，帳號為平台專用的 Google 帳號。過程中等待回呼的程式隨工作階段結束被停止，使用者看到 502；已補上「貼上回呼網址完成授權」的備援，並驗證 state 必須相符 |
| 7 天期限 | ✅（第 8 天待複查） | token 回應欄位為 `access_token, expires_in, refresh_token, scope, token_type`，沒有 `refresh_token_expires_in`，即已是正式版。強制過期後刷新成功。2026-10-10 後再刷新一次確認 |
| 貼上外部 token | ⏸ 未測 | 優先度低，延後 |
| resumable 續傳 | ✅ | 1 GiB 檔在 40 塊（320 MiB）時中止，狀態檔保留 session；重啟後伺服器回報 335,544,320 位元組，從該處續傳完成；遠端只有一份，SHA-256 相符 |
| `sha256Checksum` 時效 | ✅ | 上傳完成的回應就已帶 `sha256Checksum`（用 `fields` 指定），之後查詢 0.27–0.3 秒內即有值；4 個檔案全部與本地 SHA-256 相符 |
| Range 讀取 | ✅（延遲偏高） | 7 次 64 KiB 讀取都回 206、Content-Range 正確；從開始請求到收到回應約 **0.58–0.68 秒**，與位置無關。播放與每次 seek 都至少有這段延遲，無 SEEKTABLE 的 FLAC 需多次請求時會疊加，第 2 節的伺服器快取與預讀需正視這點 |
| Changes API | ✅ | 6 筆變更全部取得，含建立、上傳、移動（只回報最新狀態），也包含使用者在 Drive 網頁建立的 `music-platform` 資料夾，證明網頁端操作會回報 |
| 上傳 RAM | ✅ | 分塊 8 MiB、直接從磁碟串流：1 GiB 上傳的 RSS 峰值約 17 MB，與 64 MiB 檔相同 |
| 收件匣讀取與歸檔 | ✅ | 96 kHz／24 bit FLAC（35 MiB）：2 次 Range 請求、128 KiB（0.36%）讀到 STREAMINFO、SEEKTABLE、日文 Vorbis 標籤，303 KB 內嵌封面跳過未抓；MP3：ID3v2.3 標籤 97 KB，2 次請求內取得。伺服器端移動每次 0.75–1.1 秒、file ID 不變、不傳資料。移出測試資料夾的請求被防護擋下 |
| 容量 | ✅ | 上限 5120 GiB |
| 上傳速度（參考） | — | 開發工作區到 Drive 約 11–13 MiB/s |

推估：既有曲庫約 1,000 首，若之後以收件匣方式匯入，讀檔頭約需 1,000 × 2 次請求、約 125 MB 傳輸，依序處理約 20 分鐘，可並行縮短。

## 2. 串流與 Android 播放（D3、D4）

| 驗證 | 通過標準 |
|---|---|
| Go 代理 Drive Range | Media3 透過本服務播放 Drive 上的 FLAC、MP3、M4A，拖曳進度正常 |
| 無 SEEKTABLE 的 FLAC seek | 可正常拖曳；記錄每次 seek 產生的 Range 請求數與延遲。現有曲庫抽樣中 54% 的 FLAC 沒有 SEEKTABLE；測試要涵蓋 30 分鐘、150 MB 級的廣播劇單軌，以及 96 kHz／24 bit、約 100 MB 的單曲 |
| VBR MP3 seek | 有 Xing／VBRI 標頭時定位準確；沒有時的偏差程度記錄在案 |
| 無縫播放 | CUE 分軌後的連續兩軌，在 Media3 播放清單中銜接處無可聞間隙 |
| 背景播放 | 鎖定螢幕、切換 App 後持續播放；通知列與耳機按鍵可控制 |
| 裝置端快取 | 第二次播放同一首不再向服務請求音訊資料 |
| 經網域存取 | 經 `music.ser1ka.com`（DNS only）＋內建 ACME 憑證播放正常 |

### 第 2 節結果：伺服器端部分（2026-10-02，`p0drive serve`、`bench-stream.sh`、`bench-ffmpeg.sh`）

播放器端（Media3）尚未測，需 Android 工具鏈與實機。以下以 curl 模擬單次 seek，以 ffmpeg 8.1 當播放器：它對無 SEEKTABLE 的 FLAC 同樣以位元組二分搜尋定位，與 Media3 的 `FlacBinarySearchSeeker` 同類。

測試對照兩種模式：

- **direct**：每個播放器請求都轉成一個 Drive Range 請求；
- **cache**：開始播放時在背景把整首下載進本地稀疏快取檔（256 KiB 區塊）；seek 到下載進度前方 2 MiB 以外時，從該處另開一條下載；兩條下載接上時自動停止。

| 測試 | direct | cache 冷 | cache 熱 |
|---|---|---|---|
| curl 單次 64 KiB 讀取（149 MB FLAC） | 0.55–0.63 秒 | 0.66–0.72 秒 | 0.001 秒 |
| ffmpeg seek 到 15 分，FLAC 無 SEEKTABLE（12 次請求） | **7.7 秒** | 1.8 秒 | 0.03–0.04 秒 |
| ffmpeg seek 到 2 分，MP3 CBR（2 次請求） | 1.8 秒 | 1.0 秒 | 0.1 秒 |
| 經 `music.ser1ka.com` 隧道，curl 64 KiB（35 MB FLAC） | 0.76–0.81 秒 | 0.88 秒 | 0.16–0.26 秒 |

- Drive 整檔下載 36–50 MiB/s，首位元組約 0.76–0.8 秒：149 MB 廣播劇約 3 秒、35 MB Hi-Res 單曲不到 1 秒即整首進快取。
- 兩條下載不重複：Drive 實際傳輸 156.8 MB，對 156.4 MB 的檔案只多出 direct 對照組的 6 次 64 KiB 讀取。
- ffmpeg 在無 SEEKTABLE 的 FLAC 上 seek 一次發出 12 個請求：讀檔尾、讀檔頭，再在目標附近做 10 次二分搜尋，每次預讀約 2.8 MB 後斷線。direct 模式每次都付約 0.6 秒的 Drive 首位元組延遲，因此需要 7.7 秒。
- 隧道帶來每個請求約 0.16–0.26 秒額外延遲（由工作區經 Cloudflare 邊緣往返）；正式部署改 DNS only 直連 VPS 後需在 VPS 重測。
- 尚未測：VBR MP3（目前樣本皆為 CBR）、M4A、無縫播放、背景播放、裝置端快取、ACME。

結論見 `decisions.md` D7。

## 3. 格式轉換與分軌（D2）

| 驗證 | 通過標準 |
|---|---|
| 無損轉 FLAC | APE、TAK、WavPack、TTA、ALAC、WAV 轉為 FLAC 後，解碼 PCM MD5 與來源相同 |
| CUE 分軌精確度 | 各軌 PCM 依序串接的 MD5 等於整軌 PCM MD5；涵蓋有 pregap 與有 HTOA 的 CUE |
| 多 `FILE` 的 CUE | 判定為逐首檔案，不分割，只取 metadata |
| 資源 | 在 1.5 GB 目標環境處理一張約 500 MB 的整軌，記錄耗時、RAM 峰值與暫存峰值 |
| FFmpeg 打包 | 確認 linux/amd64、arm64 靜態建置來源與授權組態（LGPL 或 GPL） |

## 4. aria2（D5）

| 驗證 | 通過標準 |
|---|---|
| 執行檔來源 | 官方最新版 1.37.0（2023-11）只提供 Windows／Android 執行檔，沒有 Linux 版。比較兩條路：直接用第三方 musl 靜態建置（abcfy2/aria2-static-build，有 x86_64、aarch64），或參考其腳本自行建置並固定依賴版本。計畫書要求可重現部署包，傾向自行建置 |
| RPC | 只綁 loopback、使用 secret；以 WebSocket 接收事件通知 |
| 先取 metadata 再選檔 | 加入 Nyaa 的 `.torrent` 後先取得檔案清單，不下載內容；選檔後只下載選取檔案 |
| 部分選檔的磁碟占用 | 未選取檔案因 piece 邊界產生的額外占用有多大，是否計入配額 |
| 控制與恢復 | 暫停、恢復、取消；aria2 重啟後以 session 檔恢復任務 |
| 資源 | `bt-max-peers=30` 下載與做種時的 RSS 與連線數 |

aria2 上游約三年未發布新版，屬於維護風險；計畫書的下載器適配層（可改接 qBittorrent）可緩解。

### 第 4 節結果（2026-10-02，B3 實作過程中驗證）

| 驗證 | 結果 | 實測 |
|---|---|---|
| 執行檔來源 | ✅（開發期） | 使用 abcfy2/aria2-static-build 1.37.0（x86_64 musl 靜態版），SHA-256 與 GitHub 公布值相符。正式發布前仍依原計畫改為自行建置 |
| RPC | ✅ | 只綁 127.0.0.1、隨機埠；密鑰寫在 600 權限的設定檔，不出現在程序列表 |
| 先取 metadata 再選檔 | ✅（magnet 未實測） | `.torrent` 網址由服務自行下載後以暫停狀態加入 aria2；magnet 用 `bt-metadata-only` 取得 metadata 後同樣以 torrent 加入。以 archive.org 的 CC 授權專輯實測網址路徑：13 個檔案列出、依 D2 預設勾選。magnet 路徑寫好但尚未用真實 magnet 驗證 |
| 部分選檔的磁碟占用 | ✅ | 未勾選檔案仍會寫入與相鄰檔案共用的 piece：測試中 100 KB 影片寫入 40 KB，實際 torrent 中 5.5 MB 的 MP3 寫入 392 KB，皆為稀疏檔；計入暫存預算，任務結束後刪除。aria2 回報的完成量也包含這部分，顯示時封頂為 100% |
| 控制與恢復 | ✅（發現並修正一個問題） | 暫停、繼續、取消皆可。**aria2 在任務資料夾不存在時，會默默不寫 torrent、也不把任務存進 session，重啟後任務遺失**；改為加入前先建立資料夾，並以「等待選檔時重啟 aria2」的回歸測試鎖住 |
| 資源 | 部分 | 做種、少量 peer 時 aria2 RSS 12 MB、主服務 25 MB。30 個 peer 滿載時的數字待測 |
| 逾時 | 已調整 | 實測 archive.org 一個無回應的 web seed 鏡像讓下載停住 5 分鐘（預設 60 秒 × 5 次）；改為連線 15 秒、傳輸 30 秒 |

## 5. 匯入、解析與去重

| 驗證 | 通過標準 |
|---|---|
| 標籤庫選型 | 比較純 Go 唯讀方案（如 `dhowden/tag`）與可寫入方案；以日文樣本檢查 ID3v2.3／2.4、FLAC、M4A、Ogg 的讀取正確性 |
| 文字編碼偵測 | CP932 的 ZIP 檔名、ID3、CUE 正確還原；UTF-8 不被誤判；UTF-16 EAC log 正確讀取 |
| 串流雜湊 | 邊接收邊計算 SHA-256，RAM 固定，不隨檔案大小增長 |
| FLAC 音訊 MD5 | 直接讀取 STREAMINFO MD5；處理 MD5 為全零（編碼器未寫入）的情況 |
| ZIP 安全 | 拒絕路徑逃逸、超出展開比例與總量的 ZIP（zip bomb）；超配額 ZIP 在可預測時間內拒絕 |

## 6. RSS（計畫書 §3）

| 驗證 | 通過標準 |
|---|---|
| Nyaa 範本 | 固定 URL 格式（`?page=rss&c=2_1&q=...`）；解析 `nyaa:infoHash`、`nyaa:size`、`nyaa:seeders`。2026-10-01 已實際確認這些欄位存在 |
| 通用 RSS | 正確辨識 enclosure torrent、magnet、明確下載連結；只有詳情頁連結的條目標示為不可直接下載 |
| 去重 | 同一 infohash 跨來源合併；同名不同版本不合併 |

## 7. SQLite 與搜尋

| 驗證 | 通過標準 |
|---|---|
| 驅動 | 純 Go 驅動（如 `modernc.org/sqlite`）可交叉編譯 amd64／arm64，且支援 FTS5 |
| 日文搜尋 | FTS5 trigram 可搜尋 3 字以上日文子字串；1–2 字查詢的退回方案（LIKE）在 1 萬首規模下的延遲 |

## 8. Android 檔案存取

| 驗證 | 通過標準 |
|---|---|
| 多選檔案 | 一次選取多個音檔並分塊上傳、可續傳 |
| 資料夾 | 以 SAF 選取資料夾並遞迴列出音檔，保留相對路徑 |
| ZIP | 選取 ZIP 後上傳到服務處理；確認大型 ZIP 是否需要在裝置端解壓 |

## 9. 整體資源（計畫書 §6）

在 1.5 GB RAM／20 GB 磁碟的目標 VPS 上，同時進行播放、下載、上傳與入庫，記錄程序 RSS、系統可用 RAM、CPU、磁碟峰值、首次播放延遲與 Drive 請求量。通過標準依計畫書：常態 150–300 MB，尖峰 ≤ 500 MB，不 OOM。

目前工作區（15 GB RAM、無 systemd）無法可靠模擬這個限制，本項以目標 VPS 為準。

## 執行環境

| 環境 | 用途 | 現況 |
|---|---|---|
| 目前工作區 | 1、3、4、5、6、7 的功能驗證 | 有 Go 1.27.1；缺 FFmpeg、aria2 |
| 目標 VPS | 2 的網域部分、9 的資源驗證 | OS／架構待確認 |
| Android 實機 | 2、8 | 工作區缺 JDK、Android SDK；以 APK 安裝測試 |

## 需要使用者提供

1. ~~GCP 專案與 OAuth client~~：已收到；localhost redirect URI 已加入並驗證；2026-10-02 品牌檢查通過（應用程式名稱 ser1ka Music）。是否已是正式版，首次授權時以 token 回應有無 `refresh_token_expires_in` 確認。以下為原始說明：在 Google Auth Platform 的「目標對象（Audience）」頁把發布狀態改為「正式版（In production）」。若「發布應用程式」按鈕無法點選，通常是品牌資訊不完整：需要首頁網址、隱私權政策網址，以及已驗證的授權網域 `ser1ka.com`（可在 Search Console 以 Cloudflare DNS TXT 紀錄驗證，不需要先架網站）。在改好之前，P0 可先在「測試中」狀態進行，但要把 平台專用的 Google 帳號 加為測試使用者。
2. 在平台專用的 Google 帳號建一個測試用資料夾（P0 期間只在此資料夾內寫入）。
3. 測試音檔：現有曲庫已涵蓋無 SEEKTABLE 的 FLAC、Hi-Res、無標籤 MP3、UTF-16 LOG、cp1252 CUE（見 `library-survey.md`）。仍缺：APE、TAK、整軌 FLAC＋CUE（最好有含 pregap 的）、CP932 標籤的 MP3、含日文檔名的 ZIP。TAK 編碼器只有 Windows 版，無法在這裡自行產生樣本。
4. 一台 Android 8.1 以上的實機。
5. 目標 VPS 的存取權（進行第 2、9 項時）。

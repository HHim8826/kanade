# 決策紀錄

各項決策覆蓋 `plan-v0.5.md` 中的對應內容；計畫書本身保留原樣。

## D1　Google Drive 存取比照 OpenList

- 日期：2026-10-01
- 狀態：已決定（token 模式選 C）
- 取代：計畫書 §1「rclone 的儲存能力嵌入 Go」、§3「rclone 儲存模組」、§10 rclone 參考

### 決定

不嵌入 rclone。Go 服務直接以 REST 呼叫 Drive API v3，做法比照 OpenList 的 `google_drive` 驅動。

### OpenList 的做法

參考版本：OpenList `744dbd5`（`drivers/google_drive`，2026-01-08）、OpenList-APIPages `src/driver/googleui_oa.ts`。

| 項目 | 做法 |
|---|---|
| 認證設定 | 保存 `refresh_token`；刷新有三種方式：線上 API（預設）、自有 `client_id`/`client_secret`、service account JSON |
| 線上 API 模式 | 把 refresh token 送到 `api.oplist.org/googleui/renewapi`，由對方用其 client 換 access token |
| 取得 refresh token | token 產生器網頁走 Web 應用 OAuth，redirect 回網頁網域；scope 為完整 `drive`，`access_type=offline`、`prompt=consent`；使用者複製 token 貼回 |
| 定位 | 以 file ID 操作：設定 root folder ID，按 parent ID 列檔（`pageSize=1000`） |
| 檔案資訊 | 列檔欄位包含 `md5Checksum`、`sha1Checksum`、`sha256Checksum`、`size` |
| 播放／下載 | 伺服器代理（OnlyProxy），帶 Bearer 轉送 `files/{id}?alt=media&acknowledgeAbuse=true` |
| 上傳 | resumable upload，預設 5 MB 分塊，每塊最多重試 3 次 |
| 錯誤 | 收到 401 時刷新 token 後重試 |

### 對本專案的影響

- 計畫書以 file ID 為中心（綁定 file ID、按 ID 串流、Changes API 對帳），直接對上 Drive 原生模型，不必繞過 rclone 的路徑抽象；也少了標示為實驗性的 librclone 依賴。
- Drive 會回傳 `sha256Checksum`：上傳驗證可直接比對本地 SHA-256 與大小，不再只是「可用的遠端校驗資訊」。P0 需確認上傳完成後該欄位是否立即有值。
- 完整 `drive` scope 可讀取既有 Drive 曲庫，符合首次掃描需求。
- OpenList 沒做、本專案要自行補上：
  - Changes API 增量對帳；
  - resumable upload session URL 持久化，重啟後續傳；
  - 403 `rateLimitExceeded`／429 的退避與共用限流；
  - 所有寫入限制在指定的音樂根資料夾內。
- OpenList 與 OpenList-APIPages 皆為 AGPL-3.0：參考做法，自行實作，不複製程式碼（對應計畫書待決事項 7）。

### Token 模式：C

曾考慮的選項：A 使用 OpenList 公共服務；B 自有 GCP client、授權頁內建；C 以 B 為預設並接受貼上外部 token。選 C。

**預設流程（自有 client，授權頁內建在本服務）**

- 管理員在 GCP 建立專案、啟用 Drive API，建立「Web 應用程式」類型 OAuth client，redirect URI 設為 `https://<服務網域>/oauth/google/callback`。
- OAuth 同意畫面設為「外部」並發布為「正式版」；未驗證警告對個人使用可接受。停在「測試中」會讓 refresh token 7 天過期。
- 初始化時填入 client ID／secret → 服務產生授權網址（scope `drive`、`access_type=offline`、`prompt=consent`、驗證 `state`）→ 回呼後服務端保存 refresh token。
- 刷新直接向 Google token endpoint 進行，不經第三方。

**替代入口（貼上 token）**

- 貼上 refresh token，並提供簽發它的 client ID／secret（例如自行用 rclone 或 OpenList 工具的「自有 client」模式取得）。
- 若 token 來自 OpenList 公共 client，只能經 `api.oplist.org` 刷新：列為「第三方刷新模式」，預設關閉，啟用時明確告知對方可存取整個 Drive 且依賴其服務可用。

**共通**

- client secret、refresh token、access token 只存服務端（檔案權限 0600），不回傳客戶端，日誌遮罩；客戶端只持有本服務自己的登入憑證。
- Service account 不支援：個人 Gmail 的「我的雲端硬碟」無法由 service account 上傳（沒有自己的儲存配額），只適用 Workspace 共用雲端硬碟。

## D2　日本音樂常見格式與編碼

- 日期：2026-10-01
- 狀態：已決定（使用者授權由 Claude 決定）
- 取代／修改：計畫書 §5「優先原格式播放」改為「播放格式白名單」；CUE 分軌與無損格式轉換由 P3 提前到 P2；計畫書待決事項 5 中 CUE 分軌範圍

### 背景

Nyaa 無損音樂分類實際抽樣（2026-10-01）：最新條目以 `[FLAC+CUE+LOG+BK]` 為主，一張單曲 703.5 MiB（含歌詞本掃描）。Media3（Android 播放器）支援的音訊容器為 MP4、Matroska、MP3、Ogg（Vorbis／Opus／FLAC）、WAV、ADTS、FLAC；不支援 APE、TAK、WavPack、TTA；ALAC 只能靠另行打包的 FFmpeg 擴充解碼；FLAC 需要系統解碼器（API 27 起提供）或另行打包擴充。

### 決定

**1. 播放格式白名單**

曲庫中可播放的音檔只有：MP3、AAC（M4A）、FLAC、Ogg Vorbis、Ogg Opus。這組格式 Media3（minSdk 27）與主流瀏覽器都能原生播放，客戶端不需打包額外解碼器。

**2. 其他無損格式入庫時轉為 FLAC**

- APE、TAK、WavPack、TTA、ALAC、WAV、AIFF → FLAC，保持原取樣率、位元深度與聲道。
- 驗證：來源與輸出解碼後的 PCM MD5 必須相同，否則該檔失敗並保留來源。
- 來源檔預設不上傳 Drive；記錄來源 SHA-256 與大小，用於再次匯入時去重。做種原檔仍依做種規則保留在本地。
- 原標籤保存於資料庫（仍為唯一真實來源），並複製到輸出 FLAC 以利攜帶。
- 有損非白名單格式（WMA、MPC 等）與 DSD（DSF／DFF）：第一版在預覽中標示「不支援」並略過。不做有損轉有損。
- Hi-Res FLAC 維持原樣，不降頻。播放時轉碼仍預設關閉（P3）。

**3. CUE＋整軌：入庫時實體分軌**

- 整軌音檔按 CUE 分割為逐首 FLAC，每首是一般 asset。不採「虛擬分軌」（只存整軌、播放時剪輯）：虛擬分軌會讓每個客戶端都要實作剪輯，播放單首也要拉整張專輯的檔案，快取與歌單都變複雜。
- 驗證：各軌 PCM 依序串接的 MD5 必須等於整軌 PCM MD5（整軌為 FLAC 且 STREAMINFO 含 MD5 時一併比對）。
- 軌間 pregap 併入前一軌；首軌 INDEX 01 之前的隱藏音軌（HTOA）若存在，獨立為第 0 軌。
- CUE 若已對應逐首檔案（多個 `FILE`），不分割，只取用其 metadata。
- `FILE` 指向的檔案不存在時（常見：EAC 的 CUE 指向 `.wav`，實際是 `.flac`），依「同主檔名換副檔名」再依曲序比對。
- CUE 的 TITLE、PERFORMER、REM DATE、DISCNUMBER、CATALOG 等視為「原始標籤」層級的分組證據。
- 整軌來源 SHA-256 記錄為匯入來源，同一整軌再次匯入時直接略過。

**4. 文字編碼偵測**

- 適用：ZIP 項目名稱、ID3v1 與宣告為 ISO-8859-1 的 ID3v2 frame、CUE、LOG、M3U、LRC、torrent 內檔名（有 `.utf-8` 欄位時優先使用）。
- 規則：有 BOM 依 BOM（EAC log 常為 UTF-16LE）；合法 UTF-8 直接使用；否則先試 CP932（Shift-JIS），解碼無錯且含合理的假名／漢字比例才採用；都不符合時保留原始位元組並標示「編碼未知」。
- 合理性檢查（2026-10-01 依曲庫調查補充）：CP932 解碼結果若主要是零散的半形片假名（例如 cp1252 的 `0xB7` 會被誤讀成「ｷ」），不採用 CP932。若文字大部分是「.」或「?」佔位字元，判定為「原資料已遺失」，該檔不作為分組證據。
- 預覽顯示偵測到的編碼，使用者可按批次改為 CP932、GBK、Big5 或 Latin-1。
- 原始位元組保存於資料庫，與「保留原資料」政策一致。
- 實作使用 `golang.org/x/text/encoding`；ZIP 依 `archive/zip` 的 `NonUTF8` 旗標與內容判斷。

**5. 附屬檔案預設處理**

| 類型 | BT 選檔預設 | 入庫處理 |
|---|---|---|
| 音檔、CUE、LOG、LRC | 勾選 | 音檔入庫；CUE／LOG 作旁附檔上傳 Drive；LRC 作歌詞 |
| 封面 | 勾選 | 依序取：內嵌封面 → 專輯根目錄的 `cover`／`folder`／`front`／`jacket` 圖檔 → 掃描資料夾中檔名像封面者（同上關鍵字，或型號後綴 `_01`／`-01`）→ 掃描資料夾依檔名排序的第一張 → 留空，由使用者指定；縮圖受尺寸限制 |
| 歌詞本掃描（BK、Scans 等資料夾） | 勾選；總量超過 300 MB 時只勾選檔名像封面的圖片 | 用來挑封面；只上傳選中的封面，其餘預設不上傳，使用者可選擇作旁附檔保存 |
| 影片（BD／DVD 特典的 mkv、m2ts 等） | 不勾選 | 第一版不匯入 |

**6. 同一音訊、不同標籤的判斷訊號**

每個 asset 記錄解碼後的音訊 MD5（FLAC 直接讀 STREAMINFO，轉換或分軌時順帶計算）。MD5 相同的不同檔案在預覽中標為「同一音訊，標籤不同」；仍不自動合併或替換。

這個值是可選訊號，不保證存在：曲庫抽樣中 26%（20／76）的 FLAC STREAMINFO MD5 全為零。既有曲庫不會為了計算它下載整檔；精確去重仍以 SHA-256＋大小為準。

**7. 工具與資源**

- FFmpeg 由「可選」改為隨產品打包：轉換與分軌需要它；一般 MP3／FLAC／M4A 匯入不需要。
- 以子程序執行，固定版本，同時最多 1 個，低優先權；RAM 與暫存計入既有預算。先用官方靜態版本，P3 再改為只含音訊編解碼的精簡建置。
- FFmpeg 不可用或某檔處理失敗時，該檔標示原因進入待處理，同批其他檔案照常入庫。

**8. 階段調整**

| 階段 | 內容 |
|---|---|
| P0 | 分軌 MD5 驗證、轉換耗時與 RAM 峰值（1.5 GB 目標環境）、Drive `sha256Checksum` 時效 |
| P1 | 文字編碼偵測（日文標籤一開始就會遇到） |
| P2 | 無損格式轉 FLAC、CUE 分軌、附屬檔案規則 |
| P3 | 播放時轉碼、精簡 FFmpeg 建置 |

## D3　客戶端架構

- 日期：2026-10-01
- 狀態：已決定（使用者授權由 Claude 決定）
- 補充：計畫書 §3「Kotlin 客戶端」、§7、待決事項 2（發佈順序、網頁版）

### 決定

**1. 發佈順序**

Android 先行；桌面版在 P3；第一版不做網頁播放器。Go 服務只提供 OAuth 授權與回呼等必要的簡單網頁。因為 D2 的白名單格式瀏覽器都能原生播放，日後補網頁版的成本可控。

**2. 專案結構**

Kotlin Multiplatform：`shared` 模組放資料模型、API 客戶端、本地快取與 Compose Multiplatform UI；`androidApp` 放 Android 專屬部分。UI 從一開始寫在共用模組，桌面版之後可沿用。

**3. 播放層**

- 共用模組定義精簡的播放器介面（佇列、播放／暫停、seek、狀態事件），UI 只依賴這個介面。
- Android 直接使用 Media3：ExoPlayer＋`MediaSessionService`，提供背景播放、通知列、鎖定畫面、藍牙／耳機按鍵控制。
- 桌面版播放器在 P3 決定，首選候選是 mediamp（open-ani，Apache-2.0，桌面後端為 VLC）。Android 端不經 mediamp：背景播放服務本來就要直接接 Media3，多一層抽象沒有收益。
- Animeko（AGPL-3.0）只作 UI 與互動參考，不複製程式碼。

**4. Android 基準**

- minSdk 27（Android 8.1）：FLAC 由系統解碼器播放，不必打包 FLAC 或 FFmpeg 擴充。
- 播放清單需無縫銜接（gapless），CUE 分軌後的連續曲目依賴這點；P0 驗證。
- 裝置端快取：Media3 `CacheDataSource`＋`SimpleCache`，LRU，預設 1 GB、可調。重複播放不必再經 VPS 與 Drive，減輕 1.5 GB VPS 的負擔。
- 請求只帶本服務的登入憑證；Google token 永不下發到裝置。

**5. 主要函式庫**

Ktor client、kotlinx.serialization、Coil 3（圖片）、Media3。本地快取資料庫（SQLDelight 或 Room KMP）在 P1 開工時擇一。

**6. 開發環境限制**

目前工作區沒有 JDK 與 Android SDK，P1 開工前需安裝。容器內是否能跑模擬器未確認；預設以 APK 安裝到實機測試。

## D4　對外網域與 HTTPS

- 日期：2026-10-01
- 狀態：已決定
- 補充：計畫書 §7「對外提供統一登入與 HTTPS」

### 決定

- 服務網域：`music.ser1ka.com`。`ser1ka.com` 由 Cloudflare 託管（NS 為 `camilo`／`tegan.ns.cloudflare.com`）；2026-10-01 查詢時 `music.ser1ka.com` 尚無任何 DNS 紀錄，部署時新增 A／AAAA 指向 VPS，設為 DNS only（灰雲），理由見下方。
- D1 的 OAuth redirect URI：`https://music.ser1ka.com/oauth/google/callback`。
- HTTPS 由 Go 服務內建 ACME（Let's Encrypt）自動取得與續期憑證，不需額外反向代理，符合「一套程式」的部署目標；也可關閉內建 TLS，改放在既有反向代理後。
- 客戶端上傳一律分塊（每塊 32 MB）並可續傳：行動網路中斷時不必重傳整檔，也避開代理的請求大小上限。

### Google OAuth 品牌頁面（2026-10-01 補充）

- Google 要求首頁與隱私權政策網址都能實際回應、兩者不同，且網域須由 GCP 專案所用帳號在 Search Console 驗證過。
- 開發階段（使用者 2026-10-01 決定先在目前工作區開發）：`music.ser1ka.com` 經工作區既有的 Cloudflare Tunnel 指向 `http://localhost:8080`。目前由 `deploy/landing`（小型 Go 程式，只綁 127.0.0.1:8080）提供 `/` 與 `/privacy`；P0 的 Go 服務之後接手同一個 port，OAuth 回呼 `https://music.ser1ka.com/oauth/google/callback` 與 `http://localhost:8080/oauth/google/callback` 都會到同一個服務。
- 經 Tunnel 屬於 Cloudflare 代理：單一請求 100 MB 上限與串流條款風險同下節，開發測試可接受。
- 正式部署到 VPS 時：移除 Tunnel 的 `music.ser1ka.com`，改設 DNS only 紀錄指向 VPS；Go 服務必須自己提供相同的 `/` 與 `/privacy`，否則 Google 端的品牌檢查會失效。
- `ser1ka.com` 原本已有一筆 `google-site-verification` TXT，但不屬於 GCP 專案所用的帳號；需用該帳號另加一筆（多筆可並存，不要刪除舊的）。
- 2026-10-02 品牌檢查通過。應用程式名稱定為 **ser1ka Music**；原名「Drive-music」含 Google 產品名，且當時首頁內容過於簡略，一直被判定「名稱與首頁不同」。
- Google 的檢查會實際抓首頁（存取紀錄中 User-Agent 為 `GoogleAgent-URLContext`），讀內容判斷名稱與用途。首頁需符合 Google 的 App Homepage 要求：名稱清楚出現、說明功能、說明使用 Google Drive 的目的、連到 `https://music.ser1ka.com/privacy`、不需登入。正式服務接手 `/` 時，內容至少要保留這些（現行版本見 `deploy/landing/index.html`）。

### 若網域託管在 Cloudflare

- 開啟代理（橘雲）或使用 Cloudflare Tunnel 時，免費方案單一請求 body 上限為 100 MB；分塊上傳已能避開。
- 經 Cloudflare 代理長期串流大量音檔，有違反其 CDN 服務條款（大型檔案限制）的風險。音樂子網域建議設為 DNS only（灰雲），由服務自行處理 TLS；代價是 VPS IP 公開。

## D5　BT 在 VPS 上執行

- 日期：2026-10-01
- 狀態：已決定（使用者確認 VPS 允許 BT）
- 補充：計畫書待決事項 4、8

### 決定

- 下載段維持計畫書設計：VPS 上的內附 aria2 負責 BT 下載與做種；P1 範圍不變。本地代理仍只是 P3 的擴充。
- VPS 規格以計畫書的 1.5 GB RAM／20 GB 磁碟為設計目標；OS 與 CPU 架構部署時再確認。主服務、aria2、FFmpeg 都建置 linux/amd64 與 linux/arm64 兩種，架構不阻擋開發。

### 做種預設值（可在設定中調整）

| 項目 | 預設 |
|---|---|
| 停止條件 | 分享率達 1.0 或做種滿 72 小時，先到者為準 |
| 磁碟 | 做種資料計入 2 GB 共用暫存配額；新任務需要空間時，最早完成上傳驗證的做種任務先停止並清理 |
| 前提 | 檔案必須已上傳且驗證成功，才可因空間不足提前結束做種並刪除 |
| 連線 | 每個任務最多 30 個 peers（aria2 `bt-max-peers`），控制小 VPS 的記憶體與連線數 |
| 上傳速度 | 不限速，可在設定中調整 |

## D6　平台帳號與既有曲庫

- 日期：2026-10-01
- 狀態：帳號已確認；既有曲庫的遷移由使用者決定延後處理

### 已確認

- 平台授權的 Google 帳號：`phsub350@gmail.com`，專為本平台建立的獨立新帳號。使用者表示容量約 5 TB，P0 授權後以 Drive `about.storageQuota` 確認。
- 既有曲庫：OpenList「Music」就是全部曲庫（約 1,000 首、29 GB，見 `library-survey.md`），儲存在 **OneDrive**，不是 Google Drive。

### 對設計的影響

- 第一版只有一個儲存來源：`phsub350` 的 Google Drive。不需要跨帳號或多來源設計，D1 不變。
- `library-survey.md` 的內容分析（格式、標籤、分組難點）與儲存位置無關，仍然有效；只有「以 Drive `sha256Checksum` 原地索引」的建議不適用於 OneDrive 上的檔案。

### 日後遷移的選項（記錄備查，現在不決定）

| 選項 | 做法 | 評估 |
|---|---|---|
| 1　OpenList 跨儲存複製 | 在使用者的 OpenList 加入 `phsub350` 的 Google Drive，用 OpenList 的複製任務把 Music 複製到 Drive 的「匯入收件匣」資料夾，再由平台從收件匣匯入 | 傳輸由 OpenList 所在主機負責，不經 VPS；平台只需讀檔頭，並在 Drive 內以伺服器端移動歸檔。傾向此項 |
| 2　經 VPS 下載後上傳 | 平台從 OneDrive 下載，再走一般匯入流程 | 29 GB 經 VPS 進出，且受 2 GB 暫存配額限制，需分批，最慢 |
| 3　平台直接接 OneDrive | 新增 OneDrive（Microsoft Graph）唯讀來源 | 多一個儲存適配器與多來源資料模型，不列入第一版 |

選項 1 需要平台支援「從 Drive 收件匣匯入」：檔案已在 Drive 上，以 Range 讀檔頭解析、用 `sha256Checksum` 去重，再以伺服器端移動（不重新上傳）歸入平台資料夾。這個能力對日常使用也有用（使用者可直接從 Drive 網頁丟檔案進收件匣），建議列入 P2。

## D7　串流：播放即整首背景快取

- 日期：2026-10-02
- 狀態：已決定（依 P0 第 2 節伺服器端實測）
- 取代：計畫書 §5「第一版先完成按需串流與有容量限制的檔案快取；分塊快取的必要性由驗證決定」

### 依據

- Drive 每個請求的首位元組延遲約 0.6–0.8 秒，與位置無關；整檔下載則有 36–50 MiB/s。
- 單純轉送時，無 SEEKTABLE 的 FLAC 一次 seek 需 12 個請求、7.7 秒；現有曲庫抽樣中 54% 的 FLAC 沒有 SEEKTABLE。
- 播放即在背景整首下載後，同樣的 seek 冷啟動 1.8 秒、快取完成後 0.03 秒。

### 決定

- 單純轉送 Drive Range 不作為播放路徑。
- 開始播放一首時，在背景把整首下載進本地快取；以 256 KiB 區塊記錄已到資料，請求先從快取供應，未到的區塊等候下載。
- seek 到下載進度前方 2 MiB 以外時，從該處另開一條下載；兩條接上時停止，避免重複傳輸。每首最多 3 條。
- 快取以整首為淘汰單位（LRU），正在播放或 30 秒內用過的不淘汰；容量沿用計畫書 512 MiB 預設。預載最多下一首（計畫書既有規則）。
- 同一首的多個請求共用同一組下載（計畫書「同一內容請求共用拉取工作」）。
- ETag 使用 Drive `sha256Checksum`，作為內容版本。
- 不在入庫時為 FLAC 補寫 SEEKTABLE：那會改寫音檔，違反「預設不改寫音檔」，且快取已解決問題。

### 尚待驗證

Media3 實際的請求模式與無縫播放（需 Android 實機）；VBR MP3、M4A；正式 VPS 上的延遲與 512 MiB 配額下的實際命中率。

## D8　網頁端先行

- 日期：2026-10-02
- 狀態：已決定（使用者提出）
- 修改：D3「第一版不做網頁播放器」；Android 仍照 D3 的架構，順序改為在網頁端之後

### 決定

- 先做瀏覽器客戶端，網址 `https://music.ser1ka.com/app/`；首頁 `/` 仍保留給 Google 品牌檢查，並加上「開啟 App」連結。
- 網頁檔案嵌入 Go 執行檔，部署仍是單一程式。不用建置流程：Preact 10.29.8＋htm 3.1.1 以 `npm pack` 取得（核對完整性雜湊）後放在 `internal/web/static/vendor/`。
- 登入改用 HttpOnly、SameSite=Strict 的 cookie，腳本讀不到 token；以 cookie 驗證的寫入請求還必須帶 `X-Requested-With: ser1ka` header，作為第二層 CSRF 防護。API 仍接受 Bearer token，供 App 與命令列工具使用。
- 播放用同源的 `<audio>`，直接走 D7 的串流快取；開始播放一首時，會先請求下一首的第一個位元組，讓伺服器提早開始抓（計畫書「預載最多下一首」）。
- 頁面採嚴格 CSP：只允許同源腳本與樣式，不允許 inline 程式碼與第三方資源。
- 靜態檔放在以內容雜湊命名的版本路徑（`/app/v/<版本>/…`）。Cloudflare 會把 `.js`／`.css` 的快取標頭改成瀏覽器快取一小時，不換網址的話，部署新版後最多要等一小時才會生效。

### 已知限制

- `<audio>` 不支援無縫接播；手機瀏覽器的背景播放受系統限制。這兩項留給 Android 版處理。
- 網頁上傳不在瀏覽器計算 SHA-256（大檔會占用手機記憶體），只驗證大小；伺服器入庫時仍會計算 SHA-256，並與 Drive 回報的值核對。
- 上傳到一半重新整理頁面時，網頁不會記得原本的分組，要重新上傳；伺服器端支援續傳，日後可把分組存在瀏覽器裡改善。

## D9　播放紀錄與首頁

- 日期：2026-10-02
- 狀態：已決定（首頁內容由使用者授權 Claude 決定）
- 補充：計畫書 §5「歌單與播放紀錄」中的播放紀錄部分

### 計一次播放

- 聽到的時間（不是播放位置）≥ 曲長一半或 4 分鐘，取較短者，才算一次播放；短於 30 秒的曲目不計（與 Last.fm 規則相同）。拖曳進度跳過的部分不算「聽到」。
- 客戶端每次播放產生一個 session ID，播放中每 15 秒、暫停、結束、換曲與關閉頁面時回報。同一 session 重複或亂序的回報只會更新同一筆，聽到的時間只增不減，已計數、已聽完不會被撤回。

### 續播

- 首頁最上方的卡片（2026-10-02 依使用者回報修訂）：
  - 本分頁正在放東西時，顯示「正在播放／已暫停」與即時進度，可直接暫停、點開全螢幕播放頁；
  - 否則顯示 30 天內最近一次播放（不分裝置、不論是否聽完），在原本播放的專輯脈絡中接續：沒聽完就從上次位置繼續，已聽完（或離結尾不到 30 秒）就放專輯的下一首，專輯最後一首則從頭。
  - 原本的規則（只取聽超過 30 秒、尚未聽完的播放）會讓之後聽的歌永遠蓋不過舊的廣播劇，因此只留給「未聽完的廣播劇」清單使用。
- 回報只在位置或聽到的時間有變時才送，開始播放時立刻送一次；這樣「最近一次播放」不會被另一台裝置上暫停中的分頁在切到背景時搶走。
- 廣播劇與電台（`spoken`）在任何地方播放都自動回到上次位置。
- 內容類型：匯入時依 GENRE（Spoken、Drama、Radio、朗読、ドラマ等）、路徑或專輯名中的 Drama CD、DJCD、radio（整字）、ドラマ、ラジオ判定；既有曲目由遷移 0005 以同樣規則回填。

### 首頁

由上而下：
1. 隨便聽一張：只從含音樂的專輯中抽，不抽廣播劇；
2. 繼續播放；
3. 進行中的任務摘要（有任務時才出現）；
4. 最近播放的專輯；
5. 未聽完的廣播劇；
6. 最近加入；
7. 待整理（沒有專輯或歌手的歌曲、匯入失敗）。

不做推薦模型（計畫書已排除）。

## D10　專案名稱：Kanade

- 日期：2026-10-02
- 狀態：已決定（使用者選擇）

- 產品名稱 **Kanade**（奏）。網頁、首頁、隱私權政策、PWA、Drive 平台資料夾（依 file ID 原地改名）都已改用新名稱。
- 程式改名：執行檔 `kanade`、Go 模組 `github.com/HHim8826/kanade/server`、環境變數 `KANADE_DATA`／`KANADE_ARIA2`（舊的 `SER1KA_*` 仍可用）、登入 cookie `kanade_session`（改名後需重新登入一次）。
- 網域仍為 `music.ser1ka.com`；伺服器上的工作目錄 `/data/music-platform` 不變。
- 原始碼在私人 repo `HHim8826/kanade`。
- Google OAuth 同意畫面的應用程式名稱須改為「Kanade」，才會與首頁一致（D4 的品牌檢查）。

## 仍待決

目前沒有阻擋 P0 的待決事項。

- 部署前需確認 VPS 的 OS、CPU 架構、實際可用磁碟與流量額度（Hi-Res 每首約 95 MB，每次播放都經 VPS 轉送）。
- 既有曲庫遷移：使用者決定之後再處理（D6）。

# 現有曲庫抽樣調查

- 日期：2026-10-01
- 來源：使用者的 OpenList 分享「Music」（`opt.ser1ka.com`），實際儲存在 OneDrive；這就是使用者的全部曲庫
- 方法：唯讀列出全部 160 個資料夾；用 Range 讀取檔頭解析標籤（每個資料夾的第一首，共 87 首，另加海貓 Episode 1 全部 43 首）；下載全部 9 個文字小檔。沒有下載任何完整音檔。

## 規模與格式

| 項目 | 數值 |
|---|---|
| 檔案／資料夾 | 1,358 個檔案、159 個資料夾，共 28.9 GB |
| 音檔 | 980 個：FLAC 805 個（24.2 GB）、MP3 175 個（4.5 GB） |
| 圖片 | 371 個（0.3 GB），其中 358 個在 `Scans/` 子資料夾 |
| 其他 | CUE 1、LOG 3、M3U 1、TXT 2 |
| 音檔大小 | 中位數 23.3 MB，最大 204 MB；96 kHz／24 bit 專輯每首平均 95 MB |
| 廣播劇／電台類 | 路徑含 Drama CD、Radio、DJCD 者 315 個、12.5 GB，占音訊總量 44% |
| 目錄深度 | 最深 5 層，典型為「系列／類別／專輯／Disc N／」 |

- 沒有 APE、TAK、WavPack，也沒有「整軌＋CUE」：全部落在 D2 的播放白名單內。
- 只有一個 FLAC 的資料夾都是真正的單軌廣播劇（11–30 分鐘、有完整標籤），不是缺 CUE 的整軌。
- 取樣率（抽樣 76 首 FLAC）：44.1 kHz／16 bit 72 首、96 kHz／24 bit 3 首、48 kHz／24 bit 1 首。

## 標籤品質

**FLAC（抽樣 76 首）**

- 74 首有 ALBUM、ARTIST、TITLE、TRACKNUMBER；Vorbis comment 全部是合法 UTF-8。
- 同義鍵混用：`ALBUM ARTIST`（17）與 `ALBUMARTIST`（11）、`DISCTOTAL` 與 `TOTALDISCS`、`TRACKTOTAL` 與 `TOTALTRACKS`；另有自訂鍵（如 `HOLO`、`LAWRENCE`）。
- 名稱混用羅馬字（`Navigation 01 - paatii keikaku`）、英文與日文。歌手欄有逗號分隔的多人，也有「角色（聲優）」寫法：`Mizunashi Akari (Hazuki Erino)`。
- STREAMINFO 音訊 MD5：56 首有值，20 首全為零（libFLAC 1.1.4／1.2.1 年代的檔案）。
- SEEKTABLE：35 首有，41 首沒有。
- 內嵌封面：只有 6 首。

**MP3**

- Persona 3 Reload：ID3v2.3，文字 frame 宣告 ISO-8859-1 但內容只有 ASCII；內嵌封面 92 KB。
- ARIA The STATION 系列的電台 MP3：完全沒有 ID3 標籤，檔名如 `DUE01.mp3`、`tri40.mp3`。
- 抽樣的 MP3 都沒有 Xing／Info／VBRI 標頭（應為 CBR，seek 可用固定位元率估算）。

## 分組難點

- **合輯資料夾**：「The Lossless Collection of Umineko Songs v3.0／Episode 1」43 首有 12 種不同的 ALBUM 標籤，21 首沒有 ALBUMARTIST，碟號寫在專輯名裡（`DISC 1`、`Disc1`、`[Disc2]`）。只依標籤分組會拆成 12 張專輯，多數只有一首。
- **資料夾名與標籤不一致**：資料夾 `Persona 3 Reload Original Soundtrack`，標籤 `Persona 3 Reload Limited Box Original Soundtrack`；資料夾 `ARIA The ANIMATION DVD Box Set Drama CD`，標籤 `ARIA The ANIMATION-DVD Bonus Drama CD`。
- **同一張專輯混合格式與標籤狀態**：ARIA The STATION 各期的 `Disc1` 是無標籤 MP3，`Disc2` 是有標籤 FLAC。
- **碟資料夾命名不一**：`Disc 1`、`Disc1`、`DISC1`、`CD 1`；`Episode N` 則是合輯的分冊，不是碟。
- **非專輯的分類資料夾**：`Album`、`Drama CD`、`OST`、`Single`。「每個子資料夾是一張專輯」只能套用在直接含音檔的資料夾。
- 資料夾名帶發布組與格式標記：`[LonE]`、`(FLAC)`、`[FLAC 96kHz／24bit]`。

## 附屬檔案

- **封面**：`Scans/` 以外的圖片只有 13 張（`cover.jpg`／`Cover.jpg` 9 張、`folder.jpg` 1 張、`VTCL-60114_01.jpg` 這類型號圖 3 張）。多數專輯既沒有根目錄封面，也沒有內嵌封面，封面只能從 `Scans/` 挑。
- **Scans 檔名**：型號式（`VICL-xxxxx_01.jpg`）、`jacket#.jpg`、`obi.jpg`、`label#.jpg`、純數字（`01.jpg`）等。
- **LOG**：3 個都是 UTF-16LE 帶 BOM（EAC）。
- **CUE**（ARIA The CREPUSCOLO Drama CD）：
  - 以 cp1252 儲存，日文在抓軌當下已被替換成「.」，無法還原；
  - 唯一的高位元組 `0xB7` 若誤用 CP932 解碼，會變成半形片假名「ｷ」；
  - `FILE` 指向 `.wav`，實際檔案是 `.flac`；
  - 3 個 `FILE` 對應 3 個 `TRACK`，屬於逐首 CUE，不需分軌。
- **M3U**：非 UTF-8。

## 已反映到其他文件的修正

- `decisions.md` D2：掃描圖預設下載、封面挑選順序、編碼偵測的合理性檢查、CUE 檔名比對、音訊 MD5 改為可選訊號。
- `p0-checklist.md`：既有曲庫原地索引、無 SEEKTABLE 的 seek 比例。

## 建議（待使用者確認）

1. ~~既有曲庫原地索引~~：已不適用。使用者確認這個曲庫存在 OneDrive，平台使用另一個 Google 帳號；遷移延後處理，選項見 `decisions.md` D6。本次讀檔頭的方式（抽樣平均每首 2–3 個小請求）仍可用於日後「從 Drive 收件匣匯入」。
2. **合輯資料夾**：同一資料夾出現多種 ALBUM 標籤時，預覽提供「依原專輯入庫，並以資料夾建立歌單（保留資料夾順序）」。資料正確，也保留使用者原本的整理方式。
3. **有聲劇／電台內容類型**：依 GENRE（如 `Spoken`）與路徑關鍵字（Drama CD、Radio、DJCD）判定。預設不進隨機播放、每首記住播放位置、曲庫可單獨篩選。本曲庫占 44%，影響明顯。
4. **專輯名內嵌碟號**：建議拆成「專輯名＋碟號」，只作為資料庫覆寫建議，保留原標籤。
5. **標籤鍵正規化**：同義鍵對應到同一欄位，原始鍵值仍保存。
6. **頻寬**：96 kHz／24 bit 每首約 95 MB，每次播放都要經 VPS 轉送（Drive → VPS → 手機）。D3 的手機端快取可減輕；部署前需確認 VPS 的流量額度。

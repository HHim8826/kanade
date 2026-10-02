import { useEffect, useState } from '../../vendor/hooks.module.js';
import { get, post } from '../api.js';
import { resetPlayer } from '../player.js';
import { href } from '../router.js';
import { ErrorBox, Icon, Spinner, fmtBytes, html, toast, useLoad } from '../ui.js';

const when = (ms) => (ms ? new Date(ms).toLocaleString('zh-TW', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false }) : '還沒有');

// DriveSync: the change feed, the full check and the inbox (P2-6).
function DriveSync() {
  const [rev, setRev] = useState(0);
  const sync = useLoad(() => get('/drive/sync'), [], rev);
  const s = sync.data || {};
  useEffect(() => { // a full check runs in the background: follow it
    if (!s.full_running) return;
    const t = setTimeout(() => setRev((n) => n + 1), 2000);
    return () => clearTimeout(t);
  }, [s.full_running, rev]);
  const reconcile = async () => {
    try {
      await post('/drive/reconcile');
      toast('開始完整對帳');
      setRev((n) => n + 1);
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const inbox = async () => {
    try {
      const r = await post('/drive/inbox');
      const later = r.waiting ? `；${r.waiting} 個檔案剛放進來，5 分鐘內沒有新檔案後再匯入` : '';
      toast((r.files ? `收件匣有 ${r.files} 個新檔案，已開始匯入` : '收件匣沒有可匯入的新檔案') + later);
      setRev((n) => n + 1);
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const res = s.last_full_result;
  return html`<h2 class="section-title">與 Drive 同步</h2>
    <div class="card pad">
      <div class="sub">每 10 分鐘檢查 Drive 的變更：在 Drive 刪除、移到垃圾桶或內容被改寫的曲庫檔案會標成「遺失」（曲庫資料不刪），還原成原本的內容後自動恢復。上次檢查：${when(s.last_checked)}</div>
      ${s.last_error && html`<div class="task-error">${s.last_error}</div>`}
      ${s.baseline_pending && html`<div class="sub state-failed">基準對帳尚未完成：開始同步或變更紀錄過期後，會自動做一次完整對帳；完成前，之前就已在 Drive 刪除的檔案可能還沒標出。</div>`}
      <div class="sub">上次完整對帳：${when(s.last_full)}${res ? `（檢查 ${res.checked} 個，標為遺失 ${res.missing}，恢復 ${res.restored}）` : ''}</div>
      <div class="actions"><button class="btn tonal" disabled=${s.full_running} onClick=${reconcile}><${Icon} name="refresh" />${s.full_running ? '對帳中…' : '完整對帳'}</button>
        <a class="btn text" href=${href('missing')}>遺失的檔案</a></div>
    </div>
    <h2 class="section-title">Drive 收件匣</h2>
    <div class="card pad">
      <div class="sub">把音樂（可含資料夾、CUE、LOG、歌詞、封面、ZIP）放進 Google Drive 的「Kanade/inbox」資料夾，會直接在 Drive 上歸檔進曲庫，不必經伺服器重新上傳；曲庫已有的檔案會移到「inbox/重複」，處理完剩下的原始檔（如已轉檔的 WAV、歌詞、封面）移到「inbox/已處理」，都不會刪除。每 10 分鐘自動檢查；剛放進來的資料夾會等 5 分鐘沒有新檔案才匯入。上次檢查：${when(s.last_inbox)}</div>
      <div class="actions"><button class="btn tonal" onClick=${inbox}><${Icon} name="download" />立即檢查收件匣</button></div>
    </div>`;
}

export function Settings({ onLogout }) {
  const status = useLoad(() => get('/status'), []);
  const drive = useLoad(() => get('/drive'), []);
  const connect = async () => {
    try {
      const { url } = await post('/drive/auth', {});
      location.href = url; // Google returns to /oauth/google/callback on this server
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const logout = async () => {
    resetPlayer(); // stop and report the playback while the login still works (review #14)
    await post('/logout').catch(() => {});
    onLogout();
  };
  const d = drive.data;
  return html`<section>
    <h1 class="page-title">設定</h1>
    <h2 class="section-title">Google Drive</h2>
    <div class="card pad">
      ${drive.loading && html`<${Spinner} />`}
      <${ErrorBox} error=${drive.error} onRetry=${drive.reload} />
      ${d && (d.status.connected
        ? html`<div class="title">${d.account.email}</div>
            <div class="sub">已使用 ${fmtBytes(d.account.usage_bytes)}${d.account.limit_bytes ? ` / ${fmtBytes(d.account.limit_bytes)}` : ''}</div>
            ${d.status.testing_mode && html`<div class="task-error">OAuth 應用程式仍在「測試中」，授權 7 天後失效。</div>`}`
        : html`<div class="title">尚未連線</div>`)}
      <div class="actions"><button class="btn tonal" onClick=${connect}><${Icon} name="refresh" />${d && d.status.connected ? '重新連線' : '連線 Google Drive'}</button></div>
    </div>
    ${d && d.status.connected && html`<${DriveSync} />`}
    <h2 class="section-title">服務</h2>
    <div class="card pad">
      ${status.data && html`<div class="sub">下載器（aria2）：${status.data.aria2_ready ? '運作中' : '未就緒'}</div>
        <div class="sub">格式轉換與 CUE 分軌（FFmpeg）：${status.data.ffmpeg ? '可用' : '未安裝'}</div>
        ${status.data.disk && html`<div class=${'sub' + (status.data.disk.low ? ' state-failed' : '')}>磁碟：剩 ${fmtBytes(status.data.disk.free_bytes)}（保留 ${fmtBytes(status.data.disk.reserve_bytes)}）${status.data.disk.low ? '，空間不足' : ''}</div>`}
        <div class="sub">已運行 ${Math.floor(status.data.uptime_seconds / 3600)} 小時 ${Math.floor((status.data.uptime_seconds % 3600) / 60)} 分</div>`}
    </div>
    <h2 class="section-title">帳號</h2>
    <div class="actions"><button class="btn outlined" onClick=${logout}><${Icon} name="logout" />登出</button></div>
  </section>`;
}

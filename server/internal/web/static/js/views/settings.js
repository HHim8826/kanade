import { useEffect, useState } from '../../vendor/hooks.module.js';
import { api, getInitial, post } from '../api.js';
import { addPasskey, passkeyMessage, passkeysSupported } from '../passkey.js';
import { resetPlayer } from '../player.js';
import { href } from '../router.js';
import { loadTheme, modes, setTheme, themes } from '../theme.js';
import { Dialog, ErrorBox, Icon, IconButton, Spinner, fmtBytes, html, showDialog, toast, useInitialLoad, useLoad } from '../ui.js';

const when = (ms) => (ms ? new Date(ms).toLocaleString('zh-TW', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false }) : '還沒有');

// DriveSync: the change feed, the full check and the inbox (P2-6).
function DriveSync({ sync }) {
  const s = sync.data || {};
  useEffect(() => { // a full check runs in the background: follow it
    if (!s.full_running || sync.loading) return;
    const t = setTimeout(sync.reload, 2000);
    return () => clearTimeout(t);
  }, [s.full_running, sync.loading]);
  const reconcile = async () => {
    try {
      await post('/drive/reconcile');
      toast('開始完整對帳');
      sync.reload();
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const inbox = async () => {
    try {
      const r = await post('/drive/inbox');
      const later = r.waiting ? `；${r.waiting} 個檔案剛放進來，5 分鐘內沒有新檔案後再匯入` : '';
      toast((r.files ? `收件匣有 ${r.files} 個新檔案，已開始匯入` : '收件匣沒有可匯入的新檔案') + later);
      sync.reload();
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const res = s.last_full_result;
  return html`<h2 class="section-title">與 Drive 同步</h2>
    <div class="card pad">
      <${ErrorBox} error=${sync.error} onRetry=${sync.reload} />
      ${sync.loading && !sync.data && html`<${Spinner} />`}
      <div class="sub">每 10 分鐘檢查 Drive 的變更：在 Drive 刪除、移到垃圾桶或內容被改寫的曲庫檔案會標成「遺失」（曲庫資料不刪），還原成原本的內容後自動恢復。上次檢查：${sync.data ? when(s.last_checked) : '—'}</div>
      ${s.last_error && html`<div class="task-error">${s.last_error}</div>`}
      ${s.trash_pending > 0 && html`<div class="sub state-failed">${s.trash_pending} 個已永久刪除的檔案還沒移到 Drive 垃圾桶，會自動重試${s.trash_error ? `（上次錯誤：${s.trash_error}）` : ''}。</div>`}
      ${s.baseline_pending && html`<div class="sub state-failed">基準對帳尚未完成：開始同步或變更紀錄過期後，會自動做一次完整對帳；完成前，之前就已在 Drive 刪除的檔案可能還沒標出。</div>`}
      <div class="sub">上次完整對帳：${sync.data ? when(s.last_full) : '—'}${res ? `（檢查 ${res.checked} 個，標為遺失 ${res.missing}，恢復 ${res.restored}）` : ''}</div>
      <div class="actions"><button class="btn tonal" disabled=${s.full_running} onClick=${reconcile}><${Icon} name="refresh" />${s.full_running ? '對帳中…' : '完整對帳'}</button>
        <a class="btn text" href=${href('missing')}>遺失的檔案</a></div>
    </div>
    <h2 class="section-title">Drive 收件匣</h2>
    <div class="card pad">
      <div class="sub">把音樂（可含資料夾、CUE、LOG、歌詞、封面、ZIP）放進 Google Drive 的「Kanade/inbox」資料夾，會直接在 Drive 上歸檔進曲庫，不必經伺服器重新上傳；曲庫已有的檔案會移到「inbox/重複」，處理完剩下的原始檔（如已轉檔的 WAV、歌詞、封面）移到「inbox/已處理」，都不會刪除。每 10 分鐘自動檢查；剛放進來的資料夾會等 5 分鐘沒有新檔案才匯入。上次檢查：${sync.data ? when(s.last_inbox) : '—'}</div>
      <div class="actions"><button class="btn tonal" onClick=${inbox}><${Icon} name="download" />立即檢查收件匣</button></div>
    </div>`;
}

// Appearance: the color theme and light or dark, applied at once and kept in this browser.
function Appearance() {
  const [t, setT] = useState(loadTheme);
  const choose = (patch) => {
    const next = { ...t, ...patch };
    setT(next);
    setTheme(next);
  };
  return html`<h2 class="section-title">外觀</h2>
    <div class="card pad">
      <div class="sub">配色</div>
      <div class="choices" role="radiogroup" aria-label="配色">
        ${themes.map(([k, label]) => html`<button type="button" class="choice" role="radio" aria-checked=${t.theme === k}
          onClick=${() => choose({ theme: k })}><span class=${'swatch ' + k}></span>${label}</button>`)}
      </div>
      <div class="sub">明暗</div>
      <div class="choices" role="radiogroup" aria-label="明暗">
        ${modes.map(([k, label]) => html`<button type="button" class="choice" role="radio" aria-checked=${t.mode === k}
          onClick=${() => choose({ mode: k })}>${label}</button>`)}
      </div>
      <div class="sub">只記在這個瀏覽器。</div>
    </div>`;
}

// deviceName guesses a name for a passkey made on this device.
function deviceName() {
  const ua = navigator.userAgent;
  const os = /iPhone|iPad/.test(ua) ? 'iPhone' : /Android/.test(ua) ? 'Android' : /Mac OS X/.test(ua) ? 'Mac'
    : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : '';
  const browser = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : '';
  return [os, browser].filter(Boolean).join(' · ') || 'Passkey';
}

// Passkeys: adding one (after the password) makes the login page offer it.
function Passkeys({ data }) {
  const add = () => showDialog((close) => html`<${AddPasskey} close=${close} onAdded=${data.reload} />`);
  const rename = async (p) => {
    const name = prompt('Passkey 名稱', p.name);
    if (!name || name === p.name) return;
    try {
      await api('PATCH', `/passkeys/${p.id}`, { name });
      data.reload();
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const remove = async (p) => {
    if (!confirm(`移除 passkey「${p.name}」？之後就不能用它登入（裝置上的 passkey 也可以一併刪除）。`)) return;
    try {
      await api('DELETE', `/passkeys/${p.id}`);
      toast('已移除 passkey');
      data.reload();
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const list = data.data || [];
  return html`<div class="card pad">
    <div class="title">Passkey</div>
    <div class="sub">加入 passkey 後，登入頁會出現「使用 passkey 登入」，用裝置的指紋、臉部或螢幕鎖定登入，不必輸入密碼；密碼仍然可以使用。</div>
    <${ErrorBox} error=${data.error} onRetry=${data.reload} />
    ${data.loading && !data.data && html`<${Spinner} />`}
    ${list.length > 0 && html`<ul class="list passkeys">${list.map((p) => html`<li key=${p.id} class="row">
      <${Icon} name="passkey" />
      <span class="grow"><span class="title">${p.name}</span>
        <span class="sub">加入於 ${when(p.created_at)} · 上次使用 ${p.last_used_at ? when(p.last_used_at) : '還沒有'}</span></span>
      <${IconButton} icon="edit" label=${`重新命名「${p.name}」`} onClick=${() => rename(p)} />
      <${IconButton} icon="delete" label=${`移除「${p.name}」`} onClick=${() => remove(p)} />
    </li>`)}</ul>`}
    <div class="actions">
      ${passkeysSupported()
        ? html`<button class="btn tonal" onClick=${add}><${Icon} name="passkey" />新增 passkey</button>`
        : html`<span class="sub">這個瀏覽器不支援 passkey。</span>`}
    </div>
  </div>`;
}

function AddPasskey({ close, onAdded }) {
  const [name, setName] = useState(deviceName);
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await addPasskey(password, name.trim());
      toast('已加入 passkey，之後可以在登入頁使用');
      onAdded();
      close();
    } catch (err) {
      setError(new Error(passkeyMessage(err) || '已取消，沒有加入 passkey。'));
      setBusy(false);
    }
  };
  return html`<${Dialog} title="新增 passkey" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button type="submit" form="passkey-form" class="btn filled" disabled=${busy || !password}>${busy ? '請在裝置上確認…' : '繼續'}</button>`}>
    <form id="passkey-form" onSubmit=${submit}>
      <p class="hint">先輸入目前的密碼確認是你本人，接著依裝置的提示建立 passkey（指紋、臉部或螢幕鎖定）。</p>
      <label class="field"><span>名稱</span><input value=${name} maxlength="60" onInput=${(e) => setName(e.target.value)} /></label>
      <label class="field"><span>目前的密碼</span><input type="password" autocomplete="current-password" value=${password}
        onInput=${(e) => setPassword(e.target.value)} required /></label>
      <${ErrorBox} error=${error} />
    </form>
  <//>`;
}

export function Settings({ onLogout }) {
  const status = useLoad(() => getInitial('/status'), []);
  const drive = useLoad(() => getInitial('/drive'), []);
  const passkeys = useLoad(() => getInitial('/passkeys'), []);
  const connected = !!(drive.data && drive.data.status.connected);
  const sync = useLoad(() => connected ? getInitial('/drive/sync') : Promise.resolve(null), [connected]);
  const ready = useInitialLoad([status, drive, passkeys, sync]);
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
  if (!ready) return html`<section aria-busy="true"><h1 class="page-title">設定</h1><${Spinner} /></section>`;
  return html`<section>
    <h1 class="page-title">設定</h1>
    <h2 class="section-title">Google Drive</h2>
    <div class="card pad">
      ${drive.loading && !d && html`<${Spinner} />`}
      <${ErrorBox} error=${drive.error} onRetry=${drive.reload} />
      ${d && (d.status.connected
        ? html`<div class="title">${d.account.email}</div>
            <div class="sub">已使用 ${fmtBytes(d.account.usage_bytes)}${d.account.limit_bytes ? ` / ${fmtBytes(d.account.limit_bytes)}` : ''}</div>
            ${d.status.testing_mode && html`<div class="task-error">OAuth 應用程式仍在「測試中」，授權 7 天後失效。</div>`}`
        : html`<div class="title">尚未連線</div>`)}
      <div class="actions"><button class="btn tonal" disabled=${drive.loading} onClick=${connect}><${Icon} name="refresh" />${connected ? '重新連線' : '連線 Google Drive'}</button></div>
    </div>
    ${connected && html`<${DriveSync} sync=${sync} />`}
    <${Appearance} />
    <h2 class="section-title">服務</h2>
    <div class="card pad">
      <${ErrorBox} error=${status.error} onRetry=${status.reload} />
      ${status.loading && !status.data && html`<${Spinner} />`}
      ${status.data && html`<div class="sub">下載器（aria2）：${status.data.aria2_ready ? '運作中' : '未就緒'}</div>
        <div class="sub">格式轉換與 CUE 分軌（FFmpeg）：${status.data.ffmpeg ? '可用' : '未安裝'}</div>
        ${status.data.disk && html`<div class=${'sub' + (status.data.disk.low ? ' state-failed' : '')}>磁碟：剩 ${fmtBytes(status.data.disk.free_bytes)}（保留 ${fmtBytes(status.data.disk.reserve_bytes)}）${status.data.disk.low ? '，空間不足' : ''}</div>`}
        <div class="sub">已運行 ${Math.floor(status.data.uptime_seconds / 3600)} 小時 ${Math.floor((status.data.uptime_seconds % 3600) / 60)} 分</div>`}
    </div>
    <h2 class="section-title">帳號</h2>
    <${Passkeys} data=${passkeys} />
    <div class="actions"><button class="btn outlined" onClick=${logout}><${Icon} name="logout" />登出</button></div>
  </section>`;
}

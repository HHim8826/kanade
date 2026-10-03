import { useEffect, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { addPasskey, passkeyMessage, passkeysSupported } from '../passkey.js';
import { resetPlayer } from '../player.js';
import { href } from '../router.js';
import { loadTheme, modes, setTheme, themes } from '../theme.js';
import { Dialog, ErrorBox, Icon, IconButton, Spinner, fmtBytes, html, showDialog, toast, useLoad } from '../ui.js';

const when = (ms) => (ms ? new Date(ms).toLocaleString('zh-TW', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false }) : '還沒有');

// The page's reads give up after ten seconds; a read is { data } or { error }.
const read = (path) => get(path, { timeout: 10000 }).then((data) => ({ data }), (error) => ({ error }));

// useSection keeps a section's data, from the page's first read (null: read it now), and reloads
// that section alone: what the page shows stays in place while it does.
function useSection(path, first) {
  const [s, setS] = useState(first || { loading: true });
  const reload = () => {
    setS((v) => ({ ...v, loading: true }));
    read(path).then((r) => setS((v) => ({ data: r.data ?? v.data, error: r.error || null })));
  };
  useEffect(() => {
    if (!first) reload();
  }, []);
  return { ...s, reload };
}

// DriveSync: the change feed, the full check and the inbox (P2-6).
function DriveSync({ first }) {
  const sync = useSection('/drive/sync', first);
  const s = sync.data || {};
  useEffect(() => { // a full check runs in the background: follow it
    if (!s.full_running) return;
    const t = setTimeout(sync.reload, 2000);
    return () => clearTimeout(t);
  }, [sync.data]);
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
      ${!sync.data && sync.loading && html`<${Spinner} />`}
      <${ErrorBox} error=${sync.error} onRetry=${sync.reload} />
      <div class="sub">每 10 分鐘檢查 Drive 的變更：在 Drive 刪除、移到垃圾桶或內容被改寫的曲庫檔案會標成「遺失」（曲庫資料不刪），還原成原本的內容後自動恢復。上次檢查：${when(s.last_checked)}</div>
      ${s.last_error && html`<div class="task-error">${s.last_error}</div>`}
      ${s.trash_pending > 0 && html`<div class="sub state-failed">${s.trash_pending} 個已永久刪除的檔案還沒移到 Drive 垃圾桶，會自動重試${s.trash_error ? `（上次錯誤：${s.trash_error}）` : ''}。</div>`}
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
function Passkeys({ first }) {
  const data = useSection('/passkeys', first);
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

// copyText: the clipboard API needs https; reached over plain http (an IP address), the old way.
async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const t = document.createElement('textarea');
    t.value = text;
    document.body.append(t);
    t.select();
    const ok = document.execCommand('copy');
    t.remove();
    if (!ok) throw new Error('無法複製，請自己選取後複製。');
  }
}

const outLink = (url, text) => html`<a class="link" href=${url} target="_blank" rel="noopener noreferrer">${text}</a>`;
const cloud = (path, text) => outLink('https://console.cloud.google.com/' + path, text);
const unverified = '出現「Google 尚未驗證這個應用程式」時，這是你自己建立的用戶端，按「進階」→「前往…」繼續';

// Drive: Kanade reaches Drive with the administrator's own OAuth client from Google Cloud, so first
// that client, then the connection.
function Drive({ drive }) {
  const d = drive.data;
  const st = d && d.status;
  const setup = () => showDialog((close) => html`<${ClientSetup} status=${st} close=${close} onSaved=${drive.reload} />`);
  const connect = async () => {
    if (st.paste) {
      showDialog((close) => html`<${PasteConnect} close=${close} onDone=${drive.reload} />`);
      return;
    }
    try {
      const { url } = await post('/drive/auth', {});
      location.href = url; // Google returns to /oauth/google/callback on this server
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  return html`<div class="card pad">
    <${ErrorBox} error=${drive.error} onRetry=${drive.reload} />
    ${st && !st.has_client && html`<div class="title">尚未設定</div>
      <div class="sub">Kanade 透過你自己在 Google Cloud 建立的 OAuth 用戶端存取 Google Drive，音樂存在你的 Drive。先建立用戶端（約 10 分鐘，設定頁會帶著做），再連線。</div>
      <div class="actions"><button class="btn filled" onClick=${setup}>設定 OAuth 用戶端</button></div>`}
    ${st && st.has_client && html`${st.connected
        ? html`<div class="title">${d.account.email}</div>
          <div class="sub">已使用 ${fmtBytes(d.account.usage_bytes)}${d.account.limit_bytes ? ` / ${fmtBytes(d.account.limit_bytes)}` : ''}</div>
          ${st.testing_mode && html`<div class="task-error">OAuth 應用程式仍在「測試中」，授權 7 天後失效。</div>`}`
        : html`<div class="title">尚未連線</div>
          <div class="sub">連線時選擇要存放音樂的 Google 帳號，並允許存取 Google Drive。${unverified}。</div>`}
      <div class="sub break">OAuth 用戶端：${st.client_id}</div>
      <div class="actions"><button class="btn tonal" onClick=${connect}><${Icon} name="refresh" />${st.connected ? '重新連線' : '連線 Google Drive'}</button>
        <button class="btn text" onClick=${setup}>更換用戶端</button></div>`}
  </div>`;
}

// ClientSetup: creating the OAuth client in Google Cloud step by step, with this server's redirect
// URI to register; then its ID and secret, typed or read from the JSON Google offers to download.
function ClientSetup({ status, close, onSaved }) {
  const [id, setId] = useState('');
  const [secret, setSecret] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const pick = async (e) => {
    const f = e.target.files[0];
    if (!f) return;
    try {
      const j = JSON.parse(await f.text());
      const c = j.web || j.installed || j;
      if (!c.client_id || !c.client_secret) throw new Error();
      setId(c.client_id);
      setSecret(c.client_secret);
      setError(null);
    } catch {
      setError(new Error('這不是 Google 的 OAuth 用戶端 JSON 檔。'));
    }
  };
  const save = async (e) => {
    e.preventDefault();
    if (!id.trim().endsWith('.apps.googleusercontent.com')) {
      setError(new Error('用戶端 ID 應該以 .apps.googleusercontent.com 結尾。'));
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await post('/drive/client', { client_id: id.trim(), client_secret: secret.trim() });
      toast('已儲存 OAuth 用戶端，接著按「連線 Google Drive」');
      onSaved();
      close();
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  const copy = () => copyText(status.redirect_uri).then(() => toast('已複製重新導向 URI'), (e) => toast(e.message, 'error'));
  return html`<${Dialog} title="設定 OAuth 用戶端" wide onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button type="submit" form="client-form" class="btn filled" disabled=${busy || !id.trim() || !secret.trim()}>${busy ? '儲存中…' : '儲存'}</button>`}>
    <p class="hint">Kanade 用你自己在 Google Cloud 建立的 OAuth 用戶端存取 Drive，不經過其他服務。請用要存放音樂的 Google 帳號操作：</p>
    <ol class="steps">
      <li>${cloud('projectcreate', '建立 Google Cloud 專案')}（或選一個現有的），然後${cloud('apis/library/drive.googleapis.com', '啟用 Google Drive API')}。</li>
      <li>到 ${cloud('auth/overview', 'Google Auth Platform')} 按「開始」：應用程式名稱填 Kanade，填支援與聯絡信箱，目標對象選「外部」。</li>
      <li>到${cloud('auth/audience', '「目標對象」')}按「發布應用程式」改為正式版。若停在「測試中」，要把自己的帳號加為測試使用者，而且授權每 7 天就失效。</li>
      <li>到${cloud('auth/clients/create', '「用戶端」建立用戶端')}：應用程式類型選「網頁應用程式」，在「已授權的重新導向 URI」新增下面這個網址。</li>
      <li>按「建立」後，用戶端密鑰只會顯示這一次：按「下載 JSON」，在下面選擇這個檔案；或複製用戶端 ID 與密鑰貼上。</li>
    </ol>
    <div class="field"><span>重新導向 URI</span>
      <div class="inline-form"><input readonly value=${status.redirect_uri} onFocus=${(e) => e.target.select()} />
        <button type="button" class="btn tonal" onClick=${copy}><${Icon} name="copy" />複製</button></div></div>
    ${status.paste && html`<p class="hint">這台的公開網址不是 https 網域，Google 不會把瀏覽器導回這裡，所以用 localhost：連線時，授權後瀏覽器會停在一個打不開的頁面，把那個網址貼回 Kanade 就完成了。之後設好 https 網域、改了公開網址（kanade-manager config），要在用戶端加上新的重新導向 URI。</p>`}
    <form id="client-form" onSubmit=${save}>
      <label class="field"><span>下載的 JSON 檔</span><input type="file" accept=".json,application/json" onChange=${pick} /></label>
      <label class="field"><span>用戶端 ID</span><input value=${id} autocomplete="off" spellcheck="false" placeholder="….apps.googleusercontent.com"
        onInput=${(e) => setId(e.target.value)} /></label>
      <label class="field"><span>用戶端密鑰</span><input type="password" value=${secret} autocomplete="off" onInput=${(e) => setSecret(e.target.value)} /></label>
      ${status.connected && html`<p class="hint">換成另一個用戶端後，要重新連線 Google Drive。</p>`}
      <${ErrorBox} error=${error} />
    </form>
  <//>`;
}

// PasteConnect: connecting where Google cannot send the browser back to Kanade (no https domain):
// Google's page in another tab, then the address the browser ends on, pasted here.
function PasteConnect({ close, onDone }) {
  const [authURL, setAuthURL] = useState(null);
  const [pasted, setPasted] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  useEffect(() => {
    post('/drive/auth', {}).then((r) => setAuthURL(r.url), setError);
  }, []);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await post('/drive/auth/paste', { url: pasted.trim() });
      toast('已連線 Google Drive');
      onDone();
      close();
    } catch (err) {
      setError(/state/.test(err.message) ? new Error('這不是這次授權的網址，或已超過一小時。請重新開啟 Google 授權頁再試一次。')
        : /not granted/.test(err.message) ? new Error('Google 沒有授權（在授權頁按了取消）。') : err);
      setBusy(false);
    }
  };
  return html`<${Dialog} title="連線 Google Drive" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button type="submit" form="paste-form" class="btn filled" disabled=${busy || !/[?&]state=/.test(pasted)}>${busy ? '連線中…' : '完成連線'}</button>`}>
    <ol class="steps">
      <li>${authURL ? outLink(authURL, '開啟 Google 授權頁') : '準備授權頁…'}（新分頁），選擇要存放音樂的帳號，允許存取 Google Drive。${unverified}。</li>
      <li>允許後，瀏覽器會打開一個無法連線的 localhost 頁面，這是正常的：複製網址列裡的整個網址。</li>
      <li>貼在下面，按「完成連線」。</li>
    </ol>
    <form id="paste-form" onSubmit=${submit}>
      <label class="field"><span>授權後的網址</span>
        <textarea class="paste-box" rows="3" value=${pasted} placeholder="http://localhost/oauth/google/callback?state=…&code=…"
          onInput=${(e) => setPasted(e.target.value)}></textarea></label>
      <${ErrorBox} error=${error} />
    </form>
  <//>`;
}

// Settings shows once its first reads are done (each waits at most ten seconds), all at once and in
// a fixed order: the Drive connection decides whether the sync and inbox sections are there, so no
// section appears above one already shown. A read that failed shows its error in its section.
export function Settings({ onLogout }) {
  const first = useLoad(async () => {
    const [status, drive, passkeys] = await Promise.all([read('/status'), read('/drive'), read('/passkeys')]);
    const sync = drive.data && drive.data.status.connected ? await read('/drive/sync') : null;
    return { status, drive, passkeys, sync };
  }, []);
  if (!first.data) return html`<section><h1 class="page-title">設定</h1><div class="spinner late"></div></section>`;
  return html`<${SettingsPage} first=${first.data} onLogout=${onLogout} />`;
}

function SettingsPage({ first, onLogout }) {
  const status = useSection('/status', first.status);
  const drive = useSection('/drive', first.drive);
  const logout = async () => {
    resetPlayer(); // stop and report the playback while the login still works (review #14)
    await post('/logout').catch(() => {});
    onLogout();
  };
  const d = drive.data;
  const st = status.data;
  return html`<section>
    <h1 class="page-title">設定</h1>
    <h2 class="section-title">Google Drive</h2>
    <${Drive} drive=${drive} />
    ${d && d.status.connected && html`<${DriveSync} first=${first.sync} />`}
    <${Appearance} />
    <h2 class="section-title">服務</h2>
    <div class="card pad">
      <${ErrorBox} error=${status.error} onRetry=${status.reload} />
      ${st && html`<div class="sub">下載器（aria2）：${st.aria2_ready ? '運作中' : '未就緒'}</div>
        <div class="sub">格式轉換與 CUE 分軌（FFmpeg）：${st.ffmpeg ? '可用' : '未安裝'}</div>
        ${st.disk && html`<div class=${'sub' + (st.disk.low ? ' state-failed' : '')}>磁碟：剩 ${fmtBytes(st.disk.free_bytes)}（保留 ${fmtBytes(st.disk.reserve_bytes)}）${st.disk.low ? '，空間不足' : ''}</div>`}
        <div class="sub">已運行 ${Math.floor(st.uptime_seconds / 3600)} 小時 ${Math.floor((st.uptime_seconds % 3600) / 60)} 分</div>`}
    </div>
    <h2 class="section-title">帳號</h2>
    <${Passkeys} first=${first.passkeys} />
    <div class="actions"><button class="btn outlined" onClick=${logout}><${Icon} name="logout" />登出</button></div>
  </section>`;
}

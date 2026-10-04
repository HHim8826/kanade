import { useEffect, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { addPasskey, passkeyMessage, passkeysSupported } from '../passkey.js';
import { player, resetPlayer, setMode, setPrefs } from '../player.js';
import { href } from '../router.js';
import { loadTheme, modes, setTheme, themes } from '../theme.js';
import { useStore } from '../store.js';
import { StatsTimeZone } from './stats.js';
import { Dialog, ErrorBox, FilePick, Icon, IconButton, Spinner, fmtBytes, html, showDialog, toast, useLoad } from '../ui.js';

const when = (ms) => (ms ? new Date(ms).toLocaleString('zh-TW', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false }) : '還沒有');

// The page's reads give up after ten seconds; a read is { data } or { error }.
const read = (path) => get(path, { timeout: 10000 }).then((data) => ({ data }), (error) => ({ error }));

// useSection keeps a section's data, from the page's first read (null: read it now), and reloads
// that section alone: what the page shows stays in place while it does.
function useSection(path, first) {
  const [s, setS] = useState(first || { loading: true });
  // reload reads the section again; given data (a save's answer), it shows that instead.
  const reload = (data) => {
    if (data && !(data instanceof Event)) {
      setS({ data, error: null });
      return;
    }
    setS((v) => ({ ...v, loading: true }));
    read(path).then((r) => setS((v) => ({ data: r.data ?? v.data, error: r.error || null })));
  };
  useEffect(() => {
    if (!first) reload();
  }, []);
  return { ...s, reload };
}

// DriveSync: the change feed, the full check and the inbox (P2-6), and how often they run (#77).
function DriveSync({ first, conf }) {
  const sync = useSection('/drive/sync', first);
  const s = sync.data || {};
  const d = conf.data ? conf.data.drive : { check_minutes: 10, settle_minutes: 5, auto_inbox: true };
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
      const later = r.waiting ? `；${r.waiting} 個檔案剛放進來，${d.settle_minutes} 分鐘內沒有新檔案後再匯入` : '';
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
      <div class="sub">每 ${d.check_minutes} 分鐘檢查 Drive 的變更：在 Drive 刪除、移到垃圾桶或內容被改寫的曲庫檔案會標成「遺失」（曲庫資料不刪），還原成原本的內容後自動恢復。上次檢查：${when(s.last_checked)}</div>
      ${s.last_error && html`<div class="task-error">${s.last_error}</div>`}
      ${s.trash_pending > 0 && html`<div class="sub state-failed">${s.trash_pending} 個已永久刪除的檔案還沒移到 Drive 垃圾桶，會自動重試${s.trash_error ? `（上次錯誤：${s.trash_error}）` : ''}。</div>`}
      ${s.baseline_pending && html`<div class="sub state-failed">基準對帳尚未完成：開始同步或變更紀錄過期後，會自動做一次完整對帳；完成前，之前就已在 Drive 刪除的檔案可能還沒標出。</div>`}
      <div class="sub">上次完整對帳：${when(s.last_full)}${res ? `（檢查 ${res.checked} 個，標為遺失 ${res.missing}，恢復 ${res.restored}）` : ''}</div>
      <div class="actions"><button class="btn tonal" disabled=${s.full_running} onClick=${reconcile}><${Icon} name="refresh" />${s.full_running ? '對帳中…' : '完整對帳'}</button>
        <a class="btn text" href=${href('missing')}>遺失的檔案</a></div>
    </div>
    <h2 class="section-title">Drive 收件匣</h2>
    <div class="card pad">
      <div class="sub">把音樂（可含資料夾、CUE、LOG、歌詞、封面、ZIP）放進 Google Drive 的「Kanade/inbox」資料夾，會直接在 Drive 上歸檔進曲庫，不必經伺服器重新上傳；曲庫已有的檔案會移到「inbox/重複」，處理完剩下的原始檔（如已轉檔的 WAV、歌詞、封面）移到「inbox/已處理」，都不會刪除。${d.auto_inbox
        ? `每 ${d.check_minutes} 分鐘自動檢查；${d.settle_minutes ? `剛放進來的資料夾會等 ${d.settle_minutes} 分鐘沒有新檔案才匯入` : '新檔案下次檢查就匯入'}。`
        : '自動匯入已關閉：按「立即檢查收件匣」才會匯入。'}上次檢查：${when(s.last_inbox)}</div>
      <div class="actions"><button class="btn tonal" onClick=${inbox}><${Icon} name="download" />立即檢查收件匣</button></div>
      <${DriveTiming} conf=${conf} />
    </div>`;
}

// useForm keeps a form's values beside the saved ones it started from; changed tells them apart.
function useForm(saved) {
  const [v, setV] = useState(saved);
  useEffect(() => setV(saved), [JSON.stringify(saved)]);
  return [v, (patch) => setV((x) => ({ ...x, ...patch })), JSON.stringify(v) !== JSON.stringify(saved)];
}

// saveSection PUTs a settings group and shows the answer (the settings now) in the page.
async function saveSection(conf, path, body, done) {
  try {
    const r = await api('PUT', path, body);
    conf.reload(r);
    toast(done);
  } catch (e) {
    toast(e.message, 'error');
  }
}

const num = (v) => (v === '' ? NaN : Number(v));

// Toggle is a switch with its label and, below, what it means.
function Toggle({ label, sub, checked, onChange, disabled }) {
  return html`<label class="toggle-row">
    <span class="grow"><span class="title">${label}</span>${sub && html`<span class="sub">${sub}</span>`}</span>
    <input type="checkbox" role="switch" checked=${checked} disabled=${disabled} onChange=${(e) => onChange(e.target.checked)} />
  </label>`;
}

// NumberField is a whole number (or with step, a decimal) with its unit.
function NumberField({ label, value, unit, min, max, step = 1, onInput, hint }) {
  return html`<label class="field num-field"><span>${label}</span>
    <span class="with-unit"><input type="number" inputmode=${step < 1 ? 'decimal' : 'numeric'} min=${min} max=${max} step=${step}
      value=${value} onInput=${(e) => onInput(e.target.value)} /><span class="unit">${unit}</span></span>
    ${hint && html`<span class="hint tight">${hint}</span>`}</label>`;
}

// DriveTiming: whether the inbox is imported by itself, how often Drive is looked at, and how long
// an inbox folder must go without new files (review #77).
function DriveTiming({ conf }) {
  const saved = conf.data ? conf.data.drive : null;
  const [v, set, changed] = useForm(saved || {});
  if (!saved) return null;
  const ok = num(v.check_minutes) >= 1 && num(v.check_minutes) <= 1440 && num(v.settle_minutes) >= 0 && num(v.settle_minutes) <= 1440;
  const save = () => saveSection(conf, '/settings/drive',
    { auto_inbox: v.auto_inbox, check_minutes: num(v.check_minutes), settle_minutes: num(v.settle_minutes) }, '已儲存 Drive 同步設定');
  return html`<div class="settings-form">
    <${Toggle} label="自動匯入收件匣" sub="關閉後收件匣只在按「立即檢查收件匣」時匯入；Drive 變更與垃圾桶的同步照常進行。"
      checked=${v.auto_inbox} onChange=${(on) => set({ auto_inbox: on })} />
    <div class="form-grid">
      <${NumberField} label="檢查間隔" unit="分鐘" min="1" max="1440" value=${v.check_minutes} onInput=${(x) => set({ check_minutes: x })}
        hint="檢查 Drive 的變更與收件匣" />
      <${NumberField} label="收件匣等待時間" unit="分鐘" min="0" max="1440" value=${v.settle_minutes} onInput=${(x) => set({ settle_minutes: x })}
        hint="資料夾這段時間沒有新檔案才匯入，避免還在複製的專輯被拆開" />
    </div>
    <div class="actions"><button class="btn filled" disabled=${!changed || !ok} onClick=${save}>儲存</button></div>
  </div>`;
}

// Downloads: speed limits, how many at once, connections and seeding (review #75).
function DownloadSettings({ conf }) {
  const saved = conf.data ? conf.data.downloads : null;
  const [v, set, changed] = useForm(saved || {});
  if (!saved) return null;
  const n = (k) => num(v[k]);
  const ok = n('down_kib') >= 0 && n('up_kib') >= 0 && n('concurrent') >= 1 && n('concurrent') <= 5 && n('max_peers') >= 1 &&
    n('max_peers') <= 500 && n('seed_ratio') >= 0 && n('seed_hours') >= 0;
  const save = () => saveSection(conf, '/settings/downloads', {
    down_kib: n('down_kib'), up_kib: n('up_kib'), concurrent: n('concurrent'), max_peers: n('max_peers'),
    seed: v.seed, seed_ratio: n('seed_ratio'), seed_hours: n('seed_hours'),
  }, '已儲存下載設定');
  const forever = v.seed && !n('seed_ratio') && !n('seed_hours');
  return html`<h2 class="section-title">下載</h2>
    <div class="card pad settings-form">
      <div class="form-grid">
        <${NumberField} label="下載速度上限" unit="KB/s" min="0" value=${v.down_kib} onInput=${(x) => set({ down_kib: x })} hint="0 為不限速" />
        <${NumberField} label="上傳速度上限" unit="KB/s" min="0" value=${v.up_kib} onInput=${(x) => set({ up_kib: x })} hint="0 為不限速" />
        <${NumberField} label="同時下載" unit="個" min="1" max="5" value=${v.concurrent} onInput=${(x) => set({ concurrent: x })}
          hint="同時下載的種子仍共用暫存空間，放不下的會等待" />
        <${NumberField} label="每個種子的連線數上限" unit="個" min="1" max="500" value=${v.max_peers} onInput=${(x) => set({ max_peers: x })} />
      </div>
      <${Toggle} label="下載完成後做種" sub="做種時檔案留在伺服器；停止後，已存進曲庫的檔案才會清除，沒存進去的會保留。"
        checked=${v.seed} onChange=${(on) => set({ seed: on })} />
      ${v.seed && html`<div class="form-grid">
        <${NumberField} label="分享率達到時停止" unit="倍" min="0" step="0.1" value=${v.seed_ratio} onInput=${(x) => set({ seed_ratio: x })}
          hint="上傳量 ÷ 下載量；0 為不看分享率" />
        <${NumberField} label="做種時間達到時停止" unit="小時" min="0" value=${v.seed_hours} onInput=${(x) => set({ seed_hours: x })}
          hint="從下載完成算起；0 為不限時間" />
      </div>`}
      <p class="hint">${forever ? '分享率與時間都不限：會一直做種，直到在任務頁按「停止做種」。'
        : v.seed ? '分享率或時間先達到的就停止。' : '下載完成、交給匯入後就停止，不做種。'}
        速度與連線數立即套用到進行中的下載；同時下載數與做種條件在 2 秒內套用到所有下載。重新啟動服務後，做種中的下載會繼續做種。</p>
      <div class="actions"><button class="btn filled" disabled=${!changed || !ok} onClick=${save}>儲存</button></div>
    </div>`;
}

const flagName = { cache_mib: '--cache-mib', staging_mib: '--staging-mib', reserve_gib: '--reserve-gib' };

// Storage: the stream cache, the staging space and the disk reserve, with what they use; the
// cache can be emptied (review #74). The server's address and tools are shown, changed elsewhere.
function StorageSettings({ conf, status }) {
  const data = conf.data;
  const saved = data ? data.resources.saved : null;
  const [v, set, changed] = useForm(saved || {});
  const [busy, setBusy] = useState(false);
  if (!data) return html`<h2 class="section-title">儲存空間</h2><div class="card pad"><${ErrorBox} error=${conf.error} onRetry=${conf.reload} /></div>`;
  const r = data.resources;
  const n = (k) => num(v[k]);
  const ok = n('cache_mib') >= 64 && n('staging_mib') >= 256 && n('reserve_gib') >= 1;
  const save = () => saveSection(conf, '/settings/resources',
    { cache_mib: n('cache_mib'), staging_mib: n('staging_mib'), reserve_gib: n('reserve_gib') }, '已儲存儲存空間設定');
  const trim = async () => {
    setBusy(true);
    try {
      const t = await post('/cache/trim');
      toast(`已清除 ${fmtBytes(t.freed_bytes)} 的播放快取${t.cache_bytes ? `（正在播放的 ${fmtBytes(t.cache_bytes)} 保留）` : ''}`);
      conf.reload();
    } catch (e) {
      toast(e.message, 'error');
    }
    setBusy(false);
  };
  const pinned = (k) => r.pinned.includes(k) && html`<span class="hint tight state-failed">目前由啟動參數 ${flagName[k]} 指定為 ${r.now[k]}；不帶這個參數重新啟動後才使用這裡的值。</span>`;
  const sv = data.server;
  const disk = status && status.disk;
  return html`<h2 class="section-title">儲存空間</h2>
    <div class="card pad settings-form">
      <div class="form-grid">
        <div>
          <${NumberField} label="播放快取上限" unit="MB" min="64" value=${v.cache_mib} onInput=${(x) => set({ cache_mib: x })}
            hint=${`目前用了 ${fmtBytes(r.usage.cache_bytes || 0)}。串流時從 Drive 讀到的部分暫存在這裡，重播較快。`} />${pinned('cache_mib')}
        </div>
        <div>
          <${NumberField} label="暫存空間上限" unit="MB" min="256" value=${v.staging_mib} onInput=${(x) => set({ staging_mib: x })}
            hint=${`目前用了或預留 ${fmtBytes(r.usage.staging_bytes || 0)}。下載、上傳與匯入共用，超過時下載會分批進行。`} />${pinned('staging_mib')}
        </div>
        <div>
          <${NumberField} label="磁碟保留空間" unit="GB" min="1" value=${v.reserve_gib} onInput=${(x) => set({ reserve_gib: x })}
            hint=${`磁碟剩下不到這麼多時，先清播放快取，再暫停下載與上傳。${disk ? `目前剩 ${fmtBytes(disk.free_bytes)}。` : ''}`} />${pinned('reserve_gib')}
        </div>
      </div>
      <p class="hint">儲存後立即套用；已在進行的下載、上傳與匯入保留它們預留的空間，新的照新上限。清除播放快取只刪快取，曲庫的音樂與還沒匯入的檔案不受影響。</p>
      <div class="actions">
        <button class="btn filled" disabled=${!changed || !ok} onClick=${save}>儲存</button>
        <button class="btn tonal" disabled=${busy || !r.usage.cache_bytes} onClick=${trim}><${Icon} name="delete" />清除播放快取</button>
      </div>
    </div>
    <div class="card pad">
      <div class="title">伺服器</div>
      <dl class="facts">
        <dt>公開網址</dt><dd class="break">${sv.public_url || '（未設定）'}</dd>
        <dt>監聽位址</dt><dd>${sv.listen}</dd>
        <dt>資料目錄</dt><dd class="break">${sv.data_dir}</dd>
        <dt>aria2</dt><dd class="break">${sv.aria2 || '沒有（BitTorrent 下載停用）'}</dd>
        <dt>FFmpeg</dt><dd class="break">${sv.ffmpeg || '沒有（轉檔與 CUE 切割停用）'}</dd>
      </dl>
      <p class="hint tight">在伺服器上用 <code>sudo kanade-manager config</code> 修改公開網址與監聽位址、<code>sudo kanade-manager tools</code> 安裝 aria2 與 FFmpeg，重新啟動後生效。</p>
    </div>`;
}

// Playback: the play mode (the player bar's own), what shuffle draws from, keeping on after the
// queue, where a chosen song starts and preloading (reviews #72, #73, #78); kept in this browser and
// applied at once without stopping the song playing.
function PlaybackSettings() {
  // One value each: a fresh object would re-render this four times a second while a song plays.
  const s = {
    mode: useStore(player, (p) => p.mode), scope: useStore(player, (p) => p.scope), autoContinue: useStore(player, (p) => p.autoContinue),
    preload: useStore(player, (p) => p.preload), resume: useStore(player, (p) => p.resume),
  };
  const choices = (label, list, value, pick) => html`<div class="sub">${label}</div>
    <div class="choices" role="radiogroup" aria-label=${label}>
      ${list.map(([k, text]) => html`<button type="button" class="choice" role="radio" aria-checked=${value === k} onClick=${() => pick(k)}>${text}</button>`)}
    </div>`;
  const resume = (kind, k) => setPrefs({ resume: { ...s.resume, [kind]: k } });
  return html`<h2 class="section-title">播放</h2>
    <div class="card pad">
      ${choices('播放模式（與播放列的按鈕相同）', [['order', '順序播放'], ['all', '列表循環'], ['one', '單曲循環'], ['shuffle', '隨機播放']], s.mode, setMode)}
      ${choices('隨機播放的範圍', [['queue', '目前的佇列'], ['library', '全曲庫']], s.scope, (k) => setPrefs({ scope: k }))}
      <p class="hint tight">「全曲庫」時，隨機播放會從整個曲庫挑歌（每首歌機會相同，不限專輯；音樂與廣播劇分開），一直接著播。首頁與曲庫的「全曲庫隨機播放」不論模式都這樣播。</p>
      <${Toggle} label="播完後自動接續" sub="佇列（專輯、歌單）播完後，從曲庫隨機挑歌接著播；自己加入的歌先播。單曲循環與列表循環時照常循環；隨機播放目前佇列時，播完一輪改為接續。"
        checked=${s.autoContinue} onChange=${(on) => setPrefs({ autoContinue: on })} />
      ${choices('音樂：點一首歌時', [['start', '從頭播放'], ['resume', '從上次停下的地方']], s.resume.music, (k) => resume('music', k))}
      ${choices('廣播劇與談話：點一集時', [['resume', '從上次停下的地方'], ['start', '從頭播放']], s.resume.spoken, (k) => resume('spoken', k))}
      <p class="hint tight">首頁的「繼續播放／繼續收聽」一定從記錄的位置開始；單曲與列表循環再播一次時一定從頭開始。</p>
      <${Toggle} label="預先載入下一首" sub="播放時先讓伺服器從 Drive 讀好下一首的開頭，切歌較快；網路流量有限時可以關閉。"
        checked=${s.preload} onChange=${(on) => setPrefs({ preload: on })} />
      <p class="hint tight">這些設定存在這個瀏覽器。</p>
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

// fromAgent names a device by its browser's user agent: "Windows · Chrome".
function fromAgent(ua) {
  const os = /iPhone|iPad/.test(ua) ? 'iPhone' : /Android/.test(ua) ? 'Android' : /Mac OS X/.test(ua) ? 'Mac'
    : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : '';
  const browser = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : '';
  return [os, browser].filter(Boolean).join(' · ');
}

// deviceName guesses a name for a passkey made on this device.
const deviceName = () => fromAgent(navigator.userAgent) || 'Passkey';

// loginName shows what a client said it was when logging in; the web client sends its user agent.
function loginName(name) {
  const web = name.match(/^web: (.*?)( \(passkey\))?$/);
  if (!web) return name || '未命名的裝置';
  return (fromAgent(web[1]) || '瀏覽器') + (web[2] ? '（passkey 登入）' : '');
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

// Logins: every device logged in to this account, and ending the others (review #76).
function Logins({ first, onLogout }) {
  const data = useSection('/sessions', first);
  const end = async (v) => {
    if (!confirm(`登出「${loginName(v.name)}」？那台裝置要重新登入才能使用。`)) return;
    try {
      await api('DELETE', `/sessions/${v.id}`);
      toast('已登出那台裝置');
      data.reload();
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const endOthers = async () => {
    if (!confirm('登出這台以外的所有裝置？它們要重新登入才能使用。')) return;
    try {
      const r = await post('/sessions/end-others');
      toast(r.ended ? `已登出 ${r.ended} 台裝置` : '沒有其他登入中的裝置');
      data.reload();
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const list = data.data || [];
  const others = list.filter((v) => !v.current).length;
  return html`<div class="card pad">
    <div class="title">登入中的裝置</div>
    <div class="sub">登入 90 天後會自動登出。最後活動時間每小時更新一次。</div>
    <${ErrorBox} error=${data.error} onRetry=${data.reload} />
    ${list.length > 0 && html`<ul class="list passkeys">${list.map((v) => html`<li key=${v.id} class="row">
      <${Icon} name=${v.current ? 'check' : 'devices'} />
      <span class="grow"><span class="title">${loginName(v.name)}${v.current && html` <span class="pill good">這台裝置</span>`}</span>
        <span class="sub">登入於 ${when(v.created_at)} · 最後活動 ${when(v.last_used_at)}</span></span>
      ${!v.current && html`<${IconButton} icon="logout" label=${`登出「${loginName(v.name)}」`} onClick=${() => end(v)} />`}
    </li>`)}</ul>`}
    <div class="actions">
      <button class="btn tonal" onClick=${() => showDialog((close) => html`<${ChangePassword} close=${close} onDone=${onLogout} />`)}>
        <${Icon} name="key" />更改密碼</button>
      <button class="btn text" disabled=${!others} onClick=${endOthers}>登出其他裝置</button>
    </div>
  </div>`;
}

// ChangePassword confirms the current password; every device, this one too, then logs in again.
function ChangePassword({ close, onDone }) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [again, setAgain] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const problem = next && next.length < 10 ? '新密碼至少要 10 個字元。'
    : again && again !== next ? '兩次輸入的新密碼不一樣。'
    : next && next === current ? '新密碼和目前的密碼相同。' : '';
  const submit = async (e) => {
    e.preventDefault();
    if (problem) return;
    setBusy(true);
    setError(null);
    try {
      await post('/account/password', { current, password: next });
      close();
      toast('已更改密碼，所有裝置都已登出；請用新密碼重新登入');
      resetPlayer();
      onDone();
    } catch (err) {
      setError(err.status === 403 ? new Error('目前的密碼不對。') : err.status === 429 ? new Error('錯誤太多次，請 15 分鐘後再試。') : err);
      setBusy(false);
    }
  };
  return html`<${Dialog} title="更改密碼" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button type="submit" form="password-form" class="btn filled" disabled=${busy || !current || !next || next !== again || !!problem}>
        ${busy ? '更改中…' : '更改密碼'}</button>`}>
    <form id="password-form" onSubmit=${submit}>
      <p class="hint">更改後，所有裝置（包括這台）都會登出，要用新密碼重新登入；passkey 不受影響。</p>
      <label class="field"><span>目前的密碼</span><input type="password" autocomplete="current-password" value=${current}
        onInput=${(e) => setCurrent(e.target.value)} required /></label>
      <label class="field"><span>新密碼（至少 10 個字元）</span><input type="password" autocomplete="new-password" value=${next}
        onInput=${(e) => setNext(e.target.value)} required /></label>
      <label class="field"><span>再次輸入新密碼</span><input type="password" autocomplete="new-password" value=${again}
        onInput=${(e) => setAgain(e.target.value)} required /></label>
      ${problem && html`<p class="sub state-failed">${problem}</p>`}
      <${ErrorBox} error=${error} />
    </form>
  <//>`;
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
    ${st && st.has_client && html`${!st.connected
        ? html`<div class="title">尚未連線</div>
          <div class="sub">連線時選擇要存放音樂的 Google 帳號，並允許存取 Google Drive。${unverified}。</div>`
        : d.account
        ? html`<div class="title">${d.account.email}</div>
          <div class="sub">已使用 ${fmtBytes(d.account.usage_bytes)}${d.account.limit_bytes ? ` / ${fmtBytes(d.account.limit_bytes)}` : ''}</div>
          ${st.testing_mode && html`<div class="task-error">OAuth 應用程式仍在「測試中」，授權 7 天後失效。</div>`}`
        : html`<div class="title">無法讀取 Google Drive</div>
          <div class="task-error">${d.account_error}</div>
          <div class="sub">${d.reconnect
            ? `Google 不再接受這次的授權：可能已過期${st.testing_mode ? '（應用程式在「測試中」，授權 7 天後失效）' : ''}，或在 Google 帳號移除了 Kanade 的存取權。按「重新連線」重新授權。`
            : '可能暫時連不上 Google：稍後按「重試」。一直失敗的話，按「重新連線」重新授權。'}</div>`}
      <div class="sub break">OAuth 用戶端：${st.client_id}</div>
      <div class="actions"><button class=${'btn ' + (d.reconnect ? 'filled' : 'tonal')} onClick=${connect}><${Icon} name="refresh" />${st.connected ? '重新連線' : '連線 Google Drive'}</button>
        ${d.account_error && !d.reconnect && html`<button class="btn text" disabled=${drive.loading} onClick=${drive.reload}>重試</button>`}
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
  const [jsonName, setJsonName] = useState('');
  const pick = async (e) => {
    const f = e.target.files[0];
    e.target.value = ''; // the same file can be chosen again
    if (!f) return;
    setJsonName(f.name);
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
      <div class="field"><span>下載的 JSON 檔</span>
        <${FilePick} label="選擇 JSON 檔" icon="upload" accept=".json,application/json" onPick=${pick} chosen=${jsonName} /></div>
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
    const [status, drive, passkeys, sessions, conf] = await Promise.all([read('/status'), read('/drive'), read('/passkeys'),
      read('/sessions'), read('/settings')]);
    const sync = drive.data && drive.data.status.connected ? await read('/drive/sync') : null;
    return { status, drive, passkeys, sessions, conf, sync };
  }, []);
  if (!first.data) return html`<section><h1 class="page-title">設定</h1><div class="spinner late"></div></section>`;
  return html`<${SettingsPage} first=${first.data} onLogout=${onLogout} />`;
}

function SettingsPage({ first, onLogout }) {
  const status = useSection('/status', first.status);
  const drive = useSection('/drive', first.drive);
  const conf = useSection('/settings', first.conf);
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
    ${d && d.status.connected && html`<${DriveSync} first=${first.sync} conf=${conf} />`}
    <${PlaybackSettings} />
    <${Appearance} />
    <${StatsTimeZone} />
    <${DownloadSettings} conf=${conf} />
    <h2 class="section-title">服務</h2>
    <div class="card pad">
      <${ErrorBox} error=${status.error} onRetry=${status.reload} />
      ${st && html`<div class="sub">下載器（aria2）：${st.aria2_ready ? '運作中' : '未就緒'}</div>
        <div class="sub">格式轉換與 CUE 分軌（FFmpeg）：${st.ffmpeg ? '可用' : '未安裝'}</div>
        ${st.disk && html`<div class=${'sub' + (st.disk.low ? ' state-failed' : '')}>磁碟：剩 ${fmtBytes(st.disk.free_bytes)}（保留 ${fmtBytes(st.disk.reserve_bytes)}）${st.disk.low ? '，空間不足' : ''}</div>`}
        <div class="sub">已運行 ${Math.floor(st.uptime_seconds / 3600)} 小時 ${Math.floor((st.uptime_seconds % 3600) / 60)} 分</div>`}
    </div>
    <${StorageSettings} conf=${conf} status=${st} />
    <h2 class="section-title">帳號</h2>
    <${Logins} first=${first.sessions} onLogout=${onLogout} />
    <${Passkeys} first=${first.passkeys} />
    <div class="actions"><button class="btn outlined" onClick=${logout}><${Icon} name="logout" />登出</button></div>
  </section>`;
}

// Storage locations (#95, #99, #100): where imported music is kept besides Google Drive, and
// collections already somewhere (a NAS, a OneDrive) imported where they are.
import { useEffect, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { loadPlaces } from '../places.js';
import { confirmDialog, Field, textDialog, useRunner } from './organize.js';
import { Dialog, ErrorBox, Icon, copyText, fmtBytes, html, showDialog, toast, useLoad } from '../ui.js';

const kinds = { local: '資料夾', s3: 'R2／S3', onedrive: 'OneDrive' };
const modes = { managed: '存放位置', source: '只讀來源' };
const when = (ms) => new Date(ms).toLocaleString('zh-TW', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });

const odConfig = (st) => (typeof st.config === 'string' ? JSON.parse(st.config) : st.config) || {};
// odReady says whether a OneDrive is signed in to and has its folder; other kinds are.
const odReady = (st) => st.kind !== 'onedrive' || (st.signed_in && !!odConfig(st).folder);

function stateLine(st) {
  if (st.kind === 'onedrive' && !st.signed_in) return html`<span class="sub state-failed">還沒連線：按「連線 OneDrive」登入要使用的 Microsoft 帳號。</span>`;
  if (st.kind === 'onedrive' && !odConfig(st).folder) return html`<span class="sub state-failed">已連線 ${odConfig(st).account}，還沒選擇資料夾：按「選擇資料夾」。</span>`;
  if (!st.state) return html`<span class="sub">還沒檢查</span>`;
  if (st.state !== 'ok') {
    const auth = st.kind === 'onedrive' && /refused its keys or sign-in|not connected/.test(st.error || '');
    return html`<span class="sub state-failed">${st.state === 'offline' ? '目前無法使用（未掛載、離線或連不上）' : '無法使用'}：${st.error}</span>
      ${auth && html`<span class="sub state-failed">Microsoft 不再接受這次登入（密碼改了、在帳號移除了存取權，或用戶端密碼過期）：按「重新連線」，用戶端密碼過期的話先「更新用戶端密碼」。</span>`}`;
  }
  const u = st.usage;
  const room = u && u.free ? `，剩 ${fmtBytes(u.free)}${u.total ? `／共 ${fmtBytes(u.total)}` : ''}`
    : u && u.used ? `，已用 ${fmtBytes(u.used)}（${u.files || 0} 個檔案）` : '';
  return html`<span class="sub state-completed">可以使用${room}</span>`;
}

function scanLine(st) {
  const s = st.scan ? (typeof st.scan === 'string' ? JSON.parse(st.scan) : st.scan) : null;
  if (st.running) return html`<span class="sub">${st.mode === 'source' ? '正在掃描…' : '正在檢查…'}</span>`;
  if (!s) return st.mode === 'source' ? html`<span class="sub">還沒掃描：按「掃描」把這裡的音樂匯入曲庫。</span>` : null;
  if (st.mode === 'source') {
    const parts = [s.new && `新加入 ${s.new} 個`, s.changed && `內容改變 ${s.changed} 個`, s.moved && `搬動 ${s.moved} 個`,
      s.gone && `${s.gone} 首不見了（標成遺失）`, s.back && `${s.back} 首回來了`].filter(Boolean);
    return html`<span class="sub">上次掃描：${when(s.at)}，共 ${s.files} 個檔案${parts.length ? '；' + parts.join('、') : '，沒有變動'}${s.bytes ? `（匯入要讀 ${fmtBytes(s.bytes)}）` : ''}</span>
      ${s.held > 0 && html`<span class="sub state-failed">有 ${s.held} 首一次不見太多，看起來不是被刪除而是沒有完整掛載，所以沒有標成遺失。確認來源正常後再掃描一次。</span>`}
      ${s.error && html`<span class="sub state-failed">${s.error}</span>`}`;
  }
  return html`<span class="sub">上次檢查：${when(s.at)}，這裡有曲庫的 ${s.checked} 首歌${s.missing ? `，${s.missing} 首不見了（標成遺失）` : ''}${s.restored ? `，${s.restored} 首回來了` : ''}${s.lost ? `，${s.lost} 個封面或附屬檔不見了` : ''}</span>
    ${s.held > 0 && html`<span class="sub state-failed">有 ${s.held} 首一次不見太多，沒有標成遺失；確認位置正常後再檢查一次。</span>`}
    ${s.error && html`<span class="sub state-failed">${s.error}</span>`}`;
}

// CategoryField picks the category a source's albums go in when they are imported (#92).
function CategoryField({ value, onChange, disabled, field }) {
  const cats = useLoad(() => get('/categories').then((v) => v.categories), []);
  if (!cats.data) return null;
  const pick = html`<select value=${String(value || 0)} disabled=${disabled} onChange=${(e) => onChange(Number(e.target.value))}>
    <option value="0">不放進分類</option>
    ${cats.data.map((c) => html`<option key=${c.id} value=${String(c.id)}>${c.name}</option>`)}
  </select>`;
  const hint = cats.data.length
    ? '只影響之後匯入的專輯；已在曲庫的，可以在曲庫的專輯頁依來源篩選後全選加入。'
    : '還沒有分類；可以在曲庫的「分類」建立。';
  return field
    ? html`<label class="field">匯入的專輯放進分類${pick}<span class="hint tight">${hint}</span></label>`
    : html`<div class="store-category"><label class="select-field">新匯入的專輯放進分類${pick}</label><span class="sub">${hint}</span></div>`;
}

function StoreRow({ st, isDefault, onChange }) {
  const [busy, run] = useRunner();
  const scan = (preview) => run(async () => {
    await post(`/storage/${st.id}/scan`, { preview });
    toast(st.mode === 'source' ? (preview ? '開始掃描，新的檔案會先停在「任務」的預覽' : '開始掃描，新的檔案會自動匯入') : '開始檢查');
    onChange();
  });
  const check = () => run(async () => {
    const v = await post(`/storage/${st.id}/check`);
    toast(v.state === 'ok' ? '可以使用' : '目前無法使用', v.state === 'ok' ? 'info' : 'error');
    onChange();
  });
  const rename = () => textDialog({
    title: '重新命名', label: '名稱', initial: st.name, maxLength: 60,
    onSubmit: async (name) => { await api('PATCH', `/storage/${st.id}`, { name: name.trim() }); onChange(); },
  });
  const auto = (on) => run(async () => { await api('PATCH', `/storage/${st.id}`, { auto_scan: on }); onChange(); });
  const categorize = (category) => run(async () => {
    await api('PATCH', `/storage/${st.id}`, { category });
    toast(category ? '之後從這裡匯入的專輯會放進這個分類' : '之後從這裡匯入的專輯不放進分類');
    onChange();
  });
  const makeDefault = () => run(async () => {
    await api('PUT', '/storage/default', { id: st.id });
    toast(`新匯入的音樂會存到「${st.name}」`);
    onChange();
  });
  const remove = () => confirmDialog({
    title: `移除「${st.name}」`, action: '移除', danger: st.mode === 'source' && st.use.songs > 0,
    children: st.mode === 'source'
      ? html`<p>${st.use.songs > 0 ? `這個來源的 ${st.use.songs} 首歌會從曲庫移除（連同它們的播放記錄、收藏和歌單項目）。` : ''}${st.location} 裡的檔案不會動。</p>`
      : html`<p>只是不再使用這個位置；${st.location} 裡的檔案不會動。</p>`,
    onConfirm: async () => {
      const r = await api('DELETE', `/storage/${st.id}`);
      toast(r.songs ? `已移除，曲庫少了 ${r.songs} 首歌` : '已移除');
      onChange();
    },
  });
  const inUse = st.mode === 'managed' && st.use.songs + st.use.others > 0;
  const scanned = !!st.scan;
  const od = st.kind === 'onedrive';
  const ready = odReady(st);
  const hasFiles = st.use.songs + st.use.others > 0;
  const connect = () => connectOneDrive(st, onChange);
  const pickFolder = () => chooseFolder(st, onChange);
  return html`<div class="card pad store-row">
    <div class="store-head">
      <${Icon} name=${st.kind === 'local' ? 'folder' : 'cloud'} />
      <span class="grow"><span class="title">${st.name}</span>
        <span class="sub break">${kinds[st.kind] || st.kind} · ${modes[st.mode]}${isDefault ? ' · 新匯入存在這裡' : ''} · ${st.location}</span>
        ${od && st.signed_in && odConfig(st).account && html`<span class="sub break">帳號：${odConfig(st).account}</span>`}</span>
    </div>
    <div class="store-lines">
      ${stateLine(st)}
      ${(st.use.songs > 0 || st.mode === 'managed') && html`<span class="sub">曲庫有 ${st.use.songs} 首歌在這裡${st.use.bytes ? `（${fmtBytes(st.use.bytes)}）` : ''}${st.use.missing ? html`，<span class="state-failed">${st.use.missing} 首遺失</span>` : ''}</span>`}
      ${scanLine(st)}
    </div>
    ${st.mode === 'source' && scanned && html`<label class="toggle-row">
      <span class="grow"><span class="title">自動匯入新檔案</span><span class="sub">每 6 小時掃描一次：新的檔案直接匯入，不見的標成遺失、回來的恢復。</span></span>
      <input type="checkbox" role="switch" checked=${st.auto_scan} disabled=${busy} onChange=${(e) => auto(e.target.checked)} />
    </label>`}
    ${st.mode === 'source' && html`<${CategoryField} value=${st.category} disabled=${busy} onChange=${categorize} />`}
    <div class="actions">
      ${od && !st.signed_in && html`<button class="btn filled" onClick=${connect}><${Icon} name="cloud" />連線 OneDrive</button>`}
      ${od && st.signed_in && !odConfig(st).folder && html`<button class="btn filled" onClick=${pickFolder}><${Icon} name="folder" />選擇資料夾</button>`}
      ${ready && (st.mode === 'source'
        ? html`<button class="btn tonal" disabled=${busy || st.running} onClick=${() => scan(false)}><${Icon} name="refresh" />${scanned ? '掃描' : '掃描並匯入'}</button>
          <button class="btn text" disabled=${busy || st.running} onClick=${() => scan(true)}>掃描並先預覽</button>`
        : html`${!isDefault && html`<button class="btn tonal" disabled=${busy || st.state !== 'ok'} onClick=${makeDefault}>設為新匯入的位置</button>`}
          <button class="btn text" disabled=${busy || st.running} onClick=${() => scan(false)}>檢查檔案</button>`)}
      ${ready && html`<button class="btn text" disabled=${busy} onClick=${check}>檢查連線</button>`}
      ${st.kind === 's3' && html`<button class="btn text" onClick=${() => changeKeys(st, onChange)}>更新金鑰</button>`}
      ${od && st.signed_in && html`<button class="btn text" onClick=${connect}>重新連線</button>`}
      ${od && st.signed_in && odConfig(st).folder && html`<button class="btn text" disabled=${hasFiles || st.running} title=${hasFiles ? '曲庫還有檔案在這個資料夾；搬到別處的功能另外設計' : ''} onClick=${pickFolder}>更換資料夾</button>`}
      ${od && html`<button class="btn text" onClick=${() => changeClientSecret(st, onChange)}>更新用戶端密碼</button>`}
      <button class="btn text" onClick=${rename}>重新命名</button>
      <button class="btn text danger-text" disabled=${inUse || st.running} title=${inUse ? '曲庫還有檔案存在這裡；搬到別處的功能另外設計，目前要先刪除這些歌曲' : ''} onClick=${remove}>移除</button>
    </div>
    ${inUse && html`<p class="hint tight">曲庫還有 ${st.use.songs} 首歌${st.use.others ? `和 ${st.use.others} 個封面或附屬檔` : ''}存在這裡，所以不能移除。</p>`}
  </div>`;
}

// StorageLocations is the settings section: where new imports go, the locations, and adding one.
export function StorageLocations() {
  const [v, setV] = useState(null);
  const [error, setError] = useState(null);
  const load = () => loadPlaces().then((r) => {
    if (r) {
      setV(r);
      setError(null);
    } else {
      setError(new Error('讀不到儲存位置'));
    }
  });
  useEffect(() => { load(); }, []);
  const running = v && v.stores.some((s) => s.running);
  useEffect(() => {
    if (!running) return undefined;
    const t = setInterval(load, 1500);
    return () => clearInterval(t);
  }, [running]);
  const [busy, run] = useRunner();
  if (error) return html`<${ErrorBox} error=${error} onRetry=${load} />`;
  if (!v) return null;
  const managed = v.stores.filter((s) => s.mode === 'managed');
  const setDefault = (id) => run(async () => {
    await api('PUT', '/storage/default', { id: id === 'drive' ? 'drive' : Number(id) });
    toast('已更改新匯入的位置');
    load();
  });
  const add = () => showDialog((close) => html`<${AddStore} close=${close} dataDir=${v.data_dir} redirect=${v.onedrive_redirect} paste=${v.onedrive_paste} onAdded=${load} />`);
  const drive = v.drive || {};
  const defaultOffline = v.default === 'drive' ? !drive.connected : (managed.find((s) => String(s.id) === v.default) || {}).state !== 'ok';
  return html`<div class="card pad">
      <label class="field">新匯入的音樂存到
        <select value=${v.default} disabled=${busy} onChange=${(e) => setDefault(e.target.value)}>
          <option value="drive">Google Drive${drive.connected ? '' : '（尚未連線）'}</option>
          ${managed.map((s) => html`<option value=${String(s.id)}>${s.name}（${kinds[s.kind]}）</option>`)}
        </select>
        <span class="hint tight">上傳、下載與轉檔完成的音樂、封面、CUE 和 LOG 都存到這裡。只影響之後的匯入，已在曲庫的歌留在原處。</span>
      </label>
      ${defaultOffline && html`<div class="task-error">${v.default === 'drive'
        ? '還沒連線 Google Drive：匯入會失敗。請連線 Google Drive，或新增一個存放位置並選它。'
        : '這個位置目前無法使用：匯入會失敗，直到它恢復。'}${v.stores.some((s) => s.mode === 'source')
        ? '只讀來源的歌仍可原位匯入，但需要轉檔或分軌的檔案會匯入失敗（之後可以在任務重試），只內嵌在音檔裡的封面也不會保存。' : ''}</div>`}
      <div class="sub">Google Drive 有曲庫的 ${drive.songs || 0} 首歌。</div>
    </div>
    ${v.stores.map((st) => html`<${StoreRow} key=${st.id} st=${st} isDefault=${String(st.id) === v.default} onChange=${load} />`)}
    <div class="actions"><button class="btn tonal" onClick=${add}><${Icon} name="add" />新增儲存位置</button></div>`;
}

// SecretField is a key: typed, not shown, not kept by the browser.
function SecretField({ label, value, onInput, placeholder }) {
  return html`<label class="field">${label}
    <input type="password" autocomplete="new-password" spellcheck="false" value=${value} placeholder=${placeholder} onInput=${(e) => onInput(e.target.value)} />
  </label>`;
}

function Seg({ label, value, options, onChange }) {
  return html`<div class="field">${label}
    <div class="seg" role="radiogroup" aria-label=${label}>${options.map(([v, text]) => html`<label key=${v} class=${value === v ? 'on' : ''}>
      <input type="radio" checked=${value === v} onChange=${() => onChange(v)} />${text}</label>`)}</div>
  </div>`;
}

function AddStore({ close, dataDir, redirect, paste, onAdded }) {
  const [kind, setKind] = useState('local');
  const [mode, setMode] = useState('managed');
  const [name, setName] = useState('');
  const [root, setRoot] = useState('');
  const [bucket, setBucket] = useState({ endpoint: '', bucket: '', prefix: 'kanade', region: 'auto', access_key_id: '', secret_access_key: '' });
  const [app, setApp] = useState({ client_id: '', client_secret: '', tenant: 'common', region: '' });
  const [category, setCategory] = useState(0);
  const [error, setError] = useState(null);
  const [busy, run] = useRunner();
  const suggested = dataDir ? dataDir.replace(/\/$/, '') + '/library' : '';
  const setB = (k) => (v) => setBucket((b) => ({ ...b, [k]: v }));
  const setA = (k) => (v) => setApp((a) => ({ ...a, [k]: v }));
  const ready = kind === 'local' ? !!root.trim()
    : kind === 'onedrive' ? !!(app.client_id.trim() && app.client_secret.trim())
    : !!(bucket.endpoint.trim() && bucket.bucket.trim() && bucket.access_key_id.trim() && bucket.secret_access_key.trim());
  const fallbackName = () => (kind === 'local'
    ? root.trim().split('/').filter(Boolean).pop() || '資料夾'
    : kind === 'onedrive' ? 'OneDrive'
    : [bucket.bucket.trim(), bucket.prefix.trim().replace(/^\/+|\/+$/g, '')].filter(Boolean).join('/'));
  const save = (e) => {
    e && e.preventDefault();
    if (!ready || busy) return;
    setError(null);
    run(async () => {
      const body = { kind, mode, name: name.trim() || fallbackName(), category: mode === 'source' ? category : 0 };
      if (kind === 'local') {
        body.config = { root: root.trim() };
      } else if (kind === 'onedrive') {
        body.config = { client_id: app.client_id.trim(), tenant: app.tenant.trim() || 'common', region: app.region };
        body.secret = { client_secret: app.client_secret.trim() };
      } else {
        body.config = { endpoint: bucket.endpoint.trim(), bucket: bucket.bucket.trim(), prefix: bucket.prefix.trim(), region: bucket.region.trim() || 'auto' };
        body.secret = { access_key_id: bucket.access_key_id.trim(), secret_access_key: bucket.secret_access_key.trim() };
      }
      let st;
      try {
        st = await post('/storage', body);
      } catch (err) {
        setError(err);
        return;
      }
      close();
      onAdded();
      if (st.kind === 'onedrive') {
        connectOneDrive(st, onAdded);
      } else if (st.mode === 'source') {
        confirmDialog({
          title: `掃描「${st.name}」？`, action: '掃描並匯入',
          children: html`<p>把 ${st.location} 裡的音樂原位匯入曲庫：不會複製、移動或改寫任何檔案。${st.kind === 'local' ? '' : '匯入時會逐首下載到暫存空間讀取，完成後刪除暫存複本；播放時從原處串流。'}也可以之後在設定頁按「掃描並先預覽」，先確認專輯分組。</p>`,
          onConfirm: async () => { await post(`/storage/${st.id}/scan`, {}); toast('開始掃描'); onAdded(); },
        });
      } else {
        toast(`已新增「${st.name}」；可以在上面把它設為新匯入的位置`);
      }
    });
  };
  const modeHint = mode === 'managed'
    ? '存放位置：匯入的音樂、封面、CUE 和 LOG 存在這裡的 library／covers／sidecars 資料夾，和存在 Google Drive 時一樣。'
    : '只讀來源：已經在這裡的音樂原位匯入，播放時直接讀原檔；不會寫入、移動或刪除任何檔案。';
  return html`<${Dialog} title="新增儲存位置" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !ready} onClick=${save}>${busy ? (kind === 's3' ? '連線測試中…' : '新增中…') : kind === 'onedrive' ? '新增並連線' : '新增'}</button>`}>
    <form onSubmit=${save}>
      <${Seg} label="種類" value=${kind} onChange=${(v) => { setKind(v); setError(null); }}
        options=${[['local', '伺服器上的資料夾或 NAS'], ['s3', 'Cloudflare R2／S3'], ['onedrive', 'OneDrive']]} />
      <div>
        <${Seg} label="用途" value=${mode} onChange=${(v) => { setMode(v); if (kind === 's3' && bucket.prefix === (mode === 'managed' ? 'kanade' : '')) setB('prefix')(v === 'managed' ? 'kanade' : ''); }}
          options=${[['managed', '存放位置'], ['source', '只讀來源']]} />
        <span class="hint tight">${modeHint}</span>
      </div>
      ${kind === 'onedrive' ? html`<${OneDriveApp} app=${app} set=${setA} mode=${mode} redirect=${redirect} paste=${paste} />`
      : kind === 'local' ? html`
        <${Field} label="伺服器上的資料夾路徑" value=${root} onInput=${setRoot} placeholder=${mode === 'managed' ? suggested || '/mnt/nas/kanade' : '/mnt/nas/music'} autofocus />
        <p class="hint tight">${mode === 'managed'
          ? html`要先建好資料夾或掛載好 NAS（資料目錄裡的資料夾會自動建立${suggested ? html`，例如 <a class="link" href="#" onClick=${(e) => { e.preventDefault(); setRoot(suggested); }}>${suggested}</a>` : ''}）。Kanade 會在裡面放一個 .kanade-store 檔，用來確認 NAS 有掛載；沒掛載時不會把檔案寫到底下的本機磁碟。執行 Kanade 的帳號要能寫入這個資料夾。`
          : '執行 Kanade 的帳號要能讀取這個資料夾。NAS 沒掛載（資料夾是空的）或一次不見太多檔案時，不會把歌曲標成遺失。'}</p>`
      : html`
        <${Field} label="S3 API 端點" value=${bucket.endpoint} onInput=${setB('endpoint')} placeholder="https://<帳戶 ID>.r2.cloudflarestorage.com" autofocus />
        <div class="form-grid">
          <${Field} label="Bucket 名稱" value=${bucket.bucket} onInput=${setB('bucket')} placeholder="music" />
          <${Field} label=${mode === 'managed' ? 'Bucket 裡的資料夾' : 'Bucket 裡的資料夾（可不填）'} value=${bucket.prefix} onInput=${setB('prefix')} placeholder=${mode === 'managed' ? 'kanade' : '整個 bucket'} />
          <${Field} label="Access Key ID" value=${bucket.access_key_id} onInput=${setB('access_key_id')} />
          <${SecretField} label="Secret Access Key" value=${bucket.secret_access_key} onInput=${setB('secret_access_key')} />
          <${Field} label="區域" value=${bucket.region} onInput=${setB('region')} placeholder="auto" />
        </div>
        <p class="hint tight">在 Cloudflare 的 R2 →「管理 API 權杖」建立權杖：${mode === 'managed' ? '權限選「物件讀取和寫入」' : '權限選「物件唯讀」即可'}，並限定這個 bucket。端點在 bucket 的「設定」裡（S3 API）。Bucket 保持私有：播放時由伺服器簽章讀取。金鑰只存在伺服器，不會再顯示。其他 S3 相容服務（MinIO、Backblaze B2 等）也可以，區域照服務填寫。</p>`}
      ${mode === 'source' && html`<${CategoryField} field value=${category} onChange=${setCategory} />`}
      <${Field} label="名稱（可不填）" value=${name} onInput=${setName} placeholder=${kind === 's3' ? '例如：R2 音樂庫' : kind === 'onedrive' ? '例如：OneDrive 音樂' : mode === 'managed' ? '例如：NAS 音樂庫' : '例如：NAS 上的收藏'} maxLength=${60} />
      <${ErrorBox} error=${error} />
      <button type="submit" hidden></button>
    </form>
  <//>`;
}

// changeKeys gives a bucket new keys: they are tried before they are kept.
function changeKeys(st, onChange) {
  showDialog((close) => html`<${KeysDialog} st=${st} close=${close} onChange=${onChange} />`);
}

function KeysDialog({ st, close, onChange }) {
  const [keys, setKeys] = useState({ access_key_id: '', secret_access_key: '' });
  const [error, setError] = useState(null);
  const [busy, run] = useRunner();
  const ready = keys.access_key_id.trim() && keys.secret_access_key.trim();
  const save = (e) => {
    e && e.preventDefault();
    if (!ready || busy) return;
    setError(null);
    run(async () => {
      try {
        await api('PATCH', `/storage/${st.id}`, { secret: { access_key_id: keys.access_key_id.trim(), secret_access_key: keys.secret_access_key.trim() } });
      } catch (err) {
        setError(err);
        return;
      }
      toast('已更新金鑰');
      close();
      onChange();
    });
  };
  return html`<${Dialog} title=${`更新「${st.name}」的金鑰`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !ready} onClick=${save}>${busy ? '連線測試中…' : '更新'}</button>`}>
    <form onSubmit=${save}>
      <${Field} label="Access Key ID" value=${keys.access_key_id} onInput=${(v) => setKeys((k) => ({ ...k, access_key_id: v }))} autofocus />
      <${SecretField} label="Secret Access Key" value=${keys.secret_access_key} onInput=${(v) => setKeys((k) => ({ ...k, secret_access_key: v }))} />
      <p class="hint tight">新的金鑰先試過能用才會換上。</p>
      <${ErrorBox} error=${error} />
      <button type="submit" hidden></button>
    </form>
  <//>`;
}

// ---- OneDrive (#95) ----

// Where an app is registered, by Microsoft's cloud (OneDriveConfig.Region).
const portals = { '': 'https://entra.microsoft.com/', cn: 'https://portal.azure.cn/', us: 'https://portal.azure.us/' };
const clouds = [['', '全球（一般）'], ['cn', '中國（世紀互聯）'], ['us', '美國政府雲']];
const azure = (region, text) => html`<a class="link" href=${portals[region] + '#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade'} target="_blank" rel="noopener noreferrer">${text}</a>`;

// OneDriveApp: registering the app in Microsoft Entra, with this server's redirect URI; then its ID
// and a client secret.
function OneDriveApp({ app, set, mode, redirect, paste }) {
  const copy = () => copyText(redirect).then(() => toast('已複製重新導向 URI'), (e) => toast(e.message, 'error'));
  return html`<${Seg} label="Microsoft 雲端" value=${app.region} onChange=${set('region')} options=${clouds} />
    ${app.region && html`<p class="hint tight">${app.region === 'cn' ? '世紀互聯營運的 OneDrive：帳號和應用程式都要在中國雲建立，和一般的 Microsoft 帳號不通用。' : '美國政府雲（GCC High／DoD）的組織帳號。'}</p>`}
    <ol class="steps">
      <li>用${app.region ? '這個雲端的' : '任何'} Microsoft 帳號登入 ${azure(app.region, app.region ? '「應用程式註冊」' : 'Microsoft Entra 的「應用程式註冊」')}，按「新增註冊」：名稱填 Kanade；支援的帳戶類型選「任何組織目錄中的帳戶及個人 Microsoft 帳戶」（只用公司或學校帳號的話選第一項）。</li>
      <li>重新導向 URI 的平台選「Web」，填下面這個網址，按「註冊」。</li>
      <li>複製概觀頁的「應用程式 (用戶端) 識別碼」。</li>
      <li>到「憑證及秘密」新增用戶端密碼，複製它的「值」（不是秘密識別碼；只顯示這一次）。密碼最長 24 個月後過期，過期前在這裡按「更新用戶端密碼」換新的。</li>
    </ol>
    <div class="field"><span>重新導向 URI</span>
      <div class="inline-form"><input readonly value=${redirect} onFocus=${(e) => e.target.select()} />
        <button type="button" class="btn tonal" onClick=${copy}><${Icon} name="copy" />複製</button></div></div>
    ${paste && html`<p class="hint tight">這台的公開網址不是 https 網域，Microsoft 不會把瀏覽器導回這裡，所以用 localhost：登入後瀏覽器會停在一個打不開的頁面，把那個網址貼回來就完成了。</p>`}
    <div class="form-grid">
      <${Field} label="應用程式（用戶端）識別碼" value=${app.client_id} onInput=${set('client_id')} placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" autofocus />
      <${SecretField} label="用戶端密碼的值" value=${app.client_secret} onInput=${set('client_secret')} />
      <${Field} label="租用戶" value=${app.tenant} onInput=${set('tenant')} placeholder="common" />
    </div>
    <p class="hint tight">租用戶保持 common 即可（個人帳號也可以填 consumers）。登入時會要求${mode === 'managed' ? '讀寫' : '讀取'}你的檔案（${mode === 'managed' ? 'Files.ReadWrite' : 'Files.Read'}），不需要另外設定 API 權限，也不需要系統管理員同意。${mode === 'source' ? '只讀來源絕不寫入、搬移或刪除 OneDrive 裡的檔案；從曲庫刪除歌曲時，OneDrive 的原檔也留在原處。' : ''}用戶端密碼與登入只存在伺服器。</p>`;
}

// connectOneDrive signs a OneDrive store in: Microsoft's page in another tab, then back by itself
// (this server's callback) or with the address the browser ended on pasted; then its folder.
export function connectOneDrive(st, onChange) {
  showDialog((close) => html`<${OneDriveConnect} st=${st} close=${close} onChange=${onChange} />`);
}

function OneDriveConnect({ st, close, onChange }) {
  const [start, setStart] = useState(null);
  const [pasted, setPasted] = useState('');
  const [opened, setOpened] = useState(false);
  const [error, setError] = useState(null);
  const [busy, run] = useRunner();
  useEffect(() => {
    post(`/storage/${st.id}/connect`, {}).then(setStart, setError);
  }, []);
  const done = (now) => {
    close();
    onChange();
    toast(`已連線 OneDrive${odConfig(now).account ? `（${odConfig(now).account}）` : ''}`);
    if (!odConfig(now).folder) chooseFolder(now, onChange);
  };
  // Back by itself: the callback signed it in; the store says so.
  useEffect(() => {
    if (!opened || !start || start.paste) return undefined;
    const was = JSON.stringify([st.signed_in, odConfig(st).account, st.checked_at]);
    const t = setInterval(async () => {
      const v = await loadPlaces();
      const now = v && v.stores.find((x) => x.id === st.id);
      if (now && now.signed_in && JSON.stringify([now.signed_in, odConfig(now).account, now.checked_at]) !== was) {
        clearInterval(t);
        done(now);
      }
    }, 2000);
    return () => clearInterval(t);
  }, [opened, start]);
  const submit = (e) => {
    e.preventDefault();
    setError(null);
    run(async () => {
      try {
        done(await post(`/storage/${st.id}/connect`, { url: pasted.trim() }));
      } catch (err) {
        setError(/sign-in; start again|state/.test(err.message) ? new Error('這不是這次登入的網址，或已超過 10 分鐘。請重新開啟 Microsoft 登入頁再試一次。')
          : /access_denied/.test(err.message) ? new Error('Microsoft 沒有授權（在登入頁按了取消或拒絕）。') : err);
      }
    });
  };
  const open = () => setOpened(true);
  const account = st.signed_in && odConfig(st).account;
  return html`<${Dialog} title=${`連線 OneDrive：${st.name}`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>${start && !start.paste && opened ? '關閉' : '取消'}</button>
      ${start && start.paste && html`<button type="submit" form="od-paste" class="btn filled" disabled=${busy || !/[?&]state=/.test(pasted)}>${busy ? '連線中…' : '完成連線'}</button>`}`}>
    ${!start && !error && html`<div class="spinner"></div>`}
    ${start && html`<ol class="steps">
      <li><a class="link" href=${start.url} target="_blank" rel="noopener noreferrer" onClick=${open}>開啟 Microsoft 登入頁</a>（新分頁），${account ? `用 ${account} 登入` : '登入要使用的帳號'}，允許 ${st.mode === 'managed' ? '讀寫' : '讀取'}你的檔案。${account && odConfig(st).folder ? '換成別的帳號不行：曲庫的檔案在這個帳號的 OneDrive。' : ''}</li>
      ${start.paste
        ? html`<li>允許後，瀏覽器會打開一個無法連線的 localhost 頁面，這是正常的：複製網址列裡的整個網址。</li>
          <li>貼在下面，按「完成連線」。</li>`
        : html`<li>登入完成的分頁可以關閉；這裡會自動接著${odConfig(st).folder ? '完成' : '選擇資料夾'}。</li>`}
    </ol>`}
    ${start && start.paste && html`<form id="od-paste" onSubmit=${submit}>
      <label class="field"><span>登入後的網址</span>
        <textarea class="paste-box" rows="3" value=${pasted} placeholder=${start.redirect_uri + '?code=…&state=…'} onInput=${(e) => setPasted(e.target.value)}></textarea></label>
    </form>`}
    ${start && !start.paste && opened && html`<p class="hint tight">等待登入完成…</p>`}
    <${ErrorBox} error=${error} />
  <//>`;
}

// chooseFolder picks a OneDrive store's folder: its drive's folders, one level at a time; a managed
// store can make one.
export function chooseFolder(st, onChange) {
  showDialog((close) => html`<${FolderPicker} st=${st} close=${close} onChange=${onChange} />`);
}

function FolderPicker({ st, close, onChange }) {
  const [trail, setTrail] = useState([{ id: '', name: 'OneDrive' }]); // the folders above, and this one
  // The folder shown, for the place in the trail it was asked for: an answer that comes after the
  // trail moved on is dropped, so the folder chosen is always the one shown (review #216).
  const [shown, setShown] = useState({ at: null, here: null, list: null });
  const [error, setError] = useState(null);
  const [busy, run] = useRunner();
  const at = trail[trail.length - 1];
  const place = trail.map((f) => f.id).join('/');
  useEffect(() => {
    let alive = true;
    setError(null);
    get(`/storage/${st.id}/folders?parent=${encodeURIComponent(at.id)}`).then((r) => {
      if (alive) setShown({ at: place, here: r.here, list: r.folders });
    }, (e) => alive && setError(e));
    return () => { alive = false; };
  }, [place]);
  const current = shown.at === place;
  const here = current ? shown.here : null;
  const list = current ? shown.list : null;
  const into = (f) => setTrail((t) => [...t, { id: f.id, name: f.name }]);
  const make = () => textDialog({
    title: '新增資料夾', label: '名稱', initial: 'Kanade', maxLength: 120, action: '新增',
    onSubmit: async (name) => {
      const f = await post(`/storage/${st.id}/folders`, { parent: at.id, name: name.trim() });
      into(f);
    },
  });
  const choose = () => run(async () => {
    if (!here) return;
    const now = await api('PATCH', `/storage/${st.id}`, { folder: here.id });
    close();
    onChange();
    if (now.state === 'ok' && now.mode === 'source') {
      confirmDialog({
        title: `掃描「${now.name}」？`, action: '掃描並匯入',
        children: html`<p>把 ${now.location} 裡的音樂原位匯入曲庫：不會複製、移動或改寫任何檔案。匯入時會逐首下載到暫存空間讀取，用 OneDrive 的 quickXorHash 核對後再計算 SHA-256，完成後刪除暫存複本；播放時從 OneDrive 串流。也可以之後按「掃描並先預覽」。</p>`,
        onConfirm: async () => { await post(`/storage/${now.id}/scan`, {}); toast('開始掃描'); onChange(); },
      });
    } else {
      toast(now.state === 'ok' ? `「${now.name}」使用 ${now.location}${now.mode === 'managed' ? '；可以把它設為新匯入的位置' : ''}` : `已選擇資料夾，但目前無法使用：${now.error}`, now.state === 'ok' ? 'info' : 'error');
    }
  });
  const path = here ? here.path : '';
  return html`<${Dialog} title="選擇 OneDrive 資料夾" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !here || !list} onClick=${choose}>${busy ? '檢查中…' : '使用這個資料夾'}</button>`}>
    <nav class="crumbs" aria-label="資料夾路徑">${trail.map((f, i) => html`${i > 0 && html`<span aria-hidden="true">›</span>`}
      ${i === trail.length - 1 ? html`<span class="here">${f.name}</span>`
        : html`<button type="button" onClick=${() => setTrail((t) => t.slice(0, i + 1))}>${f.name}</button>`}`)}</nav>
    <p class="hint tight">${st.mode === 'managed'
      ? '匯入的音樂、封面、CUE 和 LOG 會存在這個資料夾的 library／covers／sidecars 裡。建議新增一個專用的資料夾，例如 Kanade。'
      : '這個資料夾（含子資料夾）裡的音樂會原位匯入曲庫。'}</p>
    <${ErrorBox} error=${error} />
    ${!list && !error && html`<div class="spinner"></div>`}
    ${list && html`<ul class="folder-list">
      ${list.length === 0 && html`<li class="sub empty">這裡沒有資料夾</li>`}
      ${list.map((f) => html`<li key=${f.id}><button type="button" onClick=${() => into(f)}>
        <${Icon} name="folder" /><span class="grow">${f.name}</span><span class="sub" aria-hidden="true">›</span></button></li>`)}
    </ul>`}
    <div class="actions">
      ${trail.length > 1 && html`<button class="btn text" onClick=${() => setTrail((t) => t.slice(0, -1))}><${Icon} name="back" />上一層</button>`}
      ${st.mode === 'managed' && html`<button class="btn text" disabled=${!list} onClick=${make}><${Icon} name="add" />新增資料夾</button>`}
    </div>
    ${path && html`<p class="hint tight break">將使用：OneDrive ${path}</p>`}
  <//>`;
}

// changeClientSecret gives a OneDrive's app a new client secret (they expire).
function changeClientSecret(st, onChange) {
  showDialog((close) => html`<${ClientSecretDialog} st=${st} close=${close} onChange=${onChange} />`);
}

function ClientSecretDialog({ st, close, onChange }) {
  const [secret, setSecret] = useState('');
  const [error, setError] = useState(null);
  const [busy, run] = useRunner();
  const save = (e) => {
    e && e.preventDefault();
    if (!secret.trim() || busy) return;
    setError(null);
    run(async () => {
      try {
        await api('PATCH', `/storage/${st.id}`, { secret: { client_secret: secret.trim() } });
      } catch (err) {
        setError(/invalid_client/.test(err.message) ? new Error('Microsoft 不接受這個用戶端密碼：確認複製的是「值」而不是秘密識別碼，而且屬於這個應用程式。') : err);
        return;
      }
      toast('已更新用戶端密碼');
      close();
      onChange();
    });
  };
  return html`<${Dialog} title=${`更新「${st.name}」的用戶端密碼`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !secret.trim()} onClick=${save}>${busy ? '確認中…' : '更新'}</button>`}>
    <form onSubmit=${save}>
      <p class="hint">在 Microsoft Entra 的應用程式註冊（用戶端識別碼 ${odConfig(st).client_id}）→「憑證及秘密」新增一個用戶端密碼，貼上它的「值」。已連線的話會先用它向 Microsoft 確認。</p>
      <${SecretField} label="用戶端密碼的值" value=${secret} onInput=${setSecret} />
      <${ErrorBox} error=${error} />
      <button type="submit" hidden></button>
    </form>
  <//>`;
}


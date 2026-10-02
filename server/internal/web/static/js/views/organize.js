import { useEffect, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { go, href } from '../router.js';
import { createStore, useStore } from '../store.js';
import { Cover, Dialog, Empty, ErrorBox, Icon, Spinner, html, showDialog, toast, useLoad } from '../ui.js';

// Organizing the library (P2-2): edit dialogs, merge / split / remove, MusicBrainz identification,
// and the edit log. Every change is one entry in the log and can be undone.

// Bumped after every change, so pages that show library data load it again.
export const libRev = createStore({ n: 0 });
export const useLibRev = () => useStore(libRev, (s) => s.n);
const changed = () => libRev.set((s) => ({ n: s.n + 1 }));

// done reports a change; the toast offers to undo it right away.
function done(res, message) {
  if (!res || !res.group) {
    toast('沒有任何變更');
    return false;
  }
  changed();
  toast(message, 'info', { label: '撤回', onClick: () => undo(res.group) });
  return true;
}

export async function undo(group) {
  try {
    const r = await post(`/edits/${group}/undo`);
    changed();
    if (r.conflicts.length) showConflicts(r.conflicts, false);
    else toast('已撤回');
  } catch (e) {
    if (e.status === 409 && e.body && e.body.conflicts) showConflicts(e.body.conflicts, true);
    else toast(e.status === 409 ? '這項修改已經撤回過了' : e.message, 'error');
  }
}

const fieldNames = {
  artist: '歌手', version: '版本說明', kind: '類型', aliases: '別名', mb_recording: 'MusicBrainz 錄音', album_artist: '專輯歌手',
  date: '日期', catalog: '型號', edition: '版本', cover_id: '封面', merged_into: '合併到', mb_release: 'MusicBrainz 發行',
  album_id: '所屬專輯', disc_no: '碟號', track_no: '曲序', row: '收錄',
};
const fieldName = (target, field) => (field === 'title' ? (target === 'album' ? '專輯名稱' : '曲名') : fieldNames[field] || field);

function shown(v, field, label) {
  if (label) return label;
  if (v === null || v === undefined) return '（無）';
  if (v === '') return '（空白）';
  if (field === 'kind') return v === 'spoken' ? '廣播劇／談話' : '音樂';
  if (field === 'aliases') return v.split('\n').join('、');
  if (field === 'cover_id') return '另一張封面';
  return v;
}

function conflictText(c) {
  if (c.reason === 'gone') return '已不存在';
  if (c.field === 'row') return c.now ? '收錄已在專輯中' : '收錄之後被移除了';
  if (c.reason === 'failed') return '無法還原（例如會和其他收錄重複）';
  return `之後改成了「${shown(c.now, c.field)}」`;
}

function showConflicts(list, none) {
  showDialog((close) => html`<${Dialog} title=${none ? '無法撤回' : '部分欄位沒有撤回'} onClose=${close}
      actions=${html`<button class="btn filled" onClick=${close}>知道了</button>`}>
    <p>${none ? '這項修改的欄位之後都被改過，或對象已不存在，所以保留目前的值。' : '其他欄位已撤回。下面這些之後又被修改過，所以保留目前的值，不會覆蓋你後來的修改：'}</p>
    <ul class="plain-list">${list.map((c, i) => html`<li key=${i}><b>${c.name || '（已刪除）'}</b> 的${fieldName(c.target, c.field)}：${conflictText(c)}</li>`)}</ul>
  <//>`);
}

// ---- small form pieces ----

function Field({ label, value, onInput, multiline, rows = 3, placeholder, autofocus, wide }) {
  return html`<label class=${'field' + (wide ? ' span' : '')}>${label}
    ${multiline
      ? html`<textarea rows=${rows} value=${value} placeholder=${placeholder} onInput=${(e) => onInput(e.target.value)}></textarea>`
      : html`<input value=${value} placeholder=${placeholder} autofocus=${autofocus} onInput=${(e) => onInput(e.target.value)} />`}
  </label>`;
}

function KindPicker({ value, onChange, allowKeep }) {
  const opts = [...(allowKeep ? [['', '不變']] : []), ['music', '音樂'], ['spoken', '廣播劇／談話']];
  return html`<div class="field span">類型
    <div class="seg" role="radiogroup" aria-label="類型">${opts.map(([v, label]) => html`<label key=${v} class=${value === v ? 'on' : ''}>
      <input type="radio" checked=${value === v} onChange=${() => onChange(v)} />${label}</label>`)}</div>
    <span class="hint tight">廣播劇／談話會記住播放位置，也不會出現在「隨便聽一張」。</span>
  </div>`;
}

const lines = (s) => s.split('\n').map((x) => x.trim()).filter(Boolean);

// run wraps a dialog action: busy flag, error toast.
function useRunner() {
  const [busy, setBusy] = useState(false);
  const run = async (f) => {
    setBusy(true);
    try {
      await f();
    } catch (e) {
      toast(e.message, 'error');
    }
    setBusy(false);
  };
  return [busy, run];
}

function TextDialog({ title, label, initial = '', multiline, hint, action = '儲存', onSubmit, close }) {
  const [value, setValue] = useState(initial);
  const [busy, run] = useRunner();
  const submit = (e) => {
    e.preventDefault();
    run(async () => {
      if (await onSubmit(value) !== false) close();
    });
  };
  return html`<${Dialog} title=${title} onClose=${close}>
    <form onSubmit=${submit}>
      ${hint && html`<p class="hint">${hint}</p>`}
      <${Field} label=${label} value=${value} onInput=${setValue} multiline=${multiline} rows=${5} autofocus />
      <div class="dialog-actions">
        <button type="button" class="btn text" onClick=${close}>取消</button>
        <button class="btn filled" disabled=${busy || (!multiline && !value.trim())}>${action}</button>
      </div>
    </form>
  <//>`;
}

function Confirm({ title, children, action, danger, onConfirm, close }) {
  const [busy, run] = useRunner();
  return html`<${Dialog} title=${title} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class=${'btn filled' + (danger ? ' danger' : '')} disabled=${busy} onClick=${() => run(async () => { await onConfirm(); close(); })}>${action}</button>`}>
    ${children}
  <//>`;
}

const confirmDialog = (props) => showDialog((close) => html`<${Confirm} ...${props} close=${close} />`);

// ---- tracks ----

export function editTrack(trackId) {
  showDialog((close) => html`<${TrackEditor} id=${trackId} close=${close} />`);
}

function TrackEditor({ id, close }) {
  const t = useLoad(() => get('/tracks/' + id), [id]);
  const [form, setForm] = useState(null);
  const [busy, run] = useRunner();
  const d = t.data;
  const init = d && { title: d.title, artist: d.artist, version: d.version, kind: d.kind, aliases: d.aliases.join('\n') };
  const f = form || init;
  const set = (k) => (v) => setForm({ ...f, [k]: v });
  const save = () => run(async () => {
    if (!f.title.trim()) throw new Error('曲名不能是空白');
    const body = {};
    for (const k of ['title', 'artist', 'version', 'kind']) if (f[k] !== init[k]) body[k] = f[k];
    if (f.aliases !== init.aliases) body.aliases = lines(f.aliases);
    const res = await api('PATCH', '/tracks/' + id, body);
    done(res, '已更新歌曲資訊');
    close();
  });
  const restore = () => confirmDialog({
    title: '恢復原標籤', action: '恢復',
    children: html`<p>把「${d.title}」的曲名、歌手、類型改回匯入時音檔標籤的值。別名不變。可在修改紀錄撤回。</p>`,
    onConfirm: async () => done(await post(`/tracks/${id}/restore`), '已恢復原標籤'),
  });
  const destroy = () => confirmDialog({
    title: '永久刪除歌曲', action: '永久刪除', danger: true,
    children: html`<p>要永久刪除「${d.title}」嗎？</p>
      <p class="hint">會一併刪除它在所有專輯的收錄、歌單項目、收藏、歌詞與播放記錄。沒有其他歌曲使用的音檔會移到 Google Drive 垃圾桶（30 天內可在 Drive 還原）。這個操作無法在 Kanade 撤回。</p>`,
    onConfirm: async () => {
      const r = await api('DELETE', '/tracks/' + id);
      changed();
      toast(r.trash_failed ? '已刪除；有音檔沒能移到 Drive 垃圾桶' : '已永久刪除');
    },
  });
  return html`<${Dialog} title="編輯歌曲資訊" onClose=${close} actions=${d && html`
      <button class="btn text" onClick=${restore}>恢復原標籤</button>
      <button class="btn text danger-text" onClick=${destroy}>永久刪除…</button>
      <span class="grow"></span>
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy} onClick=${save}>儲存</button>`}>
    ${t.loading ? html`<${Spinner} />` : html`<${ErrorBox} error=${t.error} onRetry=${t.reload} />`}
    ${d && d.missing && html`<div class="task-error">這首歌的音檔已從 Google Drive 遺失，暫時無法播放。從 Drive 垃圾桶還原後會自動恢復；不要了的話可以永久刪除。</div>`}
    ${f && html`<div class="form-grid">
      <${Field} label="曲名" value=${f.title} onInput=${set('title')} wide />
      <${Field} label="歌手" value=${f.artist} onInput=${set('artist')} />
      <${Field} label="版本說明" value=${f.version} onInput=${set('version')} placeholder="Live、Remix、TV size…" />
      <${KindPicker} value=${f.kind} onChange=${set('kind')} />
      <${Field} label="別名（每行一個，搜尋時也找得到）" value=${f.aliases} onInput=${set('aliases')} multiline wide />
    </div>
    ${d.entries.length > 0 && html`<p class="hint">收錄於：${d.entries.map((e, i) => html`${i ? '、' : ''}<a class="link" href=${href('album/' + e.album_id)} onClick=${close}>${e.album}</a>`)}</p>`}`}
  <//>`;
}

// ---- albums ----

export function editAlbum(album) {
  showDialog((close) => html`<${AlbumEditor} album=${album} close=${close} />`);
}

function AlbumEditor({ album, close }) {
  const init = { title: album.title, album_artist: album.album_artist, date: album.date, catalog: album.catalog, edition: album.edition,
    aliases: album.aliases.join('\n'), kind: '' };
  const [f, setF] = useState(init);
  const start = album.entries.map((e) => ({ entry_id: e.entry_id, disc_no: String(e.disc_no), track_no: String(e.track_no), title: e.title, artist: e.artist }));
  const [rows, setRows] = useState(start);
  const [busy, run] = useRunner();
  const set = (k) => (v) => setF({ ...f, [k]: v });
  const setRow = (i, k) => (v) => setRows(rows.map((r, j) => (j === i ? { ...r, [k]: v } : r)));
  const save = () => run(async () => {
    if (!f.title.trim()) throw new Error('專輯名稱不能是空白');
    const body = {};
    for (const k of ['title', 'album_artist', 'date', 'catalog', 'edition']) if (f[k] !== init[k]) body[k] = f[k];
    if (f.aliases !== init.aliases) body.aliases = lines(f.aliases);
    if (f.kind) body.kind = f.kind;
    const entries = [];
    rows.forEach((r, i) => {
      const o = start[i], e = { entry_id: r.entry_id };
      for (const k of ['disc_no', 'track_no']) {
        if (r[k] === o[k]) continue;
        const n = Number(r[k]);
        if (!Number.isInteger(n) || n < (k === 'disc_no' ? 1 : 0) || n > 999) throw new Error(`第 ${i + 1} 列的${k === 'disc_no' ? '碟號' : '曲序'}不是有效的數字`);
        e[k] = n;
      }
      if (r.title !== o.title) {
        if (!r.title.trim()) throw new Error(`第 ${i + 1} 列的曲名是空白`);
        e.title = r.title;
      }
      if (r.artist !== o.artist) e.artist = r.artist;
      if (Object.keys(e).length > 1) entries.push(e);
    });
    if (entries.length) body.entries = entries;
    done(await api('PATCH', '/albums/' + album.id, body), '已更新專輯資訊');
    close();
  });
  return html`<${Dialog} title="編輯專輯資訊" wide onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy} onClick=${save}>儲存</button>`}>
    <p class="hint">只改 Kanade 的資料，不改寫音檔。每次儲存都會記在修改紀錄，可以撤回。</p>
    <div class="form-grid">
      <${Field} label="專輯名稱" value=${f.title} onInput=${set('title')} wide />
      <${Field} label="專輯歌手" value=${f.album_artist} onInput=${set('album_artist')} />
      <${Field} label="日期" value=${f.date} onInput=${set('date')} placeholder="2006-06-21" />
      <${Field} label="型號" value=${f.catalog} onInput=${set('catalog')} placeholder="VICL-61905" />
      <${Field} label="版本" value=${f.edition} onInput=${set('edition')} placeholder="初回限定盤、Remaster…" />
      <${KindPicker} value=${f.kind} onChange=${set('kind')} allowKeep />
      <${Field} label="別名（每行一個）" value=${f.aliases} onInput=${set('aliases')} multiline rows=${2} wide />
    </div>
    <div class="section-head"><h3 class="section-title small">歌曲</h3>
      <button class="btn text" disabled=${!f.album_artist.trim()} onClick=${() => setRows(rows.map((r) => ({ ...r, artist: f.album_artist })))}>全部的歌手設為專輯歌手</button></div>
    <div class="entry-rows" role="table" aria-label="歌曲">
      <div class="entry-row head" role="row"><span>碟</span><span>曲序</span><span>曲名</span><span>歌手</span></div>
      ${rows.map((r, i) => html`<div class="entry-row" role="row" key=${r.entry_id}>
        <input aria-label="碟號" inputmode="numeric" value=${r.disc_no} onInput=${(e) => setRow(i, 'disc_no')(e.target.value)} />
        <input aria-label="曲序" inputmode="numeric" value=${r.track_no} onInput=${(e) => setRow(i, 'track_no')(e.target.value)} />
        <input aria-label="曲名" value=${r.title} onInput=${(e) => setRow(i, 'title')(e.target.value)} />
        <input aria-label="歌手" value=${r.artist} placeholder="歌手" onInput=${(e) => setRow(i, 'artist')(e.target.value)} />
      </div>`)}
    </div>
  <//>`;
}

export function changeCover(album) {
  const input = document.createElement('input');
  input.type = 'file';
  input.accept = 'image/jpeg,image/png';
  input.onchange = async () => {
    const file = input.files[0];
    if (!file) return;
    if (file.size > 16 << 20) return toast('圖片超過 16 MB', 'error');
    toast('上傳封面中…');
    try {
      done(await api('PUT', `/albums/${album.id}/cover`, file, { contentType: file.type }), '已更換封面');
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  input.click();
}

export function restoreAlbum(album) {
  confirmDialog({
    title: '恢復原標籤', action: '恢復',
    children: html`<p>把「${album.title}」恢復成匯入時音檔標籤的樣子：</p>
      <ul class="plain-list"><li>專輯名稱、專輯歌手、日期，以及每首歌的曲名、歌手、碟號、曲序</li>
      <li>合併進來的歌曲回到原本的專輯，拆分出去的歌曲回到這張專輯</li>
      <li>型號、版本與 MusicBrainz 資料清除；別名與封面不變</li></ul>
      <p class="hint">可在修改紀錄撤回。</p>`,
    onConfirm: async () => done(await post(`/albums/${album.id}/restore`), '已恢復原標籤'),
  });
}

export function removeFromAlbum(album, item) {
  post(`/albums/${album.id}/remove`, { entries: [item.entryId] })
    .then((res) => done(res, `已從專輯移除「${item.title}」，歌曲仍在「歌曲」分頁`), (e) => toast(e.message, 'error'));
}

export function removeAlbum(album) {
  showDialog((close) => html`<${RemoveAlbum} album=${album} close=${close} />`);
}

function RemoveAlbum({ album, close }) {
  const [mode, setMode] = useState('keep');
  const [busy, run] = useRunner();
  const submit = () => run(async () => {
    const res = await api('DELETE', `/albums/${album.id}${mode === 'delete' ? '?tracks=1' : ''}`);
    if (mode === 'delete') {
      changed();
      toast(`已刪除專輯與 ${res.deleted_tracks} 首歌曲${res.trash_failed ? '；有音檔沒能移到 Drive 垃圾桶' : ''}`);
    } else {
      done(res, `已移除「${album.title}」，歌曲仍在「歌曲」分頁`);
    }
    close();
    go('library/albums');
  });
  return html`<${Dialog} title="移除專輯" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class=${'btn filled' + (mode === 'delete' ? ' danger' : '')} disabled=${busy} onClick=${submit}>${mode === 'delete' ? '永久刪除' : '移除'}</button>`}>
    <div class="choice-list" role="radiogroup">
      <label class=${mode === 'keep' ? 'on' : ''}><input type="radio" checked=${mode === 'keep'} onChange=${() => setMode('keep')} />
        <span><b>只移除專輯</b><span class="sub">歌曲留在「歌曲」分頁，音檔不動。可在修改紀錄撤回。</span></span></label>
      <label class=${mode === 'delete' ? 'on' : ''}><input type="radio" checked=${mode === 'delete'} onChange=${() => setMode('delete')} />
        <span><b>連同歌曲永久刪除</b><span class="sub">只在這張專輯的歌曲從曲庫刪除，音檔移到 Google Drive 垃圾桶（30 天內可在 Drive 還原），無法在 Kanade 撤回。也收錄在其他專輯的歌曲與音檔不受影響。</span></span></label>
    </div>
  <//>`;
}

export function splitAlbum(album) {
  showDialog((close) => html`<${Split} album=${album} close=${close} />`);
}

function Split({ album, close }) {
  const [title, setTitle] = useState(album.title);
  const [sel, setSel] = useState(() => new Set());
  const [busy, run] = useRunner();
  const toggle = (id) => setSel((s) => {
    const n = new Set(s);
    if (n.has(id)) n.delete(id);
    else n.add(id);
    return n;
  });
  const submit = () => run(async () => {
    const res = await post(`/albums/${album.id}/split`, { title, entries: [...sel] });
    if (done(res, `已拆分出「${title}」`)) {
      close();
      go('album/' + res.album_id);
    }
  });
  return html`<${Dialog} title="拆分專輯" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !sel.size || !title.trim()} onClick=${submit}>拆分 ${sel.size || ''} 首</button>`}>
    <p class="hint">選取的歌曲會移到一張新專輯（專輯歌手、日期、封面沿用這張）。可在修改紀錄撤回。</p>
    <${Field} label="新專輯名稱" value=${title} onInput=${setTitle} />
    <ul class="check-list">${album.entries.map((e) => html`<li key=${e.entry_id}><label>
      <input type="checkbox" checked=${sel.has(e.entry_id)} onChange=${() => toggle(e.entry_id)} />
      <span class="num">${e.disc_no > 1 ? e.disc_no + '-' : ''}${e.track_no || ''}</span><span class="grow">${e.title}</span></label></li>`)}</ul>
  <//>`;
}

export function mergeAlbum(album) {
  showDialog((close) => html`<${Merge} album=${album} close=${close} />`);
}

function Merge({ album, close }) {
  const [q, setQ] = useState(album.title);
  const [term, setTerm] = useState(album.title);
  const [target, setTarget] = useState(null);
  const [busy, run] = useRunner();
  useEffect(() => {
    const t = setTimeout(() => setTerm(q), 250);
    return () => clearTimeout(t);
  }, [q]);
  const res = useLoad(() => (term.trim() ? get('/search?q=' + encodeURIComponent(term)) : Promise.resolve({ albums: [] })), [term]);
  const albums = res.data ? res.data.albums.filter((a) => a.id !== album.id) : [];
  const submit = () => run(async () => {
    if (done(await post(`/albums/${album.id}/merge`, { into: target.id }), `已合併到「${target.title}」`)) {
      close();
      go('album/' + target.id);
    }
  });
  if (target) {
    return html`<${Dialog} title="合併專輯" onClose=${close} actions=${html`
        <button class="btn text" onClick=${() => setTarget(null)}>返回</button>
        <button class="btn filled" disabled=${busy} onClick=${submit}>合併</button>`}>
      <p>把「${album.title}」的 ${album.entries.length} 首移到「${target.title}」${target.album_artist ? `（${target.album_artist}）` : ''}？</p>
      <ul class="plain-list hint">
        <li>同一個音檔已在「${target.title}」的不會重複加入。</li>
        ${album.original && html`<li>之後用「${album.title}」原本的標籤匯入的檔案也會歸到「${target.title}」。</li>`}
        <li>可在修改紀錄撤回，或在專輯頁「恢復原標籤」拆回來。</li>
      </ul>
    <//>`;
  }
  return html`<${Dialog} title="合併到其他專輯" onClose=${close} actions=${html`<button class="btn text" onClick=${close}>取消</button>`}>
    <p class="hint">選擇要把「${album.title}」併入的專輯，例如同一張專輯被拆成兩張時。</p>
    <label class="search-field small"><${Icon} name="search" /><input type="search" value=${q} placeholder="搜尋專輯" aria-label="搜尋專輯"
      onInput=${(e) => setQ(e.target.value)} autofocus /></label>
    ${res.loading ? html`<${Spinner} />` : html`<${ErrorBox} error=${res.error} onRetry=${res.reload} />`}
    ${res.data && !albums.length && html`<${Empty} icon="album">找不到其他專輯<//>`}
    <ul class="list">${albums.map((a) => html`<li key=${a.id}><button class="row plain wide" onClick=${() => setTarget(a)}>
      <${Cover} id=${a.cover_id} size=${96} className="thumb" />
      <span class="grow track-text"><span class="title">${a.title}</span><span class="sub">${[a.album_artist, a.date, `${a.tracks} 首`].filter(Boolean).join(' · ')}</span></span>
    </button></li>`)}</ul>
  <//>`;
}

// ---- MusicBrainz ----

export function identifyAlbum(album) {
  showDialog((close) => html`<${Identify} album=${album} close=${close} />`);
}

// The artist to search with: the album artist, or for "Various Artists" the most common track artist
// (singles get that album artist when their instrumentals credit someone else).
function searchArtist(album) {
  if (album.album_artist && album.album_artist.toLowerCase() !== 'various artists') return album.album_artist;
  const n = {};
  for (const e of album.entries) if (e.artist) n[e.artist] = (n[e.artist] || 0) + 1;
  return Object.keys(n).sort((a, b) => n[b] - n[a])[0] || '';
}

function Identify({ album, close }) {
  const [q, setQ] = useState({ title: album.title, artist: searchArtist(album), catalog: album.catalog });
  const [cands, setCands] = useState(null);
  const [prop, setProp] = useState(null);
  const [picked, setPicked] = useState(() => new Set());
  const [linkMB, setLinkMB] = useState(true);
  const [cover, setCover] = useState(false);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const step = async (f) => {
    setBusy(true);
    setError(null);
    try {
      await f();
    } catch (e) {
      setError(e);
    }
    setBusy(false);
  };
  const search = (e) => {
    e && e.preventDefault();
    step(async () => {
      const p = new URLSearchParams({ title: q.title, artist: q.artist, catalog: q.catalog });
      setCands((await get(`/albums/${album.id}/identify?${p}`)).candidates);
    });
  };
  const choose = (c) => step(async () => {
    const p = await get(`/albums/${album.id}/identify/${c.id}`);
    setPicked(new Set(p.changes.filter((x) => x.default && !x.field.startsWith('mb_')).map((x) => x.key)));
    setCover(p.cover && !album.cover_id);
    setProp(p);
  });
  const apply = () => step(async () => {
    const keys = [...picked];
    if (linkMB) keys.push(...prop.changes.filter((x) => x.field.startsWith('mb_')).map((x) => x.key));
    if (cover) keys.push('cover');
    done(await post(`/albums/${album.id}/identify/${prop.release.id}`, { keys }), '已套用 MusicBrainz 的資料');
    close();
  });
  const toggle = (key) => setPicked((s) => {
    const n = new Set(s);
    if (n.has(key)) n.delete(key);
    else n.add(key);
    return n;
  });

  if (prop) {
    const visible = prop.changes.filter((x) => !x.field.startsWith('mb_'));
    const groups = [];
    for (const c of visible) {
      const g = groups.find((x) => x.label === c.label);
      if (g) g.items.push(c);
      else groups.push({ label: c.label, items: [c] });
    }
    const r = prop.release;
    return html`<${Dialog} title="套用 MusicBrainz 資料" wide onClose=${close} actions=${html`
        <button class="btn text" onClick=${() => setProp(null)}>返回</button>
        <span class="grow"></span>
        <button class="btn text" onClick=${close}>取消</button>
        <button class="btn filled" disabled=${busy || (!picked.size && !cover && !linkMB)} onClick=${apply}>套用</button>`}>
      <div class="mb-head"><b>${r.title}</b><span class="sub">${[r.artist, r.date, r.country, r.label, r.catalog, r.format].filter(Boolean).join(' · ')}</span>
        <a class="sub link" href=${'https://musicbrainz.org/release/' + r.id} target="_blank" rel="noopener noreferrer">在 MusicBrainz 查看</a></div>
      <p class="hint">對上 ${prop.matched} 首${prop.unmatched ? `，${prop.unmatched} 首對不上（保持不變）` : ''}。勾選要套用的欄位；長度不符的預設不勾。套用後可在修改紀錄撤回。</p>
      ${visible.length > 0 && html`<div class="file-tools">
        <button class="btn text" onClick=${() => setPicked(new Set(visible.map((x) => x.key)))}>全選</button>
        <button class="btn text" onClick=${() => setPicked(new Set())}>全不選</button></div>`}
      ${!visible.length && html`<p>這張專輯的資料已和 MusicBrainz 一致。</p>`}
      ${groups.map((g) => html`<div class="diff-group" key=${g.label}>
        <div class="diff-label">${g.label || '專輯'}</div>
        ${g.items.map((c) => html`<label class="diff-row" key=${c.key}>
          <input type="checkbox" checked=${picked.has(c.key)} onChange=${() => toggle(c.key)} />
          <span class="diff-field">${fieldName(c.target, c.field)}</span>
          <span class="diff-values"><span class="old">${shown(c.old, c.field)}</span><span class="arrow">→</span><span class="new">${c.new}</span>
            ${c.warn && html`<span class="warn-text">${c.warn}</span>`}</span>
        </label>`)}
      </div>`)}
      <div class="diff-group">
        ${prop.cover && html`<label class="diff-row"><input type="checkbox" checked=${cover} onChange=${() => setCover(!cover)} />
          <span class="diff-field">封面</span><span class="diff-values">使用 Cover Art Archive 的封面${album.cover_id ? '（取代目前的封面）' : ''}</span></label>`}
        <label class="diff-row"><input type="checkbox" checked=${linkMB} onChange=${() => setLinkMB(!linkMB)} />
          <span class="diff-field">連結</span><span class="diff-values">記住對應的 MusicBrainz 發行與錄音 ID</span></label>
      </div>
      <${ErrorBox} error=${error} />
    <//>`;
  }

  return html`<${Dialog} title="從 MusicBrainz 辨識" wide onClose=${close} actions=${html`<button class="btn text" onClick=${close}>關閉</button>`}>
    <p class="hint">按「搜尋」時，會把下面三個欄位，以及這張專輯的年份與歌曲數，送到 MusicBrainz（musicbrainz.org）查詢；不會送出音檔或其他資料。找到的資料只在你勾選並套用後才會寫入。</p>
    <form class="form-grid" onSubmit=${search}>
      <${Field} label="專輯名稱" value=${q.title} onInput=${(v) => setQ({ ...q, title: v })} wide />
      <${Field} label="歌手" value=${q.artist} onInput=${(v) => setQ({ ...q, artist: v })} />
      <${Field} label="型號" value=${q.catalog} onInput=${(v) => setQ({ ...q, catalog: v })} placeholder="例如 VICL-61905" />
      <div class="span"><button class="btn tonal" disabled=${busy || (!q.title.trim() && !q.catalog.trim())}><${Icon} name="search" />搜尋</button></div>
    </form>
    <${ErrorBox} error=${error} />
    ${busy && html`<${Spinner} />`}
    ${cands && !cands.length && html`<${Empty} icon="search">MusicBrainz 沒有找到。可以改用型號，或縮短專輯名稱再試。<//>`}
    ${cands && cands.length > 0 && html`<ul class="list">${cands.map((c) => html`<li key=${c.id}>
      <button class="row plain wide" disabled=${busy} onClick=${() => choose(c)}>
        <span class="grow track-text"><span class="title">${c.title}${c.disambiguation ? html` <span class="sub">(${c.disambiguation})</span>` : ''}</span>
          <span class="sub">${[c.artist, c.date, c.country, c.label, c.catalog].filter(Boolean).join(' · ')}</span>
          <span class="sub">${[c.format, `${c.tracks} 首`, c.type].filter(Boolean).join(' · ')}${album.tracks !== c.tracks ? `（這張有 ${album.tracks} 首）` : ''}</span></span>
        <span class="chip">${c.score}</span>
      </button></li>`)}</ul>`}
  <//>`;
}

// ---- artists ----

export function renameArtist(artist) {
  showDialog((close) => html`<${TextDialog} title="歌手改名" label="名稱" initial=${artist.name} close=${close}
    hint=${`所有歌手或專輯歌手剛好是「${artist.name}」的歌曲與專輯都會改成新名稱。新名稱已存在時會合在一起。可在修改紀錄撤回。`}
    onSubmit=${async (name) => {
      const res = await api('PATCH', '/artists/' + artist.id, { name });
      if (done(res, '已改名') && res.artist_id !== artist.id) go('artist/' + res.artist_id);
    }} />`);
}

export function editArtistAliases(artist) {
  showDialog((close) => html`<${TextDialog} title="編輯別名" label="別名（每行一個）" multiline initial=${artist.aliases.join('\n')} close=${close}
    hint="例如羅馬字、舊名、簡稱。搜尋別名時也會找到這位歌手。"
    onSubmit=${async (text) => done(await api('PATCH', '/artists/' + artist.id, { aliases: lines(text) }), '已更新別名')} />`);
}

// ---- files gone from Drive (P2-6) ----

export function Missing() {
  const list = useLoad(() => get('/library/missing'), []);
  return html`<section>
    <h1 class="page-title">Drive 中遺失的檔案</h1>
    <p class="hint">這些歌曲的音檔在 Google Drive 被刪除或移到垃圾桶，暫時無法播放；曲庫資料都還在。從 Drive 垃圾桶還原後，下一次同步（或設定頁的「完整對帳」）會自動恢復。若是不要了，可以在歌曲的「編輯資訊」裡永久刪除。</p>
    ${list.loading ? html`<${Spinner} />` : html`<${ErrorBox} error=${list.error} onRetry=${list.reload} />`}
    ${list.data && !list.data.length && html`<${Empty} icon="note">沒有遺失的檔案。<//>`}
    ${list.data && list.data.length > 0 && html`<ul class="list">${list.data.map((m) => html`<li key=${m.track_id}>
      <button class="row plain wide" onClick=${() => editTrack(m.track_id)}>
        <span class="avatar"><${Icon} name="note" /></span>
        <span class="grow track-text"><span class="title">${m.title}</span>
          <span class="sub">${[m.artist || '未知歌手', m.album, m.format.toUpperCase()].filter(Boolean).join(' · ')}</span></span>
      </button></li>`)}</ul>`}
  </section>`;
}

// ---- the edit log ----

const sourceNames = { user: '手動', identify: 'MusicBrainz', restore: '恢復原標籤', undo: '撤回' };
const when = (ms) => new Date(ms).toLocaleString('zh-TW', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });

export function Edits() {
  const rev = useLibRev();
  const first = useLoad(() => get('/edits?limit=50'), [], rev);
  const [more, setMore] = useState({ key: null, pages: [], done: false });
  const [open, setOpen] = useState(null);
  const pages = more.key === rev ? more.pages : [];
  const list = [...(first.data || []), ...pages.flat()];
  const loadMore = async () => {
    try {
      const next = await get(`/edits?limit=50&before=${list[list.length - 1].id}`);
      setMore({ key: rev, pages: [...pages, next], done: next.length < 50 });
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  return html`<section>
    <h1 class="page-title">修改紀錄</h1>
    <p class="hint">曲庫資料的每次修改。「撤回」只還原那次改過的欄位；之後又被改過的欄位會保留，不會覆蓋你後來的修改。</p>
    ${first.loading && !first.data ? html`<${Spinner} />` : html`<${ErrorBox} error=${first.error} onRetry=${first.reload} />`}
    ${first.data && !list.length && html`<${Empty} icon="history">還沒有修改紀錄。<//>`}
    <ul class="edit-log">${list.map((g) => html`<li key=${g.id} class=${g.undone_by ? 'undone' : ''}>
      <div class="edit-head">
        <button class="plain grow edit-title" onClick=${() => setOpen(open === g.id ? null : g.id)} aria-expanded=${open === g.id}>
          <span class="title">${g.summary}</span>
          <span class="sub">${when(g.created_at)} · ${sourceNames[g.source] || g.source}${g.changes ? ` · ${g.changes} 個欄位` : ''}${g.undone_by ? ' · 已撤回' : ''}</span>
        </button>
        ${!g.undone_by && g.changes > 0 && html`<button class="btn text" onClick=${() => undo(g.id)}><${Icon} name="undo" />撤回</button>`}
      </div>
      ${open === g.id && html`<${EditDetails} id=${g.id} />`}
    </li>`)}</ul>
    ${first.data && list.length >= 50 && !(more.key === rev && more.done) && html`<div class="actions center">
      <button class="btn tonal" onClick=${loadMore}>載入更多</button></div>`}
  </section>`;
}

function EditDetails({ id }) {
  const g = useLoad(() => get('/edits/' + id), [id]);
  if (g.loading) return html`<${Spinner} />`;
  if (g.error) return html`<${ErrorBox} error=${g.error} onRetry=${g.reload} />`;
  if (!g.data.edits || !g.data.edits.length) return html`<p class="hint">這項操作沒有可撤回的欄位（例如永久刪除）。</p>`;
  return html`<ul class="edit-details">${g.data.edits.map((e, i) => html`<li key=${i}>
    <span class="diff-field">${e.name || '（已刪除）'} · ${fieldName(e.target, e.field)}</span>
    ${e.field === 'row'
      ? html`<span>${e.new ? '放回專輯' : '從專輯移除'}</span>`
      : html`<span class="diff-values"><span class="old">${shown(e.old, e.field, e.old_label)}</span><span class="arrow">→</span><span class="new">${shown(e.new, e.field, e.new_label)}</span></span>`}
  </li>`)}</ul>`;
}

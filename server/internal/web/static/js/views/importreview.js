import { useEffect, useState } from '../../vendor/hooks.module.js';
import { get, post } from '../api.js';
import { go, href } from '../router.js';
import { Dialog, Empty, ErrorBox, Icon, IconButton, Spinner, fmtTime, html, openMenu, showDialog, toast } from '../ui.js';

// The import preview (P2-3): how a batch's files will become albums, corrected before anything is
// uploaded.

const warningText = {
  no_album_artist: '沒有專輯歌手',
  track_numbers: '曲序不完整或重複',
  lost_text: '文字看起來已損毀（原檔資料遺失）',
  guessed_encoding: '部分文字的編碼是猜的；有亂碼時請在上方改用其他編碼',
  same_audio: '有歌曲與曲庫中的音訊相同（標籤不同）',
};
const encodingNames = { '': '自動判斷', cp932: 'Shift-JIS（CP932，日文）', gbk: 'GBK（簡體中文）', big5: 'Big5（繁體中文）', latin1: 'Latin-1（西歐）', cp1252: 'CP1252（西歐）', 'utf-16': 'UTF-16' };
const stateText = { skipped: '不匯入', failed: '無法讀取', excluded: '已手動排除', expanded: '已展開', published: '已入庫', duplicate: '已存在', pending: '等待中' };

function reason(it) {
  if (it.state === 'excluded') return '已手動排除';
  if (it.role === 'sidecar') return it.path.toLowerCase().endsWith('.log') ? '翻錄紀錄，會和專輯一起保存' : 'CUE 檔，會和專輯一起保存';
  if (it.role === 'zip' && it.state === 'expanded') return '壓縮檔已展開';
  const e = it.error || '';
  if (e.startsWith('not a recognized audio file')) return '不是可辨識的音訊檔';
  if (e.includes('is not playable yet')) return `${(it.format || '').toUpperCase()} 格式暫不支援（之後會轉成 FLAC）`;
  if (e.startsWith('cannot read audio')) return '無法讀取：' + e.slice(18);
  return e || stateText[it.state] || it.state;
}

const num = (p) => `${p.disc > 1 ? p.disc + '-' : ''}${p.track ? String(p.track).padStart(2, '0') : '—'}`;

export function ImportReview({ id }) {
  const [p, setP] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const load = () => get(`/imports/${id}/preview`).then((v) => { setP(v); setError(null); }, setError);
  useEffect(() => {
    load();
  }, [id]);
  useEffect(() => { // analysis takes a moment: check again until it is done
    if (!p || p.state !== 'analyzing') return;
    const t = setTimeout(load, 1500);
    return () => clearTimeout(t);
  }, [p]);

  // op sends one change; the server answers with the whole new preview.
  const op = async (body) => {
    setBusy(true);
    try {
      setP(await post(`/imports/${id}/plan`, body));
      return true;
    } catch (e) {
      toast(e.message, 'error');
      return false;
    } finally {
      setBusy(false);
    }
  };
  const start = async () => {
    setBusy(true);
    try {
      await post(`/imports/${id}/start`);
      toast('開始匯入');
      go('tasks');
    } catch (e) {
      toast(e.message, 'error');
      setBusy(false);
    }
  };
  const cancel = () => showDialog((close) => html`<${Dialog} title="取消匯入" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>返回</button>
      <button class="btn filled danger" onClick=${async () => {
        try {
          await post(`/imports/${id}/cancel`);
          close();
          toast('已取消匯入');
          go('tasks');
        } catch (e) {
          toast(e.message, 'error');
        }
      }}>取消匯入</button>`}>
    <p>不匯入這批檔案？${p && p.kind === 'upload' ? '已上傳到伺服器暫存區的檔案會刪除。' : '原始檔案不受影響。'}</p>
  <//>`);

  if (error && !p) return html`<${ErrorBox} error=${error} onRetry=${load} />`;
  if (!p) return html`<${Spinner} />`;
  const review = p.state === 'review';
  const songs = p.groups.reduce((n, g) => n + g.items.length, 0) + p.standalone.length;
  const groupMenu = (e, g) => openMenu(e, [
    { icon: 'edit', label: '編輯…', onClick: () => editGroup(g, p, op) },
    p.groups.length > 1 && { icon: 'merge', label: '合併到…', onClick: () => moveItems(g.items.map((i) => i.id), p, op, g.key) },
    { icon: 'note', label: '拆成單曲', onClick: () => op({ op: 'standalone', group: g.key }) },
    { icon: 'close', label: '整組不匯入', onClick: () => op({ op: 'exclude', items: g.items.map((i) => i.id) }) },
  ]);
  const itemMenu = (e, it, g) => openMenu(e, [
    { icon: 'edit', label: '編輯…', onClick: () => editItem(it, op) },
    { icon: 'album', label: g ? '移到…' : '加入專輯…', onClick: () => moveItems([it.id], p, op, g && g.key) },
    g && { icon: 'note', label: '改為單曲', onClick: () => op({ op: 'move', items: [it.id], into: '' }) },
    { icon: 'close', label: '不匯入', onClick: () => op({ op: 'exclude', items: [it.id] }) },
  ]);
  const detected = Object.entries(p.detected || {});

  return html`<section class="import-review">
    <div class="page-head">
      <div class="grow">
        <h1 class="page-title">匯入預覽</h1>
        <div class="sub">${{ upload: '上傳', local: '伺服器資料夾', download: 'BT 下載' }[p.kind] || p.kind}${p.kind !== 'upload' && p.source ? '：' + p.source : ''}</div>
      </div>
      ${review && html`<div class="actions tight">
        <button class="btn text" disabled=${busy} onClick=${cancel}>取消匯入</button>
        <button class="btn filled" disabled=${busy || !songs} onClick=${start}><${Icon} name="upload" />開始匯入 ${songs} 首</button>
      </div>`}
    </div>
    ${p.state === 'analyzing' && html`<div class="card pad"><${Spinner} /><p class="center sub">正在讀取標籤與展開壓縮檔…</p></div>`}
    ${p.state !== 'analyzing' && !review && html`<div class="card pad">
      這批匯入${{ running: '正在進行', done: '已完成', canceled: '已取消' }[p.state] || p.state}。<a class="link" href=${href('tasks')}>到任務頁查看</a></div>`}
    ${p.state !== 'analyzing' && html`
      <p class="hint">確認每組會成為哪張專輯；這裡的修改不會改寫音檔，匯入後也可以再整理。</p>
      ${review && html`<div class="toolbar">
        <button class="btn tonal" disabled=${busy} onClick=${() => op({ op: 'folders' })}><${Icon} name="album" />每個子資料夾是一張專輯</button>
        <label class="select-field">文字編碼
          <select value=${p.encoding} disabled=${busy} onChange=${(e) => op({ op: 'encoding', encoding: e.target.value })}>
            ${['', ...p.encodings].map((k) => html`<option value=${k}>${encodingNames[k] || k}</option>`)}
          </select>
        </label>
        ${detected.length > 0 && !p.encoding && html`<span class="sub">自動判斷用了：${detected.map(([k, n]) => `${encodingNames[k] || k} ${n} 個檔案`).join('、')}</span>`}
      </div>`}
      <div class="sub summary-line">${p.groups.length} 張專輯 · ${p.standalone.length} 首單曲${p.other.length ? ` · ${p.other.length} 個其他檔案` : ''}</div>
      ${!songs && html`<${Empty} icon="upload">沒有可以匯入的歌曲。<//>`}
      ${p.groups.map((g) => html`<article class="card import-group" key=${g.key}>
        <div class="group-head">
          <div class="grow">
            <h2 class="group-title">${g.album}</h2>
            <div class="sub">${[g.album_artist || '（沒有專輯歌手）', g.date, `${g.items.length} 首`, g.kind === 'spoken' ? '廣播劇／談話' : g.kind === 'mixed' ? '音樂與談話混合' : ''].filter(Boolean).join(' · ')}</div>
            <div class="sub path">${g.folders.map((f) => (f === '.' ? '（最上層）' : f + '/')).join('、')}</div>
          </div>
          ${review && html`<${IconButton} icon="more" label="這組的更多操作" onClick=${(e) => groupMenu(e, g)} />`}
        </div>
        <div class="chips">
          ${g.existing ? html`<span class="pill">加入曲庫中的「${g.existing.title}」（已有 ${g.existing.tracks} 首）</span>` : html`<span class="pill good">新專輯</span>`}
          ${g.existing && review && html`<label class="pill check"><input type="checkbox" checked=${g.new_album} disabled=${busy}
            onChange=${(e) => op({ op: 'group', group: g.key, new_album: e.target.checked })} />改為另建一張專輯</label>`}
          ${g.new_album && !g.existing && html`<span class="pill">另建（與同批另一組標籤相同）</span>`}
          ${g.warnings.map((w) => html`<span class="pill warn">${warningText[w] || w}</span>`)}
        </div>
        <${ItemRows} items=${g.items} review=${review} onMenu=${(e, it) => itemMenu(e, it, g)} />
      </article>`)}
      ${p.standalone.length > 0 && html`<article class="card import-group">
        <div class="group-head"><div class="grow"><h2 class="group-title">單曲</h2><div class="sub">沒有專輯標籤，各自成為獨立歌曲</div></div></div>
        <${ItemRows} items=${p.standalone} review=${review} onMenu=${(e, it) => itemMenu(e, it, null)} />
      </article>`}
      ${p.other.length > 0 && html`<h2 class="section-title">其他檔案</h2>
        <ul class="items">${p.other.map((it) => html`<li key=${it.id}>
          <span class="grow path">${it.path}</span>
          <span class="sub">${reason(it)}</span>
          ${review && it.state === 'excluded' && html`<button class="btn text" disabled=${busy} onClick=${() => op({ op: 'include', items: [it.id] })}>重新加入</button>`}
        </li>`)}</ul>`}
    `}
  </section>`;
}

function ItemRows({ items, review, onMenu }) {
  return html`<ol class="import-items">${items.map((it) => html`<li key=${it.id}>
    <span class="num">${num(it.plan)}</span>
    <span class="grow track-text">
      <span class="title">${it.plan.title}</span>
      <span class="sub">${[it.plan.artist || '未知歌手', it.plan.kind === 'spoken' && '談話', it.format && it.format.toUpperCase(), it.duration_ms && fmtTime(it.duration_ms)].filter(Boolean).join(' · ')}</span>
      ${it.same_audio && html`<span class="warn-text">曲庫已有相同音訊：「${it.same_audio}」</span>`}
      <span class="sub path small">${it.path}</span>
    </span>
    ${review && html`<${IconButton} icon="more" label="更多" className="row-more" onClick=${(e) => onMenu(e, it)} />`}
  </li>`)}</ol>`;
}

function editGroup(g, p, op) {
  showDialog((close) => html`<${GroupEditor} g=${g} op=${op} close=${close} />`);
}

function GroupEditor({ g, op, close }) {
  const init = { album: g.album, album_artist: g.album_artist, date: g.date, kind: g.kind === 'mixed' ? '' : g.kind };
  const [f, setF] = useState(init);
  const start = g.items.map((it) => ({ id: it.id, disc: String(it.plan.disc), track: String(it.plan.track), title: it.plan.title, artist: it.plan.artist }));
  const [rows, setRows] = useState(start);
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setF({ ...f, [k]: e.target.value });
  const setRow = (i, k) => (e) => setRows(rows.map((r, j) => (j === i ? { ...r, [k]: e.target.value } : r)));
  const save = async () => {
    if (!f.album.trim()) return toast('專輯名稱不能是空白', 'error');
    setBusy(true);
    const body = { op: 'group', group: g.key };
    for (const k of ['album', 'album_artist', 'date']) if (f[k] !== init[k]) body[k] = f[k];
    if (f.kind && f.kind !== init.kind) body.kind = f.kind;
    let ok = Object.keys(body).length === 2 || await op(body);
    for (let i = 0; ok && i < rows.length; i++) {
      const r = rows[i], o = start[i], b = { op: 'items', items: [r.id] };
      if (r.title !== o.title) b.title = r.title;
      if (r.artist !== o.artist) b.artist = r.artist;
      if (r.disc !== o.disc) b.disc = Number(r.disc);
      if (r.track !== o.track) b.track = Number(r.track);
      if (Object.keys(b).length > 2) ok = await op(b);
    }
    setBusy(false);
    if (ok) close();
  };
  return html`<${Dialog} title="編輯這組" wide onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy} onClick=${save}>套用</button>`}>
    <div class="form-grid">
      <label class="field span">專輯名稱<input value=${f.album} onInput=${set('album')} /></label>
      <label class="field">專輯歌手<input value=${f.album_artist} onInput=${set('album_artist')} /></label>
      <label class="field">日期<input value=${f.date} onInput=${set('date')} /></label>
      <label class="field">類型<select value=${f.kind} onChange=${set('kind')}>
        ${!init.kind && html`<option value="">（混合，不變）</option>`}
        <option value="music">音樂</option><option value="spoken">廣播劇／談話</option></select></label>
    </div>
    ${g.existing && html`<p class="hint">這組會加入曲庫中的「${g.existing.title}」，專輯名稱以曲庫為準；要用這裡的名稱，請勾選「改為另建一張專輯」。</p>`}
    <h3 class="section-title small">歌曲</h3>
    <div class="entry-rows">
      <div class="entry-row head"><span>碟</span><span>曲序</span><span>曲名</span><span>歌手</span></div>
      ${rows.map((r, i) => html`<div class="entry-row" key=${r.id}>
        <input aria-label="碟號" inputmode="numeric" value=${r.disc} onInput=${setRow(i, 'disc')} />
        <input aria-label="曲序" inputmode="numeric" value=${r.track} onInput=${setRow(i, 'track')} />
        <input aria-label="曲名" value=${r.title} onInput=${setRow(i, 'title')} />
        <input aria-label="歌手" value=${r.artist} onInput=${setRow(i, 'artist')} />
      </div>`)}
    </div>
  <//>`;
}

function editItem(it, op) {
  showDialog((close) => html`<${ItemEditor} it=${it} op=${op} close=${close} />`);
}

function ItemEditor({ it, op, close }) {
  const [f, setF] = useState({ title: it.plan.title, artist: it.plan.artist, disc: String(it.plan.disc), track: String(it.plan.track), kind: it.plan.kind });
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setF({ ...f, [k]: e.target.value });
  const save = async () => {
    setBusy(true);
    const ok = await op({ op: 'items', items: [it.id], title: f.title, artist: f.artist, disc: Number(f.disc), track: Number(f.track), kind: f.kind });
    setBusy(false);
    if (ok) close();
  };
  return html`<${Dialog} title="編輯歌曲" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !f.title.trim()} onClick=${save}>套用</button>`}>
    <p class="sub path">${it.path}</p>
    <div class="form-grid">
      <label class="field span">曲名<input value=${f.title} onInput=${set('title')} /></label>
      <label class="field span">歌手<input value=${f.artist} onInput=${set('artist')} /></label>
      <label class="field">碟號<input inputmode="numeric" value=${f.disc} onInput=${set('disc')} /></label>
      <label class="field">曲序<input inputmode="numeric" value=${f.track} onInput=${set('track')} /></label>
      <label class="field span">類型<select value=${f.kind} onChange=${set('kind')}>
        <option value="music">音樂</option><option value="spoken">廣播劇／談話</option></select></label>
    </div>
  <//>`;
}

// moveItems puts files into another group, a new album, or out of any album.
function moveItems(ids, p, op, from) {
  showDialog((close) => html`<${MoveDialog} ids=${ids} p=${p} op=${op} from=${from} close=${close} />`);
}

function MoveDialog({ ids, p, op, from, close }) {
  const [name, setName] = useState('');
  const [artist, setArtist] = useState('');
  const run = async (body) => {
    if (await op({ op: 'move', items: ids, ...body })) close();
  };
  return html`<${Dialog} title=${ids.length > 1 ? `移動 ${ids.length} 首` : '移到…'} onClose=${close} actions=${html`<button class="btn text" onClick=${close}>取消</button>`}>
    <ul class="list">${p.groups.filter((g) => g.key !== from).map((g) => html`<li key=${g.key}>
      <button class="row plain wide" onClick=${() => run({ into: g.key })}>
        <span class="avatar"><${Icon} name="album" /></span>
        <span class="grow track-text"><span class="title">${g.album}</span><span class="sub">${[g.album_artist, `${g.items.length} 首`].filter(Boolean).join(' · ')}</span></span>
      </button></li>`)}
      ${from && html`<li><button class="row plain wide" onClick=${() => run({ into: '' })}>
        <span class="avatar"><${Icon} name="note" /></span><span class="grow">不屬於任何專輯（單曲）</span></button></li>`}
    </ul>
    <h3 class="section-title small">新專輯</h3>
    <form class="form-grid" onSubmit=${(e) => { e.preventDefault(); if (name.trim()) run({ into: 'new', album: name, album_artist: artist }); }}>
      <label class="field">專輯名稱<input value=${name} onInput=${(e) => setName(e.target.value)} /></label>
      <label class="field">專輯歌手<input value=${artist} onInput=${(e) => setArtist(e.target.value)} /></label>
      <div class="span"><button class="btn tonal" disabled=${!name.trim()}>移到新專輯</button></div>
    </form>
  <//>`;
}

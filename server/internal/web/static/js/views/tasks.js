import { useCallback, useEffect, useRef, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { href } from '../router.js';
import { Dialog, Empty, ErrorBox, Icon, IconButton, Spinner, fmtBytes, html, toast } from '../ui.js';
import { confirmDialog } from './organize.js';

const downloadStates = {
  metadata: '取得檔案清單', selecting: '等待選擇檔案', queued: '排隊中', downloading: '下載中', paused: '已暫停',
  importing: '匯入這一批', seeding: '做種中', completed: '完成', failed: '失敗', canceled: '已取消',
};
const itemStates = {
  pending: '等待中', uploading: '上傳中', published: '已入庫', duplicate: '已存在', skipped: '略過', failed: '失敗',
  excluded: '已排除', expanded: '已展開', split: '已分軌', discarded: '已捨棄',
};
const batchStates = { analyzing: '分析中', review: '等待確認', running: '進行中', done: '完成', canceled: '已取消' };
const batchKinds = { local: '伺服器資料夾', download: 'BT 下載', upload: '上傳', inbox: 'Drive 收件匣' };

const fmtWhen = (ms) => new Date(ms).toLocaleString('zh-TW', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' });

function usePoll(loader, ms, deps = []) {
  const [state, setState] = useState({ data: null, error: null });
  const load = useCallback(() => loader().then((data) => setState({ data, error: null }), (error) => setState((s) => ({ ...s, error }))), deps);
  useEffect(() => {
    load();
    const t = setInterval(() => document.visibilityState === 'visible' && load(), ms);
    return () => clearInterval(t);
  }, [load]);
  return { ...state, reload: load };
}

function Progress({ value }) {
  return html`<div class="progress" role="progressbar" aria-valuenow=${Math.round(value * 100)} aria-valuemin="0" aria-valuemax="100">
    <div style=${{ width: `${Math.min(value, 1) * 100}%` }}></div></div>`;
}

const finished = {
  downloads: (d) => d.state === 'completed' || d.state === 'canceled',
  imports: (b) => b.clearable,
};

// The task center lists every task under way or waiting for the user, and the finished ones a page
// at a time; a finished task's record can be removed (the songs, albums and files stay). The latest
// finished ones are kept up to date; older pages are read once, by ID, so every record can be
// reached (review #68). Once older ones are listed, the update covers every finished task from the
// first page's oldest on, and the older ones that changed since the page opened.
export function Tasks() {
  const [older, setOlder] = useState({ downloads: null, imports: null }); // { items, more, since }
  const changed = useRef(0);
  const query = () => {
    const p = new URLSearchParams();
    for (const k of ['downloads', 'imports']) if (older[k]) p.set('since_' + k, older[k].since);
    if (older.downloads || older.imports) p.set('changed', changed.current);
    return p.toString();
  };
  const tasks = usePoll(() => get('/tasks?' + query()).then((t) => {
    changed.current ||= t.now;
    return t;
  }), 2000, [older]);
  const [adding, setAdding] = useState(false);
  const [selecting, setSelecting] = useState(null);
  const [loadingMore, setLoadingMore] = useState('');
  const t = tasks.data;
  // What a kind shows: the kept-up-to-date tasks, then the older pages without the ones that came
  // back with the update (fetched again, finished again).
  const list = (k) => {
    if (!t) return [];
    const o = older[k];
    if (!o) return t[k];
    const fresh = new Set(t[k].map((x) => x.id));
    return [...t[k], ...o.items.filter((x) => !fresh.has(x.id))].sort((a, b) => b.id - a.id);
  };
  const hasMore = (k) => (older[k] ? older[k].more : t && t['more_' + k]);
  const loadMore = async (k) => {
    setLoadingMore(k);
    try {
      const o = older[k];
      const since = o ? o.since : Math.min(...t[k].filter(finished[k]).map((x) => x.id));
      const before = o && o.items.length ? o.items[o.items.length - 1].id : since;
      const r = await get(`/tasks/older?kind=${k}&before=${before}`);
      setOlder((v) => ({ ...v, [k]: { since, items: [...(v[k] ? v[k].items : []), ...r.items], more: r.more } }));
    } catch (e) {
      toast(e.message, 'error');
    }
    setLoadingMore('');
  };
  // An older task acted on (its record removed) leaves its page; when it is still there, it comes
  // back with the update.
  const changedOlder = (k, id) => () => {
    setOlder((v) => (v[k] ? { ...v, [k]: { ...v[k], items: v[k].items.filter((x) => x.id !== id) } } : v));
    tasks.reload();
  };
  const isOlder = (k, id) => older[k] && t && !t[k].some((x) => x.id === id);
  const downloads = list('downloads');
  const imports = list('imports');
  const clearable = t && (downloads.some((d) => d.clearable) || imports.some((b) => b.clearable) || hasMore('downloads') || hasMore('imports'));
  const clearAll = () => confirmDialog({
    title: '清除已結束的記錄', action: '清除',
    children: html`<p>從任務清單移除所有已結束的下載與匯入記錄。已入庫的歌曲、專輯和 Drive 上的檔案都不受影響；還在進行、做種，或還有檔案沒存進曲庫的任務會留著。</p>`,
    onConfirm: async () => {
      const r = await post('/tasks/clear');
      toast(`已移除 ${r.downloads} 個下載和 ${r.imports} 個匯入的記錄`);
      setOlder({ downloads: null, imports: null });
      tasks.reload();
    },
  });
  const more = (k) => hasMore(k) && html`<button class="btn text more-tasks" disabled=${loadingMore === k} onClick=${() => loadMore(k)}>
    ${loadingMore === k ? '載入中…' : '顯示更早的記錄'}</button>`;
  return html`<section>
    <div class="page-head">
      <h1 class="page-title">任務</h1>
      <div class="actions">
        <a class="btn tonal" href=${href('feeds')}><${Icon} name="queue" />RSS 訂閱</a>
        <button class="btn tonal" onClick=${() => setAdding(true)}><${Icon} name="download" />新增下載</button>
        <a class="btn tonal" href=${href('upload')}><${Icon} name="upload" />上傳音樂</a>
      </div>
    </div>
    <${ErrorBox} error=${tasks.error} onRetry=${tasks.reload} />
    ${tasks.data && tasks.data.disk && tasks.data.disk.low && html`<div class="error-box" role="alert"><span>
      磁碟空間不足（剩 ${fmtBytes(tasks.data.disk.free_bytes)}，需保留 ${fmtBytes(tasks.data.disk.reserve_bytes)}）：已清掉播放快取${tasks.data.disk.stopped ? '，並暫停下載、暫不接受新的下載與上傳' : ''}。空間恢復後會自動繼續，不會刪除還沒存進 Drive 的檔案。</span></div>`}
    ${!tasks.data && !tasks.error && html`<${Spinner} />`}
    ${t && html`
      ${clearable && html`<div class="task-tools"><button class="btn text" onClick=${clearAll}><${Icon} name="delete" />清除已結束的記錄</button></div>`}
      <h2 class="section-title">下載</h2>
      ${downloads.length ? downloads.map((d) => html`<${DownloadCard} key=${d.id} d=${d} onSelect=${() => setSelecting(d.id)}
          onChange=${isOlder('downloads', d.id) ? changedOlder('downloads', d.id) : tasks.reload} />`)
        : html`<${Empty} icon="download">沒有下載任務<//>`}
      ${more('downloads')}
      <h2 class="section-title">匯入</h2>
      ${imports.length ? imports.map((b) => html`<${ImportCard} key=${b.id} b=${b}
          onChange=${isOlder('imports', b.id) ? changedOlder('imports', b.id) : tasks.reload} />`)
        : html`<${Empty} icon="upload">沒有匯入紀錄<//>`}
      ${more('imports')}
    `}
    ${adding && html`<${AddDownload} onClose=${() => setAdding(false)} onAdded=${(id) => { setAdding(false); tasks.reload(); toast('已加入，正在取得檔案清單'); }} />`}
    ${selecting && html`<${SelectFiles} id=${selecting} onClose=${() => setSelecting(null)} onDone=${() => { setSelecting(null); tasks.reload(); }} />`}
  </section>`;
}

function DownloadCard({ d, onSelect, onChange }) {
  const act = (action) => post(`/downloads/${d.id}/${action}`).then(onChange, (e) => toast(e.message, 'error'));
  const progress = d.total_bytes ? d.done_bytes / d.total_bytes : 0;
  const active = d.state === 'downloading' || d.state === 'seeding';
  return html`<article class="task">
    <div class="task-head">
      <div class="grow">
        <div class="title">${d.name || d.source}</div>
        <div class="sub">
          <span class=${'chip state-' + d.state}>${downloadStates[d.state] || d.state}</span>
          ${d.total_bytes > 0 && html` ${fmtBytes(Math.min(d.done_bytes, d.total_bytes))} / ${fmtBytes(d.total_bytes)}`}
          ${active && html` · ↓${fmtBytes(d.down_speed)}/s ↑${fmtBytes(d.up_speed)}/s · ${d.peers} 連線`}
          ${d.import_batch_id ? ' · 已送入匯入' : ''}
        </div>
      </div>
      <div class="task-actions">
        ${d.state === 'selecting' && html`<button class="btn filled" onClick=${onSelect}>選擇檔案</button>`}
        ${(d.state === 'downloading' || d.state === 'queued' || d.state === 'seeding') && html`<${IconButton} icon="pause" label="暫停" onClick=${() => act('pause')} />`}
        ${d.state === 'paused' && html`<${IconButton} icon="play" label="繼續" onClick=${() => act('resume')} />`}
        ${d.can_retry && html`<button class="btn tonal" onClick=${() => act('retry')}>重試</button>`}
        ${!['completed', 'canceled'].includes(d.state) && (d.state !== 'failed' || !d.files_removed) && html`<${IconButton} icon="close"
          label=${d.state === 'seeding' ? '停止做種' : d.state === 'failed' ? '放棄並清除' : '取消'}
          onClick=${() => confirm(cancelPrompt(d)) && act('cancel')} />`}
        ${d.clearable && html`<${IconButton} icon="delete" label="移除記錄" onClick=${() => act('clear')} />`}
      </div>
    </div>
    ${(d.state === 'downloading' || d.state === 'paused' || d.state === 'queued' || d.state === 'importing') && html`<${Progress} value=${progress} />`}
    ${(d.rounds > 1 || d.left > 0) && !['selecting', 'metadata'].includes(d.state) && html`<div class="sub">
      分批下載：${d.state === 'importing' ? `第 ${d.round} 批已下載，匯入並清掉後再下載下一批` : d.round > 0 ? `第 ${d.round} 批` : '尚未開始'}${d.left > 0 && d.state !== 'importing' ? `，還有 ${d.left} 個檔案等下一批` : ''}</div>`}
    ${d.waiting_space && html`<div class="sub">等待暫存空間：其他下載、上傳或匯入釋出空間後自動開始。</div>`}
    ${d.error && html`<div class="task-error">${d.error}</div>`}
  </article>`;
}

// A failed download keeps what it fetched for a retry (review #49); giving it up clears what no
// import has. Songs already imported stay in the library.
const cancelPrompt = (d) => ({
  seeding: '停止做種？檔案已匯入，會在之後清除。',
  failed: '放棄這個下載？還沒匯入的檔案會被清除，已入庫的歌曲保留。',
}[d.state] || '取消這個下載？');

function AddDownload({ onClose, onAdded }) {
  const [uri, setUri] = useState('');
  const [file, setFile] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      const r = file ? await api('POST', '/downloads', file, { contentType: 'application/x-bittorrent' })
        : await post('/downloads', { uri: uri.trim() });
      onAdded(r.id);
    } catch (e) {
      setError(e);
      setBusy(false);
    }
  };
  return html`<${Dialog} title="新增下載" onClose=${onClose} actions=${html`
    <button class="btn text" onClick=${onClose}>取消</button>
    <button class="btn filled" disabled=${busy || (!uri.trim() && !file)} onClick=${submit}>${busy ? '加入中…' : '加入'}</button>`}>
    <label class="field"><span>magnet 連結或 .torrent 網址</span>
      <textarea rows="3" value=${uri} onInput=${(e) => setUri(e.target.value)} placeholder="magnet:?xt=urn:btih:… 或 https://nyaa.si/download/….torrent" disabled=${!!file}></textarea>
    </label>
    <label class="field"><span>或選擇 .torrent 檔</span>
      <input type="file" accept=".torrent,application/x-bittorrent" onChange=${(e) => setFile(e.target.files[0] || null)} />
    </label>
    <p class="hint">加入後會先取得檔案清單，選好要下載的檔案才開始下載。</p>
    <${ErrorBox} error=${error} />
  <//>`;
}

function SelectFiles({ id, onClose, onDone }) {
  const [d, setD] = useState(null);
  const [chosen, setChosen] = useState(new Set());
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    get('/downloads/' + id).then((v) => {
      setD(v);
      setChosen(new Set(v.files.filter((f) => f.suggested).map((f) => f.index)));
    }, setError);
  }, [id]);
  const toggle = (i) => setChosen((s) => {
    const n = new Set(s);
    n.has(i) ? n.delete(i) : n.add(i);
    return n;
  });
  const total = d ? d.files.filter((f) => chosen.has(f.index)).reduce((a, f) => a + f.length, 0) : 0;
  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      await post(`/downloads/${id}/select`, { files: [...chosen] });
      toast('開始下載');
      onDone();
    } catch (e) {
      setError(e);
      setBusy(false);
    }
  };
  const rounds = d && d.budget && total > d.budget;
  return html`<${Dialog} title="選擇要下載的檔案" wide onClose=${onClose} actions=${html`
    <span class="grow sub">已選 ${chosen.size} 個，${fmtBytes(total)}${rounds ? `，超過暫存空間 ${fmtBytes(d.budget)}，會分批下載` : ''}</span>
    <button class="btn text" onClick=${onClose}>取消</button>
    <button class="btn filled" disabled=${busy || !chosen.size} onClick=${submit}>${busy ? '處理中…' : '下載'}</button>`}>
    ${!d && !error && html`<${Spinner} />`}
    ${d && html`<div class="sub">${d.name}</div>
      ${rounds && html`<p class="hint">選取的總量超過伺服器的暫存空間，會自動分批：每批下載、匯入曲庫、清掉後再下載下一批，同一個資料夾的 CUE、LOG、封面會跟著它的音檔。不用減少選取。</p>`}
      <div class="file-tools">
        <button class="btn text" onClick=${() => setChosen(new Set(d.files.filter((f) => f.suggested).map((f) => f.index)))}>建議項目</button>
        <button class="btn text" onClick=${() => setChosen(new Set(d.files.map((f) => f.index)))}>全選</button>
        <button class="btn text" onClick=${() => setChosen(new Set())}>全不選</button>
      </div>
      <ul class="files">
        ${d.files.map((f) => html`<li key=${f.index}><label>
          <input type="checkbox" checked=${chosen.has(f.index)} onChange=${() => toggle(f.index)} />
          <span class="grow path">${f.path}</span><span class="sub">${fmtBytes(f.length)}</span>
        </label></li>`)}
      </ul>`}
    <${ErrorBox} error=${error} />
  <//>`;
}

// retried says what a retry did: files whose source is gone are not simply queued to fail again
// (review #57).
const retried = (r) => [
  r.requeued && `重新排入 ${r.requeued} 個檔案`,
  r.saved && `${r.saved} 個檔案的來源已不在，但同一個檔案已由其他批次存進曲庫，標為已存在`,
  r.fetching && `${r.fetching} 個檔案已不在伺服器上，正從下載重新取得，取得後自動匯入`,
  r.lost && `${r.lost} 個檔案已不在，也無法重新取得（原因見檔案），請重新匯入或捨棄`,
].filter(Boolean).join('；') || '沒有可以重試的檔案';

function ImportCard({ b, onChange }) {
  const [open, setOpen] = useState(false);
  const [detail, setDetail] = useState(null);
  useEffect(() => {
    if (!open) return;
    const load = () => get('/imports/' + b.id).then(setDetail, () => {});
    load();
    const t = b.state === 'running' && setInterval(load, 2000);
    return () => t && clearInterval(t);
  }, [open, b.state]);
  const c = b.counts;
  const total = Object.values(c).reduce((a, n) => a + n, 0);
  const done = total - (c.pending || 0) - (c.uploading || 0);
  const clear = () => post(`/imports/${b.id}/clear`).then(onChange, (e) => toast(e.message, 'error'));
  const retry = () => post(`/imports/${b.id}/retry`).then((r) => { toast(retried(r), r.lost ? 'error' : 'info'); onChange(); }, (e) => toast(e.message, 'error'));
  const kept = { upload: '原始檔留在伺服器的暫存空間', download: '下載的檔案會保留', inbox: '檔案留在 Drive 收件匣' }[b.kind] || '';
  const discard = () => confirmDialog({
    title: '捨棄未存進曲庫的檔案', action: '捨棄', danger: true,
    children: html`<p>這次匯入有 ${b.unsaved} 個檔案沒有存進曲庫（原因見下方各檔案）。捨棄後就不再重試，${b.kind === 'upload' ? '伺服器上的上傳暫存會刪除' : b.kind === 'download' ? '做種結束後下載的檔案會刪除' : '不會刪除任何檔案'}。</p>`,
    onConfirm: async () => {
      const r = await post(`/imports/${b.id}/discard`);
      toast(`已捨棄 ${r.discarded} 個檔案`);
      onChange();
    },
  });
  return html`<article class="task">
    <button class="task-head plain" onClick=${() => setOpen(!open)} aria-expanded=${open}>
      <div class="grow">
        <div class="title">${batchKinds[b.kind] || b.kind}${b.kind !== 'upload' && b.source ? '：' + b.source : ''}</div>
        <div class="sub">
          <span class=${'chip state-' + ({ done: 'completed', review: 'selecting', canceled: 'canceled' }[b.state] || 'downloading')}>${batchStates[b.state] || b.state}</span>
          <span class="count">${fmtWhen(b.created_at)}</span>
          ${Object.entries(c).map(([k, n]) => html` <span class=${'count state-' + k}>${itemStates[k] || k} ${n}</span>`)}
        </div>
      </div>
      <span class=${'chev' + (open ? ' open' : '')}><${Icon} name="expand" /></span>
    </button>
    ${b.state === 'running' && html`<${Progress} value=${total ? done / total : 0} />`}
    ${(b.state === 'review' || b.state === 'analyzing') && html`<div class="task-actions">
      <a class="btn filled" href=${href('import/' + b.id)}>${b.state === 'review' ? '確認並開始匯入' : '查看'}</a></div>`}
    ${b.unsaved > 0 && html`<div class="sub state-failed">${b.unsaved} 個檔案沒有存進曲庫${kept ? `；${kept}，等你重試或捨棄` : ''}。</div>`}
    ${(c.failed > 0 || b.unsaved > 0) && html`<div class="task-actions">
      ${c.failed > 0 && html`<button class="btn text" onClick=${retry}><${Icon} name="refresh" />重試失敗項目</button>`}
      ${b.unsaved > 0 && html`<button class="btn text danger-text" onClick=${discard}><${Icon} name="delete" />捨棄…</button>`}</div>`}
    ${b.clearable && html`<div class="task-actions"><button class="btn text" onClick=${clear}><${Icon} name="delete" />移除記錄</button></div>`}
    ${open && detail && html`<ul class="items">
      ${detail.items.map((it) => html`<li key=${it.id}>
        <span class="grow path">${it.path}</span>
        <span class=${'count state-' + it.state}>${itemStates[it.state] || it.state}${it.state === 'uploading' && it.total_bytes ? ` ${Math.round((it.sent_bytes / it.total_bytes) * 100)}%` : ''}</span>
        ${it.error && html`<div class="task-error">${it.error}</div>`}
      </li>`)}
    </ul>`}
  </article>`;
}

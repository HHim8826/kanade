import { useCallback, useEffect, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { href } from '../router.js';
import { Dialog, Empty, ErrorBox, Icon, IconButton, Spinner, fmtBytes, html, toast } from '../ui.js';

const downloadStates = {
  metadata: '取得檔案清單', selecting: '等待選擇檔案', queued: '排隊中', downloading: '下載中', paused: '已暫停',
  seeding: '做種中', completed: '完成', failed: '失敗', canceled: '已取消',
};
const itemStates = {
  pending: '等待中', uploading: '上傳中', published: '已入庫', duplicate: '已存在', skipped: '略過', failed: '失敗',
  excluded: '已排除', expanded: '已展開', split: '已分軌',
};
const batchStates = { analyzing: '分析中', review: '等待確認', running: '進行中', done: '完成', canceled: '已取消' };
const batchKinds = { local: '伺服器資料夾', download: 'BT 下載', upload: '上傳', inbox: 'Drive 收件匣' };

const fmtWhen = (ms) => new Date(ms).toLocaleString('zh-TW', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' });

function usePoll(loader, ms) {
  const [state, setState] = useState({ data: null, error: null });
  const load = useCallback(() => loader().then((data) => setState({ data, error: null }), (error) => setState((s) => ({ ...s, error }))), []);
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

export function Tasks() {
  const tasks = usePoll(() => get('/tasks'), 2000);
  const [adding, setAdding] = useState(false);
  const [selecting, setSelecting] = useState(null);
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
    ${tasks.data && html`
      <h2 class="section-title">下載</h2>
      ${tasks.data.downloads.length ? tasks.data.downloads.map((d) => html`<${DownloadCard} key=${d.id} d=${d} onSelect=${() => setSelecting(d.id)} onChange=${tasks.reload} />`)
        : html`<${Empty} icon="download">沒有下載任務<//>`}
      <h2 class="section-title">匯入</h2>
      ${tasks.data.imports.length ? tasks.data.imports.map((b) => html`<${ImportCard} key=${b.id} b=${b} onChange=${tasks.reload} />`)
        : html`<${Empty} icon="upload">沒有匯入紀錄<//>`}
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
        ${!['completed', 'failed', 'canceled'].includes(d.state) && html`<${IconButton} icon="close" label=${d.state === 'seeding' ? '停止做種' : '取消'}
          onClick=${() => confirm(d.state === 'seeding' ? '停止做種？檔案已匯入，會在之後清除。' : '取消這個下載？') && act('cancel')} />`}
      </div>
    </div>
    ${(d.state === 'downloading' || d.state === 'paused' || d.state === 'queued') && html`<${Progress} value=${progress} />`}
    ${d.error && html`<div class="task-error">${d.error}</div>`}
  </article>`;
}

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
  return html`<${Dialog} title="選擇要下載的檔案" onClose=${onClose} actions=${html`
    <span class="grow sub">已選 ${chosen.size} 個，${fmtBytes(total)}</span>
    <button class="btn text" onClick=${onClose}>取消</button>
    <button class="btn filled" disabled=${busy || !chosen.size} onClick=${submit}>${busy ? '處理中…' : '下載'}</button>`}>
    ${!d && !error && html`<${Spinner} />`}
    ${d && html`<div class="sub">${d.name}</div>
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
  const retry = () => post(`/imports/${b.id}/retry`).then((r) => { toast(`重新排入 ${r.requeued} 個檔案`); onChange(); }, (e) => toast(e.message, 'error'));
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
    ${c.failed > 0 && html`<div class="task-actions"><button class="btn text" onClick=${retry}><${Icon} name="refresh" />重試失敗項目</button></div>`}
    ${open && detail && html`<ul class="items">
      ${detail.items.map((it) => html`<li key=${it.id}>
        <span class="grow path">${it.path}</span>
        <span class=${'count state-' + it.state}>${itemStates[it.state] || it.state}${it.state === 'uploading' && it.total_bytes ? ` ${Math.round((it.sent_bytes / it.total_bytes) * 100)}%` : ''}</span>
        ${it.error && html`<div class="task-error">${it.error}</div>`}
      </li>`)}
    </ul>`}
  </article>`;
}

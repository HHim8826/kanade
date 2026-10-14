import { useEffect, useState } from '../../vendor/hooks.module.js';
import { get, post } from '../api.js';
import { go } from '../router.js';
import { Dialog, ErrorBox, Icon, Spinner, fmtBytes, html, showDialog, toast, useLoad } from '../ui.js';
import { useRunner } from './organize.js';

// ---- moving the library's files to another storage location ----
// Every file of a location (to empty it before it is removed, or to change where the library
// lives), or those of chosen albums, moved to Google Drive or a managed location: each file is
// copied there and checked before the library reads it from there, and the old copy goes to its
// location's trash a while later. The task page follows a move, and can stop it or try again.

const kinds = { local: '資料夾', s3: 'R2／S3', onedrive: 'OneDrive' };
const fileKinds = { song: '歌曲', cover: '封面', sidecar: 'CUE／LOG', image: '內頁圖' };
const moveStates = { running: '搬移中', waiting: '等待重試', done: '完成', canceled: '已取消' };
const reasons = {
  dest_offline: '目的地目前連不上',
  dest_auth: '目的地不接受目前的登入或金鑰：Google Drive 或 OneDrive 請重新連線，R2 請更新金鑰',
  dest_full: '目的地的空間不夠',
  dest_gone: '目的地已移除或改成只讀來源，搬移停止了',
  origin_offline: '檔案所在的位置目前連不上',
  origin_auth: '檔案所在的位置不接受目前的登入或金鑰',
  staging: '伺服器的暫存空間不夠（雲端之間搬移時，檔案要先經過伺服器）',
  error: '發生錯誤',
};
const failures = {
  gone: '原位置找不到這個檔案',
  changed: '檔案內容和匯入時不同（SHA-256 不符），留在原處',
  error: '試了三次都失敗，留在原處',
};
const fmtWhen = (ms) => new Date(ms).toLocaleString('zh-TW', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' });

// moveFiles asks where to move every file of a location (origin: "drive" or a store's ID), or those
// of albums (ids), shows what it would do, and starts it.
export function moveFiles({ origin, albums, title, onDone }) {
  showDialog((close) => html`<${MoveFiles} origin=${origin} albums=${albums} title=${title} close=${close} onDone=${onDone} />`);
}

function MoveFiles({ origin, albums, title, close, onDone }) {
  const places = useLoad(() => get('/storage'), []);
  const [dest, setDest] = useState('');
  const [plan, setPlan] = useState({ key: null });
  const [busy, run] = useRunner();
  const v = places.data;
  const choices = v ? [{ id: 'drive', name: 'Google Drive', sub: v.drive && v.drive.connected ? '' : '尚未連線', off: !(v.drive && v.drive.connected) },
    ...v.stores.filter((s) => s.mode === 'managed').map((s) => ({ id: String(s.id), name: s.name,
      sub: [kinds[s.kind], s.state && s.state !== 'ok' ? '目前無法使用' : ''].filter(Boolean).join(' · ') }))].filter((c) => c.id !== origin) : [];
  const usable = choices.filter((c) => !c.off);
  const chosen = dest || (v && ((usable.find((c) => c.id === v.default) || usable[0] || {}).id)) || '';
  const body = origin ? { dest: chosen, origin } : { dest: chosen, albums };
  const key = JSON.stringify(body);
  useEffect(() => {
    if (!chosen) return undefined;
    let alive = true;
    post('/moves/preview', body).then((p) => alive && setPlan({ key, p }), (error) => alive && setPlan({ key, error }));
    return () => { alive = false; };
  }, [key]);
  const p = plan.key === key ? plan.p : null;
  const start = () => run(async () => {
    await post('/moves', body);
    close();
    toast('開始搬移，可以在「任務」查看進度', 'info', { label: '前往任務', onClick: () => go('tasks') });
    onDone && onDone();
  });
  const what = origin ? `「${title}」的所有檔案` : albums.length > 1 ? `${albums.length} 張專輯的檔案` : `「${title}」的檔案`;
  const others = p ? p.files - p.songs : 0;
  return html`<${Dialog} title="搬移檔案" wide onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !p || !p.files} onClick=${start}>開始搬移</button>`}>
    <p>把${what}搬到：</p>
    ${places.error && html`<${ErrorBox} error=${places.error} onRetry=${places.reload} />`}
    ${!v && !places.error && html`<${Spinner} />`}
    ${v && !choices.length && html`<p class="hint">沒有其他可以存放的位置：先在設定的「儲存位置」新增一個存放位置，或連線 Google Drive。</p>`}
    ${choices.length > 0 && html`<div class="choice-list" role="radiogroup" aria-label="搬到">
      ${choices.map((c) => html`<label key=${c.id} class=${(chosen === c.id ? 'on' : '') + (c.off ? ' disabled' : '')}>
        <input type="radio" checked=${chosen === c.id} disabled=${c.off} onChange=${() => setDest(c.id)} />
        <span><b>${c.name}</b>${c.sub && html`<span class="sub">${c.sub}</span>`}</span></label>`)}
    </div>`}
    ${plan.key === key && plan.error && html`<${ErrorBox} error=${plan.error} />`}
    ${chosen && !p && !plan.error && html`<${Spinner} />`}
    ${p && html`<div class="merge-plan">
      <p>${p.files
        ? html`<b>${p.songs ? `${p.songs} 首歌` : ''}${p.songs && others ? '和 ' : ''}${others ? `${others} 個封面、CUE／LOG 或內頁圖` : ''}</b>會搬到「${p.dest_name}」${p.bytes ? `（約 ${fmtBytes(p.bytes)}）` : ''}。`
        : `沒有要搬的檔案。`}
        ${p.here ? `${p.here} 個已經在「${p.dest_name}」。` : ''}${p.fixed ? `${p.fixed} 個在只讀來源裡，留在原處。` : ''}</p>
      ${p.missing > 0 && html`<p class="state-failed">另有 ${p.missing} 首遺失的歌，檔案已不在這裡，沒辦法搬；要刪除或找回它們之後，才能移除這個位置。</p>`}
      ${p.default && html`<p class="state-failed">「${title}」仍是新匯入的位置：之後匯入的音樂還是會存到這裡。要清空它的話，先在「儲存位置」最上方把新匯入的位置改到別處。</p>`}
      ${p.files > 0 && html`<p class="hint tight">每個檔案複製到新位置並核對 SHA-256 之後，曲庫才改從新位置讀取，播放中的歌不受影響；原本的檔案 15 分鐘後移到它所在位置的垃圾桶。在雲端之間搬移時，檔案會先經過伺服器的暫存空間。目的地暫時連不上或空間不夠時會等一下再試，可以在「任務」查看進度或取消。</p>`}
    </div>`}
  <//>`;
}

// MoveCard is a move on the task page.
export function MoveCard({ v, onChange }) {
  const [open, setOpen] = useState(false);
  const [detail, setDetail] = useState(null);
  const [busy, run] = useRunner();
  useEffect(() => {
    if (!open || !v.failed) return;
    get('/moves/' + v.id).then(setDetail, () => {});
  }, [open, v.failed]);
  const act = (what, said) => run(async () => {
    await post(`/moves/${v.id}/${what}`);
    if (said) toast(said);
    onChange();
  });
  const over = v.state === 'done' || v.state === 'canceled';
  const left = v.files - v.moved - v.skipped - v.failed;
  const where = v.dest_name ? `「${v.dest_name}」` : '已移除的位置';
  const what = v.origin ? `「${v.title}」的所有檔案` : v.albums && v.albums.length > 1 ? `「${v.title}」等 ${v.albums.length} 張專輯` : `「${v.title}」`;
  const c = v.current;
  const sent = v.moved_bytes + (c ? c.sent : 0);
  // Only a move with files it could not move opens: to list them.
  const Head = v.failed ? 'button' : 'div';
  return html`<article class="task">
    <${Head} class="task-head plain" onClick=${v.failed ? () => setOpen(!open) : undefined} aria-expanded=${v.failed ? open : undefined}>
      <div class="grow">
        <div class="title">搬移${what}到${where}</div>
        <div class="sub">
          <span class=${'chip state-' + ({ done: 'completed', canceled: 'canceled', waiting: 'paused' }[v.state] || 'downloading')}>${moveStates[v.state] || v.state}</span>
          <span class="count">${fmtWhen(v.created_at)}</span>
          <span class="count state-published">已搬 ${v.moved}／${v.files}</span>
          ${v.bytes > 0 && html` <span class="count">${fmtBytes(v.moved_bytes)}／${fmtBytes(v.bytes)}</span>`}
          ${v.skipped > 0 && html` <span class="count state-skipped">略過 ${v.skipped}</span>`}
          ${v.failed > 0 && html` <span class="count state-failed">失敗 ${v.failed}</span>`}
        </div>
      </div>
      ${v.failed > 0 && html`<span class=${'chev' + (open ? ' open' : '')}><${Icon} name="expand" /></span>`}
    <//>
    ${!over && html`<div class="progress" role="progressbar" aria-valuenow=${Math.round((v.bytes ? sent / v.bytes : (v.files - left) / v.files) * 100)} aria-valuemin="0" aria-valuemax="100">
      <div style=${{ width: `${Math.min(v.bytes ? sent / v.bytes : (v.files - left) / v.files, 1) * 100}%` }}></div></div>`}
    ${v.state === 'running' && c && html`<div class="sub">正在搬：${fileKinds[c.kind] || ''} ${c.name}${c.total ? `（${Math.round((c.sent / c.total) * 100)}%）` : ''}</div>`}
    ${v.reason && html`<div class="task-error">${reasons[v.reason] || reasons.error}${v.error ? `：${v.error}` : ''}${v.state === 'waiting' && v.next_at ? `。${fmtWhen(v.next_at)} 自動再試。` : ''}</div>`}
    ${v.state === 'canceled' && left > 0 && !v.reason && html`<div class="sub">還有 ${left} 個檔案沒有搬，留在原處。</div>`}
    ${v.skipped > 0 && html`<div class="sub">略過的是搬移開始後已從曲庫移除或已被搬走的檔案。</div>`}
    <div class="task-actions">
      ${v.state === 'waiting' && html`<button class="btn text" disabled=${busy} onClick=${() => act('retry', '立即再試')}><${Icon} name="refresh" />立即再試</button>`}
      ${over && (v.failed > 0 || left > 0) && html`<button class="btn text" disabled=${busy} onClick=${() => act('retry', v.failed ? '重試失敗和還沒搬的檔案' : '繼續搬移')}><${Icon} name="refresh" />${v.failed ? '重試失敗項目' : '繼續搬移'}</button>`}
      ${!over && html`<button class="btn text danger-text" disabled=${busy} onClick=${() => act('cancel', '已取消搬移；已搬好的檔案留在新位置')}>取消搬移</button>`}
      ${over && html`<button class="btn text" disabled=${busy} onClick=${() => act('clear')}><${Icon} name="delete" />移除記錄</button>`}
    </div>
    ${open && detail && html`<ul class="items">
      ${(detail.failures || []).map((f) => html`<li key=${f.ref}>
        <span class="grow path">${fileKinds[f.kind] || f.kind}：${f.name}</span>
        <span class="count state-failed">失敗</span>
        <div class="task-error">${failures[f.reason] || failures.error}${f.error ? `（${f.error}）` : ''}</div>
      </li>`)}
    </ul>`}
  </article>`;
}

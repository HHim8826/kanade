import { useState } from '../../vendor/hooks.module.js';
import { ApiError, api, get, post } from '../api.js';
import { go } from '../router.js';
import { ErrorBox, FilePick, Icon, fmtBytes, html, toast, useLoad } from '../ui.js';
import { confirmDialog } from './organize.js';

const AUDIO = /\.(flac|mp3|m4a|mp4|aac|ogg|oga|opus|wav|aiff?|ape|tak|wv|tta|dsf|dff|wma)$/i;
const IMAGE = /\.(jpe?g|png)$/i;
const SIDECAR = /\.(lrc|cue|log)$/i; // lyrics; CUE sheets and rip logs are kept with the album
const ARCHIVE = /\.zip$/i; // expanded on the server, after its limits are checked

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Sends one file in chunks, resuming from whatever the server already has (decision D4).
async function sendFile(group, entry, onProgress) {
  const { upload: up, chunk_size: chunk } = await post('/uploads', { group, path: entry.path, size: entry.file.size });
  if (up.state !== 'receiving') return;
  let offset = up.received;
  for (let attempt = 0; offset < entry.file.size;) {
    const end = Math.min(offset + chunk, entry.file.size);
    try {
      const r = await api('PUT', `/uploads/${up.id}?offset=${offset}`, entry.file.slice(offset, end), { contentType: 'application/octet-stream' });
      offset = r.received;
      attempt = 0;
      onProgress(offset);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409 && e.body && typeof e.body.received === 'number') {
        offset = e.body.received; // the server has a different count: continue from there
        continue;
      }
      if (attempt++ >= 5 || (e instanceof ApiError && e.status < 500 && e.status !== 409)) throw e;
      await sleep(1000 * 2 ** attempt); // network hiccup: back off and retry the chunk
    }
  }
  await post(`/uploads/${up.id}/complete`);
}

// The same selection keeps its upload group across retries and reloads (review #6): choosing the
// same files again continues where they stopped instead of reserving the space a second time.
const GROUPS = 'kanade.uploadGroups';
const selectionKey = (list) => list.map((e) => `${e.path}\u0000${e.file.size}\u0000${e.file.lastModified}`).sort().join('\n');
function groupFor(key) {
  try {
    const map = JSON.parse(localStorage.getItem(GROUPS) || '{}');
    if (map[key]) return map[key].group;
    const group = 'web-' + crypto.randomUUID();
    map[key] = { group, at: Date.now() };
    for (const [k, v] of Object.entries(map)) if (Date.now() - v.at > 7 * 86400000) delete map[k]; // the server forgets them too
    localStorage.setItem(GROUPS, JSON.stringify(map));
    return group;
  } catch {
    return 'web-' + crypto.randomUUID();
  }
}
function forgetGroup(key) {
  try {
    const map = JSON.parse(localStorage.getItem(GROUPS) || '{}');
    delete map[key];
    localStorage.setItem(GROUPS, JSON.stringify(map));
  } catch { /* nothing kept */ }
}

// Unfinished lists selections the server still holds: being sent, or sent and not imported.
function Unfinished({ rev, onChange }) {
  const list = useLoad(() => get('/uploads'), [rev]);
  const groups = (list.data || []).filter((g) => !g.importing);
  if (!groups.length) return null;
  const cancel = (g) => confirmDialog({
    title: '取消這次上傳', action: '取消上傳', danger: true,
    children: html`<p>刪除伺服器上這次上傳已收到的 ${fmtBytes(g.received)}，釋出它預留的暫存空間。檔案還在你的電腦上，之後可以重新上傳。</p>`,
    onConfirm: async () => {
      await api('DELETE', '/uploads/groups/' + encodeURIComponent(g.group));
      toast('已取消上傳');
      onChange();
    },
  });
  return html`<h2 class="section-title">未完成的上傳</h2>
    <p class="hint">重新選擇同樣的檔案或資料夾並按「開始上傳」，會從中斷的地方繼續。一週沒有動靜的上傳會自動清除。</p>
    <ul class="items">${groups.map((g) => html`<li key=${g.group}>
      <span class="grow"><span class="title">${g.files} 個檔案，已收到 ${fmtBytes(g.received)} / ${fmtBytes(g.size)}</span>
        <span class="sub">${g.complete === g.files ? '已傳完，尚未匯入' : '上傳中斷'} · ${new Date(g.updated_at).toLocaleString('zh-TW')}</span></span>
      <button class="btn text danger-text" onClick=${() => cancel(g)}>取消上傳</button>
    </li>`)}</ul>`;
}

export function Upload() {
  const [entries, setEntries] = useState([]);
  const [progress, setProgress] = useState(null); // { sent, total, file }
  const [error, setError] = useState(null);
  const [rev, setRev] = useState(0);

  const pick = (e) => {
    const files = [...e.target.files];
    const folder = files.some((f) => f.webkitRelativePath);
    // From a folder, keep cover and scan images too: the server picks album art from them.
    const list = files
      .filter((f) => AUDIO.test(f.name) || SIDECAR.test(f.name) || ARCHIVE.test(f.name) || (folder && IMAGE.test(f.name)))
      .map((f) => ({ file: f, path: f.webkitRelativePath || f.name }));
    setEntries(list);
    setError(null);
    e.target.value = '';
  };

  const audioCount = entries.filter((e) => AUDIO.test(e.path)).length;
  const zipCount = entries.filter((e) => ARCHIVE.test(e.path)).length;
  const imageCount = entries.filter((e) => IMAGE.test(e.path)).length;
  const otherCount = entries.length - audioCount - zipCount - imageCount;
  const totalBytes = entries.reduce((a, e) => a + e.file.size, 0);

  const start = async () => {
    const key = selectionKey(entries);
    const group = groupFor(key);
    let sent = 0;
    setError(null);
    try {
      for (const [index, entry] of entries.entries()) {
        const base = sent;
        setProgress({ sent, total: totalBytes, index, fileSent: 0 });
        await sendFile(group, entry, (n) => setProgress({ sent: base + n, total: totalBytes, index, fileSent: n }));
        sent = base + entry.file.size;
      }
      setProgress({ sent: totalBytes, total: totalBytes, index: entries.length, importing: true });
      const r = await post('/imports', { upload_group: group, preview: true });
      forgetGroup(key);
      toast('已上傳，請確認要怎麼匯入');
      go('import/' + r.id);
    } catch (e) {
      setError(e);
      setProgress(null);
      setRev((n) => n + 1);
    }
  };

  const busy = progress !== null;
  return html`<section>
    <h1 class="page-title">上傳音樂</h1>
    <p class="hint">選擇資料夾時會保留資料夾結構，Disc 子資料夾、封面與掃描圖都會用來整理專輯。也可以上傳 ZIP 壓縮檔。上傳後會先列出要怎麼分成專輯，確認了才開始匯入。</p>
    <div class="actions">
      <${FilePick} label="選擇檔案" icon="note" multiple disabled=${busy} onPick=${pick}
        accept="audio/*,.flac,.ape,.tak,.wv,.opus,.lrc,.cue,.log,.zip,application/zip" />
      <${FilePick} label="選擇資料夾" icon="album" directory disabled=${busy} onPick=${pick} />
    </div>
    ${entries.length > 0 && html`<div class="card pad">
      <div class="title">${[audioCount && `${audioCount} 首音樂`, zipCount && `${zipCount} 個壓縮檔`, imageCount && `${imageCount} 張圖片`, otherCount && `${otherCount} 個歌詞或附屬檔`].filter(Boolean).join('、')}，共 ${fmtBytes(totalBytes)}</div>
      <ul class="items compact">${entries.slice(0, 50).map((e, i) => html`<li key=${e.path} class=${busy && i === progress.index ? 'current' : ''}>
        <span class="grow path">${e.path}</span><span class="sub">${!busy ? fmtBytes(e.file.size) : i < progress.index ? '已上傳'
          : i === progress.index ? `上傳中 ${fmtBytes(progress.fileSent)} / ${fmtBytes(e.file.size)}` : `等待中 · ${fmtBytes(e.file.size)}`}</span></li>`)}</ul>
      ${entries.length > 50 && html`<div class="sub">…還有 ${entries.length - 50} 個檔案</div>`}
      ${busy
        ? html`<div class="upload-progress">
            <div class="progress"><div style=${{ width: `${(progress.sent / progress.total) * 100}%` }}></div></div>
            ${(progress.importing || entries.length > 1) && html`<div class="sub">${progress.importing ? '已上傳，正在建立匯入…'
              : `第 ${progress.index + 1} / ${entries.length} 個檔案 · 共 ${fmtBytes(progress.sent)} / ${fmtBytes(progress.total)}`}</div>`}</div>`
        : html`<div class="actions"><button class="btn filled" disabled=${!audioCount && !zipCount} onClick=${start}><${Icon} name="upload" />${error ? '繼續上傳' : '開始上傳'}</button></div>`}
    </div>`}
    <${ErrorBox} error=${error} />
    <${Unfinished} rev=${rev} onChange=${() => setRev((n) => n + 1)} />
  </section>`;
}

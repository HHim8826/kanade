import { useState } from '../../vendor/hooks.module.js';
import { ApiError, api, post } from '../api.js';
import { go } from '../router.js';
import { ErrorBox, Icon, fmtBytes, html, toast } from '../ui.js';

const AUDIO = /\.(flac|mp3|m4a|mp4|aac|ogg|oga|opus|wav|aiff?|ape|tak|wv|tta|dsf|dff|wma)$/i;
const IMAGE = /\.(jpe?g|png)$/i;
const SIDECAR = /\.(lrc|cue|log)$/i; // lyrics; CUE sheets and rip logs are kept with the album

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

export function Upload() {
  const [entries, setEntries] = useState([]);
  const [progress, setProgress] = useState(null); // { sent, total, file }
  const [error, setError] = useState(null);

  const pick = (e) => {
    const files = [...e.target.files];
    const folder = files.some((f) => f.webkitRelativePath);
    // From a folder, keep cover and scan images too: the server picks album art from them.
    const list = files
      .filter((f) => AUDIO.test(f.name) || SIDECAR.test(f.name) || (folder && IMAGE.test(f.name)))
      .map((f) => ({ file: f, path: f.webkitRelativePath || f.name }));
    setEntries(list);
    setError(null);
    e.target.value = '';
  };

  const audioCount = entries.filter((e) => AUDIO.test(e.path)).length;
  const imageCount = entries.filter((e) => IMAGE.test(e.path)).length;
  const otherCount = entries.length - audioCount - imageCount;
  const totalBytes = entries.reduce((a, e) => a + e.file.size, 0);

  const start = async () => {
    const group = 'web-' + crypto.randomUUID();
    let sent = 0;
    setError(null);
    try {
      for (const entry of entries) {
        const base = sent;
        setProgress({ sent, total: totalBytes, file: entry.path });
        await sendFile(group, entry, (n) => setProgress({ sent: base + n, total: totalBytes, file: entry.path }));
        sent = base + entry.file.size;
      }
      setProgress({ sent: totalBytes, total: totalBytes, file: '建立匯入…' });
      const r = await post('/imports', { upload_group: group });
      toast(`已上傳，開始匯入 ${r.files} 首`);
      go('tasks');
    } catch (e) {
      setError(e);
      setProgress(null);
    }
  };

  const busy = progress !== null;
  return html`<section>
    <h1 class="page-title">上傳音樂</h1>
    <p class="hint">選擇資料夾時會保留資料夾結構，Disc 子資料夾、封面與掃描圖都會用來整理專輯。</p>
    <div class="actions">
      <label class=${'btn tonal' + (busy ? ' disabled' : '')}><${Icon} name="note" />選擇檔案
        <input type="file" multiple hidden disabled=${busy} accept="audio/*,.flac,.ape,.tak,.wv,.opus,.lrc,.cue,.log" onChange=${pick} /></label>
      <label class=${'btn tonal' + (busy ? ' disabled' : '')}><${Icon} name="album" />選擇資料夾
        <input type="file" hidden disabled=${busy} webkitdirectory onChange=${pick} /></label>
    </div>
    ${entries.length > 0 && html`<div class="card pad">
      <div class="title">${[`${audioCount} 首音樂`, imageCount && `${imageCount} 張圖片`, otherCount && `${otherCount} 個歌詞或附屬檔`].filter(Boolean).join('、')}，共 ${fmtBytes(totalBytes)}</div>
      <ul class="items compact">${entries.slice(0, 50).map((e) => html`<li key=${e.path}><span class="grow path">${e.path}</span><span class="sub">${fmtBytes(e.file.size)}</span></li>`)}</ul>
      ${entries.length > 50 && html`<div class="sub">…還有 ${entries.length - 50} 個檔案</div>`}
      ${busy
        ? html`<div class="upload-progress"><div class="sub">${progress.file}</div>
            <div class="progress"><div style=${{ width: `${(progress.sent / progress.total) * 100}%` }}></div></div>
            <div class="sub">${fmtBytes(progress.sent)} / ${fmtBytes(progress.total)}</div></div>`
        : html`<div class="actions"><button class="btn filled" disabled=${!audioCount} onClick=${start}><${Icon} name="upload" />開始上傳</button></div>`}
    </div>`}
    <${ErrorBox} error=${error} />
  </section>`;
}

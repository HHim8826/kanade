import { useState } from '../../vendor/hooks.module.js';
import { api, get } from '../api.js';
import { trashName } from '../places.js';
import { Dialog, ErrorBox, Icon, IconButton, Spinner, html, openMenu, showDialog, toast, useLoad } from '../ui.js';
import { DragHandle, useReorder } from './common.js';
import { PictureViewer } from './coverview.js';
import { changed, done, useLibRev, useRunner } from './organize.js';

// ---- an album's gallery (#134) ----
// The pictures that came with an album (scans of its cover, back, booklet, disc, obi) or were
// uploaded to it: a row of them on the album's page, looked at one after another in the viewer,
// told what each is, put in order, made the cover, downloaded as they were kept, taken out. The
// viewer shows a TIFF or BMP, or a scan too large to show as it is, by a JPEG made of it.

export const imageURL = (id, size) => `/api/v1/images/${id}?size=${size}`;
export const KINDS = { front: '封面', back: '封底', booklet: '內頁', disc: '光碟', obi: '側標', other: '其他' };
const ACCEPT = 'image/jpeg,image/png,image/webp,image/tiff,image/bmp,.jpg,.jpeg,.png,.webp,.tif,.tiff,.bmp';
const MAX = 128 << 20;

const discName = (album, d) => (album.sections && album.sections[d]) || `Disc ${d}`;
const caption = (album, m) => [m.disc ? discName(album, m.disc) : '', KINDS[m.kind] || ''].filter(Boolean).join(' · ');

// AlbumGallery is the row of an album's pictures on its page.
export function AlbumGallery({ album }) {
  const rev = useLibRev();
  const list = useLoad(() => (album.images ? get(`/albums/${album.id}/images`) : Promise.resolve({ images: [] })), [album.id, album.images], rev);
  if (!album.images) return null;
  const images = list.data ? list.data.images : [];
  return html`<div class="section-head">
      <h2 class="section-title">內頁圖<span class="count">${album.images} 張</span></h2>
      <div class="actions tight">
        <button class="btn text" onClick=${() => uploadImages(album)}><${Icon} name="upload" />上傳</button>
        <button class="btn text" disabled=${!images.length} onClick=${() => arrangeImages(album, images)}><${Icon} name="order" />整理</button>
      </div>
    </div>
    ${list.error ? html`<${ErrorBox} error=${list.error} onRetry=${list.reload} />`
      : !list.data ? html`<${Spinner} />`
      : html`<div class="pic-row">${images.map((m, i) => html`<button key=${m.id} class="pic-thumb" title=${m.name}
          onClick=${() => viewGallery(album, images, i)}>
          <img src=${imageURL(m.id, 256)} width=${m.width} height=${m.height} alt=${m.name} loading="lazy" />
          <span class="sub">${caption(album, m) || m.name}</span></button>`)}</div>`}`;
}

// openGallery looks at an album's gallery from its first picture.
export async function openGallery(album) {
  try {
    const { images } = await get(`/albums/${album.id}/images`);
    if (images.length) viewGallery(album, images, 0);
  } catch (e) {
    toast(e.message, 'error');
  }
}

export function viewGallery(album, images, start) {
  showDialog((close) => html`<${GalleryViewer} album=${album} images=${images} start=${start} close=${close} />`);
}

function GalleryViewer({ album, images, start, close }) {
  const [list, setList] = useState(images);
  const [asking, setAsking] = useState(null); // the picture to take out, asked about
  const [busy, run] = useRunner();
  const items = list.map((m) => ({ key: m.id, small: imageURL(m.id, 256), full: imageURL(m.id, 'view'), alt: m.name,
    info: [caption(album, m), `${m.width} × ${m.height}`].filter(Boolean).join(' · ') }));
  const more = (e, item, i) => {
    const m = list[i];
    openMenu(e, [
      { icon: 'image', label: '設為封面', onClick: () => api('POST', `/images/${m.id}/cover`).then((r) => done(r, '已設為封面'), (err) => toast(err.message, 'error')) },
      { icon: 'download', label: '下載原檔', onClick: () => download(m) },
      { icon: 'order', label: '整理內頁圖…', onClick: () => arrangeImages(album, list) },
      { icon: 'delete', label: '從內頁圖移除…', onClick: () => setAsking(m) },
    ]);
  };
  const remove = () => run(async () => {
    const m = asking;
    await api('DELETE', `/images/${m.id}`);
    setAsking(null);
    changed();
    toast(`已移除「${m.name}」`);
    if (list.length === 1) close();
    setList((l) => l.filter((x) => x.id !== m.id));
  });
  const notice = asking && html`<div class="cover-notice" role="alert">
    <span class="grow">從內頁圖移除「${asking.name}」？檔案會移到${trashName()}（來源裡的檔案留在原處），之後再上傳同一張圖可以放回來。</span>
    <button class="btn text" onClick=${() => setAsking(null)}>取消</button>
    <button class="btn filled danger" disabled=${busy} onClick=${remove}>移除</button>
  </div>`;
  if (!list.length) return null;
  return html`<${PictureViewer} items=${items} start=${start} title=${album.title} label="內頁圖" more=${more} notice=${notice} close=${close} />`;
}

function download(m) {
  const a = document.createElement('a');
  a.href = `/api/v1/images/${m.id}/file`;
  a.download = m.name;
  document.body.append(a);
  a.click();
  a.remove();
}

// uploadImages adds pictures chosen from this device to an album's gallery, one after another.
export function uploadImages(album) {
  const input = document.createElement('input');
  input.type = 'file';
  input.multiple = true;
  input.accept = ACCEPT;
  input.onchange = () => input.files.length && sendImages(album, [...input.files]);
  input.click();
}

async function sendImages(album, files) {
  let added = 0, had = 0;
  const failed = [];
  toast(files.length > 1 ? `上傳 ${files.length} 張內頁圖中…` : '上傳內頁圖中…');
  for (const f of files) {
    if (f.size > MAX) {
      failed.push(`「${f.name}」超過 128 MB`);
      continue;
    }
    try {
      const r = await api('POST', `/albums/${album.id}/images?name=${encodeURIComponent(f.name)}`, f, { contentType: f.type || 'application/octet-stream' });
      if (r.added) added++;
      else had++;
    } catch (e) {
      failed.push(`「${f.name}」${e.status === 400 ? '不是 JPEG、PNG、WebP、TIFF 或 BMP 圖片' : '：' + e.message}`);
    }
  }
  if (added) changed();
  const said = [added && `已加入 ${added} 張內頁圖`, had && `${had} 張已經在這張專輯裡`].filter(Boolean).join('，');
  if (said) toast(said);
  if (failed.length) toast(`${failed.length} 張沒有加入：${failed.slice(0, 3).join('；')}${failed.length > 3 ? '…' : ''}`, 'error');
}

// arrangeImages tells what each picture is and the disc it goes with, and puts them in order.
export function arrangeImages(album, images) {
  showDialog((close) => html`<${Arrange} album=${album} images=${images} close=${close} />`);
}

// The pictures of a disc go together, after the album's own: a disc chosen moves a picture there.
const byDisc = (rows) => [...rows].sort((a, b) => a.disc - b.disc);

function Arrange({ album, images, close }) {
  const [rows, setRows] = useState(() => byDisc(images));
  const [busy, run] = useRunner();
  const discs = [...new Set([0, ...(album.entries || []).map((e) => e.disc_no || 1), ...rows.map((r) => r.disc)])].sort((a, b) => a - b);
  const withDiscs = discs.length > 2 || rows.some((r) => r.disc);
  const move = (from, to) => setRows((rs) => {
    const next = [...rs];
    next.splice(to, 0, ...next.splice(from, 1));
    return byDisc(next);
  });
  const { drag, start, lift } = useReorder(rows.map((r) => r.id), move);
  const set = (i, field, value) => setRows((rs) => {
    const next = rs.map((r, j) => (j === i ? { ...r, [field]: value } : r));
    return field === 'disc' ? byDisc(next) : next;
  });
  const save = () => run(async () => {
    try {
      await api('PUT', `/albums/${album.id}/images`, { images: rows.map(({ id, kind, disc }) => ({ id, kind, disc })) });
    } catch (e) {
      if (e.status !== 409) throw e;
      toast('內頁圖剛才有變動，請重新開啟「整理」再試一次', 'error');
      changed();
      close();
      return;
    }
    changed();
    toast('已更新內頁圖');
    close();
  });
  return html`<${Dialog} title="整理內頁圖" wide onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy} onClick=${save}>儲存</button>`}>
    <p class="hint">拖曳或用箭頭排序，並標示每張是什麼${withDiscs ? '、屬於哪片光碟（同一片的排在一起，整張專輯的排在最前面）' : ''}。</p>
    <ol class=${'pic-arrange' + (drag ? ' dragging' : '')}>${rows.map((r, i) => html`<li key=${r.id} ...${lift(i)}>
      <div class="pic-arrange-row">
        <${DragHandle} onStart=${(e) => start(e, i)} label=${`拖曳「${r.name}」改變順序`} />
        <img src=${imageURL(r.id, 256)} alt="" loading="lazy" draggable="false" />
        <span class="grow track-text"><span class="title" title=${r.name}>${r.name}</span>
          <span class="sub">${r.width} × ${r.height}</span></span>
        <select aria-label=${`「${r.name}」是`} value=${r.kind} onChange=${(e) => set(i, 'kind', e.target.value)}>
          <option value="">未標示</option>
          ${Object.entries(KINDS).map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}
        </select>
        ${withDiscs && html`<select aria-label=${`「${r.name}」屬於`} value=${r.disc} onChange=${(e) => set(i, 'disc', Number(e.target.value))}>
          ${discs.map((d) => html`<option key=${d} value=${d}>${d ? discName(album, d) : '整張專輯'}</option>`)}
        </select>`}
        <span class="pic-arrange-moves">
          <${IconButton} icon="up" label="上移" disabled=${i === 0 || rows[i - 1].disc !== r.disc} onClick=${() => move(i, i - 1)} />
          <${IconButton} icon="down" label="下移" disabled=${i === rows.length - 1 || rows[i + 1].disc !== r.disc} onClick=${() => move(i, i + 1)} />
        </span>
      </div>
    </li>`)}</ol>
  <//>`;
}

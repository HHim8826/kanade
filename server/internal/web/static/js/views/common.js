import { useState } from '../../vendor/hooks.module.js';
import { get } from '../api.js';
import { favs, trackMenu } from '../actions.js';
import { fromEntry, fromTrack, playQueue, player } from '../player.js';
import { href } from '../router.js';
import { useStore } from '../store.js';
import { Cover, Empty, Icon, IconButton, fmtQuality, fmtTime, html } from '../ui.js';

// Lists shared by the library, playlist, favorites and history pages.

export function AlbumGrid({ albums, empty = '還沒有專輯。到「任務」上傳音樂或加入下載。' }) {
  if (!albums.length) return html`<${Empty} icon="album">${empty}<//>`;
  return html`<div class="grid">
    ${albums.map((a) => html`<a key=${a.id} class="card album-card" href=${href('album/' + a.id)}>
      <${Cover} id=${a.cover_id} alt="" />
      <div class="card-text">
        <div class="title" title=${a.title}>${a.title}</div>
        <div class="sub">${a.album_artist || '未知歌手'}</div>
      </div>
    </a>`)}
  </div>`;
}

const moved = (list, from, to) => {
  const out = [...list];
  out.splice(to, 0, ...out.splice(from, 1));
  return out;
};

// TrackList plays the whole list starting from the row that was tapped. menuExtra(item, i) adds
// entries to a row's menu; onReorder(from, to) turns on drag handles.
export function TrackList({ items, showNumber, showAlbum, menuExtra, onReorder, meta }) {
  const playingId = useStore(player, (s) => s.queue[s.index]?.assetId);
  const favTracks = useStore(favs, (s) => s.tracks);
  const [drag, setDrag] = useState(null); // { from, to } while a row is being dragged
  const view = drag ? moved(items, drag.from, drag.to) : items;

  const startDrag = (e, from) => {
    e.preventDefault();
    const handle = e.currentTarget;
    const mids = [...handle.closest('ol').children].map((li) => {
      const r = li.getBoundingClientRect();
      return r.top + r.height / 2;
    });
    let to = from;
    handle.setPointerCapture(e.pointerId);
    setDrag({ from, to });
    const move = (ev) => {
      const n = mids.filter((m, k) => k !== from && m < ev.clientY).length;
      if (n !== to) setDrag({ from, to: (to = n) });
    };
    const up = () => {
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
      setDrag(null);
      if (to !== from) onReorder(from, to);
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
  };

  return html`<ol class=${'tracks' + (drag ? ' dragging' : '')}>
    ${view.map((it, i) => html`<li key=${it.key || it.assetId + '-' + i} class=${drag && drag.to === i ? 'lifted' : ''}>
      <div class=${'track' + (it.assetId === playingId ? ' current' : '')}>
        ${onReorder && html`<span class="drag-handle" role="button" aria-label="拖曳排序" title="拖曳排序"
          onPointerDown=${(e) => startDrag(e, i)}><${Icon} name="drag" /></span>`}
        <button class="track-main" onClick=${() => playQueue(items, i)}>
          ${showNumber ? html`<span class="num">${it.number || ''}</span>` : html`<${Cover} id=${it.coverId} size=${96} className="thumb" />`}
          <span class="track-text">
            <span class="title">${it.title}</span>
            <span class="sub">${[it.artist || '未知歌手', showAlbum && it.album].filter(Boolean).join(' · ')}</span>
          </span>
          <span class="meta">${meta ? meta(it) : html`<span class="quality">${fmtQuality(it.asset)}</span><span>${fmtTime(it.durationMs)}</span>`}</span>
        </button>
        ${favTracks.has(it.trackId) && html`<span class="fav-mark" title="已收藏"><${Icon} name="favorite" size=${16} /></span>`}
        <${IconButton} icon="more" label="更多" className="row-more" onClick=${(e) => trackMenu(e, it, menuExtra ? menuExtra(it, i) : [])} />
      </div>
    </li>`)}
  </ol>`;
}

// playInAlbum plays a track inside the album it came from, so the queue continues with the album.
// position_ms resumes a half-heard track; finished moves on to the next one (the album's start
// after its last track).
export async function playInAlbum(item) {
  if (item.album_id) {
    try {
      const a = await get('/albums/' + item.album_id);
      const items = a.entries.map((e) => fromEntry(e, a));
      const i = items.findIndex((q) => q.assetId === item.asset.id);
      if (i >= 0 && item.finished) return playQueue(items, i + 1 < items.length ? i + 1 : 0);
      if (i >= 0) {
        items[i] = { ...items[i], resumeMs: item.position_ms || 0 };
        return playQueue(items, i);
      }
    } catch { /* fall back to the single track */ }
  }
  playQueue([{ ...fromTrack(item), resumeMs: item.finished ? 0 : item.position_ms || 0 }], 0);
}

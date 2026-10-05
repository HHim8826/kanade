import { useEffect, useRef, useState } from '../../vendor/hooks.module.js';
import { get } from '../api.js';
import { favs, trackMenu } from '../actions.js';
import { fromEntry, fromTrack, playQueue, player } from '../player.js';
import { href } from '../router.js';
import { useStore } from '../store.js';
import { Cover, Empty, Icon, IconButton, fmtQuality, fmtTime, html } from '../ui.js';

// Lists shared by the library, playlist, favorites and history pages.

// AlbumGrid shows albums as cards; with sel (useSelection), they can be selected (review #83).
export function AlbumGrid({ albums, empty = '還沒有專輯。到「任務」上傳音樂或加入下載。', sel }) {
  if (!albums.length) return html`<${Empty} icon="album">${empty}<//>`;
  const keys = albums.map((a) => a.id);
  return html`<div class=${'grid' + (sel && sel.on ? ' selecting' : '')}>
    ${albums.map((a, i) => {
      const on = sel && sel.has(a.id);
      return html`<a key=${a.id} class=${'card album-card' + (on ? ' selected' : '')} href=${href('album/' + a.id)}
        onClick=${sel && ((e) => sel.click(e, keys, i))} role=${sel && sel.on ? 'checkbox' : undefined} aria-checked=${sel && sel.on ? !!on : undefined}>
        <${Cover} id=${a.cover_id} alt="" />
        ${sel && sel.on && html`<span class="check" aria-hidden="true">${on ? html`<${Icon} name="check" size=${18} />` : ''}</span>`}
        <div class="card-text">
          <div class="title" title=${a.title}>${a.title}</div>
          <div class="sub" title=${a.album_artist || ''}>${a.album_artist || '未知歌手'}</div>
        </div>
      </a>`;
    })}
  </div>`;
}

// scrollParent is the nearest element that scrolls vertically.
function scrollParent(el) {
  for (let p = el.parentElement; p; p = p.parentElement) {
    const o = getComputedStyle(p).overflowY;
    if ((o === 'auto' || o === 'scroll') && p.scrollHeight > p.clientHeight) return p;
  }
  return null;
}

// useReorder drags list rows to a new place: by a row's handle with a mouse, a pen or a finger, or
// by the row itself with a mouse or a pen (a finger on a row scrolls the list). The rows stay where
// they are in the page while one is dragged: it follows the pointer and the rows it passes make
// room, so the browser keeps sending the pointer's events to it. The list scrolls while the pointer
// is held near the edge of its scrolling area. keys are the rows' keys; onReorder(from, to) runs on
// release. Escape, a canceled touch, a lost pointer or a change to the list puts the row back.
// start(e, i) goes on a handle's pointerdown, press(e, i) on a row's; lift(i) is a row's look.
// Keyboard users have the rows' move entries.
export function useReorder(keys, onReorder) {
  const [drag, setDrag] = useState(null); // { from, to, dy, h } while a row is being dragged
  const stop = useRef(null); // ends the drag under way; stop(true) drops the row where it is
  const list = keys.join('\n');
  useEffect(() => () => stop.current && stop.current(false), [list]);

  // begin drags row from, held at y0 and now at y.
  const begin = (el, pointerId, from, y0, y) => {
    const ol = el.closest('ol');
    const scroller = scrollParent(ol);
    const scrolled = () => (scroller ? scroller.scrollTop : 0);
    const top0 = scrolled();
    // Rows in the scrolling area's own coordinates, so scrolling while dragging keeps them right.
    const boxes = [...ol.children].map((li) => li.getBoundingClientRect()).map((r) => ({ top: r.top + top0, bottom: r.bottom + top0 }));
    const h = boxes[from].bottom - boxes[from].top;
    const lo = boxes[0].top - boxes[from].top, hi = boxes[boxes.length - 1].bottom - boxes[from].bottom;
    let to = from;
    // A row makes room once the dragged row's middle is past its near edge: dropped over a row, the
    // dragged one takes its place.
    const place = () => {
      const dy = Math.min(hi, Math.max(lo, y - y0 + scrolled() - top0));
      const mid = (boxes[from].top + boxes[from].bottom) / 2 + dy;
      to = dy >= 0 ? from + boxes.filter((b, k) => k > from && b.top < mid).length : from - boxes.filter((b, k) => k < from && b.bottom > mid).length;
      setDrag({ from, to, dy, h });
    };
    const edge = setInterval(() => {
      if (!scroller) return;
      const r = scroller.getBoundingClientRect();
      const by = y < r.top + 56 ? -14 : y > r.bottom - 56 ? 14 : 0;
      if (by) {
        scroller.scrollTop += by;
        place();
      }
    }, 16);
    const mine = (f) => (ev) => ev.pointerId === pointerId && f(ev);
    const move = mine((ev) => {
      y = ev.clientY;
      place();
    });
    const up = mine(() => end(true));
    const cancel = mine(() => end(false));
    const key = (ev) => ev.key === 'Escape' && end(false);
    const end = (drop) => {
      stop.current = null;
      clearInterval(edge);
      removeEventListener('pointermove', move, true);
      removeEventListener('pointerup', up, true);
      removeEventListener('pointercancel', cancel, true);
      removeEventListener('keydown', key, true);
      el.removeEventListener('lostpointercapture', cancel);
      setDrag(null);
      if (drop && to !== from) onReorder(from, to);
    };
    stop.current = end;
    addEventListener('pointermove', move, true);
    addEventListener('pointerup', up, true);
    addEventListener('pointercancel', cancel, true);
    addEventListener('keydown', key, true);
    try {
      el.setPointerCapture(pointerId);
      el.addEventListener('lostpointercapture', cancel);
    } catch { /* the pointer is already up: the window's listeners still end the drag */ }
    place();
  };

  const start = (e, from) => {
    if ((e.pointerType === 'mouse' && e.button !== 0) || stop.current) return;
    e.preventDefault();
    begin(e.currentTarget, e.pointerId, from, e.clientY, e.clientY);
  };

  // A row held and moved a few pixels starts dragging; the click its release makes does not play it.
  const press = (e, from) => {
    if (e.pointerType === 'touch' || e.button !== 0 || stop.current) return;
    const el = e.currentTarget, x0 = e.clientX, y0 = e.clientY, id = e.pointerId;
    const move = (ev) => {
      if (ev.pointerId !== id || Math.hypot(ev.clientX - x0, ev.clientY - y0) < 6) return;
      off();
      getSelection().removeAllRanges();
      addEventListener('pointerup', released, true);
      begin(el, id, from, y0, ev.clientY);
    };
    const released = (ev) => {
      if (ev.pointerId !== id) return;
      removeEventListener('pointerup', released, true);
      addEventListener('click', swallow, true); // the click comes right after, in the same task
      setTimeout(() => removeEventListener('click', swallow, true), 0);
    };
    const swallow = (ev) => {
      ev.stopPropagation();
      ev.preventDefault();
    };
    const off = () => {
      removeEventListener('pointermove', move, true);
      removeEventListener('pointerup', off, true);
      removeEventListener('pointercancel', off, true);
    };
    addEventListener('pointermove', move, true);
    addEventListener('pointerup', off, true);
    addEventListener('pointercancel', off, true);
  };

  const lift = (i) => {
    if (!drag) return {};
    const { from, to, dy, h } = drag;
    if (i === from) return { class: 'lifted', style: `transform: translateY(${dy}px)` };
    const by = from < to && i > from && i <= to ? -h : to < from && i >= to && i < from ? h : 0;
    return { style: by ? `transform: translateY(${by}px)` : '' };
  };
  return { drag, start, press, lift };
}

// DragHandle is the grip a row is dragged by.
export const DragHandle = ({ onStart, label = '拖曳排序' }) => html`<span class="drag-handle" role="button" aria-label=${label}
  title=${label} onPointerDown=${onStart}><${Icon} name="drag" /></span>`;

// TrackList plays the whole list starting from the row that was tapped; with queue (the whole
// album when this list is one disc of it) that list plays instead, from the tapped song. menuExtra(item,
// i) adds entries to a row's menu; onReorder(from, to) turns on drag handles.
// With sel (useSelection), rows can be selected by selKey(item) (the song's ID unless given; the
// entry's on an album page), and a click checks a row instead of playing it (review #83).
export function TrackList({ items, queue, showNumber, showAlbum, menuExtra, onReorder, meta, sel, selKey = (it) => it.trackId }) {
  const playingId = useStore(player, (s) => s.queue[s.index]?.assetId);
  const favTracks = useStore(favs, (s) => s.tracks);
  const key = (it, i) => it.key || it.assetId + '-' + i;
  const { drag, start, press, lift } = useReorder(items.map(key), onReorder);
  const keys = sel ? items.map(selKey) : [];
  const play = (e, it, i) => {
    if (sel && sel.click(e, keys, i)) return;
    queue ? playQueue(queue, Math.max(queue.indexOf(it), 0)) : playQueue(items, i);
  };

  return html`<ol class=${'tracks' + (drag ? ' dragging' : '') + (sel && sel.on ? ' selecting' : '')}>
    ${items.map((it, i) => html`<li key=${key(it, i)} ...${lift(i)}>
      <div class=${'track' + (it.assetId === playingId ? ' current' : '') + (sel && sel.has(keys[i]) ? ' selected' : '')}>
        ${sel && sel.on && html`<input type="checkbox" class="row-check" checked=${sel.has(keys[i])} aria-label=${`選取「${it.title}」`}
          onClick=${(e) => sel.click(e, keys, i)} />`}
        ${onReorder && !(sel && sel.on) && html`<${DragHandle} onStart=${(e) => start(e, i)} />`}
        <button class="track-main" onPointerDown=${onReorder && ((e) => press(e, i))} onClick=${(e) => play(e, it, i)}
          aria-pressed=${sel && sel.on ? sel.has(keys[i]) : undefined}>
          ${showNumber ? html`<span class="num">${it.number || ''}</span>` : html`<${Cover} id=${it.coverId} size=${96} className="thumb" />`}
          <span class="track-text">
            <span class="title">${it.title}</span>
            <span class="sub">${it.tags && it.tags.map((t) => html`<span class="track-tag" title=${t.title}>${t.label}</span>`)}${[it.artist || '未知歌手', showAlbum && it.album].filter(Boolean).join(' · ')}</span>
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

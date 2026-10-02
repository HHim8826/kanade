import { useEffect, useMemo, useRef, useState } from '../../vendor/hooks.module.js';
import { api, get } from '../api.js';
import { addToPlaylist, toggleFav, useFav } from '../actions.js';
import { current, next, playAt, player, prev, seek, toggle } from '../player.js';
import { go, href } from '../router.js';
import { createStore, useStore } from '../store.js';
import { Cover, Dialog, Empty, ErrorBox, IconButton, Spinner, fmtQuality, fmtTime, html, openMenu, showDialog, toast, useLoad } from '../ui.js';

const open = (v) => player.set({ nowPlayingOpen: v });

export function PlayerBar() {
  const s = useStore(player);
  const item = s.queue[s.index];
  if (!item) return null;
  const pct = s.duration ? (s.time / s.duration) * 100 : 0;
  return html`<div class="player-bar">
    <div class="bar-progress"><div style=${{ width: pct + '%' }}></div></div>
    <button class="now" onClick=${() => open(true)} aria-label="開啟正在播放">
      <${Cover} id=${item.coverId} size=${96} className="thumb" />
      <span class="track-text"><span class="title">${item.title}</span><span class="sub">${item.artist || '未知歌手'}</span></span>
    </button>
    <div class="controls">
      <${IconButton} icon="prev" label="上一首" onClick=${prev} />
      <${IconButton} icon=${s.playing ? 'pause' : 'play'} label=${s.playing ? '暫停' : '播放'} onClick=${toggle} filled />
      <${IconButton} icon="next" label="下一首" onClick=${next} />
    </div>
  </div>`;
}

// The queue / lyrics choice survives closing the full-screen player.
const panel = createStore({ tab: 'queue' });

function FavButton({ trackId }) {
  const on = useFav('track', trackId);
  return html`<${IconButton} icon=${on ? 'favorite' : 'favoriteOff'} label=${on ? '取消收藏' : '收藏'} pressed=${on}
    className=${on ? 'fav-on' : ''} onClick=${() => toggleFav('track', trackId)} />`;
}

export function NowPlaying() {
  const s = useStore(player);
  const { tab } = useStore(panel);
  const item = current();
  const shown = s.nowPlayingOpen && !!item;
  useEffect(() => { // the page underneath must not scroll while this covers it
    document.body.classList.toggle('locked', shown);
  }, [shown]);
  if (!shown) return null;
  const menu = (e) => openMenu(e, [
    item.trackId && { icon: 'playlistAdd', label: '加入歌單…', onClick: () => addToPlaylist([item]) },
    item.albumId && { icon: 'album', label: '前往專輯', onClick: () => go('album/' + item.albumId) },
    item.trackId && { icon: 'lyrics', label: '編輯歌詞', onClick: () => editLyrics(item) },
  ]);
  return html`<div class="now-playing" role="dialog" aria-modal="true" aria-label="正在播放">
    <div class="np-top">
      <${IconButton} icon="expand" label="收起" onClick=${() => open(false)} />
      <${IconButton} icon="more" label="更多" onClick=${menu} />
    </div>
    <div class="np-main">
      <${Cover} id=${item.coverId} size=${600} alt=${item.album || item.title} className="np-cover" />
      <div class="np-info">
        <div class="np-title-row">
          <div class="grow">
            <div class="np-title">${item.title}</div>
            <div class="sub">${item.artist || '未知歌手'}</div>
            ${item.albumId ? html`<a class="sub link" href=${href('album/' + item.albumId)} onClick=${() => open(false)}>${item.album}</a>` : ''}
          </div>
          ${item.trackId && html`<${FavButton} trackId=${item.trackId} />`}
        </div>
        <div class="quality">${fmtQuality(item.asset)}</div>
        <input class="seek" type="range" min="0" max=${s.duration || 0} step="0.1" value=${s.time}
          onInput=${(e) => seek(parseFloat(e.target.value))} aria-label="播放位置" />
        <div class="times"><span>${fmtTime(s.time * 1000)}</span><span>${s.buffering ? '緩衝中…' : ''}</span><span>${fmtTime(s.duration * 1000)}</span></div>
        <div class="np-controls">
          <${IconButton} icon="prev" label="上一首" onClick=${prev} size=${32} />
          <${IconButton} icon=${s.playing ? 'pause' : 'play'} label=${s.playing ? '暫停' : '播放'} onClick=${toggle} filled size=${40} />
          <${IconButton} icon="next" label="下一首" onClick=${next} size=${32} />
        </div>
      </div>
    </div>
    <div class="np-panel">
      <nav class="tabs" role="tablist">
        ${[['queue', '播放佇列'], ['lyrics', '歌詞']].map(([k, label]) => html`<button role="tab" aria-selected=${k === tab}
          class=${k === tab ? 'active' : ''} onClick=${() => panel.set({ tab: k })}>${label}</button>`)}
      </nav>
      ${tab === 'queue'
        ? html`<ol class="tracks">${s.queue.map((q, i) => html`<li key=${i}>
            <button class=${'track' + (i === s.index ? ' current' : '')} onClick=${() => playAt(i)}>
              <span class="num">${i + 1}</span>
              <span class="track-text"><span class="title">${q.title}</span><span class="sub">${q.artist}</span></span>
              <span class="meta">${fmtTime(q.durationMs)}</span>
            </button></li>`)}</ol>`
        : html`<${Lyrics} item=${item} time=${s.time} />`}
    </div>
  </div>`;
}

// ---- lyrics ----

const lyricsCache = new Map(); // track ID -> lyrics or null; edits clear the entry
const lyricsRev = createStore({ n: 0 }); // bumped after an edit so open views reload

function loadLyrics(trackId) {
  if (lyricsCache.has(trackId)) return Promise.resolve(lyricsCache.get(trackId));
  return get(`/tracks/${trackId}/lyrics`).catch((e) => (e.status === 404 ? null : Promise.reject(e))).then((l) => {
    lyricsCache.set(trackId, l);
    return l;
  });
}

// parseLRC reads [mm:ss.xx] lines (several tags on one line repeat it) and [offset:±ms];
// word-level <mm:ss.xx> tags of enhanced LRC are dropped.
export function parseLRC(text) {
  let offset = 0;
  const lines = [];
  for (const raw of text.split('\n')) {
    const off = raw.match(/^\s*\[offset:\s*([+-]?\d+)\s*\]/i);
    if (off) {
      offset = Number(off[1]);
      continue;
    }
    const tags = [...raw.matchAll(/\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]/g)];
    if (!tags.length) continue; // metadata like [ti:...] or plain text
    const words = raw.replace(/\[[^\]]*\]/g, '').replace(/<\d{1,3}:\d{1,2}(?:[.:]\d{1,3})?>/g, '').trim();
    for (const m of tags) {
      const frac = m[3] ? Number(m[3]) / 10 ** m[3].length : 0;
      lines.push({ t: (Number(m[1]) * 60 + Number(m[2]) + frac) * 1000 - offset, text: words });
    }
  }
  return lines.sort((a, b) => a.t - b.t);
}

function Lyrics({ item, time }) {
  const rev = useStore(lyricsRev, (s) => s.n);
  const data = useLoad(() => loadLyrics(item.trackId), [item.trackId, rev]);
  const lines = useMemo(() => (data.data && data.data.synced ? parseLRC(data.data.text) : []), [data.data]);
  const box = useRef(null);
  const userScrolled = useRef(0);
  const ms = time * 1000;
  let at = -1;
  for (let i = 0; i < lines.length && lines[i].t <= ms; i++) at = i;

  useEffect(() => { // keep the current line in the middle, unless the listener is scrolling
    const el = box.current;
    if (!el || at < 0 || Date.now() - userScrolled.current < 4000) return;
    const li = el.children[at];
    if (li) el.scrollTo({ top: li.offsetTop - el.clientHeight / 2 + li.clientHeight / 2, behavior: 'smooth' });
  }, [at]);

  if (!item.trackId) return html`<${Empty} icon="lyrics">這首歌沒有歌詞。<//>`;
  if (data.loading) return html`<${Spinner} />`;
  if (data.error) return html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`;
  if (!data.data) {
    return html`<${Empty} icon="lyrics">這首歌沒有歌詞。
      <div class="actions center"><button class="btn tonal" onClick=${() => editLyrics(item)}>新增歌詞</button></div><//>`;
  }
  const mark = () => { userScrolled.current = Date.now(); };
  return html`<div class="lyrics-wrap">
    ${lines.length
      ? html`<ol class="lyrics synced" ref=${box} onWheel=${mark} onTouchMove=${mark}>
          ${lines.map((l, i) => html`<li key=${i} class=${i === at ? 'now' : i < at ? 'past' : ''}>
            <button onClick=${() => seek(Math.max(l.t, 0) / 1000)}>${l.text || '♪'}</button></li>`)}</ol>`
      : html`<div class="lyrics plain">${data.data.text.replace(/\[[^\]]*\]/g, '')}</div>`}
    <div class="lyrics-foot sub">
      <span>${{ embedded: '來自音檔標籤', lrc: '來自 LRC 檔', manual: '手動輸入' }[data.data.source] || ''}</span>
      <button class="btn text" onClick=${() => editLyrics(item)}>編輯</button>
    </div>
  </div>`;
}

function editLyrics(item) {
  showDialog((close) => html`<${LyricsEditor} item=${item} close=${close} />`);
}

function LyricsEditor({ item, close }) {
  const data = useLoad(() => loadLyrics(item.trackId), [item.trackId]);
  const [text, setText] = useState(null);
  const [busy, setBusy] = useState(false);
  const value = text ?? (data.data ? data.data.text : '');
  const save = async (body) => {
    setBusy(true);
    try {
      if (body) await api('PUT', `/tracks/${item.trackId}/lyrics`, { text: body });
      else await api('DELETE', `/tracks/${item.trackId}/lyrics`);
      lyricsCache.delete(item.trackId);
      lyricsRev.set((r) => ({ n: r.n + 1 }));
      toast(body ? '已儲存歌詞' : '已刪除歌詞');
      close();
    } catch (e) {
      toast(e.message, 'error');
      setBusy(false);
    }
  };
  return html`<${Dialog} title=${`歌詞：${item.title}`} onClose=${close} actions=${html`
      ${data.data && html`<button class="btn text danger-text" disabled=${busy} onClick=${() => save('')}>刪除</button>`}
      <span class="grow"></span>
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !value.trim()} onClick=${() => save(value)}>儲存</button>`}>
    ${data.loading ? html`<${Spinner} />` : html`
      <p class="hint">可貼上一般文字，或含時間標記的 LRC（例如 <code>[01:23.45]歌詞</code>），有時間標記時播放頁會逐行顯示。手動輸入的歌詞不會被之後的匯入覆蓋。</p>
      <label class="field">歌詞<textarea rows="14" value=${value} onInput=${(e) => setText(e.target.value)}></textarea></label>`}
  <//>`;
}

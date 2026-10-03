import { useEffect, useMemo, useRef, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { addToPlaylist, toggleFav, useFav } from '../actions.js';
import {
  clearUpcoming, current, cycleMode, endScrub, moveItem, next, playAfterCurrent, playAt, player, prev, removeAt, resetPlayer, scrubTo, seek,
  setVolume, toggle, toggleMute,
} from '../player.js';
import { go, href } from '../router.js';
import { DragHandle, useReorder } from './common.js';
import { createStore, useStore } from '../store.js';
import { Cover, Dialog, Empty, ErrorBox, IconButton, Spinner, fmtQuality, fmtTime, html, openMenu, showDialog, toast, useLoad } from '../ui.js';

const open = (v) => player.set({ nowPlayingOpen: v });

// The queue / lyrics choice survives closing the full-screen player.
const panel = createStore({ tab: 'queue' });
const openTab = (tab) => {
  panel.set({ tab });
  open(true);
};

// shownTime is where the song is, or where the seek bar is being dragged to.
const shownTime = (s) => (s.scrub ?? s.time);

// Seek is the playing position as a slider; the filled part follows it. Dragging previews the
// position and seeks once on release; each key press seeks at once (review #41).
function Seek({ s, className }) {
  const t = shownTime(s);
  const pct = s.duration ? Math.min(t / s.duration, 1) * 100 : 0;
  return html`<input class=${'seek ' + className} type="range" min="0" max=${s.duration || 0} step="0.1" value=${t}
    style=${{ '--p': pct + '%' }} onInput=${(e) => scrubTo(parseFloat(e.target.value))}
    onChange=${(e) => seek(parseFloat(e.target.value))} onPointerCancel=${() => endScrub(false)} onBlur=${() => endScrub(true)}
    onKeyDown=${(e) => seekKey(e, s)} aria-label="播放位置" aria-valuetext=${`${fmtTime(t * 1000)} / ${fmtTime(s.duration * 1000)}`} />`;
}

// The arrow keys move five seconds, not the slider's fine step.
function seekKey(e, s) {
  const by = { ArrowLeft: -5, ArrowDown: -5, ArrowRight: 5, ArrowUp: 5, PageDown: -30, PageUp: 30 }[e.key];
  if (!by || !s.duration) return;
  e.preventDefault();
  seek(Math.min(Math.max(s.time + by, 0), s.duration));
}

const modeLook = { order: ['order', '順序播放'], all: ['repeat', '列表循環'], one: ['repeatOne', '單曲循環'], shuffle: ['shuffle', '隨機播放'] };

// ModeButton shows the play mode and switches to the next one (review #40).
function ModeButton({ s, size }) {
  const [icon, name] = modeLook[s.mode];
  return html`<${IconButton} icon=${icon} label=${`播放模式：${name}（按一下切換）`} size=${size}
    className=${'mode' + (s.mode !== 'order' ? ' on' : '')} onClick=${cycleMode} />`;
}

function Volume({ s }) {
  const level = s.muted ? 0 : s.volume;
  return html`<div class="volume">
    <${IconButton} icon=${level === 0 ? 'volumeOff' : 'volume'} label=${s.muted ? '取消靜音' : '靜音'} pressed=${s.muted} onClick=${toggleMute} />
    <input type="range" min="0" max="1" step="0.01" value=${level} style=${{ '--p': level * 100 + '%' }} aria-label="音量"
      aria-valuetext=${Math.round(level * 100) + '%'} onInput=${(e) => setVolume(parseFloat(e.target.value))} />
  </div>`;
}

export function PlayerBar() {
  const s = useStore(player);
  const item = s.queue[s.index];
  if (!item) return null;
  return html`<div class="player-bar">
    <${Seek} s=${s} className="bar-seek" />
    <button class="now" onClick=${() => open(true)} aria-label="開啟正在播放">
      <${Cover} id=${item.coverId} size=${96} className="thumb" />
      <span class="track-text"><span class="title">${item.title}</span><span class="sub">${item.artist || '未知歌手'}</span></span>
    </button>
    <div class="controls">
      <span class="wide-only"><${ModeButton} s=${s} /></span>
      <${IconButton} icon="prev" label="上一首" onClick=${prev} />
      <${IconButton} icon=${s.playing ? 'pause' : 'play'} label=${s.playing ? '暫停' : '播放'} onClick=${toggle} filled />
      <${IconButton} icon="next" label="下一首" onClick=${next} />
    </div>
    <div class="bar-extra">
      <span class="bar-time wide-only">${fmtTime(shownTime(s) * 1000)} / ${fmtTime(s.duration * 1000)}</span>
      <span class="wide-only">${item.trackId && html`<${FavButton} trackId=${item.trackId} />`}</span>
      <span class="wide-only"><${IconButton} icon="lyrics" label="歌詞" onClick=${() => openTab('lyrics')} /></span>
      <${IconButton} icon="queue" label="播放佇列" onClick=${() => openTab('queue')} />
      <span class="wide-only"><${Volume} s=${s} /></span>
    </div>
  </div>`;
}

function FavButton({ trackId }) {
  const on = useFav('track', trackId);
  return html`<${IconButton} icon=${on ? 'favorite' : 'favoriteOff'} label=${on ? '取消收藏' : '收藏'} pressed=${on}
    className=${on ? 'fav-on' : ''} onClick=${() => toggleFav('track', trackId)} />`;
}

const showQueue = () => {
  panel.set({ tab: 'queue' });
  document.querySelector('.np-panel')?.scrollIntoView({ behavior: 'smooth', block: 'start' });
};

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
        <${Seek} s=${s} className="np-seek" />
        <div class="times"><span>${fmtTime(shownTime(s) * 1000)}</span><span>${s.buffering ? '緩衝中…' : ''}</span><span>${fmtTime(s.duration * 1000)}</span></div>
        <div class="np-controls">
          <${ModeButton} s=${s} />
          <${IconButton} icon="prev" label="上一首" onClick=${prev} size=${32} />
          <${IconButton} icon=${s.playing ? 'pause' : 'play'} label=${s.playing ? '暫停' : '播放'} onClick=${toggle} filled size=${40} />
          <${IconButton} icon="next" label="下一首" onClick=${next} size=${32} />
          <${IconButton} icon="queue" label="播放佇列" onClick=${showQueue} />
        </div>
        <${Volume} s=${s} />
      </div>
    </div>
    <div class="np-panel">
      <nav class="tabs" role="tablist">
        ${[['queue', '播放佇列'], ['lyrics', '歌詞']].map(([k, label]) => html`<button role="tab" aria-selected=${k === tab}
          class=${k === tab ? 'active' : ''} onClick=${() => panel.set({ tab: k })}>${label}</button>`)}
      </nav>
      ${tab === 'queue' ? html`<${Queue} s=${s} />` : html`<${Lyrics} item=${item} time=${s.time} />`}
    </div>
  </div>`;
}

// Queue lists what plays, in play order; songs can be moved, moved up next or taken out.
function Queue({ s }) {
  const last = s.queue.length - 1;
  const current = s.queue[s.index]?.qid;
  const { drag, start, press, lift } = useReorder(s.queue.map((q, i) => q.qid || i), moveItem);
  const menu = (e, i) => openMenu(e, [
    i !== s.index && i !== s.index + 1 && { icon: 'playNext', label: '移到下一首播放', onClick: () => playAfterCurrent(i) },
    i > 0 && { icon: 'up', label: '上移', onClick: () => moveItem(i, i - 1) },
    i < last && { icon: 'down', label: '下移', onClick: () => moveItem(i, i + 1) },
    { icon: 'close', label: '從佇列移除', onClick: () => removeAt(i) },
  ]);
  return html`<div class="queue">
    <div class="queue-head">
      <span class="sub grow">${s.queue.length} 首 · ${modeLook[s.mode][1]}</span>
      <button class="btn text" disabled=${s.index >= last} onClick=${clearUpcoming}>清除待播</button>
      <button class="btn text" onClick=${() => resetPlayer()}>停止並清空</button>
    </div>
    <ol class=${'tracks queue-list' + (drag ? ' dragging' : '')}>${s.queue.map((q, i) => html`<li key=${q.qid || i} ...${lift(i)}>
      <div class=${'track' + (q.qid === current ? ' current' : '')}>
        <${DragHandle} onStart=${(e) => start(e, i)} label=${`拖曳「${q.title}」改變播放順序`} />
        <button class="track-main" onPointerDown=${(e) => press(e, i)} onClick=${() => playAt(i)} aria-current=${q.qid === current ? 'true' : undefined}>
          <span class="num">${i + 1}</span>
          <span class="track-text"><span class="title">${q.title}</span><span class="sub">${q.artist}</span></span>
          <span class="meta">${fmtTime(q.durationMs)}</span>
        </button>
        <${IconButton} icon="more" label=${`「${q.title}」的佇列選項`} className="row-more" onClick=${(e) => menu(e, i)} />
        <${IconButton} icon="close" label=${`從佇列移除「${q.title}」`} className="row-more" onClick=${() => removeAt(i)} />
      </div>
    </li>`)}</ol>
  </div>`;
}

// ---- lyrics ----

const lyricsCache = new Map(); // track ID -> lyrics or null; edits clear the entry
const lyricsRev = createStore({ n: 0 }); // bumped after an edit so open views reload
const lookFor = new Set(); // spoken tracks the listener asked LRCLIB about

function loadLyrics(trackId) {
  if (lyricsCache.has(trackId)) return Promise.resolve(lyricsCache.get(trackId));
  return get(`/tracks/${trackId}/lyrics`).catch((e) => (e.status === 404 ? null : Promise.reject(e))).then((l) => {
    lyricsCache.set(trackId, l);
    return l;
  });
}

// ---- lyrics found online (LRCLIB) ----
// Asked only for a song whose lyrics are being looked at; the answer is kept for this page.
const foundCache = new Map(); // track ID -> candidates

function findLyrics(trackId) {
  if (foundCache.has(trackId)) return Promise.resolve(foundCache.get(trackId));
  return get(`/tracks/${trackId}/lyrics/online`).catch((e) => (e.status === 404 ? [] : Promise.reject(e))).then((list) => {
    foundCache.set(trackId, list);
    return list;
  });
}

async function applyFound(trackId, id, auto = false) {
  const r = await post(`/tracks/${trackId}/lyrics/online`, { id, auto });
  lyricsCache.delete(trackId);
  lyricsRev.set((v) => ({ n: v.n + 1 }));
  return r.saved;
}

const fmtSec = (sec) => fmtTime(Math.round(sec) * 1000);

// FoundLyrics lists what LRCLIB has for a song; choosing one stores it. With auto, an exact match
// (same title and artist, length within two seconds) is stored right away, where the song has no
// lyrics yet.
function FoundLyrics({ item, auto, onChosen }) {
  const data = useLoad(() => findLyrics(item.trackId), [item.trackId]);
  const [busy, setBusy] = useState(0);
  const list = data.data || [];
  const best = list[0];
  useEffect(() => {
    if (auto && best && best.exact && !best.instrumental) {
      setBusy(best.id);
      applyFound(item.trackId, best.id, true).catch((e) => { toast(e.message, 'error'); setBusy(0); });
    }
  }, [auto, best && best.id]);
  const choose = async (c) => {
    setBusy(c.id);
    try {
      await applyFound(item.trackId, c.id);
      toast('已套用 LRCLIB 的歌詞');
      onChosen && onChosen();
    } catch (e) {
      toast(e.message, 'error');
      setBusy(0);
    }
  };
  if (data.loading || (auto && busy)) return html`<div class="found-wait sub"><${Spinner} />正在 LRCLIB 尋找歌詞…</div>`;
  if (data.error) return html`<${ErrorBox} error=${data.error} onRetry=${() => { foundCache.delete(item.trackId); data.reload(); }} />`;
  if (!list.length) return html`<p class="sub found-none">LRCLIB 也沒有找到這首歌的歌詞。</p>`;
  return html`<div class="found">
    <p class="sub">LRCLIB 找到 ${list.length} 個可能的歌詞，選一個套用：</p>
    <ul class="found-list">${list.map((c) => html`<li key=${c.id}><button class="found-item" disabled=${busy !== 0} onClick=${() => choose(c)}>
      <span class="title">${c.title}${c.exact ? html` <span class="pill good">吻合</span>` : ''}</span>
      <span class="sub">${[c.artist, c.album, c.duration ? fmtSec(c.duration) : '', c.instrumental ? '純音樂' : c.synced ? '逐行同步' : '純文字']
        .filter(Boolean).join(' · ')}</span>
      ${c.preview && html`<span class="found-preview">${c.preview}</span>`}
    </button></li>`)}</ul>
  </div>`;
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
  if (!data.data) { // none of its own: LRCLIB is asked (not for drama and radio, rarely there)
    return html`<div class="lyrics-none">
      <${Empty} icon="lyrics">這首歌沒有歌詞。<//>
      ${item.kind === 'spoken' && !lookFor.has(item.trackId)
        ? html`<div class="actions center"><button class="btn text" onClick=${() => { lookFor.add(item.trackId); lyricsRev.set((v) => ({ n: v.n + 1 })); }}>在 LRCLIB 尋找</button></div>`
        : html`<${FoundLyrics} item=${item} auto=${item.kind !== 'spoken'} />`}
      <div class="actions center"><button class="btn tonal" onClick=${() => editLyrics(item)}>自己輸入歌詞</button></div>
    </div>`;
  }
  const mark = () => { userScrolled.current = Date.now(); };
  return html`<div class="lyrics-wrap">
    ${lines.length
      ? html`<ol class="lyrics synced" ref=${box} onWheel=${mark} onTouchMove=${mark}>
          ${lines.map((l, i) => html`<li key=${i} class=${i === at ? 'now' : i < at ? 'past' : ''}>
            <button onClick=${() => seek(Math.max(l.t, 0) / 1000)}>${l.text || '♪'}</button></li>`)}</ol>`
      : html`<div class="lyrics plain">${data.data.text.replace(/\[[^\]]*\]/g, '')}</div>`}
    <div class="lyrics-foot sub">
      <span>${{ embedded: '來自音檔標籤', lrc: '來自 LRC 檔', manual: '手動輸入', lrclib: '來自 LRCLIB' }[data.data.source] || ''}</span>
      <span>
        ${data.data.source === 'lrclib' && html`<button class="btn text" onClick=${() => editLyrics(item, true)}>換一個</button>`}
        <button class="btn text" onClick=${() => editLyrics(item)}>編輯</button>
      </span>
    </div>
  </div>`;
}

function editLyrics(item, online = false) {
  showDialog((close) => html`<${LyricsEditor} item=${item} close=${close} online=${online} />`);
}

function LyricsEditor({ item, close, online: startOnline }) {
  const data = useLoad(() => loadLyrics(item.trackId), [item.trackId]);
  const [online, setOnline] = useState(!!startOnline);
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
  if (online) {
    return html`<${Dialog} title=${`線上尋找歌詞：${item.title}`} onClose=${close} actions=${html`
        <button class="btn text" onClick=${() => setOnline(false)}>自己輸入</button>
        <span class="grow"></span>
        <button class="btn text" onClick=${close}>取消</button>`}>
      <p class="hint">只會把這首歌的標題和歌手送到 LRCLIB（lrclib.net）查詢。選擇的歌詞會取代目前的歌詞。</p>
      <${FoundLyrics} item=${item} onChosen=${close} />
    <//>`;
  }
  return html`<${Dialog} title=${`歌詞：${item.title}`} onClose=${close} actions=${html`
      ${data.data && html`<button class="btn text danger-text" disabled=${busy} onClick=${() => save('')}>刪除</button>`}
      <button class="btn text" onClick=${() => setOnline(true)}>線上尋找</button>
      <span class="grow"></span>
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !value.trim()} onClick=${() => save(value)}>儲存</button>`}>
    ${data.loading ? html`<${Spinner} />` : html`
      <p class="hint">可貼上一般文字，或含時間標記的 LRC（例如 <code>[01:23.45]歌詞</code>），有時間標記時播放頁會逐行顯示。手動輸入的歌詞不會被之後的匯入覆蓋。</p>
      <label class="field">歌詞<textarea rows="14" value=${value} onInput=${(e) => setText(e.target.value)}></textarea></label>`}
  <//>`;
}

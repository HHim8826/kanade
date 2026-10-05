import { useEffect, useMemo, useRef, useState } from '../../vendor/hooks.module.js';
import { ApiError, api, get, post } from '../api.js';
import { addToPlaylist, toggleFav, useFav } from '../actions.js';
import {
  cancelSleep, clearUpcoming, clock, current, cycleMode, endScrub, extendSleep, moveItem, next, now, playAfterCurrent, playAt, player, prev,
  removeAt, resetPlayer, retryRadio, scrubTo, seek, setPrefs, setSleep, setVolume, sleepAfterTrack, toggle, toggleMute,
} from '../player.js';
import { go, href } from '../router.js';
import { DragHandle, useReorder } from './common.js';
import { createStore, useStore } from '../store.js';
import { Cover, Dialog, Empty, ErrorBox, Icon, IconButton, Spinner, fmtQuality, fmtTime, html, openMenu, showDialog, toast, useLoad } from '../ui.js';
import { BookmarkDialog, BookmarkList } from './bookmarks.js';
import { viewCover } from './coverview.js';
import { showEffects } from './effects.js';
import { Field } from './organize.js';

const open = (v) => player.set({ nowPlayingOpen: v });

// The queue / lyrics choice survives closing the full-screen player.
const panel = createStore({ tab: 'queue' });
const openTab = (tab) => {
  panel.set({ tab });
  open(true);
};

// useShownTime is where the song is, or where the seek bar is being dragged to; only what uses it
// follows the clock (review #158).
const useShownTime = (s) => {
  const time = useStore(clock, (c) => c.time);
  return s.scrub ?? time;
};

// Elapsed is the time shown (and with total, the song's length after it).
function Elapsed({ s, total }) {
  const t = useShownTime(s);
  return total ? `${fmtTime(t * 1000)} / ${fmtTime(s.duration * 1000)}` : fmtTime(t * 1000);
}

// Seek is the playing position as a slider; the filled part follows it. Dragging previews the
// position and seeks once on release; each key press seeks at once (review #41).
function Seek({ s, className }) {
  const t = useShownTime(s);
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
  seek(Math.min(Math.max(now() + by, 0), s.duration));
}

const modeLook = { order: ['order', '順序播放'], all: ['repeat', '列表循環'], one: ['repeatOne', '單曲循環'], shuffle: ['shuffle', '隨機播放'] };

// ModeButton shows the play mode and switches to the next one (review #40).
function ModeButton({ s, size }) {
  const [icon, mode] = modeLook[s.mode];
  const name = s.mode === 'shuffle' && s.scope === 'library' ? '隨機播放（全曲庫）' : mode;
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
      ${s.sleep && html`<${SleepButton} s=${s} compact />`}
      <span class="bar-time wide-only"><${Elapsed} s=${s} total /></span>
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
    item.coverId && { icon: 'image', label: '檢視封面', onClick: () => viewCover(item.coverId, item.album || item.title) },
    { icon: 'equalizer', label: '音效…', onClick: showEffects },
  ]);
  return html`<div class="now-playing" role="dialog" aria-modal="true" aria-label="正在播放">
    <div class="np-top">
      <${IconButton} icon="expand" label="收起" onClick=${() => open(false)} />
      <${IconButton} icon="more" label="更多" onClick=${menu} />
    </div>
    <div class="np-main">
      ${item.coverId
        ? html`<button class="cover-open" onClick=${() => viewCover(item.coverId, item.album || item.title)} aria-label="檢視封面" title="檢視封面">
            <${Cover} id=${item.coverId} size=${600} alt=${item.album || item.title} className="np-cover" /></button>`
        : html`<${Cover} id=${item.coverId} size=${600} alt=${item.album || item.title} className="np-cover" />`}
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
        <div class="times"><span><${Elapsed} s=${s} /></span><span>${s.buffering ? '緩衝中…' : ''}</span><span>${fmtTime(s.duration * 1000)}</span></div>
        <div class="np-controls">
          <${ModeButton} s=${s} />
          <${IconButton} icon="prev" label="上一首" onClick=${prev} size=${32} />
          <${IconButton} icon=${s.playing ? 'pause' : 'play'} label=${s.playing ? '暫停' : '播放'} onClick=${toggle} filled size=${40} />
          <${IconButton} icon="next" label="下一首" onClick=${next} size=${32} />
          <${IconButton} icon="queue" label="播放佇列" onClick=${showQueue} />
        </div>
        <${Volume} s=${s} />
        <div class="np-tools">
          <${SleepButton} s=${s} />
          ${item.trackId && html`<button class="btn text" onClick=${() => addBookmark(item)} aria-label="在目前位置加書籤">
            <${Icon} name="bookmarkAdd" />加書籤</button>`}
        </div>
      </div>
    </div>
    <div class="np-panel">
      <nav class="tabs" role="tablist">
        ${[['queue', '播放佇列'], ['lyrics', '歌詞'], ['bookmarks', '書籤']].map(([k, label]) => html`<button role="tab" aria-selected=${k === tab}
          class=${k === tab ? 'active' : ''} onClick=${() => panel.set({ tab: k })}>${label}</button>`)}
      </nav>
      ${tab === 'queue' ? html`<${Queue} s=${s} />` : tab === 'lyrics' ? html`<${Lyrics} item=${item} />`
        : html`<${TrackBookmarks} item=${item} />`}
    </div>
  </div>`;
}

// ---- sleep timer and bookmarks (review #98) ----

// useNow is the time, a second at a time, for a countdown that runs while nothing plays.
function useNow(on) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!on) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [on]);
  return now;
}

const fmtLeft = (ms) => {
  const sec = Math.max(0, Math.ceil(ms / 1000));
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), x = sec % 60;
  return h ? `${h}:${String(m).padStart(2, '0')}:${String(x).padStart(2, '0')}` : `${m}:${String(x).padStart(2, '0')}`;
};

// SleepButton sets, extends or cancels the sleep timer, and shows what is left (compact: in the
// player bar, only while it is on, as a short countdown, an icon alone on a phone: review #110).
function SleepButton({ s, compact }) {
  const now = useNow(!!(s.sleep && s.sleep.until));
  const label = !s.sleep ? '睡眠定時' : s.sleep.endOfTrack ? '播完這首停止' : `${fmtLeft(s.sleep.until - now)} 後停止`;
  const short = s.sleep && (s.sleep.endOfTrack ? '本首' : fmtLeft(s.sleep.until - now));
  const menu = (e) => openMenu(e, s.sleep ? [
    s.sleep.until && { icon: 'add', label: '延長 15 分鐘', onClick: () => extendSleep(15) },
    { icon: 'close', label: '取消睡眠定時', onClick: () => { cancelSleep(); toast('已取消睡眠定時'); } },
  ] : [
    ...[15, 30, 60, 90].map((m) => ({ icon: 'bedtime', label: `${m} 分鐘後停止`, onClick: () => setSleep(m) })),
    { icon: 'edit', label: '自訂時間…', onClick: () => showDialog((close) => html`<${SleepDialog} close=${close} />`) },
    { icon: 'next', label: '播完這首後停止', onClick: sleepAfterTrack },
  ]);
  return html`<button class=${'btn ' + (s.sleep ? 'tonal' : 'text') + (compact ? ' sleep-chip' : '')} onClick=${menu}
    aria-label=${s.sleep ? `睡眠定時：${label}` : '睡眠定時'}><${Icon} name="bedtime" />${compact ? short && html`<span class="sleep-left">${short}</span>` : label}</button>`;
}

function SleepDialog({ close }) {
  const [min, setMin] = useState('45');
  const n = Number(min);
  const ok = Number.isFinite(n) && n >= 1 && n <= 720;
  const submit = () => { setSleep(n); close(); };
  return html`<${Dialog} title="睡眠定時" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${!ok} onClick=${submit}>開始</button>`}>
    <label class="field">幾分鐘後停止<input type="number" min="1" max="720" value=${min} onInput=${(e) => setMin(e.target.value)} /></label>
    <p class="hint">時間到會暫停並記住播放位置，最後 20 秒音量會漸弱（不改你的音量設定）。定時只在這台裝置有效，關閉網頁就取消。</p>
  <//>`;
}

const bookmarksRev = createStore({ n: 0 });

function addBookmark(item) {
  const at = Math.round((player.get().scrub ?? now()) * 1000);
  showDialog((close) => html`<${BookmarkDialog} close=${close} item=${item} at=${at}
    onSaved=${() => { bookmarksRev.set((v) => ({ n: v.n + 1 })); panel.set({ tab: 'bookmarks' }); }} />`);
}

// TrackBookmarks lists the playing song's bookmarks.
function TrackBookmarks({ item }) {
  const { n } = useStore(bookmarksRev);
  const data = useLoad(() => (item.trackId ? get('/bookmarks?track=' + item.trackId) : Promise.resolve([])), [item.trackId], n);
  if (data.loading && !data.data) return html`<${Spinner} />`;
  if (data.error) return html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`;
  return html`<div class="np-bookmarks">
    ${data.data.length ? html`<${BookmarkList} items=${data.data} onChanged=${data.reload} />`
      : html`<${Empty} icon="bookmark">這首還沒有書籤。播放到想記住的地方，按「加書籤」。<//>`}
    <a class="btn text" href=${href('bookmarks')} onClick=${() => open(false)}>所有書籤 ›</a>
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
  const libraryShuffle = s.shuffle && s.scope === 'library';
  const going = !s.radio ? '' : s.radio.playlist ? `依智慧歌單「${s.radio.name}」的條件一直挑歌接著播`
    : s.radio.chosen || libraryShuffle ? '全曲庫隨機：會一直從曲庫挑歌接著播' : '已播完佇列，正從曲庫隨機挑歌接著播';
  return html`<div class="queue">
    <div class="queue-head">
      <span class="sub grow">${s.queue.length} 首 · ${libraryShuffle ? '全曲庫隨機' : modeLook[s.mode][1]}</span>
      <button class="btn text" disabled=${s.index >= last} onClick=${clearUpcoming}>清除待播</button>
      <button class="btn text" onClick=${() => resetPlayer()}>停止並清空</button>
    </div>
    <label class="toggle-row queue-toggle">
      <span class="grow"><span class="title">播完後自動接續</span>
        <span class="sub">${going || (s.mode === 'all' ? '列表循環中：播完會從頭再播一次' : s.mode === 'one' ? '單曲循環中' : '佇列播完後，從曲庫隨機挑歌接著播')}</span></span>
      <input type="checkbox" role="switch" checked=${s.autoContinue} onChange=${(e) => setPrefs({ autoContinue: e.target.checked })} />
    </label>
    ${s.radioError && html`<div class="error-box" role="alert"><span>沒能${s.radio && s.radio.playlist ? '依條件' : '從曲庫'}挑歌接著播：${s.radioError}</span>
      <button class="btn text" onClick=${retryRadio}>重試</button></div>`}
    <ol class=${'tracks queue-list' + (drag ? ' dragging' : '')}>${s.queue.map((q, i) => html`<li key=${q.qid || i} ...${lift(i)}>
      <div class=${'track' + (q.qid === current ? ' current' : '')}>
        <${DragHandle} onStart=${(e) => start(e, i)} label=${`拖曳「${q.title}」改變播放順序`} />
        <button class="track-main" onPointerDown=${(e) => press(e, i)} onClick=${() => playAt(i)} aria-current=${q.qid === current ? 'true' : undefined}>
          <span class="num">${i + 1}</span>
          <span class="track-text"><span class="title">${q.title}${q.auto ? html` <span class="pill">自動</span>` : ''}</span><span class="sub">${q.artist}</span></span>
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
const foundCache = new Map(); // track ID and search -> candidates
// Exact matches stored by themselves, once a page (track?search#record -> trying, done or failed):
// one that failed waits in the list for the listener, and one stored is never stored again.
const autoTried = new Map();

// findLyrics asks LRCLIB for a song's lyrics; query is a search adjusted by hand ("title=…&artist=…"
// or "q=…"; empty: the song's own title and artist), which keys its answer (review #122).
function findLyrics(trackId, query = '') {
  const key = trackId + '?' + query;
  if (foundCache.has(key)) return Promise.resolve(foundCache.get(key));
  // 503 with Kanade's own answer: LRCLIB, not this server, failed.
  const said = (e) => (e.status === 503 && e.body ? new ApiError(503, 'LRCLIB 暫時沒有回應，請稍後再試。', e.body, e.retryAfter) : e);
  return get(`/tracks/${trackId}/lyrics/online${query ? '?' + query : ''}`).catch((e) => (e.status === 404 ? [] : Promise.reject(said(e)))).then((list) => {
    foundCache.set(key, list);
    return list;
  });
}

async function applyFound(trackId, id, auto = false) {
  const r = await post(`/tracks/${trackId}/lyrics/online`, { id, auto });
  await lyricsChanged(trackId);
  return r.saved;
}

// lyricsChanged fetches a song's lyrics after they changed, and only then has the open views show
// them: the lyrics tab goes from what it showed straight to the new lyrics (review #133).
async function lyricsChanged(trackId) {
  lyricsCache.delete(trackId);
  await loadLyrics(trackId).catch(() => {}); // a failure shows where they are loaded again
  lyricsRev.set((v) => ({ n: v.n + 1 }));
}

// LyricsWait keeps about the room the lyrics take while they load or are looked for, so the page
// does not shrink under the tabs and jump (review #132).
const LyricsWait = ({ label }) => html`<div class="lyrics-wait sub"><${Spinner} />${label}</div>`;

const fmtSec = (sec) => fmtTime(Math.round(sec) * 1000);

// FoundLyrics lists what LRCLIB has for a song (query: a search adjusted by hand); choosing one
// stores it. With auto, an exact match (same title and artist, length within two seconds) is stored
// right away, where the song has no lyrics yet: looking, storing and showing what was stored is one
// wait, with no list to choose from in between (review #133). onAdjust offers to adjust the search;
// frame puts what is shown at last (not the wait) in the lyrics tab's own words.
function FoundLyrics({ item, auto, onChosen, query = '', onAdjust, frame = (body) => body }) {
  const key = item.trackId + '?' + query;
  const loaded = useLoad(() => findLyrics(item.trackId, query), [item.trackId, query]);
  const data = loaded.loading && foundCache.has(key) ? { ...loaded, loading: false, data: foundCache.get(key) } : loaded;
  const [busy, setBusy] = useState(0);
  const [, setTried] = useState(0);
  const retry = () => { foundCache.delete(key); data.reload(); };
  // The server asked to wait (Retry-After): asked once more by itself after that, at most twice for
  // a song; leaving the song or the panel cancels it (review #79).
  const tries = useRef({ track: 0, n: 0 });
  useEffect(() => {
    const e = data.error;
    if (tries.current.track !== item.trackId) tries.current = { track: item.trackId, n: 0 };
    if (!e || !e.retryAfter || tries.current.n >= 2) return;
    const t = setTimeout(() => { tries.current.n++; retry(); }, e.retryAfter * 1000);
    return () => clearTimeout(t);
  }, [data.error]);
  const list = data.data || [];
  const best = list[0];
  const at = best && key + '#' + best.id;
  const tried = autoTried.get(at);
  const pick = auto && !data.loading && best && best.exact && !best.instrumental && tried !== 'done' && tried !== 'failed' ? best : null;
  useEffect(() => {
    if (!pick || autoTried.has(at)) return;
    let alive = true;
    autoTried.set(at, 'trying');
    applyFound(item.trackId, pick.id, true).then(() => autoTried.set(at, 'done'), (e) => {
      autoTried.set(at, 'failed'); // shown in the list, to choose again
      toast(e.message, 'error');
    }).then(() => alive && setTried((n) => n + 1));
    return () => { alive = false; };
  }, [pick && at]);
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
  if (data.loading || pick) {
    return auto ? html`<${LyricsWait} label="正在 LRCLIB 尋找歌詞…" />` : html`<div class="found-wait sub"><${Spinner} />正在 LRCLIB 尋找歌詞…</div>`;
  }
  const adjust = onAdjust && html`<div class="actions center"><button class="btn text" onClick=${onAdjust}><${Icon} name="search" />調整搜尋</button></div>`;
  if (data.error) {
    return frame(html`<p class="sub found-none">線上歌詞暫時查不到，不代表 LRCLIB 沒有這首歌的歌詞。</p>
      <${ErrorBox} error=${data.error} onRetry=${retry} />${adjust}`);
  }
  if (!list.length) {
    return frame(html`<p class="sub found-none">${query ? '這樣搜尋沒有找到歌詞，可以換個曲名、歌手或關鍵字。' : 'LRCLIB 沒有找到吻合這首歌標題的歌詞；改用其他曲名、歌手或關鍵字也許找得到。'}</p>${adjust}`);
  }
  return frame(html`<div class="found">
    <p class="sub">LRCLIB 找到 ${list.length} 個可能的歌詞，選一個套用：</p>
    <ul class="found-list">${list.map((c) => html`<li key=${c.id}><button class="found-item" disabled=${busy !== 0} onClick=${() => choose(c)}>
      <span class="title">${c.title}${c.exact ? html` <span class="pill good">吻合</span>` : ''}</span>
      <span class="sub">${[c.artist, c.album, c.duration ? fmtSec(c.duration) : '', c.instrumental ? '純音樂' : c.synced ? '逐行同步' : '純文字']
        .filter(Boolean).join(' · ')}</span>
      ${c.preview && html`<span class="found-preview">${c.preview}</span>`}
    </button></li>`)}</ul>
  </div>`);
}

// parseLRC reads [mm:ss.xx] lines (several tags on one line repeat it) and [offset:±ms];
// word-level <mm:ss.xx> tags of enhanced LRC are dropped.
// lineAt is the last line started by ms (-1 before the first).
function lineAt(lines, ms) {
  let at = -1;
  for (let i = 0; i < lines.length && lines[i].t <= ms; i++) at = i;
  return at;
}

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

function Lyrics({ item }) {
  const rev = useStore(lyricsRev, (s) => s.n);
  const loaded = useLoad(() => loadLyrics(item.trackId), [item.trackId, rev]);
  // Lyrics this page has already are shown at once, with no wait in between (review #132).
  const data = loaded.loading && lyricsCache.has(item.trackId) ? { ...loaded, loading: false, data: lyricsCache.get(item.trackId) } : loaded;
  const lines = useMemo(() => (data.data && data.data.synced ? parseLRC(data.data.text) : []), [data.data]);
  const box = useRef(null);
  const userScrolled = useRef(0);
  // The line now: this follows the clock, and changes a few times a minute (review #158).
  const at = useStore(clock, (c) => lineAt(lines, c.time * 1000));

  useEffect(() => { // keep the current line in the middle, unless the listener is scrolling
    const el = box.current;
    if (!el || at < 0 || Date.now() - userScrolled.current < 4000) return;
    const li = el.children[at];
    if (li) el.scrollTo({ top: li.offsetTop - el.clientHeight / 2 + li.clientHeight / 2, behavior: 'smooth' });
  }, [at]);

  if (!item.trackId) return html`<${Empty} icon="lyrics">這首歌沒有歌詞。<//>`;
  if (data.loading) return html`<${LyricsWait} label="正在載入歌詞…" />`;
  if (data.error) return html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`;
  if (!data.data) { // none of its own: LRCLIB is asked (not for drama and radio, rarely there)
    // "No lyrics" is said once LRCLIB has answered too, not while it is asked (review #133).
    const none = (found) => html`<div class="lyrics-none">
      <${Empty} icon="lyrics">這首歌沒有歌詞。<//>
      ${found}
      <div class="actions center"><button class="btn tonal" onClick=${() => editLyrics(item)}>自己輸入歌詞</button></div>
    </div>`;
    if (item.kind === 'spoken' && !lookFor.has(item.trackId)) {
      return none(html`<div class="actions center"><button class="btn text" onClick=${() => { lookFor.add(item.trackId); lyricsRev.set((v) => ({ n: v.n + 1 })); }}>在 LRCLIB 尋找</button></div>`);
    }
    return html`<${FoundLyrics} item=${item} auto=${item.kind !== 'spoken'} onAdjust=${() => editLyrics(item, true)} frame=${none} />`;
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

// AdjustSearch changes what LRCLIB is asked: another title and artist, or keywords, which LRCLIB
// matches across title, artist and album (review #122). The song itself is not changed.
function AdjustSearch({ item, onSearch }) {
  const [title, setTitle] = useState(item.title || '');
  const [artist, setArtist] = useState(item.artist || '');
  const [q, setQ] = useState('');
  const search = (e) => {
    e.preventDefault();
    const p = new URLSearchParams();
    if (q.trim()) p.set('q', q.trim());
    else if (title.trim() !== (item.title || '').trim() || artist.trim() !== (item.artist || '').trim()) {
      p.set('title', title.trim());
      p.set('artist', artist.trim());
    }
    onSearch(p.toString());
  };
  return html`<form class="adjust-search" onSubmit=${search}>
    <div class="form-grid">
      <${Field} label="曲名" value=${title} onInput=${setTitle} />
      <${Field} label="歌手" value=${artist} onInput=${setArtist} />
    </div>
    <${Field} label="或用關鍵字（填了就只用關鍵字搜尋）" value=${q} onInput=${setQ} placeholder="例如：ユーフォリア 牧野由依" />
    <div class="actions"><button class="btn tonal" type="submit" disabled=${!q.trim() && !title.trim()}><${Icon} name="search" />搜尋</button></div>
  </form>`;
}

function editLyrics(item, online = false) {
  showDialog((close) => html`<${LyricsEditor} item=${item} close=${close} online=${online} />`);
}

function LyricsEditor({ item, close, online: startOnline }) {
  const data = useLoad(() => loadLyrics(item.trackId), [item.trackId]);
  const [online, setOnline] = useState(!!startOnline);
  const [query, setQuery] = useState('');
  const [text, setText] = useState(null);
  const [busy, setBusy] = useState(false);
  const value = text ?? (data.data ? data.data.text : '');
  const save = async (body) => {
    setBusy(true);
    try {
      if (body) await api('PUT', `/tracks/${item.trackId}/lyrics`, { text: body });
      else await api('DELETE', `/tracks/${item.trackId}/lyrics`);
      await lyricsChanged(item.trackId);
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
      <p class="hint">只會把下面的曲名和歌手（或關鍵字）送到 LRCLIB（lrclib.net）查詢。選擇的歌詞會取代目前的歌詞。</p>
      <${AdjustSearch} item=${item} onSearch=${setQuery} />
      <${FoundLyrics} item=${item} onChosen=${close} query=${query} />
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

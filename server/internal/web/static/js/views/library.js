import { useCallback, useEffect, useRef, useState } from '../../vendor/hooks.module.js';
import { get } from '../api.js';
import { addToPlaylist, toggleFav, useFav } from '../actions.js';
import { clock, enqueue, fromEntry, fromTrack, playLibraryShuffle, playNext, playQueue, player, shuffled, toggle } from '../player.js';
import { driveOnly, places } from '../places.js';
import { go, href, keepInAddress, parseHash } from '../router.js';
import { useStore } from '../store.js';
import { usePageTitle } from '../site.js';
import { control } from '../remote.js';
import { Cover, Empty, ErrorBox, Icon, IconButton, Spinner, fmtBytes, fmtTime, html, openMenu, showDialog, toast, useLoad } from '../ui.js';
import { AlbumActions, Categorize, SongActions, editSections } from './batch.js';
import { CategoriesTab } from './categories.js';
import { WorkImage, WorksTab, bindSubject, linkWorks, trackWorks, useTags } from './works.js';
import { SubjectChips } from './bangumi.js';
import { SelectBar, SelectToggle, useSelection } from '../selection.js';
import { AlbumGrid, TrackList, playInAlbum } from './common.js';
import { addDownload } from './tasks.js';
import { viewCover } from './coverview.js';
import { moveFiles } from './moves.js';
import { AlbumGallery, openGallery, uploadImages } from './gallery.js';
import { changeCover, editAlbum, editArtistAliases, folderAlbums, mergeArtist, identifyAlbum, mergeAlbum, removeAlbum, removeFromAlbum, renameArtist,
  restoreAlbum, splitAlbum, useLibRev, vgmdbAlbum } from './organize.js';

// Albums in one horizontally scrolling row (home page shelves).
function Shelf({ title, albums, more }) {
  if (!albums || !albums.length) return null;
  return html`<div class="section-head"><h2 class="section-title">${title}</h2>${more}</div>
    <div class="shelf">${albums.map((a) => html`<a key=${a.id} class="card album-card" href=${href('album/' + a.id)}>
      <${Cover} id=${a.cover_id} alt="" />
      <div class="card-text"><div class="title" title=${a.title}>${a.title}</div><div class="sub" title=${a.album_artist || ''}>${a.sub || a.album_artist || '未知歌手'}</div></div>
    </a>`)}</div>`;
}

// The top card: what is playing in this tab, or else the latest playback, from any device.
function NowCard() {
  const s = useStore(player);
  const item = s.queue[s.index];
  return html`<div class="card resume-card">
    <button class="resume-open" onClick=${() => player.set({ nowPlayingOpen: true })} aria-label="開啟正在播放">
      <${Cover} id=${item.coverId} size=${300} className="resume-cover" />
      <span class="resume-text">
        <span class="overline">${s.playing || s.buffering ? '正在播放' : '已暫停'}</span>
        <span class="title">${item.title}</span>
        <span class="sub">${[item.artist, item.album].filter(Boolean).join(' · ')}</span>
        <${NowProgress} duration=${s.duration} />
      </span>
    </button>
    <${IconButton} icon=${s.playing ? 'pause' : 'play'} label=${s.playing ? '暫停' : '播放'} filled size=${28} onClick=${toggle} />
  </div>`;
}

// NowProgress follows the clock by itself, so the card is not drawn again every tick (review #158).
function NowProgress({ duration }) {
  const time = useStore(clock, (c) => c.time);
  return html`<${ProgressLine} position=${time} duration=${duration} />
    <span class="sub">${fmtTime(time * 1000)} / ${fmtTime(duration * 1000)}</span>`;
}

function LastPlayedCard({ item }) {
  const dur = item.asset.duration_ms;
  const action = !item.finished ? (item.kind === 'spoken' ? '繼續收聽' : '繼續播放') : item.album_id ? '播放下一首' : '重新播放';
  return html`<div class="card resume-card">
    <${Cover} id=${item.cover_id} size=${300} className="resume-cover" />
    <div class="resume-text">
      <div class="overline">${item.finished ? '上次聽完' : action}</div>
      <div class="title">${item.title}</div>
      <div class="sub">${[item.artist, item.album].filter(Boolean).join(' · ')}</div>
      <${ProgressLine} position=${item.finished ? dur : item.position_ms} duration=${dur} />
      <div class="sub">${item.finished ? fmtTime(dur) : `${fmtTime(item.position_ms)} / ${fmtTime(dur)}`}</div>
    </div>
    <${IconButton} icon=${item.finished && item.album_id ? 'next' : 'play'} label=${action} filled size=${28} onClick=${() => playInAlbum(item)} />
  </div>`;
}

async function playRandomAlbum() {
  try {
    const { id } = await get('/albums/random');
    const a = await get('/albums/' + id);
    playQueue(a.entries.map((e) => fromEntry(e, a)), 0); // stay on this page; the album is a tap away in the player
    toast(`正在播放：${a.title}`, 'info', { label: '前往專輯', onClick: () => go('album/' + id) });
  } catch (e) {
    toast(e.status === 404 ? '曲庫還沒有專輯' : e.message, 'error');
  }
}

function ProgressLine({ position, duration }) {
  const pct = duration ? Math.min(position / duration, 1) * 100 : 0;
  return html`<div class="progress thin"><div style=${{ width: pct + '%' }}></div></div>`;
}

const downloadLabels = { metadata: '取得清單', selecting: '等待選檔', queued: '排隊', downloading: '下載中', paused: '已暫停', seeding: '做種中' };

export function Home() {
  const home = useLoad(() => get('/home'), [], undefined, 'home');
  const d = home.data;
  const local = useStore(player, (s) => s.queue[s.index]); // re-renders on track change, not on every tick
  // Controlling another device (review #138): what is paused here is not what the buttons play.
  const live = useStore(control, (c) => !c.target) ? local : null;
  const busy = d && (Object.keys(d.tasks.downloads).length > 0 || d.tasks.importing > 0);
  const att = d && d.attention;
  useStore(places, (v) => v.stores.length); // how a missing file is worded
  const empty = d && !d.recently_added.length;
  // The track in the top card is not repeated in the drama list below it. The card waits for the
  // page's data like everything else, though the player is back at once after a reload.
  const shown = live ? live.assetId : d && d.continue ? d.continue.asset.id : 0;
  const spoken = d ? d.spoken.filter((t) => t.asset.id !== shown) : [];
  return html`<section>
    <div class="page-head">
      <h1 class="page-title">首頁</h1>
      ${d && !empty && html`<div class="actions">
        <button class="btn tonal" onClick=${() => playLibraryShuffle()}><${Icon} name="shuffle" />全曲庫隨機播放</button>
        <button class="btn tonal" onClick=${playRandomAlbum}><${Icon} name="album" />隨便聽一張</button>
      </div>`}
    </div>
    ${home.loading && !d ? html`<${Spinner} />` : html`<${ErrorBox} error=${home.error} onRetry=${home.reload} />`}
    ${(d || home.error) && (live ? html`<${NowCard} />` : d && d.continue && html`<${LastPlayedCard} item=${d.continue} />`)}
    ${(busy || (att && att.failed_imports > 0)) && html`<a class="card summary-card" href=${href('tasks')}>
      <${Icon} name="tasks" />
      <span class="grow">
        ${Object.entries(d.tasks.downloads).map(([k, n]) => html`<span class="pill">${downloadLabels[k] || k} ${n}</span>`)}
        ${d.tasks.importing > 0 && html`<span class="pill">匯入中 ${d.tasks.importing}</span>`}
        ${att.failed_imports > 0 && html`<span class="pill warn">匯入失敗 ${att.failed_imports}</span>`}
      </span>
      <span class="sub">任務 ›</span>
    </a>`}
    ${empty && html`<${Empty} icon="library">曲庫還是空的，先加入一些音樂吧。
      <div class="actions center">
        <a class="btn filled" href=${href('upload')}><${Icon} name="upload" />上傳音樂</a>
        <button class="btn tonal" onClick=${addDownload}><${Icon} name="download" />新增下載</button>
      </div><//>`}
    ${d && html`<${Shelf} title="最近播放" albums=${d.recently_played}
      more=${html`<a class="btn text" href=${href('me/history')}><${Icon} name="history" />播放記錄</a>`} />`}
    ${spoken.length > 0 && html`<h2 class="section-title">未聽完的廣播劇</h2>
      <ul class="list spoken-list">${spoken.map((t) => html`<li key=${t.id}><button class="row plain wide spoken-row" onClick=${() => playInAlbum(t)}>
        <${Cover} id=${t.cover_id} size=${96} className="thumb" />
        <span class="track-text"><span class="title">${t.title}</span>
          <span class=${'sub' + (t.album || t.artist ? '' : ' missing')}>${t.album || t.artist || '沒有專輯'}</span>
          <${ProgressLine} position=${t.position_ms} duration=${t.asset.duration_ms} /></span>
        <span class="sub left">剩 ${fmtTime(Math.max(t.asset.duration_ms - t.position_ms, 0))}</span>
      </button></li>`)}</ul>`}
    ${d && html`<${Shelf} title="最近加入" albums=${d.recently_added} />`}
    ${att && (att.without_album > 0 || att.unknown_artist > 0 || att.missing > 0) && html`<h2 class="section-title">待整理</h2>
      <a class="card summary-card" href=${href('organize')}>
        <${Icon} name="note" />
        <span class="grow">
          ${att.without_album > 0 && html`<span class="pill">沒有專輯的歌曲 ${att.without_album}</span>`}
          ${att.unknown_artist > 0 && html`<span class="pill">沒有歌手 ${att.unknown_artist}</span>`}
          ${att.missing > 0 && html`<span class="pill warn">${driveOnly() ? 'Drive 中遺失' : '檔案遺失'} ${att.missing}</span>`}
        </span>
        <span class="sub">整理 ›</span>
      </a>`}
  </section>`;
}

// The library is the music itself; what is one's own (playlists, favorites, Bangumi collections)
// is on "my" page (review #190).
const tabs = [['albums', '專輯'], ['categories', '分類'], ['works', '作品'], ['artists', '歌手'], ['tracks', '歌曲']];
const tabURL = { categories: '/categories', works: '/works' };

const PAGE = 200;
// The later pages of the lists, by list and library version: going back to one shows as much of it
// as was loaded, where it was left (review 2.1).
const pagesKept = new Map();

// LoadMore loads the next page when it scrolls into view, or when tapped.
function LoadMore({ onMore, busy }) {
  const ref = useRef(null);
  useEffect(() => {
    const el = ref.current;
    if (!el || typeof IntersectionObserver === 'undefined') return;
    // The page scrolls inside .content, so that is where to look ahead from.
    const io = new IntersectionObserver((es) => es.some((e) => e.isIntersecting) && onMore(),
      { root: el.closest('.content'), rootMargin: '400px' });
    io.observe(el);
    return () => io.disconnect();
  }, [onMore]);
  return html`<div class="actions center" ref=${ref}>
    <button class="btn tonal" disabled=${busy} onClick=${onMore}>${busy ? '載入中…' : '載入更多'}</button></div>`;
}

// Filters of the songs tab: the songs the home page asks to sort out.
const trackFilters = [['', '全部'], ['no_album', '未分類（沒有專輯）'], ['no_artist', '沒有歌手']];

// The orders of the lists (review 2.2), the first the server's own; the one chosen for each tab is
// kept in this browser.
const orders = {
  albums: [['artist', '專輯歌手'], ['title', '標題'], ['recent', '最近加入'], ['date', '發行日'], ['catalog', '型號']],
  tracks: [['title', '標題'], ['artist', '歌手'], ['recent', '最近加入']],
  artists: [['name', '名稱'], ['tracks', '歌曲數']],
};
const ORDERS = 'kanade.orders';
function readOrders() {
  try {
    return JSON.parse(localStorage.getItem(ORDERS) || '{}') || {};
  } catch {
    return {};
  }
}
const findNoun = { albums: '專輯、歌手或型號', tracks: '歌名或歌手', artists: '歌手' };

export function Library({ tab = 'albums', filter = '', find = '' }) {
  const rev = useLibRev();
  // What to find in the list, kept in the address (going back finds it again), and the order. Each
  // tab is a page of its own (app.js): another starts with what its address says.
  const [typed, setTyped] = useState(find);
  const [kept, setKept] = useState(readOrders);
  useEffect(() => setTyped(find), [filter, find]);
  const term = typed.trim();
  const [later, setLater] = useState(term);
  useEffect(() => {
    const t = setTimeout(() => setLater(term), 250);
    return () => clearTimeout(t);
  }, [term]);
  // While typing the list waits a moment; emptied or as the address says, it changes at once.
  const q = !term || term === find.trim() ? term : later;
  useEffect(() => { // the lists of the server's pages only: the works tab keeps its own address
    if (!paged) return;
    const qs = [filter && 'filter=' + filter, q && 'q=' + encodeURIComponent(q)].filter(Boolean).join('&');
    keepInAddress('library/' + tab + (qs ? '?' + qs : ''));
  }, [q]);
  const order = orders[tab] && orders[tab].some(([k]) => k === kept[tab]) ? kept[tab] : '';
  const setOrder = (k) => {
    const next = { ...readOrders(), [tab]: k };
    try {
      localStorage.setItem(ORDERS, JSON.stringify(next));
    } catch { /* storage blocked: this visit only */ }
    setKept(next);
  };
  const paged = !tabURL[tab];
  if (!tabs.some(([k]) => k === tab)) tab = 'albums';
  // Albums by where their files are (#95): Google Drive or a storage location, by its ID.
  if (tab === 'tracks' ? !trackFilters.some(([k]) => k === filter) : tab !== 'albums' || !/^(drive|\d+)$/.test(filter)) filter = '';
  const where = useStore(places);
  const stores = where.stores;
  // A location removed since the address was made.
  const gone = tab === 'albums' && where.loaded && /^\d+$/.test(filter) && !stores.some((st) => String(st.id) === filter);
  // Google Drive is a place while connected or holding songs (review #232); a menu, not a row of
  // buttons: ten places, or a name of sixty characters, fit a phone as well.
  const placeList = [...(where.drive.connected || where.drive.songs || filter === 'drive' ? [['drive', 'Google Drive']] : []),
    ...stores.map((st) => [String(st.id), st.name])];
  const placeFilters = tab === 'albums' && (placeList.length > 1 || filter) ? [['', '全部'], ...placeList] : [];
  const base = tabURL[tab] || `/${tab}?limit=${PAGE}${filter ? (tab === 'albums' ? '&place=' : '&filter=') + filter : ''}${
    order ? '&sort=' + order : ''}${q ? '&q=' + encodeURIComponent(q) : ''}`;
  const data = useLoad(() => get(base), [base], rev, base);
  // Later pages, for this tab, filter and library version only (review #9).
  const key = `${base}:${rev}`;
  const [more, setMore] = useState(() => pagesKept.get(key) || { key: null, pages: [], done: false, busy: false });
  const own = more.key === key ? more : { key, pages: [], done: false, busy: false };
  useEffect(() => {
    if (more.key && !more.busy) {
      pagesKept.delete(more.key);
      pagesKept.set(more.key, more);
      if (pagesKept.size > 10) pagesKept.delete(pagesKept.keys().next().value);
    }
  }, [more]);
  const list = data.data && paged ? [...data.data, ...own.pages.flat()] : data.data;
  const done = !paged || !data.data || data.data.length < PAGE || own.done;
  const loadMore = useCallback(async () => {
    if (own.busy || done) return;
    setMore({ ...own, busy: true });
    try {
      const next = await get(`${base}&offset=${list.length}`);
      setMore((m) => (m.key === key ? { key, pages: [...m.pages, next], done: next.length < PAGE, busy: false } : m));
    } catch (e) {
      toast(e.message, 'error');
      setMore((m) => ({ ...m, busy: false }));
    }
  }, [key, list && list.length, own.busy, done]);
  // Albums and songs can be selected to act on them at once (review #83); another tab or filter
  // starts over.
  const sel = useSelection(tab + '?' + filter + '&' + q + '&' + order);
  const canSelect = tab === 'albums' || tab === 'tracks';
  const selectable = canSelect && list;
  const keys = selectable ? list.map((x) => x.id) : [];
  return html`<section>
    <div class="page-head">
      <h1 class="page-title">曲庫</h1>
      <div class="actions">
        ${!(tab === 'albums' && !filter && list && !list.length) && html`<button class="btn tonal" onClick=${() => playLibraryShuffle()}>
          <${Icon} name="shuffle" />全曲庫隨機播放</button>`}
        ${canSelect && html`<${SelectToggle} sel=${sel} disabled=${!list || !list.length} />`}
        <a class="btn text" href=${href('organize')}><${Icon} name="edit" />整理</a>
      </div>
    </div>
    <nav class="tabs" aria-label="曲庫">
      ${tabs.map(([k, label]) => html`<a key=${k} aria-current=${k === tab ? 'page' : undefined} class=${k === tab ? 'active' : ''} href=${href('library/' + k)}>${label}</a>`)}
    </nav>
    ${tab === 'tracks' && html`<nav class="seg filter-seg" aria-label="篩選">${trackFilters.map(([k, label]) => html`<a key=${k}
      class=${k === filter ? 'on' : ''} aria-current=${k === filter ? 'page' : null} href=${href('library/tracks' + (k ? '?filter=' + k : ''))}>${label}</a>`)}</nav>`}
    ${paged && html`<div class="list-tools library-tools">
      <input type="search" class="grow" placeholder=${'找' + findNoun[tab]} aria-label=${'在清單中找' + findNoun[tab]} value=${typed}
        onInput=${(e) => setTyped(e.target.value)} onKeyDown=${(e) => e.key === 'Escape' && typed && (e.stopPropagation(), setTyped(''))} />
      <label class="select-field">排序<select value=${order || orders[tab][0][0]} onChange=${(e) => setOrder(e.target.value)}>
        ${orders[tab].map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}</select></label>
      ${placeFilters.length > 0 && html`<label class="select-field place-filter">位置
        <select value=${filter} onChange=${(e) => go('library/albums' + (e.target.value ? '?filter=' + e.target.value : ''))}>
          ${placeFilters.map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}
          ${gone && html`<option value=${filter}>（已移除的位置）</option>`}</select></label>`}
    </div>`}
    ${tab === 'tracks' && filter === 'no_album' && html`<${FolderCard} rev=${rev} />`}
    ${gone ? html`<${Empty} icon="album">這個儲存位置已經移除了。<//>`
      : data.loading && !data.data ? html`<${Spinner} />` : html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`}
    ${list && tab === 'albums' && html`<${AlbumGrid} albums=${list} sel=${sel} empty=${q ? `沒有符合「${q}」的專輯。` : filter ? '這裡沒有專輯。' : undefined} />`}
    ${tab === 'artists' && !q && html`<${SplitCard} rev=${rev} /><${SameCard} rev=${rev} />`}
    ${list && tab === 'artists' && !list.length && q && html`<${Empty} icon="person">沒有符合「${q}」的歌手。<//>`}
    ${list && tab === 'artists' && html`<ul class="list">
      ${list.map((a) => html`<li key=${a.id}><a class="row" href=${href(`artist/${a.id}?name=${encodeURIComponent(a.name)}`)}>
        <span class="avatar"><${Icon} name="person" /></span><span class="grow">${a.name}${a.kind === 'character' && html`<span class="sub"> · 角色</span>`}</span><span class="sub">${a.tracks} 首</span></a></li>`)}
    </ul>`}
    ${list && tab === 'tracks' && (list.length
      ? html`<${TrackList} items=${list.map(fromTrack)} showAlbum sel=${sel} />`
      : html`<${Empty}>${q ? `沒有符合「${q}」的歌。` : filter === 'no_album' ? '每首歌都有專輯了。' : filter === 'no_artist' ? '每首歌都有歌手了。' : '還沒有歌曲。'}<//>`)}
    ${list && !done && html`<${LoadMore} onMore=${loadMore} busy=${own.busy} />`}
    ${data.data && tab === 'categories' && html`<${CategoriesTab} data=${data.data} />`}
    ${data.data && tab === 'works' && html`<${WorksTab} data=${data.data} />`}
    ${selectable && html`<${SelectBar} sel=${sel} noun=${tab === 'albums' ? '張' : '首'} loaded=${keys} more=${!done}
      items=${tab === 'albums' ? list : list.map(fromTrack)}>
      ${tab === 'albums' ? html`<${AlbumActions} sel=${sel} />` : html`<${SongActions} sel=${sel} />`}
    <//>`}
  </section>`;
}

// FolderCard offers albums for songs whose folder names the album though their tags name none.
function FolderCard({ rev }) {
  const groups = useLoad(() => get('/organize/folders'), [], rev);
  const g = groups.data;
  if (!g || !g.length) return null;
  const songs = g.reduce((n, x) => n + x.tracks.length, 0);
  return html`<div class="card summary-card folder-card">
    <${Icon} name="album" />
    <span class="grow track-text"><span class="title">${songs} 首可以依資料夾歸入專輯</span>
      <span class="sub">${g.slice(0, 3).map((x) => x.title).join('、')}${g.length > 3 ? ` 等 ${g.length} 張` : ''}</span></span>
    <button class="btn tonal" onClick=${() => folderAlbums(g)}>依資料夾整理…</button>
  </div>`;
}

// belongs are the ways to say where an album belongs: its categories, the works (anime, games) it
// is music of, and its own entry on Bangumi.
const belongs = (a) => [
  { icon: 'folder', label: a.categories && a.categories.length ? '分類…' : '加入分類…',
    onClick: () => showDialog((close) => html`<${Categorize} ids=${[a.id]} close=${close} />`) },
  { icon: 'work', label: '動畫、遊戲作品…', onClick: () => linkWorks(a) },
  { icon: 'note', label: a.subject ? '改綁 Bangumi 條目…' : '綁定 Bangumi 條目…', onClick: () => bindSubject(a) },
];

export function Album({ id }) {
  const rev = useLibRev();
  const album = useLoad(() => get('/albums/' + id), [id], rev, 'album/' + id);
  const sel = useSelection(id); // songs of this album, selected to act on at once (review #83)
  usePageTitle(album.data && album.data.title);
  if (album.loading && !album.data) return html`<${Spinner} />`;
  if (album.error) return html`<${ErrorBox} error=${album.error} onRetry=${album.reload} />`;
  const a = album.data;
  if (!a.entries.length) return html`<${EmptyAlbum} album=${a} />`;
  const items = a.entries.map((e) => ({ ...fromEntry(e, a), number: e.track_no || '', key: 'e' + e.entry_id, disc: e.disc_no, entryId: e.entry_id,
    uses: e.works || [], tags: useTags(e.works, a.works) }));
  const discs = [...new Set(items.map((i) => i.disc))];
  const named = Object.keys(a.sections || {}).length > 0;
  // The album's own menu, in groups: playing it, what it says, its pictures, where it belongs and
  // how it is put together, and removing it last (review 2.4).
  const menu = (e) => openMenu(e, [
    { icon: 'playNext', label: '下一首播放', onClick: () => playNext(items) },
    { icon: 'queue', label: '加入佇列', onClick: () => enqueue(items) },
    { icon: 'playlistAdd', label: '加入歌單…', onClick: () => addToPlaylist(items) },
    !sel.on && { icon: 'check', label: '選取歌曲', onClick: sel.start },
    '-',
    { icon: 'edit', label: '編輯專輯資訊…', onClick: () => editAlbum(a) },
    { icon: 'order', label: '區段名稱…', onClick: () => editSections(a) },
    { icon: 'identify', label: '從 MusicBrainz 辨識…', onClick: () => identifyAlbum(a) },
    { icon: 'identify', label: '從 VGMdb 匯入…', onClick: () => vgmdbAlbum(a) },
    a.original && { icon: 'restore', label: '恢復原標籤…', onClick: () => restoreAlbum(a) },
    '-',
    a.cover_id && { icon: 'image', label: '檢視封面', onClick: () => viewCover(a.cover_id, a.title) },
    { icon: 'image', label: a.cover_id ? '更換封面…' : '加上封面…', onClick: () => changeCover(a) },
    a.images > 0 && { icon: 'image', label: `檢視內頁圖（${a.images} 張）`, onClick: () => openGallery(a) },
    { icon: 'upload', label: '上傳內頁圖…', onClick: () => uploadImages(a) },
    '-',
    ...belongs(a),
    '-',
    { icon: 'merge', label: '合併到其他專輯…', onClick: () => mergeAlbum(a) },
    a.entries.length > 1 && { icon: 'split', label: '拆分…', onClick: () => splitAlbum(a) },
    !driveOnly() && { icon: 'move', label: '搬移檔案到…', onClick: () => moveFiles({ albums: [a.id], title: a.title }) },
    '-',
    { icon: 'delete', label: '移除專輯…', onClick: () => removeAlbum(a), danger: true },
  ]);
  const cats = a.categories || [], works = a.works || [];
  return html`<section>
    <header class="album-head">
      ${a.cover_id
        ? html`<button class="cover-open" onClick=${() => viewCover(a.cover_id, a.title)} aria-label="檢視封面" title="檢視封面">
            <${Cover} id=${a.cover_id} size=${600} alt=${a.title} className="big" /></button>`
        : html`<button class="cover-open add" onClick=${() => changeCover(a)} aria-label="加上封面" title="加上封面">
            <${Cover} id=${0} alt=${a.title} className="big" /></button>`}
      <div class="album-info">
        <div class="album-titles">
          <div class="overline">專輯${a.edition ? ` · ${a.edition}` : ''}</div>
          <h1>${a.title}</h1>
          <div class="sub">${[a.album_artist, a.date, a.catalog].filter(Boolean).join(' · ')}</div>
          <div class="sub">${a.tracks} 首 · ${fmtTime(a.duration_ms)}</div>
        </div>
        ${/* Where it belongs, as chips when it does; one way in to set any of it (review 2.5). */ ''}
        <div class="chips album-categories">
          ${cats.map((c) => html`<a key=${'c' + c.id} class="chip" href=${href('category/' + c.id)}><${Icon} name="folder" size=${16} />${c.name}</a>`)}
          ${works.map((w) => html`<a key=${'w' + w.id} class="chip work-chip" href=${href('work/' + w.id)} title=${[w.name_cn, w.date].filter(Boolean).join(' · ')}>
            <${WorkImage} id=${w.image ? w.id : 0} size=${96} />${w.name}</a>`)}
          <${SubjectChips} album=${a} />
          <button class="chip ghost" onClick=${(e) => openMenu(e, belongs(a))} aria-label="分類、作品與 Bangumi 條目">
            <${Icon} name=${cats.length || works.length || a.subject ? 'edit' : 'add'} size=${16} />${cats.length || works.length || a.subject ? '修改…' : '分類、作品…'}</button>
        </div>
        <div class="actions">
          <button class="btn filled" onClick=${() => playQueue(items, 0)}><${Icon} name="play" />播放</button>
          <button class="btn tonal" onClick=${() => playQueue(shuffled(items), 0)}><${Icon} name="shuffle" />隨機播放</button>
          <${AlbumFavorite} id=${a.id} />
          <${IconButton} icon="more" label="更多" onClick=${menu} />
          ${sel.on && html`<${SelectToggle} sel=${sel} />`}
        </div>
      </div>
    </header>
    ${discs.map((d) => html`<div key=${d}>
      ${(discs.length > 1 || named) && html`<h2 class="section-title">${(a.sections && a.sections[d]) || `Disc ${d}`}</h2>`}
      <${TrackList} items=${items.filter((i) => i.disc === d)} queue=${items} showNumber sel=${sel} selKey=${(it) => it.entryId}
        menuExtra=${(it) => ['-', { icon: 'work', label: '作品用途…', onClick: () => trackWorks(it, a) },
          { icon: 'delete', label: '從專輯移除', onClick: () => removeFromAlbum(a, it), danger: true }]} />
    </div>`)}
    <${SelectBar} sel=${sel} noun="首" loaded=${items.map((i) => i.entryId)} items=${items}>
      <${SongActions} sel=${sel} album=${a} />
    <//>
    <${AlbumGallery} album=${a} />
    ${a.sidecars.length > 0 && html`<h2 class="section-title">附屬檔案</h2>
      <ul class="items">${a.sidecars.map((c) => html`<li key=${c.id}>
        <span class="grow path">${c.name}</span>
        <span class="sub">${c.kind === 'cue' ? 'CUE' : c.kind === 'log' ? '翻錄紀錄' : c.kind} · ${fmtBytes(c.size)}</span>
        <a class="btn text" href=${'/api/v1/sidecars/' + c.id} download=${c.name}><${Icon} name="download" />下載</a>
      </li>`)}</ul>`}
  </section>`;
}

// An album with no songs left: merged into another one, or removed. It stays so that undo can
// fill it again.
function EmptyAlbum({ album }) {
  return html`<section>
    <h1 class="page-title">${album.title}</h1>
    <${Empty} icon="album">${album.merged_into
      ? html`這張專輯已合併到<a class="link" href=${href('album/' + album.merged_into)}>另一張專輯</a>。`
      : '這張專輯已經沒有歌曲。'}
      <div class="actions center">
        ${album.original && html`<button class="btn tonal" onClick=${() => restoreAlbum(album)}>恢復原分組</button>`}
        <a class="btn text" href=${href('edits')}>修改紀錄</a>
      </div><//>
  </section>`;
}

function AlbumFavorite({ id }) {
  const on = useFav('album', id);
  return html`<${IconButton} icon=${on ? 'favorite' : 'favoriteOff'} label=${on ? '取消收藏專輯' : '收藏專輯'} pressed=${on}
    className=${on ? 'fav-on' : ''} onClick=${() => toggleFav('album', id)} />`;
}

// The other roles an artist is credited in, as its page heads them (D13).
const roleTitles = { voice: '配音演唱', composer: '作曲', lyricist: '作詞', arranger: '編曲' };

// PeopleLine links the artists paired with this one: who voiced a character, the characters one voiced.
function PeopleLine({ label, list }) {
  if (!list || !list.length) return null;
  return html`<div class="sub alias-line">${label}：${list.map((p, i) => html`${i ? '、' : ''}<a class="link"
    href=${href(`artist/${p.id}?name=${encodeURIComponent(p.name)}`)}>${p.name}</a>`)}</div>`;
}

export function Artist({ id, name }) {
  const rev = useLibRev();
  const artist = useLoad(() => get('/artists/' + id), [id], rev, 'artist/' + id);
  const a = artist.data;
  usePageTitle(a ? a.name : name);
  const sung = a ? a.items.map(fromTrack) : [];
  // A voice's songs say which character it sang as.
  const roles = a ? (a.roles || []).map((r) => ({ ...r, items: r.items.map((t) => ({ ...fromTrack(t), key: r.role + t.id,
    tags: t.as ? [{ label: t.as, title: '飾演的角色' }] : undefined })) })) : [];
  const every = [...sung, ...roles.flatMap((r) => r.items)].filter((t, i, all) => all.findIndex((x) => x.trackId === t.trackId) === i);
  // The albums the artist is on, those with the most of their songs first (review 2.7).
  const albums = [];
  for (const t of every) {
    if (!t.albumId) continue;
    const al = albums.find((x) => x.id === t.albumId);
    if (al) al.n++;
    else albums.push({ id: t.albumId, title: t.album, cover_id: t.coverId, n: 1 });
  }
  albums.sort((x, y) => y.n - x.n);
  for (const al of albums) al.sub = `${al.n} 首`;
  return html`<section>
    <div class="page-head">
      <div class="grow">
        ${a && a.kind === 'character' && html`<div class="overline">角色</div>`}
        <h1 class="page-title">${a ? a.name : name || '歌手'}</h1>
        ${a && a.aliases.length > 0 && html`<div class="sub alias-line">別名：${a.aliases.join('、')}</div>`}
        ${a && html`<${PeopleLine} label="聲優" list=${a.voices} /><${PeopleLine} label="飾演" list=${a.characters} />`}
      </div>
    </div>
    ${a && html`<div class="actions artist-actions">
      ${every.length > 0 && html`<button class="btn filled" onClick=${() => playQueue(every, 0)}><${Icon} name="play" />播放</button>
        <button class="btn tonal" onClick=${() => playQueue(shuffled(every), 0)}><${Icon} name="shuffle" />隨機播放</button>`}
      <${IconButton} icon="more" label="更多" onClick=${(e) => openMenu(e, [
        every.length && { icon: 'queue', label: '加入佇列', onClick: () => enqueue(every) },
        every.length && '-',
        { icon: 'edit', label: '改名…', onClick: () => renameArtist(a) },
        { icon: 'note', label: '編輯別名…', onClick: () => editArtistAliases(a) },
        { icon: 'merge', label: '和其他歌手合併…', onClick: () => mergeArtist({ ...a, tracks: every.length }) },
      ])} />
    </div>`}
    ${artist.loading && !a ? html`<${Spinner} />` : html`<${ErrorBox} error=${artist.error} onRetry=${artist.reload} />`}
    ${a && !every.length && html`<${Empty} icon="person">這位歌手已經沒有歌曲。<//>`}
    ${albums.length > 1 && html`<${Shelf} title="專輯" albums=${albums} />`}
    ${sung.length > 0 && html`${(roles.length > 0 || albums.length > 1) && html`<h2 class="section-title">${roles.length > 0 ? '演唱' : '歌曲'}</h2>`}
      <${TrackList} items=${sung} showAlbum hideArtist=${a.name} />`}
    ${roles.map((r) => html`<div key=${r.role}><h2 class="section-title">${roleTitles[r.role] || r.role}</h2>
      <${TrackList} items=${r.items} showAlbum hideArtist=${a.name} /></div>`)}
  </section>`;
}

// SplitCard asks about the names that look like several artists, on the artists tab (D13).
function SplitCard({ rev }) {
  const splits = useLoad(() => get('/artists/splits'), [], rev);
  const list = splits.data ? splits.data.proposals : [];
  if (!list.length) return null;
  return html`<div class="card summary-card">
    <${Icon} name="person" />
    <span class="grow track-text"><span class="title">${list.length} 個歌手名稱看起來是幾位歌手</span>
      <span class="sub">${list.slice(0, 3).map((x) => x.name).join('、')}${list.length > 3 ? ' 等' : ''}</span></span>
    <a class="btn tonal" href=${href('splits')}>逐一確認…</a>
  </div>`;
}

// SameCard offers the artists that look like one under two names (D13).
function SameCard({ rev }) {
  const same = useLoad(() => get('/artists/same'), [], rev);
  const list = same.data ? same.data.pairs : [];
  if (!list.length) return null;
  return html`<div class="card summary-card">
    <${Icon} name="merge" />
    <span class="grow track-text"><span class="title">${list.length} 組歌手看起來是同一位</span>
      <span class="sub">${list.slice(0, 2).map((p) => `${p.a.name} ↔ ${p.b.name}`).join('、')}${list.length > 2 ? ' 等' : ''}</span></span>
    <a class="btn tonal" href=${href('same')}>逐一確認…</a>
  </div>`;
}

// Searches made, the latest first, kept in this browser (review 2.8).
const SEARCHES = 'kanade.searches';
function pastSearches() {
  try {
    const v = JSON.parse(localStorage.getItem(SEARCHES) || '[]');
    return Array.isArray(v) ? v.filter((x) => typeof x === 'string' && x.trim()).slice(0, 8) : [];
  } catch {
    return [];
  }
}
function keepSearches(list) {
  try {
    localStorage.setItem(SEARCHES, JSON.stringify(list.slice(0, 8)));
  } catch { /* storage blocked: not kept */ }
}
function rememberSearch(q) {
  if ((q = q.trim())) keepSearches([q, ...pastSearches().filter((x) => x !== q)]);
}

// openSearch goes to the search, its box focused (/ and Ctrl+K, shortcuts.js).
let focusOnArrival = false;
export function openSearch() {
  const box = document.querySelector('.search-page input[type="search"]');
  if (box) {
    box.focus();
    box.select();
    return;
  }
  focusOnArrival = true;
  go('search');
}

// A result's links and songs, in the order shown: the arrows go through them (review 2.8).
const RESULT = '.search-results :is(a.row, a.card, .track-main)';
const fold = (t) => (t || '').normalize('NFKC').toLowerCase().replace(/[\s・･·]+/g, '');

// bestOf is the artist, album or work named just as searched (or starting so), shown first.
function bestOf(r, q) {
  const k = fold(q);
  if (!k) return null;
  const all = [
    ...r.artists.map((a) => ({ kind: 'artist', name: a.name, item: a })),
    ...r.albums.map((a) => ({ kind: 'album', name: a.title, item: a })),
    ...(r.works || []).map((w) => ({ kind: 'work', name: w.name, item: w })),
  ];
  return all.find((x) => fold(x.name) === k) || (k.length > 1 && all.find((x) => fold(x.name).startsWith(k))) || null;
}

function BestResult({ best }) {
  const { kind, item } = best;
  const link = kind === 'artist' ? `artist/${item.id}?name=${encodeURIComponent(item.name)}` : kind === 'album' ? 'album/' + item.id : 'work/' + item.id;
  const pic = kind === 'artist' ? html`<span class="avatar best-pic"><${Icon} name="person" size=${36} /></span>`
    : kind === 'album' ? html`<${Cover} id=${item.cover_id} size=${300} className="best-pic" />`
      : html`<${WorkImage} id=${item.image ? item.id : 0} size=${300} className="best-pic" />`;
  const sub = kind === 'artist' ? `歌手 · ${item.tracks} 首`
    : kind === 'album' ? ['專輯', item.album_artist, item.date && item.date.slice(0, 4)].filter(Boolean).join(' · ')
      : ['作品', item.name_cn, item.albums && `${item.albums} 張專輯`].filter(Boolean).join(' · ');
  return html`<section class="search-group"><h2 class="section-title">最佳結果</h2>
    <a class="row card best-result" href=${href(link)}>${pic}
      <span class="grow track-text"><span class="title">${kind === 'work' ? item.name : best.name}</span><span class="sub">${sub}</span></span></a></section>`;
}

// Group is a kind of result: its first few, and the rest on asking.
function Group({ title, items, n = 5, children }) {
  const [all, setAll] = useState(false);
  if (!items.length) return null;
  return html`<section class="search-group">
    <div class="group-head"><h2 class="section-title">${title}</h2>
      ${items.length > n && html`<button class="btn text" aria-expanded=${all} onClick=${() => setAll(!all)}>${all ? '收起' : `顯示全部（${items.length}${items.length >= 50 ? '+' : ''}）`}</button>`}</div>
    ${children(all ? items : items.slice(0, n))}
  </section>`;
}

const albumRows = (list) => html`<ul class="list">${list.map((a) => html`<li key=${a.id}><a class="row" href=${href('album/' + a.id)}>
  <${Cover} id=${a.cover_id} size=${96} className="row-pic" />
  <span class="grow track-text"><span class="title">${a.title}</span>
    <span class="sub">${[a.album_artist || '未知歌手', a.date && a.date.slice(0, 4), `${a.tracks} 首`].filter(Boolean).join(' · ')}</span></span></a></li>`)}</ul>`;
const artistRows = (list) => html`<ul class="list">${list.map((a) => html`<li key=${a.id}><a class="row" href=${href(`artist/${a.id}?name=${encodeURIComponent(a.name)}`)}>
  <span class="avatar"><${Icon} name="person" /></span><span class="grow">${a.name}${a.kind === 'character' && html`<span class="sub"> · 角色</span>`}</span><span class="sub">${a.tracks} 首</span></a></li>`)}</ul>`;
const workRows = (list) => html`<ul class="list">${list.map((w) => html`<li key=${w.id}><a class="row" href=${href('work/' + w.id)}>
  <${WorkImage} id=${w.image ? w.id : 0} size=${96} className="row-pic" />
  <span class="grow track-text"><span class="title">${w.name}</span>
    <span class="sub">${[w.name_cn, w.albums && `${w.albums} 張專輯`, w.tracks && `${w.tracks} 首歌`].filter(Boolean).join(' · ')}</span></span></a></li>`)}</ul>`;

// Search keeps its words in the address (#/search?q=…), so coming back from a result finds them
// and their results again; an error is for the search that made it (review #164). The results come
// in kinds, a few of each, the best one first (review 2.8).
export function Search() {
  const [q, setQ] = useState(() => parseHash().query.get('q') || '');
  const [result, setResult] = useState(null);
  const [error, setError] = useState(null);
  const [past, setPast] = useState(pastSearches);
  const box = useRef(null);
  useEffect(() => { // arriving by / or Ctrl+K, or with a keyboard and mouse: ready to type
    if (focusOnArrival || matchMedia('(pointer: fine)').matches) box.current && box.current.focus();
    focusOnArrival = false;
  }, []);
  useEffect(() => {
    keepInAddress(q.trim() ? 'search?q=' + encodeURIComponent(q) : 'search');
    setError(null);
    if (!q.trim()) {
      setResult(null);
      return;
    }
    const ctrl = new AbortController();
    const t = setTimeout(() => {
      get('/search?q=' + encodeURIComponent(q), { signal: ctrl.signal }).then((r) => { setResult({ q, ...r }); setError(null); },
        (e) => e.name !== 'AbortError' && setError(e));
    }, 250);
    return () => { clearTimeout(t); ctrl.abort(); };
  }, [q]);
  const term = q.trim();
  const shown = term && result ? result : null;
  const nothing = shown && !shown.tracks.length && !shown.albums.length && !shown.artists.length && !(shown.works || []).length;
  const best = shown && bestOf(shown, shown.q);
  const remember = () => {
    rememberSearch(term);
    setPast(pastSearches());
  };
  // ↓ and ↑ go through the results and back to the box; Enter in the box opens the first.
  const onKeyDown = (e) => {
    if (e.isComposing || e.keyCode === 229 || e.altKey || e.ctrlKey || e.metaKey) return;
    const inBox = e.target === box.current;
    if (inBox && e.key === 'Enter') {
      e.preventDefault();
      remember();
      const first = document.querySelector(RESULT);
      if (first && shown && shown.q === q) first.click();
      return;
    }
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
    const all = [...document.querySelectorAll(RESULT)];
    const at = inBox ? -1 : all.indexOf(e.target);
    if (!inBox && at < 0) return;
    const to = at + (e.key === 'ArrowDown' ? 1 : -1);
    if (to >= all.length) return;
    e.preventDefault();
    const el = to < 0 ? box.current : all[to];
    el.focus();
    if (to >= 0) el.scrollIntoView({ block: 'nearest' });
  };
  return html`<section class="search-page" onKeyDown=${onKeyDown}>
    <h1 class="page-title">搜尋</h1>
    <label class="search-field">
      <${Icon} name="search" />
      <input type="search" ref=${box} placeholder="歌名、歌手、專輯、作品" aria-label="搜尋歌名、歌手、專輯、作品" value=${q}
        onInput=${(e) => setQ(e.target.value)} aria-keyshortcuts="/ Control+K" />
    </label>
    <${ErrorBox} error=${error} />
    ${!term && past.length > 0 && html`<section class="search-group">
      <div class="group-head"><h2 class="section-title">最近搜尋</h2>
        <button class="btn text" onClick=${() => { keepSearches([]); setPast([]); }}>清除</button></div>
      <ul class="list past-searches">${past.map((p) => html`<li key=${p} class="past-search">
        <button class="row plain wide" onClick=${() => { setQ(p); box.current && box.current.focus(); }}><${Icon} name="history" /><span class="grow">${p}</span></button>
        <${IconButton} icon="close" label=${`從最近搜尋移除「${p}」`} onClick=${() => { const left = past.filter((x) => x !== p); keepSearches(left); setPast(left); }} />
      </li>`)}</ul></section>`}
    ${!term && !past.length && html`<p class="hint">輸入歌名、歌手、專輯或作品的一部分。在其他頁按 <kbd>/</kbd> 或 <kbd>Ctrl</kbd> + <kbd>K</kbd> 也會來到這裡。</p>`}
    ${nothing && html`<${Empty} icon="search">找不到「${shown.q}」<//>`}
    ${shown && html`<div class="search-results" key=${shown.q} onClick=${(e) => e.target.closest('a[href], .track-main') && remember()}>
      ${best && html`<${BestResult} best=${best} />`}
      <${Group} title="歌曲" items=${shown.tracks}>${(list) => html`<${TrackList} items=${list.map(fromTrack)} showAlbum />`}<//>
      <${Group} title="歌手" items=${shown.artists}>${artistRows}<//>
      <${Group} title="專輯" items=${shown.albums}>${albumRows}<//>
      <${Group} title="作品" items=${shown.works || []}>${workRows}<//>
    </div>`}
    ${term && html`<div class="actions"><a class="btn text" href=${href('feeds?q=' + encodeURIComponent(term))}><${Icon} name="download" />在 RSS 資源中找「${term}」</a></div>`}
  </section>`;
}

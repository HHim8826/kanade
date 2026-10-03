import { useCallback, useEffect, useRef, useState } from '../../vendor/hooks.module.js';
import { get } from '../api.js';
import { addToPlaylist, toggleFav, useFav } from '../actions.js';
import { enqueue, fromEntry, fromTrack, playLibraryShuffle, playNext, playQueue, player, shuffled, toggle } from '../player.js';
import { go, href } from '../router.js';
import { useStore } from '../store.js';
import { Cover, Empty, ErrorBox, Icon, IconButton, Spinner, fmtBytes, fmtTime, html, openMenu, toast, useLoad } from '../ui.js';
import { FavoritesTab, PlaylistsTab } from './collections.js';
import { AlbumGrid, TrackList, playInAlbum } from './common.js';
import { changeCover, editAlbum, editArtistAliases, folderAlbums, identifyAlbum, mergeAlbum, removeAlbum, removeFromAlbum, renameArtist,
  restoreAlbum, splitAlbum, useLibRev, vgmdbAlbum } from './organize.js';

// Albums in one horizontally scrolling row (home page shelves).
function Shelf({ title, albums, more }) {
  if (!albums || !albums.length) return null;
  return html`<div class="section-head"><h2 class="section-title">${title}</h2>${more}</div>
    <div class="shelf">${albums.map((a) => html`<a key=${a.id} class="card album-card" href=${href('album/' + a.id)}>
      <${Cover} id=${a.cover_id} alt="" />
      <div class="card-text"><div class="title" title=${a.title}>${a.title}</div><div class="sub" title=${a.album_artist || ''}>${a.album_artist || '未知歌手'}</div></div>
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
        <${ProgressLine} position=${s.time} duration=${s.duration} />
        <span class="sub">${fmtTime(s.time * 1000)} / ${fmtTime(s.duration * 1000)}</span>
      </span>
    </button>
    <${IconButton} icon=${s.playing ? 'pause' : 'play'} label=${s.playing ? '暫停' : '播放'} filled size=${28} onClick=${toggle} />
  </div>`;
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
  const home = useLoad(() => get('/home'), []);
  const d = home.data;
  const live = useStore(player, (s) => s.queue[s.index]); // re-renders on track change, not on every tick
  const busy = d && (Object.keys(d.tasks.downloads).length > 0 || d.tasks.importing > 0);
  const att = d && d.attention;
  const empty = d && !d.recently_added.length;
  // The track in the top card is not repeated in the drama list below it.
  const shown = live ? live.assetId : d && d.continue ? d.continue.asset.id : 0;
  const spoken = d ? d.spoken.filter((t) => t.asset.id !== shown) : [];
  return html`<section>
    <div class="page-head">
      <h1 class="page-title">首頁</h1>
      <div class="actions">
        <button class="btn tonal" onClick=${() => playLibraryShuffle()}><${Icon} name="shuffle" />全曲庫隨機播放</button>
        <button class="btn tonal" onClick=${playRandomAlbum}><${Icon} name="album" />隨便聽一張</button>
      </div>
    </div>
    ${home.loading && !d ? html`<${Spinner} />` : html`<${ErrorBox} error=${home.error} onRetry=${home.reload} />`}
    ${live ? html`<${NowCard} />` : d && d.continue && html`<${LastPlayedCard} item=${d.continue} />`}
    ${(busy || (att && att.failed_imports > 0)) && html`<a class="card summary-card" href=${href('tasks')}>
      <${Icon} name="tasks" />
      <span class="grow">
        ${Object.entries(d.tasks.downloads).map(([k, n]) => html`<span class="pill">${downloadLabels[k] || k} ${n}</span>`)}
        ${d.tasks.importing > 0 && html`<span class="pill">匯入中 ${d.tasks.importing}</span>`}
        ${att.failed_imports > 0 && html`<span class="pill warn">匯入失敗 ${att.failed_imports}</span>`}
      </span>
      <span class="sub">任務 ›</span>
    </a>`}
    ${empty && html`<${Empty} icon="library">曲庫還是空的。到「任務」新增下載，或上傳音樂。<//>`}
    ${d && html`<${Shelf} title="最近播放" albums=${d.recently_played}
      more=${html`<a class="btn text" href=${href('history')}><${Icon} name="history" />播放記錄</a>`} />`}
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
      <a class="card summary-card" href=${href(att.missing > 0 ? 'missing' : 'library/tracks?filter=' + (att.without_album > 0 ? 'no_album' : 'no_artist'))}>
        <${Icon} name="note" />
        <span class="grow">
          ${att.without_album > 0 && html`<span class="pill">沒有專輯的歌曲 ${att.without_album}</span>`}
          ${att.unknown_artist > 0 && html`<span class="pill">沒有歌手 ${att.unknown_artist}</span>`}
          ${att.missing > 0 && html`<span class="pill warn">Drive 中遺失 ${att.missing}</span>`}
        </span>
        <span class="sub">${att.missing > 0 ? '查看 ›' : '整理 ›'}</span>
      </a>`}
  </section>`;
}

const tabs = [['albums', '專輯'], ['artists', '歌手'], ['tracks', '歌曲'], ['playlists', '歌單'], ['favorites', '收藏']];
const tabURL = { playlists: '/playlists', favorites: '/favorites' };

const PAGE = 200;

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

export function Library({ tab = 'albums', filter = '' }) {
  const rev = useLibRev();
  const paged = !tabURL[tab];
  if (tab !== 'tracks' || !trackFilters.some(([k]) => k === filter)) filter = '';
  const base = tabURL[tab] || `/${tab}?limit=${PAGE}${filter ? '&filter=' + filter : ''}`;
  const data = useLoad(() => get(base), [base], rev);
  // Later pages, for this tab, filter and library version only (review #9).
  const key = `${base}:${rev}`;
  const [more, setMore] = useState({ key: null, pages: [], done: false, busy: false });
  const own = more.key === key ? more : { key, pages: [], done: false, busy: false };
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
  return html`<section>
    <div class="page-head">
      <h1 class="page-title">曲庫</h1>
      <div class="actions">
        <button class="btn tonal" onClick=${() => playLibraryShuffle()}><${Icon} name="shuffle" />全曲庫隨機播放</button>
        <a class="btn text" href=${href('edits')}><${Icon} name="history" />修改紀錄</a>
      </div>
    </div>
    <nav class="tabs" role="tablist">
      ${tabs.map(([k, label]) => html`<a role="tab" aria-selected=${k === tab} class=${k === tab ? 'active' : ''} href=${href('library/' + k)}>${label}</a>`)}
    </nav>
    ${tab === 'tracks' && html`<nav class="seg filter-seg" aria-label="篩選">${trackFilters.map(([k, label]) => html`<a key=${k}
      class=${k === filter ? 'on' : ''} aria-current=${k === filter ? 'page' : null} href=${href('library/tracks' + (k ? '?filter=' + k : ''))}>${label}</a>`)}</nav>`}
    ${tab === 'tracks' && filter === 'no_album' && html`<${FolderCard} rev=${rev} />`}
    ${data.loading && !data.data ? html`<${Spinner} />` : html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`}
    ${list && tab === 'albums' && html`<${AlbumGrid} albums=${list} />`}
    ${list && tab === 'artists' && html`<ul class="list">
      ${list.map((a) => html`<li key=${a.id}><a class="row" href=${href(`artist/${a.id}?name=${encodeURIComponent(a.name)}`)}>
        <span class="avatar"><${Icon} name="person" /></span><span class="grow">${a.name}</span><span class="sub">${a.tracks} 首</span></a></li>`)}
    </ul>`}
    ${list && tab === 'tracks' && (list.length
      ? html`<${TrackList} items=${list.map(fromTrack)} showAlbum />`
      : html`<${Empty}>${filter === 'no_album' ? '每首歌都有專輯了。' : filter === 'no_artist' ? '每首歌都有歌手了。' : '還沒有歌曲。'}<//>`)}
    ${list && !done && html`<${LoadMore} onMore=${loadMore} busy=${own.busy} />`}
    ${data.data && tab === 'playlists' && html`<${PlaylistsTab} lists=${data.data} />`}
    ${data.data && tab === 'favorites' && html`<${FavoritesTab} data=${data.data} />`}
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

export function Album({ id }) {
  const rev = useLibRev();
  const album = useLoad(() => get('/albums/' + id), [id], rev);
  if (album.loading && !album.data) return html`<${Spinner} />`;
  if (album.error) return html`<${ErrorBox} error=${album.error} onRetry=${album.reload} />`;
  const a = album.data;
  if (!a.entries.length) return html`<${EmptyAlbum} album=${a} />`;
  const items = a.entries.map((e) => ({ ...fromEntry(e, a), number: e.track_no || '', key: 'e' + e.entry_id, disc: e.disc_no, entryId: e.entry_id }));
  const discs = [...new Set(items.map((i) => i.disc))];
  const menu = (e) => openMenu(e, [
    { icon: 'playNext', label: '下一首播放', onClick: () => playNext(items) },
    { icon: 'queue', label: '加入佇列', onClick: () => enqueue(items) },
    { icon: 'playlistAdd', label: '加入歌單…', onClick: () => addToPlaylist(items) },
    { icon: 'edit', label: '編輯專輯資訊…', onClick: () => editAlbum(a) },
    { icon: 'identify', label: '從 MusicBrainz 辨識…', onClick: () => identifyAlbum(a) },
    { icon: 'identify', label: '從 VGMdb 匯入…', onClick: () => vgmdbAlbum(a) },
    { icon: 'image', label: '更換封面…', onClick: () => changeCover(a) },
    { icon: 'merge', label: '合併到其他專輯…', onClick: () => mergeAlbum(a) },
    a.entries.length > 1 && { icon: 'split', label: '拆分…', onClick: () => splitAlbum(a) },
    a.original && { icon: 'restore', label: '恢復原標籤…', onClick: () => restoreAlbum(a) },
    { icon: 'delete', label: '移除專輯…', onClick: () => removeAlbum(a) },
  ]);
  return html`<section>
    <header class="album-head">
      <${Cover} id=${a.cover_id} size=${600} alt=${a.title} className="big" />
      <div class="album-info">
        <div class="overline">專輯${a.edition ? ` · ${a.edition}` : ''}</div>
        <h1>${a.title}</h1>
        <div class="sub">${[a.album_artist, a.date, a.catalog].filter(Boolean).join(' · ')}</div>
        <div class="sub">${a.tracks} 首 · ${fmtTime(a.duration_ms)}</div>
        <div class="actions">
          <button class="btn filled" onClick=${() => playQueue(items, 0)}><${Icon} name="play" />播放</button>
          <button class="btn tonal" onClick=${() => playQueue(shuffled(items), 0)}><${Icon} name="shuffle" />隨機播放</button>
          <${AlbumFavorite} id=${a.id} />
          <${IconButton} icon="more" label="更多" onClick=${menu} />
        </div>
      </div>
    </header>
    ${discs.map((d) => html`<div key=${d}>
      ${discs.length > 1 && html`<h2 class="section-title">Disc ${d}</h2>`}
      <${TrackList} items=${items.filter((i) => i.disc === d)} queue=${items} showNumber
        menuExtra=${(it) => [{ icon: 'delete', label: '從專輯移除', onClick: () => removeFromAlbum(a, it) }]} />
    </div>`)}
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

export function Artist({ id, name }) {
  const rev = useLibRev();
  const artist = useLoad(() => get('/artists/' + id), [id], rev);
  const a = artist.data;
  return html`<section>
    <div class="page-head">
      <div class="grow">
        <h1 class="page-title">${a ? a.name : name || '歌手'}</h1>
        ${a && a.aliases.length > 0 && html`<div class="sub alias-line">別名：${a.aliases.join('、')}</div>`}
      </div>
      ${a && html`<${IconButton} icon="more" label="更多" onClick=${(e) => openMenu(e, [
        a.items.length && { icon: 'play', label: '全部播放', onClick: () => playQueue(a.items.map(fromTrack), 0) },
        { icon: 'edit', label: '改名…', onClick: () => renameArtist(a) },
        { icon: 'note', label: '編輯別名…', onClick: () => editArtistAliases(a) },
      ])} />`}
    </div>
    ${artist.loading && !a ? html`<${Spinner} />` : html`<${ErrorBox} error=${artist.error} onRetry=${artist.reload} />`}
    ${a && (a.items.length ? html`<${TrackList} items=${a.items.map(fromTrack)} showAlbum />` : html`<${Empty} icon="person">這位歌手已經沒有歌曲。<//>`)}
  </section>`;
}

export function Search() {
  const [q, setQ] = useState('');
  const [result, setResult] = useState(null);
  const [error, setError] = useState(null);
  useEffect(() => {
    if (!q.trim()) {
      setResult(null);
      return;
    }
    const ctrl = new AbortController();
    const t = setTimeout(() => {
      get('/search?q=' + encodeURIComponent(q), { signal: ctrl.signal }).then(setResult, (e) => e.name !== 'AbortError' && setError(e));
    }, 250);
    return () => { clearTimeout(t); ctrl.abort(); };
  }, [q]);
  const nothing = result && !result.tracks.length && !result.albums.length && !result.artists.length;
  return html`<section>
    <h1 class="page-title">搜尋</h1>
    <label class="search-field">
      <${Icon} name="search" />
      <input type="search" placeholder="歌名、歌手、專輯" value=${q} onInput=${(e) => setQ(e.target.value)} autofocus />
    </label>
    <${ErrorBox} error=${error} />
    ${q.trim() && html`<div class="actions"><a class="btn text" href=${href('feeds?q=' + encodeURIComponent(q.trim()))}><${Icon} name="download" />在 RSS 資源中找「${q.trim()}」</a></div>`}
    ${nothing && html`<${Empty} icon="search">找不到「${q}」<//>`}
    ${result?.artists.length > 0 && html`<h2 class="section-title">歌手</h2><ul class="list">
      ${result.artists.map((a) => html`<li key=${a.id}><a class="row" href=${href(`artist/${a.id}?name=${encodeURIComponent(a.name)}`)}>
        <span class="avatar"><${Icon} name="person" /></span><span class="grow">${a.name}</span><span class="sub">${a.tracks} 首</span></a></li>`)}
    </ul>`}
    ${result?.albums.length > 0 && html`<h2 class="section-title">專輯</h2><${AlbumGrid} albums=${result.albums} />`}
    ${result?.tracks.length > 0 && html`<h2 class="section-title">歌曲</h2><${TrackList} items=${result.tracks.map(fromTrack)} showAlbum />`}
  </section>`;
}

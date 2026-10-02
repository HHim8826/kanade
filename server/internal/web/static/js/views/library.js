import { useEffect, useState } from '../../vendor/hooks.module.js';
import { get } from '../api.js';
import { fromEntry, fromTrack, playQueue, player, shuffled, toggle } from '../player.js';
import { href } from '../router.js';
import { useStore } from '../store.js';
import { Cover, Empty, ErrorBox, Icon, IconButton, Spinner, fmtQuality, fmtTime, html, toast, useLoad } from '../ui.js';

function AlbumGrid({ albums }) {
  if (!albums.length) return html`<${Empty} icon="album">還沒有專輯。到「任務」上傳音樂或加入下載。<//>`;
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

// TrackList plays the whole list starting from the row that was tapped.
export function TrackList({ items, showNumber, showAlbum }) {
  const { queue, index } = useStore(player);
  const playingId = queue[index]?.assetId;
  return html`<ol class="tracks">
    ${items.map((it, i) => html`<li key=${it.key || it.assetId + '-' + i}>
      <button class=${'track' + (it.assetId === playingId ? ' current' : '')} onClick=${() => playQueue(items, i)}>
        ${showNumber ? html`<span class="num">${it.number || ''}</span>` : html`<${Cover} id=${it.coverId} size=${96} className="thumb" />`}
        <span class="track-text">
          <span class="title">${it.title}</span>
          <span class="sub">${[it.artist || '未知歌手', showAlbum && it.album].filter(Boolean).join(' · ')}</span>
        </span>
        <span class="meta"><span class="quality">${fmtQuality(it.asset)}</span><span>${fmtTime(it.durationMs)}</span></span>
      </button>
    </li>`)}
  </ol>`;
}

// Albums in one horizontally scrolling row (home page shelves).
function Shelf({ title, albums }) {
  if (!albums || !albums.length) return null;
  return html`<h2 class="section-title">${title}</h2>
    <div class="shelf">${albums.map((a) => html`<a key=${a.id} class="card album-card" href=${href('album/' + a.id)}>
      <${Cover} id=${a.cover_id} alt="" />
      <div class="card-text"><div class="title" title=${a.title}>${a.title}</div><div class="sub">${a.album_artist || '未知歌手'}</div></div>
    </a>`)}</div>`;
}

// Resume a half-heard track inside the album it was played from, so the queue continues naturally.
// A finished track moves on to the next one (back to the start after the album's last track).
async function resume(item) {
  if (item.album_id) {
    try {
      const a = await get('/albums/' + item.album_id);
      const items = a.entries.map((e) => fromEntry(e, a));
      const i = items.findIndex((q) => q.assetId === item.asset.id);
      if (i >= 0 && item.finished) return playQueue(items, i + 1 < items.length ? i + 1 : 0);
      if (i >= 0) {
        items[i] = { ...items[i], resumeMs: item.position_ms };
        return playQueue(items, i);
      }
    } catch { /* fall back to the single track */ }
  }
  playQueue([{ ...fromTrack(item), resumeMs: item.finished ? 0 : item.position_ms }], 0);
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
    <${IconButton} icon=${item.finished && item.album_id ? 'next' : 'play'} label=${action} filled size=${28} onClick=${() => resume(item)} />
  </div>`;
}

async function playRandomAlbum() {
  try {
    const { id } = await get('/albums/random');
    const a = await get('/albums/' + id);
    playQueue(a.entries.map((e) => fromEntry(e, a)), 0);
    location.hash = href('album/' + id);
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
      <button class="btn tonal" onClick=${playRandomAlbum}><${Icon} name="shuffle" />隨便聽一張</button>
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
    ${d && html`<${Shelf} title="最近播放" albums=${d.recently_played} />`}
    ${spoken.length > 0 && html`<h2 class="section-title">未聽完的廣播劇</h2>
      <ul class="list">${spoken.map((t) => html`<li key=${t.id}><button class="row plain wide" onClick=${() => resume(t)}>
        <${Cover} id=${t.cover_id} size=${96} className="thumb" />
        <span class="grow track-text"><span class="title">${t.title}</span><span class="sub">${t.album || t.artist}</span>
          <${ProgressLine} position=${t.position_ms} duration=${t.asset.duration_ms} /></span>
        <span class="sub">剩 ${fmtTime(t.asset.duration_ms - t.position_ms)}</span>
      </button></li>`)}</ul>`}
    ${d && html`<${Shelf} title="最近加入" albums=${d.recently_added} />`}
    ${att && (att.without_album > 0 || att.unknown_artist > 0) && html`<h2 class="section-title">待整理</h2>
      <a class="card summary-card" href=${href('library/tracks')}>
        <${Icon} name="note" />
        <span class="grow">
          ${att.without_album > 0 && html`<span class="pill">沒有專輯的歌曲 ${att.without_album}</span>`}
          ${att.unknown_artist > 0 && html`<span class="pill">沒有歌手 ${att.unknown_artist}</span>`}
        </span>
        <span class="sub">歌曲 ›</span>
      </a>`}
  </section>`;
}

const tabs = [['albums', '專輯'], ['artists', '歌手'], ['tracks', '歌曲']];

export function Library({ tab = 'albums' }) {
  const data = useLoad(() => get(`/${tab}?limit=500`), [tab]);
  return html`<section>
    <h1 class="page-title">曲庫</h1>
    <nav class="tabs" role="tablist">
      ${tabs.map(([k, label]) => html`<a role="tab" aria-selected=${k === tab} class=${k === tab ? 'active' : ''} href=${href('library/' + k)}>${label}</a>`)}
    </nav>
    ${data.loading ? html`<${Spinner} />` : html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`}
    ${data.data && tab === 'albums' && html`<${AlbumGrid} albums=${data.data} />`}
    ${data.data && tab === 'artists' && html`<ul class="list">
      ${data.data.map((a) => html`<li key=${a.id}><a class="row" href=${href(`artist/${a.id}?name=${encodeURIComponent(a.name)}`)}>
        <span class="avatar"><${Icon} name="person" /></span><span class="grow">${a.name}</span><span class="sub">${a.tracks} 首</span></a></li>`)}
    </ul>`}
    ${data.data && tab === 'tracks' && (data.data.length
      ? html`<${TrackList} items=${data.data.map(fromTrack)} showAlbum />`
      : html`<${Empty}>還沒有歌曲。<//>`)}
  </section>`;
}

export function Album({ id }) {
  const album = useLoad(() => get('/albums/' + id), [id]);
  if (album.loading) return html`<${Spinner} />`;
  if (album.error) return html`<${ErrorBox} error=${album.error} onRetry=${album.reload} />`;
  const a = album.data;
  const items = a.entries.map((e) => ({ ...fromEntry(e, a), number: e.track_no || '', key: 'e' + e.entry_id, disc: e.disc_no }));
  const discs = [...new Set(items.map((i) => i.disc))];
  return html`<section>
    <header class="album-head">
      <${Cover} id=${a.cover_id} size=${600} alt=${a.title} className="big" />
      <div class="album-info">
        <div class="overline">專輯</div>
        <h1>${a.title}</h1>
        <div class="sub">${[a.album_artist, a.date].filter(Boolean).join(' · ')}</div>
        <div class="sub">${a.tracks} 首 · ${fmtTime(a.duration_ms)}</div>
        <div class="actions">
          <button class="btn filled" onClick=${() => playQueue(items, 0)}><${Icon} name="play" />播放</button>
          <button class="btn tonal" onClick=${() => playQueue(shuffled(items), 0)}><${Icon} name="shuffle" />隨機播放</button>
        </div>
      </div>
    </header>
    ${discs.map((d) => html`<div key=${d}>
      ${discs.length > 1 && html`<h2 class="section-title">Disc ${d}</h2>`}
      <${TrackList} items=${items.filter((i) => i.disc === d)} showNumber />
    </div>`)}
  </section>`;
}

export function Artist({ id, name }) {
  const tracks = useLoad(() => get('/artists/' + id), [id]);
  return html`<section>
    <h1 class="page-title">${name || '歌手'}</h1>
    ${tracks.loading ? html`<${Spinner} />` : html`<${ErrorBox} error=${tracks.error} onRetry=${tracks.reload} />`}
    ${tracks.data && html`<${TrackList} items=${tracks.data.map(fromTrack)} showAlbum />`}
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
    ${nothing && html`<${Empty} icon="search">找不到「${q}」<//>`}
    ${result?.artists.length > 0 && html`<h2 class="section-title">歌手</h2><ul class="list">
      ${result.artists.map((a) => html`<li key=${a.id}><a class="row" href=${href(`artist/${a.id}?name=${encodeURIComponent(a.name)}`)}>
        <span class="avatar"><${Icon} name="person" /></span><span class="grow">${a.name}</span><span class="sub">${a.tracks} 首</span></a></li>`)}
    </ul>`}
    ${result?.albums.length > 0 && html`<h2 class="section-title">專輯</h2><${AlbumGrid} albums=${result.albums} />`}
    ${result?.tracks.length > 0 && html`<h2 class="section-title">歌曲</h2><${TrackList} items=${result.tracks.map(fromTrack)} showAlbum />`}
  </section>`;
}

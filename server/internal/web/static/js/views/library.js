import { useEffect, useState } from '../../vendor/hooks.module.js';
import { get } from '../api.js';
import { fromEntry, fromTrack, playQueue, player, shuffled } from '../player.js';
import { href } from '../router.js';
import { useStore } from '../store.js';
import { Cover, Empty, ErrorBox, Icon, Spinner, fmtQuality, fmtTime, html, useLoad } from '../ui.js';

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

export function Home() {
  const albums = useLoad(() => get('/albums?sort=recent&limit=24'), []);
  return html`<section>
    <h1 class="page-title">首頁</h1>
    <h2 class="section-title">最近加入</h2>
    ${albums.loading ? html`<${Spinner} />` : html`<${ErrorBox} error=${albums.error} onRetry=${albums.reload} />`}
    ${albums.data && html`<${AlbumGrid} albums=${albums.data} />`}
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

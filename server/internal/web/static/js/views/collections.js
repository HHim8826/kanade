import { useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { addToPlaylist } from '../actions.js';
import { enqueue, fromTrack, playNext, playQueue, shuffled } from '../player.js';
import { go, href } from '../router.js';
import { Cover, Dialog, Empty, ErrorBox, Icon, IconButton, Spinner, fmtTime, html, openMenu, showDialog, toast, useLoad } from '../ui.js';
import { AlbumGrid, TrackList, playInAlbum } from './common.js';
import { AlbumActions, SongActions } from './batch.js';
import { SelectBar, SelectToggle, useSelection } from '../selection.js';
import { useLibRev } from './organize.js';
import { SmartActions, describe, editSmart, newSmart, saveAsPlain, useRuleNames } from './smart.js';

// Playlists, favorites and play history (P2-1).

function NameDialog({ title, initial = '', action, onSubmit, close }) {
  const [name, setName] = useState(initial);
  const [busy, setBusy] = useState(false);
  const submit = async (e) => {
    e.preventDefault();
    if (!name.trim()) return;
    setBusy(true);
    try {
      await onSubmit(name.trim());
      close();
    } catch (err) {
      toast(err.message, 'error');
      setBusy(false);
    }
  };
  return html`<${Dialog} title=${title} onClose=${close}>
    <form onSubmit=${submit}>
      <label class="field">名稱<input value=${name} maxlength="200" autofocus onInput=${(e) => setName(e.target.value)} /></label>
      <div class="dialog-actions">
        <button type="button" class="btn text" onClick=${close}>取消</button>
        <button class="btn filled" disabled=${busy || !name.trim()}>${action}</button>
      </div>
    </form>
  <//>`;
}

function newPlaylist() {
  showDialog((close) => html`<${NameDialog} title="新增歌單" action="建立" close=${close}
    onSubmit=${async (name) => go('playlist/' + (await post('/playlists', { name })).id)} />`);
}

export function PlaylistsTab({ lists }) {
  return html`<div class="actions"><button class="btn tonal" onClick=${newPlaylist}><${Icon} name="add" />新增歌單</button>
      <button class="btn tonal" onClick=${newSmart}><${Icon} name="shuffle" />新增智慧歌單</button></div>
    ${lists.length === 0
      ? html`<${Empty} icon="queue">還沒有歌單。在歌曲的「更多」選單選「加入歌單」，或按上面的按鈕建立。<//>`
      : html`<div class="grid">${lists.map((p) => html`<a key=${p.id} class="card album-card" href=${href('playlist/' + p.id)}>
          <${Cover} id=${p.cover_id} alt="" />
          <div class="card-text"><div class="title" title=${p.name}>${p.name}</div><div class="sub">${p.smart ? '智慧歌單 · ' : ''}${p.tracks} 首</div></div>
        </a>`)}</div>`}`;
}

export function FavoritesTab({ data }) {
  const items = data.tracks.map(fromTrack);
  const albums = useSelection('favorite-albums');
  const songs = useSelection('favorite-songs');
  if (!items.length && !data.albums.length) {
    return html`<${Empty} icon="favoriteOff">還沒有收藏。在歌曲的「更多」選單或專輯頁按愛心收藏。<//>`;
  }
  return html`
    ${data.albums.length > 0 && html`<div class="section-head"><h2 class="section-title">專輯</h2>
      <div class="actions tight"><${SelectToggle} sel=${albums} /></div></div>
      <${AlbumGrid} albums=${data.albums} sel=${albums} />`}
    ${items.length > 0 && html`<div class="section-head"><h2 class="section-title">歌曲 <span class="sub">${items.length} 首</span></h2>
      <div class="actions tight">
        <button class="btn tonal" onClick=${() => playQueue(items, 0)}><${Icon} name="play" />播放</button>
        <${IconButton} icon="shuffle" label="隨機播放" onClick=${() => playQueue(shuffled(items), 0)} />
        <${SelectToggle} sel=${songs} />
      </div></div>
      <${TrackList} items=${items} showAlbum sel=${songs} />`}
    <${SelectBar} sel=${albums} noun="張" loaded=${data.albums.map((a) => a.id)} items=${data.albums}><${AlbumActions} sel=${albums} /><//>
    <${SelectBar} sel=${songs} noun="首" loaded=${items.map((it) => it.trackId)} items=${items}><${SongActions} sel=${songs} /><//>`;
}

export function Playlist({ id }) {
  const rev = useLibRev();
  const pl = useLoad(() => get('/playlists/' + id), [id], rev);
  const [order, setOrder] = useState(null); // optimistic item order while a reorder is saved
  const names = useRuleNames(pl.data && pl.data.rules);
  if (pl.loading && !pl.data) return html`<${Spinner} />`;
  if (pl.error) return html`<${ErrorBox} error=${pl.error} onRetry=${pl.reload} />`;
  const p = pl.data;
  if (p.smart) return html`<${SmartPlaylist} p=${p} names=${names} reload=${pl.reload} />`;
  const byId = new Map(p.items.map((it) => [it.item_id, it]));
  // A saved order stays in use while it still names exactly the loaded items, so the list does not
  // jump back while the reload is on its way.
  const current = order && order.length === byId.size && order.every((i) => byId.has(i)) ? order : p.items.map((it) => it.item_id);
  const rows = current.map((i) => byId.get(i));
  const items = rows.map((it) => ({ ...fromTrack(it), key: 'p' + it.item_id, itemId: it.item_id }));

  const saveOrder = async (ids) => {
    setOrder(ids);
    try {
      await api('PUT', `/playlists/${id}/order`, { items: ids });
    } catch (e) {
      toast(e.message, 'error');
      setOrder(null);
    }
    pl.reload();
  };
  const reorder = (from, to) => {
    const ids = rows.map((it) => it.item_id);
    ids.splice(to, 0, ...ids.splice(from, 1));
    saveOrder(ids);
  };
  const remove = async (item) => {
    try {
      await api('DELETE', `/playlists/${id}/items/${item.itemId}`);
      toast(`已從歌單移除「${item.title}」`);
      pl.reload();
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const rename = () => showDialog((close) => html`<${NameDialog} title="重新命名歌單" action="儲存" initial=${p.name} close=${close}
    onSubmit=${async (name) => { await api('PATCH', '/playlists/' + id, { name, description: p.description }); pl.reload(); }} />`);
  const destroy = () => showDialog((close) => html`<${Dialog} title="刪除歌單" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled danger" onClick=${async () => {
        try {
          await api('DELETE', '/playlists/' + id);
          close();
          toast(`已刪除「${p.name}」`);
          go('me/playlists');
        } catch (e) {
          toast(e.message, 'error');
        }
      }}>刪除</button>`}>
    <p>要刪除「${p.name}」嗎？歌單裡的歌曲不會從曲庫刪除。</p>
  <//>`);

  return html`<section>
    <header class="album-head">
      <${Cover} id=${p.cover_id} size=${600} alt=${p.name} className="big" />
      <div class="album-info">
        <div class="overline">歌單</div>
        <h1>${p.name}</h1>
        ${p.description && html`<div class="sub">${p.description}</div>`}
        <div class="sub">${p.tracks} 首 · ${fmtTime(p.duration_ms)}</div>
        <div class="actions">
          <button class="btn filled" disabled=${!items.length} onClick=${() => playQueue(items, 0)}><${Icon} name="play" />播放</button>
          <button class="btn tonal" disabled=${!items.length} onClick=${() => playQueue(shuffled(items), 0)}><${Icon} name="shuffle" />隨機播放</button>
          <${IconButton} icon="more" label="更多" onClick=${(e) => openMenu(e, [
            items.length && { icon: 'playNext', label: '下一首播放', onClick: () => playNext(items) },
            items.length && { icon: 'queue', label: '加入佇列', onClick: () => enqueue(items) },
            items.length && { icon: 'playlistAdd', label: '加入其他歌單…', onClick: () => addToPlaylist(items) },
            { icon: 'edit', label: '重新命名', onClick: rename },
            { icon: 'delete', label: '刪除歌單', onClick: destroy },
          ])} />
        </div>
      </div>
    </header>
    ${items.length === 0
      ? html`<${Empty} icon="queue">歌單是空的。在歌曲的「更多」選單選「加入歌單」。<//>`
      : html`<${TrackList} items=${items} showAlbum onReorder=${reorder} menuExtra=${(it, i) => [
          i > 0 && { icon: 'up', label: '上移', onClick: () => reorder(i, i - 1) },
          i < items.length - 1 && { icon: 'down', label: '下移', onClick: () => reorder(i, i + 1) },
          { icon: 'delete', label: '從歌單移除', onClick: () => remove(it) },
        ]} />`}
  </section>`;
}

// SmartPlaylist shows what a smart playlist picks now (review #96), to play as it is or on and on.
function SmartPlaylist({ p, names }) {
  const items = p.items.map((it, i) => ({ ...fromTrack(it), key: 's' + it.id + ':' + i }));
  const rename = () => showDialog((close) => html`<${NameDialog} title="重新命名歌單" action="儲存" initial=${p.name} close=${close}
    onSubmit=${async (name) => { await api('PATCH', '/playlists/' + p.id, { name, description: p.description }); go('playlist/' + p.id); }} />`);
  const destroy = () => showDialog((close) => html`<${Dialog} title="刪除智慧歌單" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled danger" onClick=${async () => {
        try {
          await api('DELETE', '/playlists/' + p.id);
          close();
          toast(`已刪除「${p.name}」`);
          go('me/playlists');
        } catch (e) {
          toast(e.message, 'error');
        }
      }}>刪除</button>`}>
    <p>只刪除這個智慧歌單的條件；曲庫裡的歌曲都不受影響。</p>
  <//>`);
  return html`<section>
    <header class="album-head">
      <${Cover} id=${p.cover_id} size=${600} alt=${p.name} className="big" />
      <div class="album-info">
        <div class="overline">智慧歌單</div>
        <h1>${p.name}</h1>
        <div class="sub">${describe(p.rules, names)}</div>
        <div class="sub">${p.tracks} 首 · ${fmtTime(p.duration_ms)}${p.matches > p.tracks ? `（符合條件的有 ${p.matches} 首）` : ''}</div>
        <div class="actions">
          <${SmartActions} p=${p} items=${items} />
          <${IconButton} icon="more" label="更多" onClick=${(e) => openMenu(e, [
            { icon: 'edit', label: '編輯條件…', onClick: () => editSmart(p) },
            items.length && { icon: 'playlistAdd', label: '另存為一般歌單', onClick: () => saveAsPlain(p, items) },
            items.length && { icon: 'queue', label: '加入佇列', onClick: () => enqueue(items) },
            { icon: 'edit', label: '重新命名', onClick: rename },
            { icon: 'delete', label: '刪除智慧歌單', onClick: destroy },
          ])} />
        </div>
      </div>
    </header>
    <p class="hint">內容依條件即時挑選：曲庫、分類、收藏或聆聽紀錄改變後，重新開啟就會更新。「依條件一直播」每次接歌都重新挑，不重複最近播過的歌。</p>
    ${items.length === 0 ? html`<${Empty} icon="queue">目前沒有符合條件的歌。<//>` : html`<${TrackList} items=${items} showAlbum />`}
  </section>`;
}

// ---- history ----

const dayLabel = (ms) => {
  const d = new Date(ms), today = new Date();
  const start = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const days = Math.round((start(today) - start(d)) / 86400000);
  if (days === 0) return '今天';
  if (days === 1) return '昨天';
  return d.toLocaleDateString('zh-TW', { month: 'long', day: 'numeric', weekday: 'short', year: d.getFullYear() === today.getFullYear() ? undefined : 'numeric' });
};
const clock = (ms) => new Date(ms).toLocaleTimeString('zh-TW', { hour: '2-digit', minute: '2-digit', hour12: false });

// HistoryTab is the history tab of "my" page (review #190).
export function HistoryTab() {
  const rev = useLibRev();
  const top = useLoad(() => get('/history/top?days=30'), [], rev);
  const [pages, setPages] = useState([]);
  const first = useLoad(() => get('/history?limit=50'), [], rev);
  const [more, setMore] = useState({ busy: false, done: false });
  const list = [...(first.data || []), ...pages.flat()];

  const loadMore = async () => {
    setMore({ busy: true, done: false });
    try {
      const last = list[list.length - 1];
      const next = await get(`/history?limit=50&before=${last.updated_at}&before_id=${last.play_id}`);
      setPages((p) => [...p, next]);
      setMore({ busy: false, done: next.length < 50 });
    } catch (e) {
      toast(e.message, 'error');
      setMore({ busy: false, done: false });
    }
  };

  const topItems = (top.data || []).slice(0, 10).map((t) => ({ ...fromTrack(t), plays: t.plays, key: 't' + t.id }));
  let lastDay = '';
  return html`
    ${topItems.length > 0 && html`<h2 class="section-title">最近 30 天最常播放</h2>
      <${TrackList} items=${topItems} showAlbum meta=${(it) => html`<span>${it.plays} 次</span>`} />`}
    <h2 class="section-title">最近播放</h2>
    ${first.loading && !first.data ? html`<${Spinner} />` : html`<${ErrorBox} error=${first.error} onRetry=${first.reload} />`}
    ${first.data && !list.length && html`<${Empty} icon="history">還沒有播放記錄。<//>`}
    <ul class="list">${list.map((h) => {
      const day = dayLabel(h.updated_at);
      const head = day !== lastDay;
      lastDay = day;
      return html`<li key=${h.play_id}>
        ${head && html`<div class="day-head">${day}</div>`}
        <button class="row plain wide" onClick=${() => playInAlbum({ ...h, position_ms: 0, finished: false })}>
          <${Cover} id=${h.cover_id} size=${96} className="thumb" />
          <span class="grow track-text"><span class="title">${h.title}</span>
            <span class="sub">${[h.artist || '未知歌手', h.album].filter(Boolean).join(' · ')}</span></span>
          <span class="sub">${clock(h.updated_at)}</span>
        </button></li>`;
    })}</ul>
    ${first.data && list.length >= 50 && !more.done && html`<div class="actions center">
      <button class="btn tonal" disabled=${more.busy} onClick=${loadMore}>載入更多</button></div>`}`;
}

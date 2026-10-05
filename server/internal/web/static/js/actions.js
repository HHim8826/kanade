import { useState } from '../vendor/hooks.module.js';
import { api, get, post } from './api.js';
import { enqueue, playNext } from './player.js';
import { go } from './router.js';
import { createStore, useStore } from './store.js';
import { Cover, Dialog, ErrorBox, Spinner, html, openMenu, showDialog, toast, useLoad } from './ui.js';
import { editTrack } from './views/organize.js';

// Actions shared by every list of songs: favorites, the track menu, adding to playlists.

// ---- favorites ----
// All favorite IDs are loaded once, so any row, the album page and the player can show the heart
// without each query carrying a flag.
export const favs = createStore({ tracks: new Set(), albums: new Set() });

export function loadFavorites() {
  return get('/favorites/ids').then((r) => favs.set({ tracks: new Set(r.tracks), albums: new Set(r.albums) }), () => {});
}

export const useFav = (kind, id) => useStore(favs, (s) => s[kind + 's'].has(id));

export async function toggleFav(kind, id) {
  const key = kind + 's';
  const on = !favs.get()[key].has(id);
  const apply = (value) => favs.set((s) => {
    const set = new Set(s[key]);
    if (value) set.add(id);
    else set.delete(id);
    return { [key]: set };
  });
  apply(on);
  try {
    await api(on ? 'PUT' : 'DELETE', `/favorites/${key}/${id}`);
    toast(on ? '已加入收藏' : '已從收藏移除');
  } catch (e) {
    apply(!on);
    toast(e.message, 'error');
  }
}

// ---- menus ----

// trackMenu opens the "more" menu of one queue item; extra entries go at the end.
export function trackMenu(e, item, extra = []) {
  const fav = item.trackId && favs.get().tracks.has(item.trackId);
  openMenu(e, [
    { icon: 'playNext', label: '下一首播放', onClick: () => playNext(item) },
    { icon: 'queue', label: '加入佇列', onClick: () => enqueue(item) },
    item.trackId && { icon: 'playlistAdd', label: '加入歌單…', onClick: () => addToPlaylist([item]) },
    item.trackId && { icon: fav ? 'favorite' : 'favoriteOff', label: fav ? '取消收藏' : '收藏', onClick: () => toggleFav('track', item.trackId) },
    item.albumId && { icon: 'album', label: '前往專輯', onClick: () => go('album/' + item.albumId) },
    item.trackId && { icon: 'edit', label: '編輯資訊…', onClick: () => editTrack(item.trackId) },
    ...extra,
  ]);
}

// ---- playlists ----

export function addToPlaylist(items) {
  showDialog((close) => html`<${AddToPlaylist} items=${items} close=${close} />`);
}

function AddToPlaylist({ items, close }) {
  const lists = useLoad(() => get('/playlists?plain=1'), []); // smart playlists pick their songs themselves (review #162)
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const payload = items.map((q) => ({ track_id: q.trackId, album_id: q.albumId || 0, asset_id: q.assetId }));
  const run = async (f) => {
    setBusy(true);
    try {
      await f();
      close();
    } catch (e) {
      toast(e.message, 'error');
      setBusy(false);
    }
  };
  const addTo = (p) => run(async () => {
    await post(`/playlists/${p.id}/items`, { items: payload });
    toast(`已加入「${p.name}」`);
  });
  const create = (e) => {
    e.preventDefault();
    if (!name.trim()) return;
    run(async () => {
      const p = await post('/playlists', { name, items: payload });
      toast(`已建立「${p.name}」`);
    });
  };
  return html`<${Dialog} title=${items.length > 1 ? `將 ${items.length} 首加入歌單` : '加入歌單'} onClose=${close}
      actions=${html`<button class="btn text" onClick=${close}>取消</button>`}>
    <form class="inline-form" onSubmit=${create}>
      <input placeholder="新歌單名稱" value=${name} maxlength="200" onInput=${(e) => setName(e.target.value)} aria-label="新歌單名稱" />
      <button class="btn tonal" disabled=${busy || !name.trim()}>建立並加入</button>
    </form>
    ${lists.loading ? html`<${Spinner} />` : html`<${ErrorBox} error=${lists.error} onRetry=${lists.reload} />`}
    ${lists.data && html`<ul class="list">${lists.data.map((p) => html`<li key=${p.id}>
      <button class="row plain wide" disabled=${busy} onClick=${() => addTo(p)}>
        <${Cover} id=${p.cover_id} size=${96} className="thumb" />
        <span class="grow title">${p.name}</span><span class="sub">${p.tracks} 首</span>
      </button></li>`)}</ul>`}
  <//>`;
}

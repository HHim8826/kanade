import { useEffect, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { addToPlaylist, favs } from '../actions.js';
import { enqueue, fromEntry, playNext, playQueue } from '../player.js';
import { go } from '../router.js';
import { Cover, Dialog, Empty, ErrorBox, Icon, Spinner, html, showDialog, toast, useLoad } from '../ui.js';
import { Field, KindPicker, changed, done, useRunner } from './organize.js';
import { linkAlbumsWorks } from './works.js';

// Acting on several albums or songs at once (review #83): the selection bar's actions and their
// dialogs. The server checks every album and song first and makes each change one edit, which the
// toast (and the edit log) can undo; deleting songs for good is said to be final, and reports the
// songs it could not delete so they can be tried again.

const report = (e) => toast(e.message, 'error');

// setFavorites marks albums or songs favorites (or not) in one request, and the hearts follow.
async function setFavorites(kind, ids, on) {
  try {
    await post('/favorites/batch', { [kind + 's']: ids, on });
    favs.set((s) => {
      const set = new Set(s[kind + 's']);
      for (const id of ids) on ? set.add(id) : set.delete(id);
      return { [kind + 's']: set };
    });
    toast(on ? `已收藏 ${ids.length} ${kind === 'album' ? '張專輯' : '首歌'}` : `已取消收藏 ${ids.length} ${kind === 'album' ? '張專輯' : '首歌'}`);
  } catch (e) {
    report(e);
  }
}

const Btn = ({ icon, label, onClick, danger }) => html`<button class=${'btn ' + (danger ? 'text danger-text' : 'tonal')} onClick=${onClick}>
  ${icon && html`<${Icon} name=${icon} />`}${label}</button>`;

// ---- albums ----

// albumItems are the songs of albums, album by album in the order given.
async function albumItems(ids) {
  const list = await Promise.all(ids.map((id) => get('/albums/' + id)));
  return list.flatMap((a) => a.entries.map((e) => fromEntry(e, a)));
}

export function AlbumActions({ sel }) {
  const ids = sel.keys;
  const chosen = sel.chosen();
  const songs = (f) => albumItems(ids).then(f, report);
  return html`
    <${Btn} icon="play" label="播放" onClick=${() => songs((items) => playQueue(items, 0))} />
    <${Btn} icon="playNext" label="下一首播放" onClick=${() => songs(playNext)} />
    <${Btn} icon="queue" label="加入佇列" onClick=${() => songs(enqueue)} />
    <${Btn} icon="playlistAdd" label="加入歌單…" onClick=${() => songs(addToPlaylist)} />
    <${Btn} icon="favorite" label="收藏" onClick=${() => setFavorites('album', ids, true)} />
    <${Btn} icon="favoriteOff" label="取消收藏" onClick=${() => setFavorites('album', ids, false)} />
    <${Btn} icon="folder" label="分類…" onClick=${() => showDialog((close) => html`<${Categorize} ids=${ids} close=${close} />`)} />
    <${Btn} icon="work" label="關聯作品…" onClick=${() => linkAlbumsWorks(ids, chosen)} />
    <${Btn} icon="merge" label="合併…" onClick=${() => showDialog((close) => html`<${MergeAlbums} albums=${chosen} close=${close} onDone=${sel.stop} />`)} />
    <${Btn} icon="edit" label="修改資訊…" onClick=${() => showDialog((close) => html`<${EditAlbums} ids=${ids} close=${close} />`)} />
    <${Btn} icon="delete" label="移除…" danger onClick=${() => showDialog((close) => html`<${RemoveAlbums} ids=${ids} close=${close} onDone=${sel.stop} />`)} />`;
}

// MergeAlbums puts the selected albums into one: one of them, or a new one; as sections (each album
// a section named after it, as for a collection, review #82) or with their own disc numbers. What
// it will do is shown before it is done.
function MergeAlbums({ albums, close, onDone }) {
  const artists = [...new Set(albums.map((a) => a.album_artist).filter(Boolean))];
  const [into, setInto] = useState(0); // 0: a new album
  const [title, setTitle] = useState(albums[0] ? albums[0].title : '');
  const [artist, setArtist] = useState(artists.length === 1 ? artists[0] : 'Various Artists');
  const [sections, setSections] = useState(true);
  const [busy, run] = useRunner();
  const body = { albums: albums.map((a) => a.id), into, title, album_artist: artist, sections };
  const key = JSON.stringify(body);
  const [plan, setPlan] = useState({ key: null });
  useEffect(() => {
    if (!into && !title.trim()) return;
    const t = setTimeout(() => post('/albums/merge', { ...body, preview: true }).then((p) => setPlan({ key, p }), (error) => setPlan({ key, error })), 250);
    return () => clearTimeout(t);
  }, [key]);
  const p = plan.key === key ? plan.p : null;
  const submit = () => run(async () => {
    const res = await post('/albums/merge', body);
    if (done(res, `已合併成「${into ? albums.find((a) => a.id === into).title : title}」`)) {
      close();
      onDone();
      go('album/' + res.album_id);
    }
  });
  const discs = p ? Object.entries(p.sections).sort((a, b) => a[0] - b[0]) : [];
  const count = (d) => (p ? p.moves.filter((m) => m.disc === Number(d) && !m.duplicate).length : 0);
  const dups = p ? p.moves.filter((m) => m.duplicate).length : 0;
  return html`<${Dialog} title=${`合併 ${albums.length} 張專輯`} wide onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !p || (!into && !title.trim())} onClick=${submit}>合併</button>`}>
    <div class="choice-list" role="radiogroup" aria-label="合併到">
      <label class=${into === 0 ? 'on' : ''}><input type="radio" checked=${into === 0} onChange=${() => setInto(0)} />
        <span><b>一張新專輯</b><span class="sub">例如把同一份合集被拆開的專輯合成一張。</span></span></label>
      ${albums.map((a) => html`<label key=${a.id} class=${into === a.id ? 'on' : ''}><input type="radio" checked=${into === a.id} onChange=${() => setInto(a.id)} />
        <span><b>併入「${a.title}」</b><span class="sub">${a.album_artist || '未知歌手'} · ${a.tracks} 首</span></span></label>`)}
    </div>
    ${into === 0 && html`<div class="form-grid">
      <${Field} label="專輯名稱" value=${title} onInput=${setTitle} />
      <${Field} label="專輯歌手" value=${artist} onInput=${setArtist} />
    </div>`}
    <label class="toggle-row">
      <span class="grow"><span class="title">每張專輯作為一個分區</span>
        <span class="sub">以原專輯的名稱命名分區（例如 Episode 1、Episode 2），各自保留曲序；關閉時保留原本的碟號與曲號。</span></span>
      <input type="checkbox" role="switch" checked=${sections} onChange=${(e) => setSections(e.target.checked)} />
    </label>
    ${plan.key === key && plan.error && html`<${ErrorBox} error=${plan.error} />`}
    ${!p && !plan.error && html`<${Spinner} />`}
    ${p && html`<div class="merge-plan">
      <p><b>${p.moves.length - dups} 首</b>會在「${p.target.title}」${dups ? `，${dups} 首已有同一個音檔，不會重複加入` : ''}。
        ${p.emptied.length ? `${p.emptied.length} 張專輯會清空，之後用它們的標籤匯入的檔案也會歸到這裡。` : ''}
        ${p.sidecars ? `${p.sidecars} 個 CUE／LOG 附屬檔案跟著過去。` : ''}音檔不會重新下載或上傳。可在修改紀錄撤回。</p>
      ${discs.length > 0 && html`<ol class="plain-list sections-list">${discs.map(([d, name]) => html`<li key=${d}>${name}（${count(d)} 首）</li>`)}</ol>`}
    </div>`}
  <//>`;
}

// EditAlbums gives the selected albums the same album artist, date or edition; fields not ticked
// stay as they are, and the songs' own artists never change.
function EditAlbums({ ids, close }) {
  const [f, setF] = useState({ album_artist: null, date: null, edition: null });
  const [busy, run] = useRunner();
  const fields = [['album_artist', '專輯歌手'], ['date', '日期'], ['edition', '版本']];
  const body = Object.fromEntries(Object.entries(f).filter(([, v]) => v !== null));
  const submit = () => run(async () => {
    if (done(await post('/albums/edit', { albums: ids, ...body }), `已修改 ${ids.length} 張專輯`)) close();
  });
  return html`<${Dialog} title=${`修改 ${ids.length} 張專輯的資訊`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !Object.keys(body).length} onClick=${submit}>儲存</button>`}>
    <p class="hint">勾選要改的欄位，選取的專輯都會改成同一個值；沒勾的不變。改專輯歌手不會改動每首歌自己的歌手。</p>
    ${fields.map(([k, label]) => html`<div key=${k} class="batch-field">
      <label class="check"><input type="checkbox" checked=${f[k] !== null} onChange=${(e) => setF({ ...f, [k]: e.target.checked ? '' : null })} />${label}</label>
      <input value=${f[k] ?? ''} disabled=${f[k] === null} aria-label=${label} onInput=${(e) => setF({ ...f, [k]: e.target.value })} />
    </div>`)}
  <//>`;
}

// RemoveAlbums takes the selected albums away: only the albums (their songs stay, undoable), or
// with the songs only on them, deleted for good.
function RemoveAlbums({ ids, close, onDone }) {
  const [mode, setMode] = useState('keep');
  const [busy, run] = useRunner();
  const submit = () => run(async () => {
    const res = await post('/albums/remove', { albums: ids, delete_tracks: mode === 'delete' });
    if (mode === 'delete') {
      changed();
      const failed = Object.keys(res.failed || {}).length;
      toast(`已移除 ${ids.length} 張專輯，永久刪除 ${res.deleted.length} 首歌${failed ? `；${failed} 首沒能刪除` : ''}${res.trash_failed ? '；有音檔沒能移到 Drive 垃圾桶' : ''}`,
        failed ? 'error' : 'info');
    } else {
      done(res, `已移除 ${ids.length} 張專輯，歌曲仍在「歌曲」分頁`);
    }
    close();
    onDone();
  });
  return html`<${Dialog} title=${`移除 ${ids.length} 張專輯`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class=${'btn filled' + (mode === 'delete' ? ' danger' : '')} disabled=${busy} onClick=${submit}>${mode === 'delete' ? '永久刪除' : '移除'}</button>`}>
    <div class="choice-list" role="radiogroup">
      <label class=${mode === 'keep' ? 'on' : ''}><input type="radio" checked=${mode === 'keep'} onChange=${() => setMode('keep')} />
        <span><b>只移除專輯</b><span class="sub">歌曲留在「歌曲」分頁，音檔不動。可在修改紀錄一次撤回。</span></span></label>
      <label class=${mode === 'delete' ? 'on' : ''}><input type="radio" checked=${mode === 'delete'} onChange=${() => setMode('delete')} />
        <span><b>連同歌曲永久刪除</b><span class="sub">只在這些專輯的歌曲從曲庫刪除，音檔移到 Google Drive 垃圾桶（30 天內可在 Drive 還原），無法在 Kanade 撤回。也收錄在其他專輯的歌曲不受影響。</span></span></label>
    </div>
  <//>`;
}

// ---- songs ----

// SongActions acts on selected songs (queue items, remembered by the selection). On an album page
// (album), entries can be moved to another section or album, or taken off it.
export function SongActions({ sel, album }) {
  const chosen = sel.chosen();
  const ids = [...new Set(chosen.map((it) => it.trackId))];
  const removeFrom = () => post(`/albums/${album.id}/remove`, { entries: chosen.map((it) => it.entryId) })
    .then((res) => { if (done(res, `已從專輯移除 ${chosen.length} 首，歌曲仍在「歌曲」分頁`)) sel.stop(); }, report);
  return html`
    <${Btn} icon="play" label="播放" onClick=${() => playQueue(chosen, 0)} />
    <${Btn} icon="playNext" label="下一首播放" onClick=${() => playNext(chosen)} />
    <${Btn} icon="queue" label="加入佇列" onClick=${() => enqueue(chosen)} />
    <${Btn} icon="playlistAdd" label="加入歌單…" onClick=${() => addToPlaylist(chosen)} />
    <${Btn} icon="favorite" label="收藏" onClick=${() => setFavorites('track', ids, true)} />
    <${Btn} icon="favoriteOff" label="取消收藏" onClick=${() => setFavorites('track', ids, false)} />
    <${Btn} icon="album" label=${album ? '移到分區或專輯…' : '放進專輯…'}
      onClick=${() => showDialog((close) => html`<${PlaceSongs} items=${chosen} album=${album} close=${close} onDone=${sel.stop} />`)} />
    <${Btn} icon="edit" label="修改資訊…" onClick=${() => showDialog((close) => html`<${EditSongs} ids=${ids} close=${close} />`)} />
    ${album && html`<${Btn} icon="close" label="從專輯移除" onClick=${removeFrom} />`}
    <${Btn} icon="delete" label="永久刪除…" danger onClick=${() => showDialog((close) => html`<${DeleteSongs} items=${chosen} close=${close} onDone=${sel.stop} />`)} />`;
}

// AlbumSearch finds an album of the library to put songs into.
function AlbumSearch({ initial, onPick, not }) {
  const [q, setQ] = useState(initial);
  const [term, setTerm] = useState(initial);
  useEffect(() => {
    const t = setTimeout(() => setTerm(q), 250);
    return () => clearTimeout(t);
  }, [q]);
  const res = useLoad(() => (term.trim() ? get('/search?q=' + encodeURIComponent(term)) : get('/albums?sort=recent&limit=20')), [term]);
  const list = res.data ? (Array.isArray(res.data) ? res.data : res.data.albums).filter((a) => a.id !== not) : [];
  return html`<label class="search-field small"><${Icon} name="search" /><input type="search" value=${q} placeholder="搜尋專輯" aria-label="搜尋專輯"
      onInput=${(e) => setQ(e.target.value)} /></label>
    ${res.loading ? html`<${Spinner} />` : html`<${ErrorBox} error=${res.error} onRetry=${res.reload} />`}
    ${res.data && !list.length && html`<${Empty} icon="album">找不到專輯<//>`}
    <ul class="list">${list.map((a) => html`<li key=${a.id}><button class="row plain wide" onClick=${() => onPick(a)}>
      <${Cover} id=${a.cover_id} size=${96} className="thumb" />
      <span class="grow track-text"><span class="title">${a.title}</span><span class="sub">${[a.album_artist, `${a.tracks} 首`].filter(Boolean).join(' · ')}</span></span>
    </button></li>`)}</ul>`;
}

// PlaceSongs puts songs into an album and a section of it: another section of this album, an album
// of the library, or a new album. Moved from an album page, the entries leave it; otherwise the songs
// join with their files and stay where they are too.
function PlaceSongs({ items, album, close, onDone }) {
  const [target, setTarget] = useState(album ? { ...album, here: true } : null); // { id, title } or { id: 0 } new
  const [title, setTitle] = useState('');
  const [artist, setArtist] = useState('');
  const [disc, setDisc] = useState(0); // 0: a new section
  const [section, setSection] = useState('');
  const [busy, run] = useRunner();
  const detail = useLoad(() => (target && target.id ? get('/albums/' + target.id) : Promise.resolve(null)), [target && target.id]);
  const d = detail.data;
  const discs = d ? [...new Set(d.entries.map((e) => e.disc_no))] : [];
  const name = (n) => (d && d.sections[n]) || `Disc ${n}`;
  const submit = () => run(async () => {
    const body = { album: target.id, title, album_artist: artist, disc, section };
    if (album) body.entries = items.map((it) => it.entryId);
    else body.tracks = [...new Set(items.map((it) => it.trackId))];
    const res = await post('/tracks/place', body);
    if (done(res, `已把 ${items.length} 首放進「${target.id ? target.title : title}」`)) {
      close();
      onDone();
    }
  });
  if (!target) {
    return html`<${Dialog} title=${`把 ${items.length} 首放進專輯`} onClose=${close} actions=${html`<button class="btn text" onClick=${close}>取消</button>`}>
      <div class="actions"><button class="btn tonal" onClick=${() => setTarget({ id: 0 })}><${Icon} name="add" />新專輯…</button></div>
      <${AlbumSearch} initial="" onPick=${(a) => { setTarget(a); setDisc(0); }} not=${album && album.id} />
    <//>`;
  }
  return html`<${Dialog} title=${`把 ${items.length} 首放進「${target.id ? target.title : '新專輯'}」`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${() => setTarget(null)}>${target.here ? '改放其他專輯…' : '返回'}</button>
      <span class="grow"></span>
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || (!target.id && !title.trim())} onClick=${submit}>${album && target.here ? '移動' : '放進去'}</button>`}>
    ${!target.id && html`<div class="form-grid">
      <${Field} label="專輯名稱" value=${title} onInput=${setTitle} autofocus />
      <${Field} label="專輯歌手" value=${artist} onInput=${setArtist} />
    </div>`}
    ${target.id ? html`<label class="field">分區
      <select value=${disc} onChange=${(e) => setDisc(Number(e.target.value))}>
        ${discs.map((n) => html`<option key=${n} value=${n}>${name(n)}</option>`)}
        <option value="0">新的分區（接在最後）</option>
      </select></label>` : ''}
    <${Field} label=${disc ? '分區名稱（留空則不變）' : '新分區的名稱（可留空）'} value=${section} onInput=${setSection} placeholder="例如 Episode 1、Bonus" />
    <p class="hint">${album && target.id === album.id ? '選取的歌曲移到這張專輯的另一個分區，' : album ? '選取的收錄從這張專輯移過去，' : '歌曲以原本的音檔加入，原本所在的專輯不變，'}
      依選取的順序接在分區最後。專輯已有同一個音檔的不會重複加入。可在修改紀錄撤回。</p>
  <//>`;
}

// EditSongs gives the selected songs the same artist or kind; fields not ticked stay.
function EditSongs({ ids, close }) {
  const [artist, setArtist] = useState(null);
  const [kind, setKind] = useState('');
  const [busy, run] = useRunner();
  const body = { ...(artist !== null ? { artist } : {}), ...(kind ? { kind } : {}) };
  const submit = () => run(async () => {
    if (done(await post('/tracks/edit', { tracks: ids, ...body }), `已修改 ${ids.length} 首歌`)) close();
  });
  return html`<${Dialog} title=${`修改 ${ids.length} 首歌的資訊`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !Object.keys(body).length} onClick=${submit}>儲存</button>`}>
    <div class="batch-field">
      <label class="check"><input type="checkbox" checked=${artist !== null} onChange=${(e) => setArtist(e.target.checked ? '' : null)} />歌手</label>
      <input value=${artist ?? ''} disabled=${artist === null} aria-label="歌手" onInput=${(e) => setArtist(e.target.value)} />
    </div>
    <${KindPicker} value=${kind} onChange=${setKind} allowKeep />
  <//>`;
}

// DeleteSongs deletes songs for good; those it could not delete are listed and can be tried again.
function DeleteSongs({ items, close, onDone }) {
  const [left, setLeft] = useState(items);
  const [failed, setFailed] = useState(null);
  const [busy, run] = useRunner();
  const submit = () => run(async () => {
    const res = await post('/tracks/delete', { tracks: [...new Set(left.map((it) => it.trackId))] });
    changed();
    const bad = Object.entries(res.failed);
    if (!bad.length) {
      toast(`已永久刪除 ${res.deleted.length} 首歌${res.trash_failed ? '；有音檔沒能移到 Drive 垃圾桶' : ''}`);
      close();
      onDone();
      return;
    }
    setFailed(Object.fromEntries(bad));
    setLeft(left.filter((it) => res.failed[it.trackId]));
  });
  return html`<${Dialog} title=${`永久刪除 ${left.length} 首歌`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>${failed ? '關閉' : '取消'}</button>
      <button class="btn filled danger" disabled=${busy || !left.length} onClick=${submit}>${failed ? '重試沒刪掉的' : '永久刪除'}</button>`}>
    ${failed
      ? html`<p class="state-failed">${left.length} 首沒能刪除：</p>
        <ul class="plain-list">${left.map((it) => html`<li key=${it.trackId}>${it.title}：${failed[it.trackId]}</li>`)}</ul>`
      : html`<p>要永久刪除這 ${left.length} 首歌嗎？</p>
        <p class="hint">會一併刪除它們在所有專輯的收錄、歌單項目、收藏、歌詞與播放記錄。沒有其他歌曲使用的音檔會移到 Google Drive 垃圾桶（30 天內可在 Drive 還原）。這個操作無法在 Kanade 撤回。</p>`}
  <//>`;
}

// ---- sections (review #82) ----

export function editSections(album) {
  showDialog((close) => html`<${EditSections} album=${album} close=${close} />`);
}

function EditSections({ album, close }) {
  const discs = [...new Set(album.entries.map((e) => e.disc_no))];
  const [names, setNames] = useState(() => Object.fromEntries(discs.map((d) => [d, album.sections[d] || ''])));
  const [busy, run] = useRunner();
  const submit = () => run(async () => {
    if (done(await api('PUT', `/albums/${album.id}/sections`, { sections: names }), '已更新區段名稱')) close();
  });
  return html`<${Dialog} title="區段名稱" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy} onClick=${submit}>儲存</button>`}>
    <p class="hint">每個碟號可以取名字（例如 Episode 1），專輯頁會顯示這個名字；留空則顯示 Disc N。</p>
    ${discs.map((d) => html`<${Field} key=${d} label=${`Disc ${d}（${album.entries.filter((e) => e.disc_no === d).length} 首）`}
      value=${names[d]} onInput=${(v) => setNames({ ...names, [d]: v })} />`)}
  <//>`;
}

// ---- categories (review #92) ----

// Categorize puts albums (ids) into categories and takes them out of others in one edit: a box per
// category, checked when every album is in it and half-checked when some are; a new category can be
// made in the same edit. Each box goes round what it can do and back to how it was, and the change
// to be made is listed before it is saved: what is shown is what is sent (reviews #114, #115).
export function Categorize({ ids, close, onDone }) {
  const cats = useLoad(() => get('/categories?albums=' + ids.join(',')), [ids.join(',')]);
  const [want, setWant] = useState({}); // category ID -> 'all' (put all in) / 'none' (take all out); missing: as it is
  const [fresh, setFresh] = useState('');
  const [busy, run] = useRunner();
  if (cats.loading) return html`<${Dialog} title="分類" onClose=${close}><${Spinner} /><//>`;
  if (cats.error) return html`<${Dialog} title="分類" onClose=${close}><${ErrorBox} error=${cats.error} onRetry=${cats.reload} /><//>`;
  const list = cats.data.categories;
  const n = ids.length;
  const was = (c) => (c.selected === n ? 'all' : c.selected === 0 ? 'none' : 'some');
  const stateOf = (c) => want[c.id] || was(c);
  // as it is → all in → all out → as it is; a state that is how it was is no change.
  const toggle = (c) => {
    const order = was(c) === 'some' ? ['some', 'all', 'none'] : was(c) === 'all' ? ['all', 'none'] : ['none', 'all'];
    const next = order[(order.indexOf(stateOf(c)) + 1) % order.length];
    const w = { ...want };
    if (next === was(c)) delete w[c.id];
    else w[c.id] = next;
    setWant(w);
  };
  const name = fresh.trim().replace(/\s+/g, ' ');
  const existing = name && list.find((c) => c.name.toLowerCase() === name.toLowerCase());
  const useExisting = () => {
    const w = { ...want };
    if (was(existing) === 'all') delete w[existing.id];
    else w[existing.id] = 'all';
    setWant(w);
    setFresh('');
  };
  const add = list.filter((c) => want[c.id] === 'all').map((c) => c.id);
  const remove = list.filter((c) => want[c.id] === 'none').map((c) => c.id);
  const create = name && !existing ? name : '';
  const changes = [
    ...list.filter((c) => want[c.id]).map((c) => (want[c.id] === 'all'
      ? `放入「${c.name}」：${n - c.selected} 張${c.selected ? `（另 ${c.selected} 張已在裡面）` : ''}`
      : `移出「${c.name}」：${c.selected} 張`)),
    create && `新增「${create}」並放入 ${n} 張`,
  ].filter(Boolean);
  const submit = () => run(async () => {
    const res = await post('/albums/categorize', { albums: ids, add, remove, create });
    if (done(res, n === 1 ? '已更新專輯的分類' : `已更新 ${n} 張專輯的分類`)) {
      close();
      onDone && onDone();
    }
  });
  const status = (c) => {
    const st = stateOf(c);
    if (want[c.id]) return st === 'all' ? `→ 全部放入（${n === 1 ? '這張' : `${n} 張`}）` : `→ 全部移出（${c.selected} 張）`;
    if (n === 1) return st === 'all' ? '在這個分類' : '不在這個分類';
    return st === 'all' ? `所選 ${n} 張都在` : st === 'none' ? `所選 ${n} 張都不在` : `所選 ${n} 張中有 ${c.selected} 張在，維持原樣`;
  };
  return html`<${Dialog} title=${n === 1 ? '分類' : `${n} 張專輯的分類`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !changes.length || !!existing} onClick=${submit}>儲存</button>`}>
    <p class="hint">點分類切換：全部放入、全部移出，再點一次回到原樣。分類只影響瀏覽，不會改動專輯、歌曲或音檔，可在修改紀錄撤回。</p>
    ${list.length ? html`<ul class="category-picks">${list.map((c) => {
      const st = stateOf(c);
      return html`<li key=${c.id}><label class=${'check' + (want[c.id] ? ' changed' : '')}>
        <input type="checkbox" checked=${st === 'all'} ref=${(el) => el && (el.indeterminate = st === 'some')} onChange=${() => toggle(c)} />
        <span class="grow"><span>${c.name}</span><span class="sub">${status(c)}</span></span><span class="sub">共 ${c.albums} 張</span></label></li>`;
    })}</ul>` : html`<p class="sub">還沒有分類，在下面輸入名稱新增一個。</p>`}
    <${Field} label="新增分類並放入" value=${fresh} onInput=${setFresh} placeholder="例如：狼と香辛料" />
    ${existing && html`<div class="hint-row"><span class="hint">已有「${existing.name}」，不會再新增一個。</span>
      <button class="btn tonal" onClick=${useExisting}>放入這個分類</button></div>`}
    ${changes.length > 0 && html`<div class="change-summary" aria-live="polite"><b>儲存後會：</b>
      <ul>${changes.map((t) => html`<li key=${t}>${t}</li>`)}</ul></div>`}
  <//>`;
}

import { useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { playBookmark } from '../player.js';
import { Cover, Dialog, Empty, ErrorBox, IconButton, Spinner, fmtTime, html, openMenu, showDialog, toast, useLoad } from '../ui.js';
import { Field, useRunner } from './organize.js';

// Bookmarks (review #98): named places in songs, kept on the server apart from where playback last
// stopped. A bookmark whose song's file changed since (removed, cut again) is shown as it was and
// plays only when asked, so it never jumps to the same second of other audio unawares.

// BookmarkDialog names a new bookmark at a place in item (at, ms), or renames one (bookmark).
export function BookmarkDialog({ close, item, at, bookmark, onSaved }) {
  const [name, setName] = useState(bookmark ? bookmark.name : `書籤 ${fmtTime(at)}`);
  const [note, setNote] = useState(bookmark ? bookmark.note : '');
  const [busy, run] = useRunner();
  const submit = () => run(async () => {
    if (bookmark) await api('PATCH', '/bookmarks/' + bookmark.id, { name, note });
    else await post('/bookmarks', { asset_id: item.assetId, position_ms: at, name, note });
    toast(bookmark ? '已更新書籤' : `已在 ${fmtTime(at)} 加上書籤`);
    close();
    onSaved && onSaved();
  });
  return html`<${Dialog} title=${bookmark ? '編輯書籤' : `在 ${fmtTime(at)} 加書籤`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !name.trim()} onClick=${submit}>儲存</button>`}>
    <${Field} label="名稱" value=${name} onInput=${setName} placeholder="例如：下次從這裡" autofocus />
    <${Field} label="備註" value=${note} onInput=${setNote} multiline rows=${2} placeholder="選填" />
  <//>`;
}

// BookmarkList lists bookmarks; one plays at its place, and can be renamed or deleted. withSong
// names the song of each (the list of all bookmarks).
export function BookmarkList({ items, onChanged, withSong }) {
  const play = (b) => {
    if (b.moved) {
      showDialog((close) => html`<${MovedBookmark} b=${b} close=${close} />`);
      return;
    }
    playBookmark(b);
  };
  const menu = (e, b) => openMenu(e, [
    { icon: 'edit', label: '編輯…', onClick: () => showDialog((close) => html`<${BookmarkDialog} close=${close} bookmark=${b} onSaved=${onChanged} />`) },
    { icon: 'delete', label: '刪除', onClick: () => api('DELETE', '/bookmarks/' + b.id).then(() => { toast('已刪除書籤'); onChanged(); },
      (err) => toast(err.message, 'error')) },
  ]);
  return html`<ul class="items bookmark-list">${items.map((b) => html`<li key=${b.id} class="bookmark-row">
    <button class="row grow" disabled=${!b.track} onClick=${() => play(b)} aria-label=${`從 ${fmtTime(b.position_ms)} 播放「${b.name}」`}>
      ${withSong && html`<${Cover} id=${b.track && b.track.cover_id} size=${300} className="thumb" alt="" />`}
      <span class="bm-time">${fmtTime(b.position_ms)}</span>
      <span class="track-text"><span class="title">${b.name}</span>
        <span class="sub">${[withSong && (b.track ? b.track.title : '（歌曲已移除）'), b.note].filter(Boolean).join(' · ')}</span>
        ${b.moved && html`<span class="sub warn-text">原檔已變動，位置可能對不上</span>`}</span>
    </button>
    <${IconButton} icon="more" label="書籤選項" onClick=${(e) => menu(e, b)} />
  </li>`)}</ul>`;
}

function MovedBookmark({ b, close }) {
  return html`<${Dialog} title="原檔已變動" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" onClick=${() => { playBookmark(b, true); close(); }}>仍從 ${fmtTime(b.position_ms)} 播放</button>`}>
    <p>這個書籤建立時的音檔已經不是這首歌現在的檔案（可能被移除或重新分軌），${fmtTime(b.position_ms)} 不一定是當時的內容。</p>
  <//>`;
}

// BookmarksTab lists every bookmark, the latest first: the bookmarks tab of "my" page (review #190).
export function BookmarksTab() {
  const data = useLoad(() => get('/bookmarks'), []);
  return html`<p class="hint">在正在播放的畫面按「加書籤」記住一個位置；書籤存在伺服器，換裝置也看得到。</p>
    ${data.loading && !data.data && html`<${Spinner} />`}
    ${data.error && html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`}
    ${data.data && (data.data.length ? html`<${BookmarkList} items=${data.data} onChanged=${data.reload} withSong />`
      : html`<${Empty} icon="bookmark">還沒有書籤。<//>`)}`;
}

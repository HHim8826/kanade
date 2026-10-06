import { get } from '../api.js';
import { href } from '../router.js';
import { ErrorBox, Spinner, html, useLoad } from '../ui.js';
import { CollectionsTab } from './bangumi.js';
import { BookmarksTab } from './bookmarks.js';
import { FavoritesTab, HistoryTab, PlaylistsTab } from './collections.js';
import { useLibRev } from './organize.js';
import { StatsTab } from './stats.js';

// "My" page (review #190): what is one's own, apart from the library, which is the music itself.
// The tab is in the address (#/me/favorites), and so is what each tab shows.
const tabs = [['playlists', '歌單'], ['favorites', '收藏'], ['bookmarks', '書籤'], ['history', '記錄'], ['stats', '聆聽'], ['bangumi', 'Bangumi']];

export function Me({ tab }) {
  if (!tabs.some(([k]) => k === tab)) tab = 'playlists';
  return html`<section>
    <div class="page-head"><h1 class="page-title">我的</h1></div>
    <nav class="tabs" role="tablist">
      ${tabs.map(([k, label]) => html`<a key=${k} role="tab" aria-selected=${k === tab} class=${k === tab ? 'active' : ''} href=${href('me/' + k)}>${label}</a>`)}
    </nav>
    ${tab === 'playlists' && html`<${Loaded} path="/playlists">${(lists) => html`<${PlaylistsTab} lists=${lists} />`}<//>`}
    ${tab === 'favorites' && html`<${Loaded} path="/favorites">${(data) => html`<${FavoritesTab} data=${data} />`}<//>`}
    ${tab === 'bookmarks' && html`<${BookmarksTab} />`}
    ${tab === 'history' && html`<${HistoryTab} />`}
    ${tab === 'stats' && html`<${StatsTab} />`}
    ${tab === 'bangumi' && html`<${CollectionsTab} />`}
  </section>`;
}

// Loaded shows what path answers, read again when the library changes.
function Loaded({ path, children }) {
  const rev = useLibRev();
  const data = useLoad(() => get(path), [path], rev);
  return html`${data.loading && !data.data ? html`<${Spinner} />` : html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`}
    ${data.data && children(data.data)}`;
}

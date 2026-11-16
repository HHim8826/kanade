import { render } from '../vendor/preact.module.js';
import { useEffect, useLayoutEffect, useRef, useState } from '../vendor/hooks.module.js';
import { get, setUnauthorizedHandler } from './api.js';
import { loadFavorites } from './actions.js';
import { player, resetPlayer, restoreSession } from './player.js';
import { checkPresence, stopPresence } from './presence.js';
import { control, startRemote, stopRemote } from './remote.js';
import './scrollbars.js';
import './shortcuts.js';
import { loadPlaces } from './places.js';
import { setPageTitle, site } from './site.js';
import { useStore } from './store.js';
import { UpdateBar, watchUpdates } from './update.js';
import { href, useRoute } from './router.js';
import { Boundary, DialogHost, Icon, MenuHost, Spinner, Toasts, html } from './ui.js';
import { Playlist } from './views/collections.js';
import { Album, Artist, Home, Library, Search } from './views/library.js';
import { CategoryPage } from './views/categories.js';
import { WorkPage } from './views/works.js';
import { Me } from './views/me.js';
import { Login } from './views/login.js';
import { Feeds } from './views/feeds.js';
import { ImportReview } from './views/importreview.js';
import { Edits, Missing, Organize, SameArtists, Splits } from './views/organize.js';
import { NowPlaying, PlayerBar } from './views/player.js';
import { RemoteNowPlaying } from './views/remote.js';
import { Settings } from './views/settings.js';
import { Tasks } from './views/tasks.js';
import { Upload } from './views/upload.js';

// Five places at the bottom of a phone (review 2.3); settings is on "my" page there, and at the
// foot of the rail on a wide screen.
const nav = [
  ['', 'home', '首頁'],
  ['search', 'search', '搜尋'],
  ['library', 'library', '曲庫'],
  ['me', 'me', '我的'],
  ['tasks', 'tasks', '任務'],
  ['settings', 'settings', '設定'],
];

// What each kind of page is called in the tab's title, until it names itself.
const titles = { '': '首頁', search: '搜尋', library: '曲庫', album: '專輯', category: '分類', work: '作品', artist: '歌手',
  playlist: '歌單', me: '我的', edits: '修改紀錄', splits: '拆分歌手名稱', same: '同一位歌手', missing: '遺失的檔案', organize: '待整理',
  tasks: '任務', upload: '上傳音樂', import: '匯入預覽', feeds: 'RSS 訂閱', settings: '設定' };

function Page({ route, onLogout }) {
  const [name, arg] = route.parts;
  switch (name) {
    case undefined: return html`<${Home} />`;
    case 'search': return html`<${Search} />`;
    case 'library': return html`<${Library} key=${arg || 'albums'} tab=${arg || 'albums'} filter=${route.query.get('filter') || ''} find=${route.query.get('q') || ''} />`;
    case 'album': return html`<${Album} id=${arg} />`;
    case 'category': return html`<${CategoryPage} id=${arg} />`;
    case 'work': return html`<${WorkPage} id=${arg} />`;
    case 'artist': return html`<${Artist} id=${arg} name=${route.query.get('name')} />`;
    case 'playlist': return html`<${Playlist} id=${arg} />`;
    case 'me': return html`<${Me} tab=${arg || 'playlists'} />`;
    case 'edits': return html`<${Edits} />`;
    case 'splits': return html`<${Splits} />`;
    case 'same': return html`<${SameArtists} />`;
    case 'missing': return html`<${Missing} />`;
    case 'organize': return html`<${Organize} />`;
    case 'tasks': return html`<${Tasks} />`;
    case 'upload': return html`<${Upload} />`;
    case 'import': return html`<${ImportReview} id=${arg} />`;
    case 'feeds': return html`<${Feeds} tab=${route.query.get('tab') || 'items'} q=${route.query.get('q') || ''} />`;
    case 'settings': return html`<${Settings} onLogout=${onLogout} section=${route.query.get('s')} />`;
    default: return html`<p>找不到頁面</p>`;
  }
}

function App() {
  const [auth, setAuth] = useState('checking');
  const route = useRoute();
  useStore(site, (v) => v.name); // what the pages say names the site: renamed, they say it again
  useEffect(() => {
    setUnauthorizedHandler(() => { // logged out elsewhere: nothing may keep playing (review #14)
      resetPlayer(false, true);
      setAuth('out');
    });
    // A server slow to answer still lets the pages show and say what fails (review #149).
    get('/status', { allow401: true, timeout: 15000 }).then(() => setAuth('in'), (e) => setAuth(e.status === 401 ? 'out' : 'in'));
  }, []);
  // Leaving a page closes the full-screen player, so it never hides the page you went to.
  useEffect(() => {
    player.set({ nowPlayingOpen: false });
    control.set({ open: false });
  }, [route]);
  useLayoutEffect(() => setPageTitle(titles[route.parts[0] || ''] || ''), [route.raw]);
  // Another page: the keyboard and a screen reader go on from its title, not from the link that led
  // there (review 2.12), unless the page put the focus somewhere itself.
  const shown = useRef(route.raw);
  useEffect(() => {
    if (shown.current === route.raw) return;
    shown.current = route.raw;
    if (auth !== 'in') return;
    const t = setTimeout(() => {
      const content = document.getElementById('content');
      if (!content || content.contains(document.activeElement)) return;
      const h = content.querySelector('h1') || content;
      h.tabIndex = -1;
      h.focus({ preventScroll: true });
    }, 0);
    return () => clearTimeout(t);
  }, [route.raw]);
  useEffect(() => {
    if (auth !== 'in') return;
    loadFavorites();
    loadPlaces(); // where the library's files are, for how deleting and missing files are worded (#99)
    restoreSession(); // the queue this browser had, paused where it was
    checkPresence(); // what plays here, told for the other devices and Discord (reviews #135, #138)
    startRemote(); // the other devices, and what they ask of this one (review #138)
    return () => {
      stopRemote();
      stopPresence();
    };
  }, [auth]);
  if (auth === 'checking') return html`<main class="login"><${Spinner} /></main>`;
  if (auth === 'out') return html`<${Login} onLogin=${() => setAuth('in')} /><${Toasts} /><${UpdateBar} />`;
  const section = route.parts[0] || '';
  const active = (key) => key === section || (key === 'library' && ['album', 'artist', 'edits', 'splits', 'same', 'missing', 'organize', 'category', 'work'].includes(section))
    || (key === 'me' && section === 'playlist') || (key === 'tasks' && ['upload', 'import', 'feeds'].includes(section));
  const skip = () => {
    const content = document.getElementById('content');
    const h = content.querySelector('h1') || content;
    h.tabIndex = -1;
    h.focus();
  };
  // The page scrolls on its own above the player bar and the navigation, which keep their room
  // (review #48). On a phone, settings is reached from "my" page, which stands for it there.
  return html`<div class="shell">
    <button class="skip-link" onClick=${skip}>跳到主要內容</button>
    <nav class="nav" aria-label="主要">
      ${nav.map(([key, icon, label]) => html`<a key=${key} href=${href(key)} aria-current=${active(key) ? 'page' : undefined}
        class=${[active(key) && 'active', key === 'settings' && 'nav-settings', key === 'me' && section === 'settings' && 'active-narrow'].filter(Boolean).join(' ')}>
        <span class="nav-icon"><${Icon} name=${icon} /></span><span class="nav-label">${label}</span></a>`)}
    </nav>
    <main class="content" id="content" tabindex="-1"><div class="page"><${Boundary} key=${route.raw}><${Page} route=${route} onLogout=${() => setAuth('out')} /><//></div></main>
    <${PlayerBar} />
    </div>
    <${NowPlaying} />
    <${RemoteNowPlaying} />
    <${MenuHost} />
    <${DialogHost} />
    <${Toasts} />
    <${UpdateBar} />`;
}

watchUpdates(); // a page left open offers the release the server is on now
render(html`<${App} />`, document.getElementById('app'));

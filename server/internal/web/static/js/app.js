import { render } from '../vendor/preact.module.js';
import { useEffect, useState } from '../vendor/hooks.module.js';
import { get, setUnauthorizedHandler } from './api.js';
import { loadFavorites } from './actions.js';
import { player } from './player.js';
import { href, useRoute } from './router.js';
import { Boundary, DialogHost, Icon, MenuHost, Spinner, Toasts, html } from './ui.js';
import { History, Playlist } from './views/collections.js';
import { Album, Artist, Home, Library, Search } from './views/library.js';
import { Login } from './views/login.js';
import { NowPlaying, PlayerBar } from './views/player.js';
import { Settings } from './views/settings.js';
import { Tasks } from './views/tasks.js';
import { Upload } from './views/upload.js';

const nav = [
  ['', 'home', '首頁'],
  ['search', 'search', '搜尋'],
  ['library', 'library', '曲庫'],
  ['tasks', 'tasks', '任務'],
  ['settings', 'settings', '設定'],
];

function Page({ route, onLogout }) {
  const [name, arg] = route.parts;
  switch (name) {
    case undefined: return html`<${Home} />`;
    case 'search': return html`<${Search} />`;
    case 'library': return html`<${Library} tab=${arg || 'albums'} />`;
    case 'album': return html`<${Album} id=${arg} />`;
    case 'artist': return html`<${Artist} id=${arg} name=${route.query.get('name')} />`;
    case 'playlist': return html`<${Playlist} id=${arg} />`;
    case 'history': return html`<${History} />`;
    case 'tasks': return html`<${Tasks} />`;
    case 'upload': return html`<${Upload} />`;
    case 'settings': return html`<${Settings} onLogout=${onLogout} />`;
    default: return html`<p>找不到頁面</p>`;
  }
}

function App() {
  const [auth, setAuth] = useState('checking');
  const route = useRoute();
  useEffect(() => {
    setUnauthorizedHandler(() => setAuth('out'));
    get('/status', { allow401: true }).then(() => setAuth('in'), (e) => setAuth(e.status === 401 ? 'out' : 'in'));
  }, []);
  // Leaving a page closes the full-screen player, so it never hides the page you went to.
  useEffect(() => player.set({ nowPlayingOpen: false }), [route]);
  useEffect(() => {
    if (auth === 'in') loadFavorites();
  }, [auth]);
  if (auth === 'checking') return html`<main class="login"><${Spinner} /></main>`;
  if (auth === 'out') return html`<${Login} onLogin=${() => setAuth('in')} /><${Toasts} />`;
  const section = route.parts[0] || '';
  const active = (key) => key === section || (key === 'library' && ['album', 'artist', 'playlist'].includes(section))
    || (key === 'tasks' && section === 'upload') || (key === '' && section === 'history');
  return html`
    <nav class="nav" aria-label="主要">
      ${nav.map(([key, icon, label]) => html`<a key=${key} href=${href(key)} class=${active(key) ? 'active' : ''} aria-current=${active(key) ? 'page' : undefined}>
        <span class="nav-icon"><${Icon} name=${icon} /></span><span class="nav-label">${label}</span></a>`)}
    </nav>
    <main class="content"><${Boundary} key=${location.hash}><${Page} route=${route} onLogout=${() => setAuth('out')} /><//></main>
    <${PlayerBar} />
    <${NowPlaying} />
    <${MenuHost} />
    <${DialogHost} />
    <${Toasts} />`;
}

render(html`<${App} />`, document.getElementById('app'));

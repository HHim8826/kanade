import { clock, player } from './player.js';
import { control, devices, positionOf, target } from './remote.js';
import { createStore } from './store.js';
import { fmtTime } from './ui.js';

// The site's name and icon, as its owner set them (Settings → 網站). The page as served names them
// in its application-name meta, so the login page shows them before anything is asked of the
// server; a change made here is shown at once (applySite), on other devices once they load again.
const meta = document.querySelector('meta[name="application-name"]');
const INFO = 'kanade.title'; // whether the tab's title says what plays: this browser's own

function loadInfo() {
  try {
    return localStorage.getItem(INFO) === '1';
  } catch {
    return false;
  }
}

export const site = createStore({ name: (meta && meta.content) || 'Kanade', icon: (meta && meta.dataset.icon) || '', info: loadInfo() });
export const siteName = () => site.get().name;
// iconURL is a size of the uploaded icon (icon: its version): 32, 180, 192, 512 or maskable.
export const iconURL = (icon, size) => `/app/site/${icon}/icon-${size}.png`;

export function setTitleInfo(on) {
  site.set({ info: on });
  try {
    localStorage.setItem(INFO, on ? '1' : '0');
  } catch {
    // kept for this visit only
  }
}

// builtIn is a file of this build, where the stylesheet is: the built-in icon's.
const builtIn = (file) => {
  const css = document.querySelector('link[rel="stylesheet"]');
  return ((css && css.getAttribute('href')) || '').replace(/style\.css$/, '') + file;
};
export const builtInIcon = () => builtIn('icon.svg');

// applySite shows the site as the server now has it ({ name, icon }) in this page: its name and the
// tab's and home screen's icons.
export function applySite({ name, icon }) {
  site.set({ name, icon });
  if (meta) {
    meta.content = name;
    meta.dataset.icon = icon;
  }
  document.querySelectorAll('link[rel="icon"], link[rel="apple-touch-icon"]').forEach((l) => l.remove());
  const links = icon
    ? [['icon', iconURL(icon, 32), '32x32', 'image/png'], ['icon', iconURL(icon, 192), '192x192', 'image/png'], ['apple-touch-icon', iconURL(icon, 180)]]
    : [['icon', builtInIcon(), '', 'image/svg+xml'], ['apple-touch-icon', builtIn('icon-180.png')]];
  for (const [rel, href, sizes, type] of links) {
    const l = document.createElement('link');
    l.rel = rel;
    l.href = href;
    if (sizes) l.sizes = sizes;
    if (type) l.type = type;
    document.head.append(l);
  }
}

// The tab's title: the site's name, and while there is a song (with info on) where it is and what
// it is, the time first since a narrow tab cuts the end: 「Kanade · ▶ 1:23 / 3:45 · 歌名」. While
// this tab controls another device, the song is that device's, as the player bar shows it (review
// #210).
function title() {
  const { name, info } = site.get();
  if (!info) return name;
  if (control.get().target) {
    const d = target();
    if (!d || (d.state !== 'playing' && d.state !== 'paused')) return name;
    return line(name, d.state === 'playing', positionOf(d), d.duration_ms, d.title);
  }
  const s = player.get(), item = s.queue[s.index];
  if (!item) return name;
  const length = s.duration > 0 ? s.duration * 1000 : item.durationMs; // a queue restored is not loaded yet
  return line(name, s.playing, clock.get().time * 1000, length, item.title);
}

const line = (name, playing, at, length, song) =>
  `${name} · ${playing ? '▶' : '❚❚'} ${fmtTime(at)}${length > 0 ? ' / ' + fmtTime(length) : ''} · ${song}`;

// A device controlled goes on playing with nothing new from it: its place is worked out a second at
// a time.
let ticking = 0;
function showTitle() {
  const t = title();
  if (document.title !== t) document.title = t;
  const d = site.get().info && control.get().target && target();
  const on = !!d && d.state === 'playing';
  if (on && !ticking) ticking = setInterval(showTitle, 1000);
  else if (!on && ticking) {
    clearInterval(ticking);
    ticking = 0;
  }
}

for (const store of [site, player, clock, control, devices]) store.subscribe(showTitle);
showTitle();

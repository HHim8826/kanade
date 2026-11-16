import { useEffect, useState } from '../vendor/hooks.module.js';

// Hash routes (#/album/5) need no server-side routing and survive reloads.
export function parseHash() {
  const raw = location.hash.replace(/^#\/?/, '');
  const [path, query = ''] = raw.split('?');
  return { raw, parts: path.split('/').filter(Boolean).map(decodeURIComponent), query: new URLSearchParams(query) };
}

// keepInAddress puts a page's state in its address without going to it again (no hashchange): a
// page that comes back from history finds it there (review #164).
export function keepInAddress(path) {
  const target = href(path);
  if (location.hash !== target) history.replaceState(history.state, '', target);
}

// Pages that moved to "my" page keep their old addresses (review #190): one leads to where its page
// is now, with what it asked for, in place of it in the history.
const moved = [[/^library\/(playlists|favorites|bangumi)(?=$|[/?])/, 'me/$1'], [/^(stats|history|bookmarks)(?=$|[/?])/, 'me/$1']];
function settled() {
  const raw = location.hash.replace(/^#\/?/, '');
  const to = moved.find(([from]) => from.test(raw));
  if (to) history.replaceState(history.state, '', href(raw.replace(to[0], to[1])));
  return parseHash();
}

// ---- where each page was (review 2.1) ----
// Going back (or forward) to a page puts it where it was left; going somewhere new starts at the
// top. Each entry of the history has a key in its state, and where the page of that entry was
// scrolled to is kept under it, for this tab (a reload keeps it too). The page scrolls in #content,
// not the window; a page whose list is not there yet is put in place once it is long enough.

const PLACES = 'kanade.places';
const places = new Map(readPlaces());
let entry = ''; // the key of the entry shown
let placing = null; // a page being put back in place: { stop }

function readPlaces() {
  try {
    return JSON.parse(sessionStorage.getItem(PLACES)) || [];
  } catch {
    return [];
  }
}

let saving = 0;
function remember(top) {
  places.delete(entry);
  places.set(entry, top);
  if (places.size > 100) places.delete(places.keys().next().value);
  clearTimeout(saving);
  saving = setTimeout(() => {
    try {
      sessionStorage.setItem(PLACES, JSON.stringify([...places]));
    } catch { /* kept for this page only */ }
  }, 300);
}

// enter is called for the entry shown now: its key, made when it has none, and where it was.
function enter() {
  const st = history.state;
  if (st && st.kanadeEntry) {
    entry = st.kanadeEntry;
  } else {
    entry = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
    history.replaceState({ ...(st || {}), kanadeEntry: entry }, '');
  }
  return places.get(entry) || 0;
}

addEventListener('scroll', (e) => {
  if (e.target.id === 'content' && !placing) remember(e.target.scrollTop);
}, { capture: true, passive: true });

// place puts the page at top: at once when it is 0, else once the page is long enough; until then,
// or until the user scrolls or taps, at the top. Given up after wait ms.
function place(top, wait) {
  if (placing) placing.stop();
  const content = document.getElementById('content');
  if (!top) {
    content?.scrollTo(0, 0);
    return;
  }
  const until = Date.now() + wait;
  let frame = 0;
  const stop = () => {
    cancelAnimationFrame(frame);
    for (const ev of ['wheel', 'touchstart', 'keydown', 'pointerdown']) removeEventListener(ev, stop, true);
    if (placing && placing.stop === stop) placing = null;
  };
  for (const ev of ['wheel', 'touchstart', 'keydown', 'pointerdown']) addEventListener(ev, stop, true);
  placing = { stop };
  const tryNow = () => {
    const el = document.getElementById('content');
    if (el && el.scrollHeight - el.clientHeight >= top - 1) {
      el.scrollTo(0, top);
      return stop();
    }
    if (Date.now() > until) return stop();
    frame = requestAnimationFrame(tryNow);
  };
  content?.scrollTo(0, 0);
  tryNow();
}

export function useRoute() {
  const [route, setRoute] = useState(settled);
  useEffect(() => {
    place(enter(), 5000); // a reload: where it was, once the page is back
    const onChange = () => {
      const top = enter();
      setRoute(settled());
      place(top, 3000);
    };
    addEventListener('hashchange', onChange);
    return () => removeEventListener('hashchange', onChange);
  }, []);
  return route;
}

export const href = (path) => '#/' + path.replace(/^\//, '');
export const go = (path) => {
  location.hash = href(path);
};

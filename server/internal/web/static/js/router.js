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

export function useRoute() {
  const [route, setRoute] = useState(settled);
  useEffect(() => {
    const onChange = () => {
      setRoute(settled());
      document.getElementById('content')?.scrollTo(0, 0); // the page scrolls, not the window
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

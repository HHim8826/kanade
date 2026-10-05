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

export function useRoute() {
  const [route, setRoute] = useState(parseHash);
  useEffect(() => {
    const onChange = () => {
      setRoute(parseHash());
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

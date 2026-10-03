import { h } from '../vendor/preact.module.js';
import { useEffect, useErrorBoundary, useRef, useState } from '../vendor/hooks.module.js';
import htm from '../vendor/htm.module.js';
import { coverURL } from './api.js';
import { createStore, useStore } from './store.js';

export const html = htm.bind(h);

// Material Icons paths (Apache-2.0), 24×24 viewBox.
const icons = {
  home: 'M10 20v-6h4v6h5v-8h3L12 3 2 12h3v8z',
  search: 'M15.5 14h-.79l-.28-.27A6.47 6.47 0 0 0 16 9.5 6.5 6.5 0 1 0 9.5 16c1.61 0 3.09-.59 4.23-1.57l.27.28v.79l5 4.99L20.49 19l-4.99-5zm-6 0C7.01 14 5 11.99 5 9.5S7.01 5 9.5 5 14 7.01 14 9.5 11.99 14 9.5 14z',
  library: 'M20 2H8c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h12c1.1 0 2-.9 2-2V4c0-1.1-.9-2-2-2zm-2 5h-3v5.5a2.5 2.5 0 0 1-5 0 2.5 2.5 0 0 1 2.5-2.5c.57 0 1.08.19 1.5.51V5h4v2zM4 6H2v14c0 1.1.9 2 2 2h14v-2H4V6z',
  tasks: 'M3 13h2v-2H3v2zm0 4h2v-2H3v2zm0-8h2V7H3v2zm4 4h14v-2H7v2zm0 4h14v-2H7v2zM7 7v2h14V7H7z',
  settings: 'M3 17v2h6v-2H3zM3 5v2h10V5H3zm10 16v-2h8v-2h-8v-2h-2v6h2zM7 9v2H3v2h4v2h2V9H7zm14 4v-2H11v2h10zm-6-4h2V7h4V5h-4V3h-2v6z',
  play: 'M8 5v14l11-7z',
  pause: 'M6 19h4V5H6v14zm8-14v14h4V5h-4z',
  next: 'M6 18l8.5-6L6 6v12zM16 6v12h2V6h-2z',
  prev: 'M6 6h2v12H6zm3.5 6l8.5 6V6z',
  shuffle: 'M10.59 9.17L5.41 4 4 5.41l5.17 5.17 1.42-1.41zM14.5 4l2.04 2.04L4 18.59 5.41 20 17.96 7.46 20 9.5V4h-5.5zm.33 9.41l-1.41 1.41 3.13 3.13L14.5 20H20v-5.5l-2.04 2.04-3.13-3.13z',
  queue: 'M15 6H3v2h12V6zm0 4H3v2h12v-2zM3 16h8v-2H3v2zM17 6v8.18A3 3 0 0 0 16 14a3 3 0 1 0 3 3V8h3V6h-5z',
  close: 'M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z',
  album: 'M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zm0 14.5a4.5 4.5 0 1 1 0-9 4.5 4.5 0 0 1 0 9zm0-5.5a1 1 0 1 0 0 2 1 1 0 0 0 0-2z',
  person: 'M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zm0 2c-2.67 0-8 1.34-8 4v2h16v-2c0-2.66-5.33-4-8-4z',
  note: 'M12 3v10.55A4 4 0 1 0 14 17V7h4V3h-6z',
  add: 'M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6v2z',
  upload: 'M9 16h6v-6h4l-7-7-7 7h4zm-4 2h14v2H5z',
  download: 'M19 9h-4V3H9v6H5l7 7 7-7zM5 18v2h14v-2H5z',
  passkey: 'M12.65 10C11.83 7.67 9.61 6 7 6c-3.31 0-6 2.69-6 6s2.69 6 6 6c2.61 0 4.83-1.67 5.65-4H17v4h4v-4h2v-4H12.65zM7 14c-1.1 0-2-.9-2-2s.9-2 2-2 2 .9 2 2-.9 2-2 2z',
  logout: 'M17 7l-1.41 1.41L18.17 11H8v2h10.17l-2.58 2.58L17 17l5-5zM4 5h8V3H4c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h8v-2H4V5z',
  back: 'M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z',
  expand: 'M16.59 8.59L12 13.17 7.41 8.59 6 10l6 6 6-6z',
  refresh: 'M17.65 6.35A7.96 7.96 0 0 0 12 4a8 8 0 1 0 7.73 10h-2.08A6 6 0 1 1 12 6c1.66 0 3.14.69 4.22 1.78L13 11h7V4l-2.35 2.35z',
  favorite: 'M12 21.35l-1.45-1.32C5.4 15.36 2 12.28 2 8.5 2 5.42 4.42 3 7.5 3c1.74 0 3.41.81 4.5 2.09C13.09 3.81 14.76 3 16.5 3 19.58 3 22 5.42 22 8.5c0 3.78-3.4 6.86-8.55 11.54L12 21.35z',
  favoriteOff: 'M16.5 3c-1.74 0-3.41.81-4.5 2.09C10.91 3.81 9.24 3 7.5 3 4.42 3 2 5.42 2 8.5c0 3.78 3.4 6.86 8.55 11.54L12 21.35l1.45-1.32C18.6 15.36 22 12.28 22 8.5 22 5.42 19.58 3 16.5 3zm-4.4 15.55l-.1.1-.1-.1C7.14 14.24 4 11.39 4 8.5 4 6.5 5.5 5 7.5 5c1.54 0 3.04.99 3.57 2.36h1.87C13.46 5.99 14.96 5 16.5 5c2 0 3.5 1.5 3.5 3.5 0 2.89-3.14 5.74-7.9 10.05z',
  more: 'M12 8c1.1 0 2-.9 2-2s-.9-2-2-2-2 .9-2 2 .9 2 2 2zm0 2c-1.1 0-2 .9-2 2s.9 2 2 2 2-.9 2-2-.9-2-2-2zm0 6c-1.1 0-2 .9-2 2s.9 2 2 2 2-.9 2-2-.9-2-2-2z',
  playlistAdd: 'M14 10H2v2h12v-2zm0-4H2v2h12V6zm4 8v-4h-2v4h-4v2h4v4h2v-4h4v-2h-4zM2 16h8v-2H2v2z',
  playNext: 'M3 10h11v2H3v-2zm0-4h11v2H3V6zm0 8h7v2H3v-2zm13-1v8l6-4-6-4z',
  lyrics: 'M14 17H4v2h10v-2zm6-8H4v2h16V9zM4 15h16v-2H4v2zM4 5v2h16V5H4z',
  history: 'M13 3a9 9 0 0 0-9 9H1l3.89 3.89.07.14L9 12H6c0-3.87 3.13-7 7-7s7 3.13 7 7-3.13 7-7 7c-1.93 0-3.68-.79-4.94-2.06l-1.42 1.42A8.954 8.954 0 0 0 13 21a9 9 0 0 0 0-18zm-1 5v5l4.28 2.54.72-1.21-3.5-2.08V8H12z',
  delete: 'M6 19c0 1.1.9 2 2 2h8c1.1 0 2-.9 2-2V7H6v12zM19 4h-3.5l-1-1h-5l-1 1H5v2h14V4z',
  edit: 'M3 17.25V21h3.75L17.81 9.94l-3.75-3.75L3 17.25zM20.71 7.04a1 1 0 0 0 0-1.41l-2.34-2.34a1 1 0 0 0-1.41 0l-1.83 1.83 3.75 3.75 1.83-1.83z',
  drag: 'M20 9H4v2h16V9zM4 15h16v-2H4v2z',
  up: 'M4 12l1.41 1.41L11 7.83V20h2V7.83l5.58 5.59L20 12l-8-8-8 8z',
  down: 'M20 12l-1.41-1.41L13 16.17V4h-2v12.17l-5.58-5.59L4 12l8 8 8-8z',
  merge: 'M17 20.41L18.41 19 15 15.59 13.59 17 17 20.41zM7.5 8H11v5.59L5.59 19 7 20.41l6-6V8h3.5L12 3.5 7.5 8z',
  split: 'M14 4l2.29 2.29-2.88 2.88 1.42 1.42 2.88-2.88L20 10V4h-6zm-4 0H4v6l2.29-2.29 4.71 4.7V20h2v-8.41l-5.29-5.3L10 4z',
  restore: 'M13 3a9 9 0 0 0-9 9H1l3.89 3.89.07.14L9 12H6c0-3.87 3.13-7 7-7s7 3.13 7 7-3.13 7-7 7c-1.93 0-3.68-.79-4.94-2.06l-1.42 1.42A8.954 8.954 0 0 0 13 21a9 9 0 0 0 0-18z',
  identify: 'M15.5 14h-.79l-.28-.27A6.47 6.47 0 0 0 16 9.5 6.5 6.5 0 1 0 9.5 16c1.61 0 3.09-.59 4.23-1.57l.27.28v.79l5 4.99L20.49 19l-4.99-5zm-6 0C7.01 14 5 11.99 5 9.5S7.01 5 9.5 5 14 7.01 14 9.5 11.99 14 9.5 14zm2.5-4h-2v2H9v-2H7V9h2V7h1v2h2v1z',
  image: 'M21 19V5c0-1.1-.9-2-2-2H5c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h14c1.1 0 2-.9 2-2zM8.5 13.5l2.5 3.01L14.5 12l4.5 6H5l3.5-4.5z',
  undo: 'M12.5 8c-2.65 0-5.05.99-6.9 2.6L2 7v9h9l-3.62-3.62A7.95 7.95 0 0 1 12.5 10c3.54 0 6.55 2.31 7.6 5.5l2.37-.78C21.08 10.53 17.15 8 12.5 8z',
  repeat: 'M7 7h10v3l4-4-4-4v3H5v6h2V7zm10 10H7v-3l-4 4 4 4v-3h12v-6h-2v4z',
  order: 'M2 17h2v.5H3v1h1v.5H2v1h3v-4H2v1zm1-9h1V4H2v1h1v3zm-1 3h1.8L2 13.1v.9h3v-1H3.2L5 10.9V10H2v1zm5-6v2h14V5H7zm0 14h14v-2H7v2zm0-6h14v-2H7v2z',
  repeatOne: 'M7 7h10v3l4-4-4-4v3H5v6h2V7zm10 10H7v-3l-4 4 4 4v-3h12v-6h-2v4zm-4-2V9h-1l-2 1v1h1.5v4H13z',
  copy: 'M16 1H4c-1.1 0-2 .9-2 2v14h2V3h12V1zm3 4H8c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h11c1.1 0 2-.9 2-2V7c0-1.1-.9-2-2-2zm0 16H8V7h11v14z',
  volume: 'M3 9v6h4l5 5V4L7 9H3zm13.5 3A4.5 4.5 0 0 0 14 7.97v8.05c1.48-.73 2.5-2.25 2.5-4.02zM14 3.23v2.06c2.89.86 5 3.54 5 6.71s-2.11 5.85-5 6.71v2.06c4.01-.91 7-4.49 7-8.77s-2.99-7.86-7-8.77z',
  volumeOff: 'M16.5 12A4.5 4.5 0 0 0 14 7.97v2.21l2.45 2.45c.03-.2.05-.41.05-.63zm2.5 0c0 .94-.2 1.82-.54 2.64l1.51 1.51A8.796 8.796 0 0 0 21 12c0-4.28-2.99-7.86-7-8.77v2.06c2.89.86 5 3.54 5 6.71zM4.27 3L3 4.27 7.73 9H3v6h4l5 5v-6.73l4.25 4.25c-.67.52-1.42.93-2.25 1.18v2.06a8.99 8.99 0 0 0 3.69-1.81L19.73 21 21 19.73l-9-9L4.27 3zM12 4L9.91 6.09 12 8.18V4z',
};

export function Icon({ name, size = 24, label }) {
  return html`<svg class="icon" width=${size} height=${size} viewBox="0 0 24 24" aria-hidden=${label ? undefined : 'true'}
    role=${label ? 'img' : undefined} aria-label=${label}><path d=${icons[name]} /></svg>`;
}

export function IconButton({ icon, label, onClick, disabled, filled, size = 24, className = '', pressed }) {
  return html`<button class=${'icon-btn' + (filled ? ' filled' : '') + (className ? ' ' + className : '')} type="button" title=${label}
    aria-label=${label} aria-pressed=${pressed} onClick=${onClick} disabled=${disabled}><${Icon} name=${icon} size=${size} /></button>`;
}

// Cover shows a cover, or a placeholder when there is none or it fails to load. A failure belongs to
// that image: a reused Cover given another one tries it (review #12).
export function Cover({ id, size = 300, alt = '', className = '' }) {
  const [failedSrc, setFailedSrc] = useState(null);
  const src = coverURL(id, size);
  if (!src || failedSrc === src) {
    return html`<div class=${'cover placeholder ' + className} role="img" aria-label=${alt}><${Icon} name="album" size=${40} /></div>`;
  }
  return html`<img key=${src} class=${'cover ' + className} src=${src} alt=${alt} loading="lazy" onError=${() => setFailedSrc(src)} />`;
}

export function fmtTime(ms) {
  if (!ms || !isFinite(ms) || ms < 0) return '0:00';
  const s = Math.floor(ms / 1000);
  const hh = Math.floor(s / 3600), mm = Math.floor((s % 3600) / 60), ss = String(s % 60).padStart(2, '0');
  return hh ? `${hh}:${String(mm).padStart(2, '0')}:${ss}` : `${mm}:${ss}`;
}

export function fmtBytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), u.length - 1);
  return `${(n / 1024 ** i).toFixed(i ? 1 : 0)} ${u[i]}`;
}

// "FLAC 96/24", "MP3 320k": what the listener cares about.
export function fmtQuality(a) {
  if (!a) return '';
  const f = (a.format === 'm4a' ? a.codec : a.format).toUpperCase();
  if (a.bit_depth) return `${f} ${a.sample_rate / 1000}/${a.bit_depth}`;
  return a.bitrate ? `${f} ${Math.round(a.bitrate / 1000)}k` : f;
}

export function Spinner() {
  return html`<div class="spinner" role="status" aria-label="載入中"></div>`;
}

export function Empty({ icon = 'note', children }) {
  return html`<div class="empty"><${Icon} name=${icon} size=${48} /><p>${children}</p></div>`;
}

export function ErrorBox({ error, onRetry }) {
  if (!error) return null;
  return html`<div class="error-box" role="alert"><span>${error.message || String(error)}</span>
    ${onRetry && html`<button class="btn text" onClick=${onRetry}>重試</button>`}</div>`;
}

// useLoad runs an async loader and keeps loading / error / data together. Data loaded for other
// deps (another tab, another album) is never returned: rendering one tab's rows with another
// tab's component crashed the library page. A change of refresh (the library revision after an
// edit) loads again but keeps showing the current data meanwhile.
export function useLoad(loader, deps, refresh) {
  const key = JSON.stringify(deps);
  const [state, setState] = useState({ key: null, loading: true, error: null, data: null });
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let alive = true;
    setState((s) => ({ ...s, loading: true, error: null }));
    loader().then(
      (data) => alive && setState({ key, loading: false, error: null, data }),
      (error) => alive && setState({ key, loading: false, error, data: null }),
    );
    return () => { alive = false; };
  }, [key, tick, refresh]);
  const fresh = state.key === key;
  return {
    loading: !fresh || state.loading,
    error: fresh ? state.error : null,
    data: fresh ? state.data : null,
    reload: () => setTick((t) => t + 1),
  };
}

// Boundary keeps a failing page from taking the whole app (navigation, player) down with it.
export function Boundary({ children }) {
  const [error, reset] = useErrorBoundary((e) => console.error(e));
  if (error) {
    return html`<div class="error-box" role="alert"><span>這個頁面發生錯誤：${String(error.message || error)}</span>
      <button class="btn text" onClick=${reset}>重新載入</button></div>`;
  }
  return children;
}

// ---- toasts ----

export const toasts = createStore({ list: [] });
let toastSeq = 0;
const dropToast = (id) => toasts.set((s) => ({ list: s.list.filter((t) => t.id !== id) }));

// toast shows a message; action { label, onClick } adds a button (for example "undo") and keeps
// the toast up longer.
export function toast(message, kind = 'info', action = null) {
  const id = ++toastSeq;
  toasts.set((s) => ({ list: [...s.list, { id, message, kind, action }] }));
  setTimeout(() => dropToast(id), action ? 8000 : kind === 'error' ? 6000 : 3500);
}

export function Toasts() {
  const { list } = useStore(toasts);
  return html`<div class="toasts" aria-live="polite">
    ${list.map((t) => html`<div key=${t.id} class=${'toast ' + t.kind}><span>${t.message}</span>
      ${t.action && html`<button class="toast-action" onClick=${() => { dropToast(t.id); t.action.onClick(); }}>${t.action.label}</button>`}</div>`)}
  </div>`;
}

// ---- menus ----
// One popover menu at a time, anchored to the button that opened it. items: { icon, label, onClick }
// (falsy entries are skipped, so callers can write `cond && {...}`).

const menus = createStore({ menu: null });

export function openMenu(e, items) {
  e.stopPropagation();
  const opener = e.currentTarget;
  const r = opener.getBoundingClientRect();
  menus.set({ menu: { rect: { top: r.top, bottom: r.bottom, right: r.right }, items: items.filter(Boolean), opener } });
}

// closeMenu with refocus puts the keyboard focus back on the button that opened the menu.
const closeMenu = (refocus = false) => {
  const { menu } = menus.get();
  menus.set({ menu: null });
  if (refocus === true && menu?.opener?.isConnected) menu.opener.focus({ preventScroll: true });
};

// placeMenu puts a menu of this height below the button, or above it when only that side has
// room. When neither does, it takes the roomier side and scrolls inside, so every item stays
// reachable however short the window (review #47).
function placeMenu(rect, height, vh = innerHeight) {
  const margin = 8, gap = 4;
  const below = vh - rect.bottom - gap - margin, above = rect.top - gap - margin;
  if (height <= below || (height > above && below >= above)) return { top: rect.bottom + gap, maxHeight: below };
  return { bottom: vh - rect.top + gap, maxHeight: above };
}

export function MenuHost() {
  const { menu } = useStore(menus);
  const ref = useRef(null);
  useEffect(() => {
    if (!menu) return;
    const items = () => [...ref.current.querySelectorAll('[role=menuitem]')];
    items()[0]?.focus({ preventScroll: true });
    const onKey = (e) => {
      if (e.key === 'Escape') return closeMenu(true);
      const list = items(), at = list.indexOf(document.activeElement);
      const to = { ArrowDown: at + 1, ArrowUp: at - 1, Home: 0, End: list.length - 1 }[e.key];
      if (to === undefined || !list.length) return;
      e.preventDefault();
      list[(to + list.length) % list.length].focus(); // scrolls a long menu to it
    };
    addEventListener('keydown', onKey);
    const away = () => closeMenu();
    addEventListener('resize', away);
    addEventListener('hashchange', away);
    return () => {
      removeEventListener('keydown', onKey);
      removeEventListener('resize', away);
      removeEventListener('hashchange', away);
    };
  }, [menu]);
  if (!menu) return null;
  const { rect, items } = menu;
  const width = 240, height = items.length * 48 + 16;
  const left = Math.max(8, Math.min(rect.right - width, innerWidth - width - 8));
  const place = placeMenu(rect, height);
  const style = { left: left + 'px', width: width + 'px', maxHeight: place.maxHeight + 'px',
    ...(place.top !== undefined ? { top: place.top + 'px' } : { bottom: place.bottom + 'px' }) };
  // Scrolling the wheel outside the menu closes it; inside, it scrolls the menu.
  const onWheel = (e) => !ref.current?.contains(e.target) && closeMenu();
  return html`<div class="menu-scrim" onClick=${() => closeMenu(true)} onWheel=${onWheel}>
    <ul class="menu" role="menu" ref=${ref} style=${style} onClick=${(e) => e.stopPropagation()}>
      ${items.map((it, i) => html`<li key=${i} role="none"><button role="menuitem" onClick=${() => { closeMenu(true); it.onClick(); }}>
        <${Icon} name=${it.icon} /><span>${it.label}</span></button></li>`)}
    </ul>
  </div>`;
}

// ---- dialog ----
// showDialog(render) puts one dialog on screen; render receives close().

const dialogs = createStore({ render: null });

export function showDialog(render) {
  dialogs.set({ render });
}

export function DialogHost() {
  const { render } = useStore(dialogs);
  return render ? render(() => dialogs.set({ render: null })) : null;
}

export function Dialog({ title, onClose, children, actions, wide }) {
  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose();
    addEventListener('keydown', onKey);
    return () => removeEventListener('keydown', onKey);
  }, [onClose]);
  return html`<div class="scrim" onClick=${(e) => e.target === e.currentTarget && onClose()}>
    <div class=${'dialog' + (wide ? ' wide' : '')} role="dialog" aria-modal="true" aria-label=${title}>
      <h2>${title}</h2>
      <div class="dialog-body">${children}</div>
      ${actions && html`<div class="dialog-actions">${actions}</div>`}
    </div>
  </div>`;
}

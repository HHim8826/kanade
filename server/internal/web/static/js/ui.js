import { h } from '../vendor/preact.module.js';
import { useEffect, useErrorBoundary, useState } from '../vendor/hooks.module.js';
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
  logout: 'M17 7l-1.41 1.41L18.17 11H8v2h10.17l-2.58 2.58L17 17l5-5zM4 5h8V3H4c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h8v-2H4V5z',
  back: 'M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z',
  expand: 'M16.59 8.59L12 13.17 7.41 8.59 6 10l6 6 6-6z',
  refresh: 'M17.65 6.35A7.96 7.96 0 0 0 12 4a8 8 0 1 0 7.73 10h-2.08A6 6 0 1 1 12 6c1.66 0 3.14.69 4.22 1.78L13 11h7V4l-2.35 2.35z',
};

export function Icon({ name, size = 24, label }) {
  return html`<svg class="icon" width=${size} height=${size} viewBox="0 0 24 24" aria-hidden=${label ? undefined : 'true'}
    role=${label ? 'img' : undefined} aria-label=${label}><path d=${icons[name]} /></svg>`;
}

export function IconButton({ icon, label, onClick, disabled, filled, size = 24 }) {
  return html`<button class=${'icon-btn' + (filled ? ' filled' : '')} type="button" title=${label} aria-label=${label}
    onClick=${onClick} disabled=${disabled}><${Icon} name=${icon} size=${size} /></button>`;
}

export function Cover({ id, size = 300, alt = '', className = '' }) {
  const [failed, setFailed] = useState(false);
  const src = coverURL(id, size);
  if (!src || failed) {
    return html`<div class=${'cover placeholder ' + className} role="img" aria-label=${alt}><${Icon} name="album" size=${40} /></div>`;
  }
  return html`<img class=${'cover ' + className} src=${src} alt=${alt} loading="lazy" onError=${() => setFailed(true)} />`;
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
// tab's component crashed the library page.
export function useLoad(loader, deps) {
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
  }, [key, tick]);
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
export function toast(message, kind = 'info') {
  const id = ++toastSeq;
  toasts.set((s) => ({ list: [...s.list, { id, message, kind }] }));
  setTimeout(() => toasts.set((s) => ({ list: s.list.filter((t) => t.id !== id) })), kind === 'error' ? 6000 : 3500);
}

export function Toasts() {
  const { list } = useStore(toasts);
  return html`<div class="toasts" aria-live="polite">
    ${list.map((t) => html`<div key=${t.id} class=${'toast ' + t.kind}>${t.message}</div>`)}
  </div>`;
}

// ---- dialog ----

export function Dialog({ title, onClose, children, actions }) {
  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose();
    addEventListener('keydown', onKey);
    return () => removeEventListener('keydown', onKey);
  }, [onClose]);
  return html`<div class="scrim" onClick=${(e) => e.target === e.currentTarget && onClose()}>
    <div class="dialog" role="dialog" aria-modal="true" aria-label=${title}>
      <h2>${title}</h2>
      <div class="dialog-body">${children}</div>
      ${actions && html`<div class="dialog-actions">${actions}</div>`}
    </div>
  </div>`;
}

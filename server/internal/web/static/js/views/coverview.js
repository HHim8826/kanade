import { useEffect, useLayoutEffect, useRef, useState } from '../../vendor/hooks.module.js';
import { coverURL } from '../api.js';
import { Icon, IconButton, Spinner, html, showDialog } from '../ui.js';

// ---- the cover, whole (review #134) ----
// An album's cover over everything else, uncropped: fitted to the window first, zoomed and moved by
// the wheel, a pinch, dragging, a double click or tap, the buttons or the keys (+ - 0 1); the 600 px
// picture at once, the original once it has loaded. Esc, the close button or going back closes it;
// the page and the playback under it stay as they were.

const BG = 'kanade.coverBg';
const PAD = 16; // room around the fitted picture

export function viewCover(id, title) {
  if (id) showDialog((close) => html`<${CoverViewer} id=${id} title=${title} close=${close} />`);
}

const clamp = (v, lo, hi) => Math.min(Math.max(v, lo), hi);

function CoverViewer({ id, title, close }) {
  const [light, setLight] = useState(() => {
    try {
      return localStorage.getItem(BG) === 'light';
    } catch {
      return false;
    }
  });
  const [src, setSrc] = useState(coverURL(id, 600));
  const [nat, setNat] = useState(null); // the picture shown: { w, h, orig }
  const [orig, setOrig] = useState('loading'); // the original: loading, ok or failed
  const [failed, setFailed] = useState(false); // not even the small picture
  const [tries, setTries] = useState(0);
  const [stage, setStage] = useState({ w: 0, h: 0 });
  const [view, setView] = useState({ s: 1, x: 0, y: 0 }); // zoom over the fitted size, and the offset from the centre
  const box = useRef(null), img = useRef(null), closeBtn = useRef(null);

  // The original, decoded before it takes the small one's place, so nothing flashes.
  useEffect(() => {
    let alive = true;
    setOrig('loading');
    const im = new Image();
    im.src = coverURL(id, 0);
    im.decode().then(() => {
      if (!alive) return;
      setSrc(im.src);
      setNat({ w: im.naturalWidth, h: im.naturalHeight, orig: true });
      setOrig('ok');
      setFailed(false);
    }, () => alive && setOrig('failed'));
    return () => { alive = false; };
  }, [id, tries]);

  useLayoutEffect(() => {
    const el = box.current;
    const measure = () => setStage({ w: el.clientWidth, h: el.clientHeight });
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // The fitted size keeps the picture's shape (the small one has the original's); 1:1 is one pixel
  // of the original to one of the screen.
  const fit = nat && stage.w > 0 ? Math.min((stage.w - 2 * PAD) / nat.w, (stage.h - 2 * PAD) / nat.h) : 0;
  const bw = nat ? nat.w * fit : 0, bh = nat ? nat.h * fit : 0;
  const one = nat && nat.orig && fit > 0 ? 1 / fit : 0; // the zoom that shows it at its own size
  const most = Math.max(4, one * 2);
  const settle = (v) => { // no zooming out past the fit; no moving the picture off the screen
    const s = clamp(v.s, 1, most);
    const mx = Math.max(0, (bw * s - stage.w) / 2), my = Math.max(0, (bh * s - stage.h) / 2);
    return { s, x: clamp(v.x, -mx, mx), y: clamp(v.y, -my, my) };
  };
  const view2 = settle(view);
  const zoomAt = (s, px = 0, py = 0) => setView((v) => {
    const next = clamp(s, 1, most), k = next / v.s;
    return settle({ s: next, x: px - (px - v.x) * k, y: py - (py - v.y) * k });
  });
  const fitted = view2.s <= 1.001;
  const toggle = (px = 0, py = 0) => (fitted ? zoomAt(one > 1.2 ? one : 2.5, px, py) : setView({ s: 1, x: 0, y: 0 }));

  useLayoutEffect(() => {
    const el = img.current;
    if (!el || !nat) return;
    el.style.width = `${bw}px`;
    el.style.height = `${bh}px`;
    el.style.transform = `translate(-50%, -50%) translate(${view2.x}px, ${view2.y}px) scale(${view2.s})`;
  });

  // Going back (a phone's back gesture) closes it, without leaving the page.
  useEffect(() => {
    const opener = document.activeElement;
    const wasLocked = document.body.classList.contains('locked');
    document.body.classList.add('locked');
    history.pushState({ kanadeCover: true }, '');
    let gone = false;
    const onPop = () => { gone = true; close(); };
    addEventListener('popstate', onPop);
    closeBtn.current && closeBtn.current.focus();
    return () => {
      removeEventListener('popstate', onPop);
      if (!gone && history.state && history.state.kanadeCover) history.back();
      if (!wasLocked && !document.querySelector('.now-playing')) document.body.classList.remove('locked');
      if (opener && opener.focus && document.contains(opener)) opener.focus({ preventScroll: true });
    };
  }, []);

  useEffect(() => {
    const onKey = (e) => {
      if (e.key === 'Escape') close();
      else if (e.key === '+' || e.key === '=') zoomAt(view2.s * 1.25);
      else if (e.key === '-') zoomAt(view2.s / 1.25);
      else if (e.key === '0') setView({ s: 1, x: 0, y: 0 });
      else if (e.key === '1' && one) zoomAt(one);
      else return;
      e.preventDefault();
    };
    addEventListener('keydown', onKey);
    return () => removeEventListener('keydown', onKey);
  });

  // Pointers: one drags a zoomed picture, two pinch; a double tap toggles like a double click.
  const pointers = useRef(new Map());
  const pinch = useRef(null);
  const lastTap = useRef({ t: 0, x: 0, y: 0 });
  const touched = useRef(false); // the last pointer was a finger: its double tap is ours, not the browser's dblclick
  const [dragging, setDragging] = useState(false);
  const local = (e) => {
    const r = box.current.getBoundingClientRect();
    return { x: e.clientX - r.left - r.width / 2, y: e.clientY - r.top - r.height / 2 };
  };
  const pinchOf = () => {
    const [a, b] = [...pointers.current.values()];
    return { dist: Math.hypot(a.x - b.x, a.y - b.y), mid: { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 } };
  };
  const down = (e) => {
    box.current.setPointerCapture(e.pointerId);
    pointers.current.set(e.pointerId, local(e));
    touched.current = e.pointerType === 'touch';
    pinch.current = pointers.current.size === 2 ? pinchOf() : null; // a pinch measures from where both fingers came down
    setDragging(true);
  };
  const move = (e) => {
    const ps = pointers.current;
    if (!ps.has(e.pointerId)) return;
    const prev = ps.get(e.pointerId), p = local(e);
    ps.set(e.pointerId, p);
    if (ps.size === 1) {
      setView((v) => settle({ ...v, x: v.x + p.x - prev.x, y: v.y + p.y - prev.y }));
      return;
    }
    if (ps.size !== 2) return;
    const { dist, mid } = pinchOf();
    const last = pinch.current;
    pinch.current = { dist, mid };
    if (!last || !last.dist) return;
    setView((v) => {
      const s = clamp(v.s * (dist / last.dist), 1, most), k = s / v.s;
      return settle({ s, x: mid.x - (last.mid.x - v.x) * k, y: mid.y - (last.mid.y - v.y) * k });
    });
  };
  const up = (e) => {
    const ps = pointers.current;
    if (!ps.has(e.pointerId)) return;
    const p = ps.get(e.pointerId);
    ps.delete(e.pointerId);
    pinch.current = null;
    if (!ps.size) setDragging(false);
    if (e.pointerType !== 'mouse' && e.type === 'pointerup' && !ps.size) {
      const t = performance.now(), l = lastTap.current;
      if (t - l.t < 300 && Math.hypot(p.x - l.x, p.y - l.y) < 30) {
        toggle(p.x, p.y);
        lastTap.current = { t: 0, x: 0, y: 0 };
      } else lastTap.current = { t, x: p.x, y: p.y };
    }
  };
  const wheel = (e) => {
    e.preventDefault();
    const p = local(e);
    zoomAt(view2.s * Math.exp(-e.deltaY * (e.deltaMode === 1 ? 0.05 : 0.0015)), p.x, p.y);
  };

  const setBg = () => {
    setLight(!light);
    try {
      localStorage.setItem(BG, light ? 'dark' : 'light');
    } catch { /* this time only */ }
  };
  const retry = () => { setFailed(false); setSrc(coverURL(id, 600)); setTries((n) => n + 1); };
  return html`<div class=${'cover-view' + (light ? ' light' : '')} role="dialog" aria-modal="true" aria-label=${`封面：${title || ''}`}>
    <div class="cover-bar">
      <div class="cover-title">
        <div class="title">${title}</div>
        <div class="dims">${orig === 'ok' && nat ? `${nat.w} × ${nat.h}` : orig === 'loading' && !failed ? '載入原圖…' : ''}</div>
      </div>
      ${orig === 'failed' && !failed && html`<button class="btn text" onClick=${retry}>原圖載入失敗，重試</button>`}
      <${IconButton} icon="zoomOut" label="縮小" onClick=${() => zoomAt(view2.s / 1.25)} disabled=${fitted} />
      <${IconButton} icon="zoomIn" label="放大" onClick=${() => zoomAt(view2.s * 1.25)} disabled=${!nat || view2.s >= most - 0.001} />
      ${fitted
        ? html`<button class="icon-btn one-to-one" onClick=${() => zoomAt(one)} disabled=${!one} aria-label="原始大小" title="原始大小">1:1</button>`
        : html`<${IconButton} icon="fit" label="適合視窗" onClick=${() => setView({ s: 1, x: 0, y: 0 })} />`}
      <${IconButton} icon="contrast" label=${light ? '深色背景' : '淺色背景'} onClick=${setBg} />
      <button class="icon-btn" ref=${closeBtn} onClick=${close} aria-label="關閉" title="關閉"><${Icon} name="close" /></button>
    </div>
    <div class=${'cover-stage' + (fitted ? '' : ' zoomed') + (dragging ? ' dragging' : '')} ref=${box}
      onPointerDown=${down} onPointerMove=${move} onPointerUp=${up} onPointerCancel=${up} onWheel=${wheel}
      onDblClick=${(e) => { if (touched.current) return; const p = local(e); toggle(p.x, p.y); }}>
      ${failed
        ? html`<div class="cover-msg"><span>無法載入封面。</span><button class="btn tonal" onClick=${retry}>重試</button></div>`
        : html`<img class="cover-full" ref=${img} src=${src} alt=${title || '封面'} draggable="false"
            onLoad=${(e) => !nat && setNat({ w: e.target.naturalWidth, h: e.target.naturalHeight, orig: false })}
            onError=${() => orig !== 'ok' && setFailed(true)} />`}
      ${!nat && !failed && html`<div class="cover-msg"><${Spinner} /></div>`}
    </div>
  </div>`;
}

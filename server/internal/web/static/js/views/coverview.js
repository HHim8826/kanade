import { useEffect, useLayoutEffect, useRef, useState } from '../../vendor/hooks.module.js';
import { coverURL } from '../api.js';
import { player } from '../player.js';
import { Icon, IconButton, Spinner, html, showDialog, useLayer } from '../ui.js';

// ---- a picture, whole: an album's cover (review #134), or its gallery's ----
// Over everything else, uncropped: fitted to the window first, zoomed and moved by the wheel, a
// pinch, dragging, a double click or tap, the buttons or the keys (+ - 0 1); a small picture at
// once, the large one once it has loaded. A gallery pages by the arrows, the strip of its pictures,
// a swipe or the keys (← → Home End), the large pictures beside the one shown loaded ahead (not in
// lite mode). Esc, the close button or going back closes it; the page and the playback under it
// stay as they were.

const BG = 'kanade.coverBg';
const PAD = 16; // room around the fitted picture

export function viewCover(id, title) {
  if (id) {
    showDialog((close) => html`<${PictureViewer} items=${[{ key: 'c' + id, small: coverURL(id, 600), full: coverURL(id, 0) }]}
      title=${title} close=${close} />`);
  }
}

const clamp = (v, lo, hi) => Math.min(Math.max(v, lo), hi);

// PictureViewer shows items ({ key, small, full, info, alt }: info says what one is, in place of
// its size) from the one at start. more(e, item, index) opens what can be done to one; notice is
// shown under the bar.
export function PictureViewer({ items, start = 0, title, label = '封面', more, notice, close }) {
  const [light, setLight] = useState(() => {
    try {
      return localStorage.getItem(BG) === 'light';
    } catch {
      return false;
    }
  });
  const [index, setIndex] = useState(start);
  const at = clamp(index, 0, items.length - 1);
  const item = items[at];
  // What is known of each picture, by its key: the large one's size once decoded (or 'failed'),
  // and the small one's, shown till then.
  const [full, setFull] = useState({});
  const [small, setSmall] = useState({});
  const [tries, setTries] = useState(0);
  const [stage, setStage] = useState({ w: 0, h: 0 });
  const [view, setView] = useState({ s: 1, x: 0, y: 0 }); // zoom over the fitted size, and the offset from the centre
  const box = useRef(null), img = useRef(null), closeBtn = useRef(null), strip = useRef(null);
  const got = full[item.key], ok = !!got && got !== 'failed';
  const orig = ok ? 'ok' : got || 'loading'; // the large picture: loading, ok or failed
  const src = ok ? item.full : item.small;
  const nat = ok ? { ...got, orig: true } : small[item.key] && small[item.key] !== 'failed' ? { ...small[item.key], orig: false } : null; // the picture shown
  const failed = !ok && small[item.key] === 'failed'; // not even the small picture
  const known = useRef(full);
  known.current = full;

  // The large picture, decoded before it takes the small one's place, so nothing flashes; one left
  // for another before it came is no longer fetched.
  useEffect(() => {
    const had = known.current[item.key];
    if (had && had !== 'failed') return undefined;
    let alive = true, done = false;
    const im = new Image();
    im.src = item.full;
    im.decode().then(() => alive && setFull((f) => ({ ...f, [item.key]: { w: im.naturalWidth, h: im.naturalHeight } })),
      () => alive && setFull((f) => ({ ...f, [item.key]: 'failed' }))).finally(() => { done = true; });
    return () => {
      alive = false;
      if (!done) im.src = '';
    };
  }, [item.key, item.full, tries]);

  // The pictures either side, loaded ahead: the browser keeps them for when they are shown.
  const ahead = useRef(new Map());
  useEffect(() => {
    if (items.length < 2 || player.get().lite) return;
    const near = [items[at + 1], items[at - 1]].filter(Boolean);
    const keep = new Set([item.key, ...near.map((it) => it.key)]);
    for (const [k, im] of ahead.current) {
      if (keep.has(k)) continue;
      if (!im.complete) im.src = '';
      ahead.current.delete(k);
    }
    for (const it of near) {
      if (ahead.current.has(it.key) || known.current[it.key]) continue;
      const im = new Image();
      im.src = it.full;
      ahead.current.set(it.key, im);
    }
  }, [at, items]);
  useEffect(() => () => ahead.current.forEach((im) => { if (!im.complete) im.src = ''; }), []);
  useEffect(() => {
    const el = strip.current, on = el?.querySelector('.on');
    if (on) el.scrollTo({ left: on.offsetLeft - (el.clientWidth - on.offsetWidth) / 2, behavior: 'smooth' });
  }, [at]);
  // page goes to a picture; step(by) from the one shown, so that keys pressed before the page
  // shows the last go on from there.
  const page = (to) => {
    if (to < 0 || to >= items.length) return;
    setIndex(to);
    setView({ s: 1, x: 0, y: 0 });
  };
  const step = (by) => {
    setIndex((i) => clamp(clamp(i, 0, items.length - 1) + by, 0, items.length - 1));
    setView({ s: 1, x: 0, y: 0 });
  };

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
  // A picture smaller than the screen is shown larger to fit it, and can go down to its own size
  // too (review #147); a larger one goes no smaller than the fit.
  const least = one > 0 ? Math.min(1, one) : 1;
  const settle = (v) => { // no zooming out past the fit (or the own size); no moving the picture off the screen
    const s = clamp(v.s, least, most);
    const mx = Math.max(0, (bw * s - stage.w) / 2), my = Math.max(0, (bh * s - stage.h) / 2);
    return { s, x: clamp(v.x, -mx, mx), y: clamp(v.y, -my, my) };
  };
  const view2 = settle(view);
  const zoomAt = (s, px = 0, py = 0) => setView((v) => {
    const next = clamp(s, least, most), k = next / v.s;
    return settle({ s: next, x: px - (px - v.x) * k, y: py - (py - v.y) * k });
  });
  const fitted = Math.abs(view2.s - 1) < 0.001, zoomed = view2.s > 1.001;
  // A double click goes from the fit to the picture's own size (or 2.5 times, when that is about the
  // fit), and back.
  const toggle = (px = 0, py = 0) => (fitted ? zoomAt(one > 1.2 || (one > 0 && one < 0.9) ? one : 2.5, px, py) : setView({ s: 1, x: 0, y: 0 }));

  useLayoutEffect(() => {
    const el = img.current;
    if (!el) return;
    el.style.visibility = nat ? '' : 'hidden'; // a picture of another size waits for its own
    if (!nat) return;
    el.style.width = `${bw}px`;
    el.style.height = `${bh}px`;
    el.style.transform = `translate(-50%, -50%) translate(${view2.x}px, ${view2.y}px) scale(${view2.s})`;
  });

  // A layer like the dialogs (review #159): going back (a phone's back gesture) closes it without
  // leaving the page, and focus goes back to what opened it.
  const root = useRef(null);
  useLayer(root, close, closeBtn);
  useEffect(() => {
    const wasLocked = document.body.classList.contains('locked');
    document.body.classList.add('locked');
    return () => {
      if (!wasLocked && !document.querySelector('.now-playing')) document.body.classList.remove('locked');
    };
  }, []);

  useEffect(() => {
    const onKey = (e) => {
      if (e.key === 'Escape') close();
      else if (e.key === '+' || e.key === '=') zoomAt(view2.s * 1.25);
      else if (e.key === '-') zoomAt(view2.s / 1.25);
      else if (e.key === '0') setView({ s: 1, x: 0, y: 0 });
      else if (e.key === '1' && one) zoomAt(one);
      else if ((e.key === 'ArrowLeft' || e.key === 'ArrowRight') && items.length > 1) step(e.key === 'ArrowLeft' ? -1 : 1);
      else if ((e.key === 'Home' || e.key === 'End') && items.length > 1) page(e.key === 'Home' ? 0 : items.length - 1);
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
  const swipe = useRef(null); // where one pointer came down on a picture not zoomed: moved far sideways, it pages
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
    swipe.current = pointers.current.size === 1 && !zoomed && items.length > 1 ? { x: e.clientX, y: e.clientY } : null;
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
    swipe.current = null;
    const { dist, mid } = pinchOf();
    const last = pinch.current;
    pinch.current = { dist, mid };
    if (!last || !last.dist) return;
    setView((v) => {
      const s = clamp(v.s * (dist / last.dist), least, most), k = s / v.s;
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
    const sw = swipe.current;
    swipe.current = null;
    if (sw && e.type === 'pointerup' && !ps.size) {
      const dx = e.clientX - sw.x, dy = e.clientY - sw.y;
      if (Math.abs(dx) > 48 && Math.abs(dx) > 1.5 * Math.abs(dy)) {
        step(dx < 0 ? 1 : -1);
        lastTap.current = { t: 0, x: 0, y: 0 };
        return;
      }
    }
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
  const retry = () => {
    setSmall((m) => ({ ...m, [item.key]: undefined }));
    setFull((f) => ({ ...f, [item.key]: undefined }));
    setTries((n) => n + 1);
  };
  const info = item.info ?? (ok ? `${nat.w} × ${nat.h}` : '');
  const loading = orig === 'loading' && !failed ? '載入原圖…' : '';
  const many = items.length > 1;
  const nav = (e) => e.stopPropagation(); // a press on the arrows is not a drag of the picture
  return html`<div class=${'cover-view' + (light ? ' light' : '')} role="dialog" aria-modal="true" aria-label=${`${label}：${title || ''}`} ref=${root}>
    <div class="cover-bar">
      <div class="cover-title">
        <div class="title">${title}</div>
        <div class="dims">${[many && `${at + 1} / ${items.length}`, info, loading].filter(Boolean).join(' · ')}</div>
      </div>
      ${orig === 'failed' && !failed && html`<button class="btn text" onClick=${retry}>原圖載入失敗，重試</button>`}
      <${IconButton} icon="zoomOut" label="縮小" className="zoom-btn" onClick=${() => zoomAt(view2.s / 1.25)} disabled=${view2.s <= least + 0.001} />
      <${IconButton} icon="zoomIn" label="放大" className="zoom-btn" onClick=${() => zoomAt(view2.s * 1.25)} disabled=${!nat || view2.s >= most - 0.001} />
      ${fitted
        ? html`<button class="icon-btn one-to-one" onClick=${() => zoomAt(one)} disabled=${!one} aria-label="原始大小" title="原始大小">1:1</button>`
        : html`<${IconButton} icon="fit" label="適合視窗" onClick=${() => setView({ s: 1, x: 0, y: 0 })} />`}
      <${IconButton} icon="contrast" label=${light ? '深色背景' : '淺色背景'} onClick=${setBg} />
      ${more && html`<${IconButton} icon="more" label="更多" onClick=${(e) => more(e, item, at)} />`}
      <button class="icon-btn" ref=${closeBtn} onClick=${close} aria-label="關閉" title="關閉"><${Icon} name="close" /></button>
    </div>
    ${notice}
    <div class=${'cover-stage' + (zoomed ? ' zoomed' : '') + (dragging ? ' dragging' : '')} ref=${box}
      onPointerDown=${down} onPointerMove=${move} onPointerUp=${up} onPointerCancel=${up} onWheel=${wheel}
      onDblClick=${(e) => { if (touched.current) return; const p = local(e); toggle(p.x, p.y); }}>
      ${failed
        ? html`<div class="cover-msg"><span>無法載入${label}。</span><button class="btn tonal" onClick=${retry}>重試</button></div>`
        : html`<img key=${item.key + '/' + tries} class="cover-full" ref=${img} src=${src} alt=${item.alt || title || label} draggable="false"
            onLoad=${(e) => !ok && setSmall((m) => ({ ...m, [item.key]: { w: e.target.naturalWidth, h: e.target.naturalHeight } }))}
            onError=${() => !ok && setSmall((m) => ({ ...m, [item.key]: 'failed' }))} />`}
      ${!nat && !failed && html`<div class="cover-msg"><${Spinner} /></div>`}
      ${many && html`<button class="pic-nav prev" onPointerDown=${nav} onDblClick=${nav} onClick=${() => step(-1)} disabled=${at === 0}
          aria-label="上一張" title="上一張（←）"><${Icon} name="left" /></button>
        <button class="pic-nav next" onPointerDown=${nav} onDblClick=${nav} onClick=${() => step(1)} disabled=${at === items.length - 1}
          aria-label="下一張" title="下一張（→）"><${Icon} name="right" /></button>`}
    </div>
    ${many && html`<div class="pic-strip" ref=${strip}>${items.map((it, i) => html`<button key=${it.key} class=${i === at ? 'on' : ''}
      aria-current=${i === at ? 'true' : null} aria-label=${`第 ${i + 1} 張${it.alt ? '：' + it.alt : ''}`} title=${it.alt} onClick=${() => page(i)}>
      <img src=${it.small} alt="" loading="lazy" draggable="false" /></button>`)}</div>`}
  </div>`;
}

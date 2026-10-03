// Scroll bars stay out of sight (style.css) until they are wanted: while an area scrolls (wheel,
// keys, a dragged thumb, a finger), and while the mouse is near an area's bar, so it can be grabbed.
const timers = new WeakMap();

addEventListener('scroll', (e) => {
  const el = e.target === document ? document.documentElement : e.target;
  if (!(el instanceof Element)) return;
  el.classList.add('scrolling');
  clearTimeout(timers.get(el));
  timers.set(el, setTimeout(() => el.classList.remove('scrolling'), 900));
}, { capture: true, passive: true });

const reach = 20; // px from the edge where the bar is
const scrolls = (overflow) => overflow === 'auto' || overflow === 'scroll';

// barNear is the scrolling area under the pointer whose bar the pointer is next to.
function barNear(target, x, y) {
  for (let el = target instanceof Element ? target : null; el && el !== document.body; el = el.parentElement) {
    const tall = el.scrollHeight > el.clientHeight, wide = el.scrollWidth > el.clientWidth;
    if (!tall && !wide) continue;
    const st = getComputedStyle(el), r = el.getBoundingClientRect();
    if (tall && scrolls(st.overflowY) && x >= r.right - reach) return el;
    if (wide && scrolls(st.overflowX) && y >= r.bottom - reach) return el;
  }
  return null;
}

let near = null, frame = 0;
addEventListener('pointermove', (e) => {
  if (e.pointerType !== 'mouse' || frame) return;
  frame = requestAnimationFrame(() => {
    frame = 0;
    const hit = barNear(e.target, e.clientX, e.clientY);
    if (hit === near) return;
    near?.classList.remove('scroll-near');
    hit?.classList.add('scroll-near');
    near = hit;
  });
}, { passive: true });

document.documentElement.addEventListener('mouseleave', () => {
  near?.classList.remove('scroll-near');
  near = null;
});

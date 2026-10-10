import { useEffect, useRef, useState } from '../vendor/hooks.module.js';
import { IconButton, html } from './ui.js';

// Selecting several albums or songs of a list to act on them at once (review #83). What is
// selected is kept by the items' stable keys (album, song or entry IDs), in the order chosen, so
// loading more of the list keeps it; another scope (tab, filter, search, album) starts over. In
// selection mode a click or tap checks an item instead of opening or playing it; Ctrl or ⌘ click
// starts selecting from anywhere, and Shift click takes the range from the item clicked last.
// The items themselves are remembered too (SelectBar hands them over as the list shows them), so an
// action takes every selected item even after the list was loaded again with fewer pages, and the
// latest data of each (review #89).
export function useSelection(scope) {
  const [s, setS] = useState({ on: false, keys: new Set(), last: -1 });
  const seen = useRef(new Map()); // key -> item, as the list last showed it
  useEffect(() => {
    setS({ on: false, keys: new Set(), last: -1 });
    seen.current = new Map();
  }, [scope]);
  const update = (f) => setS((v) => ({ ...v, ...f(v) }));
  return {
    on: s.on,
    count: s.keys.size,
    keys: [...s.keys],
    has: (k) => s.keys.has(k),
    // remember keeps the items of the list (keys[i] is items[i]'s key).
    remember(keys, items) {
      keys.forEach((k, i) => seen.current.set(k, items[i]));
    },
    // chosen is every selected item, in the order selected.
    chosen: () => [...s.keys].map((k) => seen.current.get(k)).filter((it) => it !== undefined),
    start: () => update(() => ({ on: true })),
    stop: () => setS({ on: false, keys: new Set(), last: -1 }),
    clear: () => update(() => ({ keys: new Set(), last: -1 })),
    setMany: (keys, on = true) => update((v) => {
      const next = new Set(v.keys);
      for (const k of keys) on ? next.add(k) : next.delete(k);
      return { keys: next };
    }),
    // click handles a click on item i of the list (keys: the list's keys); true when it was a
    // selection, so the item does not open or play.
    click(e, keys, i) {
      const mod = e.ctrlKey || e.metaKey;
      if (!s.on && !mod && !e.shiftKey) return false;
      // A row's checkbox checks itself, as selected here; kept from it, the browser would undo it.
      if (e.currentTarget.type !== 'checkbox') e.preventDefault();
      e.stopPropagation();
      update((v) => {
        const next = new Set(v.keys);
        if (e.shiftKey && v.last >= 0) {
          const [a, b] = v.last < i ? [v.last, i] : [i, v.last];
          for (let j = a; j <= b; j++) next.add(keys[j]);
        } else if (next.has(keys[i])) {
          next.delete(keys[i]);
        } else {
          next.add(keys[i]);
        }
        return { on: true, keys: next, last: i };
      });
      return true;
    },
  };
}

// SelectToggle starts or ends selecting (the way in on a phone, where there are no modifier keys).
// It keeps its place while the list loads, fails or is empty (disabled then), and ending a
// selection is always possible (review #101).
export const SelectToggle = ({ sel, disabled }) => html`<button class=${'btn ' + (sel.on ? 'tonal' : 'text')} aria-pressed=${sel.on}
  disabled=${!sel.on && disabled} onClick=${sel.on ? sel.stop : sel.start}>${sel.on ? '完成' : '選取'}</button>`;

// SelectBar stays at the bottom of the page while selecting: how many are selected, selecting all
// that is loaded (more says the list has more not loaded yet), and the actions. items are the
// loaded items (loaded holds their keys), which the selection remembers for the actions.
export function SelectBar({ sel, noun, loaded, items, more, children }) {
  sel.remember(loaded, items);
  if (!sel.on) return null;
  const all = loaded.length > 0 && loaded.every((k) => sel.has(k));
  const shown = new Set(loaded);
  const away = sel.keys.filter((k) => !shown.has(k)).length; // selected before the list was loaded again
  return html`<div class="select-bar" role="toolbar" aria-label="批次操作">
    <div class="select-info">
      <${IconButton} icon="close" label="結束選取" onClick=${sel.stop} />
      <span class="grow"><b>已選 ${sel.count} ${noun}</b>${away ? html`<span class="sub" title=${`其中 ${away} ${noun}在清單重新載入後沒有顯示，操作仍包含它們`}>　含 ${away} ${noun}未顯示，仍會處理</span>`
        : more ? html`<span class="sub">　還有沒載入的；全選只含已載入的</span>` : ''}</span>
      <button class="btn text" onClick=${() => sel.setMany(loaded, !all)}>${all ? '全不選' : `全選已載入的 ${loaded.length} ${noun}`}</button>
    </div>
    ${sel.count > 0 ? html`<div class="select-actions">${children}</div>` : html`<div class="sub select-hint">點選要處理的項目；按住 Shift 可以一次選一段。</div>`}
  </div>`;
}

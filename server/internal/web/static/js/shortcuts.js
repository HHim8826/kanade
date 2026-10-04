import { current, next, player, prev, seek, setVolume, toggle, toggleMute } from './player.js';
import { Dialog, html, showDialog } from './ui.js';

// ---- keyboard shortcuts ----
// Anywhere in the app, for a keyboard: not while typing, while a dialog, a menu or a cover is open
// (they have keys of their own), nor for the keys the focused control uses itself — Space and Enter
// press a button, arrows move a slider.

// Each: the key combinations (keys pressed together), and what they do.
export const SHORTCUTS = [
  [[['空白鍵'], ['K']], '播放／暫停'],
  [[['←'], ['→']], '倒退／快轉 5 秒'],
  [[['Shift', '←'], ['Shift', '→']], '上一首／下一首'],
  [[['↑'], ['↓']], '音量增減 5%'],
  [[['M']], '靜音／取消靜音'],
  [[['Esc']], '收起正在播放'],
  [[['?']], '顯示這些快捷鍵'],
];

const combo = (keys) => keys.map((k, i) => html`${i > 0 && ' + '}<kbd>${k}</kbd>`);

export function showShortcuts() {
  showDialog((close) => html`<${Dialog} title="鍵盤快捷鍵" onClose=${close}
      actions=${html`<button class="btn filled" onClick=${close}>知道了</button>`}>
    <dl class="shortcuts">${SHORTCUTS.map(([combos, what]) => html`<div key=${what}>
      <dt>${combos.map((c, i) => html`${i > 0 && html`<span class="sub">或</span>`}<span>${combo(c)}</span>`)}</dt><dd>${what}</dd></div>`)}</dl>
    <p class="hint">輸入文字時，或有對話框、選單、封面開著時不會作用。</p>
  <//>`);
}

const TEXT = /^(text|search|email|password|url|tel|number|date|time|datetime-local|month|week)$/;

// What the focused element does with a key itself.
function ownKey(el, key) {
  if (!el || el === document.body) return false;
  if (el.isContentEditable || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT') return true;
  if (el.tagName === 'INPUT') {
    if (TEXT.test(el.type) || !el.type) return true;
    if (el.type === 'range') return /^(Arrow|Home|End|Page)/.test(key);
    return key === ' ' || key === 'Enter'; // checkboxes, radios, buttons
  }
  if (el.closest('button, a[href], summary, [role=button], [role=tab], [role=menuitem]')) return key === ' ' || key === 'Enter';
  return false;
}

addEventListener('keydown', (e) => {
  if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return;
  if (document.querySelector('.scrim, .menu-scrim, .cover-view')) return;
  if (ownKey(document.activeElement, e.key)) return;
  const s = player.get(), playing = !!current();
  const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
  switch (true) {
    case key === '?':
      showShortcuts();
      break;
    case key === 'Escape' && s.nowPlayingOpen:
      player.set({ nowPlayingOpen: false });
      break;
    case !playing:
      return;
    case key === ' ' || key === 'k':
      toggle();
      break;
    case (key === 'ArrowLeft' || key === 'ArrowRight') && e.shiftKey:
      (key === 'ArrowLeft' ? prev : next)();
      break;
    case key === 'ArrowLeft' || key === 'ArrowRight': {
      const t = (s.scrub ?? s.time) + (key === 'ArrowLeft' ? -5 : 5);
      seek(Math.min(Math.max(t, 0), s.duration || t));
      break;
    }
    case key === 'ArrowUp' && s.muted: // back to the volume it had
      toggleMute();
      break;
    case key === 'ArrowDown' && s.muted: // already silent
      break;
    case key === 'ArrowUp' || key === 'ArrowDown':
      setVolume(Math.round((s.volume + (key === 'ArrowUp' ? 0.05 : -0.05)) * 100) / 100);
      break;
    case key === 'm':
      toggleMute();
      break;
    default:
      return;
  }
  e.preventDefault();
});

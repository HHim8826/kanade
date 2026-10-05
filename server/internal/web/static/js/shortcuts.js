import { current, next, now, player, prev, seek, setVolume, toggle, toggleMute } from './player.js';
import { Dialog, html, showDialog } from './ui.js';

// ---- keyboard shortcuts ----
// Anywhere in the app, for a keyboard: not while typing, while a dialog, a menu or a cover is open
// (they have keys of their own), nor for the keys the focused control uses itself — Space and Enter
// press a button, arrows move a slider. Space and the arrows scroll the page: they work the player
// only in the player bar or now playing (review #156); elsewhere the letters and Shift with an
// arrow do.

// Each: the key combinations (keys pressed together), and what they do; then those of the player.
export const SHORTCUTS = [
  [[['K']], '播放／暫停'],
  [[['J'], ['L']], '倒退／快轉 5 秒'],
  [[['Shift', '←'], ['Shift', '→']], '上一首／下一首'],
  [[['Shift', '↑'], ['Shift', '↓']], '音量增減 5%'],
  [[['M']], '靜音／取消靜音'],
  [[['Esc']], '收起正在播放'],
  [[['?']], '顯示這些快捷鍵'],
];
export const PLAYER_SHORTCUTS = [
  [[['空白鍵']], '播放／暫停'],
  [[['←'], ['→']], '倒退／快轉 5 秒'],
  [[['↑'], ['↓']], '音量增減 5%'],
];

const combo = (keys) => keys.map((k, i) => html`${i > 0 && ' + '}<kbd>${k}</kbd>`);

const list = (items) => html`<dl class="shortcuts">${items.map(([combos, what]) => html`<div key=${what}>
  <dt>${combos.map((c, i) => html`${i > 0 && html`<span class="sub">或</span>`}<span>${combo(c)}</span>`)}</dt><dd>${what}</dd></div>`)}</dl>`;

export function showShortcuts() {
  showDialog((close) => html`<${Dialog} title="鍵盤快捷鍵" onClose=${close}
      actions=${html`<button class="btn filled" onClick=${close}>知道了</button>`}>
    ${list(SHORTCUTS)}
    <h3 class="shortcuts-sub">在播放列或「正在播放」上</h3>
    ${list(PLAYER_SHORTCUTS)}
    <p class="hint">其他地方的空白鍵和方向鍵照常捲動頁面。輸入文字時，或有對話框、選單、封面開著時，快捷鍵都不會作用。</p>
  <//>`);
}

// inPlayer: the keys go to the player bar or now playing (focused there, or now playing is open
// over the page).
const inPlayer = (el, s) => (el && el !== document.body && !!el.closest('.player-bar, .now-playing')) ||
  (s.nowPlayingOpen && (!el || el === document.body));

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
  const s = player.get(), playing = !!current(), here = inPlayer(document.activeElement, s);
  const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
  const arrow = key === 'ArrowLeft' || key === 'ArrowRight', upDown = key === 'ArrowUp' || key === 'ArrowDown';
  const step = (by) => {
    const t = (s.scrub ?? now()) + by;
    seek(Math.min(Math.max(t, 0), s.duration || t));
  };
  const volume = (up) => {
    if (s.muted) {
      if (up) toggleMute(); // back to the volume it had; down: already silent
      return;
    }
    setVolume(Math.round((s.volume + (up ? 0.05 : -0.05)) * 100) / 100);
  };
  switch (true) {
    case key === '?':
      showShortcuts();
      break;
    case key === 'Escape' && s.nowPlayingOpen:
      player.set({ nowPlayingOpen: false });
      break;
    case !playing:
      return;
    case key === 'k' || (key === ' ' && here):
      toggle();
      break;
    case arrow && e.shiftKey:
      (key === 'ArrowLeft' ? prev : next)();
      break;
    case key === 'j' || key === 'l':
      step(key === 'j' ? -5 : 5);
      break;
    case arrow && here:
      step(key === 'ArrowLeft' ? -5 : 5);
      break;
    case upDown && (e.shiftKey || here):
      volume(key === 'ArrowUp');
      break;
    case key === 'm':
      toggleMute();
      break;
    default:
      return;
  }
  e.preventDefault();
});

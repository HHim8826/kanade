// The look (review #31): a color theme and light, dark or the system's choice, kept in the browser.
// theme-init.js applies it before the first paint; this module changes it at once.

export const themes = [['violet', '藍紫（預設）'], ['teal', '青綠'], ['rose', '玫瑰']];
export const modes = [['system', '跟隨系統'], ['light', '淺色'], ['dark', '深色']];
const KEY = 'kanade.theme';
const bars = { violet: ['#fbf8ff', '#121318'], teal: ['#f4fbfa', '#0e1515'], rose: ['#fff8f7', '#1a1111'] };

export function loadTheme() {
  try {
    const p = JSON.parse(localStorage.getItem(KEY) || '{}') || {};
    return { theme: bars[p.theme] ? p.theme : 'violet', mode: ['light', 'dark'].includes(p.mode) ? p.mode : 'system' };
  } catch {
    return { theme: 'violet', mode: 'system' };
  }
}

export function setTheme(t) {
  try {
    localStorage.setItem(KEY, JSON.stringify(t));
  } catch { /* not kept: it lasts for this page */ }
  const d = document.documentElement;
  if (t.theme === 'violet') d.removeAttribute('data-theme');
  else d.setAttribute('data-theme', t.theme);
  if (t.mode === 'system') d.removeAttribute('data-mode');
  else d.setAttribute('data-mode', t.mode);
  for (const m of document.querySelectorAll('meta[name="theme-color"]')) {
    const dark = /dark/.test(m.media);
    m.content = bars[t.theme][t.mode === 'dark' || (t.mode !== 'light' && dark) ? 1 : 0];
  }
}

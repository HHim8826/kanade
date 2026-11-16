import { createStore, useStore } from './store.js';
import { html } from './ui.js';

// A new release changes the files the page loads, but a page left open (an installed app brought
// back from the background) runs the ones it loaded until it is loaded again. So, when it is looked
// at again, it asks which release the server is on, and offers to load it.

// mine is the release this page loaded: its files are under /app/v/<version>/.
const mine = (new URL(import.meta.url).pathname.match(/\/app\/(v\/[0-9a-f]+)\//) || [])[1] || '';
const update = createStore({ ready: false, later: false });
let asked = 0;

async function check() {
  if (!mine || update.get().ready || Date.now() - asked < 60_000) return;
  asked = Date.now();
  try {
    const res = await fetch('/app/version', { cache: 'no-store' });
    const now = res.ok ? (await res.text()).trim() : '';
    if (/^v\/[0-9a-f]+$/.test(now) && now !== mine) update.set({ ready: true });
  } catch { /* offline: asked again next time */ }
}

export function watchUpdates() {
  document.addEventListener('visibilitychange', () => document.visibilityState === 'visible' && check());
  setInterval(() => document.visibilityState === 'visible' && check(), 30 * 60_000);
}

export function UpdateBar() {
  const { ready, later } = useStore(update);
  if (!ready || later) return null;
  return html`<div class="update-bar" role="status">
    <span class="grow">Kanade 已更新，重新載入就會換成新版。</span>
    <button class="btn text" onClick=${() => update.set({ later: true })}>稍後</button>
    <button class="btn filled" onClick=${() => location.reload()}>重新載入</button>
  </div>`;
}

import { api, get } from './api.js';
import { clock, player } from './player.js';

// What this tab plays, told to Kanade for the companions that show it, such as Discord's status on
// a computer (review #135). A tab tells only once it has played: a queue brought back paused, a
// song loaded ahead or an album looked at tells nothing. Nothing is told while no companion is
// paired. A song playing on is told again every half minute, so the server knows the tab is there.

// fromAgent names a device by its browser's user agent: "Windows · Chrome".
export function fromAgent(ua) {
  const os = /iPhone|iPad/.test(ua) ? 'iPhone' : /Android/.test(ua) ? 'Android' : /Mac OS X/.test(ua) ? 'Mac'
    : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : '';
  const browser = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : '';
  return [os, browser].filter(Boolean).join(' · ');
}

const random = () => Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => b.toString(16).padStart(2, '0')).join('');
const tab = random();

// device is this browser, the same in all its tabs: what a companion follows.
export const device = (() => {
  try {
    let d = localStorage.getItem('kanade.device');
    if (!/^[0-9a-f]{24}$/.test(d || '')) {
      d = random();
      localStorage.setItem('kanade.device', d);
    }
    return d;
  } catch {
    return tab; // storage blocked: this tab alone
  }
})();
export const deviceName = fromAgent(navigator.userAgent) || '瀏覽器';

let active = false; // logged in: asked once
let publish = false;
let played = false; // this tab has played the song it has
let seq = 0;
let told = null; // the last report: { key, state, pos, at }

function now() {
  const s = player.get();
  const it = s.queue[s.index];
  if (!it) return { state: 'stopped' };
  return {
    state: s.playing ? 'playing' : 'paused', title: it.title || '', artist: it.artist || '', album: it.album || '', album_id: it.albumId || 0,
    duration_ms: Math.round(s.duration ? s.duration * 1000 : it.durationMs || 0), position_ms: Math.round(clock.get().time * 1000),
  };
}

// tell reports this tab when what it plays changed: another song, playing or paused, or a jump
// (a seek, a song begun again); again tells it anyway.
function tell(again) {
  const st = now();
  if (st.state === 'playing') played = true;
  if (st.state === 'stopped') played = false;
  if (!publish || (!played && !told)) return;
  if (!played) st.state = 'stopped'; // a song since loaded and not played: nothing to show
  const key = [st.state, st.title, st.artist, st.album, st.duration_ms].join('\u001f');
  const at = Date.now();
  let moved = !told || key !== told.key;
  if (!moved && st.state === 'playing') moved = Math.abs(st.position_ms - (told.pos + (at - told.at))) > 2000;
  if (!moved && !again) return;
  if (st.state === 'stopped' && !told) return;
  told = st.state === 'stopped' ? null : { key, state: st.state, pos: st.position_ms, at };
  api('PUT', `/presence/players/${tab}`, { device, device_name: deviceName, seq: ++seq, ...st }, { allow401: true })
    .then((r) => { if (r && r.publish === false) publish = false; }, () => {});
}

player.subscribe(() => tell(false));
clock.subscribe(() => tell(false));
setInterval(() => told && tell(true), 30000);
addEventListener('pagehide', () => {
  if (told) api('DELETE', `/presence/players/${tab}`, undefined, { keepalive: true, allow401: true }).catch(() => {});
});

// checkPresence asks whether a companion is paired, so this tab tells or not; done when logged in,
// when the tab comes back into view, every few minutes, and when the settings page pairs one.
export async function checkPresence(info) {
  active = true;
  try {
    const r = info || await get('/presence', { allow401: true });
    const was = publish;
    publish = !!r.publish;
    if (publish && !was) {
      told = null;
      tell(true);
    }
  } catch { /* asked again later */ }
}
setInterval(() => active && document.visibilityState === 'visible' && checkPresence(), 5 * 60 * 1000);
document.addEventListener('visibilitychange', () => active && document.visibilityState === 'visible' && checkPresence());

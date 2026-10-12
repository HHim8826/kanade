import { api } from './api.js';
import { clock, player, queueId } from './player.js';

// What this tab plays, told to Kanade: for the account's other devices, which list it to take the
// playback over or control it (review #138), and for its owner's Discord status when linked (review
// #135). A tab tells what it plays only once it has played: a queue brought back paused, a song
// loaded ahead or an album looked at tells nothing. A song playing or paused is told again every half
// minute, so the server knows the tab is there. Until then, or once its queue is cleared, it is idle,
// and tells only its controls (the volume, muted, the mode), for a device controlling it (review
// #211): told as they change, and again each time its event stream connects (remote.js).

// fromAgent names a device by its browser's user agent: "Windows · Chrome".
export function fromAgent(ua) {
  const os = /iPhone|iPad/.test(ua) ? 'iPhone' : /Android/.test(ua) ? 'Android' : /Mac OS X/.test(ua) ? 'Mac'
    : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : '';
  const browser = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : '';
  return [os, browser].filter(Boolean).join(' · ');
}

const random = () => Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => b.toString(16).padStart(2, '0')).join('');
// tab is this page's player: what the other devices see and send commands to.
export const tab = random();

// device is this browser, the same in all its tabs: what the status can follow.
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

let active = false; // logged in
let played = false; // this tab has played the song it has
let seq = 0;
let told = null; // the last report: { key, state, pos, at }
let idleKey = ''; // the controls last told while idle

function now() {
  const s = player.get();
  const it = s.queue[s.index];
  if (!it) return { state: 'stopped' };
  return {
    state: s.playing ? 'playing' : 'paused', title: it.title || '', artist: it.artist || '', album: it.album || '', album_id: it.albumId || 0,
    duration_ms: Math.round(s.duration ? s.duration * 1000 : it.durationMs || 0), position_ms: Math.round(clock.get().time * 1000),
    // for a device controlling this one
    asset_id: it.assetId || 0, track_id: it.trackId || 0, cover_id: it.coverId || 0, qid: it.qid || 0, queue: queueId(), index: s.index,
    count: s.queue.length, volume: s.volume, muted: !!s.muted, mode: s.mode, blocked: !!s.blocked,
  };
}

// tell reports this tab when what it plays changed: another song, playing or paused, a jump (a
// seek, a song begun again), or what a device controlling it shows (the queue, the volume, the
// mode); again tells it anyway.
function tell(again) {
  const st = now();
  if (st.state === 'playing') played = true;
  if (st.state === 'stopped') played = false;
  if (!active) return;
  if (!played) return idle(again);
  idleKey = '';
  const key = [st.state, st.title, st.artist, st.album, st.duration_ms, st.asset_id, st.qid, st.queue, st.index, st.count, st.volume,
    st.muted, st.mode, st.blocked].join('\u001f');
  const at = Date.now();
  let moved = !told || key !== told.key;
  if (!moved && st.state === 'playing') moved = Math.abs(st.position_ms - (told.pos + (at - told.at))) > 2000;
  if (!moved && !again) return;
  told = { key, state: st.state, pos: st.position_ms, at };
  put(st);
}

// idle tells the controls of a tab with no song played (one it played is let go of: the server
// lists it as idle, with them).
function idle(again) {
  const s = player.get();
  const st = { state: 'idle', volume: s.volume, muted: !!s.muted, mode: s.mode, blocked: !!s.blocked };
  const key = [st.volume, st.muted, st.mode, st.blocked].join('\u001f');
  if (!told && key === idleKey && !again) return;
  told = null;
  idleKey = key;
  put(st);
}

const put = (st) => api('PUT', `/presence/players/${tab}`, { device, device_name: deviceName, seq: ++seq, ...st }, { allow401: true }).catch(() => {});

// A change is told a moment later, with those right after it: a song ending, the next one loaded
// and started, its clock set to its start, is one report of the new song where it is (review #183).
let soon = null;
const later = () => {
  if (!soon) soon = setTimeout(() => { soon = null; tell(false); }, 300);
};
player.subscribe(later);
clock.subscribe(later);
setInterval(() => told && tell(true), 30000);
addEventListener('pagehide', () => {
  if (told) api('DELETE', `/presence/players/${tab}`, undefined, { keepalive: true, allow401: true }).catch(() => {});
});

// checkPresence starts telling, once logged in (again: the settings page linked Discord).
export function checkPresence() {
  active = true;
  told = null;
  tell(true);
}

// connected tells again once this tab's event stream is open: an idle tab's controls are kept with it.
export function connected() {
  if (active) tell(true);
}

// stopPresence stops telling: logged out.
export function stopPresence() {
  active = false;
  told = null;
  played = false;
  idleKey = '';
}
// Back in view, after a while frozen in the background, perhaps forgotten: told again.
document.addEventListener('visibilitychange', () => active && told && document.visibilityState === 'visible' && tell(true));

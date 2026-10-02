import { coverURL, get, post, streamURL } from './api.js';
import { createStore } from './store.js';
import { toast } from './ui.js';

// Queue item: { assetId, title, artist, album, albumId, coverId, durationMs, kind, asset, resumeMs? }
export const player = createStore({
  queue: [],
  index: -1,
  playing: false,
  buffering: false,
  time: 0, // seconds
  duration: 0, // seconds
  nowPlayingOpen: false,
});

const audio = new Audio();
audio.preload = 'auto';

export const current = () => {
  const s = player.get();
  return s.queue[s.index];
};

// From an album entry or a track list item to a queue item.
export function fromEntry(e, album) {
  return {
    assetId: e.asset.id, title: e.title, artist: e.artist, album: album.title, albumId: album.id,
    coverId: album.cover_id, durationMs: e.asset.duration_ms, asset: e.asset, kind: e.kind,
  };
}

export function fromTrack(t) {
  return {
    assetId: t.asset.id, title: t.title, artist: t.artist, album: t.album, albumId: t.album_id,
    coverId: t.cover_id, durationMs: t.asset.duration_ms, asset: t.asset, kind: t.kind,
  };
}

// ---- play history (decision D9) ----
// Each playback gets its own session; "heard" adds only normal progress, so seeking does not count.
let session = null;

function report(finished = false, keepalive = false) {
  if (!session) return;
  const p = session;
  post('/plays', {
    session: p.id, asset_id: p.item.assetId, album_id: p.item.albumId || 0,
    position_ms: Math.round(audio.currentTime * 1000), listened_ms: Math.round(p.heard), finished,
  }, { keepalive }).catch(() => {});
}

let pendingSeek = null; // ms to jump to once the new track can seek

function load(index, autoplay = true) {
  const s = player.get();
  const item = s.queue[index];
  if (!item) return;
  if (session) report(); // close out the track we are leaving
  session = { id: crypto.randomUUID(), item, heard: 0, last: null };
  pendingSeek = item.resumeMs || null;
  if (!pendingSeek && item.kind === 'spoken') { // drama and radio pick up where they stopped
    get(`/assets/${item.assetId}/resume`).then((r) => {
      if (session && session.item === item && r.position_ms && audio.currentTime < 5) seekWhenReady(r.position_ms);
    }, () => {});
  }
  player.set({ index, time: 0, duration: (item.durationMs || 0) / 1000, buffering: true });
  audio.src = streamURL(item.assetId);
  if (autoplay) audio.play().catch(() => player.set({ playing: false, buffering: false }));
  updateMediaSession(item);
}

function seekWhenReady(ms) {
  if (audio.readyState >= 1) audio.currentTime = ms / 1000;
  else pendingSeek = ms;
}

audio.addEventListener('loadedmetadata', () => {
  if (pendingSeek) {
    audio.currentTime = pendingSeek / 1000;
    pendingSeek = null;
  }
});
audio.addEventListener('timeupdate', () => {
  if (!session || audio.paused) return;
  const t = audio.currentTime;
  if (session.last !== null && t > session.last && t - session.last < 2) session.heard += (t - session.last) * 1000;
  session.last = t;
});
audio.addEventListener('seeking', () => session && (session.last = null));
setInterval(() => !audio.paused && report(), 15000);
document.addEventListener('visibilitychange', () => document.visibilityState === 'hidden' && report(false, true));
addEventListener('pagehide', () => report(false, true));

export function playQueue(items, index = 0) {
  if (!items.length) return;
  player.set({ queue: items, index });
  load(index);
}

export function shuffled(items) {
  const a = [...items];
  for (let i = a.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [a[i], a[j]] = [a[j], a[i]];
  }
  return a;
}

export function enqueue(item) {
  const s = player.get();
  if (s.index < 0) return playQueue([item]);
  player.set({ queue: [...s.queue, item] });
  toast(`已加入佇列：${item.title}`);
}

export function toggle() {
  if (!current()) return;
  if (audio.paused) audio.play().catch(() => {});
  else audio.pause();
}

export function next() {
  const s = player.get();
  if (s.index + 1 < s.queue.length) load(s.index + 1);
  else {
    audio.pause();
    player.set({ playing: false });
  }
}

export function prev() {
  const s = player.get();
  if (audio.currentTime > 3 || s.index === 0) audio.currentTime = 0;
  else load(s.index - 1);
}

export const playAt = (i) => load(i);
export const seek = (sec) => {
  if (isFinite(sec)) audio.currentTime = sec;
};

// Warm the server's stream cache for the next track (plan §5: preload at most the next one).
function prefetchNext() {
  const s = player.get();
  const n = s.queue[s.index + 1];
  if (n) fetch(streamURL(n.assetId), { headers: { Range: 'bytes=0-0' }, credentials: 'same-origin' }).catch(() => {});
}

let lastTick = 0;
audio.addEventListener('timeupdate', () => {
  const now = performance.now();
  if (now - lastTick < 250) return; // 4 updates a second is plenty
  lastTick = now;
  player.set({ time: audio.currentTime });
});
audio.addEventListener('durationchange', () => isFinite(audio.duration) && player.set({ duration: audio.duration }));
audio.addEventListener('play', () => player.set({ playing: true }));
audio.addEventListener('pause', () => {
  player.set({ playing: false });
  if (!audio.ended) report();
});
audio.addEventListener('waiting', () => player.set({ buffering: true }));
audio.addEventListener('playing', () => {
  player.set({ buffering: false, playing: true });
  prefetchNext();
});
audio.addEventListener('ended', () => {
  report(true);
  session = null;
  next();
});
audio.addEventListener('error', () => {
  const item = current();
  player.set({ buffering: false, playing: false });
  if (item) toast(`無法播放「${item.title}」`, 'error');
});

// Lock screen, notification and headset controls.
function updateMediaSession(item) {
  if (!('mediaSession' in navigator)) return;
  const art = item.coverId ? [96, 256, 512].map((n) => ({ src: coverURL(item.coverId, n), sizes: `${n}x${n}`, type: 'image/jpeg' })) : [];
  navigator.mediaSession.metadata = new MediaMetadata({ title: item.title, artist: item.artist, album: item.album || '', artwork: art });
}

if ('mediaSession' in navigator) {
  const ms = navigator.mediaSession;
  ms.setActionHandler('play', toggle);
  ms.setActionHandler('pause', toggle);
  ms.setActionHandler('previoustrack', prev);
  ms.setActionHandler('nexttrack', next);
  try {
    ms.setActionHandler('seekto', (d) => seek(d.seekTime));
  } catch { /* not supported */ }
}

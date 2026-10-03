import { coverURL, get, post, streamURL } from './api.js';
import { createStore } from './store.js';
import { toast } from './ui.js';

// Queue item: { qid, assetId, trackId, title, artist, album, albumId, coverId, durationMs, kind, asset, resumeMs? };
// qid tells apart the same song queued twice.
const PREFS = 'kanade.player';

// One play mode at a time, switched by one button (review #40): in order, looping the queue,
// looping this song, or shuffled (which goes on in a new order after the last song).
export const MODES = ['order', 'all', 'one', 'shuffle'];
const modeState = (mode) => ({ mode, shuffle: mode === 'shuffle', repeat: { order: 'off', all: 'all', one: 'one', shuffle: 'all' }[mode] });

// modeFrom reads the saved mode, or the separate shuffle and repeat switches saved before.
export function modeFrom(p) {
  if (MODES.includes(p.mode)) return p.mode;
  if (p.repeat === 'one') return 'one';
  if (p.shuffle) return 'shuffle';
  return p.repeat === 'all' ? 'all' : 'order';
}

function loadPrefs() {
  let p = {};
  try {
    p = JSON.parse(localStorage.getItem(PREFS) || '{}') || {};
  } catch { /* storage blocked: defaults */ }
  return {
    volume: typeof p.volume === 'number' ? Math.min(Math.max(p.volume, 0), 1) : 1, muted: !!p.muted,
    ...modeState(modeFrom(p)),
  };
}

export const player = createStore({
  queue: [],
  index: -1,
  playing: false,
  buffering: false,
  time: 0, // seconds
  duration: 0, // seconds
  nowPlayingOpen: false,
  original: null, // the queue in its own order while shuffle is on
  scrub: null, // seconds the seek bar is being dragged to, not yet sought (review #41)
  ...loadPrefs(), // volume 0–1, muted, mode, and from it shuffle and repeat: off | all | one
});

function savePrefs() {
  const { volume, muted, mode } = player.get();
  try {
    localStorage.setItem(PREFS, JSON.stringify({ volume, muted, mode }));
  } catch { /* private mode: the choice lasts for this page */ }
}

const audio = new Audio();
audio.preload = 'auto';
const applyVolume = () => {
  const s = player.get();
  audio.volume = s.volume;
  audio.muted = s.muted;
};
applyVolume();

let qseq = 0;
const tag = (items) => items.map((it) => ({ ...it, qid: ++qseq }));

export const current = () => {
  const s = player.get();
  return s.queue[s.index];
};

// From an album entry or a track list item to a queue item.
export function fromEntry(e, album) {
  return {
    assetId: e.asset.id, trackId: e.track_id, title: e.title, artist: e.artist, album: album.title, albumId: album.id,
    coverId: album.cover_id, durationMs: e.asset.duration_ms, asset: e.asset, kind: e.kind,
  };
}

export function fromTrack(t) {
  return {
    assetId: t.asset.id, trackId: t.id, title: t.title, artist: t.artist, album: t.album, albumId: t.album_id,
    coverId: t.cover_id, durationMs: t.asset.duration_ms, asset: t.asset, kind: t.kind,
  };
}

// ---- play history (decision D9) ----
// Each playback gets its own session; "heard" adds only normal progress, so seeking does not count.
let session = null;

function report(finished = false, keepalive = false) {
  if (!session) return;
  const p = session;
  const position = Math.round(audio.currentTime * 1000), heard = Math.round(p.heard);
  // Nothing new (say, a paused tab going to the background): stay quiet, or this old playback
  // would become the "latest" one on the home page over what was played since on another device.
  if (!finished && p.sent && p.sent.position === position && p.sent.heard === heard) return;
  p.sent = { position, heard };
  // seq orders this playback's reports and at dates them, so one that arrives late changes nothing
  // newer (review #8).
  post('/plays', {
    session: p.id, asset_id: p.item.assetId, album_id: p.item.albumId || 0,
    position_ms: position, listened_ms: heard, finished, seq: (p.seq = (p.seq || 0) + 1), at: Date.now(),
  }, { keepalive }).catch(() => {});
}

let pendingSeek = null; // ms to jump to once the new track can seek

// load plays the queue item at index. again is a loop coming round (repeat one, or the queue
// starting over): that plays from the start, never from where it was resumed (review #42).
function load(index, autoplay = true, again = false) {
  const s = player.get();
  const item = s.queue[index];
  if (!item) return;
  if (session) report(); // close out the track we are leaving
  session = { id: crypto.randomUUID(), item, heard: 0, last: null };
  // A resume point ("continue" on the home page) is for the play it was asked for only.
  pendingSeek = again ? null : item.resumeMs || null;
  delete item.resumeMs;
  if (!again && !pendingSeek && item.kind === 'spoken') { // drama and radio pick up where they stopped
    get(`/assets/${item.assetId}/resume`).then((r) => {
      if (session && session.item === item && r.position_ms && audio.currentTime < 5) seekWhenReady(r.position_ms);
    }, () => {});
  }
  player.set({ index, time: 0, scrub: null, duration: (item.durationMs || 0) / 1000, buffering: true });
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
audio.addEventListener('playing', () => report()); // the home page shows the latest playback from its start
setInterval(() => !audio.paused && report(), 15000);
document.addEventListener('visibilitychange', () => document.visibilityState === 'hidden' && report(false, true));
addEventListener('pagehide', () => report(false, true));

// playQueue plays a list from index. With shuffle on, the chosen song plays first and the rest
// follow in random order; turning shuffle off goes back to the list's own order.
export function playQueue(items, index = 0) {
  if (!items.length) return;
  const list = tag(items);
  if (player.get().shuffle) {
    const first = list[index];
    player.set({ original: list, queue: [first, ...shuffled(list.filter((q) => q !== first))], index: 0 });
    load(0);
    return;
  }
  player.set({ queue: list, index, original: null });
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

// enqueue adds one item or a list at the end of the queue.
export function enqueue(items) {
  const raw = Array.isArray(items) ? items : [items];
  const s = player.get();
  if (!raw.length) return;
  if (s.index < 0) return playQueue(raw);
  const list = tag(raw);
  player.set({ queue: [...s.queue, ...list], original: s.original && [...s.original, ...list] });
  toast(list.length > 1 ? `已將 ${list.length} 首加入佇列` : `已加入佇列：${list[0].title}`);
}

// playNext puts items right after the current track.
export function playNext(items) {
  const raw = Array.isArray(items) ? items : [items];
  const s = player.get();
  if (!raw.length) return;
  if (s.index < 0) return playQueue(raw);
  const list = tag(raw);
  const queue = [...s.queue];
  queue.splice(s.index + 1, 0, ...list);
  let original = s.original;
  if (original) { // also right after the current song in the unshuffled order
    original = [...original];
    original.splice(original.findIndex((q) => q.qid === queue[s.index].qid) + 1, 0, ...list);
  }
  player.set({ queue, original });
  toast(list.length > 1 ? `接下來播放 ${list.length} 首` : `下一首播放：${list[0].title}`);
}

// ---- modes and the queue (review #16) ----

export function setVolume(v) {
  player.set({ volume: Math.min(Math.max(v, 0), 1), muted: v <= 0 });
  applyVolume();
  savePrefs();
}

// toggleMute silences and brings back the volume it had.
export function toggleMute() {
  const s = player.get();
  player.set({ muted: !s.muted, volume: s.muted && s.volume === 0 ? 0.5 : s.volume });
  applyVolume();
  savePrefs();
}

// cycleMode goes in order → loop the queue → loop this song → shuffle → in order. The current song
// keeps playing: shuffling puts the rest of the queue in random order after it, and leaving
// shuffle puts the queue back in its own order around it.
export function cycleMode() {
  const s = player.get();
  const mode = MODES[(MODES.indexOf(s.mode) + 1) % MODES.length];
  const cur = s.queue[s.index];
  let queue = {};
  if (mode === 'shuffle' && cur) {
    queue = { original: s.queue, queue: [cur, ...shuffled(s.queue.filter((q) => q !== cur))], index: 0 };
  } else if (s.shuffle) {
    const list = s.original || s.queue;
    queue = { original: null, queue: list, index: cur ? Math.max(list.findIndex((q) => q.qid === cur.qid), 0) : -1 };
  }
  player.set({ ...modeState(mode), ...queue });
  savePrefs();
}

const without = (list, gone) => list && list.filter((q) => !gone.has(q.qid));

// removeAt takes a song out of the queue. Removing the one playing moves on to the next (paused if
// it was paused); removing the last one left stops and clears the player.
export function removeAt(i) {
  const s = player.get();
  const item = s.queue[i];
  if (!item) return;
  if (s.queue.length === 1) return resetPlayer();
  const gone = new Set([item.qid]);
  const queue = without(s.queue, gone), original = without(s.original, gone);
  if (i !== s.index) {
    player.set({ queue, original, index: i < s.index ? s.index - 1 : s.index });
    return;
  }
  const wasPlaying = !audio.paused;
  let index = i < queue.length ? i : 0;
  if (i >= queue.length && s.repeat === 'off') { // it was the last: stop at the start of the queue
    player.set({ queue, original });
    load(queue.length - 1, false);
    return;
  }
  player.set({ queue, original });
  load(index, wasPlaying);
}

// clearUpcoming keeps the songs up to the current one.
export function clearUpcoming() {
  const s = player.get();
  if (s.index < 0) return;
  const gone = new Set(s.queue.slice(s.index + 1).map((q) => q.qid));
  player.set({ queue: s.queue.slice(0, s.index + 1), original: without(s.original, gone) });
}

// moveItem changes the play order; the current song keeps playing wherever it ends up.
export function moveItem(from, to) {
  const s = player.get();
  if (from === to || !s.queue[from] || to < 0 || to >= s.queue.length) return;
  const cur = s.queue[s.index];
  const queue = [...s.queue];
  queue.splice(to, 0, ...queue.splice(from, 1));
  player.set({ queue, index: queue.findIndex((q) => q.qid === cur.qid) });
}

// playAfterCurrent moves a queued song to right after the one playing.
export function playAfterCurrent(i) {
  const s = player.get();
  moveItem(i, i < s.index ? s.index : s.index + 1);
}

// resetPlayer stops playback and forgets the queue (logging out, clearing the queue). With report,
// the playback so far is reported first.
export function resetPlayer(withReport = true) {
  if (withReport) report(false, true);
  session = null;
  pendingSeek = null;
  audio.pause();
  audio.removeAttribute('src');
  audio.load();
  player.set({ queue: [], index: -1, original: null, playing: false, buffering: false, time: 0, scrub: null, duration: 0, nowPlayingOpen: false });
  if ('mediaSession' in navigator) navigator.mediaSession.metadata = null;
}

export function toggle() {
  if (!current()) return;
  if (audio.paused) audio.play().catch(() => {});
  else audio.pause();
}

// next moves on. Past the end it starts over in the looping modes (shuffle in a new order), and
// stops in order mode.
export function next() {
  const s = player.get();
  if (s.index + 1 < s.queue.length) load(s.index + 1);
  else if (s.repeat !== 'off' && s.queue.length) {
    if (s.shuffle && s.queue.length > 1) {
      const queue = shuffled(s.queue);
      if (queue[0] === s.queue[s.index]) queue.push(queue.shift()); // not the song that just played
      player.set({ queue });
    }
    load(0, true, true);
  } else {
    audio.pause();
    player.set({ playing: false });
  }
}

export function prev() {
  const s = player.get();
  if (audio.currentTime > 3 || (s.index === 0 && s.repeat === 'off')) seek(0);
  else load(s.index > 0 ? s.index - 1 : s.queue.length - 1);
}

export const playAt = (i) => load(i);

// seek jumps there and shows it at once. Dragging the seek bar only previews (scrubTo) and seeks
// once on release, so a drag is one jump, not one per pointer move (review #41).
export const seek = (sec) => {
  if (!isFinite(sec) || !current()) return;
  audio.currentTime = sec;
  player.set({ time: sec, scrub: null });
};
export const scrubTo = (sec) => isFinite(sec) && player.set({ scrub: sec });
export const endScrub = (commit) => {
  const { scrub } = player.get();
  if (scrub === null) return;
  if (commit) seek(scrub);
  else player.set({ scrub: null });
};

// Warm the server's stream cache for the next track (plan §5: preload at most the next one).
function prefetchNext() {
  const s = player.get();
  const n = s.repeat === 'one' ? null : s.queue[s.index + 1] || (s.repeat === 'all' ? s.queue[0] : null);
  if (n) fetch(streamURL(n.assetId), { headers: { Range: 'bytes=0-0' }, credentials: 'same-origin' }).catch(() => {});
}

let lastTick = 0;
audio.addEventListener('timeupdate', () => {
  const now = performance.now();
  if (now - lastTick < 250) return; // 4 updates a second is plenty
  lastTick = now;
  player.set({ time: audio.currentTime });
});
audio.addEventListener('seeked', () => player.set({ time: audio.currentTime }));
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
  if (player.get().repeat === 'one') load(player.get().index, true, true);
  else next();
});
audio.addEventListener('error', () => {
  if (!audio.getAttribute('src')) return; // emptied on purpose
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

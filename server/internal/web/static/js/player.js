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

// Playback preferences (review #78), kept in this browser with the volume and the mode:
//   scope: what shuffle draws from, the queue or the whole library (#72);
//   autoContinue: when the queue's songs run out, go on with songs from the library (#73);
//   resume: per kind, whether choosing a song starts it over or where it was left;
//   preload: warm the next song's stream ahead.
function loadPrefs() {
  let p = {};
  try {
    p = JSON.parse(localStorage.getItem(PREFS) || '{}') || {};
  } catch { /* storage blocked: defaults */ }
  const r = p.resume || {};
  return {
    volume: typeof p.volume === 'number' ? Math.min(Math.max(p.volume, 0), 1) : 1, muted: !!p.muted,
    ...modeState(modeFrom(p)),
    scope: p.scope === 'library' ? 'library' : 'queue',
    autoContinue: !!p.autoContinue,
    preload: p.preload !== false,
    resume: { music: r.music === 'resume' ? 'resume' : 'start', spoken: r.spoken === 'start' ? 'start' : 'resume' },
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
  from: 0, // the song (qid) a shuffle of the whole library began at: the list goes on after it
  scrub: null, // seconds the seek bar is being dragged to, not yet sought (review #41)
  // The queue goes on by itself with songs from the whole library ({ kind }), a shuffle of the
  // library or "keep playing" once the queue's own songs ran out; songs it adds are marked auto.
  radio: null,
  radioError: null, // why the last pick from the library failed
  sleep: null, // the sleep timer (review #98): { until } (a time, ms) or { endOfTrack: true }; this device only
  ...loadPrefs(), // volume 0–1, muted, mode (and from it shuffle and repeat: off | all | one), and the preferences above
});

function savePrefs() {
  const { volume, muted, mode, scope, autoContinue, preload, resume } = player.get();
  try {
    localStorage.setItem(PREFS, JSON.stringify({ volume, muted, mode, scope, autoContinue, preload, resume }));
  } catch { /* private mode: the choice lasts for this page */ }
}

// setPrefs changes playback preferences (the settings page, the queue's switch). Turning "keep
// playing" off ends the library songs not reached yet; turning the library shuffle scope on or off
// takes effect the next time shuffle starts.
export function setPrefs(patch) {
  player.set(patch);
  savePrefs();
  const s = player.get();
  if (patch.autoContinue === false && s.radio && !(s.shuffle && s.scope === 'library') && !s.radio.chosen) {
    endRadio();
  }
  if (patch.autoContinue) topUp();
}

const audio = new Audio();
audio.preload = 'auto';
const SLEEP_FADE_MS = 20_000;
// applyVolume sets the volume chosen, faded out over the last seconds of a sleep timer; the fade
// never touches the setting itself.
const applyVolume = () => {
  const s = player.get();
  const fade = s.sleep && s.sleep.until ? Math.min(1, Math.max(0, (s.sleep.until - Date.now()) / SLEEP_FADE_MS)) : 1;
  audio.volume = s.volume * fade;
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
  // A song loaded but never played (brought back paused after a reload, say) is no playback: the
  // home page keeps what was last heard, here or on another device (review #127).
  if (!session || !session.played) return;
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
let loads = 0; // counts songs loaded: a wait begun for one song controls nothing once another began (review #125)

// load plays the queue item at index. again is a loop coming round (repeat one, or the queue
// starting over): that plays from the start, never from where it was resumed (review #42).
function load(index, autoplay = true, again = false) {
  const s = player.get();
  const item = s.queue[index];
  if (!item) return;
  if (session) report(); // close out the track we are leaving
  loads++;
  session = { id: crypto.randomUUID(), item, heard: 0, last: null, played: false };
  // A resume point ("continue" on the home page, a bookmark) is for the play it was asked for only;
  // 0 is a place too, the start, which the settings' resuming does not override (review #106).
  pendingSeek = again ? null : item.resumeMs ?? null;
  delete item.resumeMs;
  // Otherwise as the settings say for its kind: drama and radio pick up where they stopped, music
  // starts over (review #78). A loop coming round always starts over.
  if (!again && pendingSeek === null && s.resume[item.kind === 'spoken' ? 'spoken' : 'music'] === 'resume') {
    get(`/assets/${item.assetId}/resume`).then((r) => {
      if (session && session.item === item && r.position_ms && audio.currentTime < 5) seekWhenReady(r.position_ms);
    }, () => {});
  }
  player.set({ index, time: (pendingSeek || 0) / 1000, scrub: null, duration: (item.durationMs || 0) / 1000, buffering: autoplay });
  audio.src = streamURL(item.assetId);
  if (autoplay) audio.play().catch(() => player.set({ playing: false, buffering: false }));
  updateMediaSession(item);
  savePlace();
}

function seekWhenReady(ms) {
  if (audio.readyState >= 1) audio.currentTime = ms / 1000;
  else pendingSeek = ms;
}

audio.addEventListener('loadedmetadata', () => {
  if (pendingSeek !== null) {
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
audio.addEventListener('playing', () => { // the home page shows the latest playback from its start
  if (session) session.played = true;
  report();
});
setInterval(() => !audio.paused && report(), 15000);
document.addEventListener('visibilitychange', () => document.visibilityState === 'hidden' && report(false, true));
addEventListener('pagehide', () => report(false, true));

// playQueue plays a list from index. With shuffle on, the chosen song plays first and the rest
// follow in random order; turning shuffle off goes back to the list's own order. Shuffling the
// whole library, the chosen song plays first and songs from the library follow (review #72).
export function playQueue(items, index = 0) {
  if (!items.length) return;
  gen++;
  const list = tag(items);
  const s = player.get();
  if (s.shuffle && s.scope === 'library') {
    const first = list[index];
    player.set({ original: list, from: first.qid, queue: [first], index: 0, radio: { kind: kindOf(first) }, radioError: null });
    load(0);
    topUp();
    return;
  }
  if (s.shuffle) {
    const first = list[index];
    player.set({ original: list, queue: [first, ...shuffled(list.filter((q) => q !== first))], index: 0, radio: null, radioError: null });
    load(0);
    return;
  }
  player.set({ queue: list, index, original: null, radio: null, radioError: null });
  load(index);
}

// ---- songs from the whole library (reviews #72, #73) ----

let gen = 0; // a new queue (or a reset) makes answers for the old one late: they are dropped
// The request for more library songs on its way, shared by everyone who needs songs meanwhile: the
// end of the queue reached before it answers waits for it (review #90). It resolves to 'ok',
// 'failed' (radioError says why) or 'stale' (asked for a queue since replaced).
let topping = null;
let pauses = 0; // pauses the listener asked for: a wait for songs that saw one does not start playing
const kindOf = (item) => (item && item.kind === 'spoken' ? 'spoken' : 'music');

// playLibraryShuffle plays songs picked at random from the whole library, one after another, each
// song as likely as any other; the queue goes on with more as it plays. kind: music (drama and
// radio left out) or spoken.
export async function playLibraryShuffle(kind = 'music') {
  const my = ++gen;
  const s = player.get();
  try {
    const list = await get(`/tracks/random?n=3&kind=${kind}&not=${recentIds(s).join(',')}`);
    if (my !== gen) return;
    if (!list.length) {
      toast(kind === 'spoken' ? '曲庫沒有可以播放的廣播劇或談話' : '曲庫沒有可以播放的歌', 'error');
      return;
    }
    player.set({ queue: tag(list.map(fromTrack).map(autoItem)), index: 0, original: null, radio: { kind, chosen: true }, radioError: null });
    load(0);
  } catch (e) {
    if (my === gen) toast(e.message, 'error');
  }
}

// playSmart plays a smart playlist on and on (review #96): every pick asks its rules again,
// leaving out the songs just played.
export async function playSmart(id, name) {
  const my = ++gen;
  const s = player.get();
  try {
    const list = await get(`/playlists/${id}/next?n=3&not=${recentIds(s).join(',')}`);
    if (my !== gen) return;
    if (!list.length) {
      toast('沒有符合條件的歌', 'error');
      return;
    }
    player.set({ queue: tag(list.map(fromTrack).map(autoItem)), index: 0, original: null,
      radio: { kind: 'music', playlist: id, name, chosen: true }, radioError: null });
    load(0);
  } catch (e) {
    if (my === gen) toast(e.message, 'error');
  }
}

const autoItem = (it) => ({ ...it, auto: true });
// recentIds are the songs not to pick again, the latest first: those queued to come, the one
// playing, then the last ones played. With nothing else left the server goes round again from the
// end of the list, so what is queued is not added twice (review #105).
const recentIds = (s) => {
  const ahead = s.queue.slice(s.index + 1), played = s.queue.slice(Math.max(s.index - 49, 0), s.index + 1).reverse();
  return [...new Set([...ahead, ...played].map((q) => q.trackId).filter(Boolean))].slice(0, 200);
};

// topUp keeps library songs ahead while the queue goes on by itself: two ahead with preloading
// (so the next one can be warmed), else one when the queue reaches its end (now), which needs one
// song ahead to go on.
export async function topUp(now = false) {
  for (;;) {
    if (topping) {
      const got = await topping;
      if (got === 'failed') return false;
      continue; // it brought songs (enough now?), or was for an old queue: ask anew
    }
    const s = player.get();
    if (!s.radio) return false;
    const ahead = s.queue.length - 1 - s.index;
    const want = s.preload ? 2 : now ? 1 : 0;
    if (ahead >= (now ? 1 : want)) return true; // at the end, one song ahead is enough to go on
    const asked = fetchMore(s, Math.max(want - ahead, 1));
    topping = asked;
    const got = await asked;
    if (topping === asked) topping = null;
    if (got !== 'stale') return got === 'ok';
  }
}

async function fetchMore(s, n) {
  const my = gen;
  try {
    const list = await get(s.radio.playlist // a smart playlist played on: its rules pick (review #96)
      ? `/playlists/${s.radio.playlist}/next?n=${n}&not=${recentIds(s).join(',')}`
      : `/tracks/random?n=${n}&kind=${s.radio.kind}&not=${recentIds(s).join(',')}`);
    if (my !== gen || !player.get().radio) return 'stale'; // another queue began, or it was turned off
    if (!list.length) throw new Error(s.radio.playlist ? '沒有其他符合條件的歌' : '曲庫沒有可以接續的歌');
    const q = player.get();
    player.set({ queue: [...q.queue, ...tag(list.map(fromTrack).map(autoItem))], radioError: null });
    prefetchNext();
    return 'ok';
  } catch (e) {
    if (my !== gen) return 'stale';
    player.set({ radioError: e.message });
    return 'failed';
  }
}

// retryRadio picks again after a failure, and plays on when the queue had stopped at its end: only
// the queue it was asked for, as it was, with no pause since (review #104).
export async function retryRadio() {
  const s = player.get();
  const atEnd = s.index === s.queue.length - 1 && audio.paused;
  const my = gen, paused = pauses, loaded = loads, at = s.index, qid = s.queue[at] && s.queue[at].qid;
  if (!(await topUp(true)) || !atEnd) return;
  const q = player.get();
  if (my === gen && pauses === paused && loads === loaded && q.index === at && q.queue[at] && q.queue[at].qid === qid && at + 1 < q.queue.length) {
    load(at + 1);
  }
}

// endRadio stops going on by itself: library songs not reached yet leave the queue.
function endRadio() {
  gen++;
  const s = player.get();
  const queue = s.queue.filter((q, i) => i <= s.index || !q.auto);
  player.set({ queue, radio: null, radioError: null });
}

export function shuffled(items) {
  const a = [...items];
  for (let i = a.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [a[i], a[j]] = [a[j], a[i]];
  }
  return a;
}

// enqueue adds one item or a list at the end of the queue: before the songs the queue picked by
// itself from the library, which come after what the user chose (review #73).
export function enqueue(items) {
  const raw = Array.isArray(items) ? items : [items];
  const s = player.get();
  if (!raw.length) return;
  if (s.index < 0) return playQueue(raw);
  const list = tag(raw);
  const queue = [...s.queue];
  let at = queue.findIndex((q, i) => i > s.index && q.auto);
  if (at < 0) at = queue.length;
  queue.splice(at, 0, ...list);
  player.set({ queue, original: s.original && [...s.original, ...list] });
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

// cycleMode goes in order → loop the queue → loop this song → shuffle → in order.
export function cycleMode() {
  const s = player.get();
  setMode(MODES[(MODES.indexOf(s.mode) + 1) % MODES.length]);
}

// setMode changes the play mode (the player bar's button, the settings page). The current song keeps
// playing: shuffling puts the rest of the queue in random order after it, or with the library
// scope, songs from the whole library (review #72); leaving shuffle puts the queue back in its own
// order around it.
export function setMode(mode) {
  const s = player.get();
  if (mode === s.mode || !MODES.includes(mode)) return;
  const cur = s.queue[s.index];
  let queue = {};
  if (mode === 'shuffle' && cur && s.scope === 'library') {
    gen++;
    queue = { original: s.queue, from: cur.qid, queue: s.queue.slice(0, s.index + 1), radio: { kind: kindOf(cur) }, radioError: null };
  } else if (mode === 'shuffle' && cur) {
    queue = { original: s.queue, queue: [cur, ...shuffled(s.queue.filter((q) => q !== cur))], index: 0 };
  } else if (s.shuffle) {
    let list = s.original || s.queue;
    let index = cur ? list.findIndex((q) => q.qid === cur.qid) : -1;
    if (cur && index < 0) { // a library song: it stays, and the list goes on after the song shuffling began at
      const at = list.findIndex((q) => q.qid === s.from);
      list = [...list.slice(0, at + 1), cur, ...list.slice(at + 1)];
      index = at + 1;
    }
    queue = { original: null, queue: list, index: Math.max(index, cur ? 0 : -1) };
    if (s.radio && !s.radio.chosen) {
      gen++;
      queue.radio = null;
    }
  }
  player.set({ ...modeState(mode), ...queue });
  savePrefs();
  topUp();
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

// clearUpcoming keeps the songs up to the current one; library songs on their way are dropped.
export function clearUpcoming() {
  const s = player.get();
  if (s.index < 0) return;
  gen++;
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
  player.set({ queue, index: cur ? queue.findIndex((q) => q.qid === cur.qid) : s.index });
}

// playAfterCurrent moves a queued song to right after the one playing.
export function playAfterCurrent(i) {
  const s = player.get();
  moveItem(i, i < s.index ? s.index : s.index + 1);
}

// ---- this device's playback, kept across reloads ----
// The queue, the song and the place in it are kept in this browser, so reloading the page (or
// opening it again) brings the player back where it was, paused. Logging out forgets them.
// Every tab of the browser shares what is kept (review #126): it is the queue of the tab that did
// something last (changed its queue, played, paused, sought), and the place goes with that queue
// (its id) and song. Tabs hear of each other's writes late, so the two keys can still come from two
// tabs: a place is used only for the queue and entry it was kept for, else the place kept with the
// queue itself; never another queue's. A tab that brought a queue back goes on with that queue
// (and its id) until it changes it.
const SESSION = 'kanade.playback', PLACE = 'kanade.playback.at';
const MAX_SAVED = 500; // songs kept around the one playing
let sid = crypto.randomUUID(); // this tab's queue: a new one once the queue changes
let savedSid = null; // the queue this tab kept last
let restored = false; // nothing is saved before what was saved is brought back (the empty start would erase it)
let owner = false; // what is kept is this tab's
let unsaved = false; // this tab's queue changed and is not kept yet

// place is where in the current song playback is, with the song it is for.
function place() {
  const s = player.get(), item = s.queue[s.index];
  return item && { tab: sid, qid: item.qid, asset: item.assetId, t: pendingSeek !== null ? pendingSeek / 1000 : audio.currentTime || 0,
    d: isFinite(audio.duration) && audio.duration > 0 ? audio.duration : (item.durationMs || 0) / 1000 };
}

function saveSession() {
  if (!restored) return;
  clearTimeout(saveTimer);
  unsaved = false;
  const s = player.get();
  try {
    if (!s.queue.length || s.index < 0) {
      if (owner) forgetSession();
      return;
    }
    const start = Math.max(0, Math.min(s.index - 100, s.queue.length - MAX_SAVED));
    const strip = ({ resumeMs, ...it }) => it;
    localStorage.setItem(SESSION, JSON.stringify({
      tab: sid, queue: s.queue.slice(start, start + MAX_SAVED).map(strip), index: s.index - start,
      original: s.original && s.original.length <= MAX_SAVED ? s.original.map(strip) : null,
      from: s.from, radio: s.radio, at: place(),
    }));
    owner = true;
    savedSid = sid;
  } catch { /* storage full or blocked: kept for this page only */ }
}

// savePlace keeps where in the song playback is (a small key of its own, written often). When
// another tab's queue is kept, or this tab's as it was before a change, the queue is kept first:
// the two go together.
function savePlace() {
  const at = restored && place();
  if (!at) return;
  if (!owner || savedSid !== sid) saveSession();
  if (!owner) return;
  try {
    localStorage.setItem(PLACE, JSON.stringify(at));
  } catch { /* as above */ }
}

let savedShape = null, saveTimer = 0;
player.subscribe((s) => { // the queue changed: saved shortly after (a burst of changes is written once)
  const shape = [s.queue, s.index, s.original, s.from, s.radio];
  if (savedShape && shape.every((v, i) => v === savedShape[i])) return;
  // Other songs in it: another queue, which places kept for the old one do not fit.
  if (savedShape && (s.queue !== savedShape[0] || s.original !== savedShape[2])) sid = crypto.randomUUID();
  savedShape = shape;
  unsaved = true;
  clearTimeout(saveTimer);
  saveTimer = setTimeout(saveSession, 300);
});
setInterval(() => !audio.paused && savePlace(), 5000);
audio.addEventListener('pause', savePlace); // seek keeps the place too: not the seeks a loading song makes by itself
const saveOnLeave = () => {
  if (!owner && !unsaved) return;
  saveSession();
  savePlace();
};
addEventListener('pagehide', saveOnLeave);
document.addEventListener('visibilitychange', () => document.visibilityState === 'hidden' && saveOnLeave());
addEventListener('storage', (e) => { // another tab kept its own (or forgot it)
  if (e.key === SESSION || e.key === PLACE || e.key === null) owner = false;
});

// restoreSession brings back this browser's last queue, paused at the place it was left; a song
// played to its end comes back from its start. Called once logged in.
export function restoreSession() {
  if (restored) return;
  restored = true;
  if (player.get().queue.length) return;
  let saved, at;
  try {
    saved = JSON.parse(localStorage.getItem(SESSION) || 'null');
    at = JSON.parse(localStorage.getItem(PLACE) || 'null');
  } catch {
    return;
  }
  const ok = (list) => Array.isArray(list) && list.length > 0 && list.every((q) => q && q.assetId > 0 && q.qid > 0);
  if (!saved || !ok(saved.queue)) return;
  const original = ok(saved.original) ? saved.original : null;
  const index = Math.min(Math.max(Number(saved.index) || 0, 0), saved.queue.length - 1);
  qseq = Math.max(qseq, ...saved.queue.map((q) => q.qid), ...(original || []).map((q) => q.qid));
  const item = saved.queue[index];
  // The place kept most recently, if it is for this queue and song; else the one kept with the queue.
  const mine = (p) => p && p.tab === saved.tab && p.qid === item.qid && (p.asset ?? item.assetId) === item.assetId;
  const p = mine(at) ? at : mine(saved.at) ? saved.at : null;
  let t = p ? Number(p.t) || 0 : 0;
  const d = (p && Number(p.d)) || (item.durationMs || 0) / 1000;
  if (d && t > d - 2) t = 0;
  item.resumeMs = Math.round(t * 1000);
  restored = false; // bringing it back is not a change of this tab's to keep
  player.set({ queue: saved.queue, index, original, from: saved.from || 0, radio: saved.radio || null, radioError: null });
  load(index, false);
  restored = true;
  clearTimeout(saveTimer);
  unsaved = false;
  sid = saved.tab || sid; // the same queue: what is kept is this tab's, until another tab keeps its own
  savedSid = saved.tab || null;
  owner = true;
}

function forgetSession() {
  try {
    localStorage.removeItem(SESSION);
    localStorage.removeItem(PLACE);
  } catch { /* nothing kept */ }
}

// resetPlayer stops playback and forgets the queue (clearing the queue; signOut: logging out,
// which also forgets what another tab kept). With report, the playback so far is reported first.
export function resetPlayer(withReport = true, signOut = false) {
  if (withReport) report(false, true);
  if (owner || signOut) forgetSession();
  owner = false;
  gen++;
  session = null;
  pendingSeek = null;
  audio.pause();
  audio.removeAttribute('src');
  audio.load();
  player.set({ queue: [], index: -1, original: null, playing: false, buffering: false, time: 0, scrub: null, duration: 0, nowPlayingOpen: false,
    radio: null, radioError: null });
  clearTimeout(saveTimer); // an empty queue is nothing to keep
  unsaved = false;
  if ('mediaSession' in navigator) navigator.mediaSession.metadata = null;
}

export function toggle() {
  if (!current()) return;
  if (audio.paused) audio.play().catch(() => {});
  else {
    pauses++;
    audio.pause();
  }
}

// next moves on. Past the end: a queue going on by itself picks from the library; looping the
// queue starts it over; with "keep playing" the queue goes on with library songs, also after a
// round of a shuffled queue; else a shuffled queue starts over in a new order, and in order it
// stops (review #73: looping a song or the list comes first).
export async function next() {
  const s = player.get();
  if (s.index + 1 < s.queue.length) return load(s.index + 1);
  if (!s.queue.length) return;
  if (s.radio || (s.autoContinue && s.mode !== 'all')) {
    if (!s.radio) player.set({ radio: { kind: kindOf(s.queue[s.index]) }, radioError: null });
    const my = gen, paused = pauses, loaded = loads;
    const ok = await topUp(true);
    // Another queue, or another song chosen while waiting: this wait has nothing more to do.
    if (my !== gen || loads !== loaded) return;
    const q = player.get();
    if (ok && q.index + 1 < q.queue.length) load(q.index + 1, pauses === paused); // paused while waiting: the next song waits too
    else stopAtEnd(); // nothing to go on with: radioError says why
    return;
  }
  if (s.repeat !== 'off') {
    if (s.shuffle && s.queue.length > 1) {
      const queue = shuffled(s.queue);
      if (queue[0] === s.queue[s.index]) queue.push(queue.shift()); // not the song that just played
      player.set({ queue });
    }
    load(0, true, true);
  } else {
    stopAtEnd();
  }
}

function stopAtEnd() {
  audio.pause();
  player.set({ playing: false });
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
  savePlace();
};
export const scrubTo = (sec) => isFinite(sec) && player.set({ scrub: sec });
export const endScrub = (commit) => {
  const { scrub } = player.get();
  if (scrub === null) return;
  if (commit) seek(scrub);
  else player.set({ scrub: null });
};

// Warm the server's stream cache for the next track (plan §5: preload at most the next one),
// unless the settings turned it off (review #78).
function prefetchNext() {
  const s = player.get();
  if (!s.preload) return;
  const n = s.repeat === 'one' ? null : s.queue[s.index + 1] || (s.repeat === 'all' && !s.radio ? s.queue[0] : null);
  if (n) fetch(streamURL(n.assetId), { headers: { Range: 'bytes=0-0' }, credentials: 'same-origin' }).catch(() => {});
}

let lastTick = 0;
audio.addEventListener('timeupdate', () => {
  const now = performance.now();
  if (now - lastTick < 250) return; // 4 updates a second is plenty
  lastTick = now;
  player.set({ time: audio.currentTime });
  if (player.get().sleep) checkSleep();
});

// ---- sleep timer (review #98) ----
// On this device only: playback pauses at a time (its last 20 s fading out) or when the playing song
// ends. The time is a deadline, checked as the song plays, on a timer and when the page shows again,
// so a page left in the background is not late; changing songs does not reset it. Closing the page
// ends it with the playback.
let sleepTick = 0;
function armSleep() {
  clearInterval(sleepTick);
  sleepTick = setInterval(checkSleep, 1000);
  checkSleep();
}
function endSleep() {
  clearInterval(sleepTick);
  sleepTick = 0;
  player.set({ sleep: null });
  applyVolume();
}
function checkSleep() {
  const s = player.get().sleep;
  if (s && s.until && Date.now() >= s.until) {
    endSleep();
    // As if the listener paused: nothing goes on by itself, not even a song still being picked
    // while the queue waits at its end (review #103).
    pauses++;
    if (!audio.paused) audio.pause();
    toast('睡眠定時到了，已暫停播放');
    return;
  }
  applyVolume();
}
export function setSleep(minutes) {
  player.set({ sleep: { until: Date.now() + minutes * 60_000 } });
  armSleep();
}
export function sleepAfterTrack() {
  player.set({ sleep: { endOfTrack: true } });
  armSleep();
}
export function extendSleep(minutes) {
  const s = player.get().sleep;
  if (!s || !s.until) return;
  player.set({ sleep: { until: Math.max(s.until, Date.now()) + minutes * 60_000 } });
  applyVolume();
}
export const cancelSleep = endSleep;
document.addEventListener('visibilitychange', () => player.get().sleep && checkSleep());

// playBookmark plays a bookmark's place on the file it was made on (its asset); one whose file
// changed since plays only when asked (anyway), on the song's file now.
export function playBookmark(b, anyway = false) {
  if (!b.track || (!b.asset && !anyway)) return;
  const item = fromTrack(b.track);
  if (b.asset) Object.assign(item, { asset: b.asset, assetId: b.asset.id, durationMs: b.asset.duration_ms });
  const cur = current();
  if (cur && cur.assetId === item.assetId) {
    seek(b.position_ms / 1000);
    if (audio.paused) audio.play().catch(() => {});
    return;
  }
  playQueue([{ ...item, resumeMs: b.position_ms }], 0);
}
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
  topUp();
  prefetchNext();
});
audio.addEventListener('ended', () => {
  report(true);
  session = null;
  const sleep = player.get().sleep;
  if (sleep && sleep.endOfTrack) { // before looping the song or the queue, before going on by itself
    endSleep();
    stopAtEnd();
    toast('這首播完了，已依睡眠定時停止');
    return;
  }
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

import { get } from './api.js';
import { createStore } from './store.js';

// ---- sound effects: the equalizer, and the volume balance (reviews #139, #136) ----
// Kept per device, for its own headphones or speakers. The audio element plays as it is — the
// browser's own way, with the phone's sound settings — unless the equalizer has a curve to apply (on,
// and not flat): only then does its sound go through Web Audio, in a graph made for playback
// (latencyHint 'playback'; on Android the low-latency path leaves the system's sound effects out).
// An element that fed a graph cannot leave it, so going back to its own sound takes a fresh element
// where the old one was (the player's fresh), and the graph is closed. In the graph the volume, mute,
// the sleep timer's fade and the balance are applied by a gain of its own (Safari ignores an
// element's volume there), then a pre-gain takes back the curve's largest boost (the bands added up
// as they overlap) before the bands, so nothing clips.

const KEY = 'kanade.effects';
export const BANDS = [31, 62, 125, 250, 500, 1000, 2000, 4000, 8000, 16000];
export const MAX_DB = 12;
// Built-in curves (dB per band).
export const PRESETS = [
  ['flat', '平直', [0, 0, 0, 0, 0, 0, 0, 0, 0, 0]],
  ['bass', '低音增強', [6, 5, 4, 2, 0, 0, 0, 0, 0, 0]],
  ['vocal', '人聲', [-2, -2, -1, 0, 2, 4, 4, 2, 0, -1]],
  ['treble', '高音增強', [0, 0, 0, 0, 0, 0, 1, 3, 5, 6]],
  ['soft', '柔和', [0, 0, 0, 0, 0, 0, -1, -2, -3, -4]],
  ['loud', '響度', [4, 3, 1, 0, -1, -1, 0, 1, 3, 4]],
];

// Loudness the volume balance brings every song to (LUFS): ReplayGain's, or louder.
export const TARGETS = [[-18, '標準（−18 LUFS）'], [-14, '較大聲（−14 LUFS）']];

const clamp = (v, lo, hi) => Math.min(Math.max(v, lo), hi);
const curve = (g) => (Array.isArray(g) && g.length === BANDS.length && g.every((x) => typeof x === 'number' && isFinite(x))
  ? g.map((x) => clamp(Math.round(x * 2) / 2, -MAX_DB, MAX_DB)) : null);

function load() {
  let p = {};
  try {
    p = JSON.parse(localStorage.getItem(KEY) || '{}') || {};
  } catch { /* storage blocked: defaults */ }
  const customs = {};
  for (const [name, g] of Object.entries(p.customs || {})) {
    if (curve(g)) customs[name] = curve(g);
  }
  return {
    eq: p.eq === true,
    preset: typeof p.preset === 'string' ? p.preset : 'flat', // a built-in key, 'custom:<name>', or '' (changed by hand)
    gains: curve(p.gains) || PRESETS[0][2].slice(),
    customs,
    balance: ['track', 'album'].includes(p.balance) ? p.balance : 'off',
    target: TARGETS.some(([t]) => t === p.target) ? p.target : TARGETS[0][0],
  };
}

// effects holds the settings, and whether this browser can apply them (unsupported) or the graph
// is stopped (suspended: it waits for a tap or a key to sound).
export const effects = createStore({ ...load(), unsupported: false, suspended: false });

export function setEffects(patch) {
  effects.set(patch);
  const { eq, preset, gains, customs, balance, target } = effects.get();
  try {
    localStorage.setItem(KEY, JSON.stringify({ eq, preset, gains, customs, balance, target }));
  } catch { /* this page only */ }
  settle();
  if ('balance' in patch || 'target' in patch) {
    learn(around());
    reapply();
  }
}

// headroom is the curve's largest band.
export const headroom = (gains) => Math.max(0, ...gains);
const flat = (gains) => gains.every((g) => g === 0);
// wanted: the equalizer has something to do.
const wanted = () => {
  const { eq, gains, unsupported } = effects.get();
  return eq && !flat(gains) && !unsupported;
};

let element = null, reapply = () => {}, around = () => [], renew = null;
// The graph, while the equalizer has a curve: source → master (volume) → pre → bands → speakers.
let ctx = null, master = null, pre = null, filters = [];

// attach gives the player's audio element, how to apply its volume again (once the graph takes the
// volume over, or gives it back, or a song's loudness is known), the queue's songs around the one
// playing, and how to go on with a fresh element.
export function attach(audio, apply, near, fresh) {
  element = audio;
  reapply = apply;
  around = near;
  renew = fresh;
}

function build() {
  if (ctx || !element) return !!ctx;
  const AC = window.AudioContext || window.webkitAudioContext;
  if (!AC) {
    effects.set({ unsupported: true });
    return false;
  }
  let c;
  try {
    c = new AC({ latencyHint: 'playback' });
    const src = c.createMediaElementSource(element);
    master = c.createGain();
    pre = c.createGain();
    filters = BANDS.map((f) => {
      const b = c.createBiquadFilter();
      b.type = 'peaking';
      b.frequency.value = f;
      b.Q.value = Q;
      return b;
    });
    [src, master, pre, ...filters, c.destination].reduce((a, b) => (a.connect(b), b));
  } catch (e) {
    console.warn('sound effects unavailable', e);
    if (c) c.close().catch(() => {});
    master = pre = null;
    filters = [];
    effects.set({ unsupported: true });
    return false;
  }
  ctx = c;
  ctx.onstatechange = () => effects.set({ suspended: !!ctx && ctx.state !== 'running' && !element.paused });
  tune(true);
  reapply(); // the volume moves into the graph
  wake();
  return true;
}

// teardown closes the graph: the song goes on in a fresh element, with its own sound.
function teardown() {
  if (!ctx) return;
  const c = ctx;
  ctx = master = pre = null;
  filters = [];
  effects.set({ suspended: false });
  if (renew) renew(); // applies the volume there, on the element again
  c.close().catch(() => {});
}

// settle builds or closes the graph as the settings need it, and sets the curve.
function settle() {
  if (wanted()) {
    if (!ctx) build();
    tune();
  } else {
    teardown();
  }
}

// tune sets the curve, gliding to it so a change makes no click (at once when the graph is new).
function tune(now = false) {
  if (!ctx) return;
  const { gains } = effects.get();
  const at = ctx.currentTime, set = (p, v) => (now ? (p.value = v) : p.setTargetAtTime(v, at, 0.03));
  filters.forEach((f, i) => set(f.gain, gains[i]));
  set(pre.gain, 10 ** (-boostOf(gains) / 20));
}

const Q = 1.41; // about an octave each
let probe = null; // an offline context the curve's response is worked out on

// boostOf is the most the curve raises any frequency, the bands added up as they overlap, from the
// filters' own response.
function boostOf(gains) {
  if (flat(gains)) return 0;
  try {
    probe = probe || new OfflineAudioContext(1, 1, 48000);
  } catch {
    return headroom(gains);
  }
  const n = 256, hz = new Float32Array(n), mag = new Float32Array(n), phase = new Float32Array(n), total = new Float32Array(n);
  for (let i = 0; i < n; i++) hz[i] = 20 * 1000 ** (i / (n - 1)); // 20 Hz to 20 kHz
  BANDS.forEach((f, b) => {
    const filter = probe.createBiquadFilter();
    filter.type = 'peaking';
    filter.frequency.value = f;
    filter.Q.value = Q;
    filter.gain.value = gains[b];
    filter.getFrequencyResponse(hz, mag, phase);
    for (let i = 0; i < n; i++) total[i] += 20 * Math.log10(mag[i]);
  });
  return Math.max(0, ...total);
}

// The boost a curve is taken back by, to show.
export const boost = (gains) => Math.round(boostOf(gains) * 10) / 10;

// wake gets the graph ready before playing: built if the equalizer has a curve, running again if the
// browser stopped it. Called on every play; a play the listener asked for (a tap, a key) lets the
// browser start it.
export function wake() {
  if (wanted() && !ctx) build();
  if (ctx && ctx.state !== 'running') ctx.resume().catch(() => {});
}

// output applies the volume: level (the volume chosen, faded by the sleep timer), muted, and gain
// (the balance, linear; it can raise the level only through the graph, else up to the element's 1).
export function output(level, muted, gain = 1) {
  if (!element) return;
  if (!ctx) {
    element.volume = clamp(level * Math.min(gain, 1), 0, 1);
    element.muted = muted;
    return;
  }
  element.volume = 1;
  element.muted = false;
  master.gain.setTargetAtTime(muted ? 0 : level * gain, ctx.currentTime, 0.015);
}

// A graph the browser stopped while something plays is started again by the next tap or key.
for (const ev of ['pointerdown', 'keydown']) {
  addEventListener(ev, () => {
    if (ctx && ctx.state !== 'running' && element && !element.paused) ctx.resume().catch(() => {});
  }, true);
}

// presetOf names the preset a curve matches (built-in or saved), '' when it matches none.
export function presetOf(gains, customs) {
  const same = (g) => g.every((x, i) => x === gains[i]);
  const built = PRESETS.find(([, , g]) => same(g));
  if (built) return built[0];
  const name = Object.keys(customs).find((n) => same(customs[n]));
  return name ? 'custom:' + name : '';
}

// ---- the volume balance (review #136) ----
// Every song (with 'album', every album) brought to the target loudness: the target less how loud
// the server measured it, never so far up that its peaks pass −1 dBFS. Through the graph a quiet
// song is raised too; without it only the louder ones are brought down (an element's volume stops at
// 1). A song not measured yet is taken to be as loud as the library's median.

const ASK_AGAIN = 5 * 60_000; // a song not measured is asked about again after this (a scan may be on it)
const known = { assets: new Map(), albums: new Map(), median: null }; // id -> { v: loudness or null, t }
const asking = { assets: new Set(), albums: new Set() };

const stale = (map, id) => {
  const k = map.get(id);
  return !k || (k.v === null && Date.now() - k.t > ASK_AGAIN);
};

// learn asks the server how loud the songs (and their albums) are, those it was not asked about;
// the volume is applied again once it answers.
export function learn(items) {
  const { balance } = effects.get();
  if (balance === 'off') return;
  const pick = (kind, key) => [...new Set(items.map((q) => q && q[key]))].filter((id) => id > 0 && !asking[kind].has(id) && stale(known[kind], id));
  const assets = pick('assets', 'assetId'), albums = balance === 'album' ? pick('albums', 'albumId') : [];
  if (!assets.length && !albums.length && known.median !== null) return;
  assets.forEach((id) => asking.assets.add(id));
  albums.forEach((id) => asking.albums.add(id));
  get(`/loudness?assets=${assets.join(',')}&albums=${albums.join(',')}`).then((r) => {
    const t = Date.now();
    assets.forEach((id) => known.assets.set(id, { v: r.assets[id] || null, t }));
    albums.forEach((id) => known.albums.set(id, { v: r.albums[id] || null, t }));
    known.median = r.median ?? null;
    reapply();
  }, () => {}).finally(() => {
    assets.forEach((id) => asking.assets.delete(id));
    albums.forEach((id) => asking.albums.delete(id));
  });
}

// balanceGain is the linear gain that brings item to the target loudness (1 with the balance off,
// or with nothing known yet).
export function balanceGain(item) {
  const { balance, target } = effects.get();
  if (balance === 'off' || !item) return 1;
  const album = balance === 'album' && item.albumId ? known.albums.get(item.albumId) : null;
  const own = known.assets.get(item.assetId);
  const l = (album && album.v) || (own && own.v);
  const lufs = l ? l.lufs : known.median;
  if (lufs === null || lufs === undefined) return 1;
  let db = target - lufs;
  if (l) db = Math.min(db, -1 - l.peak); // its peaks stay under −1 dBFS
  return 10 ** (db / 20);
}

import { createStore } from './store.js';

// ---- sound effects: the equalizer, and the volume balance (reviews #139, #136) ----
// Kept per device, for its own headphones or speakers. Nothing goes through Web Audio until the
// equalizer is first turned on: the audio element plays as it is (which also keeps playing with the
// screen locked everywhere). From then on its sound goes through one graph, built once for the page
// (an element can feed only one); turning the equalizer off flattens it. In the graph the volume,
// the sleep timer's fade and the balance are applied by a gain of its own (Safari ignores an
// element's volume there), before the equalizer, whose boosts are taken back by a pre-gain so that
// they do not clip, and a limiter last against what is left.

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
  };
}

// effects holds the settings, and whether this browser can apply them (unsupported) or the graph
// is stopped (suspended: it waits for a tap or a key to sound).
export const effects = createStore({ ...load(), unsupported: false, suspended: false });

export function setEffects(patch) {
  effects.set(patch);
  const { eq, preset, gains, customs, balance } = effects.get();
  try {
    localStorage.setItem(KEY, JSON.stringify({ eq, preset, gains, customs, balance }));
  } catch { /* this page only */ }
  if (patch.eq) build();
  tune();
}

// headroom is how much the curve's largest boost takes off the level, so a boost does not clip.
export const headroom = (gains) => Math.max(0, ...gains);

let element = null, reapply = () => {};
let ctx = null, master = null, pre = null, filters = [];

// attach gives the player's audio element, and how to apply its volume again (once the graph
// takes the volume over).
export function attach(audio, apply) {
  element = audio;
  reapply = apply;
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
    c = new AC();
    const src = c.createMediaElementSource(element);
    master = c.createGain();
    pre = c.createGain();
    filters = BANDS.map((f) => {
      const b = c.createBiquadFilter();
      b.type = 'peaking';
      b.frequency.value = f;
      b.Q.value = 1.41; // about an octave each
      b.gain.value = 0;
      return b;
    });
    const limiter = c.createDynamicsCompressor();
    limiter.threshold.value = -1;
    limiter.knee.value = 0;
    limiter.ratio.value = 20;
    limiter.attack.value = 0.002;
    limiter.release.value = 0.1;
    [src, master, pre, ...filters, limiter, c.destination].reduce((a, b) => (a.connect(b), b));
  } catch (e) {
    console.warn('sound effects unavailable', e);
    if (c) c.close().catch(() => {});
    effects.set({ unsupported: true });
    return false;
  }
  ctx = c;
  ctx.onstatechange = () => effects.set({ suspended: ctx.state !== 'running' && !element.paused });
  reapply(); // the volume moves into the graph
  wake();
  return true;
}

// tune sets the curve (flat when the equalizer is off), gliding to it so a change makes no click.
function tune() {
  if (!ctx) return;
  const { eq, gains } = effects.get();
  const now = ctx.currentTime;
  filters.forEach((f, i) => f.gain.setTargetAtTime(eq ? gains[i] : 0, now, 0.03));
  pre.gain.setTargetAtTime(eq ? 10 ** (-headroom(gains) / 20) : 1, now, 0.03);
}

// wake gets the graph ready before playing: built if the equalizer is on, running again if the
// browser stopped it. Called on every play; a play the listener asked for (a tap, a key) lets the
// browser start it.
export function wake() {
  if (effects.get().eq && !ctx) build();
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

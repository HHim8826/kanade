import { ApiError, post } from './api.js';
import * as P from './player.js';
import { device, deviceName, tab } from './presence.js';
import { createStore } from './store.js';
import { toast } from './ui.js';

// The account's other devices (review #138). Each tab keeps an event stream open: down it come the
// devices as they change, the commands other tabs send this one, and the answers to its own. A
// device can take another's playback over ("continue here": views/remote.js asks for its snapshot,
// plays it, then has it pause), or control it: while this tab controls another, what is asked of
// the player here is sent there (player.js's remote hook), and the player bar shows that device as
// it last told, never what was only asked of it.

// devices is the account's players as the server last listed them (list), when (at, this clock),
// and whether the stream is open.
export const devices = createStore({ list: [], at: 0, connected: false });
// control is the player this tab controls (target), its device's name, and the commands not yet
// answered (busy).
export const control = createStore({ target: null, name: '', busy: 0 });

export class RemoteError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

const reasons = {
  offline: '目前沒有連線',
  timeout: '沒有回應',
  blocked: '的瀏覽器要先在那台裝置上點一下頁面才能播放',
  stale: '的佇列已經變了',
  changed: '播放的內容已經變了',
  empty: '沒有在播放',
  none: '沒有找到可以播放的歌',
  bad: '看不懂這個要求',
};

// ---- the stream ----

let source = null;
let on = false;
let again = 0, wait = 1000;
let offset = 0; // the server's clock less this one (ms)
let lastSeq = 0; // the last command carried out

export function startRemote() {
  if (on) return;
  on = true;
  connect();
}

// stopRemote closes the stream: logged out.
export function stopRemote() {
  on = false;
  clearTimeout(again);
  if (source) source.close();
  source = null;
  endControl();
  devices.set({ list: [], at: 0, connected: false });
}

function connect() {
  if (!on || source) return;
  const es = source = new EventSource(`/api/v1/players/${tab}/events?device=${device}&name=${encodeURIComponent(deviceName)}`);
  es.addEventListener('hello', (e) => {
    offset = JSON.parse(e.data).now - Date.now();
    wait = 1000;
    devices.set({ connected: true });
  });
  es.addEventListener('players', (e) => devices.set({ list: JSON.parse(e.data), at: Date.now() }));
  es.addEventListener('command', (e) => run(JSON.parse(e.data)));
  es.addEventListener('ack', (e) => answered(JSON.parse(e.data)));
  es.onerror = () => {
    devices.set({ connected: false });
    if (es.readyState !== EventSource.CLOSED) return; // the browser tries again by itself
    // Refused (logged out, say): tried again a while later, while still logged in.
    es.close();
    if (source === es) source = null;
    clearTimeout(again);
    again = setTimeout(connect, wait);
    wait = Math.min(wait * 2, 60_000);
  };
}

// ---- asking another device ----

const waiting = new Map(); // command ID -> what to do with its answer
const early = new Map(); // answers that came before the command's ID did

// send asks player target to do op; it is what the answer carries once done there, or fails with
// why (a RemoteError: offline, timeout, or the target's reason).
export async function send(target, op, args) {
  let r;
  try {
    r = await post(`/players/${target}/commands`, { from: tab, op, args });
  } catch (e) {
    throw e instanceof ApiError && e.status === 409 ? new RemoteError('offline', reasons.offline) : e;
  }
  return new Promise((resolve, reject) => {
    const done = (a) => (a.ok ? resolve(a.data) : reject(new RemoteError(a.error || 'failed', reasons[a.error] || a.error || '沒能完成')));
    if (early.has(r.id)) {
      done(early.get(r.id));
      early.delete(r.id);
      return;
    }
    const timer = setTimeout(() => {
      waiting.delete(r.id);
      reject(new RemoteError('timeout', reasons.timeout));
    }, 11_000);
    waiting.set(r.id, { done, timer });
  });
}

function answered(a) {
  const w = waiting.get(a.id);
  if (!w) {
    early.set(a.id, a);
    setTimeout(() => early.delete(a.id), 15_000);
    return;
  }
  waiting.delete(a.id);
  clearTimeout(w.timer);
  w.done(a);
}

// ---- asked by another device ----

let executing = false; // carrying out a command: the player here, past the remote hook
function local(f) {
  executing = true;
  try {
    return f();
  } finally {
    executing = false;
  }
}
const fail = (code) => {
  throw new RemoteError(code, reasons[code] || code);
};

// started waits a few seconds for this tab to play: refused by the browser (it wants a click
// here first), it says so; still loading, it is taken as playing.
function started() {
  return new Promise((resolve, reject) => {
    const settled = (s) => s.blocked || (s.playing && !s.buffering);
    const end = (s) => (s.blocked ? reject(new RemoteError('blocked', reasons.blocked)) : resolve());
    if (settled(P.player.get())) return end(P.player.get());
    const unsubscribe = P.player.subscribe((s) => {
      if (!settled(s)) return;
      unsubscribe();
      clearTimeout(timer);
      end(s);
    });
    const timer = setTimeout(() => {
      unsubscribe();
      resolve();
    }, 4000);
  });
}
const playing = () => {
  P.player.set({ blocked: false });
  return started();
};

// clean keeps the songs of a list sent here (as queue entries: this queue numbers them anew).
function clean(items) {
  const list = Array.isArray(items) ? items.filter((it) => it && typeof it === 'object' && Number(it.assetId) > 0).slice(0, 1000) : [];
  if (!list.length) fail('bad');
  return list.map(({ qid, auto, ...it }) => it);
}

const ops = {
  toggle: async () => {
    const was = P.player.get().playing;
    local(P.toggle);
    if (!was && P.current()) await playing();
  },
  play: async () => {
    if (!P.current()) fail('empty');
    local(P.resume);
    await playing();
  },
  pause: () => local(P.pause),
  next: () => { local(P.next); },
  prev: () => local(P.prev),
  seek: ({ sec }) => {
    if (!(Number(sec) >= 0)) fail('bad');
    local(() => P.seek(Number(sec)));
  },
  seekBy: ({ by }) => {
    if (!Number.isFinite(Number(by))) fail('bad');
    local(() => P.seekBy(Number(by)));
  },
  volume: ({ v }) => local(() => P.setVolume(Number(v) || 0)),
  volumeBy: ({ up }) => local(() => P.nudgeVolume(!!up)),
  mute: ({ on: muted }) => {
    if (!!P.player.get().muted !== !!muted) local(P.toggleMute);
  },
  mode: ({ mode }) => {
    if (!P.MODES.includes(mode)) fail('bad');
    local(() => P.setMode(mode));
  },
  jump: async ({ qid }) => {
    const i = P.player.get().queue.findIndex((q) => q.qid === qid);
    if (i < 0) fail('stale');
    local(() => P.playAt(i));
    await playing();
  },
  enqueue: ({ items }) => local(() => P.enqueue(clean(items))),
  playNext: ({ items }) => local(() => P.playNext(clean(items))),
  playQueue: async ({ items, index }) => {
    const list = clean(items);
    local(() => P.playQueue(list, Math.min(Math.max(Math.floor(Number(index)) || 0, 0), list.length - 1)));
    await playing();
  },
  // The whole library or a smart playlist played on and on: picked here, as it goes (review #204).
  shuffle: async ({ kind }) => {
    if (!(await local(() => P.playLibraryShuffle(kind === 'spoken' ? 'spoken' : 'music')))) fail('none');
    await playing();
  },
  smart: async ({ id, name }) => {
    if (!(Number(id) > 0)) fail('bad');
    if (!(await local(() => P.playSmart(Number(id), String(name || ''))))) fail('none');
    await playing();
  },
  snapshot: () => P.snapshot(),
  // release pauses this tab for the device that took its playback over: only while it still plays
  // the queue taken (nothing else played here since) at the song taken or the one after. Its sleep
  // timer ends with it: the other device asked whether to take it.
  release: ({ gen, qids }) => {
    const cur = P.current();
    if (P.generation() !== gen || !cur || !Array.isArray(qids) || !qids.includes(cur.qid)) fail('changed');
    local(P.pause);
    if (P.player.get().sleep) P.cancelSleep();
  },
};

async function run(cmd) {
  if (cmd.seq <= lastSeq) return;
  lastSeq = cmd.seq;
  // Late (from a tab woken in the background, say): its sender gave up on it, and it is not done.
  if (Date.now() + offset > cmd.expires) return;
  let ok = true, error = '', data = null;
  try {
    if (!Object.hasOwn(ops, cmd.op)) fail('bad');
    data = (await ops[cmd.op](cmd.args || {})) ?? null;
  } catch (e) {
    ok = false;
    error = e.code || 'failed';
  }
  post(`/players/${tab}/acks`, { seq: cmd.seq, ok, error, data }).catch(() => {});
}

// ---- controlling another device ----

// target is the device this tab controls, as last listed (null: none, or not listed now).
export function target() {
  const t = control.get().target;
  return (t && devices.get().list.find((d) => d.player === t)) || null;
}

// controlDevice controls device d from here: this tab pauses, one device playing at a time.
export function controlDevice(d) {
  if (!d || !d.player || d.player === tab) return;
  local(P.pause);
  dropVolume();
  control.set({ target: d.player, name: d.device_name || '另一台裝置', busy: 0 });
  P.setRemote(route);
  P.player.set({ nowPlayingOpen: false });
}

export function endControl() {
  dropVolume();
  if (!control.get().target) return;
  P.setRemote(null);
  control.set({ target: null, name: '', busy: 0 });
}

// Something starting to play here (asked by a third device, say) ends the control: the player bar
// shows, and its buttons work, what plays (review #204).
let wasPlaying = false;
P.player.subscribe((s) => {
  if (s.playing && !wasPlaying && control.get().target) {
    toast(`這裡開始播放了，已結束控制「${control.get().name}」`);
    endControl();
  }
  wasPlaying = s.playing;
});

// command sends the device controlled op, saying when it is done (said) or why not.
export function command(op, args, said) {
  const { target: to, name } = control.get();
  if (!to) return Promise.resolve();
  control.set((c) => ({ busy: c.busy + 1 }));
  return send(to, op, args).then(
    (data) => {
      if (said) toast(said);
      return data;
    },
    (e) => {
      toast(`「${name}」${e.message}`, 'error');
      throw e;
    },
  ).finally(() => control.set((c) => ({ busy: Math.max(0, c.busy - 1) })));
}
const quietly = (p) => p.catch(() => {});

// around keeps at most 500 songs of a list, around the one at index.
function around(items, index) {
  const start = Math.max(0, Math.min(index - 100, items.length - 500));
  return { items: items.slice(start, start + 500), index: index - start };
}

// A volume dragged is sent a few times a second, its latest value; it is for the device it was
// dragged for, and not sent once another is controlled (review #206).
let volumeTimer = 0, volumeTo = 0;
function dropVolume() {
  clearTimeout(volumeTimer);
  volumeTimer = 0;
}

// route sends what is asked of the player here to the device controlled (player.js's remote hook).
function route(op, args = {}) {
  if (executing || !control.get().target) return false;
  const d = target(), name = control.get().name;
  switch (op) {
    case 'cycleMode':
      quietly(command('mode', { mode: P.MODES[(P.MODES.indexOf(d && d.mode) + 1) % P.MODES.length] }));
      break;
    case 'mute':
      quietly(command('mute', { on: !(d && d.muted) }));
      break;
    case 'volume':
      volumeTo = args.v;
      if (!volumeTimer) volumeTimer = setTimeout(() => { volumeTimer = 0; quietly(command('volume', { v: volumeTo })); }, 200);
      break;
    case 'playQueue':
      quietly(command('playQueue', around(args.items, args.index)));
      break;
    case 'enqueue':
    case 'playNext': {
      const n = args.items.length, first = args.items[0].title;
      quietly(command(op, { items: args.items.slice(0, 500) }, op === 'enqueue'
        ? (n > 1 ? `已將 ${n} 首加入「${name}」的佇列` : `已加入「${name}」的佇列：${first}`)
        : (n > 1 ? `「${name}」接下來播放 ${n} 首` : `「${name}」下一首播放：${first}`)));
      break;
    }
    default:
      quietly(command(op, args));
  }
  return true;
}

// positionOf is where device d is in its song now (ms), from where it last said it was.
export function positionOf(d, now = Date.now()) {
  if (!d) return 0;
  let p = d.position_ms || 0;
  if (d.state === 'playing') p += now - devices.get().at;
  return d.duration_ms ? Math.min(p, d.duration_ms) : p;
}

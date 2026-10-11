import { useEffect, useRef, useState } from '../../vendor/hooks.module.js';
import { api, get } from '../api.js';
import {
  adopt, cycleMode, fromEntry, generation, next, player, playQueue, prev, seek, setVolume, toggle, toggleMute,
} from '../player.js';
import { device, deviceName, tab } from '../presence.js';
import { command, control, controlDevice, devices, endControl, positionOf, send, target } from '../remote.js';
import { useStore } from '../store.js';
import { Cover, Dialog, Empty, Icon, IconButton, Spinner, fmtTime, html, openMenu, showDialog, toast, useLayer } from '../ui.js';
import { confirmDialog, textDialog } from './organize.js';

// The account's other devices (review #138): the devices dialog lists them, to continue here what
// one plays or to control it; the player bar shows the device controlled; with nothing playing
// here, the bar offers what another device plays.

const nameOf = (d) => d.device_name || '另一台裝置';
const hasSong = (d) => d.state === 'playing' || d.state === 'paused';

function ago(ms) {
  const min = Math.floor((Date.now() - ms) / 60_000);
  if (min < 1) return '剛剛';
  if (min < 60) return `${min} 分鐘前`;
  return `${Math.floor(min / 60)} 小時前`;
}

// useTick re-renders every second while on (a time that moves by itself).
function useTick(on) {
  const [, set] = useState(0);
  useEffect(() => {
    if (!on) return;
    const t = setInterval(() => set((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, [on]);
}

// others are the devices but this tab: playing first, then paused, connected, idle, and what
// browsers no longer here played last, the latest first.
function others(list) {
  const rank = (d) => (d.gone ? 5 : d.state === 'playing' ? 0 : d.state === 'paused' ? (d.online ? 1 : 2) : 3);
  return list.filter((d) => d.player !== tab)
    .sort((a, b) => rank(a) - rank(b) || (b.seen || b.changed) - (a.seen || a.changed) || (a.player < b.player ? -1 : 1));
}

function status(d) {
  if (d.gone) return `已離開 · 最後播放《${d.title}》，${ago(d.seen)}`;
  if (d.state === 'idle') return '閒置';
  const what = `${d.state === 'playing' ? '播放中' : '已暫停'}：${d.title}${d.artist ? ' — ' + d.artist : ''}`;
  return d.online ? what : `${what}（目前收不到遙控）`;
}

export const showDevices = () => showDialog((close) => html`<${DevicesDialog} close=${close} />`);

// DevicesButton opens the devices; it says how many others play.
export function DevicesButton({ size }) {
  const n = useStore(devices, (s) => s.list.filter((d) => d.player !== tab && d.state === 'playing').length);
  return html`<span class="devices-button"><${IconButton} icon="devices" size=${size}
    label=${n ? `播放裝置（另有 ${n} 台在播放）` : '播放裝置'} onClick=${showDevices} />${n > 0 && html`<span class="badge" aria-hidden="true">${n}</span>`}</span>`;
}

function rename(d) {
  textDialog({
    title: '裝置名稱', label: '名稱', action: '儲存', maxLength: 40, initial: nameOf(d),
    hint: '這個帳號的所有裝置都會用這個名稱稱呼它。',
    onSubmit: (name) => api('PUT', '/devices/' + d.device, { name }).then(() => toast('已更改裝置名稱')),
  });
}
const unname = (d) => api('PUT', '/devices/' + d.device, { name: '' }).then(() => toast('已改回瀏覽器的名稱'), (e) => toast(e.message, 'error'));

function DevicesDialog({ close }) {
  return html`<${Dialog} title="播放裝置" onClose=${close} actions=${html`<button class="btn text" onClick=${close}>關閉</button>`}>
    <${DeviceList} done=${close} />
  <//>`;
}

// DevicesSettings is the devices on the settings page: there with nothing playing anywhere too, to
// name them or have one play.
export function DevicesSettings() {
  return html`<h2 class="section-title">播放裝置</h2><div class="card pad"><${DeviceList} /></div>`;
}

// DeviceList lists this tab and the account's other devices, to continue here what one plays or
// control it (done: when something was chosen).
function DeviceList({ done = () => {} }) {
  const { list, connected } = useStore(devices);
  const controlled = useStore(control, (c) => c.target);
  useTick(true);
  const me = list.find((d) => d.player === tab) || { device, device_name: deviceName, state: 'idle' };
  const rest = others(list);
  const act = (f) => () => {
    done();
    f();
  };
  const menu = (e, d) => openMenu(e, [
    { icon: 'edit', label: '改名…', onClick: () => rename(d) },
    { icon: 'undo', label: '改回瀏覽器的名稱', onClick: () => unname(d) },
  ]);
  return html`${!connected && html`<p class="hint">還沒連上伺服器的即時通道，清單可能不是最新的。</p>`}
    <ul class="devices">
      <li class="device this">
        <span class="device-icon"><${Icon} name="devices" /></span>
        <span class="grow"><span class="title">${nameOf(me)}（這個分頁）</span>
          <span class="sub">${controlled ? `正在控制「${control.get().name}」` : status(me)}</span></span>
        <${IconButton} icon="more" label="這個裝置的選項" onClick=${(e) => menu(e, me)} />
      </li>
      ${rest.map((d) => html`<li key=${d.player || 'gone-' + d.device} class=${'device' + (d.player && d.player === controlled ? ' on' : '')}>
        ${hasSong(d) && d.cover_id ? html`<${Cover} id=${d.cover_id} size=${96} className="thumb" />`
          : html`<span class="device-icon"><${Icon} name="devices" /></span>`}
        <span class="grow"><span class="title">${nameOf(d)}${d.device === me.device && !d.gone ? '（這個瀏覽器的另一個分頁）' : ''}</span>
          <span class="sub">${status(d)}</span></span>
        <span class="device-actions">
          ${d.online && hasSong(d) && html`<${IconButton} icon=${d.state === 'playing' ? 'pause' : 'play'}
            label=${`${d.state === 'playing' ? '暫停' : '播放'}「${nameOf(d)}」`} onClick=${() => send(d.player, 'toggle').catch((e) => toast(`「${nameOf(d)}」${e.message}`, 'error'))} />`}
          ${(hasSong(d) || d.gone) && html`<button class="btn tonal" onClick=${act(() => takeOver(d))}>在這裡接續</button>`}
          ${d.online && d.player && (d.player === controlled
            ? html`<button class="btn text" onClick=${act(endControl)}>結束控制</button>`
            : html`<button class="btn text" onClick=${act(() => controlDevice(d))}>控制</button>`)}
          ${d.device && html`<${IconButton} icon="more" label=${`「${nameOf(d)}」的選項`} onClick=${(e) => menu(e, d)} />`}
        </span>
      </li>`)}
    </ul>
    ${!rest.length && html`<${Empty} icon="devices">還沒有其他裝置。用同一個帳號在手機或另一台電腦登入，就會列在這裡。<//>`}
    <p class="hint">「在這裡接續」把那台裝置的佇列與播放位置接過來，在這裡開始播放後那台會暫停；「控制」讓這裡的播放列操作那台裝置，在這裡選的歌也會在那台播放。手機的瀏覽器在背景或鎖定時，可能收不到遙控。</p>`;
}

// ---- continue here ----

let taking = 0; // the latest take-over: an older one's answer arriving late does nothing

// takeOver continues here what device d plays: its queue, place, play mode and (if wanted) sleep
// timer; once this tab really plays, d pauses, only if it still plays what was taken. Not answering,
// only its last song can be continued here.
export async function takeOver(d) {
  const my = ++taking, name = nameOf(d), began = generation();
  if (!d.online || !d.player) return fallback(d, d.gone ? '已經離開' : '目前收不到要求');
  toast(`正在接手「${name}」的播放…`);
  let snap;
  try {
    snap = await send(d.player, 'snapshot');
  } catch (e) {
    if (my === taking) fallback(d, e.message);
    return;
  }
  const got = Date.now();
  if (my !== taking) return;
  if (!snap) return toast(`「${name}」沒有在播放`, 'error');
  // Something was played here meanwhile: that stays, the older snapshot does not override it.
  if (generation() !== began) return toast(`這裡已經在播別的了，沒有接手「${name}」`, 'error');
  let sleep = false;
  if (snap.sleep) {
    sleep = await askSleep(snap.sleep, name);
    if (sleep === null || my !== taking || generation() !== began) return;
  }
  endControl();
  if (!adopt(snap, { sleep, since: Date.now() - got })) return toast(`沒能接手：「${name}」的佇列是空的`, 'error');
  // The other device pauses once this one is heard: not while the browser here wants a click first.
  if (!(await heard(generation())) || my !== taking) return;
  const qids = [snap.queue[snap.index], snap.queue[snap.index + 1]].filter(Boolean).map((q) => q.qid);
  try {
    await send(d.player, 'release', { gen: snap.gen, qids });
    toast(`已在這裡接續，「${name}」已暫停`);
  } catch (e) {
    toast(e.code === 'changed' ? `已在這裡接續；「${name}」之後播了別的，沒有暫停它` : `已在這裡接續，但沒能讓「${name}」暫停：${e.message}`,
      e.code === 'changed' ? 'info' : 'error');
  }
}

// heard is whether this tab plays what gen began, before anything else is played here.
function heard(gen) {
  return new Promise((resolve) => {
    const check = (s) => {
      const done = generation() !== gen ? false : s.playing && !s.buffering ? true : null;
      if (done === null) return;
      unsubscribe();
      resolve(done);
    };
    const unsubscribe = player.subscribe(check);
    check(player.get());
  });
}

// askSleep asks whether the other device's sleep timer comes along: true, false, or null (closed).
function askSleep(sleep, name) {
  const left = sleep.endOfTrack ? '播完這首後停止' : `${Math.max(1, Math.round(sleep.leftMs / 60_000))} 分鐘後停止`;
  return new Promise((resolve) => {
    showDialog((close) => {
      const done = (v) => () => {
        close();
        resolve(v);
      };
      return html`<${Dialog} title="睡眠定時" onClose=${done(null)} actions=${html`
        <button class="btn text" onClick=${done(false)}>在這裡取消</button>
        <button class="btn filled" onClick=${done(true)}>帶過來</button>`}>
        <p>「${name}」設了睡眠定時（${left}）。要在這裡繼續這個定時嗎？</p>
      <//>`;
    });
  });
}

// fallback offers what device d played last, from where it was, with its album after it: the
// queue cannot come along.
function fallback(d, why) {
  const name = nameOf(d);
  if (!(d.asset_id > 0 && d.album_id > 0)) {
    return toast(`沒辦法接續「${name}」：它${why}，也沒有說最後播的是曲庫的哪首歌`, 'error');
  }
  const at = positionOf(d);
  confirmDialog({
    title: '只能從最後的歌接續', action: '從這首開始',
    onConfirm: async () => {
      const a = await get('/albums/' + d.album_id);
      const items = a.entries.map((e) => fromEntry(e, a));
      const i = items.findIndex((q) => q.assetId === d.asset_id);
      if (i < 0) return toast(`曲庫裡找不到《${d.title}》了`, 'error');
      items[i] = { ...items[i], resumeMs: at };
      playQueue(items, i);
    },
    children: html`<p>「${name}」${why}，它的佇列帶不過來。</p>
      <p>可以從它最後播放的《${d.title}》${fmtTime(at)} 開始，接著播同一張專輯。</p>`,
  });
}

// ---- the device controlled ----

// RemoteSeek is the controlled device's place in its song; dragging shows where, and seeks there
// once let go.
function RemoteSeek({ d, className }) {
  const [drag, setDrag] = useState(null);
  const total = (d.duration_ms || 0) / 1000;
  const t = drag ?? positionOf(d) / 1000;
  const pct = total ? Math.min(t / total, 1) * 100 : 0;
  return html`<input class=${'seek ' + className} type="range" min="0" max=${total} step="0.1" value=${t} disabled=${!d.online || !total}
    style=${{ '--p': pct + '%' }} onInput=${(e) => setDrag(parseFloat(e.target.value))}
    onChange=${(e) => { setDrag(null); seek(parseFloat(e.target.value)); }} aria-label="播放位置"
    aria-valuetext=${`${fmtTime(t * 1000)} / ${fmtTime(total * 1000)}`} />`;
}

function RemoteVolume({ d }) {
  const [drag, setDrag] = useState(null);
  const level = drag ?? (d.muted ? 0 : d.volume);
  useEffect(() => { // the device said where it is now: that shows
    setDrag(null);
  }, [d.volume, d.muted]);
  return html`<div class="volume">
    <${IconButton} icon=${level === 0 ? 'volumeOff' : 'volume'} label=${d.muted ? '取消靜音' : '靜音'} pressed=${!!d.muted} onClick=${toggleMute} />
    <input type="range" min="0" max="1" step="0.01" value=${level} style=${{ '--p': level * 100 + '%' }} aria-label="音量"
      aria-valuetext=${Math.round(level * 100) + '%'} onInput=${(e) => { const v = parseFloat(e.target.value); setDrag(v); setVolume(v); }} />
  </div>`;
}

const modeLook = { order: ['order', '順序播放'], all: ['repeat', '列表循環'], one: ['repeatOne', '單曲循環'], shuffle: ['shuffle', '隨機播放'] };

function RemoteMode({ d, size }) {
  const [icon, label] = modeLook[d.mode] || modeLook.order;
  return html`<${IconButton} icon=${icon} label=${`播放模式：${label}（按一下切換）`} size=${size}
    className=${'mode' + (d.mode && d.mode !== 'order' ? ' on' : '')} onClick=${cycleMode} />`;
}

// useTarget is the device controlled, as last listed: or, gone from the list, how it was with
// online off.
function useTarget() {
  useStore(devices);
  const { target: id, name } = useStore(control);
  const last = useRef(null);
  const d = target();
  if (d) last.current = d;
  useTick(!!d && d.state === 'playing');
  if (!id) return null;
  return d || { ...(last.current || { state: 'idle', device_name: name }), online: false };
}

// RemoteBar is the player bar while controlling another device: what it plays, as it said, and
// controls that ask it to do things.
export function RemoteBar() {
  const d = useTarget();
  const { name, busy } = useStore(control);
  if (!d) return null;
  const song = hasSong(d);
  return html`<div class="player-bar remote-bar">
    <${RemoteSeek} d=${d} className="bar-seek" />
    <button class="now" onClick=${() => control.set({ open: true })} aria-label=${`開啟「${name}」的播放`}>
      <${Cover} id=${song ? d.cover_id : 0} size=${96} className="thumb" />
      <span class="track-text"><span class="title">${song ? d.title : '沒有在播放'}</span>
        <span class="sub remote-label"><${Icon} name="devices" size=${16} />正在控制「${name}」${d.online ? '' : '（離線）'}</span></span>
    </button>
    <div class="controls">
      <span class="wide-only"><${RemoteMode} d=${d} /></span>
      <${IconButton} icon="prev" label="上一首" onClick=${prev} />
      <${IconButton} icon=${d.state === 'playing' ? 'pause' : 'play'} label=${d.state === 'playing' ? '暫停' : '播放'} onClick=${toggle} filled />
      <${IconButton} icon="next" label="下一首" onClick=${next} />
    </div>
    <div class="bar-extra">
      ${busy > 0 && html`<span class="remote-busy" title="等待回應"><${Spinner} /></span>`}
      <span class="bar-time wide-only">${fmtTime(positionOf(d))} / ${fmtTime(d.duration_ms || 0)}</span>
      <span class="wide-only"><${RemoteVolume} d=${d} /></span>
      <${DevicesButton} />
      <${IconButton} icon="close" label=${`結束控制「${name}」`} onClick=${endControl} />
    </div>
  </div>`;
}

// RemoteNowPlaying is the device controlled, full screen: its song, controls and queue.
export function RemoteNowPlaying() {
  const open = useStore(control, (c) => !!c.target && !!c.open);
  useEffect(() => {
    document.body.classList.toggle('locked', open);
  }, [open]);
  return open ? html`<${RemoteView} />` : null;
}

function RemoteView() {
  const d = useTarget();
  const { name } = useStore(control);
  const root = useRef(null);
  const close = () => control.set({ open: false });
  useLayer(root, close);
  if (!d) return null;
  const song = hasSong(d);
  return html`<div class="now-playing remote-view" role="dialog" aria-modal="true" aria-label=${`「${name}」正在播放`} ref=${root}>
    <div class="np-top">
      <${IconButton} icon="expand" label="收起" onClick=${close} />
      <span class="remote-label grow"><${Icon} name="devices" size=${18} />正在控制「${name}」${d.online ? '' : '（離線）'}</span>
      <button class="btn text" onClick=${() => { close(); endControl(); }}>結束控制</button>
    </div>
    <div class="np-main">
      <${Cover} id=${song ? d.cover_id : 0} size=${600} alt=${d.album || d.title || ''} className="np-cover" />
      <div class="np-info">
        <div class="np-title" title=${d.title}>${song ? d.title : '沒有在播放'}</div>
        ${song && html`<div class="sub">${d.artist || '未知歌手'}</div>${d.album && html`<div class="sub">${d.album}</div>`}`}
        <${RemoteSeek} d=${d} className="np-seek" />
        <div class="times"><span>${fmtTime(positionOf(d))}</span><span>${d.blocked ? '那台裝置要先點一下頁面才能播放' : ''}</span>
          <span>${fmtTime(d.duration_ms || 0)}</span></div>
        <div class="np-controls">
          <${RemoteMode} d=${d} />
          <${IconButton} icon="prev" label="上一首" onClick=${prev} size=${32} />
          <${IconButton} icon=${d.state === 'playing' ? 'pause' : 'play'} label=${d.state === 'playing' ? '暫停' : '播放'} onClick=${toggle} filled size=${40} />
          <${IconButton} icon="next" label="下一首" onClick=${next} size=${32} />
          <${DevicesButton} />
        </div>
        <${RemoteVolume} d=${d} />
      </div>
    </div>
    <div class="np-panel"><${RemoteQueue} d=${d} /></div>
  </div>`;
}

// RemoteQueue is the controlled device's queue, asked of it again whenever it changed there.
function RemoteQueue({ d }) {
  const [snap, setSnap] = useState(null);
  const [error, setError] = useState(null);
  const [n, again] = useState(0);
  useEffect(() => {
    if (!d.online || !d.player) return;
    let live = true;
    send(d.player, 'snapshot').then((s) => { if (live) { setSnap(s); setError(null); } }, (e) => live && setError(e));
    return () => { live = false; };
  }, [d.player, d.queue, d.online, n]);
  const jump = (q) => command('jump', { qid: q.qid }).catch((e) => e.code === 'stale' && again((x) => x + 1));
  if (!d.online) return html`<${Empty} icon="devices">那台裝置目前沒有連線，看不到它的佇列。<//>`;
  if (error && !snap) return html`<div class="error-box" role="alert"><span>沒能讀到佇列：${error.message}</span>
    <button class="btn text" onClick=${() => again((x) => x + 1)}>重試</button></div>`;
  if (!snap) return d.state === 'idle' ? html`<${Empty} icon="queue">那台裝置的佇列是空的。在這裡選歌播放，就會在那台播放。<//>` : html`<${Spinner} />`;
  return html`<div class="queue">
    <div class="queue-head"><span class="sub grow">「${nameOf(d)}」的佇列 · ${snap.queue.length} 首</span></div>
    <ol class="tracks queue-list">${snap.queue.map((q, i) => html`<li key=${q.qid}>
      <div class=${'track' + (q.qid === d.qid ? ' current' : '')}>
        <button class="track-main" onClick=${() => jump(q)} aria-current=${q.qid === d.qid ? 'true' : undefined}>
          <span class="num">${i + 1}</span>
          <span class="track-text"><span class="title">${q.title}</span><span class="sub">${q.artist}</span></span>
          <span class="meta">${fmtTime(q.durationMs)}</span>
        </button>
      </div>
    </li>`)}</ol>
  </div>`;
}

// Elsewhere is the player bar with nothing playing here: what another device plays, to continue
// here or control.
export function Elsewhere() {
  const list = useStore(devices, (s) => s.list);
  const d = others(list).find((x) => x.online && x.player && hasSong(x));
  if (!d) return null;
  return html`<div class="player-bar elsewhere">
    <${Cover} id=${d.cover_id} size=${96} className="thumb" />
    <span class="track-text grow"><span class="title">${d.title}</span>
      <span class="sub"><${Icon} name="devices" size=${16} />「${nameOf(d)}」${d.state === 'playing' ? '播放中' : '已暫停'}</span></span>
    <button class="btn tonal" onClick=${() => takeOver(d)}>在這裡接續</button>
    <span class="wide-only"><button class="btn text" onClick=${() => controlDevice(d)}>控制</button></span>
    <${DevicesButton} />
  </div>`;
}

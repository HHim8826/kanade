import { useEffect } from '../../vendor/hooks.module.js';
import { current, next, playAt, player, prev, seek, toggle } from '../player.js';
import { href } from '../router.js';
import { useStore } from '../store.js';
import { Cover, IconButton, fmtQuality, fmtTime, html } from '../ui.js';

const open = (v) => player.set({ nowPlayingOpen: v });

export function PlayerBar() {
  const s = useStore(player);
  const item = s.queue[s.index];
  if (!item) return null;
  const pct = s.duration ? (s.time / s.duration) * 100 : 0;
  return html`<div class="player-bar">
    <div class="bar-progress"><div style=${{ width: pct + '%' }}></div></div>
    <button class="now" onClick=${() => open(true)} aria-label="開啟正在播放">
      <${Cover} id=${item.coverId} size=${96} className="thumb" />
      <span class="track-text"><span class="title">${item.title}</span><span class="sub">${item.artist || '未知歌手'}</span></span>
    </button>
    <div class="controls">
      <${IconButton} icon="prev" label="上一首" onClick=${prev} />
      <${IconButton} icon=${s.playing ? 'pause' : 'play'} label=${s.playing ? '暫停' : '播放'} onClick=${toggle} filled />
      <${IconButton} icon="next" label="下一首" onClick=${next} />
    </div>
  </div>`;
}

export function NowPlaying() {
  const s = useStore(player);
  const item = current();
  const shown = s.nowPlayingOpen && !!item;
  useEffect(() => { // the page underneath must not scroll while this covers it
    document.body.classList.toggle('locked', shown);
  }, [shown]);
  if (!shown) return null;
  return html`<div class="now-playing" role="dialog" aria-modal="true" aria-label="正在播放">
    <div class="np-top"><${IconButton} icon="expand" label="收起" onClick=${() => open(false)} /></div>
    <div class="np-main">
      <${Cover} id=${item.coverId} size=${600} alt=${item.album || item.title} className="np-cover" />
      <div class="np-info">
        <div class="np-title">${item.title}</div>
        <div class="sub">${item.artist || '未知歌手'}</div>
        ${item.albumId ? html`<a class="sub link" href=${href('album/' + item.albumId)} onClick=${() => open(false)}>${item.album}</a>` : ''}
        <div class="quality">${fmtQuality(item.asset)}</div>
        <input class="seek" type="range" min="0" max=${s.duration || 0} step="0.1" value=${s.time}
          onInput=${(e) => seek(parseFloat(e.target.value))} aria-label="播放位置" />
        <div class="times"><span>${fmtTime(s.time * 1000)}</span><span>${s.buffering ? '緩衝中…' : ''}</span><span>${fmtTime(s.duration * 1000)}</span></div>
        <div class="np-controls">
          <${IconButton} icon="prev" label="上一首" onClick=${prev} size=${32} />
          <${IconButton} icon=${s.playing ? 'pause' : 'play'} label=${s.playing ? '暫停' : '播放'} onClick=${toggle} filled size=${40} />
          <${IconButton} icon="next" label="下一首" onClick=${next} size=${32} />
        </div>
      </div>
    </div>
    <div class="np-queue">
      <h2 class="section-title">播放佇列</h2>
      <ol class="tracks">${s.queue.map((q, i) => html`<li key=${i}>
        <button class=${'track' + (i === s.index ? ' current' : '')} onClick=${() => playAt(i)}>
          <span class="num">${i + 1}</span>
          <span class="track-text"><span class="title">${q.title}</span><span class="sub">${q.artist}</span></span>
          <span class="meta">${fmtTime(q.durationMs)}</span>
        </button></li>`)}</ol>
    </div>
  </div>`;
}

import { useEffect, useMemo, useRef, useState } from '../../vendor/hooks.module.js';
import { api, get } from '../api.js';
import { fromTrack, playQueue } from '../player.js';
import { href } from '../router.js';
import { Cover, Dialog, Empty, ErrorBox, Icon, IconButton, Spinner, html, showDialog, toast, useLoad } from '../ui.js';
import { useRunner } from './organize.js';

// My listening (review #93): how long was listened each day of the year (a heat map), today, this
// week, month and year, what was listened to most, and when. Times are what was really heard; a
// play counts once it reaches the D9 threshold. Every number a chart shows is also in its table.

const TZ_KEY = 'kanade.statsTz';
const browserTz = () => Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
function savedTz() {
  try {
    return localStorage.getItem(TZ_KEY) || '';
  } catch {
    return '';
  }
}

// fmtDur says a listening time: 2 小時 5 分, 45 分, 30 秒.
export function fmtDur(ms) {
  if (!ms) return '0 分';
  if (ms < 60_000) return `${Math.round(ms / 1000)} 秒`;
  const min = Math.round(ms / 60_000);
  const h = Math.floor(min / 60), m = min % 60;
  return h ? `${h} 小時${m ? ` ${m} 分` : ''}` : `${m} 分`;
}
const weekdays = ['一', '二', '三', '四', '五', '六', '日'];
// Dates are calendar days (YYYY-MM-DD) of the time zone the server counted in; arithmetic on them
// uses UTC so the browser's own zone never shifts them.
const dayOf = (s) => new Date(s + 'T00:00:00Z');
const ymd = (d) => d.toISOString().slice(0, 10);
const addDays = (s, n) => { const d = dayOf(s); d.setUTCDate(d.getUTCDate() + n); return ymd(d); };
const longDate = (s) => { const d = dayOf(s); return `${d.getUTCFullYear()} 年 ${d.getUTCMonth() + 1} 月 ${d.getUTCDate()} 日（週${weekdays[(d.getUTCDay() + 6) % 7]}）`; };
const shortDate = (s) => { const d = dayOf(s); return `${d.getUTCMonth() + 1}/${d.getUTCDate()}`; };
function todayIn(tz) {
  const p = Object.fromEntries(new Intl.DateTimeFormat('en-CA', { timeZone: tz, year: 'numeric', month: '2-digit', day: '2-digit' })
    .formatToParts(new Date()).map((x) => [x.type, x.value]));
  return `${p.year}-${p.month}-${p.day}`;
}

const kinds = [['', '全部'], ['music', '音樂'], ['spoken', '廣播劇／談話']];

export function Stats() {
  const [kind, setKind] = useState('');
  const [tz, setTz] = useState(() => savedTz() || browserTz());
  const q = `tz=${encodeURIComponent(tz)}${kind ? '&kind=' + kind : ''}`;
  const today = todayIn(tz);
  const [rev, setRev] = useState(0);
  return html`<section class="stats">
    <div class="page-head">
      <div><div class="overline"><a href=${href('')}>首頁</a></div><h1 class="page-title">我的聆聽</h1></div>
    </div>
    <div class="filter-row" role="group" aria-label="篩選">
      <nav class="seg" aria-label="類型">${kinds.map(([k, label]) => html`<button key=${k} class=${k === kind ? 'on' : ''}
        aria-pressed=${k === kind} onClick=${() => setKind(k)}>${label}</button>`)}</nav>
      <span class="sub">時區 ${tz}</span>
    </div>
    <${Overview} q=${q} rev=${rev} />
    <${Heatmap} q=${q} today=${today} rev=${rev} />
    <${Period} q=${q} today=${today} rev=${rev} />
    <${DataTools} q=${q} tz=${tz} setTz=${(v) => { setTz(v); try { localStorage.setItem(TZ_KEY, v === browserTz() ? '' : v); } catch { /* per device only */ } }}
      onCleared=${() => setRev(rev + 1)} />
  </section>`;
}

// Overview: today, this week, month and year.
function Overview({ q, rev }) {
  const data = useLoad(() => get('/stats/summary?' + q), [q], rev);
  if (data.error) return html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`;
  const s = data.data;
  const tiles = [['today', '今天'], ['week', '本週'], ['month', '本月'], ['year', '今年']];
  return html`<div class=${'stat-tiles' + (data.loading ? ' refreshing' : '')}>${tiles.map(([k, label]) => html`<div key=${k} class="stat-tile">
    <div class="label">${label}</div>
    <div class="value">${s ? fmtDur(s[k].ms) : '—'}</div>
    ${s && html`<div class="sub">有效播放 ${s[k].plays} 次 · ${s[k].tracks} 首歌${k !== 'today' ? ` · ${s[k].active_days} 天有聽` : ''}</div>`}
  </div>`)}</div>`;
}

// levels splits the days with listening into four steps: by quartile, so a few long days do not
// wash the rest out; with only a few days, by their share of the longest, which is always the top.
function levels(days) {
  const v = days.map((d) => d.ms).filter((x) => x > 0).sort((a, b) => a - b);
  if (v.length < 8) return (ms) => (ms <= 0 ? 0 : Math.max(1, Math.ceil((ms / v[v.length - 1]) * 4)));
  const at = (p) => v[Math.min(v.length - 1, Math.floor(p * v.length))];
  const cuts = [at(0.25), at(0.5), at(0.75)];
  return (ms) => (ms <= 0 ? 0 : ms <= cuts[0] ? 1 : ms <= cuts[1] ? 2 : ms <= cuts[2] ? 3 : 4);
}

// Heatmap shows a year of days, a week a column (Monday first); a day opens what was heard in it.
function Heatmap({ q, today, rev }) {
  const thisYear = Number(today.slice(0, 4));
  const [year, setYear] = useState(thisYear);
  const [picked, setPicked] = useState('');
  const [focus, setFocus] = useState('');
  const [tip, setTip] = useState(null);
  const grid = useRef(null);
  const data = useLoad(() => get(`/stats/days?year=${year}&${q}`), [year, q], rev);
  const byDate = useMemo(() => Object.fromEntries((data.data ? data.data.days : []).map((d) => [d.date, d])), [data.data]);
  const level = useMemo(() => levels(data.data ? data.data.days : []), [data.data]);
  const first = `${year}-01-01`, last = `${year}-12-31`;
  const start = addDays(first, -((dayOf(first).getUTCDay() + 6) % 7));
  const weeks = [];
  for (let d = start; d <= last; d = addDays(d, 7)) weeks.push(d);
  const months = weeks.map((w, i) => {
    const m = [0, 1, 2, 3, 4, 5, 6].map((k) => addDays(w, k)).find((d) => d.slice(8) === '01' && d.slice(0, 4) === String(year));
    return m && i < weeks.length - 1 ? `${Number(m.slice(5, 7))} 月` : '';
  });
  const current = focus || (today.startsWith(String(year)) ? today : first);
  useEffect(() => { // where the year does not fit (a phone), start at today's week, not January
    const el = grid.current, cell = el && el.querySelector(`[data-date="${current}"]`);
    if (cell && el.scrollWidth > el.clientWidth) el.scrollLeft = Math.max(0, cell.offsetLeft - el.clientWidth + 48);
  }, [!!data.data, year]);
  const show = (e, d) => {
    const box = grid.current.getBoundingClientRect(), r = e.currentTarget.getBoundingClientRect();
    setTip({ d, x: r.left - box.left + r.width / 2, y: r.top - box.top });
  };
  const move = (e) => {
    const step = { ArrowLeft: -7, ArrowRight: 7, ArrowUp: -1, ArrowDown: 1 }[e.key];
    if (!step) return;
    e.preventDefault();
    const next = addDays(current, step);
    if (next < first || next > last) return;
    setFocus(next);
    requestAnimationFrame(() => {
      const el = grid.current && grid.current.querySelector(`[data-date="${next}"]`);
      if (el) el.focus();
    });
  };
  const total = data.data ? data.data.days.reduce((a, d) => a + d.ms, 0) : 0;
  return html`<section class="stats-section">
    <div class="section-head">
      <h2 class="section-title grow">每日聆聽</h2>
      <${IconButton} icon="back" label="前一年" onClick=${() => { setYear(year - 1); setPicked(''); }} />
      <span class="year">${year}</span>
      <${IconButton} icon="back" label="後一年" className="flip" disabled=${year >= thisYear} onClick=${() => { setYear(year + 1); setPicked(''); }} />
    </div>
    <p class="sub">${data.data ? `${year} 年共聽了 ${fmtDur(total)}，${data.data.days.filter((d) => d.ms >= 60_000).length} 天有聽。` : ''}
      ${data.data && data.data.estimated_until ? `${data.data.estimated_until} 以前的時間是從舊的播放紀錄推算的（每次播放從最後回報往前算），跨午夜的部分可能歸錯日子。` : ''}</p>
    ${data.error && html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`}
    <div class=${'heat-wrap' + (data.loading ? ' refreshing' : '')} ref=${grid}>
      <div class="heat" role="grid" aria-label=${`${year} 年每天的聆聽時間`} onKeyDown=${move}
        style=${`grid-template-columns: auto repeat(${weeks.length}, var(--cell))`}>
        <span></span>${months.map((m, i) => html`<span key=${'m' + i} class="heat-month">${m}</span>`)}
        ${weekdays.map((w, row) => html`
          <span key=${'w' + row} class="heat-day">${row % 2 === 0 ? w : ''}</span>
          ${weeks.map((wk) => {
            const d = addDays(wk, row);
            if (d < first || d > last) return html`<span key=${d} class="heat-pad"></span>`;
            const t = byDate[d];
            const ms = t ? t.ms : 0;
            return html`<button key=${d} data-date=${d} class=${`heat-cell l${level(ms)}${d === picked ? ' picked' : ''}${d > today ? ' future' : ''}`}
              tabindex=${d === current ? 0 : -1} aria-label=${`${longDate(d)}：${fmtDur(ms)}${t ? `，有效播放 ${t.plays} 次` : ''}`}
              aria-pressed=${d === picked}
              onPointerEnter=${(e) => show(e, d)} onPointerLeave=${() => setTip(null)} onFocus=${(e) => { setFocus(d); show(e, d); }}
              onBlur=${() => setTip(null)} onClick=${() => setPicked(d === picked ? '' : d)}></button>`;
          })}`)}
      </div>
      ${tip && html`<div class="chart-tip" style=${`left:${tip.x}px; top:${tip.y}px`} role="status">
        <b>${fmtDur(byDate[tip.d] ? byDate[tip.d].ms : 0)}</b>
        <span>${longDate(tip.d)}${byDate[tip.d] ? ` · 有效播放 ${byDate[tip.d].plays} 次 · ${byDate[tip.d].tracks} 首` : ''}</span></div>`}
    </div>
    <div class="heat-legend sub" aria-hidden="true">少<span class="heat-cell l0"></span><span class="heat-cell l1"></span><span class="heat-cell l2"></span>
      <span class="heat-cell l3"></span><span class="heat-cell l4"></span>多<span class="grow"></span>點一天看當天聽了什麼</div>
    ${data.data && html`<details class="table-view"><summary>以表格檢視</summary>
      <table><thead><tr><th>日期</th><th>聆聽時間</th><th>有效播放</th><th>歌曲</th></tr></thead>
        <tbody>${data.data.days.map((d) => html`<tr key=${d.date}><td>${d.date}</td><td>${fmtDur(d.ms)}</td><td>${d.plays}</td><td>${d.tracks}</td></tr>`)}</tbody></table>
    </details>`}
    ${picked && html`<${DayDetail} date=${picked} q=${q} rev=${rev} onClose=${() => setPicked('')} />`}
  </section>`;
}

// DayDetail lists what was heard on a day.
function DayDetail({ date, q, rev, onClose }) {
  const data = useLoad(() => get(`/stats/day?date=${date}&${q}`), [date, q], rev);
  const [all, setAll] = useState(false);
  return html`<div class="day-detail card">
    <div class="section-head"><h3 class="grow">${longDate(date)}</h3><${IconButton} icon="close" label="關閉" onClick=${onClose} /></div>
    ${data.loading && !data.data && html`<${Spinner} />`}
    ${data.error && html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`}
    ${data.data && (data.data.ms > 0 || data.data.plays > 0
      ? html`<p class="sub">聽了 ${fmtDur(data.data.ms)} · 有效播放 ${data.data.plays} 次 · ${data.data.tracks} 首歌</p>
        <${RankList} items=${all ? data.data.songs : data.data.songs.slice(0, 10)} group="tracks" by="time" />
        ${!all && data.data.songs.length > 10 && html`<button class="btn text" onClick=${() => setAll(true)}>顯示全部 ${data.data.songs.length} 首</button>`}
        ${data.data.albums.length > 0 && html`<h4>專輯</h4><${RankList} items=${data.data.albums} group="albums" by="time" />`}`
      : html`<p class="sub">這天沒有聆聽紀錄。</p>`)}
  </div>`;
}

const presets = [['7', '近 7 天'], ['30', '近 30 天'], ['90', '近 90 天'], ['year', '今年'], ['custom', '自訂']];

// Period ranks and charts a stretch of days: the presets, or a range chosen (at most 400 days).
function Period({ q, today, rev }) {
  const [preset, setPreset] = useState('30');
  const [custom, setCustom] = useState({ from: addDays(today, -29), to: today });
  const range = preset === 'custom' ? custom : preset === 'year' ? { from: today.slice(0, 4) + '-01-01', to: today }
    : { from: addDays(today, 1 - Number(preset)), to: today };
  const bad = range.from > range.to || (dayOf(range.to) - dayOf(range.from)) / 86400000 >= 400;
  const rq = `${q}&from=${range.from}&to=${range.to}`;
  return html`<section class="stats-section">
    <div class="section-head"><h2 class="section-title grow">排行與趨勢</h2></div>
    <div class="filter-row" role="group" aria-label="期間">
      <nav class="seg" aria-label="期間">${presets.map(([k, label]) => html`<button key=${k} class=${k === preset ? 'on' : ''}
        aria-pressed=${k === preset} onClick=${() => setPreset(k)}>${label}</button>`)}</nav>
      ${preset === 'custom' && html`<span class="date-range">
        <input type="date" value=${custom.from} max=${today} aria-label="開始日期" onChange=${(e) => setCustom({ ...custom, from: e.target.value })} />
        <span>～</span>
        <input type="date" value=${custom.to} max=${today} aria-label="結束日期" onChange=${(e) => setCustom({ ...custom, to: e.target.value })} /></span>`}
    </div>
    ${bad ? html`<p class="hint warn-text">期間要從開始到結束，最長 400 天。</p>` : html`
      <${PeriodSummary} rq=${rq} rev=${rev} />
      <${Trends} rq=${rq} rev=${rev} />
      <${Rankings} rq=${rq} rev=${rev} />`}
  </section>`;
}

function PeriodSummary({ rq, rev }) {
  const data = useLoad(() => get('/stats/summary?' + rq), [rq], rev);
  const s = data.data && data.data.range;
  if (!s) return null;
  return html`<p class="period-line"><b>${fmtDur(s.ms)}</b><span class="sub">有效播放 ${s.plays} 次 · ${s.tracks} 首歌 · ${s.albums} 張專輯 · ${s.active_days} 天有聽</span></p>`;
}

// niceMax rounds a maximum up to a clean scale: minutes up to two hours, then hours.
function niceMax(ms) {
  const steps = [5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 240, 360, 480, 600, 720, 960, 1200, 1440].map((m) => m * 60_000);
  return steps.find((s) => s >= ms) || Math.ceil(ms / 3600000) * 3600000;
}
const axis = (ms) => (ms >= 3600000 ? `${+(ms / 3600000).toFixed(1)} 小時` : `${Math.round(ms / 60_000)} 分`);

// Columns is a column chart of times: a column per item, its own tooltip on hover and focus, a
// clean scale, a few x labels, and a table of the same numbers.
function Columns({ title, items, label = () => '', detail = () => '' }) {
  const [tip, setTip] = useState(null);
  const box = useRef(null);
  const top = niceMax(Math.max(...items.map((i) => i.ms), 1));
  const show = (e, i) => {
    const b = box.current.getBoundingClientRect(), r = e.currentTarget.getBoundingClientRect();
    setTip({ i, x: r.left - b.left + r.width / 2, y: r.top - b.top + r.height * (1 - items[i].ms / top) });
  };
  return html`<figure class="colchart">
    <figcaption>${title}</figcaption>
    <div class="col-plot" ref=${box}>
      <div class="col-grid" aria-hidden="true">${[top, top / 2, 0].map((v) => html`<span key=${v}><i>${axis(v)}</i></span>`)}</div>
      <div class="col-bars" role="list">${items.map((it, i) => html`<button key=${i} class="col" role="listitem"
        aria-label=${`${it.name}：${fmtDur(it.ms)}`} onPointerEnter=${(e) => show(e, i)} onPointerLeave=${() => setTip(null)}
        onFocus=${(e) => show(e, i)} onBlur=${() => setTip(null)}>
        <span class="bar" style=${`height:${(it.ms / top) * 100}%`}></span></button>`)}</div>
      ${tip && html`<div class="chart-tip" style=${`left:${tip.x}px; top:${tip.y}px`} role="status">
        <b>${fmtDur(items[tip.i].ms)}</b><span>${items[tip.i].name}${detail(items[tip.i])}</span></div>`}
    </div>
    <div class="col-labels" aria-hidden="true">${items.map((it, i) => html`<span key=${i}>${label(it, i)}</span>`)}</div>
    <details class="table-view"><summary>以表格檢視</summary>
      <table><tbody>${items.map((it, i) => html`<tr key=${i}><td>${it.name}</td><td>${fmtDur(it.ms)}</td>${it.plays !== undefined && html`<td>${it.plays} 次</td>`}</tr>`)}</tbody></table>
    </details>
  </figure>`;
}

function Trends({ rq, rev }) {
  const data = useLoad(() => get('/stats/trends?' + rq), [rq], rev);
  if (data.error) return html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`;
  if (!data.data) return html`<${Spinner} />`;
  const t = data.data;
  const n = t.days.length;
  const every = n <= 10 ? 1 : n <= 31 ? 7 : n <= 92 ? 14 : 30;
  const days = t.days.map((d) => ({ ...d, name: longDate(d.date) }));
  const hours = t.hours.map((ms, h) => ({ name: `${h}:00–${h + 1}:00`, ms }));
  const week = t.weekdays.map((ms, i) => ({ name: `週${weekdays[i]}`, ms }));
  return html`<div class=${'trends' + (data.loading ? ' refreshing' : '')}>
    <div class="stat-tiles small">
      <div class="stat-tile"><div class="label">連續聆聽</div><div class="value">${t.streak} 天</div><div class="sub">一天聽滿 1 分鐘算一天</div></div>
      <div class="stat-tile"><div class="label">最長連續</div><div class="value">${t.longest} 天</div>
        <div class="sub">${t.longest_to ? `到 ${t.longest_to}` : '還沒有紀錄'}</div></div>
      <div class="stat-tile"><div class="label">新開始聽的歌</div><div class="value">${t.new_count} 首</div><div class="sub">這段期間第一次聽</div></div>
    </div>
    <${Columns} title="每天的聆聽時間" items=${days} label=${(d, i) => ((n - 1 - i) % every === 0 ? shortDate(d.date) : '')}
      detail=${(d) => (d.plays ? ` · 有效播放 ${d.plays} 次` : '')} />
    <div class="chart-pair">
      <${Columns} title="常聽的時段" items=${hours} label=${(h, i) => (i % 6 === 0 ? `${i}` : '')} />
      <${Columns} title="一週中的哪幾天" items=${week} label=${(w, i) => weekdays[i]} />
    </div>
    ${t.new_songs.length > 0 && html`<h3>新開始聽的歌${t.new_count > 10 ? `（最近 10 首，共 ${t.new_count} 首）` : ''}</h3>
      <${RankList} items=${t.new_songs.slice(0, 10)} group="tracks" plain />`}
  </div>`;
}

const groups = [['tracks', '歌曲'], ['artists', '歌手'], ['albums', '專輯']];

function Rankings({ rq, rev }) {
  const [group, setGroup] = useState('tracks');
  const [by, setBy] = useState('plays');
  const data = useLoad(() => get(`/stats/top?${rq}&group=${group}&by=${by}&limit=50`), [rq, group, by], rev);
  return html`<div class="rankings">
    <div class="filter-row">
      <h3 class="grow">排行</h3>
      <nav class="seg" aria-label="排行對象">${groups.map(([k, label]) => html`<button key=${k} class=${k === group ? 'on' : ''}
        aria-pressed=${k === group} onClick=${() => setGroup(k)}>${label}</button>`)}</nav>
      <nav class="seg" aria-label="排序">${[['plays', '播放次數'], ['time', '聆聽時間']].map(([k, label]) => html`<button key=${k}
        class=${k === by ? 'on' : ''} aria-pressed=${k === by} onClick=${() => setBy(k)}>${label}</button>`)}</nav>
    </div>
    ${data.error && html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`}
    ${data.loading && !data.data && html`<${Spinner} />`}
    ${data.data && (data.data.length
      ? html`<div class=${data.loading ? 'refreshing' : ''}><${RankList} items=${data.data} group=${group} by=${by} /></div>`
      : html`<${Empty} icon="history">這段期間沒有聆聽紀錄。<//>`)}
  </div>`;
}

// RankList lists ranked songs, artists or albums with their plays and time, a thin bar against the
// first; a song plays from the list, an album or artist opens.
function RankList({ items, group, by, plain }) {
  const top = Math.max(...items.map((it) => (by === 'plays' ? it.plays : it.ms)), 1);
  const playable = items.filter((it) => it.track).map((it) => fromTrack(it.track));
  const open = (it) => {
    if (group === 'tracks' && it.track) playQueue(playable, playable.findIndex((p) => p.trackId === it.track.id));
  };
  return html`<ol class=${'rank-list' + (plain ? ' plain' : '')}>${items.map((it, i) => {
    const name = it.removed ? '（已從曲庫移除的歌）' : it.name;
    const sub = group === 'artists' ? `${it.tracks} 首歌` : group === 'albums' ? [it.artist, `${it.tracks} 首歌`].filter(Boolean).join(' · ') : it.artist;
    const inner = html`
      ${!plain && html`<span class="rank">${i + 1}</span>`}
      ${group !== 'artists' && html`<${Cover} id=${it.cover_id} size=${300} className="thumb" alt="" />`}
      <span class="rank-text"><span class="title">${name}</span><span class="sub">${sub}</span>
        ${!plain && html`<span class="rank-bar" aria-hidden="true"><span style=${`width:${((by === 'plays' ? it.plays : it.ms) / top) * 100}%`}></span></span>`}</span>
      ${!plain && html`<span class="rank-value"><b>${by === 'plays' ? `${it.plays} 次` : fmtDur(it.ms)}</b>
        <span class="sub">${by === 'plays' ? fmtDur(it.ms) : `${it.plays} 次`}</span></span>`}`;
    return html`<li key=${(it.id || it.name) + ':' + i}>${group === 'albums'
      ? html`<a class="row" href=${href('album/' + it.id)}>${inner}</a>`
      : group === 'artists'
        ? html`<a class="row" href=${href(`artist/0?name=${encodeURIComponent(it.name)}`)}>${inner}</a>`
        : html`<button class="row" disabled=${!it.track} onClick=${() => open(it)} aria-label=${it.track ? `播放「${name}」` : name}>${inner}</button>`}</li>`;
  })}</ol>`;
}

// DataTools: the time zone days are counted in, exporting and clearing the listening.
function DataTools({ q, tz, setTz, onCleared }) {
  const zones = useMemo(() => (Intl.supportedValuesOf ? Intl.supportedValuesOf('timeZone') : [browserTz()]), []);
  const clear = () => showDialog((close) => html`<${ClearStats} close=${close} onDone=${onCleared} />`);
  return html`<section class="stats-section data-tools">
    <h2 class="section-title">資料</h2>
    <label class="field">統計時區（只影響這台裝置怎麼分日）
      <select value=${tz} onChange=${(e) => setTz(e.target.value)}>
        ${!zones.includes(tz) && html`<option value=${tz}>${tz}</option>`}
        ${zones.map((z) => html`<option key=${z} value=${z}>${z}${z === browserTz() ? '（這台裝置）' : ''}</option>`)}
      </select></label>
    <p class="hint">聆聽時間只算真正播放的部分：暫停、緩衝和拖動進度都不算；同一次播放重送的回報不會重複計算。播放滿一半或 4 分鐘（較短者）算一次有效播放，30 秒以下的歌不算次數，但聆聽時間照算。</p>
    <div class="actions">
      <a class="btn tonal" href=${'/api/v1/stats/export?format=csv&' + q} download><${Icon} name="download" />匯出 CSV</a>
      <a class="btn tonal" href=${'/api/v1/stats/export?format=json&' + q} download><${Icon} name="download" />匯出 JSON</a>
      <button class="btn text danger-text" onClick=${clear}><${Icon} name="delete" />清除聆聽統計…</button>
    </div>
  </section>`;
}

function ClearStats({ close, onDone }) {
  const [busy, run] = useRunner();
  const submit = () => run(async () => {
    await api('DELETE', '/stats');
    toast('已清除聆聽統計');
    close();
    onDone();
  });
  return html`<${Dialog} title="清除聆聽統計" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled danger" disabled=${busy} onClick=${submit}>清除</button>`}>
    <p>這會刪除所有的每日聆聽時間、播放次數與排行資料，無法復原；建議先匯出。曲庫、歌單、收藏和「繼續播放」的位置不受影響。</p>
  <//>`;
}

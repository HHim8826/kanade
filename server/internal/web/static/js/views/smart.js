import { useEffect, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { playQueue, playSmart } from '../player.js';
import { go } from '../router.js';
import { Dialog, ErrorBox, Icon, IconButton, Spinner, fmtTime, html, showDialog, toast, useLoad } from '../ui.js';
import { Field, changed, useRunner } from './organize.js';

// Smart playlists (review #96): rules that pick songs from the library whenever the playlist is
// opened or played — categories, albums, artists, kind, favorites, plays and when — in an order and
// within limits. Played as they are now, or on and on: every pick asks the rules again.

export const fields = [
  ['category', '分類'], ['album', '專輯'], ['artist', '歌手'], ['kind', '類型'], ['favorite', '收藏'],
  ['plays', '播放次數'], ['played_within', '最近播放'], ['never_played', '從未播放'], ['finished', '播完過'],
];
const ops = {
  category: [['is', '在其中之一'], ['not', '不在']], album: [['is', '是其中之一'], ['not', '不是']],
  artist: [['contains', '包含'], ['not', '不包含']], kind: [['is', '是']], favorite: [['is', '已收藏'], ['not', '沒收藏']],
  plays: [['gte', '至少'], ['lte', '至多']], played_within: [['is', '最近幾天聽過'], ['not', '最近幾天沒聽過']],
  never_played: [['is', '從未播放'], ['not', '播放過']], finished: [['is', '播完過'], ['not', '從沒播完']],
};
export const sorts = [['random', '隨機'], ['least_recent', '最久沒聽的先'], ['recent_added', '最近入庫的先'], ['most_played', '播放最多的先'],
  ['album', '依專輯曲序']];
const blank = (field) => ({ field, op: ops[field][0][0], ids: [], value: field === 'kind' ? 'music' : '', n: field === 'plays' ? 3 : 30 });

// describe says what rules pick, in a line.
export function describe(r, names = {}) {
  const say = (c) => {
    const op = Object.fromEntries(ops[c.field] || [])[c.op] || c.op;
    switch (c.field) {
      case 'category': case 'album': {
        const list = (c.ids || []).map((id) => names[c.field + id] || `#${id}`).join('、');
        return `${c.field === 'category' ? '分類' : '專輯'}${c.op === 'not' ? '不在' : '在'}「${list}」`;
      }
      case 'artist': return `歌手${op}「${c.value}」`;
      case 'kind': return c.value === 'spoken' ? '廣播劇／談話' : '音樂';
      case 'plays': return `播放${op} ${c.n} 次`;
      case 'played_within': return c.op === 'not' ? `${c.n} 天內沒聽過` : `${c.n} 天內聽過`;
      default: return op;
    }
  };
  const conds = (r.conditions || []).map(say);
  const head = conds.length ? conds.join(r.match === 'any' ? '，或' : '，且') : '曲庫所有歌曲';
  const sort = Object.fromEntries(sorts)[r.sort || 'random'] + (r.sort === 'most_played' && r.sort_days ? `（最近 ${r.sort_days} 天）` : '');
  const lim = [r.limit && `最多 ${r.limit} 首`, r.minutes && `最長 ${r.minutes} 分鐘`].filter(Boolean).join('、');
  return `${head}；${sort}${lim ? '；' + lim : ''}`;
}

// SmartEditor makes a smart playlist, or changes one (playlist), with a preview of what it picks.
export function SmartEditor({ playlist, close }) {
  const [name, setName] = useState(playlist ? playlist.name : '');
  const [r, setR] = useState(() => playlist ? JSON.parse(JSON.stringify(playlist.rules)) : { match: 'all', conditions: [blank('category')], sort: 'random' });
  const [busy, run] = useRunner();
  const cats = useLoad(() => get('/categories'), []);
  const rules = { ...r, conditions: r.conditions.map(({ field, op, ids, value, n }) => ({ field, op, ids: ids || [], value, n: Number(n) || 0 })) };
  const key = JSON.stringify(rules);
  const [preview, setPreview] = useState({ key: null });
  const incomplete = r.conditions.some((c) => ((c.field === 'category' || c.field === 'album') && !c.ids.length) || (c.field === 'artist' && !c.value.trim()));
  useEffect(() => { // only the preview of the rules shown now lands, not a late one (review #107)
    if (incomplete) return;
    let alive = true;
    const t = setTimeout(() => post('/playlists/preview', { rules })
      .then((p) => alive && setPreview({ key, p }), (error) => alive && setPreview({ key, error })), 300);
    return () => { alive = false; clearTimeout(t); };
  }, [key, incomplete]);
  const set = (i, patch) => setR({ ...r, conditions: r.conditions.map((c, j) => (j === i ? { ...c, ...patch } : c)) });
  const submit = () => run(async () => {
    let id = playlist && playlist.id;
    if (id) {
      await api('PUT', `/playlists/${id}/rules`, { rules });
      if (name.trim() !== playlist.name) await api('PATCH', '/playlists/' + id, { name, description: playlist.description || '' });
    } else {
      id = (await post('/playlists', { name, rules })).id;
    }
    toast(playlist ? '已更新智慧歌單' : `已建立智慧歌單「${name.trim()}」`);
    changed();
    close();
    go('playlist/' + id);
  });
  const p = preview.key === key ? preview.p : null;
  return html`<${Dialog} title=${playlist ? '編輯智慧歌單' : '新增智慧歌單'} wide onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !name.trim() || incomplete || (preview.key === key && preview.error)} onClick=${submit}>儲存</button>`}>
    <${Field} label="名稱" value=${name} onInput=${setName} placeholder="例如：ARIA 裡 30 天沒聽的歌" autofocus />
    <div class="rule-match">
      <span>符合</span>
      <nav class="seg">${[['all', '全部條件'], ['any', '任一條件']].map(([k, label]) => html`<button key=${k} class=${r.match === k ? 'on' : ''}
        aria-pressed=${r.match === k} onClick=${() => setR({ ...r, match: k })}>${label}</button>`)}</nav>
    </div>
    <ul class="rules">${r.conditions.map((c, i) => html`<li key=${i} class="rule">
      <select value=${c.field} aria-label="條件" onChange=${(e) => set(i, blank(e.target.value))}>
        ${fields.map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}</select>
      ${ops[c.field].length > 1 && html`<select value=${c.op} aria-label="比較" onChange=${(e) => set(i, { op: e.target.value })}>
        ${ops[c.field].map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}</select>`}
      ${c.field === 'category' && html`<span class="rule-picks">${cats.error ? html`<${ErrorBox} error=${cats.error} onRetry=${cats.reload} />` : cats.data ? cats.data.categories.length
        ? cats.data.categories.map((x) => html`<label key=${x.id} class="chip-check"><input type="checkbox" checked=${c.ids.includes(x.id)}
            onChange=${(e) => set(i, { ids: e.target.checked ? [...c.ids, x.id] : c.ids.filter((y) => y !== x.id) })} />${x.name}</label>`)
        : html`<span class="sub">還沒有分類，先到曲庫的「分類」建立。</span>` : html`<${Spinner} />`}</span>`}
      ${c.field === 'album' && html`<${AlbumPicker} ids=${c.ids} names=${c.names || {}} onChange=${(ids, names) => set(i, { ids, names })} />`}
      ${c.field === 'artist' && html`<input value=${c.value} placeholder="歌手名稱" aria-label="歌手" onInput=${(e) => set(i, { value: e.target.value })} />`}
      ${c.field === 'kind' && html`<select value=${c.value} aria-label="類型" onChange=${(e) => set(i, { value: e.target.value })}>
        <option value="music">音樂</option><option value="spoken">廣播劇／談話</option></select>`}
      ${(c.field === 'plays' || c.field === 'played_within') && html`<span class="rule-n"><input type="number" min="0" max="3650" value=${c.n}
        aria-label=${c.field === 'plays' ? '次數' : '天數'} onInput=${(e) => set(i, { n: e.target.value })} />${c.field === 'plays' ? '次' : '天'}</span>`}
      <${IconButton} icon="close" label="移除條件" onClick=${() => setR({ ...r, conditions: r.conditions.filter((_, j) => j !== i) })} />
    </li>`)}</ul>
    ${r.conditions.length < 20 && html`<button class="btn text" onClick=${() => setR({ ...r, conditions: [...r.conditions, blank('played_within')] })}>
      <${Icon} name="add" />新增條件</button>`}
    <div class="form-grid">
      <label class="field">排序<select value=${r.sort} onChange=${(e) => setR({ ...r, sort: e.target.value })}>
        ${sorts.map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}</select></label>
      ${r.sort === 'most_played' && html`<label class="field">播放次數的期間<select value=${r.sort_days || 0} onChange=${(e) => setR({ ...r, sort_days: Number(e.target.value) })}>
        ${[[0, '全部'], [7, '最近 7 天'], [30, '最近 30 天'], [90, '最近 90 天'], [365, '最近一年']].map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}</select></label>`}
      <label class="field">最多幾首（空白不限）<input type="number" min="0" max="1000" value=${r.limit || ''} onInput=${(e) => setR({ ...r, limit: Number(e.target.value) || 0 })} /></label>
      <label class="field">最長幾分鐘（空白不限）<input type="number" min="0" max="100000" value=${r.minutes || ''} onInput=${(e) => setR({ ...r, minutes: Number(e.target.value) || 0 })} /></label>
    </div>
    <div class="smart-preview">
      ${incomplete ? html`<p class="sub">選好每個條件的分類、專輯或歌手後會顯示結果。</p>`
        : preview.key === key && preview.error ? html`<${ErrorBox} error=${preview.error} />`
          : !p ? html`<${Spinner} />`
            : html`<p><b>${p.count} 首</b>，${fmtTime(p.duration_ms)}${p.matches > p.count ? `（符合條件的有 ${p.matches} 首，依上限取前面的）` : ''}。
              ${r.sort === 'random' ? '隨機排序每次開啟都不同。' : ''}</p>
              ${p.count === 0 ? html`<p class="sub">目前沒有符合的歌。</p>` : html`<ol class="plain-list preview-list">${p.tracks.slice(0, 8).map((t) => html`<li key=${t.id}>
                ${t.title}<span class="sub"> · ${t.artist}</span></li>`)}${p.count > 8 && html`<li class="sub">…</li>`}</ol>`}`}
    </div>
  <//>`;
}

// AlbumPicker finds albums of the library by name and keeps the ones chosen.
function AlbumPicker({ ids, names, onChange }) {
  const [q, setQ] = useState('');
  const [term, setTerm] = useState('');
  useEffect(() => {
    const t = setTimeout(() => setTerm(q.trim()), 250);
    return () => clearTimeout(t);
  }, [q]);
  const found = useLoad(() => (term ? get('/albums?limit=8&q=' + encodeURIComponent(term)) : Promise.resolve([])), [term]);
  return html`<span class="rule-picks">
    ${ids.map((id) => html`<button key=${id} class="chip-check on" onClick=${() => onChange(ids.filter((x) => x !== id), names)}
      aria-label=${`移除「${names[id] || id}」`}>${names[id] || `#${id}`} ×</button>`)}
    <span class="album-pick"><input value=${q} placeholder="搜尋專輯" aria-label="搜尋專輯" onInput=${(e) => setQ(e.target.value)} />
      ${term && found.data && html`<span class="pick-list">${found.data.filter((a) => !ids.includes(a.id)).map((a) => html`<button key=${a.id}
        onClick=${() => { onChange([...ids, a.id], { ...names, [a.id]: a.title }); setQ(''); }}>${a.title}<span class="sub"> · ${a.album_artist}</span></button>`)}</span>`}</span>
  </span>`;
}

// SmartActions are a smart playlist's play buttons: what it picks now, or on and on by its rules.
export function SmartActions({ p, items }) {
  return html`
    <button class="btn filled" disabled=${!items.length} onClick=${() => playQueue(items, 0)}><${Icon} name="play" />播放目前結果</button>
    <button class="btn tonal" onClick=${() => playSmart(p.id, p.name)} title="每次接歌都重新依條件挑選，不重複最近播過的歌"><${Icon} name="shuffle" />依條件一直播</button>`;
}

export const editSmart = (p) => showDialog((close) => html`<${SmartEditor} playlist=${p} close=${close} />`);
export const newSmart = () => showDialog((close) => html`<${SmartEditor} close=${close} />`);

// saveAsPlain keeps what a smart playlist picks now as an ordinary playlist.
export async function saveAsPlain(p, items) {
  try {
    const res = await post('/playlists', { name: `${p.name}（${new Date().toLocaleDateString('zh-TW')}）`,
      items: items.map((it) => ({ track_id: it.trackId, album_id: it.albumId || 0 })) });
    toast('已另存為一般歌單');
    changed();
    go('playlist/' + res.id);
  } catch (e) {
    toast(e.message, 'error');
  }
}

// useRuleNames names the categories and albums rules point at, for describe.
export function useRuleNames(rules) {
  const albums = (rules && rules.conditions || []).filter((c) => c.field === 'album').flatMap((c) => c.ids || []);
  const data = useLoad(async () => {
    const names = {};
    if ((rules && rules.conditions || []).some((c) => c.field === 'category')) {
      for (const c of (await get('/categories')).categories) names['category' + c.id] = c.name;
    }
    for (const id of albums.slice(0, 20)) {
      try { names['album' + id] = (await get('/albums/' + id)).title; } catch { /* gone: shown by number */ }
    }
    return names;
  }, [JSON.stringify(rules)]);
  return data.data || {};
}

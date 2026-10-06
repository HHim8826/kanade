import { useEffect, useRef, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { fromTrack, playQueue } from '../player.js';
import { href, keepInAddress, parseHash } from '../router.js';
import { Dialog, Empty, ErrorBox, Icon, IconButton, Spinner, html, openMenu, showDialog, toast, useLoad } from '../ui.js';
import { AlbumGrid, TrackList } from './common.js';
import { done, useLibRev, useRunner } from './organize.js';

// Works (review #94): an anime, a game, a book… as Bangumi describes it. Albums are linked to works
// by hand, after looking at the candidates (never by a likeness of names); a song can say what it is
// to a work; a work's page gathers the library's albums and songs of it. What Bangumi said is kept,
// so pages show without it; linking changes nothing of the album, and undo takes it back.

export const typeNames = { 1: '書籍', 2: '動畫', 3: '音樂', 4: '遊戲', 6: '三次元' };
const pickTypes = [[2, '動畫'], [4, '遊戲'], [1, '書籍'], [6, '三次元'], [3, '音樂']];
const collectNames = { 1: '想聽', 2: '聽過', 3: '在聽', 4: '擱置', 5: '拋棄' };
export const useNames = { op: '片頭曲', ed: '片尾曲', insert: '插曲', theme: '主題曲', character: '角色歌', bgm: '配樂', other: '其他' };
// Bangumi's names of platforms that only repeat the type.
const sameAsType = new Set(['游戏', '书籍', '音乐', '三次元']);

const unavailable = (e) => (e && e.status === 503 ? 'Bangumi 暫時無法連線，請稍後再試。已關聯的作品照常顯示。' : e && e.message);

// workLine says what a work is: its type, platform and date.
export const workLine = (w) => [typeNames[w.type], w.platform && !sameAsType.has(w.platform) ? w.platform : '', w.date].filter(Boolean).join(' · ');

// WorkImage is a work's picture (id: a kept work's; sid: a Bangumi subject's), or a placeholder.
export function WorkImage({ id, sid, size = 300, alt = '', className = '' }) {
  const [failed, setFailed] = useState(null);
  const src = id ? `/api/v1/works/${id}/image?size=${size}` : sid ? `/api/v1/bangumi/subjects/${sid}/image?size=${size}` : null;
  if (!src || failed === src) {
    return html`<div class=${'cover placeholder work-pic ' + className} role="img" aria-label=${alt}><${Icon} name="work" size=${32} /></div>`;
  }
  return html`<img key=${src} class=${'cover work-pic ' + className} src=${src} alt=${alt} loading="lazy" onError=${() => setFailed(src)} />`;
}

// WorkGrid shows works as cards.
export function WorkGrid({ works }) {
  return html`<div class="grid works-grid">${works.map((w) => html`<a key=${w.id} class="card album-card work-card" href=${href('work/' + w.id)}>
    <${WorkImage} id=${w.image ? w.id : 0} alt="" />
    <div class="card-text">
      <div class="title" title=${w.name}>${w.name}</div>
      <div class="sub">${[w.name_cn, workLine(w)].filter(Boolean).join(' · ')}</div>
      <div class="sub">${[w.albums && `${w.albums} 張專輯`, w.tracks && `${w.tracks} 首歌`].filter(Boolean).join(' · ')}</div>
    </div>
  </a>`)}</div>`;
}

// WorksTab is the library's works: those an album or a song is linked to. The type and the order
// are in the address (#/library/works?type=2&sort=date), so back from a work they are as they were
// (review #187).
export function WorksTab({ data }) {
  const [type, setType] = useState(() => Number(parseHash().query.get('type')) || 0);
  const [byDate, setByDate] = useState(() => parseHash().query.get('sort') === 'date');
  useEffect(() => {
    const q = [type && `type=${type}`, byDate && 'sort=date'].filter(Boolean).join('&');
    keepInAddress('library/works' + (q ? '?' + q : ''));
  }, [type, byDate]);
  if (!data.length) {
    return html`<${Empty} icon="work">還沒有關聯作品。在專輯頁的「關聯作品」從 Bangumi 找出它所屬的動畫、遊戲或書籍，就會出現在這裡。<//>`;
  }
  const types = Object.keys(typeNames).map(Number).filter((t) => data.some((w) => w.type === t));
  const shownType = types.includes(type) ? type : 0; // one the address names that no work is any more: all
  let list = shownType ? data.filter((w) => w.type === shownType) : data;
  if (byDate) list = [...list].sort((a, b) => (b.date || '').localeCompare(a.date || '') || a.name.localeCompare(b.name));
  return html`<div class="list-tools">
      ${types.length > 1 && html`<nav class="seg" aria-label="類型">${[[0, '全部'], ...types.map((t) => [t, typeNames[t]])].map(([k, label]) => html`<button key=${k}
        class=${k === shownType ? 'on' : ''} aria-pressed=${k === shownType} onClick=${() => setType(k)}>${label}</button>`)}</nav>`}
      <nav class="seg" aria-label="排序">${[[false, '名稱'], [true, '年份']].map(([k, label]) => html`<button key=${label}
        class=${k === byDate ? 'on' : ''} aria-pressed=${k === byDate} onClick=${() => setByDate(k)}>${label}</button>`)}</nav>
    </div>
    <${WorkGrid} works=${list} />`;
}

// guess is what to search Bangumi for an album: the name in 「」 or 『』, else the title without what
// says it is a soundtrack, a volume or an edition. Only a start: the user looks at what it finds.
export function guess(title) {
  const quoted = /[「『]([^」』]+)[」』]/.exec(title || '');
  if (quoted) return quoted[1].trim();
  return (title || '')
    .replace(/[[(（【][^\])）】]*[\])）】]/g, ' ')
    .replace(/(original\s*)?(motion\s*picture\s*)?sound\s*track|\bo\.?s\.?t\b\.?|オリジナル・?サウンドトラック|サウンドトラック|キャラクター・?ソング|character\s*songs?|ドラマ\s*cd|\bvol\.?\s*\d+|\bdisc\s*\d+/gi, ' ')
    .replace(/\s+/g, ' ').trim() || (title || '').trim();
}

export const linkWorks = (album, replace) => showDialog((close) => html`<${LinkWorks} albumId=${album.id} title=${album.title} replaceFirst=${replace} close=${close} />`);

// linkAlbumsWorks links albums chosen together (the albums of a series) to one work at once.
export const linkAlbumsWorks = (ids, albums) => showDialog((close) => html`<${LinkWorks} albumIds=${ids} titles=${albums.map((a) => a.title)} close=${close} />`);

// sharedGuess is what to search for albums chosen together: the words their guesses begin with,
// else the first one's.
function sharedGuess(titles) {
  const words = titles.map((t) => guess(t).split(' '));
  let n = 0;
  while (words.every((w) => n < w.length && w[n] === words[0][n])) n++;
  return words[0].slice(0, n).join(' ') || guess(titles[0]);
}

// bindSubject finds the album's own entry in Bangumi (a music subject), to manage its collection.
export const bindSubject = (album) => showDialog((close) => html`<${LinkWorks} albumId=${album.id} title=${album.title} subject close=${close} />`);

// plainTitle is an album's title without what is in brackets ([FLAC], (Disc 1)…).
const plainTitle = (t) => (t || '').replace(/[[(【][^\])】]*[\])】]/g, ' ').replace(/\s+/g, ' ').trim() || (t || '').trim();

// LinkWorks finds a work in Bangumi for an album and links it, or corrects or removes a link; with
// subject, it finds the album's own entry (a music subject) instead, one per album. With albumIds
// (and titles, those loaded of them), it links albums chosen together to one work.
function LinkWorks({ albumId, title, replaceFirst, subject, albumIds, titles, close }) {
  const rev = useLibRev();
  const album = useLoad(() => (albumId ? get('/albums/' + albumId) : Promise.resolve(null)), [albumId], rev);
  const works = album.data ? (subject ? (album.data.subject ? [album.data.subject] : []) : album.data.works) : [];
  const [linkedHere, setLinkedHere] = useState(() => new Set()); // albums chosen together: the subjects linked to them
  const [replacing, setReplacing] = useState(replaceFirst || null);
  const [q, setQ] = useState(() => (subject ? plainTitle(title) : albumIds ? sharedGuess(titles.length ? titles : ['']) : guess(title)));
  const [types, setTypes] = useState(new Set(subject ? [3] : [2, 4, 1, 6]));
  const [found, setFound] = useState(null); // { q, types, total, subjects, page_size }
  const [error, setError] = useState(null);
  const [loading, setLoading] = useState(false);
  const [open, setOpen] = useState(null); // the candidate shown in full
  const [busy, run] = useRunner();
  const input = useRef(null);
  // Binding the album's own entry can collect it in Bangumi too, as asked (kept in this browser).
  const [account, setAccount] = useState(null);
  const [collect, setCollect] = useState(() => {
    try {
      const v = JSON.parse(localStorage.getItem('kanade.bgm.collect') || 'null');
      if (v && typeof v.on === 'boolean' && v.type >= 1 && v.type <= 5) return v;
    } catch { /* storage blocked */ }
    return { on: true, type: 2 };
  });
  const keepCollect = (v) => {
    setCollect(v);
    try { localStorage.setItem('kanade.bgm.collect', JSON.stringify(v)); } catch { /* this dialog only */ }
  };
  useEffect(() => { if (subject) get('/bangumi/account').then(setAccount, () => setAccount({})); }, []);
  const linkedBgm = account && account.link && !account.link.error;
  const search = async (more) => {
    const words = more ? found.q : q.trim();
    const kinds = more ? found.types : [...types];
    if (!words) return;
    setLoading(true);
    setError(null);
    try {
      const r = await get(`/bangumi/search?q=${encodeURIComponent(words)}&types=${kinds.join(',')}${albumId ? '&album=' + albumId : ''}&offset=${more ? found.subjects.length : 0}`);
      setFound(more ? { ...found, subjects: [...found.subjects, ...r.subjects], total: r.total } : { ...r, q: words, types: kinds });
      if (!more) setOpen(r.subjects.length === 1 ? r.subjects[0].source_id : null);
    } catch (e) {
      setError(e);
    }
    setLoading(false);
  };
  useEffect(() => { search(false); }, []);
  const linked = (c) => works.some((w) => w.source_id === c.source_id) || linkedHere.has(c.source_id);
  const link = (c) => run(async () => {
    if (albumIds) {
      const res = await post('/albums/works', { albums: albumIds, source_id: c.source_id });
      done(res, `已將 ${albumIds.length} 張專輯關聯到「${c.name}」`);
      setLinkedHere((s) => new Set(s).add(c.source_id));
      return;
    }
    if (subject) {
      if (c.type !== 3) throw new Error('這個條目不是音樂：動畫、遊戲請用「關聯作品」。');
      const res = await api('PUT', `/albums/${albumId}/subject`, { source_id: c.source_id, collect: linkedBgm && collect.on ? collect.type : 0 });
      const said = {
        added: `，並加入收藏：${collectNames[res.status]}`, kept: `；Bangumi 上已收藏（${collectNames[res.status]}），沒有變更`,
        not_linked: '；Bangumi 帳號連結失效，沒有加入收藏', failed: '；加入 Bangumi 收藏失敗，可稍後在收藏裡再試',
      }[res.collected] || '';
      done(res, `已綁定 Bangumi 條目「${c.name}」${said}`);
      return;
    }
    const res = await post(`/albums/${albumId}/works`, { source_id: c.source_id, replace: replacing ? replacing.id : 0 });
    if (done(res, replacing ? `已將「${replacing.name}」改為「${c.name}」` : `已關聯「${c.name}」`)) setReplacing(null);
  });
  const unlink = (w) => run(async () => {
    if (subject) {
      done(await api('DELETE', `/albums/${albumId}/subject`), `已解除 Bangumi 條目「${w.name}」`);
      return;
    }
    const res = await api('DELETE', `/albums/${albumId}/works/${w.id}`);
    done(res, `已解除與「${w.name}」的關聯`);
    if (replacing && replacing.id === w.id) setReplacing(null);
  });
  const toggleType = (t) => setTypes((s) => {
    const n = new Set(s);
    n.has(t) ? n.delete(t) : n.add(t);
    return n;
  });
  const more = found && found.subjects.length < found.total && !found.subjects.every((c) => c.by_id);
  return html`<${Dialog} title=${subject ? '綁定 Bangumi 音樂條目' : '關聯 Bangumi 作品'} wide onClose=${close} actions=${html`<button class="btn filled" onClick=${close}>完成</button>`}>
    <p class="hint tight">${subject
      ? `找出「${title}」在 Bangumi 上的音樂條目（專輯、單曲本身那一頁），綁定後可以在這裡管理你的收藏、評分與標籤。只會綁定你確認的條目，可在修改紀錄撤回。`
      : albumIds ? `為選取的 ${albumIds.length} 張專輯選擇共同所屬的作品，一次關聯。已經關聯這部作品的專輯不變；專輯本身的資料不會改變，可在修改紀錄一次撤回。`
      : `為「${title}」選擇所屬的作品。只會關聯你確認的條目；專輯本身的資料不會改變，可在修改紀錄撤回。`}</p>
    ${works.length > 0 && html`<h3 class="sub-title">${subject ? '已綁定' : '已關聯'}</h3>
      <ul class="items linked-works">${works.map((w) => html`<li key=${w.id} class=${replacing && replacing.id === w.id ? 'current' : ''}>
        <${WorkImage} id=${w.image ? w.id : 0} size=${96} className="thumb" />
        <span class="grow track-text"><a class="title" href=${href('work/' + w.id)} onClick=${close}>${w.name}</a>
          <span class="sub">${[w.name_cn, workLine(w)].filter(Boolean).join(' · ')}</span></span>
        ${!subject && html`<button class="btn text" disabled=${busy} onClick=${() => { setReplacing(w); input.current && input.current.focus(); }}>改綁…</button>`}
        <button class="btn text" disabled=${busy} onClick=${() => unlink(w)}>解除</button>
      </li>`)}</ul>`}
    ${replacing && html`<div class="card pad replacing">
      <span class="grow">正在改綁「${replacing.name}」：從下面選出正確的作品取代它，專輯歌曲標註的用途會跟著移過去。</span>
      <button class="btn text" onClick=${() => setReplacing(null)}>取消改綁</button></div>`}
    <form class="work-search" onSubmit=${(e) => { e.preventDefault(); search(false); }}>
      <label class="search-field"><${Icon} name="search" />
        <input ref=${input} type="search" value=${q} onInput=${(e) => setQ(e.target.value)} placeholder="作品名稱，或貼上 Bangumi 條目連結／編號" aria-label="搜尋 Bangumi" />
      </label>
      <button class="btn tonal" type="submit" disabled=${loading || !q.trim()}>搜尋</button>
    </form>
    ${subject && account && (linkedBgm ? html`<div class="collect-on-bind">
        <label class="check-row"><input type="checkbox" checked=${collect.on} onChange=${(e) => keepCollect({ ...collect, on: e.target.checked })} />綁定時加入我的 Bangumi 收藏</label>
        <select value=${collect.type} disabled=${!collect.on} onChange=${(e) => keepCollect({ ...collect, type: Number(e.target.value) })} aria-label="加入收藏的狀態">
          ${Object.entries(collectNames).map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}
        </select>
        <span class="hint tight">已經收藏過的條目不會改動。</span>
      </div>` : html`<p class="hint tight">在設定連結 Bangumi 帳號後，綁定時可以一併加入收藏。</p>`)}
    <div class="chips" role="group" aria-label="類型">${pickTypes.map(([t, label]) => html`<label key=${t} class="chip-check">
      <input type="checkbox" checked=${types.has(t)} onChange=${() => toggleType(t)} />${label}</label>`)}</div>
    ${error && html`<${ErrorBox} error=${{ message: unavailable(error) }} onRetry=${() => search(false)} />`}
    ${loading && !found && html`<${Spinner} />`}
    ${found && !found.subjects.length && html`<p class="hint">Bangumi 找不到「${found.q}」。試試作品的原名或簡體中文名，或貼上條目連結。</p>`}
    ${found && found.subjects.length > 0 && html`<ul class="candidates">${found.subjects.map((c) => {
      const on = linked(c);
      const shown = open === c.source_id;
      return html`<li key=${c.source_id}>
        <button class="candidate" aria-expanded=${shown} onClick=${() => setOpen(shown ? null : c.source_id)}>
          <${WorkImage} sid=${c.image ? c.source_id : 0} size=${96} className="thumb" />
          <span class="grow track-text">
            <span class="title">${c.name}</span>
            ${c.name_cn && html`<span class="sub">${c.name_cn}</span>`}
            <span class="sub">${[workLine(c), c.score && `★ ${c.score.toFixed(1)}（${c.votes} 人評分）`].filter(Boolean).join(' · ')}</span>
          </span>
          ${c.by_id && html`<span class="pill">依編號</span>`}
          ${on && html`<span class="pill good">${subject ? '已綁定' : '已關聯'}</span>`}
          <span class=${'chev' + (shown ? ' open' : '')}><${Icon} name="expand" /></span>
        </button>
        ${shown && html`<div class="candidate-detail">
          ${c.summary ? html`<p class="work-summary">${c.summary}</p>` : html`<p class="hint tight">Bangumi 沒有這部作品的簡介。</p>`}
          <div class="actions">
            <a class="btn text" href=${c.url} target="_blank" rel="noopener noreferrer"><${Icon} name="openNew" />在 Bangumi 查看</a>
            <button class="btn filled" disabled=${busy || on} onClick=${() => link(c)}>${subject
              ? (on ? '已綁定' : works.length ? '改為這個條目' : '綁定這個條目')
              : (on ? '已關聯' : replacing ? '改為這部' : albumIds ? `關聯這 ${albumIds.length} 張` : '關聯這部作品')}</button>
          </div>
        </div>`}
      </li>`;
    })}</ul>`}
    ${more && html`<div class="actions center"><button class="btn tonal" disabled=${loading} onClick=${() => search(true)}>${loading ? '載入中…' : '載入更多'}</button></div>`}
  <//>`;
}

// trackWorks says what a song (a track list item) is to works: those of its album, and those it
// says something of already.
export const trackWorks = (item, album) => showDialog((close) => html`<${TrackWorks} item=${item} album=${album} close=${close} />`);

function TrackWorks({ item, album, close }) {
  const all = useLoad(() => get('/works'), []);
  const current = item.uses || [];
  const offered = [...(album ? album.works : [])];
  for (const u of current) {
    if (!offered.some((w) => w.id === u.work_id)) {
      const w = all.data && all.data.find((x) => x.id === u.work_id);
      offered.push(w || { id: u.work_id, name: `作品 ${u.work_id}`, type: 0 });
    }
  }
  const [rows, setRows] = useState(() => Object.fromEntries(offered.map((w) => {
    const u = current.find((x) => x.work_id === w.id);
    return [w.id, { on: !!u, use: u ? u.use : '', note: u ? u.note || '' : '' }];
  })));
  const [busy, run] = useRunner();
  const row = (id) => rows[id] || { on: false, use: '', note: '' };
  const set = (id, v) => setRows((r) => ({ ...r, [id]: { ...row(id), ...v } }));
  const save = () => run(async () => {
    const works = offered.filter((w) => row(w.id).on).map((w) => ({ work_id: w.id, use: row(w.id).use, note: row(w.id).note.trim() }));
    const res = await api('PUT', `/tracks/${item.trackId}/works`, { works });
    if (done(res, `已更新「${item.title}」的作品用途`) || !res.group) close();
  });
  return html`<${Dialog} title=${`「${item.title}」的作品用途`} onClose=${close} actions=${offered.length ? html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy} onClick=${save}>儲存</button>` : html`<button class="btn text" onClick=${close}>關閉</button>`}>
    ${!offered.length ? html`<p>這首歌的專輯還沒有關聯作品。先為專輯關聯作品，再標註這首歌是片頭曲、插曲或其他用途。</p>
        ${album && html`<button class="btn tonal" onClick=${() => linkWorks(album)}><${Icon} name="work" />關聯作品…</button>`}`
      : html`<p class="hint tight">勾選這首歌屬於的作品，並標註用途；作品頁會依用途列出歌曲。</p>
      <ul class="items work-uses">${offered.map((w) => {
        const r = row(w.id);
        return html`<li key=${w.id}>
          <label class="check grow"><input type="checkbox" checked=${r.on} onChange=${(e) => set(w.id, { on: e.target.checked })} />
            <span class="track-text"><span class="title">${w.name}</span><span class="sub">${workLine(w)}</span></span></label>
          <select value=${r.use} disabled=${!r.on} onChange=${(e) => set(w.id, { use: e.target.value })} aria-label=${`在「${w.name}」中的用途`}>
            <option value="">未指定</option>
            ${Object.entries(useNames).map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}
          </select>
          <input class="note" value=${r.note} disabled=${!r.on} maxLength=${200} placeholder="備註，例如：第 1–13 話"
            onInput=${(e) => set(w.id, { note: e.target.value })} aria-label=${`在「${w.name}」中的備註`} />
        </li>`;
      })}</ul>`}
  <//>`;
}

// useTags says what a song is to works, for its row: the use, and which work when the album has
// several.
export function useTags(uses, works) {
  return (uses || []).filter((u) => u.use || u.note).map((u) => {
    const w = works && works.length > 1 && works.find((x) => x.id === u.work_id);
    const label = [useNames[u.use], u.note].filter(Boolean).join(' ');
    return { label, title: [w ? w.name : '', useNames[u.use], u.note].filter(Boolean).join(' · ') };
  });
}

function fmtDay(ms) {
  return new Date(ms).toLocaleDateString('zh-TW', { year: 'numeric', month: 'long', day: 'numeric' });
}

// WorkPage is a work and what the library has of it.
export function WorkPage({ id }) {
  const rev = useLibRev();
  const data = useLoad(() => get('/works/' + id), [id], rev);
  const [full, setFull] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  if (data.loading && !data.data) return html`<${Spinner} />`;
  if (data.error) {
    return data.error.status === 404 ? html`<${Empty} icon="work">找不到這部作品。<a href=${href('library/works')}>回到作品</a><//>`
      : html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`;
  }
  const { work: w, albums, songs, url, stale } = data.data;
  const items = songs.map((t) => ({ ...fromTrack(t), key: 's' + t.id, tags: useTags([{ work_id: w.id, use: t.use, note: t.note }]), uses: null }));
  const refresh = async () => {
    setRefreshing(true);
    try {
      await post(`/works/${id}/refresh`);
      toast('已從 Bangumi 更新作品資料');
      data.reload();
    } catch (e) {
      toast(e.status === 404 ? 'Bangumi 上已經沒有這個條目，保留原本的資料。' : unavailable(e), 'error');
    }
    setRefreshing(false);
  };
  const menu = (e) => openMenu(e, [
    { icon: 'openNew', label: '在 Bangumi 查看', onClick: () => window.open(url, '_blank', 'noopener,noreferrer') },
    { icon: 'refresh', label: '從 Bangumi 更新資料', onClick: refresh },
  ]);
  const long = w.summary && w.summary.length > 220;
  return html`<section>
    <header class="album-head work-head">
      <${WorkImage} id=${w.image ? w.id : 0} size=${600} alt=${w.name} className="big" />
      <div class="album-info">
        <div class="overline"><a href=${href('library/works')}>作品</a>${workLine(w) ? ` · ${workLine(w)}` : ''}</div>
        <h1>${w.name}</h1>
        ${w.name_cn && html`<div class="sub">${w.name_cn}</div>`}
        ${w.score > 0 && html`<div class="sub">Bangumi 評分 ${w.score.toFixed(1)}（${w.votes} 人）${w.rank ? ` · 排名第 ${w.rank}` : ''}</div>`}
        <div class="sub">${[albums.length && `${albums.length} 張專輯`, songs.length && `${songs.length} 首標註用途的歌`].filter(Boolean).join(' · ') || '曲庫中已沒有它的專輯或歌曲'}</div>
        <div class="actions">
          ${items.length > 0 && html`<button class="btn filled" onClick=${() => playQueue(items, 0)}><${Icon} name="play" />播放歌曲</button>`}
          <a class="btn tonal" href=${url} target="_blank" rel="noopener noreferrer"><${Icon} name="openNew" />Bangumi</a>
          <${IconButton} icon="more" label="更多" onClick=${menu} />
        </div>
      </div>
    </header>
    ${w.summary && html`<div class=${'work-summary page-summary' + (long && !full ? ' clamped' : '')}>${w.summary}</div>
      ${long && html`<button class="btn text" onClick=${() => setFull(!full)}>${full ? '收合' : '顯示全部'}</button>`}`}
    <p class="hint tight source-line">資料來源：<a href=${url} target="_blank" rel="noopener noreferrer">Bangumi 番组计划</a>，${fmtDay(w.fetched_at)}取得${stale ? '（較舊，正在背景更新）' : ''}${refreshing ? '，更新中…' : ''}</p>
    ${songs.length > 0 && html`<h2 class="section-title">歌曲</h2>
      <${TrackList} items=${items} showAlbum menuExtra=${(it) => [{ icon: 'work', label: '作品用途…', onClick: () => songUses(it) }]} />`}
    <h2 class="section-title">專輯</h2>
    <${AlbumGrid} albums=${albums} empty="還沒有專輯關聯到這部作品。" />
  </section>`;
}

// songUses opens a song's uses from a list that is not its album's: its first album's works are
// offered, with what it says now.
async function songUses(item) {
  try {
    const a = item.albumId ? await get('/albums/' + item.albumId) : null;
    const e = a && a.entries.find((x) => x.track_id === item.trackId);
    trackWorks({ ...item, uses: e ? e.works || [] : [] }, a);
  } catch (e) {
    toast(e.message, 'error');
  }
}

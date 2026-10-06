import { useEffect, useRef, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { go, href, keepInAddress, parseHash } from '../router.js';
import { Dialog, Empty, ErrorBox, Icon, Spinner, html, showDialog, toast } from '../ui.js';
import { confirmDialog, useRunner } from './organize.js';
import { WorkImage, bindSubject, workLine } from './works.js';

// The album's own entry in Bangumi (a music subject) and its owner's collection of it, through
// their linked Bangumi account (review #94): status, rating, tags, comment, private — written to
// Bangumi only when saved here. And the owner's music collections, with the library's albums of them.

export const statusNames = { 1: '想聽', 2: '聽過', 3: '在聽', 4: '擱置', 5: '拋棄' };

const unavailable = (e) => (e && e.status === 503 ? 'Bangumi 暫時無法連線，請稍後再試。' : e && e.message);

// SubjectRow is the album page's line for its Bangumi entry: the entry and how it is collected, or a
// way to bind one.
export function SubjectRow({ album }) {
  const sub = album.subject;
  const [col, setCol] = useState(null); // { linked, collection } once read
  useEffect(() => {
    setCol(null);
    if (!sub) return;
    // Another album or entry since: its answer shows nothing (review #177).
    let alive = true;
    get(`/albums/${album.id}/collection`).then((v) => alive && setCol(v), () => alive && setCol({ failed: true }));
    return () => { alive = false; };
  }, [album.id, sub && sub.id]);
  if (!sub) {
    return html`<div class="chips album-categories album-works">
      <button class="chip ghost" onClick=${() => bindSubject(album)}><${Icon} name="note" size=${16} />綁定 Bangumi 條目</button></div>`;
  }
  const c = col && col.collection;
  const label = !col ? '…' : col.failed ? '收藏' : !col.linked ? '連結帳號以管理收藏' : c ? `${statusNames[c.type]}${c.rate ? ` ★${c.rate}` : ''}` : '收藏…';
  return html`<div class="chips album-categories album-works">
    <a class="chip work-chip" href=${`https://bgm.tv/subject/${sub.source_id}`} target="_blank" rel="noopener noreferrer"
      title=${['Bangumi', sub.name_cn, sub.date].filter(Boolean).join(' · ')}>
      <${Icon} name="openNew" size=${16} />Bangumi${sub.score ? ` ${sub.score.toFixed(1)}` : ''}</a>
    <button class=${'chip' + (c ? ' on' : ' ghost')} onClick=${() => editCollection(album, (v) => setCol(v))}>
      <${Icon} name=${c ? 'favorite' : 'favoriteOff'} size=${16} />${label}</button>
    <button class="chip ghost" onClick=${() => bindSubject(album)} title="改綁或解除 Bangumi 條目"><${Icon} name="edit" size=${16} />條目</button>
  </div>`;
}

export const editCollection = (album, onSaved) => showDialog((close) => html`<${CollectionDialog} album=${album} onSaved=${onSaved} close=${close} />`);

// CollectionDialog changes how the owner collected the album's subject in Bangumi, as Bangumi's own
// "修改收藏" does.
function CollectionDialog({ album, onSaved, close }) {
  const [data, setData] = useState(null);
  const [error, setError] = useState(null);
  const [form, setForm] = useState(null);
  const [busy, run] = useRunner();
  useEffect(() => {
    let alive = true;
    get(`/albums/${album.id}/collection`).then((d) => {
      if (!alive) return;
      setData(d);
      const c = d.collection;
      setForm({ type: c ? c.type : 2, rate: c ? c.rate : 0, tags: c ? c.tags.join(' ') : '', comment: c ? c.comment || '' : '', private: c ? c.private : false });
    }, (e) => alive && setError(e));
    return () => { alive = false; };
  }, [album.id]);
  // The owner's own tags are gathered apart: they show once they are (review #185).
  useEffect(() => {
    if (!data || !data.my_tags_pending) return;
    let alive = true, tries = 0;
    const ask = () => get('/bangumi/tags').then((r) => {
      if (!alive) return;
      if (!r.pending || ++tries >= 20) setData((d) => ({ ...d, my_tags: r.tags, my_tags_pending: false }));
      else t = setTimeout(ask, 1500);
    }, () => {});
    let t = setTimeout(ask, 1000);
    return () => { alive = false; clearTimeout(t); };
  }, [data && data.my_tags_pending]);
  const tags = form ? form.tags.split(/[\s,，、]+/).filter(Boolean) : [];
  const toggleTag = (t) => setForm((f) => {
    const list = f.tags.split(/[\s,，、]+/).filter(Boolean);
    const next = list.includes(t) ? list.filter((x) => x !== t) : [...list, t];
    return { ...f, tags: next.join(' ') };
  });
  const save = () => run(async () => {
    if (tags.length > 10) throw new Error('標籤最多 10 個');
    // The answer is the collection as saved: nothing to read again (review #185).
    const r = await api('PUT', `/albums/${album.id}/collection`, { ...form, tags: [...new Set(tags)] });
    toast(`已更新 Bangumi 收藏：${statusNames[form.type]}`);
    if (onSaved) onSaved({ ...data, collection: r.collection });
    close();
  });
  const sub = album.subject;
  return html`<${Dialog} title="修改收藏" onClose=${close} actions=${data && data.linked ? html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !form} onClick=${save}>${busy ? '儲存中…' : '儲存到 Bangumi'}</button>` : html`<button class="btn text" onClick=${close}>關閉</button>`}>
    <div class="collection-head">
      <${WorkImage} id=${sub.image ? sub.id : 0} size=${96} className="thumb" />
      <span class="track-text"><a class="title" href=${`https://bgm.tv/subject/${sub.source_id}`} target="_blank" rel="noopener noreferrer">${sub.name}</a>
        <span class="sub">${[sub.name_cn, workLine(sub), sub.score && `★ ${sub.score.toFixed(1)}`].filter(Boolean).join(' · ')}</span></span>
    </div>
    ${error && html`<${ErrorBox} error=${{ message: unavailable(error) }} />`}
    ${!data && !error && html`<${Spinner} />`}
    ${data && !data.linked && html`<p>連結你的 Bangumi 帳號後，就能在這裡標記想聽、聽過，評分、加標籤和吐槽。</p>
      <button class="btn tonal" onClick=${() => { close(); go('settings'); }}><${Icon} name="link" />到設定連結 Bangumi</button>`}
    ${data && data.linked && form && html`
      <div class="seg collection-type" role="radiogroup" aria-label="收藏狀態">${Object.entries(statusNames).map(([k, label]) => html`<label key=${k}
        class=${form.type === Number(k) ? 'on' : ''}><input type="radio" checked=${form.type === Number(k)} onChange=${() => setForm({ ...form, type: Number(k) })} />${label}</label>`)}</div>
      <div class="field">我的評價
        <div class="stars" role="radiogroup" aria-label="評分">
          <button type="button" class="star-clear" aria-label="不評分" title="不評分" onClick=${() => setForm({ ...form, rate: 0 })}><${Icon} name="close" size=${18} /></button>
          ${[1, 2, 3, 4, 5, 6, 7, 8, 9, 10].map((n) => html`<button key=${n} type="button" role="radio" aria-checked=${form.rate === n} aria-label=${`${n} 分`}
            class=${'star' + (n <= form.rate ? ' on' : '')} onClick=${() => setForm({ ...form, rate: n })}>★</button>`)}
          <span class="sub">${form.rate ? `${form.rate} 分` : '未評分'}</span>
        </div>
      </div>
      <label class="field">標籤（用空格分開，最多 10 個）
        <input value=${form.tags} onInput=${(e) => setForm({ ...form, tags: e.target.value })} /></label>
      ${tags.length > 10 && html`<p class="hint tight warn-text">標籤超過 10 個。</p>`}
      ${[['常用標籤', data.subject_tags], ['我的標籤', data.my_tags]].map(([label, list]) => list.length > 0 && html`<div key=${label} class="tag-picks">
        <span class="sub">${label}</span>
        ${list.map((t) => html`<button key=${t} type="button" class=${'chip-check' + (tags.includes(t) ? ' on' : '')} onClick=${() => toggleTag(t)}>${t}</button>`)}
      </div>`)}
      <label class="field">吐槽<textarea rows="3" maxLength=${1000} value=${form.comment} onInput=${(e) => setForm({ ...form, comment: e.target.value })}></textarea></label>
      <label class="check-row"><input type="checkbox" checked=${form.private} onChange=${(e) => setForm({ ...form, private: e.target.checked })} />僅自己可見</label>
      <p class="hint tight">儲存時才會寫到 Bangumi（帳號 ${data.username}）；播放或瀏覽不會改動你的收藏。</p>`}
  <//>`;
}

const results = {
  linked: ['已連結 Bangumi 帳號。', 'info'],
  denied: ['你在 Bangumi 取消了授權，沒有連結。', 'error'],
  expired: ['這次連結已過期，請再按一次「連結 Bangumi 帳號」。', 'error'],
  refused: ['Bangumi 拒絕了這次連結：請確認 App ID、App Secret 與回調地址都正確。', 'error'],
  failed: ['連結失敗：Bangumi 沒有回應，請稍後再試。', 'error'],
};

// BangumiAccount is the settings page's: the Bangumi application and the account linked.
export function BangumiAccount() {
  const [info, setInfo] = useState(null);
  const [error, setError] = useState(null);
  const [id, setId] = useState('');
  const [secret, setSecret] = useState('');
  const [busy, run] = useRunner();
  const load = () => get('/bangumi/account', { timeout: 10000 }).then((r) => {
    setInfo(r);
    setId(r.app_id || '');
    setError(null);
  }, setError);
  useEffect(() => {
    load();
    const r = parseHash().query.get('bangumi');
    if (r && results[r]) {
      toast(...results[r]);
      keepInAddress('settings');
    }
  }, []);
  const save = () => run(async () => {
    await api('PUT', '/bangumi/app', { app_id: id.trim(), app_secret: secret.trim() });
    setSecret('');
    toast('已儲存');
    load();
  });
  const link = () => run(async () => {
    location.href = (await post('/bangumi/link')).url;
  });
  const unlink = () => confirmDialog({
    title: '解除 Bangumi 連結', action: '解除連結', danger: true,
    children: html`<p>Kanade 會忘記這個帳號的授權，之後無法在這裡管理收藏；已綁定的條目會保留。Bangumi 沒有撤銷授權的功能，舊授權會在一週內自動失效。</p>`,
    onConfirm: () => api('DELETE', '/bangumi/link').then(() => { kept = null; load(); }),
  });
  const l = info && info.link;
  const ready = info && info.app_id && info.has_secret;
  const dirty = info && (id !== (info.app_id || '') || secret !== '');
  const copy = () => navigator.clipboard.writeText(info.redirect_uri).then(() => toast('已複製'), () => toast('無法複製，請手動選取', 'error'));
  return html`<h2 class="section-title">Bangumi 帳號</h2>
    <div class="card pad">
      <div class="sub">連結你的 Bangumi 帳號後，可以在專輯頁管理它的音樂條目收藏（想聽、聽過、評分、標籤、吐槽），在曲庫的「Bangumi」分頁看你的收藏和曲庫有沒有。只有你按儲存時才會寫到 Bangumi。</div>
      <${ErrorBox} error=${error} onRetry=${load} />
      ${!info && !error && html`<${Spinner} />`}
      ${info && html`<details class="app-setup" open=${!ready}>
        <summary>Bangumi 應用程式${info.app_id ? `：${info.app_id}` : '（還沒設定）'}</summary>
        <ol class="steps">
          <li>到 <a class="link" href="https://bgm.tv/dev/app" target="_blank" rel="noopener noreferrer">bgm.tv/dev/app</a> 建立應用程式（名稱例如「Kanade」）。</li>
          <li>回調地址（Callback URL）填：
            <div class="command"><code>${info.redirect_uri}</code><button class="btn text" onClick=${copy}><${Icon} name="copy" />複製</button></div></li>
          <li>把 App ID 與 App Secret 填在下面。Secret 只存在伺服器上，不會再顯示。</li>
        </ol>
        <div class="form-grid">
          <label class="field">App ID<input value=${id} onInput=${(e) => setId(e.target.value)} placeholder="bgm…" /></label>
          <label class="field">App Secret<input type="password" value=${secret} autocomplete="off" placeholder=${info.has_secret ? '已設定（要更換才填）' : ''}
            onInput=${(e) => setSecret(e.target.value)} /></label>
        </div>
        <div class="actions"><button class="btn filled" disabled=${busy || !dirty} onClick=${save}>儲存</button></div>
      </details>
      ${l ? html`<div class="toggle-row">
          <span class="grow"><span class="title">已連結 ${l.nickname || l.username}（@${l.username}）</span>
            ${l.error && html`<span class="sub state-failed">${l.error}</span>`}</span>
          ${l.error && html`<button class="btn tonal" disabled=${busy || !ready} onClick=${link}>重新連結</button>`}
          <button class="btn text" onClick=${unlink}>解除連結</button>
        </div>`
        : html`<div class="actions"><button class="btn filled" disabled=${busy || !ready} onClick=${link}><${Icon} name="link" />連結 Bangumi 帳號</button>
          ${!ready && html`<span class="sub">先填好上面的 App ID 與 App Secret。</span>`}</div>`}`}
    </div>`;
}

// What the collections tab read last, kept a while: back from an album, it shows as it was, with
// the pages loaded, without asking Bangumi again (review #187).
let kept = null; // { type, pages, at }
const keptFor = 5 * 60 * 1000;

// CollectionsTab is the owner's music collections in Bangumi, a type at a time, with the library's
// albums of each, on "my" page (review #190). The type is in the address (#/me/bangumi?type=1).
export function CollectionsTab() {
  const [type, setType] = useState(() => {
    const t = Number(parseHash().query.get('type'));
    return statusNames[t] ? t : 2;
  });
  const [pages, setPages] = useState(() => (kept && kept.type === type && Date.now() - kept.at < keptFor ? kept.pages : null));
  const [error, setError] = useState(null);
  const [loading, setLoading] = useState(false);
  // Each read is numbered: one that answers after a newer one, or after the type changed, shows
  // nothing (review #177).
  const asked = useRef(0);
  const load = async (more) => {
    const my = ++asked.current;
    setLoading(true);
    setError(null);
    try {
      const r = await get(`/bangumi/collections?type=${type}&offset=${more ? pages.items.length : 0}`);
      if (my !== asked.current) return;
      const next = more ? { ...r, items: [...pages.items, ...r.items] } : r;
      setPages(next);
      kept = { type, pages: next, at: Date.now() };
    } catch (e) {
      if (my !== asked.current) return;
      setError(e);
      if (!more) setPages(null);
    }
    setLoading(false);
  };
  const shown = useRef(pages ? type : null); // the type shown as it was kept
  useEffect(() => {
    keepInAddress(`me/bangumi?type=${type}`);
    if (shown.current === type) {
      shown.current = null;
      return;
    }
    setPages(null);
    load(false);
  }, [type]);
  if (error && error.body && error.body.reason === 'not_linked') {
    return html`<${Empty} icon="link">連結 Bangumi 帳號後，這裡會列出你的音樂收藏（想聽、聽過…），並標出曲庫有沒有。
      <div class="actions center"><a class="btn tonal" href=${href('settings')}>到設定連結</a></div><//>`;
  }
  return html`<div class="list-tools">
      <nav class="seg" aria-label="收藏狀態">${Object.entries(statusNames).map(([k, label]) => html`<button key=${k}
        class=${Number(k) === type ? 'on' : ''} aria-pressed=${Number(k) === type} onClick=${() => setType(Number(k))}>${label}</button>`)}</nav>
      ${pages && html`<span class="sub">@${pages.username} 共 ${pages.total} 個</span>`}
    </div>
    ${error && html`<${ErrorBox} error=${{ message: unavailable(error) }} onRetry=${() => load(false)} />`}
    ${loading && !pages && html`<${Spinner} />`}
    ${pages && !pages.items.length && html`<${Empty} icon="note">沒有「${statusNames[type]}」的音樂收藏。<//>`}
    ${pages && pages.items.length > 0 && html`<ul class="items collections">${pages.items.map((c) => html`<li key=${c.subject_id}>
      <${WorkImage} sid=${c.image ? c.subject_id : 0} size=${96} className="thumb" />
      <span class="grow track-text">
        <a class="title" href=${c.url} target="_blank" rel="noopener noreferrer">${c.name || `條目 ${c.subject_id}`}</a>
        <span class="sub">${[c.name_cn, c.date, c.rate ? `我的評分 ${c.rate}` : '', c.tags.join(' ')].filter(Boolean).join(' · ')}</span>
        ${c.comment && html`<span class="sub">「${c.comment}」</span>`}
      </span>
      <span class="collection-albums">${c.albums.length
        ? c.albums.map((a) => html`<a key=${a.id} class="chip" href=${href('album/' + a.id)}><${Icon} name="album" size=${16} />${a.title}</a>`)
        : html`<span class="sub">曲庫沒有（或還沒綁定）</span>
          <a class="btn text" href=${href('feeds?q=' + encodeURIComponent(c.name || ''))}><${Icon} name="download" />找資源</a>`}</span>
    </li>`)}</ul>`}
    ${pages && pages.items.length < pages.total && html`<div class="actions center">
      <button class="btn tonal" disabled=${loading} onClick=${() => load(true)}>${loading ? '載入中…' : '載入更多'}</button></div>`}`;
}

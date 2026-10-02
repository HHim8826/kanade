import { useEffect, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { go, href } from '../router.js';
import { Dialog, Empty, ErrorBox, Icon, IconButton, Spinner, fmtBytes, html, openMenu, showDialog, toast, useLoad } from '../ui.js';

// RSS sources of torrents (P2-5): items from the feeds, rules, automatic download.

const ago = (ms) => {
  if (!ms) return '從未';
  const s = Math.round((Date.now() - ms) / 1000);
  if (s < 60) return '剛剛';
  if (s < 3600) return `${Math.round(s / 60)} 分鐘前`;
  if (s < 86400) return `${Math.round(s / 3600)} 小時前`;
  return `${Math.round(s / 86400)} 天前`;
};
const clock = (ms) => new Date(ms).toLocaleString('zh-TW', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });

export function Feeds({ tab = 'items', q = '' }) {
  const [rev, setRev] = useState(0);
  const sources = useLoad(() => get('/rss/sources'), [], rev);
  const reload = () => setRev((n) => n + 1);
  return html`<section>
    <div class="page-head">
      <h1 class="page-title">RSS 訂閱</h1>
      <div class="actions tight">
        <a class="btn text" href=${href('tasks')}><${Icon} name="tasks" />任務</a>
        <button class="btn tonal" onClick=${() => editSource(null, reload)}><${Icon} name="add" />新增來源</button>
      </div>
    </div>
    <nav class="tabs" role="tablist">
      ${[['items', '條目'], ['sources', '來源']].map(([k, label]) => html`<a role="tab" aria-selected=${k === tab} class=${k === tab ? 'active' : ''}
        href=${href('feeds?tab=' + k)}>${label}</a>`)}
    </nav>
    ${sources.loading && !sources.data ? html`<${Spinner} />` : html`<${ErrorBox} error=${sources.error} onRetry=${sources.reload} />`}
    ${sources.data && !sources.data.length && html`<${Empty} icon="download">還沒有 RSS 來源。按「新增來源」，可以用 Nyaa 範本。<//>`}
    ${sources.data && sources.data.length > 0 && (tab === 'sources'
      ? html`<${SourceList} sources=${sources.data} reload=${reload} />`
      : html`<${ItemList} sources=${sources.data} initialQ=${q} rev=${rev} />`)}
  </section>`;
}

// ---- items ----

function ItemList({ sources, initialQ, rev }) {
  const [q, setQ] = useState(initialQ);
  const [term, setTerm] = useState(initialQ);
  const [source, setSource] = useState(0);
  const [only, setOnly] = useState(false);
  const [more, setMore] = useState({ key: '', pages: [], done: false });
  const [live, setLive] = useState(null); // { source, q, items } from a search on the site
  const [liveBusy, setLiveBusy] = useState(false);
  useEffect(() => {
    const t = setTimeout(() => setTerm(q), 300);
    return () => clearTimeout(t);
  }, [q]);
  const params = (extra = '') => `/rss/items?limit=50&q=${encodeURIComponent(term)}&source=${source}${only ? '&only=included' : ''}${extra}`;
  const key = [term, source, only, rev].join('|');
  const first = useLoad(() => get(params()), [term, source, only], rev);
  const pages = more.key === key ? more.pages : [];
  const list = [...(first.data || []), ...pages.flat()];
  const loadMore = async () => {
    try {
      const next = await get(params(`&before=${list[list.length - 1].id}`));
      setMore({ key, pages: [...pages, next], done: next.length < 50 });
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const searchable = sources.filter((s) => s.searchable && (!source || s.id === source));
  const searchSite = async (s) => {
    setLiveBusy(true);
    try {
      setLive({ source: s, q: term, items: await get(`/rss/sources/${s.id}/search?q=${encodeURIComponent(term)}`) });
    } catch (e) {
      toast(e.message, 'error');
    }
    setLiveBusy(false);
  };
  return html`
    <div class="toolbar">
      <label class="search-field small grow"><${Icon} name="search" />
        <input type="search" placeholder="篩選標題（同一格的詞都要出現）" value=${q} onInput=${(e) => setQ(e.target.value)} aria-label="篩選標題" /></label>
      <label class="select-field">來源<select value=${source} onChange=${(e) => setSource(Number(e.target.value))}>
        <option value="0">全部</option>${sources.map((s) => html`<option value=${s.id}>${s.name}</option>`)}</select></label>
      <label class="check-field"><input type="checkbox" checked=${only} onChange=${(e) => setOnly(e.target.checked)} />只看符合規則</label>
    </div>
    ${term.trim() && searchable.length > 0 && html`<div class="actions">
      ${searchable.map((s) => html`<button class="btn tonal" disabled=${liveBusy} onClick=${() => searchSite(s)}><${Icon} name="search" />在「${s.name}」站內搜尋「${term}」</button>`)}
    </div>`}
    ${liveBusy && html`<${Spinner} />`}
    ${live && html`<div class="card pad live-results">
      <div class="section-head"><h2 class="section-title">「${live.source.name}」站內搜尋「${live.q}」</h2>
        <${IconButton} icon="close" label="關閉搜尋結果" onClick=${() => setLive(null)} /></div>
      <p class="hint">即時向網站查詢，結果不會存起來；網站的 RSS 不一定有完整的歷史。</p>
      ${live.items.length ? html`<ul class="feed-items">${live.items.map((it, i) => html`<${ItemRow} key=${'l' + i} it=${it} live=${live.source} />`)}</ul>`
        : html`<${Empty} icon="search">沒有結果<//>`}
    </div>`}
    ${first.loading && !first.data ? html`<${Spinner} />` : html`<${ErrorBox} error=${first.error} onRetry=${first.reload} />`}
    ${first.data && !list.length && html`<${Empty} icon="download">${term || only ? '沒有符合的條目' : '還沒有條目；來源第一次更新後會出現在這裡。'}<//>`}
    <ul class="feed-items">${list.map((it) => html`<${ItemRow} key=${it.id} it=${it} />`)}</ul>
    ${first.data && list.length >= 50 && !(more.key === key && more.done) && html`<div class="actions center">
      <button class="btn tonal" onClick=${loadMore}>載入更多</button></div>`}`;
}

function ItemRow({ it, live }) {
  const [busy, setBusy] = useState(false);
  const [started, setStarted] = useState(0);
  const download = async () => {
    setBusy(true);
    try {
      const r = live ? await post(`/rss/sources/${live.id}/download`, { link: it.download }) : await post(`/rss/items/${it.id}/download`);
      setStarted(r.id);
      toast('已加入下載，取得檔案清單後請選擇要下載的檔案', 'info', { label: '前往任務', onClick: () => go('tasks') });
    } catch (e) {
      toast(e.message, 'error');
    }
    setBusy(false);
  };
  const done = it.downloaded || started > 0;
  return html`<li class="feed-item">
    <div class="grow">
      <div class="title">${it.title}</div>
      <div class="sub">
        ${[it.source, it.size ? fmtBytes(it.size) : '大小未知', it.seeders >= 0 ? `做種 ${it.seeders}` : '', it.published_at ? clock(it.published_at) : ''].filter(Boolean).join(' · ')}
        ${it.match === 'included' && html` <span class="pill good">符合規則</span>`}
        ${it.match === 'excluded' && html` <span class="pill">已排除</span>`}
        ${done && html` <span class="pill">已下載</span>`}
        ${!it.download && html` <span class="pill warn">只有網頁，無法直接下載</span>`}
      </div>
    </div>
    <div class="feed-actions">
      ${it.download && !done && html`<button class="btn tonal" disabled=${busy} onClick=${download}><${Icon} name="download" />下載</button>`}
      ${done && html`<a class="btn text" href=${href('tasks')}>任務</a>`}
      ${it.page && html`<a class="btn text" href=${it.page} target="_blank" rel="noopener noreferrer">網頁</a>`}
    </div>
  </li>`;
}

// ---- sources ----

function SourceList({ sources, reload }) {
  const refresh = async (s) => {
    toast(`正在更新「${s.name}」…`);
    try {
      const r = await post(`/rss/sources/${s.id}/refresh`);
      toast(`「${s.name}」：取得 ${r.fetched} 個條目，新的 ${r.new} 個${r.downloaded ? `，自動下載 ${r.downloaded} 個` : ''}`);
    } catch (e) {
      toast(`「${s.name}」更新失敗：${e.message}`, 'error');
    }
    reload();
  };
  const remove = (s) => showDialog((close) => html`<${Dialog} title="刪除來源" onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled danger" onClick=${async () => {
        try {
          await api('DELETE', '/rss/sources/' + s.id);
          close();
          toast(`已刪除「${s.name}」`);
          reload();
        } catch (e) {
          toast(e.message, 'error');
        }
      }}>刪除</button>`}>
    <p>刪除「${s.name}」與它的 ${s.items} 個條目？已開始的下載不受影響。</p>
  <//>`);
  return html`<div class="source-list">${sources.map((s) => html`<article class="task" key=${s.id}>
    <div class="task-head">
      <div class="grow">
        <div class="title">${s.name}${!s.enabled && html` <span class="chip">已停用</span>`}${s.auto_download && html` <span class="chip state-completed">自動下載</span>`}</div>
        <div class="sub path">${s.url}</div>
        <div class="sub">
          ${s.last_error ? html`<span class="state-failed">更新失敗 ${s.failures} 次：${s.last_error}</span>` : `上次更新：${ago(s.last_ok_at)}`}
          · ${s.items} 個條目 · 每 ${s.interval_min} 分鐘${s.enabled && s.next_poll_at ? ` · 下次 ${clock(s.next_poll_at)}` : ''}
        </div>
        ${(s.include || s.exclude) && html`<div class="sub rules">${s.include && html`<span>包含：${s.include.split('\n').join('｜')}</span>`}
          ${s.exclude && html`<span>排除：${s.exclude.split('\n').join('｜')}</span>`}</div>`}
        ${s.auto_download && s.baseline && html`<div class="sub">下一次更新只記錄目前的條目，之後出現的新條目才會自動下載。</div>`}
      </div>
      <div class="task-actions">
        <${IconButton} icon="refresh" label="立即更新" onClick=${() => refresh(s)} />
        <${IconButton} icon="more" label="更多" onClick=${(e) => openMenu(e, [
          { icon: 'edit', label: '編輯…', onClick: () => editSource(s, reload) },
          { icon: 'search', label: '查看條目', onClick: () => go('feeds?tab=items') },
          { icon: 'delete', label: '刪除…', onClick: () => remove(s) },
        ])} />
      </div>
    </div>
  </article>`)}</div>`;
}

function editSource(src, reload) {
  showDialog((close) => html`<${SourceDialog} src=${src} close=${close} reload=${reload} />`);
}

const nyaaCategories = [['2_1', '無損音樂'], ['2_2', '有損音樂'], ['2_0', '所有音訊']];
const nyaaFilters = [['0', '不篩選'], ['1', '排除 remake'], ['2', '只要 trusted']];

function SourceDialog({ src, close, reload }) {
  const [f, setF] = useState(() => ({
    name: src ? src.name : '', url: src ? src.url : '', interval: String(src ? src.interval_min : 30), enabled: src ? src.enabled : true,
    include: src ? src.include : '', exclude: src ? src.exclude : '', auto: src ? src.auto_download : false,
    user: src ? src.auth_user : '', pass: '', cookie: '', clearAuth: false, clearCookie: false,
  }));
  const [nyaa, setNyaa] = useState({ c: '2_1', f: '0', q: '' });
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setF({ ...f, [k]: e.target.type === 'checkbox' ? e.target.checked : e.target.value });
  const applyNyaa = () => {
    const url = `https://nyaa.si/?page=rss&c=${nyaa.c}&f=${nyaa.f}&q=${encodeURIComponent(nyaa.q.trim())}`;
    const cat = nyaaCategories.find(([k]) => k === nyaa.c)[1];
    setF({ ...f, url, name: f.name || `Nyaa ${nyaa.q.trim() || cat}` });
  };
  const save = async (e) => {
    e.preventDefault();
    if (f.auto && !f.include.trim()) return toast('自動下載需要至少一條包含規則', 'error');
    const body = { name: f.name, url: f.url, interval_min: Number(f.interval), enabled: f.enabled, include: f.include, exclude: f.exclude,
      auto_download: f.auto, auth_user: f.user, clear_auth: f.clearAuth, clear_cookie: f.clearCookie };
    if (f.pass) body.auth_pass = f.pass;
    if (f.cookie) body.cookie = f.cookie;
    setBusy(true);
    try {
      if (src) await api('PATCH', '/rss/sources/' + src.id, body);
      else await post('/rss/sources', body);
      toast(src ? '已儲存' : '已新增，正在第一次更新（只記錄目前的條目）');
      close();
      reload();
      if (!src) setTimeout(reload, 4000);
    } catch (err) {
      toast(err.message, 'error');
      setBusy(false);
    }
  };
  return html`<${Dialog} title=${src ? '編輯來源' : '新增 RSS 來源'} wide onClose=${close}>
    <form onSubmit=${save}>
      ${!src && html`<div class="card pad template">
        <div class="title">Nyaa 範本</div>
        <div class="form-grid three">
          <label class="field">分類<select value=${nyaa.c} onChange=${(e) => setNyaa({ ...nyaa, c: e.target.value })}>
            ${nyaaCategories.map(([k, label]) => html`<option value=${k}>${label}</option>`)}</select></label>
          <label class="field">篩選<select value=${nyaa.f} onChange=${(e) => setNyaa({ ...nyaa, f: e.target.value })}>
            ${nyaaFilters.map(([k, label]) => html`<option value=${k}>${label}</option>`)}</select></label>
          <label class="field">關鍵字<input value=${nyaa.q} placeholder="例如 ARIA、水樹奈々" onInput=${(e) => setNyaa({ ...nyaa, q: e.target.value })} /></label>
        </div>
        <button type="button" class="btn tonal" onClick=${applyNyaa}>套用到下面的網址</button>
      </div>`}
      <div class="form-grid">
        <label class="field">名稱<input value=${f.name} required maxlength="200" onInput=${set('name')} /></label>
        <label class="field">更新間隔（分鐘，至少 10）<input type="number" min="10" max="1440" value=${f.interval} onInput=${set('interval')} /></label>
        <label class="field span">RSS 網址<input value=${f.url} required placeholder="https://…" onInput=${set('url')} /></label>
        <label class="field">包含規則<textarea rows="3" value=${f.include} placeholder=${'例如\nflac aria\nflac 水樹奈々'} onInput=${set('include')}></textarea></label>
        <label class="field">排除規則<textarea rows="3" value=${f.exclude} placeholder=${'例如\nmp3\nremake'} onInput=${set('exclude')}></textarea></label>
        <p class="hint span tight">每行一條；同一行的詞都出現才算符合，任一行符合即可。不分大小寫、全半形與平假名／片假名。排除規則優先。</p>
        <label class="check-field span"><input type="checkbox" checked=${f.enabled} onChange=${set('enabled')} />定時更新</label>
        <label class="check-field span"><input type="checkbox" checked=${f.auto} onChange=${set('auto')} />自動下載符合包含規則的新條目</label>
        <p class="hint span tight">預設關閉。開啟後只下載「之後才出現」的條目（開啟時的下一次更新只記錄目前的內容），並自動採用預設勾選的檔案；空間不足時會停在選檔並說明原因。每次更新最多自動下載 5 個。</p>
      </div>
      <details class="auth">
        <summary>認證（私人站台用，選填）</summary>
        <div class="form-grid">
          <label class="field">使用者名稱<input value=${f.user} autocomplete="off" onInput=${set('user')} /></label>
          <label class="field">密碼<input type="password" value=${f.pass} autocomplete="new-password" placeholder=${src && src.has_password ? '已設定；留空不變' : ''} onInput=${set('pass')} /></label>
          <label class="field span">Cookie<input value=${f.cookie} autocomplete="off" placeholder=${src && src.has_cookie ? '已設定；留空不變' : 'name=value; …'} onInput=${set('cookie')} /></label>
          ${src && (src.has_password || src.auth_user) && html`<label class="check-field"><input type="checkbox" checked=${f.clearAuth} onChange=${set('clearAuth')} />清除帳號密碼</label>`}
          ${src && src.has_cookie && html`<label class="check-field"><input type="checkbox" checked=${f.clearCookie} onChange=${set('clearCookie')} />清除 Cookie</label>`}
        </div>
        <p class="hint tight">只存在伺服器的資料庫裡，用來取得 RSS 與 .torrent 檔。</p>
      </details>
      <div class="dialog-actions">
        <button type="button" class="btn text" onClick=${close}>取消</button>
        <button class="btn filled" disabled=${busy}>${src ? '儲存' : '新增'}</button>
      </div>
    </form>
  <//>`;
}

import { useCallback, useEffect, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { go, href } from '../router.js';
import { SelectBar, SelectToggle, useSelection } from '../selection.js';
import { Cover, Dialog, Empty, ErrorBox, Icon, IconButton, Spinner, html, openMenu, showDialog, toast, useLoad } from '../ui.js';
import { AlbumActions } from './batch.js';
import { AlbumGrid } from './common.js';
import { Field, changed, done, useLibRev, useRunner } from './organize.js';

// Folders of the user's own for albums (review #92): the categories tab of the library, a category's
// page, and putting albums into categories. A category only groups albums for browsing: putting an
// album in one or taking it out changes nothing of the album, its songs or files, and is one edit the
// edit log can undo.

const PAGE = 200;
const report = (e) => toast(e.message, 'error');

// Mosaic shows up to four covers of a category's albums.
function Mosaic({ covers }) {
  if (!covers.length) return html`<div class="cover mosaic empty"><${Icon} name="folder" size=${48} /></div>`;
  return html`<div class=${'cover mosaic n' + Math.min(covers.length, 4)}>
    ${covers.slice(0, 4).map((c) => html`<${Cover} key=${c} id=${c} alt="" />`)}</div>`;
}

// CategoriesTab lists the categories, and the albums in none, as cards.
export function CategoriesTab({ data }) {
  const add = () => showDialog((close) => html`<${NameCategory} close=${close} taken=${data.categories.map((c) => c.name)} />`);
  return html`<div class="section-head"><span class="grow sub">${data.categories.length
      ? `${data.categories.length} 個分類；同一張專輯可以放在多個分類，分類不會改動專輯本身。`
      : '用分類把同一部作品或系列的專輯放在一起，例如「海貓」「ARIA」。'}</span>
      <button class="btn tonal" onClick=${add}><${Icon} name="add" />新增分類</button></div>
    <div class="grid">
      ${data.categories.map((c) => html`<a key=${c.id} class="card album-card category-card" href=${href('category/' + c.id)}>
        <${Mosaic} covers=${c.covers} />
        <div class="card-text"><div class="title" title=${c.name}>${c.name}</div><div class="sub">${c.albums} 張專輯</div></div>
      </a>`)}
      <a class="card album-card category-card uncategorized" href=${href('category/none')}>
        <${Mosaic} covers=${data.uncategorized_covers} />
        <div class="card-text"><div class="title">未分類</div><div class="sub">${data.uncategorized} 張專輯</div></div>
      </a>
    </div>`;
}

// NameCategory makes a category, or renames one (category).
function NameCategory({ close, taken, category }) {
  const [name, setName] = useState(category ? category.name : '');
  const [busy, run] = useRunner();
  const clean = name.trim().replace(/\s+/g, ' ');
  const dup = taken.some((t) => t.toLowerCase() === clean.toLowerCase() && (!category || t !== category.name));
  const submit = () => run(async () => {
    if (category) {
      await api('PATCH', '/categories/' + category.id, { name: clean });
      toast(`已改名為「${clean}」`);
    } else {
      await post('/categories', { name: clean });
      toast(`已新增分類「${clean}」`);
    }
    changed();
    close();
  });
  return html`<${Dialog} title=${category ? '分類改名' : '新增分類'} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled" disabled=${busy || !clean || dup} onClick=${submit}>${category ? '改名' : '新增'}</button>`}>
    <${Field} label="分類名稱" value=${name} onInput=${setName} autofocus />
    ${dup && html`<p class="hint warn-text">已經有同名的分類。</p>`}
  <//>`;
}

// CategoryPage shows the albums of a category (id), or those in none ('none'), to search, open,
// play and select.
export function CategoryPage({ id }) {
  const none = id === 'none';
  const rev = useLibRev();
  const cats = useLoad(() => get('/categories'), [], rev);
  const [q, setQ] = useState('');
  const [term, setTerm] = useState('');
  const [sort, setSort] = useState('');
  useEffect(() => {
    const t = setTimeout(() => setTerm(q.trim()), 250);
    return () => clearTimeout(t);
  }, [q]);
  const base = `/albums?limit=${PAGE}&category=${none ? 'none' : id}${term ? '&q=' + encodeURIComponent(term) : ''}${sort ? '&sort=' + sort : ''}`;
  const data = useLoad(() => get(base), [base], rev);
  const key = `${base}:${rev}`;
  const [more, setMore] = useState({ key: null, pages: [], done: false, busy: false });
  const own = more.key === key ? more : { key, pages: [], done: false, busy: false };
  const list = data.data ? [...data.data, ...own.pages.flat()] : null;
  const end = !data.data || data.data.length < PAGE || own.done;
  const loadMore = useCallback(async () => {
    if (own.busy || end) return;
    setMore({ ...own, busy: true });
    try {
      const next = await get(`${base}&offset=${list.length}`);
      setMore((m) => (m.key === key ? { key, pages: [...m.pages, next], done: next.length < PAGE, busy: false } : m));
    } catch (e) {
      report(e);
      setMore((m) => ({ ...m, busy: false }));
    }
  }, [key, list && list.length, own.busy, end]);
  const sel = useSelection('category:' + id + '?' + term + sort);
  const c = !none && cats.data ? cats.data.categories.find((x) => String(x.id) === String(id)) : null;
  if (!none && cats.data && !c) return html`<${Empty} icon="folder">這個分類已經刪除了。<a href=${href('library/categories')}>回到分類</a><//>`;
  const title = none ? '未分類' : c ? c.name : '';
  const count = none ? cats.data && cats.data.uncategorized : c && c.albums;
  const menu = (e) => openMenu(e, [
    { icon: 'edit', label: '改名…', onClick: () => showDialog((close) => html`<${NameCategory} close=${close} category=${c}
      taken=${cats.data.categories.map((x) => x.name)} />`) },
    { icon: 'delete', label: '刪除分類…', onClick: () => showDialog((close) => html`<${DeleteCategory} category=${c} close=${close} />`) },
  ]);
  const takeOut = () => post('/albums/categorize', { albums: sel.keys, remove: [c.id] })
    .then((res) => { if (done(res, `已將 ${sel.count} 張專輯移出「${c.name}」`)) sel.stop(); }, report);
  return html`<section>
    <div class="page-head">
      <div><div class="overline"><a href=${href('library/categories')}>分類</a></div>
        <h1 class="page-title">${title}</h1>${count !== null && count !== undefined && html`<div class="sub">${count} 張專輯</div>`}</div>
      <div class="actions">
        ${list && list.length > 0 && html`<${SelectToggle} sel=${sel} />`}
        ${c && html`<${IconButton} icon="more" label="分類選項" onClick=${menu} />`}
      </div>
    </div>
    <div class="list-tools">
      <input type="search" class="grow" placeholder=${`在「${title}」中搜尋專輯或歌手`} value=${q} onInput=${(e) => setQ(e.target.value)} aria-label="搜尋" />
      <nav class="seg" aria-label="排序">${[['', '名稱'], ['recent', '最近入庫']].map(([k, label]) => html`<button key=${k}
        class=${k === sort ? 'on' : ''} aria-pressed=${k === sort} onClick=${() => setSort(k)}>${label}</button>`)}</nav>
    </div>
    ${data.loading && !data.data ? html`<${Spinner} />` : html`<${ErrorBox} error=${data.error} onRetry=${data.reload} />`}
    ${list && html`<${AlbumGrid} albums=${list} sel=${sel} empty=${term ? '沒有符合的專輯。'
      : none ? '每張專輯都有分類了。' : '這個分類還沒有專輯。在「專輯」分頁選取專輯，按「分類…」放進來。'} />`}
    ${list && !end && html`<div class="actions center"><button class="btn tonal" disabled=${own.busy} onClick=${loadMore}>${own.busy ? '載入中…' : '載入更多'}</button></div>`}
    ${list && html`<${SelectBar} sel=${sel} noun="張" loaded=${list.map((a) => a.id)} items=${list} more=${!end}>
      <${AlbumActions} sel=${sel} />
      ${c && html`<button class="btn tonal" onClick=${takeOut}><${Icon} name="close" />移出「${c.name}」</button>`}
    <//>`}
  </section>`;
}

// DeleteCategory removes a category; its albums stay, and undo brings it back with them.
function DeleteCategory({ category, close }) {
  const [busy, run] = useRunner();
  const submit = () => run(async () => {
    const res = await api('DELETE', '/categories/' + category.id);
    if (done(res, `已刪除分類「${category.name}」`)) {
      close();
      go('library/categories');
    }
  });
  return html`<${Dialog} title=${`刪除分類「${category.name}」`} onClose=${close} actions=${html`
      <button class="btn text" onClick=${close}>取消</button>
      <button class="btn filled danger" disabled=${busy} onClick=${submit}>刪除分類</button>`}>
    <p>只刪除這個分類；裡面的 ${category.albums} 張專輯、歌曲和音檔都不受影響，也仍在其他分類裡。可在修改紀錄撤回。</p>
  <//>`;
}

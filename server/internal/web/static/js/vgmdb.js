// Reading an album out of a VGMdb page the user copied. VGMdb has no API, and it answers requests
// that do not come from a browser with a challenge, so the page comes from the user's own browser:
// select all on the album page, copy, paste here. Browsers copy the HTML of what is shown (only the
// tracklist tab that is open) and plain text; the HTML is read when there is some, else the text.

const months = { jan: 1, feb: 2, mar: 3, apr: 4, may: 5, jun: 6, jul: 7, aug: 8, sep: 9, oct: 10, nov: 11, dec: 12 };
const pad = (n) => String(n).padStart(2, '0');

// "Nov 24, 2005", "Nov 2005", "2005" or "2005-11-24" → YYYY[-MM[-DD]]
export function vgmdbDate(s) {
  s = (s || '').trim();
  let m = s.match(/^(\d{4})(?:[-./](\d{1,2})(?:[-./](\d{1,2}))?)?/);
  if (m) return m[1] + (m[2] ? '-' + pad(m[2]) + (m[3] ? '-' + pad(m[3]) : '') : '');
  m = s.match(/^([A-Za-z]{3})[a-z]*\.?\s+(?:(\d{1,2}),\s*)?(\d{4})/);
  if (m && months[m[1].toLowerCase()]) return m[3] + '-' + pad(months[m[1].toLowerCase()]) + (m[2] ? '-' + pad(m[2]) : '');
  m = s.match(/\b(\d{4})\b/);
  return m ? m[1] : '';
}

// "27:01" or "1:02:03" → milliseconds; 0 when there is no length.
export function vgmdbLength(s) {
  const m = (s || '').trim().match(/^(?:(\d+):)?(\d{1,3}):(\d{2})$/);
  return m ? ((+(m[1] || 0) * 60 + +m[2]) * 60 + +m[3]) * 1000 : 0;
}

const clean = (s) => (s || '').replace(/\s+/g, ' ').trim();

// Lines of an element's text, split at <br>.
function lines(el) {
  const out = [''];
  const walk = (n) => {
    for (const c of n.childNodes) {
      if (c.nodeType === 3) out[out.length - 1] += c.nodeValue;
      else if (c.nodeName === 'BR') out.push('');
      else if (c.nodeType === 1 && !hidden(c)) walk(c);
    }
  };
  walk(el);
  return out.map((l) => clean(l).replace(/^\/\s*/, '')).filter(Boolean);
}

const hidden = (el) => el.style && el.style.display === 'none';

function idFrom(doc, cover) {
  for (const a of doc.querySelectorAll('a[href]')) {
    const m = a.getAttribute('href').match(/albums-discuss\.php\?id=(\d+)|[?&]albumid=(\d+)|vgmdb\.net\/album\/(\d+)/);
    if (m) return +(m[1] || m[2] || m[3]);
  }
  const m = cover.match(/\/albums\/[^/]+\/(\d+)\//);
  return m ? +m[1] : 0;
}

function coverFrom(doc) {
  const art = doc.querySelector('#coverart');
  const style = art ? art.getAttribute('style') || '' : '';
  let m = style.match(/url\(\s*(?:&quot;|["'])?(https:\/\/[a-z-]*media\.vgm\.io\/albums\/[^"')&]+)/);
  if (m) return m[1];
  const a = doc.querySelector('a[href*="media.vgm.io/albums/"]');
  return a ? a.getAttribute('href') : '';
}

function fromDocument(doc) {
  const h1 = doc.querySelector('h1');
  const titles = [];
  if (h1) {
    for (const span of h1.querySelectorAll('span.albumtitle')) if (!hidden(span)) titles.push(...lines(span));
    const sub = h1.nextElementSibling;
    if (sub) for (const span of sub.querySelectorAll('span.albumtitle')) titles.push(...lines(span));
  }
  const info = {};
  for (const table of doc.querySelectorAll('table#album_infobit_large')) {
    if (table.closest('#collapse_credits')) continue;
    for (const tr of table.querySelectorAll('tr')) {
      const cells = tr.querySelectorAll(':scope > td');
      if (cells.length < 2) continue;
      const label = clean(cells[0].textContent);
      info[label] = clean(cells[1].textContent);
      const link = cells[1].querySelector('a[href*="calendar"]');
      if (link) info[label + ' link'] = link.getAttribute('href');
    }
  }
  const credits = [];
  for (const tr of doc.querySelectorAll('#collapse_credits tr.maincred')) {
    const cells = tr.querySelectorAll(':scope > td');
    if (cells.length < 2) continue;
    const role = clean(cells[0].textContent);
    const names = [];
    const others = [];
    for (const n of cells[1].childNodes) {
      if (n.nodeType === 1 && n.nodeName === 'A') {
        const span = [...n.querySelectorAll('span.artistname')].find((s) => !hidden(s));
        names.push(clean((span || n).textContent));
        others.push(clean((span && span.getAttribute('title')) || ''));
      } else {
        for (const part of (n.textContent || '').split(/,(?=\s)|、/)) {
          const name = clean(part).replace(/^[(),\s]+|[(),\s]+$/g, '');
          if (/[\p{L}\p{N}]/u.test(name)) { names.push(name); others.push(''); }
        }
      }
    }
    if (role && names.length) credits.push({ role, names, others });
  }
  const discs = [];
  const tl = [...doc.querySelectorAll('#tracklist span.tl')].find((s) => !hidden(s));
  let language = '';
  if (tl) {
    const tab = doc.querySelector(`a[rel="${tl.id}"]`);
    language = tab ? clean(tab.textContent) : '';
    for (const el of tl.querySelectorAll('b, tr.rolebit')) {
      if (el.nodeName === 'B') {
        if (/^disc\b/i.test(clean(el.textContent))) discs.push({ tracks: [] });
        continue;
      }
      const wide = el.querySelector('td[width="100%"], td[colspan]');
      const time = el.querySelector('span.time');
      if (!discs.length) discs.push({ tracks: [] });
      discs[discs.length - 1].tracks.push({ title: clean(wide ? wide.textContent : ''), length_ms: vgmdbLength(time && time.textContent) });
    }
  }
  const cover = coverFrom(doc);
  const dateLink = (info['Release Date link'] || '').match(/#(\d{4})(\d{2})(\d{2})$/);
  return {
    id: idFrom(doc, cover), titles: [...new Set(titles)], catalog: info['Catalog Number'] || '',
    date: dateLink ? `${dateLink[1]}-${dateLink[2]}-${dateLink[3]}` : vgmdbDate(info['Release Date']),
    cover, credits, language, discs: discs.filter((d) => d.tracks.length),
  };
}

function fromText(text) {
  const ls = text.split(/\r?\n/).map((l) => l.replace(/ /g, ' ').trim());
  const field = (name) => {
    const l = ls.find((x) => x.startsWith(name));
    return l ? clean(l.slice(name.length)) : '';
  };
  const titles = [];
  const at = ls.findIndex((l) => l.startsWith('Catalog Number'));
  for (let i = at - 1; i >= 0 && at > 0; i--) {
    if (!ls[i] || /^(discuss\s*\|\s*edit|edit|customize|submit album)$/i.test(ls[i])) break;
    titles.unshift(ls[i]);
  }
  const credits = [];
  const ci = ls.findIndex((l) => l === 'Credits');
  const ti = ls.findIndex((l) => l === 'Tracklist');
  for (let i = ci + 1; ci >= 0 && i < (ti > ci ? ti : ls.length); i++) {
    const [role, value] = ls[i].split('\t');
    if (role && value) {
      const names = value.split(/,(?=\s)|、/).map(clean).filter(Boolean);
      credits.push({ role: clean(role), names, others: names.map(() => '') });
    }
  }
  const discs = [];
  for (let i = ti + 1; ti >= 0 && i < ls.length; i++) {
    const l = ls[i];
    if (/^(disc length|total length|notes|album stats)\b/i.test(l)) {
      if (!/^disc length/i.test(l)) break;
      continue;
    }
    if (/^disc\s*\d+/i.test(l)) {
      discs.push({ tracks: [] });
      continue;
    }
    const m = l.match(/^(\d{1,3})[\s.]+(.+?)(?:\s+(\d{1,3}:\d{2}(?::\d{2})?))?$/);
    if (m) {
      if (!discs.length) discs.push({ tracks: [] });
      discs[discs.length - 1].tracks.push({ title: clean(m[2]), length_ms: vgmdbLength(m[3]) });
    }
  }
  return { id: 0, titles, catalog: field('Catalog Number'), date: vgmdbDate(field('Release Date')), cover: '', credits, language: '',
    discs: discs.filter((d) => d.tracks.length) };
}

// parseVGMdb reads what was pasted; null when it does not look like a VGMdb album page.
export function parseVGMdb({ html = '', text = '' }) {
  let album = null;
  if (html && typeof DOMParser !== 'undefined') {
    album = fromDocument(new DOMParser().parseFromString(html, 'text/html'));
    if (!album.discs.length && text) album = null;
  }
  if (!album || !album.discs.length) album = text ? fromText(text) : album;
  return album && album.discs.length && album.titles.length ? album : null;
}

// Japanese first: the albums people keep here are mostly Japanese releases.
export const hasKana = (s) => /[぀-ヿ㐀-鿿]/.test(s);

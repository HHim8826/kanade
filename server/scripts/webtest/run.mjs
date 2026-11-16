// The web client in a browser. Builds Kanade, makes a demo library of generated tones (FFmpeg),
// starts the server on it, and checks the pages in Chrome: no script errors, no axe-core
// violations in a light and a dark look, the keyboard, the credits pages working end to end, and a
// page left open offering a new release.
//
//   cd server/scripts/webtest && npm ci && node run.mjs
//
// Needs Go, FFmpeg (FFMPEG, or on the PATH) and Chrome or Chromium (CHROME, or found). Exits 1 on a
// failed check; KEEP=1 leaves the server's files behind, in WORKDIR (else the temporary directory).
import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import puppeteer from 'puppeteer-core';

const here = path.dirname(fileURLToPath(import.meta.url));
const serverDir = path.resolve(here, '../..');
const axeSrc = fs.readFileSync(createRequire(import.meta.url).resolve('axe-core/axe.min.js'), 'utf8');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const tmp = fs.mkdtempSync(path.join(process.env.WORKDIR || os.tmpdir(), 'kanade-webtest-'));
const data = path.join(tmp, 'data');
const ffmpeg = process.env.FFMPEG || 'ffmpeg';
const port = 18000 + Math.floor(Math.random() * 1000);
const base = `http://127.0.0.1:${port}`;
const password = 'webtest-password-0';

let fails = 0;
const ok = (c, m) => {
  console.log((c ? 'ok   ' : 'FAIL ') + m);
  if (!c) fails++;
};

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { stdio: ['pipe', 'pipe', 'inherit'], encoding: 'utf8', ...opts });
  if (r.status !== 0) throw new Error(`${cmd} ${args.slice(0, 3).join(' ')}… exited ${r.status}${r.error ? ': ' + r.error.message : ''}`);
  return r.stdout;
}

function chrome() {
  if (process.env.CHROME) return process.env.CHROME;
  for (const p of ['/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/usr/bin/chromium', '/usr/bin/chromium-browser']) {
    if (fs.existsSync(p)) return p;
  }
  throw new Error('no Chrome or Chromium: set CHROME');
}

// ---- the demo library: made up, as the README's screenshots are ----

const duet = 'Mika (CV: Aoi Hoshino) & Rin (CV: Sora Kudo)';
const albums = [
  { folder: 'Harbor Lights', album: 'Harbor Lights', albumArtist: 'Harbor Unit', cover: '0x5a4fcf', tracks: [
    { title: 'Morning Tide', artist: duet, composer: 'Ken Arai', lyricist: 'Yui Mori', freq: 440 },
    { title: 'Lantern Road', artist: 'Mika (CV: Aoi Hoshino)', composer: 'Ken Arai', freq: 494 },
    { title: 'Morning Tide (Instrumental)', artist: 'Harbor Unit', composer: 'Ken Arai', freq: 523 },
  ] },
  { folder: 'Quiet Rooms', album: 'Quiet Rooms', albumArtist: 'Nao Ito', tracks: [
    { title: 'Paper Window', artist: 'Nao Ito', freq: 330, lrc: true },
    { title: 'Stairwell', artist: 'Nao Ito', freq: 392 },
  ] },
];

function makeLibrary() {
  const src = path.join(data, 'imports', 'Demo');
  for (const a of albums) {
    const dir = path.join(src, a.folder);
    fs.mkdirSync(dir, { recursive: true });
    let cover = null;
    if (a.cover) {
      cover = path.join(tmp, a.folder + '.png');
      run(ffmpeg, ['-v', 'error', '-f', 'lavfi', '-i', `color=c=${a.cover}:s=600x600`, '-frames:v', '1', cover]);
    }
    a.tracks.forEach((t, i) => {
      const file = path.join(dir, `${String(i + 1).padStart(2, '0')} ${t.title}.flac`);
      const meta = { title: t.title, artist: t.artist, album: a.album, album_artist: a.albumArtist, track: String(i + 1),
        composer: t.composer, lyricist: t.lyricist, date: '2026' };
      const args = ['-v', 'error', '-f', 'lavfi', '-i', `sine=frequency=${t.freq}:duration=4`];
      if (cover) args.push('-i', cover, '-map', '0', '-map', '1', '-c:v', 'png', '-disposition:v', 'attached_pic');
      for (const [k, v] of Object.entries(meta)) if (v) args.push('-metadata', `${k}=${v}`);
      run(ffmpeg, [...args, '-c:a', 'flac', file]);
      if (t.lrc) fs.writeFileSync(file.replace(/\.flac$/, '.lrc'), '[00:00.00]Light through paper\n[00:02.00]A window of rain\n');
    });
  }
}

// ---- the server ----

const env = { ...process.env, KANADE_DATA: data, KANADE_FFMPEG: process.env.FFMPEG || '' };
let server;

async function api(method, p, body, token) {
  const r = await fetch(base + '/api/v1' + p, { method, headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: 'Bearer ' + token } : {}) },
    body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await r.text();
  if (!r.ok) throw new Error(`${method} ${p}: ${r.status} ${text}`);
  return text ? JSON.parse(text) : null;
}

async function start() {
  const bin = path.join(tmp, 'kanade');
  run('go', ['build', '-o', bin, './cmd/kanade'], { cwd: serverDir, env: { ...process.env, CGO_ENABLED: '0' } });
  run(bin, ['user', 'add', 'demo'], { env, input: password + '\n' });
  makeLibrary();
  const log = fs.openSync(path.join(tmp, 'server.log'), 'a');
  server = spawn(bin, ['serve', '-listen', `127.0.0.1:${port}`, '-aria2', ''], { env, stdio: ['ignore', log, log] });
  for (let i = 0; ; i++) {
    try {
      if ((await fetch(base + '/app/')).ok) break;
    } catch { /* not yet */ }
    if (i > 100) throw new Error('the server did not start; see ' + path.join(tmp, 'server.log'));
    await sleep(200);
  }
  const { token } = await api('POST', '/login', { username: 'demo', password });
  fs.mkdirSync(path.join(tmp, 'store'));
  await api('POST', '/storage', { kind: 'local', name: 'Library', mode: 'managed', config: { root: path.join(tmp, 'store') } }, token);
  await api('PUT', '/storage/default', { id: '1' }, token);
  const { id } = await api('POST', '/imports', { path: 'Demo' }, token);
  for (let i = 0; ; i++) {
    const b = await api('GET', '/imports/' + id, undefined, token);
    if (b.state === 'done') {
      const published = (b.counts.published || 0) + (b.counts.duplicate || 0);
      if (published !== 5) throw new Error('import: ' + JSON.stringify(b.counts));
      break;
    }
    if (i > 300) throw new Error('import did not finish: ' + JSON.stringify(b));
    await sleep(200);
  }
  return token;
}

// ---- in the browser ----

const errors = [];

async function open() {
  const browser = await puppeteer.launch({ executablePath: chrome(), headless: true, args: ['--no-sandbox', '--autoplay-policy=no-user-gesture-required'] });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 900 });
  await page.goto(base + '/app/'); // the login page: its 401 for the status is expected
  const status = await page.evaluate(async (password) => (await fetch('/api/v1/login', { method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'kanade' },
    body: JSON.stringify({ username: 'demo', password, device: 'webtest', cookie: true }) })).status, password);
  if (status !== 200) throw new Error('login in the browser: ' + status);
  page.on('pageerror', (e) => errors.push(`${page.url()}: ${e.message}`));
  page.on('console', (m) => m.type() === 'error' && errors.push(`${page.url()}: ${m.text()}`));
  return { browser, page };
}

const look = (page, theme, mode) => page.evaluate((t) => localStorage.setItem('kanade.theme', JSON.stringify(t)), { theme, mode });

async function at(page, route) {
  await page.goto(base + '/app/#/' + route);
  await page.reload();
  await page.waitForSelector('main .page > *', { timeout: 10000 });
  await sleep(1200);
}

const texts = (page, sel) => page.$$eval(sel, (els) => els.map((e) => e.textContent.trim()));

// The credits (D13): the names that look like several artists are proposed, divided as confirmed,
// and each has a page; a character and its voice link to each other.
async function credits(page, token) {
  await at(page, 'library/artists');
  ok((await texts(page, '.summary-card .title')).some((t) => /個歌手名稱看起來是幾位歌手/.test(t)), 'the artists tab asks about names to divide');
  await at(page, 'splits');
  const items = await texts(page, '.split-item .split-head .title');
  ok(items.includes(duet) && items.includes('Mika (CV: Aoi Hoshino)'), 'proposed: ' + items.join(' | '));
  const parts = await page.$$eval('.split-item', (els, duet) => {
    const li = els.find((e) => e.querySelector('.split-head .title').textContent.trim() === duet);
    return [...li.querySelectorAll('.split-parts input')].map((i) => i.value);
  }, duet);
  ok(parts.join(',') === 'Mika,Aoi Hoshino,Rin,Sora Kudo', 'the duet divides into ' + parts.join(','));
  await page.$$eval('.split-item', (els, duet) => {
    const li = els.find((e) => e.querySelector('.split-head .title').textContent.trim() === duet);
    [...li.querySelectorAll('button')].find((b) => b.textContent.trim() === '拆分').click();
  }, duet);
  await sleep(1200);
  const track = (await api('GET', '/tracks?limit=50', undefined, token)).find((t) => t.title === 'Morning Tide');
  const detail = await api('GET', '/tracks/' + track.id, undefined, token);
  ok(detail.composer === 'Ken Arai' && detail.lyricist === 'Yui Mori', `the tags' composer and lyricist: ${detail.composer} / ${detail.lyricist}`);
  const said = detail.credits.map((c) => c.role + ':' + c.name + (c.credited_as ? '@' + c.credited_as : '')).join(' ');
  ok(said === 'artist:Mika artist:Rin voice:Aoi Hoshino@Mika voice:Sora Kudo@Rin composer:Ken Arai lyricist:Yui Mori', 'credits: ' + said);
  await at(page, 'splits');
  const left = await texts(page, '.split-item .split-head .title');
  ok(left.length === 1 && left[0] === 'Mika (CV: Aoi Hoshino)', 'left to confirm: ' + left.join(' | '));
  ok((await texts(page, '.seg button')).some((t) => t === '已確認（1）'), 'one confirmed');
  // Only one name left: nothing to divide at once.
  ok(!(await texts(page, '.page-head button')).includes('全部照建議拆分'), 'no "divide all" for one name');
  await page.evaluate(() => [...document.querySelectorAll('.split-item button')].find((b) => b.textContent.trim() === '拆分').click());
  await sleep(1200);
  ok((await texts(page, '.empty, .split-item .title')).some((t) => t.includes('沒有看起來是多位歌手的名稱')), 'nothing left to confirm');
  const artists = await api('GET', '/artists?limit=50', undefined, token);
  const byName = Object.fromEntries(artists.map((a) => [a.name, a]));
  ok(byName.Mika && byName.Mika.kind === 'character' && byName.Mika.tracks === 2 && !byName['Mika (CV: Aoi Hoshino)'],
    'artists: ' + artists.map((a) => `${a.name}/${a.kind || '-'}/${a.tracks}`).join(', '));

  await at(page, `artist/${byName['Aoi Hoshino'].id}`);
  const heads = await texts(page, 'main .section-title');
  const tags = await texts(page, 'main .track-tag');
  ok(heads.join(',') === '配音演唱' && tags.length === 2 && tags.every((t) => t === 'Mika'), `voice page: ${heads} / ${tags}`);
  ok((await texts(page, '.alias-line')).some((t) => t === '飾演：Mika'), 'voice page links the character');
  await at(page, `artist/${byName.Mika.id}`);
  ok((await texts(page, '.overline')).includes('角色') && (await texts(page, '.alias-line')).includes('聲優：Aoi Hoshino'), 'character page');
  const composer = (await api('GET', '/search?q=Ken', undefined, token)).artists[0];
  await at(page, `artist/${composer.id}`);
  ok((await texts(page, 'main .section-title')).join(',') === '作曲' && (await page.$$('main .tracks li')).length === 3, 'composer page: three songs composed');
}

// Shuffling leaves the instrumental out by default (review: variants).
async function shuffle(page, token) {
  const picks = await api('GET', '/tracks/random?n=50&kind=all&plain=1', undefined, token);
  ok(picks.length === 4 && !picks.some((t) => t.title.includes('Instrumental')), 'plain shuffle: ' + picks.map((t) => t.title).join(', '));
  await at(page, 'settings');
  const on = await page.evaluate(() => {
    const l = [...document.querySelectorAll('label')].find((x) => x.textContent.includes('隨機播放時略過伴奏與短版'));
    return l && l.querySelector('input').checked;
  });
  ok(on === true, 'settings: shuffling skips instrumentals, on by default');
}

// A page left open offers a new release when it is looked at again, and only a new one; a backup
// place the server refuses is not shown as chosen.
async function leftOpen(page) {
  const lookAgain = () => page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await at(page, 'library/albums');
  await lookAgain();
  await sleep(1000);
  ok(!(await page.$('.update-bar')), 'the release the page runs is not offered');
  await at(page, 'library/albums'); // a page loaded anew asks again at once
  await page.evaluate(() => {
    const real = window.fetch;
    window.fetch = (u, o) => (String(u).endsWith('/app/version') ? Promise.resolve(new Response('v/000000000000')) : real(u, o));
  });
  await lookAgain();
  await page.waitForSelector('.update-bar', { timeout: 5000 }).catch(() => {});
  ok((await texts(page, '.update-bar span')).some((t) => t.includes('Kanade 已更新')), 'a new release is offered');
  await page.evaluate(axeSrc);
  const v = await page.evaluate(async () => (await axe.run('.update-bar', { runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa'] } })).violations.map((x) => x.id));
  ok(v.length === 0, 'the offer passes axe-core ' + v.join(','));

  await at(page, 'settings');
  const backupTo = () => page.evaluate(() => {
    const s = [...document.querySelectorAll('label.field')].find((l) => l.textContent.includes('備份存到'))?.querySelector('select');
    return s ? { value: s.value, options: [...s.options].map((o) => o.value), disabled: s.disabled } : null;
  });
  const before = await backupTo();
  ok(before && before.value === 'drive' && before.options.length === 2, 'backups to Drive, or the managed location: ' + JSON.stringify(before));
  await page.evaluate((to) => {
    const real = window.fetch;
    window.fetch = (u, o) => (String(u).endsWith('/backup/place') ? Promise.resolve(new Response(JSON.stringify({ error: 'refused for the test' }),
      { status: 400, headers: { 'Content-Type': 'application/json' } })) : real(u, o));
    const s = [...document.querySelectorAll('label.field')].find((l) => l.textContent.includes('備份存到')).querySelector('select');
    s.value = to;
    s.dispatchEvent(new Event('change', { bubbles: true }));
  }, before.options[1]);
  await sleep(800);
  const after = await backupTo();
  ok(after.value === 'drive' && !after.disabled, 'a refused place goes back to Drive: ' + JSON.stringify(after));
  ok((await texts(page, '.toast')).some((t) => t.includes('refused for the test')), 'and says why');
}

// Keyboard and focus (review #265–#271).
async function keyboard(page) {
  await look(page, 'violet', 'light');
  await at(page, 'settings');
  const g = await page.$$eval('[role=radiogroup][aria-label="配色"] [role=radio]', (r) => r.map((b) => [b.tabIndex, b.getAttribute('aria-checked')]));
  ok(g.length > 1 && g.filter((x) => x[0] === 0).length === 1 && g.find((x) => x[0] === 0)[1] === 'true', '配色: one tab stop, on the chosen one');
  await page.focus('[role=radiogroup][aria-label="配色"] [role=radio][tabindex="0"]');
  const before = await page.evaluate(() => document.activeElement.textContent.trim());
  await page.keyboard.press('ArrowRight');
  await sleep(200);
  const after = await page.evaluate(() => [document.activeElement.textContent.trim(), document.activeElement.getAttribute('aria-checked')]);
  ok(after[0] !== before && after[1] === 'true', `ArrowRight chooses the next: ${before} → ${after[0]}`);
  await page.keyboard.press('Tab');
  await sleep(100);
  ok(await page.evaluate(() => document.activeElement.closest('[role=radiogroup]')?.getAttribute('aria-label') === '明暗'), 'Tab goes on to 明暗');
  const stops = await page.evaluate(() => [...document.querySelectorAll('[role=radiogroup]')].filter((x) => x.querySelector('button[role=radio]'))
    .map((x) => x.querySelectorAll('[role=radio][tabindex="0"]').length));
  ok(stops.length > 0 && stops.every((n) => n === 1), 'each radio group is one tab stop: ' + stops.join(','));
  const deco = await page.$$eval('.link', (l) => l.map((a) => getComputedStyle(a).textDecorationLine));
  ok(deco.length > 0 && deco.every((d) => d === 'underline'), 'links in sentences are underlined');
  await look(page, 'violet', 'light');

  await at(page, 'search');
  await page.focus('.search-field input');
  await sleep(100);
  ok(await page.evaluate(() => getComputedStyle(document.querySelector('.search-field')).outlineStyle === 'solid'), 'the search field shows focus');

  await at(page, 'me/history');
  const tabs = await page.evaluate(() => { const n = document.querySelector('nav.tabs'); return [n.getAttribute('role'), [...n.querySelectorAll('a')].filter((a) => a.getAttribute('aria-current') === 'page').length]; });
  ok(tabs[0] === null && tabs[1] === 1, 'my tabs are links, the current one marked');

  await at(page, 'me/stats');
  const heat = await page.evaluate(() => { const h = document.querySelector('.heat'); return h && [h.getAttribute('role'), h.querySelectorAll('button[tabindex="0"]').length]; });
  ok(heat && heat[0] === 'group' && heat[1] === 1, 'the heat map is one tab stop');
  await page.focus('.heat button[tabindex="0"]');
  const d0 = await page.evaluate(() => document.activeElement.dataset.date);
  await page.keyboard.press('ArrowUp');
  await sleep(200);
  const d1 = await page.evaluate(() => document.activeElement.dataset.date);
  ok(d0 && d1 && d1 < d0, `ArrowUp in the heat map: ${d0} → ${d1}`);

  await at(page, 'library/albums');
  const ph = await page.$$eval('.cover.placeholder', (p) => p.map((x) => x.getAttribute('aria-hidden')));
  ok(ph.length > 0 && ph.every((x) => x === 'true'), 'cover placeholders are hidden from screen readers');
}

const routes = ['', 'library/albums', 'library/artists', 'library/tracks', '@album', '@artist', 'splits', 'search?q=Tide', 'me/playlists',
  'me/stats', 'me/history', 'settings', 'feeds', 'works', 'edits', 'tasks', '@np'];

async function accessibility(page, token) {
  const firstAlbum = (await api('GET', '/albums?limit=10', undefined, token)).find((a) => a.title === 'Quiet Rooms');
  const voice = (await api('GET', '/artists?limit=50', undefined, token)).find((a) => a.name === 'Aoi Hoshino');
  const found = {};
  for (const [theme, mode] of [['violet', 'light'], ['rose', 'dark']]) {
    await look(page, theme, mode);
    for (const r of routes) {
      if (r === '@np') { // the full-screen player, on the lyrics
        await at(page, 'album/' + firstAlbum.id);
        await page.evaluate(() => [...document.querySelectorAll('.album-info .actions button')].find((b) => b.textContent.includes('播放')).click());
        await sleep(1200);
        await page.evaluate(() => document.querySelector('.player-bar .now')?.click());
        await sleep(800);
        await page.evaluate(() => [...document.querySelectorAll('.np-panel .tabs button')].find((b) => b.textContent === '歌詞')?.click());
        await sleep(1200);
      } else {
        await at(page, r === '@album' ? 'album/' + firstAlbum.id : r === '@artist' ? 'artist/' + voice.id : r);
      }
      await page.evaluate(axeSrc);
      const res = await page.evaluate(async () => (await axe.run(document, { runOnly: { type: 'tag',
        values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'] } }))
        .violations.map((v) => ({ id: v.id, n: v.nodes.length, eg: v.nodes[0].target.join(' ') })));
      for (const v of res) (found[v.id] ||= []).push(`${theme}-${mode} ${r || 'home'} (${v.n}, e.g. ${v.eg})`);
    }
  }
  const ids = Object.keys(found);
  ok(ids.length === 0, `axe-core on ${routes.length} pages in 2 looks` + ids.map((k) => `\n       ${k}: ${found[k].slice(0, 4).join('; ')}`).join(''));
}

let browser;
try {
  const token = await start();
  const o = await open();
  browser = o.browser;
  await credits(o.page, token);
  await shuffle(o.page, token);
  await leftOpen(o.page);
  await keyboard(o.page);
  await accessibility(o.page, token);
  const seen = [...new Set(errors)];
  ok(seen.length === 0, 'no script errors' + seen.slice(0, 10).map((e) => '\n       ' + e).join(''));
} catch (e) {
  console.error(e);
  fails++;
} finally {
  if (browser) await browser.close();
  if (server) server.kill();
  if (fails && fs.existsSync(path.join(tmp, 'server.log'))) console.log('server log: ' + path.join(tmp, 'server.log'));
  if (!process.env.KEEP && !fails) fs.rmSync(tmp, { recursive: true, force: true });
}
console.log(fails ? `${fails} failed` : 'all ok');
process.exit(fails ? 1 : 0);

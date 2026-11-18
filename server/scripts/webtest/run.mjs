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

let demoBatch = 0;

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
  demoBatch = id;
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
  ok(!(await texts(page, 'main button')).includes('全部照建議拆分'), 'no "divide all" for one name');
  await page.evaluate(() => [...document.querySelectorAll('.split-item button')].find((b) => b.textContent.trim() === '拆分').click());
  await sleep(1200);
  ok((await texts(page, '.empty, .split-item .title')).some((t) => t.includes('沒有看起來是多位歌手的名稱')), 'nothing left to confirm');
  const artists = await api('GET', '/artists?limit=50', undefined, token);
  const byName = Object.fromEntries(artists.map((a) => [a.name, a]));
  ok(byName.Mika && byName.Mika.kind === 'character' && byName.Mika.tracks === 2 && !byName['Mika (CV: Aoi Hoshino)'],
    'artists: ' + artists.map((a) => `${a.name}/${a.kind || '-'}/${a.tracks}`).join(', '));

  await at(page, `artist/${byName['Aoi Hoshino'].id}`);
  const heads = (await texts(page, 'main .section-title')).filter((t) => t !== '專輯'); // the albums sung on, when several
  const tags = await texts(page, 'main .track-tag');
  ok(heads.join(',') === '配音演唱' && tags.length === 2 && tags.every((t) => t === 'Mika'), `voice page: ${heads} / ${tags}`);
  ok((await texts(page, '.alias-line')).some((t) => t === '飾演：Mika'), 'voice page links the character');
  await at(page, `artist/${byName.Mika.id}`);
  ok((await texts(page, '.alias-line')).includes('角色 · 聲優：Aoi Hoshino'), 'character page');
  const composer = (await api('GET', '/search?q=Ken', undefined, token)).artists[0];
  await at(page, `artist/${composer.id}`);
  ok((await texts(page, 'main .section-title')).filter((t) => t !== '專輯').join(',') === '作曲' && (await page.$$('main .tracks li')).length === 3,
    'composer page: three songs composed');
}

// Shuffling leaves the instrumental out by default (review: variants).
async function shuffle(page, token) {
  const picks = await api('GET', '/tracks/random?n=50&kind=all&plain=1', undefined, token);
  ok(picks.length === 4 && !picks.some((t) => t.title.includes('Instrumental')), 'plain shuffle: ' + picks.map((t) => t.title).join(', '));
  await at(page, 'settings?s=playback');
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

  await at(page, 'settings?s=storage');
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

// The flows of the UI review (2026-11-16): back to a page where it was left, a phone's album page
// and player bar, a dialog with something typed in it, the menus, and a finished import.
async function flows(page, token) {
  const content = () => page.evaluate(() => document.getElementById('content').scrollTop);
  await page.setViewport({ width: 390, height: 700, isMobile: true, hasTouch: true });
  await at(page, 'settings?s=playback');
  await page.evaluate(() => document.getElementById('content').scrollTo(0, 1500));
  await sleep(500);
  const y0 = await content();
  await page.evaluate(() => { location.hash = '#/tasks'; });
  await sleep(800);
  ok(await content() === 0, 'another page starts at the top');
  await page.evaluate(() => history.back());
  await sleep(2000);
  const y1 = await content();
  // Sections that load late above it move it a little (the browser keeps what was in view).
  ok(y0 >= 1000 && Math.abs(y1 - y0) <= 80, `back to settings: where it was (${y0} → ${y1})`);
  ok((await page.$$eval('.nav a', (as) => as.filter((a) => a.offsetParent !== null).length)) === 5, 'five places at the bottom of a phone');

  const album = (await api('GET', '/albums?limit=10', undefined, token)).find((a) => a.title === 'Quiet Rooms');
  await at(page, 'album/' + album.id);
  const first = await page.$eval('main .tracks li', (li) => li.getBoundingClientRect().top);
  ok(first < 360, 'the first song of an album on a phone is at ' + Math.round(first));
  const title = await page.evaluate(() => document.title);
  ok(title.startsWith('Quiet Rooms · '), 'the tab names the album: ' + title);
  await page.evaluate(() => [...document.querySelectorAll('.album-info .actions button')].find((b) => b.textContent.includes('播放')).click());
  await sleep(1200);
  const room = await page.$eval('.player-bar .now', (n) => n.getBoundingClientRect().width);
  ok(room >= 200, 'the player bar on a phone leaves the title ' + Math.round(room) + 'px');
  // Songs put in by hand, and another list played: the queue can come back (review 2.10).
  await page.evaluate(() => document.querySelectorAll('main .tracks .row-more')[1].click());
  await sleep(300);
  await page.evaluate(() => [...document.querySelectorAll('.menu button')].find((b) => b.textContent.includes('加入佇列')).click());
  await sleep(300);
  await page.evaluate(() => document.querySelectorAll('main .tracks .track-main')[0].click());
  await sleep(800);
  ok((await texts(page, '.toast')).some((t) => t.includes('復原')), 'playing over songs put in by hand offers to undo');

  // A dialog: tapped around with nothing typed it closes; with something typed it stays (also on a
  // back gesture, or an Esc that ends a composed word), and Esc closes it.
  await at(page, 'me/playlists');
  const newList = () => page.evaluate(() => [...document.querySelectorAll('main button')].find((b) => b.textContent.includes('新增歌單')).click());
  await newList();
  await page.waitForSelector('.dialog input');
  await page.mouse.click(5, 5);
  await sleep(400);
  ok(!(await page.$('.dialog')), 'a dialog with nothing typed closes when tapped around');
  await newList();
  await page.waitForSelector('.dialog input');
  await page.type('.dialog input', 'Rainy');
  await page.mouse.click(5, 5);
  await sleep(400);
  ok(await page.$('.dialog') && (await page.$eval('.dialog input', (i) => i.value)) === 'Rainy', 'with something typed it stays');
  ok((await texts(page, '.toast')).some((t) => t.includes('沒有關閉')), 'and says why');
  await page.evaluate(() => document.querySelector('.dialog input').dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', isComposing: true, bubbles: true })));
  await page.evaluate(() => history.back());
  await sleep(500);
  ok(!!(await page.$('.dialog')), 'nor does Esc ending a composed word or going back close it');
  await page.keyboard.press('Escape');
  await sleep(400);
  ok(!(await page.$('.dialog')), 'Esc closes it');
  // Rules changed with a smart playlist's own buttons, nothing typed: it stays too (#281).
  const newSmart = async () => {
    await page.evaluate(() => [...document.querySelectorAll('main button')].find((b) => b.textContent.includes('新增智慧歌單')).click());
    await page.waitForSelector('.dialog .rule-match');
  };
  await newSmart();
  await page.mouse.click(5, 5);
  await sleep(400);
  ok(!(await page.$('.dialog')), 'a smart playlist left as it opened closes when tapped around');
  await newSmart();
  await page.evaluate(() => [...document.querySelectorAll('.dialog button')].find((b) => b.textContent.includes('新增條件')).click());
  await sleep(100);
  await page.evaluate(() => [...document.querySelectorAll('.dialog .rule-match button')].find((b) => b.textContent.includes('任一條件')).click());
  await sleep(200);
  await page.mouse.click(5, 5);
  await sleep(400);
  await page.evaluate(() => history.back());
  await sleep(500);
  ok(await page.$('.dialog') && (await page.$$('.dialog .rules li')).length === 2 && (await texts(page, '.dialog .rule-match .on')).join() === '任一條件',
    'its rules changed with buttons only, it stays when tapped around or gone back from');
  await page.keyboard.press('Escape');
  await sleep(400);

  await at(page, 'import/' + demoBatch);
  const said = await texts(page, '.summary-line');
  ok(said.some((t) => t.startsWith('已入庫 5 首')), 'a finished import says what it did: ' + said.join(' | '));

  await page.setViewport({ width: 1280, height: 900 });
  await at(page, 'album/' + album.id);
  ok((await page.$$eval('.nav a', (as) => as.filter((a) => a.offsetParent !== null).length)) === 6, 'settings is on the rail of a wide screen');
  const box = await (await page.$('main .tracks .track-main')).boundingBox();
  await page.mouse.click(box.x + 60, box.y + 10, { button: 'right' });
  await sleep(300);
  const items = await texts(page, '.menu [role=menuitem]');
  ok(items.includes('前往歌手') && (await page.$$('.menu .menu-line')).length > 0, 'a right click opens the song menu: ' + items.join(','));
  await page.keyboard.press('Escape');
  await page.evaluate(() => document.querySelector('.album-info .actions [aria-label="更多"]').click());
  await sleep(300);
  const last = await page.$$eval('.menu [role=menuitem]', (b) => ({ text: b.at(-1).textContent.trim(), danger: b.at(-1).classList.contains('danger') }));
  ok(last.text === '移除專輯…' && last.danger, 'the album menu ends with removing it, marked: ' + JSON.stringify(last));
  await page.keyboard.press('Escape');
}

// The second round of the UI review (2.2, 2.8, 2.9, 2.11) and one artist under two names (D13).
async function second(page, token) {
  await page.setViewport({ width: 1280, height: 900 });
  // Settings: the sections, one's own and the server's.
  await at(page, 'settings');
  ok((await texts(page, 'main .section-title')).join(',') === '我的偏好,伺服器管理' && (await page.$$('.settings-list a')).length === 6,
    'settings lists its sections: ' + (await texts(page, 'main .section-title')).join(','));
  await page.evaluate(() => [...document.querySelectorAll('.settings-list a')].find((a) => a.textContent.includes('播放')).click());
  await sleep(1500);
  ok((await page.evaluate(() => location.hash)) === '#/settings?s=playback' && (await texts(page, 'main h1')).includes('播放'), 'a section is a page of its own');

  // The library: an order kept for the tab, and finding in the list.
  await at(page, 'library/albums');
  const titles = () => texts(page, 'main .album-card .title');
  const recent = (await api('GET', '/albums?limit=50&sort=recent', undefined, token)).map((a) => a.title);
  await page.select('.library-tools select', 'recent');
  await sleep(1200);
  ok((await titles()).join() === recent.join(), 'albums, the latest first: ' + (await titles()).join(', '));
  await at(page, 'library/albums');
  ok((await page.$eval('.library-tools select', (x) => x.value)) === 'recent' && (await titles()).join() === recent.join(), 'the order is kept');
  await page.select('.library-tools select', 'artist');
  await page.type('.library-tools input', 'quiet');
  await sleep(1200);
  ok((await titles()).join() === 'Quiet Rooms' && (await page.evaluate(() => location.hash)).includes('q=quiet'), 'finding in the list: ' + (await titles()).join(', '));

  // Search: / goes there, the best result first, the arrows and Enter, the searches made.
  await at(page, 'library/tracks');
  await page.evaluate(() => localStorage.removeItem('kanade.searches'));
  await page.keyboard.press('/');
  await sleep(800);
  ok((await page.evaluate(() => [location.hash, document.activeElement.type])).join() === '#/search,search', '/ goes to the search, ready to type');
  await page.keyboard.type('Nao Ito');
  await sleep(1200);
  ok((await texts(page, '.best-result .title')).join() === 'Nao Ito', 'the best result is the artist named so');
  ok((await texts(page, '.search-results .section-title')).join(',') === '最佳結果,歌曲,歌手,專輯', 'results in kinds: ' + (await texts(page, '.search-results .section-title')).join(','));
  await page.keyboard.press('ArrowDown');
  ok(await page.evaluate(() => document.activeElement.classList.contains('best-result')), '↓ goes to the first result');
  await page.keyboard.press('ArrowUp');
  ok(await page.evaluate(() => document.activeElement.type === 'search'), '↑ goes back to the box');
  await page.keyboard.press('Enter');
  await sleep(1200);
  ok((await page.evaluate(() => location.hash)).startsWith('#/artist/'), 'Enter opens the first result');
  await at(page, 'search');
  ok((await texts(page, '.past-search .grow')).join() === 'Nao Ito', 'the search made is offered again');

  // Shuffle and repeat are two switches.
  const tracks = await api('GET', '/tracks?limit=10', undefined, token);
  await page.evaluate(() => [...document.querySelectorAll('.past-search .row')][0].click());
  await sleep(1200);
  await page.evaluate(() => document.querySelector('.search-results .track-main').click());
  await sleep(1200);
  const mode = () => page.evaluate(() => JSON.parse(localStorage.getItem('kanade.player')).mode);
  const bar = (label) => page.evaluate((l) => document.querySelector(`.player-bar [aria-label^="${l}"]`).click(), label);
  await bar('隨機播放');
  await sleep(300);
  const shuffleOnly = await mode();
  await bar('循環');
  await sleep(300);
  const both = await mode();
  const pressed = await page.$eval('.player-bar [aria-label="隨機播放"]', (b) => b.getAttribute('aria-pressed'));
  await bar('隨機播放');
  await bar('循環');
  await bar('循環');
  await sleep(300);
  ok([shuffleOnly, both, pressed, await mode()].join() === 'shuffle-off,shuffle,true,order', 'shuffle and repeat: ' + [shuffleOnly, both, pressed, await mode()].join());

  // One artist under two names: proposed, merged, the song's tag as it was, found by either name.
  const stair = tracks.find((t) => t.title === 'Stairwell');
  await api('PATCH', '/tracks/' + stair.id, { artist: 'イトウナオ' }, token);
  const same = await api('GET', '/artists/same', undefined, token);
  ok(same.pairs.length === 1 && same.pairs[0].reason === 'reading' && [same.pairs[0].a.name, same.pairs[0].b.name].sort().join() === 'Nao Ito,イトウナオ',
    'names read alike are proposed: ' + JSON.stringify(same.pairs));
  await at(page, 'organize');
  ok((await texts(page, '.summary-card .title')).some((t) => t.includes('組歌手看起來是同一位')), 'the page to sort out says so');
  await steady(page, token);
  await at(page, 'same');
  await page.evaluate(() => [...document.querySelectorAll('.same-pair button')].find((b) => b.textContent.includes('保留「Nao Ito」')).click());
  await sleep(1500);
  const after = await api('GET', '/artists?limit=50', undefined, token);
  const nao = after.find((a) => a.name === 'Nao Ito');
  ok(nao && nao.tracks === 2 && !after.some((a) => a.name === 'イトウナオ'), 'merged: ' + after.map((a) => a.name + '/' + a.tracks).join(', '));
  ok((await api('GET', '/tracks/' + stair.id, undefined, token)).artist === 'イトウナオ', 'the song says what it said');
  ok((await api('GET', '/search?q=' + encodeURIComponent('イトウ'), undefined, token)).artists.map((a) => a.name).join() === 'Nao Ito', 'and either name finds the artist');
  ok((await texts(page, '.toast')).some((t) => t.includes('已合併')), 'the merge can be undone from its note');

  // Finding the other name: the answer to a search typed before, coming in late, is not shown nor
  // offered to merge (#280).
  await at(page, `artist/${nao.id}`);
  await page.evaluate(() => document.querySelector('main [aria-label="更多"]').click());
  await sleep(300);
  await page.evaluate(() => [...document.querySelectorAll('.menu [role=menuitem]')].find((b) => b.textContent.includes('和其他歌手合併')).click());
  await page.waitForSelector('.dialog input[type=search]');
  await page.evaluate(() => {
    const f = window.fetch;
    window.fetch = (u, o) => (String(u).includes('q=Aoi') ? new Promise((r) => setTimeout(r, 1500)).then(() => f(u, o)) : f(u, o));
    window.fetchAsWas = () => { window.fetch = f; };
  });
  await page.type('.dialog input[type=search]', 'Aoi Hoshino');
  await sleep(500);
  await page.click('.dialog input[type=search]', { clickCount: 3 });
  await page.type('.dialog input[type=search]', 'Nobody Here');
  await page.waitForFunction(() => document.querySelector('.dialog').textContent.includes('找不到這個名字'), { timeout: 5000 });
  await sleep(1800);
  const offered = await texts(page, '.dialog .list .title');
  ok(!offered.length && (await texts(page, '.dialog .hint')).some((t) => t.includes('找不到這個名字')), 'a late answer to an earlier search is not shown: ' + offered.join());
  await page.evaluate(() => window.fetchAsWas());
  await page.keyboard.press('Escape');
  await sleep(300);
}

// Pages that do not jump: every title where the others are, a page under another naming it on the
// title's line where that page has its title, and the pages read in parts or polled showing once,
// whole, or what they showed last time (review 3.1).
async function steady(page, token) {
  const cdp = await page.target().createCDPSession();
  const slow = (latency) => cdp.send('Network.emulateNetworkConditions', { offline: false, latency, downloadThroughput: -1, uploadThroughput: -1 });
  await cdp.send('Network.enable');
  // What the page shows, frame by frame, from going to it (in the app, as a link does) until it settles.
  const watch = (route) => page.evaluate(async (h) => {
    const seen = [];
    location.hash = '#/' + h;
    for (let i = 0; i < 50; i++) {
      await new Promise((r) => requestAnimationFrame(r));
      const s = document.querySelector('main section');
      seen.push(s ? `${s.querySelector('h1')?.textContent}|${[...s.children].map((e) => e.className || e.tagName).join()}|${s.textContent.length}` : '');
      await new Promise((r) => setTimeout(r, 30));
    }
    return seen;
  }, route);
  await at(page, ''); // the home page's answer is kept: the page to sort out has it at once, and waits for the rest
  await slow(300);
  const parts = (await watch('organize')).filter((x) => x.startsWith('待整理|') && (x.includes('summary-card') || x.includes('hint')));
  ok(parts.length > 0 && parts.every((x) => x === parts[0]), 'the page to sort out shows once, whole: ' + [...new Set(parts)].join(' → '));
  await watch('library/albums');
  const back = (await watch('organize')).filter((x) => x.startsWith('待整理|'));
  ok(back[0] === back[back.length - 1] && back[0].includes('summary-card'), 'and at once when gone back to');
  await slow(0);
  await watch('tasks');
  await watch('library/albums');
  await slow(300);
  const tasks = (await watch('tasks')).filter((x) => x.startsWith('任務|'));
  ok(tasks.length > 0 && !tasks[0].includes('spinner') && tasks[0].includes('section-title'), 'the task page shows its list at once when gone back to');
  await slow(0);

  const where = async (route) => {
    await page.evaluate((h) => { location.hash = '#/' + h; }, route);
    await sleep(700);
    return page.evaluate(() => {
      const top = document.querySelector('.content').getBoundingClientRect().top;
      const h1 = document.querySelector('main h1');
      const up = document.querySelector('main .title-path .up');
      return { y: Math.round(h1.getBoundingClientRect().top - top), x: Math.round((up || h1).getBoundingClientRect().left) };
    });
  };
  const mika = (await api('GET', '/search?q=Mika', undefined, token)).artists[0];
  const routes = ['', 'library/albums', 'library/categories', 'me/playlists', 'settings', 'settings?s=playback', 'settings?s=storage', 'organize',
    'same', 'splits', 'edits', 'missing', 'tasks', 'upload', 'feeds', 'artist/' + mika.id];
  for (const width of [1280, 400]) {
    await page.setViewport({ width, height: 900 });
    const at0 = await where('');
    const off = [];
    for (const r of routes) {
      const w = await where(r);
      if (w.y !== at0.y || w.x !== at0.x) off.push(`${r || 'home'} ${w.x},${w.y}`);
    }
    ok(off.length === 0, `every title at ${at0.x},${at0.y} (${width}px)` + (off.length ? ': ' + off.join('; ') : ''));
  }

  // Every page, the first time and again: nothing it shows moves when the rest comes in (the layout
  // shifts the browser counts), and no spinner for a wait it would only flicker in (review 3.2).
  const tracks = await api('GET', '/tracks?limit=50', undefined, token);
  const albums = await api('GET', '/albums?limit=50', undefined, token);
  const pl = await api('POST', '/playlists', { name: 'Steady', items: tracks.map((t) => ({ track_id: t.id })) }, token);
  const cat = await api('POST', '/categories', { name: 'Steady' }, token);
  await api('POST', '/albums/categorize', { albums: albums.map((a) => a.id), add: [cat.id] }, token);
  const still = (route) => page.evaluate(async (h) => {
    const moved = [];
    const take = (list) => {
      for (const e of list) {
        if (e.hadRecentInput || e.value < 0.0005) continue;
        moved.push(`${e.value.toFixed(3)} ` + e.sources.map((x) => `${x.node ? (x.node.className || x.node.tagName) : '?'} ${Math.round(x.previousRect.y)}→${Math.round(x.currentRect.y)}`).join(', '));
      }
    };
    const po = new PerformanceObserver((l) => take(l.getEntries()));
    po.observe({ type: 'layout-shift' });
    let spun = false;
    const t0 = performance.now();
    location.hash = '#/' + h;
    while (performance.now() - t0 < 1500) {
      await new Promise((r) => requestAnimationFrame(r));
      if (performance.now() - t0 < 350 && [...document.querySelectorAll('main .spinner')].some((x) => getComputedStyle(x).visibility !== 'hidden')) spun = true;
    }
    take(po.takeRecords());
    po.disconnect();
    return { moved, spun };
  }, route);
  const sweep = ['', 'library/albums', 'library/artists', 'library/tracks', 'search?q=Tide', 'album/' + albums[0].id, 'artist/' + mika.id,
    'playlist/' + pl.id, 'category/' + cat.id, 'me/history', 'me/stats', 'organize', 'splits', 'same', 'missing', 'edits', 'tasks', 'feeds',
    'settings?s=linked', 'settings?s=playback'];
  for (const width of [1280, 400]) {
    await page.setViewport({ width, height: 900 });
    await at(page, 'settings'); // nothing kept: the first time
    await slow(300);
    const bad = [];
    for (const r of sweep) {
      for (const time of ['first', 'again']) {
        const v = await still(r);
        if (v.moved.length || v.spun) bad.push(`${r || 'home'} ${time}:${v.spun ? ' a spinner flickers' : ''} ${v.moved.join('; ')}`);
        await page.evaluate(() => { location.hash = '#/settings'; });
        await sleep(400);
      }
    }
    await slow(0);
    ok(bad.length === 0, `${sweep.length} pages hold still, the first time and again (${width}px)` + bad.map((x) => '\n       ' + x).join(''));
  }
  await page.setViewport({ width: 1280, height: 900 });
}

// Keyboard and focus (review #265–#271).
async function keyboard(page) {
  await look(page, 'violet', 'light');
  await at(page, 'settings?s=look');
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
  await at(page, 'settings?s=service'); // the link to the releases
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

const routes = ['', 'library/albums', 'library/artists', 'library/tracks', '@album', '@artist', 'splits', 'same', 'search?q=Tide', 'search',
  'me/playlists', 'me/stats', 'me/history', 'settings', 'settings?s=playback', 'settings?s=storage', 'settings?s=look', 'feeds', 'works', 'edits',
  'organize', 'tasks', '@np'];

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
  await flows(o.page, token);
  await second(o.page, token);
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

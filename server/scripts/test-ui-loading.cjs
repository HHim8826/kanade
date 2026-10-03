// Browser regressions for initial rendering, using real modules and controlled API replies.
// Requires Playwright and Chromium. Run: node server/scripts/test-ui-loading.cjs
// CHROMIUM_PATH selects an installed browser; KANADE_UI_ROOT can test another static tree.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require('playwright');

const root = path.resolve(process.env.KANADE_UI_ROOT || path.join(__dirname, '../internal/web/static'));
const nameFilter = process.argv[2] || '';
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; media-src 'self'; connect-src 'self'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'";
const service = { aria2_ready: true, ffmpeg: true, uptime_seconds: 3600, disk: { free_bytes: 1e9, reserve_bytes: 1e8 } };
const connected = { status: { connected: true }, account: { email: 'test@example.com', usage_bytes: 1000, limit_bytes: 10000 } };
const key = { id: 1, name: 'Test device', created_at: 1, last_used_at: 0 };
const headings = ['Google Drive', '與 Drive 同步', 'Drive 收件匣', '外觀', '服務', '帳號'];
let fixture;

function reply(res, status, data) {
  if (res.destroyed) return;
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(data));
}

function fixtures(auth = true) {
  const reads = new Map(), pending = new Map(), aborted = [];
  const routes = new Map([
    ['/status', { data: service }], ['/drive', { data: connected }],
    ['/drive/sync', { data: {} }], ['/passkeys', { data: [key] }],
    ['/passkeys/available', { data: { available: true } }],
  ]);
  const f = {
    auth, reads, pending, aborted,
    hold(p, patch = {}) { routes.set(p, { ...routes.get(p), ...patch, hold: true }); },
    release(p, patch = {}) {
      const route = { ...routes.get(p), ...patch, hold: false };
      routes.set(p, route);
      for (const res of pending.get(p) || []) reply(res, route.status || 200, route.data);
      pending.delete(p);
    },
    handle(req, res, p) {
      reads.set(p, (reads.get(p) || 0) + 1);
      if (p === '/status' && reads.get(p) === 1) return reply(res, f.auth ? 200 : 401, f.auth ? service : { error: 'login required' });
      if (p === '/favorites/ids') return reply(res, 200, { tracks: [], albums: [] });
      if (p === '/home') return reply(res, 200, { tasks: { downloads: {}, importing: 0 }, attention: {}, recently_added: [], recently_played: [], spoken: [] });
      if (p === '/login') {
        let body = '';
        req.on('data', (chunk) => { body += chunk; });
        req.on('end', () => { f.credentials = JSON.parse(body); f.auth = true; reply(res, 200, {}); });
        return;
      }
      if (p === '/drive/reconcile') return reply(res, 200, {});
      const route = routes.get(p);
      assert.ok(route, `unexpected API request: ${req.method} ${p}`);
      if (!route.hold) return reply(res, route.status || 200, route.data);
      pending.set(p, [...(pending.get(p) || []), res]);
      res.on('close', () => { if (!res.writableEnded) aborted.push(p); });
    },
  };
  return f;
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');
  if (url.pathname.startsWith('/api/v1/')) return fixture.handle(req, res, url.pathname.slice(7));
  let file = url.pathname.replace(/^\/app\/(?:v\/[^/]+\/)?/, '') || 'index.html';
  if (!file.includes('.')) file = 'index.html';
  const full = path.resolve(root, file);
  if (!full.startsWith(root + path.sep) || !fs.existsSync(full)) { res.writeHead(404); res.end(); return; }
  let body = fs.readFileSync(full);
  if (file === 'index.html') body = Buffer.from(body.toString().replaceAll('{{V}}', 'v/test'));
  res.writeHead(200, {
    'Content-Type': { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml', '.webmanifest': 'application/manifest+json' }[path.extname(file)] || 'application/octet-stream',
    'Content-Security-Policy': csp,
  });
  res.end(body);
});

async function requested(p, count = 1) {
  const deadline = Date.now() + 5000;
  while ((fixture.reads.get(p) || 0) < count) {
    assert.ok(Date.now() < deadline, `request not started: ${p} #${count}`);
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
}

async function frames(page) {
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
}

async function noSections(page) {
  await frames(page);
  assert.equal(await page.locator('.page h2').count(), 0, 'partial settings must not be revealed');
}

async function ready(page, expected = headings) {
  await page.getByRole('radiogroup', { name: '配色' }).waitFor();
  await frames(page);
  assert.deepEqual(await page.locator('.page h2').allTextContents(), expected);
  const paints = await page.evaluate(() => window.uiPaints);
  assert.ok(paints.every((p) => !p.headings.length || JSON.stringify(p.headings) === JSON.stringify(expected)), 'each settings paint has the complete section order');
}

(async () => {
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  let browser;
  try {
    browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || undefined, headless: true, args: ['--no-sandbox'] });
    const base = `http://127.0.0.1:${server.address().port}/app/`;
    async function test(name, auth, run, init) {
      if (!name.includes(nameFilter)) return;
      fixture = fixtures(auth);
      const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', (e) => errors.push(e.message));
      await page.addInitScript(() => {
        window.uiPaints = [];
        function record() {
          const paint = {
            form: !!document.querySelector('.login-card'),
            passkey: [...document.querySelectorAll('.login-card button')].some((b) => b.textContent.includes('使用 passkey 登入')),
            headings: [...document.querySelectorAll('.page h2')].map((h) => h.textContent),
          };
          const last = window.uiPaints[window.uiPaints.length - 1];
          if (JSON.stringify(last) !== JSON.stringify(paint)) window.uiPaints.push(paint);
          requestAnimationFrame(record);
        }
        requestAnimationFrame(record);
      });
      if (init) await page.addInitScript(init);
      try { await run(page, base); assert.deepEqual(errors, []); console.log(`PASS ${name}`); }
      finally { await context.close(); }
    }

    await test('login resolves methods before showing an interactive form', false, async (page, base) => {
      fixture.hold('/passkeys/available');
      await page.goto(base);
      await requested('/passkeys/available');
      await frames(page);
      assert.equal(await page.locator('.login-card').count(), 0);
      fixture.release('/passkeys/available');
      await page.getByRole('button', { name: '使用 passkey 登入' }).waitFor();
      await frames(page);
      assert.ok((await page.evaluate(() => window.uiPaints)).every((p) => !p.form || p.passkey));
      await page.locator('input[autocomplete="username"]').fill('admin');
      await page.locator('input[type="password"]').fill('secret');
      await page.getByRole('button', { name: '登入', exact: true }).click();
      await page.getByRole('navigation', { name: '主要' }).waitFor();
      assert.equal(fixture.credentials.username, 'admin');
      assert.equal(fixture.credentials.password, 'secret');
    });

    for (const available of [false, 'error']) await test(`login availability ${available} still offers passwords`, false, async (page, base) => {
      fixture.hold('/passkeys/available');
      await page.goto(base);
      await requested('/passkeys/available');
      fixture.release('/passkeys/available', available === 'error' ? { status: 503, data: { error: 'unavailable' } } : { data: { available: false } });
      await page.locator('.login-card').waitFor();
      assert.equal(await page.getByRole('button', { name: '使用 passkey 登入' }).count(), 0);
      assert.equal(await page.locator('input[type="password"]').isVisible(), true);
    });

    await test('unsupported browser skips the availability read', false, async (page, base) => {
      await page.goto(base);
      await page.locator('.login-card').waitFor();
      assert.equal(fixture.reads.get('/passkeys/available') || 0, 0);
    }, () => { window.PublicKeyCredential = undefined; });

    await test('stalled availability aborts and a late response cannot change the form', false, async (page, base) => {
      fixture.hold('/passkeys/available');
      await page.goto(base);
      await page.locator('.login-card').waitFor({ timeout: 5000 });
      assert.equal(await page.getByRole('button', { name: '使用 passkey 登入' }).count(), 0);
      await page.locator('input[autocomplete="username"]').fill('keep this');
      fixture.release('/passkeys/available');
      await frames(page);
      assert.equal(await page.getByRole('button', { name: '使用 passkey 登入' }).count(), 0);
      assert.equal(await page.locator('input[autocomplete="username"]').inputValue(), 'keep this');
      assert.ok(fixture.aborted.includes('/passkeys/available'));
    });

    for (const slow of ['drive', 'passkeys']) await test(`settings keep a single section order with slow ${slow}`, true, async (page, base) => {
      for (const p of ['/status', '/drive', '/drive/sync', '/passkeys']) fixture.hold(p);
      await page.goto(base + '#/settings');
      await requested('/passkeys');
      await requested('/drive');
      await requested('/status', 2);
      fixture.release('/status');
      if (slow === 'drive') fixture.release('/passkeys');
      await noSections(page);
      fixture.release('/drive');
      await requested('/drive/sync');
      await noSections(page);
      fixture.release('/drive/sync');
      if (slow === 'passkeys') { await noSections(page); fixture.release('/passkeys'); }
      await ready(page);
      assert.equal(await page.getByText('Test device', { exact: true }).isVisible(), true);
      assert.equal(await page.getByRole('button', { name: '重新連線' }).count(), 1);
    });

    await test('disconnected Drive has no sync reads or late sections', true, async (page, base) => {
      fixture.release('/drive', { data: { status: { connected: false } } });
      fixture.release('/passkeys', { data: [] });
      await page.goto(base + '#/settings');
      await ready(page, ['Google Drive', '外觀', '服務', '帳號']);
      assert.equal(fixture.reads.get('/drive/sync') || 0, 0);
      assert.equal(await page.getByRole('button', { name: '連線 Google Drive' }).count(), 1);
    });

    await test('failed reads render independently and retries retain the page controls', true, async (page, base) => {
      for (const p of ['/status', '/passkeys', '/drive/sync']) fixture.release(p, { status: 503, data: { error: `${p} unavailable` } });
      await page.goto(base + '#/settings');
      await ready(page);
      assert.equal(await page.getByRole('alert').count(), 3);
      const theme = page.getByRole('radio', { name: '青綠' });
      await theme.click();
      const control = await theme.elementHandle();
      fixture.hold('/status', { status: 200, data: service });
      await page.getByRole('alert').filter({ hasText: '/status unavailable' }).getByRole('button', { name: '重試' }).click();
      await requested('/status', 3);
      assert.equal(await control.evaluate((el) => el.isConnected), true);
      assert.equal(await theme.getAttribute('aria-checked'), 'true');
      fixture.release('/status');
      await page.getByText('下載器（aria2）：運作中', { exact: true }).waitFor();
      assert.equal(await page.getByRole('alert').count(), 2);
    });

    await test('sync polling preserves control identity, selection and focus', true, async (page, base) => {
      fixture.release('/drive/sync', { data: { full_running: true } });
      await page.goto(base + '#/settings');
      await ready(page);
      const theme = page.getByRole('radio', { name: '玫瑰' });
      await theme.click();
      await theme.focus();
      const control = await theme.elementHandle();
      fixture.hold('/drive/sync');
      await requested('/drive/sync', 2);
      assert.equal(await control.evaluate((el) => el.isConnected && document.activeElement === el), true);
      assert.equal(await theme.getAttribute('aria-checked'), 'true');
      fixture.release('/drive/sync', { data: { full_running: false } });
      await page.getByRole('button', { name: '完整對帳' }).waitFor();
      await frames(page);
      assert.equal(await control.evaluate((el) => el.isConnected && document.activeElement === el), true);
    });

    await test('stalled settings read ends in a retryable error', true, async (page, base) => {
      fixture.hold('/passkeys');
      await page.goto(base + '#/settings');
      await page.getByRole('radiogroup', { name: '配色' }).waitFor({ timeout: 12000 });
      await ready(page);
      assert.equal(await page.getByRole('alert').filter({ hasText: '載入逾時，請重試' }).count(), 1);
      fixture.release('/passkeys');
      await page.getByRole('button', { name: '重試' }).click();
      await page.getByText('Test device', { exact: true }).waitFor();
    });
  } finally {
    if (browser) await browser.close();
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  }
})().catch((e) => { console.error(e); process.exitCode = 1; });

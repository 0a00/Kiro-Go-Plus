// Run only against an isolated loopback fixture; no accounts or provider calls.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const base = new URL(process.env.KIRO_UI_TEST_URL || 'http://127.0.0.1:18097');
const password = process.env.KIRO_UI_TEST_PASSWORD;
if (!['127.0.0.1', 'localhost', '[::1]'].includes(base.hostname) || base.protocol !== 'http:' || base.username || base.password || base.search || base.hash || base.pathname !== '/' || !password) throw Error('Use an isolated loopback fixture and KIRO_UI_TEST_PASSWORD');
const out = process.env.KIRO_UI_TEST_REPORT_DIR || fs.mkdtempSync(path.join(os.tmpdir(), 'kiro-ui-e2e-'));
fs.mkdirSync(out, { recursive: true, mode: 0o700 });

async function main() {
  const browser = await chromium.launch({ headless: true, ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const cases = [];
  async function run(name, fn, options = {}) {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: 'zh-CN', ...options });
    const page = await context.newPage();
    // Do not let update checks contact GitHub during a UI test.
    await context.route(/https:\/\//, route => route.abort());
    try { await fn(page, context); cases.push({ name, status: 'PASS' }); console.log('PASS ' + name); }
    finally { await context.close(); }
  }
  const login = async page => {
    await page.goto(base + 'admin/');
    await page.fill('#password', password);
    await page.click('#submit');
    await page.locator('#mainPage').waitFor({ state: 'visible' });
    await page.waitForFunction(() => document.getElementById('footerVersion')?.textContent?.includes('.') || document.getElementById('statAccounts')?.textContent === '0');
  };
  const authenticate = async context => {
    const response = await context.request.post(base + 'admin/api/login', { data: { password } });
    assert.equal(response.status(), 200);
  };
  try {
    for (const [name, width, height, theme] of [['desktop-light', 1280, 900, 'light'], ['mobile-dark', 390, 844, 'dark'], ['narrow-light', 320, 640, 'light'], ['short-dark', 844, 390, 'dark']]) {
      await run(name, async (page, context) => {
        await context.addInitScript(theme => { localStorage.setItem('kiro_theme', theme); localStorage.setItem('kiro_lang', 'zh'); }, theme);
        const failures = []; page.on('pageerror', e => failures.push(e.message));
        await page.goto(base + 'admin/');
        await page.waitForFunction(() => document.fonts.status === 'loaded');
        assert.equal(await page.locator('html').evaluate(el => el.classList.contains('dark')), theme === 'dark');
        assert.equal(await page.locator('#heading').innerText(), '管理登录');
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1), false);
        await page.screenshot({ path: path.join(out, name + '-login.png'), fullPage: true });
        const before = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
        await page.fill('#password', password);
        await page.click('#passwordToggle'); assert.equal(await page.locator('#password').getAttribute('type'), 'text');
        await page.check('#remember'); await page.click('#submit');
        await page.locator('#mainPage').waitFor({ state: 'visible' });
        assert.equal(await page.locator('#loginPage').count(), 0);
        assert.equal(await page.evaluate(() => getComputedStyle(document.body).backgroundColor), before);
        const cookie = (await context.cookies()).find(x => x.name === 'kiro_admin_session');
        assert.ok(cookie?.httpOnly && cookie.expires > 0);
        assert.equal((await context.request.get(base + 'admin/app.js')).status(), 200);
        await page.screenshot({ path: path.join(out, name + '-console.png'), fullPage: true });
        await page.click('#logoutBtn'); await page.locator('#loginForm').waitFor();
        assert.equal((await context.request.get(base + 'admin/app.js')).status(), 404);
        assert.deepEqual(failures, []);
      }, { viewport: { width, height }, colorScheme: theme === 'light' ? 'dark' : 'light' });
    }
    await run('slow-locales-no-second-login', async (page, context) => {
      let release, seen; const gate = new Promise(r => { release = r; }), arrived = new Promise(r => { seen = r; });
      await page.route('**/admin/locales/*.json', async route => { seen(); await gate; await route.continue(); });
      await authenticate(context); await page.goto(base + 'admin/'); await arrived;
      assert.equal(await page.locator('#mainPage').isVisible(), false);
      assert.equal(await page.locator('input[type=password]:visible').count(), 0);
      assert.equal(await page.locator('#loginPage').count(), 0);
      assert.equal(await page.locator('#bootScreen').isVisible(), true);
      await page.screenshot({ path: path.join(out, 'slow-start.png') });
      release(); await page.locator('#mainPage').waitFor({ state: 'visible' });
    });
    await run('slow-script-loading-visible', async (page, context) => {
      await authenticate(context);
      let release; const gate = new Promise(r => { release = r; });
      await page.route('**/admin/vendor/tailwindcss-browser/index.global.js', async route => { await gate; await route.continue(); });
      await page.goto(base + 'admin/', { waitUntil: 'commit' });
      await page.locator('#bootScreen').waitFor({ state: 'visible' });
      assert.equal(await page.locator('#mainPage').isVisible(), false);
      release(); await page.locator('#mainPage').waitFor({ state: 'visible' });
    });
    for (const resource of ['locales/zh.json', 'app.js', 'api/version', 'api/status']) {
      await run('failed-' + resource.replaceAll('/', '-'), async (page, context) => {
        await authenticate(context);
        await page.route('**/admin/' + resource, route => route.fulfill({ status: 503, contentType: 'text/plain', body: 'fixture unavailable' }));
        await page.goto(base + 'admin/');
        await page.waitForFunction(() => document.getElementById('bootScreen')?.dataset.state === 'error');
        assert.equal(await page.locator('input[type=password]:visible').count(), 0);
        assert.equal(await page.locator('#loginPage').count(), 0);
        assert.equal(new URL(page.url()).pathname, '/admin/');
        await page.unroute('**/admin/' + resource); await page.click('#bootRetry');
        await page.locator('#mainPage').waitFor({ state: 'visible' });
      });
    }
    await run('session-expired-during-init', async (page, context) => {
      await authenticate(context);
      await page.route('**/admin/api/version', route => route.fulfill({ status: 401, json: { error: 'Unauthorized' } }));
      await page.goto(base + 'admin/'); await page.locator('#loginForm').waitFor();
      assert.equal(new URL(page.url()).pathname, '/admin/login.html');
      assert.match(await page.locator('#error').innerText(), /过期/);
    });
    await run('session-expired-after-ready', async page => {
      await login(page);
      await page.route('**/admin/api/requests?*', route => route.fulfill({ status: 401, json: { error: 'Unauthorized' } }));
      await page.click('[data-tab=requests]'); await page.locator('#loginForm').waitFor();
      assert.equal(new URL(page.url()).pathname, '/admin/login.html');
    });
    await run('blocked-storage-language-and-theme', async (page, context) => {
      await context.addInitScript(() => { for (const name of ['localStorage', 'sessionStorage']) Object.defineProperty(window, name, { get() { throw new DOMException('blocked'); } }); });
      await page.goto(base + 'admin/'); await page.click('[data-lang=en]');
      assert.equal(await page.locator('#heading').innerText(), 'Sign In');
      await page.click('#themeToggle');
      await page.fill('#password', password); await page.click('#submit');
      await page.locator('#mainPage').waitFor({ state: 'visible' });
    });
    await run('cookies-blocked-no-navigation-loop', async page => {
      await page.route('**/admin/api/login', route => route.fulfill({ status: 200, json: { success: true } }));
      await page.goto(base + 'admin/'); await page.fill('#password', password); await page.click('#submit');
      await page.waitForFunction(() => document.getElementById('error')?.textContent.includes('Cookie'));
      assert.equal(new URL(page.url()).pathname, '/admin/');
      assert.equal(await page.locator('#submit').isDisabled(), false);
    });
    await run('wrong-password-and-single-submit', async page => {
      let calls = 0, release;
      const gate = new Promise(r => { release = r; });
      await page.route('**/admin/api/login', async route => { calls++; await gate; await route.fulfill({ status: 401, json: { error: 'Unauthorized' } }); });
      await page.goto(base + 'admin/'); await page.fill('#password', 'wrong-fixture'); await page.click('#submit');
      await page.locator('#password').press('Enter'); assert.equal(await page.locator('#submit').isDisabled(), true);
      release(); await page.waitForFunction(() => document.getElementById('error')?.textContent === '密码错误');
      assert.equal(calls, 1); assert.equal(await page.locator('#password').inputValue(), '');
    });
    await run('login-network-failure-and-preferences', async page => {
      await page.goto(base + 'admin/'); await page.click('[data-lang=en]');
      await page.click('#themeToggle');
      await page.route('**/admin/api/login', route => route.abort());
      await page.fill('#password', password); await page.click('#submit');
      await page.waitForFunction(() => document.getElementById('error')?.textContent.includes('Unable'));
      assert.equal(await page.locator('#submit').isDisabled(), false);
      await page.unroute('**/admin/api/login');
      await page.fill('#password', password); await page.click('#submit');
      await page.locator('#mainPage').waitFor({ state: 'visible' });
      assert.equal(await page.locator('html').getAttribute('lang'), 'en');
      assert.equal(await page.locator('html').getAttribute('data-theme-pref'), 'light');
      await page.click('#logoutBtn'); await page.locator('#loginForm').waitFor();
      assert.equal(await page.locator('#heading').innerText(), 'Sign In');
      assert.equal(await page.locator('html').getAttribute('data-theme-pref'), 'light');
    });
    await run('timeouts-offer-retry', async (page, context) => {
      await authenticate(context);
      await context.addInitScript(() => { const timer = window.setTimeout; window.setTimeout = (f, ms, ...args) => timer(f, ms === 10000 || ms === 20000 ? 500 : ms, ...args); });
      let release; const gate = new Promise(r => { release = r; });
      await page.route('**/admin/locales/*.json', async route => { await gate; await route.abort(); });
      await page.goto(base + 'admin/');
      await page.waitForFunction(() => document.getElementById('bootScreen')?.dataset.state === 'error');
      assert.equal(await page.locator('#bootRetry').isVisible(), true);
      release();
    });
  } finally {
    fs.writeFileSync(path.join(out, 'summary.json'), JSON.stringify(cases, null, 2), { mode: 0o600 });
    await browser.close();
  }
  console.log('Reports: ' + out);
}
main().catch(e => { console.error(e.message); process.exitCode = 1; });

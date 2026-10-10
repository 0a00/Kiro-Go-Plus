const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const root = path.join(__dirname, '..', 'web');
const source = name => fs.readFileSync(path.join(root, name), 'utf8');

function appearance(saved = {}, dark = false, blocked = false) {
  const values = new Map(Object.entries(saved)), classes = new Set(), events = {};
  const media = { matches: dark, addEventListener: (name, cb) => { events[name] = cb; } };
  const storage = { getItem: k => { if (blocked) throw Error('denied'); return values.get(k) ?? null; },
    setItem: (k, v) => { if (blocked) throw Error('denied'); values.set(k, v); }, removeItem: k => values.delete(k) };
  const window = { dispatchEvent() {} }, html = { dataset: {}, classList: { toggle(k, active) { active ? classes.add(k) : classes.delete(k); } } };
  vm.runInNewContext(source('appearance.js'), { window, localStorage: storage, sessionStorage: storage,
    document: { documentElement: html }, navigator: { language: 'zh-CN' }, matchMedia: () => media, Event });
  return { api: window.AdminAppearance, values, html, classes, media, events };
}

test('shared appearance applies saved preference before rendering and rejects invalid values', () => {
  const s = appearance({ kiro_theme: 'light', kiro_lang: 'en', admin_password: 'fixture', kiro_remembered_pwd: 'fixture' }, true);
  assert.equal(s.classes.has('dark'), false);
  assert.equal(s.html.lang, 'en'); assert.equal(s.values.has('admin_password'), false);
  assert.equal(s.values.has('kiro_remembered_pwd'), false);
  s.api.setTheme('dark'); assert.equal(s.classes.has('dark'), true);
  s.api.setTheme('system'); s.media.matches = false; s.events.change(); assert.equal(s.classes.has('dark'), false);
  s.api.setTheme('bad'); assert.equal(s.api.theme(), 'system');
  const bad = appearance({ kiro_theme: 'bad', kiro_lang: '../../private' });
  assert.equal(bad.api.language(), 'zh'); assert.equal(bad.api.theme(), 'system');
});

test('blocked browser storage does not break login or preference controls', () => {
  const s = appearance({}, true, true);
  s.api.setTheme('light'); s.api.setLanguage('en');
  assert.equal(s.classes.has('dark'), false); assert.equal(s.html.lang, 'en');
  s.api.write('kiro_remember', '1'); assert.equal(s.api.read('kiro_remember'), '1');
});

function boot() {
  const listeners = {}, timers = new Map(), redirects = [];
  const nodes = Object.fromEntries(['bootScreen', 'bootMessage', 'bootRetry', 'bootLogin', 'mainPage'].map(k => [k, { hidden: false, dataset: {}, classList: { add() {} } }]));
  let id = 0;
  const window = { AdminAppearance: { language: () => 'en' }, addEventListener: (k, v) => { listeners[k] = v; } };
  vm.runInNewContext(source('admin-boot.js'), { window, location: { replace: p => redirects.push(p), reload: () => redirects.push('reload') },
    document: { getElementById: k => nodes[k], addEventListener: (k, v) => { listeners[k] = v; } },
    setTimeout: cb => { timers.set(++id, cb); return id; }, clearTimeout: id => timers.delete(id) });
  listeners.DOMContentLoaded();
  return { api: window.AdminBoot, nodes, listeners, timers, redirects };
}

test('boot cannot reveal a late or expired application; network error does not log out', () => {
  const b = boot(); assert.equal(b.nodes.bootScreen.dataset.state, 'loading');
  [...b.timers.values()][0](); assert.equal(b.nodes.bootScreen.dataset.state, 'error');
  assert.equal(b.api.ready(), false); assert.deepEqual(b.redirects, []);
  b.api.expired(); b.api.expired(); assert.deepEqual(b.redirects, ['/admin/login.html?expired=1']);
  const ok = boot(); assert.equal(ok.api.ready(), true); assert.equal(ok.nodes.bootScreen.hidden, true);
  ok.api.fail(); assert.equal(ok.nodes.bootScreen.hidden, false);
  const expired = boot(); expired.api.expired(); assert.equal(expired.api.ready(), false);
});

test('missing startup assets have a retry state, and back/forward cache rechecks auth', () => {
  const b = boot(); b.listeners.error({ target: { tagName: 'SCRIPT' } });
  assert.equal(b.nodes.bootScreen.dataset.state, 'error');
  b.listeners.pageshow({ persisted: true }); assert.deepEqual(b.redirects, ['reload']);
});

test('console cannot render an old login form and shares theme sources', () => {
  const html = source('index.html'), app = source('app.js'), login = source('login.html');
  assert.doesNotMatch(html, /id="(?:loginPage|pwdField|loginBtn|rememberPwd)"/);
  assert.doesNotMatch(app, /tryAutoLogin|bindLoginEvents|localStorage\./);
  for (const page of [html, login]) {
    assert.match(page, /\/admin\/appearance\.js/); assert.match(page, /\/admin\/appearance\.css/);
    assert.ok(page.indexOf('appearance.js') < page.indexOf('<body'));
  }
  assert.match(html, /#mainPage\.hidden\s*\{\s*display:\s*none !important/);
  assert.doesNotMatch(login, /Kiro-Go|github\.com|app\.js|locales\//);
  assert.match(login, /<form id="loginForm" method="post" action="\/admin\/api\/login">/);
  assert.doesNotMatch(source('styles.css'), /--background:\s*#/);
});

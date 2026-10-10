(() => {
  'use strict';
  const prefs = window.AdminAppearance;
  const strings = {
    en: { title: 'Sign In', brand: 'Administration', password: 'Password', remember: 'Remember this session',
      submit: 'Sign In', busy: 'Signing in...', opening: 'Opening console...', denied: 'Incorrect password',
      limited: 'Too many attempts. Try again later.', failed: 'Unable to sign in. Please try again.',
      expired: 'Your session expired. Please sign in again.', blocked: 'Session could not be established. Check browser cookies and try again.',
      show: 'Show password', hide: 'Hide password', theme: 'Theme', system: 'System', light: 'Light', dark: 'Dark', footer: 'Management Console' },
    zh: { title: '\u7ba1\u7406\u767b\u5f55', brand: '\u7ba1\u7406\u63a7\u5236\u53f0', password: '\u5bc6\u7801', remember: '\u4fdd\u6301\u767b\u5f55', submit: '\u767b\u5f55',
      busy: '\u767b\u5f55\u4e2d...', opening: '\u6b63\u5728\u8fdb\u5165\u63a7\u5236\u53f0...', denied: '\u5bc6\u7801\u9519\u8bef', limited: '\u5c1d\u8bd5\u8fc7\u591a\uff0c\u8bf7\u7a0d\u540e\u518d\u8bd5',
      failed: '\u65e0\u6cd5\u767b\u5f55\uff0c\u8bf7\u91cd\u8bd5', expired: '\u4f1a\u8bdd\u5df2\u8fc7\u671f\uff0c\u8bf7\u91cd\u65b0\u767b\u5f55', blocked: '\u672a\u80fd\u5efa\u7acb\u4f1a\u8bdd\uff0c\u8bf7\u68c0\u67e5\u6d4f\u89c8\u5668 Cookie \u8bbe\u7f6e\u540e\u91cd\u8bd5',
      show: '\u663e\u793a\u5bc6\u7801', hide: '\u9690\u85cf\u5bc6\u7801', theme: '\u4e3b\u9898', system: '\u8ddf\u968f\u7cfb\u7edf', light: '\u6d45\u8272', dark: '\u6df1\u8272', footer: '\u7ba1\u7406\u63a7\u5236\u53f0' }
  };
  const get = id => document.getElementById(id);
  let busy = false, opening = false;
  let errorKey = new URLSearchParams(location.search).get('expired') === '1' ? 'expired' : '';
  const language = () => prefs ? prefs.language() : (navigator.language.toLowerCase().startsWith('zh') ? 'zh' : 'en');
  function render() {
    const lang = language(), text = strings[lang];
    document.documentElement.lang = lang;
    document.title = text.title;
    for (const [id, value] of Object.entries({ heading: text.title, brandLabel: text.brand, passwordLabel: text.password,
      rememberLabel: text.remember, footerLabel: text.footer, submitLabel: opening ? text.opening : busy ? text.busy : text.submit })) get(id).textContent = value;
    get('error').textContent = errorKey ? text[errorKey] : '';
    document.querySelectorAll('[data-lang]').forEach(b => b.setAttribute('aria-pressed', String(b.dataset.lang === lang)));
    const shown = get('password').type === 'text';
    get('passwordToggle').title = shown ? text.hide : text.show;
    get('passwordToggle').setAttribute('aria-label', get('passwordToggle').title);
    get('passwordToggle').setAttribute('aria-pressed', String(shown));
    get('passwordToggle').firstElementChild.className = 'fa-solid ' + (shown ? 'fa-eye-slash' : 'fa-eye');
    const theme = prefs ? prefs.theme() : 'system';
    get('themeToggle').title = text.theme + ': ' + text[theme];
    get('themeToggle').setAttribute('aria-label', get('themeToggle').title);
    get('themeToggle').firstElementChild.className = 'fa-solid ' + ({system:'fa-circle-half-stroke',light:'fa-sun',dark:'fa-moon'})[theme];
  }
  if (prefs) get('remember').checked = prefs.read('kiro_remember') === '1';
  window.addEventListener('appearancechange', render);
  document.querySelectorAll('[data-lang]').forEach(b => b.addEventListener('click', () => { if (prefs) prefs.setLanguage(b.dataset.lang); }));
  get('themeToggle').addEventListener('click', () => {
    if (prefs) { const order = ['system', 'light', 'dark']; prefs.setTheme(order[(order.indexOf(prefs.theme()) + 1) % order.length]); }
  });
  get('passwordToggle').addEventListener('click', () => { get('password').type = get('password').type === 'password' ? 'text' : 'password'; render(); });
  render();
  get('loginForm').addEventListener('submit', async event => {
    event.preventDefault();
    if (busy || opening) return;
    busy = true; errorKey = ''; get('submit').disabled = true; render();
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15000);
    try {
      const response = await fetch('/admin/api/login', { method: 'POST', credentials: 'same-origin', signal: controller.signal,
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: get('password').value, remember: get('remember').checked }) });
      get('password').value = '';
      get('password').type = 'password';
      if (!response.ok) {
        errorKey = response.status === 429 ? 'limited' : response.status === 401 ? 'denied' : 'failed';
        return;
      }
      const body = await response.json();
      if (body.success !== true) { errorKey = 'failed'; return; }
      // Check the HTTP-only cookie without exposing its value to JavaScript.
      const session = await fetch('/admin/api/version', { credentials: 'same-origin', cache: 'no-store', signal: controller.signal });
      if (!session.ok) { errorKey = session.status === 401 ? 'blocked' : 'failed'; return; }
      if (prefs) prefs.write('kiro_remember', get('remember').checked ? '1' : '0');
      opening = true; render();
      location.replace('/admin/');
    } catch { errorKey = 'failed'; }
    finally {
      clearTimeout(timer); get('password').value = ''; get('password').type = 'password';
      busy = false; get('submit').disabled = opening; render();
    }
  });
})();

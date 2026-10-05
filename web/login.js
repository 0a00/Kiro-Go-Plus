(() => {
  'use strict';
  const zh = navigator.language.toLowerCase().startsWith('zh');
  const text = zh ? {
    title: '\u7ba1\u7406\u767b\u5f55', password: '\u5bc6\u7801', remember: '\u4fdd\u6301\u767b\u5f55',
    submit: '\u767b\u5f55', busy: '\u767b\u5f55\u4e2d...',
    denied: '\u5bc6\u7801\u9519\u8bef', limited: '\u5c1d\u8bd5\u8fc7\u591a\uff0c\u8bf7\u7a0d\u540e\u518d\u8bd5',
    failed: '\u767b\u5f55\u5931\u8d25\uff0c\u8bf7\u7a0d\u540e\u518d\u8bd5'
  } : { title: 'Administration', password: 'Password', remember: 'Remember this session',
    submit: 'Sign In', busy: 'Signing In...', denied: 'Incorrect password',
    limited: 'Too many attempts. Try again later.', failed: 'Sign in failed. Try again later.' };
  document.documentElement.lang = zh ? 'zh' : 'en';
  document.title = text.title;
  const get = id => document.getElementById(id);
  for (const [id, value] of Object.entries({ heading: text.title, passwordLabel: text.password,
    rememberLabel: text.remember, submit: text.submit })) get(id).textContent = value;
  get('loginForm').addEventListener('submit', async event => {
    event.preventDefault();
    const button = get('submit');
    if (button.disabled) return;
    button.disabled = true;
    button.textContent = text.busy;
    get('error').textContent = '';
    try {
      const response = await fetch('/admin/api/login', { method: 'POST', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: get('password').value, remember: get('remember').checked }) });
      get('password').value = '';
      if (response.ok) { location.replace('/admin/'); return; }
      get('error').textContent = response.status === 429 ? text.limited : response.status === 401 ? text.denied : text.failed;
    } catch { get('error').textContent = text.failed; }
    finally { button.disabled = false; button.textContent = text.submit; }
  });
})();

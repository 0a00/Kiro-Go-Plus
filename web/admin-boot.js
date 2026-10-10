(() => {
  'use strict';
  let state = 'loading';
  let redirecting = false;
  const labels = {
    en: { loading: 'Opening console...', failed: 'Unable to load the console.', retry: 'Retry', login: 'Sign In' },
    zh: { loading: '\u6b63\u5728\u8fdb\u5165\u63a7\u5236\u53f0...', failed: '\u63a7\u5236\u53f0\u52a0\u8f7d\u5931\u8d25', retry: '\u91cd\u8bd5', login: '\u8fd4\u56de\u767b\u5f55' }
  };
  function render() {
    const screen = document.getElementById('bootScreen');
    if (!screen) return;
    const text = labels[window.AdminAppearance?.language() || 'en'];
    screen.dataset.state = state;
    screen.hidden = state === 'ready';
    document.getElementById('bootMessage').textContent = state === 'error' ? text.failed : text.loading;
    document.getElementById('bootRetry').textContent = text.retry;
    document.getElementById('bootLogin').textContent = text.login;
    if (state !== 'ready') document.getElementById('mainPage')?.classList.add('hidden');
  }
  const timer = setTimeout(() => fail(), 20000);
  function fail() {
    if (redirecting) return;
    state = 'error'; clearTimeout(timer); render();
  }
  function ready() {
    if (redirecting || state !== 'loading') return false;
    state = 'ready'; clearTimeout(timer); render(); return true;
  }
  function expired() {
    if (redirecting) return;
    redirecting = true; clearTimeout(timer);
    location.replace('/admin/login.html?expired=1');
  }
  window.addEventListener('error', event => {
    if (state !== 'loading') return;
    const target = event.target;
    if (target === window || target?.tagName === 'SCRIPT' || target?.tagName === 'LINK') fail();
  }, true);
  window.addEventListener('unhandledrejection', () => { if (state === 'loading') fail(); });
  window.addEventListener('pageshow', event => { if (event.persisted) location.reload(); });
  document.addEventListener('DOMContentLoaded', render);
  window.AdminBoot = { fail, ready, expired };
})();

(() => {
  'use strict';
  let state = 'loading';
  let redirecting = false;
  const labels = {
    en: { loadingTitle: 'Opening console...', loading: 'Checking your session and preparing the workspace.', failedTitle: 'Console unavailable', failed: 'The console did not finish loading. Try again or sign in again.', retry: 'Retry', login: 'Sign In', admin: 'Admin' },
    zh: { loadingTitle: '\u6b63\u5728\u8fdb\u5165\u63a7\u5236\u53f0...', loading: '\u6b63\u5728\u9a8c\u8bc1\u4f1a\u8bdd\u5e76\u51c6\u5907\u5de5\u4f5c\u533a\u3002', failedTitle: '\u63a7\u5236\u53f0\u6682\u65f6\u4e0d\u53ef\u7528', failed: '\u63a7\u5236\u53f0\u52a0\u8f7d\u672a\u5b8c\u6210\uff0c\u8bf7\u91cd\u8bd5\u6216\u91cd\u65b0\u767b\u5f55\u3002', retry: '\u91cd\u8bd5', login: '\u767b\u5f55', admin: '\u7ba1\u7406' }
  };
  function render() {
    const screen = document.getElementById('bootScreen');
    if (!screen) return;
    const text = labels[window.AdminAppearance?.language() || 'en'];
    screen.dataset.state = state;
    screen.hidden = state === 'ready';
    document.getElementById('bootTitle').textContent = state === 'error' ? text.failedTitle : text.loadingTitle;
    document.getElementById('bootMessage').textContent = state === 'error' ? text.failed : text.loading;
    document.getElementById('bootRetryLabel').textContent = text.retry;
    document.getElementById('bootLoginLabel').textContent = text.login;
    document.getElementById('bootAdminLabel').textContent = text.admin;
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

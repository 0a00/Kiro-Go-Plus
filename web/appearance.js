(() => {
  'use strict';
  const memory = new Map();
  const read = key => {
    if (memory.has(key)) return memory.get(key);
    try { return localStorage.getItem(key); } catch { return null; }
  };
  const write = (key, value) => {
    memory.set(key, String(value));
    try { localStorage.setItem(key, value); } catch { /* Private storage may be blocked. */ }
  };
  const remove = key => {
    memory.set(key, null);
    try { localStorage.removeItem(key); } catch { /* No credential fallback. */ }
  };
  const themes = ['system', 'light', 'dark'];
  const media = matchMedia('(prefers-color-scheme: dark)');
  const theme = () => themes.includes(read('kiro_theme')) ? read('kiro_theme') : 'system';
  const language = () => ['zh', 'en'].includes(read('kiro_lang')) ? read('kiro_lang') : (navigator.language.toLowerCase().startsWith('zh') ? 'zh' : 'en');
  function apply() {
    const pref = theme();
    document.documentElement.classList.toggle('dark', pref === 'dark' || (pref === 'system' && media.matches));
    document.documentElement.dataset.themePref = pref;
    document.documentElement.lang = language();
  }
  function setTheme(value) {
    if (!themes.includes(value)) return;
    write('kiro_theme', value); apply();
    window.dispatchEvent(new Event('appearancechange'));
  }
  function setLanguage(value) {
    if (!['zh', 'en'].includes(value)) return;
    write('kiro_lang', value); apply();
    window.dispatchEvent(new Event('appearancechange'));
  }
  for (const key of ['admin_password', 'admin_login_time', 'kiro_remembered_pwd']) {
    remove(key);
    try { sessionStorage.removeItem(key); } catch { /* No dependency on storage access. */ }
  }
  media.addEventListener('change', () => { apply(); window.dispatchEvent(new Event('appearancechange')); });
  window.AdminAppearance = { read, write, remove, theme, language, setTheme, setLanguage, apply };
  apply();
})();

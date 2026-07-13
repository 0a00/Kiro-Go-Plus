/*
 * Kiro-Go Plus admin UI logic.
 */
(() => {
  'use strict';

  // State
  const baseUrl = location.origin;
  sessionStorage.removeItem('admin_password');
  sessionStorage.removeItem('admin_login_time');
  localStorage.removeItem('admin_password');
  localStorage.removeItem('admin_login_time');
  localStorage.removeItem('kiro_remembered_pwd');
  let currentLang = localStorage.getItem('kiro_lang') || 'zh';
  const dict = { en: null, zh: null };
  let accountsData = [];
  const selectedAccounts = new Set();
  let filterKeyword = '';
  let filterStatus = 'all';
  let privacyModeEnabled = true;
  let promptRules = [];
  let builderIdSession = '';
  let builderIdPollTimer = null;
  let iamSession = '';
  let kiroSsoSession = '';
  let kiroSsoPollTimer = null;
  let exportSelectedIds = new Set();
  let currentVersion = '';
  let testLogs = [];
  let testModalAccountId = '';
  let testModalModels = [];
  let testModalLoadingModels = false;
  let testModalModelError = false;
  let testModalRunning = false;
  let customSelectUid = 0;
  let customSelectObserver = null;
  let customSelectRefreshQueued = false;
  let requestRefreshTimer = null;

  // DOM helpers
  const $ = (id) => document.getElementById(id);
  const qsa = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  function escapeHtml(s) {
    const d = document.createElement('div');
    d.textContent = s == null ? '' : String(s);
    return d.innerHTML;
  }
  function escapeAttr(s) {
    return escapeHtml(s).replace(/"/g, '&quot;');
  }

  function legacyCopyText(str) {
    const ta = document.createElement('textarea');
    const active = document.activeElement;
    ta.value = str;
    ta.readOnly = true;
    ta.className = 'clipboard-proxy';
    ta.setAttribute('aria-hidden', 'true');
    document.body.appendChild(ta);
    let copied = false;
    try {
      try { ta.focus({ preventScroll: true }); } catch (e) { ta.focus(); }
      ta.select();
      ta.setSelectionRange(0, str.length);
      copied = typeof document.execCommand === 'function' && document.execCommand('copy') === true;
    } catch (e) {
      copied = false;
    } finally {
      document.body.removeChild(ta);
      if (active && active.isConnected && typeof active.focus === 'function') {
        try { active.focus({ preventScroll: true }); } catch (e) {
          try { active.focus(); } catch (ignored) { }
        }
      }
    }
    return copied;
  }

  function showClipboardFallback(str) {
    const value = $('clipboardFallbackValue');
    if (!value) return;
    value.value = str;
    openDialog('clipboardFallbackModal');
    setTimeout(() => {
      try { value.focus({ preventScroll: true }); value.select(); } catch (e) { }
    }, 0);
  }

  function closeClipboardFallback() {
    closeDialog('clipboardFallbackModal');
    const value = $('clipboardFallbackValue');
    if (value) value.value = '';
  }

  async function copyClipboardFallbackValue() {
    const value = $('clipboardFallbackValue');
    const str = value ? value.value : '';
    if (!str) return;
    let copied = legacyCopyText(str);
    if (!copied && navigator.clipboard && navigator.clipboard.writeText) {
      try {
        await navigator.clipboard.writeText(str);
        copied = true;
      } catch (e) { }
    }
    if (!copied) {
      try { value.focus({ preventScroll: true }); value.select(); } catch (e) { }
      toastWarning(t('clipboard.manualCopy'));
      return;
    }
    closeClipboardFallback();
    toastPrimary(t('common.copied'));
  }

  async function copyText(input) {
    const isPromise = input && typeof input.then === 'function';
    if (isPromise && typeof ClipboardItem !== 'undefined' && navigator.clipboard && navigator.clipboard.write) {
      try {
        const blobPromise = Promise.resolve(input).then(t => new Blob([String(t == null ? '' : t)], { type: 'text/plain' }));
        await navigator.clipboard.write([new ClipboardItem({ 'text/plain': blobPromise })]);
        return true;
      } catch (e) { }
    }
    const text = isPromise ? await input : input;
    const str = String(text == null ? '' : text);
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try {
        await navigator.clipboard.writeText(str);
        return true;
      } catch (e) { }
    }
    if (legacyCopyText(str)) return true;
    showClipboardFallback(str);
    return false;
  }
  function renderEndpointCode(id, value) {
    const el = $(id);
    if (!el) return;
    const raw = String(value || '');
    el.dataset.rawValue = raw;
    try {
      const url = new URL(raw);
      const path = url.pathname + url.search + url.hash;
      el.innerHTML =
        '<span class="api-code-protocol">' + escapeHtml(url.protocol + '//') + '</span>' +
        '<span class="api-code-host">' + escapeHtml(url.host) + '</span>' +
        '<span class="api-code-path">' + escapeHtml(path) + '</span>';
    } catch (e) {
      el.textContent = raw;
    }
  }

  // i18n
  async function loadLocale(lang) {
    if (dict[lang]) return dict[lang];
    try {
      const res = await fetch('/admin/locales/' + lang + '.json?v=' + Date.now(), { cache: 'no-store' });
      dict[lang] = await res.json();
    } catch (e) {
      dict[lang] = {};
    }
    return dict[lang];
  }
  function t(key, ...args) {
    const active = dict[currentLang] || {};
    const fallback = dict.zh || {};
    let text = active[key] || fallback[key] || key;
    args.forEach((arg, idx) => { text = text.replace('{' + idx + '}', arg); });
    return text;
  }
  function applyTranslations() {
    qsa('[data-i18n]').forEach(el => { el.textContent = t(el.dataset.i18n); });
    qsa('[data-i18n-placeholder]').forEach(el => { el.placeholder = t(el.dataset.i18nPlaceholder); });
    qsa('[data-i18n-title]').forEach(el => { el.title = t(el.dataset.i18nTitle); });
    qsa('[data-i18n-aria-label]').forEach(el => { el.setAttribute('aria-label', t(el.dataset.i18nAriaLabel)); });
    document.title = t('app.title');
    document.documentElement.lang = currentLang;
    updateLangButtons();
    applyTheme(getThemePref());
    refreshCustomSelects();
  }
  async function setLang(lang) {
    currentLang = lang;
    localStorage.setItem('kiro_lang', lang);
    await loadLocale(lang);
    applyTranslations();
    renderVersionBadge();
    renderAccounts();
    renderPromptRules();
  }
  function updateLangButtons() {
    qsa('.lang-btn').forEach(btn => btn.classList.toggle('active', btn.dataset.lang === currentLang));
    qsa('.lang-toggle').forEach(btn => {
      const label = btn.querySelector('.lang-toggle-label');
      if (label) label.textContent = currentLang === 'zh' ? t('lang.zh') : t('lang.en');
    });
  }
  function toggleLang() {
    setLang(currentLang === 'zh' ? 'en' : 'zh');
  }

  // Custom select
  function getCustomSelectLabel(select) {
    const option = select.selectedOptions && select.selectedOptions[0];
    return ((option && option.textContent) || select.value || '').trim();
  }
  function syncCustomSelect(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const value = wrap.querySelector('.custom-select-value');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (value) value.textContent = getCustomSelectLabel(select);
    if (trigger) trigger.disabled = select.disabled;
    wrap.classList.toggle('is-disabled', select.disabled);
    qsa('.custom-select-option', wrap).forEach(option => {
      const selected = option.dataset.index === String(select.selectedIndex);
      option.classList.toggle('is-selected', selected);
      option.setAttribute('aria-selected', String(selected));
    });
  }
  function renderCustomSelectOptions(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const content = wrap.querySelector('.custom-select-content');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (!content) return;
    if (trigger) labelCustomSelect(select, trigger, content, select.id);
    content.innerHTML = '';
    Array.from(select.options).forEach((option, index) => {
      const item = document.createElement('button');
      item.type = 'button';
      item.className = 'custom-select-option';
      item.setAttribute('role', 'option');
      item.dataset.index = String(index);
      item.disabled = option.disabled;
      item.textContent = (option.textContent || option.value || '').trim();
      content.appendChild(item);
    });
    syncCustomSelect(select);
  }
  function placeCustomSelectContent(select) {
    const wrap = select && select.__customSelect;
    if (!wrap || !wrap.classList.contains('is-open')) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.querySelector('.custom-select-content');
    if (!trigger || !content) return;
    const rect = trigger.getBoundingClientRect();
    const gap = 4;
    const below = window.innerHeight - rect.bottom - gap;
    const above = rect.top - gap;
    const openUp = below < 180 && above > below;
    const available = Math.max(96, Math.min(224, (openUp ? above : below) - 4));
    content.style.left = Math.round(rect.left) + 'px';
    content.style.width = Math.round(rect.width) + 'px';
    content.style.maxHeight = Math.round(available) + 'px';
    content.style.top = openUp ? 'auto' : Math.round(rect.bottom + gap) + 'px';
    content.style.bottom = openUp ? Math.round(window.innerHeight - rect.top + gap) + 'px' : 'auto';
    content.dataset.side = openUp ? 'top' : 'bottom';
  }
  function setCustomSelectOpen(select, open) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.querySelector('.custom-select-content');
    if (!trigger || !content) return;
    if (open && !select.disabled) {
      closeAllCustomSelects(select);
      renderCustomSelectOptions(select);
      wrap.classList.add('is-open');
      trigger.setAttribute('aria-expanded', 'true');
      content.hidden = false;
      placeCustomSelectContent(select);
      requestAnimationFrame(() => placeCustomSelectContent(select));
      const selected = content.querySelector('.custom-select-option.is-selected:not(:disabled)') || content.querySelector('.custom-select-option:not(:disabled)');
      if (selected) selected.focus({ preventScroll: true });
    } else {
      wrap.classList.remove('is-open');
      trigger.setAttribute('aria-expanded', 'false');
      content.hidden = true;
    }
  }
  function closeAllCustomSelects(except) {
    qsa('select.custom-select-native').forEach(select => {
      if (select !== except) setCustomSelectOpen(select, false);
    });
  }
  function chooseCustomSelectOption(select, index) {
    const option = select.options[index];
    if (!option || option.disabled) return;
    select.value = option.value;
    select.dispatchEvent(new Event('input', { bubbles: true }));
    select.dispatchEvent(new Event('change', { bubbles: true }));
    syncCustomSelect(select);
    setCustomSelectOpen(select, false);
    const trigger = select.__customSelect && select.__customSelect.querySelector('.custom-select-trigger');
    if (trigger && trigger.isConnected) trigger.focus({ preventScroll: true });
  }
  function focusSiblingCustomOption(current, dir) {
    const options = qsa('.custom-select-option:not(:disabled)', current.parentElement);
    const index = options.indexOf(current);
    const next = options[(index + dir + options.length) % options.length];
    if (next) next.focus({ preventScroll: true });
  }
  function getCustomSelectLabelElement(select) {
    const explicit = qsa('label').find(label => label.htmlFor === select.id);
    if (explicit) return explicit;
    const group = select.closest('.form-group');
    return group ? group.querySelector('label') : null;
  }
  function labelCustomSelect(select, trigger, content, id) {
    trigger.id = id + '-trigger';
    const valueId = id + '-value';
    const value = trigger.querySelector('.custom-select-value');
    if (value) value.id = valueId;
    const label = getCustomSelectLabelElement(select);
    if (label) {
      if (!label.id) label.id = id + '-label';
      trigger.removeAttribute('aria-label');
      trigger.setAttribute('aria-labelledby', label.id + ' ' + valueId);
    } else {
      trigger.removeAttribute('aria-labelledby');
      trigger.setAttribute('aria-label', select.getAttribute('aria-label') || getCustomSelectLabel(select));
    }
    content.setAttribute('aria-labelledby', trigger.id);
  }
  function enhanceCustomSelect(select) {
    if (!select || select.__customSelect || select.dataset.nativeSelect === 'true') return;

    const id = select.id || 'custom-select-' + (++customSelectUid);
    if (!select.id) select.id = id;

    const wrap = document.createElement('div');
    wrap.className = 'custom-select';
    wrap.dataset.customSelect = 'true';
    if (select.id === 'filterStatusSelect') wrap.classList.add('custom-select-filter');

    const trigger = document.createElement('button');
    trigger.type = 'button';
    trigger.className = 'custom-select-trigger';
    trigger.setAttribute('aria-haspopup', 'listbox');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('aria-controls', id + '-menu');
    trigger.innerHTML =
      '<span class="custom-select-value"></span>' +
      '<i class="fa-solid fa-chevron-down custom-select-icon" aria-hidden="true"></i>';

    const content = document.createElement('div');
    content.id = id + '-menu';
    content.className = 'custom-select-content';
    content.setAttribute('role', 'listbox');
    content.hidden = true;
    labelCustomSelect(select, trigger, content, id);

    wrap.appendChild(trigger);
    wrap.appendChild(content);
    select.insertAdjacentElement('afterend', wrap);
    select.classList.add('custom-select-native');
    select.setAttribute('aria-hidden', 'true');
    select.tabIndex = -1;
    select.__customSelect = wrap;
    wrap.__nativeSelect = select;

    trigger.addEventListener('click', () => setCustomSelectOpen(select, !wrap.classList.contains('is-open')));
    trigger.addEventListener('keydown', e => {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) {
        e.preventDefault();
        setCustomSelectOpen(select, true);
      }
    });
    content.addEventListener('click', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      chooseCustomSelectOption(select, parseInt(option.dataset.index, 10));
    });
    content.addEventListener('keydown', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      if (e.key === 'ArrowDown') { e.preventDefault(); focusSiblingCustomOption(option, 1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); focusSiblingCustomOption(option, -1); }
      else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); chooseCustomSelectOption(select, parseInt(option.dataset.index, 10)); }
      else if (e.key === 'Escape') { e.preventDefault(); setCustomSelectOpen(select, false); trigger.focus({ preventScroll: true }); }
    });
    select.addEventListener('change', () => syncCustomSelect(select));
    renderCustomSelectOptions(select);
  }
  function enhanceCustomSelects(root) {
    qsa('select:not(.custom-select-native)', root || document).forEach(enhanceCustomSelect);
  }
  function refreshCustomSelects(root) {
    enhanceCustomSelects(root);
    qsa('select.custom-select-native', root || document).forEach(renderCustomSelectOptions);
  }
  function positionOpenCustomSelects() {
    qsa('select.custom-select-native').forEach(placeCustomSelectContent);
  }
  function queueCustomSelectRefresh() {
    if (customSelectRefreshQueued) return;
    customSelectRefreshQueued = true;
    requestAnimationFrame(() => {
      customSelectRefreshQueued = false;
      refreshCustomSelects();
      positionOpenCustomSelects();
    });
  }
  function initCustomSelectObserver() {
    if (customSelectObserver || !document.body || typeof MutationObserver === 'undefined') return;
    customSelectObserver = new MutationObserver(mutations => {
      let shouldRefresh = false;
      for (const mutation of mutations) {
        const target = mutation.target;
        if (target && target.closest && target.closest('.custom-select')) continue;
        if (target && target.matches && target.matches('select')) {
          shouldRefresh = true;
          break;
        }
        for (const node of mutation.addedNodes || []) {
          if (node.nodeType !== 1) continue;
          if ((node.matches && node.matches('select')) || (node.querySelector && node.querySelector('select'))) {
            shouldRefresh = true;
            break;
          }
        }
        if (shouldRefresh) break;
      }
      if (shouldRefresh) queueCustomSelectRefresh();
    });
    customSelectObserver.observe(document.body, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['disabled', 'class', 'id', 'data-native-select']
    });
  }

  // Theme
  const THEME_ORDER = ['system', 'light', 'dark'];
  const themeMQ = window.matchMedia('(prefers-color-scheme: dark)');
  function resolveTheme(pref) {
    if (pref === 'dark') return 'dark';
    if (pref === 'light') return 'light';
    return themeMQ.matches ? 'dark' : 'light';
  }
  function applyTheme(pref) {
    const resolved = resolveTheme(pref);
    const root = document.documentElement;
    root.classList.toggle('dark', resolved === 'dark');
    root.dataset.themePref = pref;
    qsa('.theme-toggle').forEach(btn => {
      btn.dataset.theme = pref;
      const themeLabel = t('theme.status', t('theme.' + pref));
      btn.setAttribute('aria-label', themeLabel);
      btn.setAttribute('title', themeLabel);
    });
  }
  function getThemePref() {
    const saved = localStorage.getItem('kiro_theme');
    return THEME_ORDER.includes(saved) ? saved : 'system';
  }
  function initTheme() {
    applyTheme(getThemePref());
    themeMQ.addEventListener('change', () => {
      if (getThemePref() === 'system') applyTheme('system');
    });
  }
  function toggleTheme() {
    const cur = getThemePref();
    const next = THEME_ORDER[(THEME_ORDER.indexOf(cur) + 1) % THEME_ORDER.length];
    localStorage.setItem('kiro_theme', next);
    applyTheme(next);
  }

  // Privacy and email mask
  function initPrivacyMode() {
    const saved = localStorage.getItem('privacyMode');
    privacyModeEnabled = saved === null ? true : saved === 'true';
    const toggle = $('privacyModeToggle');
    if (toggle) toggle.checked = privacyModeEnabled;
  }
  function maskEmail(email) {
    if (!privacyModeEnabled || !email || email.indexOf('@') === -1) return email;
    const [local, domain] = email.split('@');
    const maskedLocal = local.length <= 2 ? local : local.substring(0, 2) + '***';
    const parts = domain.split('.');
    if (parts.length >= 2) {
      const tld = parts[parts.length - 1];
      const sld = parts[parts.length - 2];
      const maskedSld = sld.length <= 2 ? sld : sld.substring(0, 2) + '***';
      const subs = parts.slice(0, -2).map(s => s.length <= 2 ? s : s.substring(0, 2) + '***');
      return maskedLocal + '@' + [...subs, maskedSld, tld].join('.');
    }
    return maskedLocal + '@' + domain;
  }
  function getDisplayEmail(email, id) {
    const raw = email || (id ? id.substring(0, 12) + '...' : '-');
    return maskEmail(raw);
  }

  // Toast bridge
  const toast = function (msg, variant, opts) {
    if (typeof window.toast === 'function') return window.toast(msg, variant, opts);
    try { console.warn('[toast missing]', variant, msg); } catch (_) { }
    return function () {};
  };
  const toastPrimary = (msg, opts) => toast(msg, 'primary', opts);
  const toastWarning = (msg, opts) => toast(msg, 'warning', opts);
  const toastError = (msg, opts) => toast(msg, 'error', opts);

  // Modal helpers
  let modalScrollY = 0;
  let confirmResolve = null;
  const modalFocusStack = [];
  function lockModalScroll() {
    if (document.body.classList.contains('modal-open')) return;
    modalScrollY = window.scrollY || document.documentElement.scrollTop || 0;
    document.body.style.top = '-' + modalScrollY + 'px';
    document.body.classList.add('modal-open');
  }
  function unlockModalScrollIfIdle() {
    if (qsa('.modal.active').length > 0) return;
    if (!document.body.classList.contains('modal-open')) return;
    document.body.classList.remove('modal-open');
    document.body.style.top = '';
    window.scrollTo(0, modalScrollY);
  }
  function getModalFocusable(modal) {
    return qsa('a[href], button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])', modal)
      .filter(el => !el.closest('[hidden]'));
  }
  function prepareDialog(modal) {
    modal.setAttribute('role', 'dialog');
    modal.setAttribute('aria-modal', 'true');
    modal.setAttribute('aria-hidden', 'false');
    if (!modal.hasAttribute('tabindex')) modal.tabIndex = -1;
    const title = modal.querySelector('.modal-title');
    if (title) {
      if (!title.id) title.id = modal.id + 'Title';
      modal.setAttribute('aria-labelledby', title.id);
    }
  }
  function focusDialog(modal) {
    if (modal.contains(document.activeElement) && document.activeElement !== modal) return;
    const focusable = getModalFocusable(modal);
    const target = focusable[0] || modal;
    if (target && target.focus) target.focus({ preventScroll: true });
  }
  function trapDialogFocus(e) {
    const modal = e.currentTarget;
    if (e.key !== 'Tab' || !modal.classList.contains('active')) return;
    const focusable = getModalFocusable(modal);
    if (!focusable.length) {
      e.preventDefault();
      modal.focus({ preventScroll: true });
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus({ preventScroll: true });
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus({ preventScroll: true });
    }
  }
  function openDialog(id) {
    const modal = $(id);
    if (!modal) return;
    prepareDialog(modal);
    modalFocusStack.push({ id, el: document.activeElement });
    modal.removeEventListener('keydown', trapDialogFocus);
    modal.addEventListener('keydown', trapDialogFocus);
    modal.classList.add('active');
    lockModalScroll();
    focusDialog(modal);
    setTimeout(() => focusDialog(modal), 0);
  }
  function closeDialog(id) {
    const modal = $(id);
    if (!modal) return;
    modal.classList.remove('active');
    modal.setAttribute('aria-hidden', 'true');
    const stackIndex = modalFocusStack.map(item => item.id).lastIndexOf(id);
    const previous = stackIndex >= 0 ? modalFocusStack.splice(stackIndex, 1)[0].el : null;
    unlockModalScrollIfIdle();
    if (previous && previous.isConnected && previous.focus) {
      requestAnimationFrame(() => previous.focus({ preventScroll: true }));
    }
  }
  function bindDialogBackdropClose(id, closeFn) {
    const modal = $(id);
    if (!modal) return;
    let startedOnBackdrop = false;
    modal.addEventListener('pointerdown', e => {
      startedOnBackdrop = e.target === modal;
    });
    modal.addEventListener('click', e => {
      if (startedOnBackdrop && e.target === modal) closeFn();
      startedOnBackdrop = false;
    });
  }
  function closeConfirm(value) {
    if (!confirmResolve) return;
    const resolve = confirmResolve;
    confirmResolve = null;
    closeDialog('confirmModal');
    resolve(!!value);
  }
  function confirmAction(message, opts) {
    opts = opts || {};
    if (confirmResolve) closeConfirm(false);
    const modal = $('confirmModal');
    const title = $('confirmTitle');
    const msg = $('confirmMessage');
    const ok = $('confirmOk');
    const cancel = $('confirmCancel');
    const close = $('confirmClose');
    if (!modal || !title || !msg || !ok || !cancel || !close) {
      return Promise.resolve(false);
    }
    title.textContent = opts.title || t('common.confirm');
    msg.textContent = message || '';
    ok.textContent = opts.confirmText || t('common.confirm');
    cancel.textContent = opts.cancelText || t('common.cancel');
    ok.className = 'btn ' + (opts.variant === 'danger' ? 'btn-danger' : 'btn-primary');
    cancel.className = 'btn btn-secondary';
    ok.onclick = () => closeConfirm(true);
    cancel.onclick = () => closeConfirm(false);
    close.onclick = () => closeConfirm(false);
    const pending = new Promise(resolve => { confirmResolve = resolve; });
    openDialog('confirmModal');
    ok.focus({ preventScroll: true });
    return pending;
  }

  // Fetch wrapper
  function api(path, opts) {
    opts = opts || {};
    opts.headers = Object.assign({}, opts.headers || {});
    opts.credentials = 'same-origin';
    if (opts.body && !opts.headers['Content-Type']) opts.headers['Content-Type'] = 'application/json';
    return fetch('/admin/api' + path, opts);
  }

  // Login
  async function tryAutoLogin() {
    try {
      const res = await api('/status');
      if (res.ok) { showMain(); loadData(); }
    } catch (e) { }
  }
  async function login() {
    const password = $('pwdField').value;
    const remember = $('rememberPwd');
    try {
      const res = await api('/login', {
        method: 'POST',
        body: JSON.stringify({ password, remember: !!(remember && remember.checked) })
      });
      if (res.ok) {
        localStorage.setItem('kiro_remember', remember && remember.checked ? '1' : '0');
        $('pwdField').value = '';
        showMain(); loadData();
      } else {
        const body = await res.json().catch(() => ({}));
        toast(body.error || t('login.error'), 'error');
      }
    } catch (e) {
      toast(t('login.connectError'), 'error');
    }
  }
  function initRememberMe() {
    const remember = $('rememberPwd');
    const field = $('pwdField');
    if (!remember || !field) return;
    if (localStorage.getItem('kiro_remember') === '1') {
      remember.checked = true;
    }
  }
  async function logout() {
    try {
      await api('/logout', { method: 'POST' });
    } finally {
      location.reload();
    }
  }
  function showMain() {
    $('loginPage').classList.add('hidden');
    $('mainPage').classList.remove('hidden');
  }

  // Data loaders
  async function loadData() {
    await Promise.all([loadStats(), loadAccounts(), loadSettings(), loadVersion()]);
    renderEndpointCode('claudeEndpoint', baseUrl + '/v1/messages');
    renderEndpointCode('openaiEndpoint', baseUrl + '/v1/chat/completions');
    renderEndpointCode('openaiResponsesEndpoint', baseUrl + '/v1/responses');
    renderEndpointCode('modelsEndpoint', baseUrl + '/v1/models');
    renderEndpointCode('statsEndpoint', baseUrl + '/v1/stats');
    setTimeout(checkUpdate, 2000);
  }
  async function loadStats() {
    const res = await api('/status');
    const d = await res.json();
    $('statAccounts').textContent = d.accounts || 0;
    $('statRequests').textContent = d.totalRequests || 0;
    $('statSuccess').textContent = d.successRequests || 0;
    $('statFailed').textContent = d.failedRequests || 0;
    $('statTokens').textContent = formatNum(d.totalTokens || 0);
    $('statCredits').textContent = (d.totalCredits || 0).toFixed(1);
  }
  async function loadAccounts() {
    const res = await api('/accounts');
    accountsData = await res.json();
    renderAccounts();
  }

  async function loadRequests() {
    const [requestsRes, protectionRes] = await Promise.all([
      api('/requests?limit=100'),
      api('/upstream-protection/status')
    ]);
    const requests = await requestsRes.json();
    const protection = await protectionRes.json();
    renderRequestSummary(protection);
    renderRequests((requests && requests.requests) || []);
  }

  function renderRequestSummary(data) {
    const el = $('upstreamProtectionSummary');
    if (!el) return;
    const cfg = data && data.config ? data.config : {};
    const states = (data && data.accountModelStates) || [];
    const active = states.filter(s => (s.inFlight || 0) > 0).length;
    const cooling = states.filter(s => (s.cooldownSeconds || 0) > 0).length;
	const endpointCircuits = (((data || {}).networkCircuits || {}).endpoints || []).filter(s => s.state === 'open').length;
	const proxyCircuits = (((data || {}).networkCircuits || {}).proxies || []).filter(s => s.state === 'open').length;
    const accountEndpointCooldowns = (((data || {}).accountEndpointRoutes || {}).cooldowns || []).length;
    el.innerHTML =
      '<div class="request-summary-item"><span>' + escapeHtml(t('requests.protection')) + '</span><strong>' + escapeHtml(cfg.enabled === false ? t('common.disabled') : t('common.enabled')) + '</strong></div>' +
      '<div class="request-summary-item"><span>' + escapeHtml(t('requests.inFlight')) + '</span><strong>' + active + '</strong></div>' +
      '<div class="request-summary-item"><span>' + escapeHtml(t('requests.cooling')) + '</span><strong>' + cooling + '</strong></div>' +
	  '<div class="request-summary-item"><span>' + escapeHtml(t('requests.affinity')) + '</span><strong>' + ((data && data.routeAffinityCount) || 0) + '</strong></div>' +
	  '<div class="request-summary-item"><span>' + escapeHtml(t('requests.endpointCircuits')) + '</span><strong>' + endpointCircuits + '</strong></div>' +
	  '<div class="request-summary-item"><span>' + escapeHtml(t('requests.accountEndpointCooldowns')) + '</span><strong>' + accountEndpointCooldowns + '</strong></div>' +
	  '<div class="request-summary-item"><span>' + escapeHtml(t('requests.proxyCircuits')) + '</span><strong>' + proxyCircuits + '</strong></div>';
  }

  function renderRequests(items) {
    const body = $('requestsTableBody');
    if (!body) return;
    if (!items.length) {
	  body.innerHTML = '<tr><td colspan="13" class="request-empty">' + escapeHtml(t('requests.empty')) + '</td></tr>';
      return;
    }
    body.innerHTML = items.map(item => {
      const statusClass = item.status === 'success' ? 'badge-success' : 'badge-error';
      const tokens = (item.inputTokens || 0) + ' / ' + (item.outputTokens || 0);
      const cache = (item.cacheReadInputTokens || 0) + ' / ' + (item.cacheCreationInputTokens || 0);
      const account = item.accountEmail || item.accountId || '-';
      const err = item.error ? item.error : '';
	  const requestId = item.requestId || '-';
      const apiKey = item.apiKeyName || (item.apiKeyId ? item.apiKeyId.slice(0, 8) : '-');
      const outcomeParts = [];
      if (item.stopReason) outcomeParts.push(item.stopReason);
      if (item.visibleOutputChars || item.thinkingOutputChars) outcomeParts.push((item.visibleOutputChars || 0) + ' / ' + (item.thinkingOutputChars || 0));
      if (item.requestToolCount) {
        let toolOutcome = t('requests.tools') + ' ' + (item.toolUseCount || 0) + ' / ' + item.requestToolCount;
        if (item.toolUsePolicy === 'explicit') toolOutcome += ' · ' + t('requests.toolPolicyExplicit');
        if (item.toolUsePolicy === 'inferred') toolOutcome += ' · ' + t('requests.toolPolicyInferred');
        if (item.toolUseRequired) toolOutcome += ' · ' + t('requests.toolRequired');
        outcomeParts.push(toolOutcome);
      }
      const outcome = outcomeParts.join(' · ') || '-';
      const outcomeTitle = outcome + (Array.isArray(item.requestToolNames) && item.requestToolNames.length ? ' · ' + item.requestToolNames.join(', ') : '');
      const firstContentDuration = formatRequestDuration(item.firstContentMs);
      const totalDuration = formatRequestDuration(item.durationMs);
      const durationTitle = t('requests.firstContent') + ': ' + firstContentDuration + ' · ' + t('requests.totalDuration') + ': ' + totalDuration;
      return '<tr>' +
        '<td>' + escapeHtml(formatTime(item.timestamp)) + '</td>' +
        '<td>' + escapeHtml(item.protocol || '-') + '</td>' +
		'<td class="font-mono" title="' + escapeAttr(requestId) + '">' + escapeHtml(requestId === '-' ? '-' : requestId.slice(0, 12)) + '</td>' +
        '<td title="' + escapeAttr(item.apiKeyId || '') + '">' + escapeHtml(apiKey) + '</td>' +
        '<td>' + escapeHtml(item.model || '-') + '</td>' +
        '<td>' + escapeHtml(maskEmail(account)) + '</td>' +
		'<td>' + escapeHtml(item.endpoint || '-') + '</td>' +
        '<td><span class="badge ' + statusClass + '">' + escapeHtml(String(item.statusCode || '')) + '</span></td>' +
        '<td>' + escapeHtml(tokens) + '</td>' +
        '<td>' + escapeHtml(cache) + '</td>' +
        '<td title="' + escapeAttr(outcomeTitle) + '">' + escapeHtml(outcome) + '</td>' +
        '<td class="request-duration" title="' + escapeAttr(durationTitle) + '">' +
          '<span class="request-duration-line"><span class="request-duration-label">' + escapeHtml(t('requests.firstContent')) + '</span><strong>' + escapeHtml(firstContentDuration) + '</strong></span>' +
          '<span class="request-duration-line"><span class="request-duration-label">' + escapeHtml(t('requests.totalDuration')) + '</span><strong>' + escapeHtml(totalDuration) + '</strong></span>' +
        '</td>' +
        '<td class="request-error" title="' + escapeAttr(err) + '">' + escapeHtml(err) + '</td>' +
        '</tr>';
    }).join('');
  }

  // Account list
  function getFilteredAccounts() {
    return accountsData.filter(a => {
      if (filterStatus === 'enabled' && !a.enabled) return false;
      if (filterStatus === 'disabled' && (a.enabled || (a.banStatus && a.banStatus !== 'ACTIVE'))) return false;
      if (filterStatus === 'banned' && (!a.banStatus || a.banStatus === 'ACTIVE')) return false;
      if (filterKeyword) {
        const kw = filterKeyword.toLowerCase();
        if (!(a.email || '').toLowerCase().includes(kw)) return false;
      }
      return true;
    });
  }
  function onFilterChange() {
    filterKeyword = $('filterSearch').value;
    filterStatus = $('filterStatusSelect').value;
    renderAccounts();
  }
  function toggleSelectAll(checked) {
    const filtered = getFilteredAccounts();
    if (checked) filtered.forEach(a => selectedAccounts.add(a.id));
    else selectedAccounts.clear();
    renderAccounts();
    updateBatchBar();
  }
  function toggleSelectAccount(id) {
    if (selectedAccounts.has(id)) selectedAccounts.delete(id);
    else selectedAccounts.add(id);
    updateBatchBar();
  }
  function updateBatchBar() {
    const bar = $('batchBar');
    const count = selectedAccounts.size;
    const cb = $('selectAllCheckbox');
    if (cb) {
      const filtered = getFilteredAccounts();
      const selectedFiltered = filtered.filter(a => selectedAccounts.has(a.id)).length;
      cb.checked = filtered.length > 0 && selectedFiltered === filtered.length;
      cb.indeterminate = selectedFiltered > 0 && selectedFiltered < filtered.length;
    }
    if (count > 0) {
      bar.classList.remove('hidden');
      $('batchCount').textContent = String(count);
    } else {
      bar.classList.add('hidden');
    }
  }

  function formatSubscriptionLabel(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return t('subscription.power');
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return t('subscription.proPlus');
    if (s.includes('PRO')) return t('subscription.pro');
    if (s.includes('FREE')) return t('subscription.free');
    return type || t('subscription.free');
  }
  function getSubBadge(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return '<span class="badge badge-power">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return '<span class="badge badge-proplus">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO')) return '<span class="badge badge-pro">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    return '<span class="badge badge-free">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
  }
  function getTrialBadge(a) {
    if (a.trialStatus === 'ACTIVE' && a.trialUsageLimit > 0) {
      return '<span class="badge badge-trial">' + escapeHtml(t('accounts.trial')) + '</span>';
    }
    return '';
  }
  function formatTrialExpiry(ts) {
    if (!ts) return '';
    const date = new Date(ts * 1000);
    const diffDays = Math.ceil((date - new Date()) / (1000 * 60 * 60 * 24));
    if (diffDays < 0) return '(' + t('accounts.trialExpired') + ')';
    if (diffDays === 0) return '(' + t('accounts.trialToday') + ')';
    if (diffDays <= 7) return '(' + diffDays + t('accounts.trialDays') + ')';
    return '';
  }
  function formatAuthMethod(method) {
    if (!method) return '-';
    const normalized = String(method).toLowerCase();
    if (normalized === 'idc') return t('auth.enterprise');
    if (normalized === 'social') return t('auth.social');
    if (normalized === 'api_key' || normalized === 'apikey' || normalized === 'api-key') return 'API Key';
    if (normalized === 'builderid') return 'BuilderID';
    if (normalized === 'github') return t('local.providerGithub');
    if (normalized === 'google') return t('local.providerGoogle');
    return method;
  }
  function getStatusBadge(a) {
    const out = [];
    const isBanned = a.banStatus && a.banStatus !== 'ACTIVE';
    if (isBanned) {
      if (a.banStatus === 'BANNED') out.push('<span class="badge badge-banned">' + escapeHtml(t('accounts.banned')) + '</span>');
      else if (a.banStatus === 'SUSPENDED') out.push('<span class="badge badge-suspended">' + escapeHtml(t('accounts.suspended')) + '</span>');
      out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    } else {
      if (!a.hasToken)
        out.push('<span class="badge badge-error">' + escapeHtml(t('accounts.noToken')) + '</span>');
      else if (a.expiresAt && a.expiresAt < Date.now() / 1000)
        out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.expired')) + '</span>');
      else
        out.push('<span class="badge badge-success">' + escapeHtml(t('accounts.normal')) + '</span>');
      out.push(a.enabled
        ? '<span class="badge badge-info">' + escapeHtml(t('accounts.enabled')) + '</span>'
        : '<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    }
    return out.join('');
  }
  function formatTokenExpiry(ts) {
    if (!ts) return '-';
    const diff = ts - Date.now() / 1000;
    if (diff <= 0) return t('time.expired');
    if (diff < 3600) return Math.floor(diff / 60) + t('time.minutes');
    if (diff < 86400) return Math.floor(diff / 3600) + t('time.hours');
    return Math.floor(diff / 86400) + t('time.days');
  }
  function formatNum(n) {
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return n.toString();
  }
  function formatTime(ts) {
    if (!ts) return '-';
    return new Date(ts * 1000).toLocaleString();
  }
  function formatRequestDuration(value) {
    if (value === null || value === undefined || value === '') return '-';
    const ms = Number(value);
    if (!Number.isFinite(ms) || ms < 0) return '-';
    if (ms < 1000) return Math.round(ms) + 'ms';
    if (ms < 60000) {
      const digits = ms < 10000 ? 2 : 1;
      return Number((ms / 1000).toFixed(digits)) + 's';
    }
    if (ms < 3600000) {
      const minutes = Math.floor(ms / 60000);
      const seconds = (ms % 60000) / 1000;
      return minutes + 'm ' + Number(seconds.toFixed(seconds < 10 ? 1 : 0)) + 's';
    }
    const hours = Math.floor(ms / 3600000);
    const minutes = Math.floor((ms % 3600000) / 60000);
    return hours + 'h ' + minutes + 'm';
  }
  function applyUsageBars(root) {
    qsa('.usage-fill[data-usage-pct]', root).forEach(el => {
      const pct = Math.max(0, Math.min(100, parseFloat(el.dataset.usagePct) || 0));
      el.style.width = pct + '%';
    });
  }

  function renderAccounts() {
    const container = $('accountsList');
    if (!container) return;
    const filtered = getFilteredAccounts();
    if (filtered.length === 0) {
      container.innerHTML = '<div class="empty-state">' + escapeHtml(t('accounts.empty')) + '</div>';
      return;
    }
    container.innerHTML = filtered.map(a => {
      const usagePct = (a.usagePercent || 0) * 100;
      const usageClass = usagePct > 90 ? 'critical' : usagePct > 70 ? 'high' : '';
      const trialPct = (a.trialUsagePercent || 0) * 100;
      const trialClass = trialPct > 90 ? 'critical' : trialPct > 70 ? 'high' : '';
      const isSelected = selectedAccounts.has(a.id);
      const weight = a.weight || 0;
      const weightBadge = weight >= 2 ? '<span class="badge badge-warning">' + escapeHtml(t('accounts.weightShort')) + ':' + weight + '</span>' : '';
      const concurrency = a.maxConcurrency || 0;
      const concurrencyBadge = concurrency > 0 ? '<span class="badge badge-info">' + escapeHtml(t('accounts.concurrencyShort')) + ':' + concurrency + '</span>' : '';
      const overageBadge = renderOverageBadge(a);
      const banned = a.banStatus && a.banStatus !== 'ACTIVE';
      const idAttr = escapeAttr(a.id);
      const displayEmail = getDisplayEmail(a.email, a.id);
      const selectLabel = t('accounts.selectAccount', displayEmail);
      const successCount = Number(a.successCount ?? a.requestCount) || 0;
      const failureCount = Number(a.failureCount ?? a.errorCount) || 0;

      const refreshSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>';
      const userSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>';
      const copySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';

      return '' +
        '<div class="account-card' + (isSelected ? ' selected' : '') + '" data-id="' + idAttr + '">' +
        '<div class="account-header">' +
        '<div class="account-info">' +
        '<input type="checkbox" class="account-checkbox" ' + (isSelected ? 'checked' : '') + ' data-id="' + idAttr + '" aria-label="' + escapeAttr(selectLabel) + '" />' +
        '<div class="account-info-text">' +
        '<div class="account-email">' + escapeHtml(displayEmail) + '</div>' +
        '<div class="account-meta">' +
        getSubBadge(a.subscriptionType) +
        getTrialBadge(a) +
        weightBadge +
        concurrencyBadge +
        overageBadge +
        '<span class="badge badge-info">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + '</span>' +
        getStatusBadge(a) +
        '</div>' +
        '</div>' +
        '</div>' +
        '<div class="account-actions">' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="refresh" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refresh')) + '">' + refreshSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="detail" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.detail')) + '">' + userSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="copyJSON" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.copyJSON')) + '">' + copySvg + '</button>' +
        (banned ? '' :
          '<button class="btn btn-sm ' + (a.enabled ? 'btn-outline' : 'btn-primary') + '" data-action="toggle" data-id="' + idAttr + '" data-enabled="' + (!a.enabled) + '">' +
          escapeHtml(a.enabled ? t('accounts.disable') : t('accounts.enable')) +
          '</button>') +
        (a.authMethod === 'external_idp' ?
          '<button class="btn btn-sm btn-outline" data-action="switchProfile" data-id="' + idAttr + '">' + escapeHtml(t('kirosso.switchProfile')) + '</button>' : '') +
        '<button class="btn btn-sm btn-secondary" data-action="test" data-id="' + idAttr + '" id="test-' + idAttr + '">' + escapeHtml(t('accounts.test')) + '</button>' +
        '<button class="btn btn-sm btn-danger" data-action="delete" data-id="' + idAttr + '">' + escapeHtml(t('accounts.delete')) + '</button>' +
        '</div>' +
        '</div>' +
        (a.usageLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.mainQuota')) + '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + usageClass + '" data-usage-pct="' + escapeAttr(usagePct) + '"></div></div>' +
          '<div class="usage-text"><span>' + (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0) + '</span><span>' + usagePct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        (a.trialUsageLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.trialQuota')) + ' ' + escapeHtml(formatTrialExpiry(a.trialExpiresAt)) + '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + trialClass + '" data-usage-pct="' + escapeAttr(trialPct) + '"></div></div>' +
          '<div class="usage-text"><span>' + (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + (a.trialUsageLimit != null ? a.trialUsageLimit.toFixed(0) : 0) + '</span><span>' + trialPct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        '<div class="account-stats">' +
        '<div class="account-stat"><div class="account-stat-value account-stat-value--success">' + formatNum(successCount) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.successCount')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value account-stat-value--danger">' + formatNum(failureCount) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.failureCount')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + formatNum(a.totalTokens || 0) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.tokens')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + (a.totalCredits || 0).toFixed(1) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.credits')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + escapeHtml(formatTokenExpiry(a.expiresAt)) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.expiry')) + '</div></div>' +
        '</div>' +
        '</div>';
    }).join('');
    applyUsageBars(container);
    enhanceCustomSelects(container);
  }

  function kiroProfileListHtml(profiles, current) {
    const hasCurrent = !!current && profiles.some(p => p.arn === current);
    return profiles.map((profile, index) => {
      const isCurrent = !!current && profile.arn === current;
      const checked = hasCurrent ? isCurrent : index === 0;
      return '<label class="method-card kiro-profile-option">' +
        '<input type="radio" name="kiroProfilePick" value="' + escapeAttr(profile.arn) + '"' + (checked ? ' checked' : '') + ' />' +
        '<span class="method-body"><span class="method-title font-mono">' + escapeHtml(profile.region || '') +
        (isCurrent ? ' <span class="badge badge-info">' + escapeHtml(t('kirosso.currentProfile')) + '</span>' : '') +
        '</span><span class="method-desc font-mono">' + escapeHtml(profile.arn) + '</span></span>' +
        '</label>';
    }).join('');
  }

  async function openSwitchProfileModal(id, btn) {
    if (btn) btn.disabled = true;
    try {
      const res = await api('/accounts/' + id + '/kiro-profiles');
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !Array.isArray(data.profiles)) throw new Error(data.error || t('common.failed'));
      if (!data.profiles.some(profile => profile.arn !== (data.current || ''))) {
        toastWarning(t('kirosso.noAltProfiles'));
        return;
      }
      $('modalTitle').textContent = t('kirosso.switchProfile');
      $('modalBody').innerHTML =
        '<p class="help-block">' + escapeHtml(t('kirosso.switchProfileDesc')) + '</p>' +
        '<div class="method-list" id="switchProfileList">' + kiroProfileListHtml(data.profiles, data.current || '') + '</div>' +
        '<div class="modal-footer">' +
        '<button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
        '<button class="btn btn-primary" id="switchProfileConfirmBtn" type="button">' + escapeHtml(t('kirosso.useProfile')) + '</button>' +
        '</div>';
      openDialog('addModal');
      $('switchProfileConfirmBtn').addEventListener('click', async e => {
        const selected = document.querySelector('input[name="kiroProfilePick"]:checked');
        if (!selected || e.currentTarget.disabled) return;
        e.currentTarget.disabled = true;
        const update = await api('/accounts/' + id + '/kiro-profiles', {
          method: 'POST', body: JSON.stringify({ profileArn: selected.value })
        });
        const result = await update.json().catch(() => ({}));
        if (!update.ok || !result.success) {
          e.currentTarget.disabled = false;
          toastError(result.error || t('common.failed'));
          return;
        }
        closeModal();
        loadAccounts();
        toastPrimary(t(result.modelsRefreshed ? 'kirosso.switchSuccess' : 'kirosso.switchSuccessModelsPending'));
      });
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  // Account actions
  async function refreshAccount(id, card) {
    if (card) card.classList.add('loading');
    try {
      const res = await api('/accounts/' + id + '/refresh', { method: 'POST' });
      const d = await res.json();
      if (d.success) loadAccounts();
      else toastError(t('accounts.refreshFailed') + ': ' + (d.error || ''));
    } catch (e) {
      toastError(t('accounts.refreshFailed'));
    }
    if (card) card.classList.remove('loading');
  }
  async function toggleAccount(id, enabled) {
    await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ enabled }) });
    loadAccounts();
  }
  async function deleteAccount(id) {
    const ok = await confirmAction(t('accounts.confirmDelete'), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/accounts/' + id, { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('accounts.deleteSuccess'), 'danger', { icon: 'fa-solid fa-trash' });
      loadAccounts(); loadStats();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function copyAccountJSON(id, btn) {
    const password = window.prompt(t('security.reauthPassword'));
    if (!password) return;
    try {
      const jsonPromise = api('/accounts/' + id + '/credentials', {
        method: 'POST', body: JSON.stringify({ password })
      }).then(async res => {
        if (!res.ok) {
          const failure = await res.json().catch(() => ({}));
          throw new Error(failure.error || t('common.failed'));
        }
        const a = await res.json();
        const { clientId, clientSecret, accessToken, refreshToken, kiroApiKey, authMethod, provider, region, profileArn, tokenEndpoint, issuerUrl, scopes } = a;
        return JSON.stringify({ clientId, clientSecret, accessToken, refreshToken, kiroApiKey, authMethod, provider, region, profileArn, tokenEndpoint, issuerUrl, scopes }, null, 2);
      });
      if (await copyText(jsonPromise)) {
        flashCopySuccess(btn);
        toastPrimary(t('accounts.copyJSONSuccess'));
      }
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    }
  }
  function flashCopySuccess(btn) {
    if (!btn) return;
    const html = btn.innerHTML, cls = btn.className;
    btn.disabled = true;
    btn.className = 'btn btn-icon btn-sm btn-success';
    btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>';
    setTimeout(() => { btn.disabled = false; btn.className = cls; btn.innerHTML = html; }, 800);
  }

  // Batch actions
  async function batchAction(action) {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmKey = 'batch.confirm' + action.charAt(0).toUpperCase() + action.slice(1);
    const ok = await confirmAction(t(confirmKey, ids.length), {
      title: t('common.confirm'),
      confirmText: t('common.confirm'),
      variant: action === 'disable' ? 'danger' : 'primary'
    });
    if (!ok) return;
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/batch', { method: 'POST', body: JSON.stringify({ ids, action }) });
      const d = await res.json();
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      dismiss();
      if (action === 'refresh') {
        toast(t('batch.refreshResult', d.refreshed || 0, d.failed || 0), d.failed ? 'warning' : 'success');
      } else if (action === 'enable') {
        toast(t('batch.enableResult', d.count || ids.length), 'success');
      } else if (action === 'disable') {
        toast(t('batch.disableResult', d.count || ids.length), 'success');
      } else {
        toast(t('batch.done'), 'success');
      }
      selectedAccounts.clear();
      updateBatchBar();
      loadAccounts(); loadStats();
    } catch (e) {
      dismiss();
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function batchRefreshModels() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmRefreshModels', ids.length), {
      title: t('models.refreshAll'),
      confirmText: t('common.confirm')
    });
    if (!confirmed) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    let ok = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
        const d = await res.json();
        if (d.success) ok++; else fail++;
      } catch { fail++; }
    }
    dismiss();
    toast(t('batch.refreshModelsResult', ok, fail), fail ? 'warning' : 'success');
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts();
  }
  async function batchDelete() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmDelete', ids.length), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!confirmed) return;
    const dismiss = toast(t('batch.deleting'), 'info', { duration: 0 });
    let ok = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id, { method: 'DELETE' });
        const d = await res.json().catch(() => ({}));
        if (res.ok && d.success !== false) ok++; else fail++;
      } catch { fail++; }
    }
    dismiss();
    toast(t('batch.deleteResult', ok, fail), fail ? 'warning' : 'success', { icon: 'fa-solid fa-trash' });
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts(); loadStats();
  }
  async function refreshAllModels() {
    const ok = await confirmAction(t('models.confirmRefreshAll'), {
      title: t('models.refreshAll'),
      confirmText: t('models.refreshAll')
    });
    if (!ok) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      toast(t('models.refreshAllDone', d.refreshed || 0), 'success');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }
  async function refreshAccountModels(id) {
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      if (d.success) toast(t('detail.refreshModelCache') + ' · ' + (d.count || 0), 'success');
      else toast(t('common.failed') + (d.error ? ': ' + d.error : ''), 'error');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }

  // Detail modal
  function detailItem(label, value) {
    return '<div class="detail-item"><div class="detail-label">' + escapeHtml(label) + '</div><div class="detail-value">' + escapeHtml(value) + '</div></div>';
  }
  function showDetail(id) {
    const a = accountsData.find(x => x.id === id);
    if (!a) return;
    const idAttr = escapeAttr(id);
    $('detailBody').innerHTML =
      '<div class="detail-section"><h4>' + escapeHtml(t('detail.basicInfo')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.email'), getDisplayEmail(a.email, null)) +
      detailItem(t('detail.userId'), a.userId || '-') +
      detailItem(t('detail.authMethod'), formatAuthMethod(a.provider || a.authMethod)) +
      detailItem(t('detail.region'), a.region || 'us-east-1') +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.machineId')) + '</h4><div class="machine-id-row">' +
      '<input type="text" id="machineIdInput" value="' + escapeAttr(a.machineId || '') + '" placeholder="UUID" />' +
      '<button class="btn btn-sm btn-outline" id="generateMachineIdBtn" type="button">' + escapeHtml(t('detail.generate')) + '</button>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveMachineId" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.weight')) + '</h4>' +
      '<div class="form-group">' +
      '<input type="number" id="weightInput" value="' + (a.weight || 0) + '" min="0" max="100" />' +
      '<small>' + escapeHtml(t('detail.weightHint')) + '</small>' +
      '</div>' +
      '<div class="form-group">' +
      '<label>' + escapeHtml(t('detail.priority')) + '</label>' +
      '<input type="number" id="priorityInput" value="' + (a.priority || 0) + '" min="0" step="1" />' +
      '<small>' + escapeHtml(t('detail.priorityHint')) + '</small>' +
      '</div>' +
      '<div class="form-group">' +
      '<label>' + escapeHtml(t('detail.maxConcurrency')) + '</label>' +
      '<input type="number" id="maxConcurrencyInput" value="' + (a.maxConcurrency || 0) + '" min="0" max="1000" step="1" />' +
      '<small>' + escapeHtml(t('detail.maxConcurrencyHint')) + '</small>' +
      '</div>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveWeight" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div>' +

      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.overage')) +
      ' <button class="btn btn-sm btn-outline" data-detail-action="refreshOverage" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.overageRefresh')) + '</button>' +
      '</h4>' +
      '<p class="help-block">' + escapeHtml(t('detail.overageHint')) + '</p>' +
      renderOverageBlock(a, idAttr) +
      '</div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.proxyURL')) + '</h4><div class="machine-id-row">' +
      '<input type="text" id="proxyURLInput" value="' + escapeAttr(a.proxyURL || '') + '" placeholder="socks5://host:port 或 direct" />' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveProxyURL" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div><p class="help-block">' + escapeHtml(t('detail.proxyHint')) + '</p></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.subscription')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.subscriptionType'), a.subscriptionTitle || (a.subscriptionType ? formatSubscriptionLabel(a.subscriptionType) : '-')) +
      detailItem(t('detail.tokenExpiry'), a.expiresAt ? new Date(a.expiresAt * 1000).toLocaleString() : '-') +
      detailItem(t('detail.mainQuota'), (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0)) +
      detailItem(t('detail.resetDate'), a.nextResetDate || '-') +
      (a.trialUsageLimit > 0 ?
        detailItem(t('detail.trialQuota'), (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + a.trialUsageLimit.toFixed(0)) +
        detailItem(t('detail.trialStatus'), a.trialStatus || '-') +
        detailItem(t('detail.trialExpiry'), a.trialExpiresAt ? new Date(a.trialExpiresAt * 1000).toLocaleString() : '-')
        : '') +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.statistics')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.successCount'), Number(a.successCount ?? a.requestCount) || 0) +
      detailItem(t('detail.failureCount'), Number(a.failureCount ?? a.errorCount) || 0) +
      detailItem(t('detail.totalTokens'), formatNum(a.totalTokens || 0)) +
      detailItem(t('detail.totalCredits'), (a.totalCredits || 0).toFixed(2)) +
      '</div></div>' +

      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.models')) +
      ' <button class="btn btn-sm btn-outline" data-detail-action="loadModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.loadModels')) + '</button>' +
      ' <button class="btn btn-sm btn-outline" data-detail-action="refreshModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.refreshModelCache')) + '</button>' +
      '</h4>' +
      '<div id="modelsList" class="model-list"></div>' +
      '</div>';

    openDialog('detailModal');
  }
  async function loadModels(id) {
    const c = $('modelsList');
    c.innerHTML = '<p class="empty-state">' + escapeHtml(t('detail.loading')) + '</p>';
    try {
      const res = await api('/accounts/' + id + '/models');
      const d = await res.json();
      if (d.success && d.models) {
        const sorted = d.models.slice().sort((a, b) => {
          if (a.modelId === 'auto') return -1;
          if (b.modelId === 'auto') return 1;
          return (a.rateMultiplier || 1) - (b.rateMultiplier || 1);
        });
        c.innerHTML = sorted.map(m => {
          const ratio = m.rateMultiplier || 1;
          return '<div class="model-item">' +
            '<div class="model-name">' + escapeHtml(m.modelId) + '</div>' +
            '<div class="model-credit"><span class="credit-ratio">' + escapeHtml(t('detail.creditMultiplier', ratio)) + '</span></div>' +
            '<div class="model-info">' + escapeHtml(m.description || '') + '</div>' +
            '</div>';
        }).join('') || '<p class="empty-state">' + escapeHtml(t('detail.noModels')) + '</p>';
      } else {
        c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + ': ' + escapeHtml(d.error || '') + '</p>';
        toast(t('detail.loadFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + '</p>';
      toast(t('detail.loadFailed'), 'error');
    }
  }
  async function generateMachineId() {
    try {
      const res = await api('/generate-machine-id');
      const d = await res.json();
      if (d.machineId) $('machineIdInput').value = d.machineId;
    } catch (e) {
      toast(t('detail.generateFailed'), 'error');
    }
  }
  async function putAccount(id, body, successMsg) {
    try {
      const res = await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify(body) });
      const d = await res.json();
      if (d.success) {
        toast(successMsg, 'success');
        loadAccounts();
      } else {
        toast(t('detail.saveFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      toast(t('detail.saveFailed'), 'error');
    }
  }
  async function saveMachineId(id) {
    const m = $('machineIdInput').value.trim();
    if (m && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(m) && !/^[0-9a-f]{32}$/i.test(m)) {
      toast(t('detail.machineIdError'), 'warning'); return;
    }
    await putAccount(id, { machineId: m }, t('detail.saved'));
  }
  async function saveWeight(id) {
    const weight = parseInt($('weightInput').value, 10) || 0;
    const priority = parseInt($('priorityInput').value, 10) || 0;
    const maxConcurrency = parseInt($('maxConcurrencyInput').value, 10) || 0;
    if (maxConcurrency < 0 || maxConcurrency > 1000) {
      toast(t('detail.maxConcurrencyInvalid'), 'warning'); return;
    }
    await putAccount(id, { weight, priority, maxConcurrency }, t('detail.saved'));
  }
  function renderOverageBadge(a) {
    const status = (a.overageStatus || '').toUpperCase();
    if (status === 'ENABLED') {
      return '<span class="badge badge-warning">' + escapeHtml(t('accounts.overageOn')) + '</span>';
    }
    if (status === 'DISABLED') {
      return '<span class="badge badge-muted">' + escapeHtml(t('accounts.overageOff')) + '</span>';
    }
    return '';
  }
  function renderOverageBlock(a, idAttr) {
    const status = (a.overageStatus || '').toUpperCase();
    const capable = !a.overageCapability || a.overageCapability === 'OVERAGE_CAPABLE';
    const checked = status === 'ENABLED';
    const checkedAt = a.overageCheckedAt ? new Date(a.overageCheckedAt * 1000).toLocaleString() : '-';
    const statusText = status === 'ENABLED' ? t('detail.overageEnabled')
      : status === 'DISABLED' ? t('detail.overageDisabled')
      : t('detail.overageUnknown');
    const disabledAttr = capable ? '' : ' disabled';
    return '<div class="form-group flex items-center gap-2">' +
      '<label class="switch"><input type="checkbox" id="overageSwitchInput-' + idAttr + '" data-detail-action="toggleOverage" data-id="' + idAttr + '" ' + (checked ? 'checked' : '') + disabledAttr + ' /><span class="slider"></span></label>' +
      '<span id="overageSwitchLabel-' + idAttr + '">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      (capable ? '' : '<p class="help-block" style="color:#ef4444">' + escapeHtml(t('detail.overageNotCapable')) + '</p>') +
      '<div class="detail-grid">' +
      detailItem(t('detail.overageStatus'), status || '-') +
      detailItem(t('detail.overageCap'), a.overageCap ? '$' + Number(a.overageCap).toFixed(2) : '-') +
      detailItem(t('detail.overageRate'), a.overageRate ? '$' + Number(a.overageRate).toFixed(4) : '-') +
      detailItem(t('detail.overageCurrent'), a.currentOverages ? '$' + Number(a.currentOverages).toFixed(4) : '$0') +
      detailItem(t('detail.overageCheckedAt'), checkedAt) +
      '</div>';
  }
  async function toggleOverageSwitch(id, inputEl) {
    const desired = inputEl.checked;
    const labelEl = $('overageSwitchLabel-' + id);
    const oldLabel = labelEl ? labelEl.textContent : '';
    inputEl.disabled = true;
    if (labelEl) labelEl.textContent = t('detail.overageSwitching');
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', {
        method: 'POST',
        body: JSON.stringify({ enabled: desired }),
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      if (labelEl) {
        labelEl.textContent = d.overageStatus === 'ENABLED' ? t('detail.overageEnabled')
          : d.overageStatus === 'DISABLED' ? t('detail.overageDisabled')
          : t('detail.overageUnknown');
      }
      inputEl.checked = d.overageStatus === 'ENABLED';
      await loadAccounts();
    } catch (e) {
      inputEl.checked = !desired;
      if (labelEl) labelEl.textContent = oldLabel;
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    } finally {
      inputEl.disabled = false;
    }
  }
  async function refreshAccountOverage(id) {
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', { method: 'GET' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      await loadAccounts();
      showDetail(id);
    } catch (e) {
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    }
  }
  async function saveProxyURL(id) {
    const url = $('proxyURLInput').value.trim();
    if (url && url.toLowerCase() !== 'direct' && !/^(socks5|socks5h|http|https):\/\//.test(url)) {
      toast(t('detail.proxyFormatError'), 'warning'); return;
    }
    await putAccount(id, { proxyURL: url }, t('detail.proxySaved'));
  }
  function closeDetailModal() { closeDialog('detailModal'); }

  // Test flow
  function getTestAccount(id) {
    return accountsData.find(a => a.id === id) || null;
  }
  function getTestModelValue() {
    const choice = $('testModelChoice');
    return (choice && choice.value.trim()) || 'claude-sonnet-4';
  }
  function renderTestLog() {
    const c = $('testModalLog');
    if (!c) return;
    if (!testLogs.length) {
      c.innerHTML = '<div class="test-log-empty">' + escapeHtml(t('accounts.testLog.empty')) + '</div>';
      return;
    }
    c.innerHTML = testLogs.map(log =>
      '<div class="test-log-line ' + escapeAttr(log.type || 'info') + '">' +
      '<span class="test-log-time">' + escapeHtml(log.time) + '</span>' +
      '<span class="test-log-message">' + escapeHtml(log.msg) + '</span>' +
      '</div>'
    ).join('');
    c.scrollTop = c.scrollHeight;
  }
  function addTestLog(msg, type) {
    const time = new Date().toLocaleTimeString();
    testLogs.push({ time, msg, type });
    if (testLogs.length > 100) testLogs.shift();
    renderTestLog();
  }
  function clearTestLog() {
    testLogs = [];
    renderTestLog();
  }
  function renderTestModal() {
    const body = $('testBody');
    if (!body) return;
    const acc = getTestAccount(testModalAccountId);
    const idAttr = escapeAttr(testModalAccountId);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : testModalAccountId;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    const statusText = testModalLoadingModels
      ? t('accounts.testModelsLoading')
      : testModalModelError
        ? t('accounts.testModelsFallback')
        : t('accounts.testModelsReady', testModalModels.length);
    const modelField = testModalLoadingModels
      ? '<div class="test-model-loading">' + escapeHtml(t('accounts.testModelsLoading')) + '</div>'
      : testModalModels.length
        ? '<select id="testModelChoice">' +
        testModalModels.map(m => '<option value="' + escapeAttr(m) + '">' + escapeHtml(m) + '</option>').join('') +
        '</select>'
        : '<input type="text" id="testModelChoice" placeholder="claude-sonnet-4" value="claude-sonnet-4" />';

    body.innerHTML =
      '<div class="test-modal-account">' +
      '<div class="test-modal-account-main">' +
      '<div class="test-modal-email">' + escapeHtml(email) + '</div>' +
      '<div class="test-modal-meta">' +
      '<span>' + escapeHtml(formatAuthMethod(acc && (acc.provider || acc.authMethod))) + '</span>' +
      '<span>' + escapeHtml(proxy) + '</span>' +
      '</div>' +
      '</div>' +
      '<span class="test-modal-status">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      '<div class="test-modal-grid">' +
      '<div class="form-group test-model-field">' +
      '<label for="testModelChoice">' + escapeHtml(t('accounts.selectModel')) + '</label>' +
      modelField +
      '</div>' +
      '<div class="test-log-card">' +
      '<div class="test-log-header">' +
      '<span class="test-log-title">' + escapeHtml(t('accounts.testLog.title')) + '</span>' +
      '<button class="btn btn-xs btn-outline test-log-clear" id="testLogClear" type="button">' + escapeHtml(t('accounts.testLog.clear')) + '</button>' +
      '</div>' +
      '<div class="test-log-content" id="testModalLog"></div>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="testModalCancelBtn" type="button">' + escapeHtml(t('common.close')) + '</button>' +
      '<button class="btn btn-primary" id="testRunBtn" data-id="' + idAttr + '" type="button" ' + (testModalLoadingModels ? 'disabled' : '') + '>' + escapeHtml(t('accounts.test')) + '</button>' +
      '</div>';

    if (!testModalLoadingModels) enhanceCustomSelects(body);
    renderTestLog();
  }
  async function testAccount(id) {
    testModalAccountId = id;
    testModalModels = [];
    testModalLoadingModels = true;
    testModalModelError = false;
    testModalRunning = false;
    testLogs = [];
    renderTestModal();
    openDialog('testModal');
    try {
      const res = await api('/accounts/' + id + '/models/cached');
      const d = await res.json();
      testModalModels = Array.isArray(d.models) ? d.models.slice().sort() : [];
    } catch (e) {
      testModalModelError = true;
    } finally {
      testModalLoadingModels = false;
      renderTestModal();
    }
  }
  function closeTestModal() {
    closeAllCustomSelects();
    closeDialog('testModal');
  }
  async function runTestAccount(id, model) {
    if (testModalRunning) return;
    testModalRunning = true;
    const modalBtn = $('testRunBtn');
    if (modalBtn) modalBtn.setAttribute('aria-busy', 'true');
    const acc = accountsData.find(a => a.id === id);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : id;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    addTestLog(t('accounts.testLog.start', email, model, proxy), 'info');
    try {
      const startTime = Date.now();
      const res = await api('/accounts/' + id + '/test', { method: 'POST', body: JSON.stringify({ model }) });
      const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
      const d = await res.json();
      renderAccountTestChecks(d.checks);
      if (d.success) {
        addTestLog(t('accounts.testLog.success', email, elapsed, d.reply), 'ok');
      } else {
        addTestLog(t('accounts.testLog.failed', email, elapsed, d.error || t('common.unknownError')), 'err');
      }
    } catch (e) {
      addTestLog(t('accounts.testLog.error', email, e.message), 'err');
    }
    testModalRunning = false;
    if (modalBtn) modalBtn.removeAttribute('aria-busy');
  }
  function renderAccountTestChecks(checks) {
    if (!Array.isArray(checks)) return;
    for (const check of checks) {
      const label = [check.name, check.endpoint].filter(Boolean).join(' / ');
      if (check.success) {
        const details = [];
        if (check.statusCode) details.push('HTTP ' + check.statusCode);
        if (check.count != null) details.push('count=' + check.count);
        if (check.subscriptionTitle) details.push(check.subscriptionTitle);
        if (check.usageLimit) details.push('usage=' + check.usageCurrent + '/' + check.usageLimit);
        addTestLog(label + ' ok' + (details.length ? ' (' + details.join(', ') + ')' : ''), 'ok');
      } else {
        const details = [];
        if (check.statusCode) details.push('HTTP ' + check.statusCode);
        if (check.error) details.push(check.error);
        addTestLog(label + ' failed' + (details.length ? ': ' + details.join(' ') : ''), 'err');
      }
    }
  }

  // Settings
  async function loadSettings() {
    const res = await api('/settings');
    const d = await res.json();
    $('requireApiKey').checked = d.requireApiKey;
    $('allowOverUsage').checked = d.allowOverUsage || false;
    await Promise.all([
      loadRuntimeConfig(),
      loadRoutingConfig(),
      loadAutoRefreshConfig(),
      loadRetryConfig(),
      loadResponsesStorageConfig(),
      loadModelRegistryConfig(),
      loadHealthConfig(),
      loadThinkingConfig(),
      loadEndpointConfig(),
      loadProxyConfig(),
      loadUpstreamProtectionConfig(),
      loadPromptCacheConfig(),
      loadDiagnosticsConfig(),
      loadRequestLogConfig(),
      loadCountTokensProviderConfig(),
      loadWebSearchConfig(),
      loadPromptFilter(),
      loadApiKeys()
    ]);
    refreshCustomSelects();
  }
  async function loadRuntimeConfig() {
    const res = await api('/runtime-config');
    const d = await res.json();
    $('runtimeHost').value = d.host || '0.0.0.0';
    $('runtimePort').value = d.port || 8080;
    $('runtimeLogLevel').value = d.logLevel || d.activeLogLevel || 'info';
    $('runtimeKiroVersion').value = d.kiroVersion || '';
    $('runtimeSystemVersion').value = d.systemVersion || '';
    $('runtimeNodeVersion').value = d.nodeVersion || '';
    $('runtimeActiveAddress').textContent = d.externallyManaged
      ? t('settings.managedListenAddress', d.activeHost || d.host, d.activePort || d.port)
      : t('settings.activeListenAddress', d.activeHost || d.host, d.activePort || d.port);
  }
  async function saveRuntimeConfig() {
    const host = $('runtimeHost').value.trim();
    const port = Number($('runtimePort').value);
    const logLevel = $('runtimeLogLevel').value;
    if (!host) { toast(t('settings.hostRequired'), 'warning'); return; }
    if (!Number.isFinite(port) || port < 1 || port > 65535) { toast(t('settings.portInvalid'), 'warning'); return; }
    const res = await api('/runtime-config', {
      method: 'POST',
      body: JSON.stringify({
        host,
        port: Math.round(port),
        logLevel,
        kiroVersion: $('runtimeKiroVersion').value.trim(),
        systemVersion: $('runtimeSystemVersion').value.trim(),
        nodeVersion: $('runtimeNodeVersion').value.trim()
      })
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(d.restartRequired ? t('settings.runtimeSavedRestart') : t('settings.runtimeSaved'), d.restartRequired ? 'warning' : 'success');
    if (d.config) loadRuntimeConfig();
  }
  async function loadRoutingConfig() {
    const res = await api('/routing-config');
    const d = await res.json();
    $('loadBalancingMode').value = d.loadBalancingMode || 'weighted';
  }
  async function saveRoutingConfig() {
    const res = await api('/routing-config', {
      method: 'POST',
      body: JSON.stringify({ loadBalancingMode: $('loadBalancingMode').value })
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.routingSaved'), 'success');
    loadRoutingConfig();
  }
  async function loadAutoRefreshConfig() {
    const res = await api('/auto-refresh');
    const d = await res.json();
    $('autoRefreshEnabled').checked = d.enabled !== false;
    $('autoRefreshIntervalMinutes').value = d.intervalMinutes || 30;
    $('autoRefreshBeforeSeconds').value = d.tokenRefreshBeforeSeconds || 120;
    $('autoRefreshMaxAccounts').value = d.maxAccountsPerRun || 0;
    $('autoRefreshConcurrency').value = d.refreshConcurrency || 5;
    $('autoRefreshFailureCooldown').value = d.failureCooldownSeconds || 300;
    $('autoRefreshQueueCapacity').value = d.refreshQueueCapacity || 1000;
    $('autoRefreshTaskTimeoutSeconds').value = d.refreshTaskTimeoutSeconds || 60;
    $('autoRefreshJitterSeconds').value = d.refreshJitterSeconds ?? 30;
    $('autoRefreshModels').checked = d.refreshModels !== false;
    $('autoRefreshModelIntervalMinutes').value = d.modelIntervalMinutes || 60;
    $('autoRefreshMaxModelsPerRun').value = d.maxModelsPerRun ?? 25;
    $('autoRefreshModelConcurrency').value = d.modelRefreshConcurrency || 3;
    loadAutoRefreshStatus();
  }
  async function saveAutoRefreshConfig() {
    const intervalMinutes = Math.round(Number($('autoRefreshIntervalMinutes').value) || 0);
    const beforeSeconds = Math.round(Number($('autoRefreshBeforeSeconds').value) || 0);
    const maxAccounts = Math.round(Number($('autoRefreshMaxAccounts').value) || 0);
    const concurrency = Math.round(Number($('autoRefreshConcurrency').value) || 0);
    const cooldown = Math.round(Number($('autoRefreshFailureCooldown').value) || 0);
    const queueCapacity = Math.round(Number($('autoRefreshQueueCapacity').value) || 0);
    const taskTimeout = Math.round(Number($('autoRefreshTaskTimeoutSeconds').value) || 0);
    const jitterSeconds = Math.round(Number($('autoRefreshJitterSeconds').value) || 0);
    const modelIntervalMinutes = Math.round(Number($('autoRefreshModelIntervalMinutes').value) || 0);
    const maxModelsPerRun = Math.round(Number($('autoRefreshMaxModelsPerRun').value) || 0);
    const modelRefreshConcurrency = Math.round(Number($('autoRefreshModelConcurrency').value) || 0);
    if (intervalMinutes < 1 || beforeSeconds < 60 || cooldown < 60 || maxAccounts < 0 || concurrency < 1 || concurrency > 50 || queueCapacity < 1 || queueCapacity > 100000 || taskTimeout < 10 || taskTimeout > 600 || jitterSeconds < 0 || jitterSeconds > 3600 || modelIntervalMinutes < 30 || modelIntervalMinutes > 10080 || maxModelsPerRun < 0 || modelRefreshConcurrency < 1 || modelRefreshConcurrency > 20) {
      toast(t('settings.autoRefreshInvalid'), 'warning');
      return;
    }
    const res = await api('/auto-refresh', {
      method: 'POST',
      body: JSON.stringify({
        enabled: $('autoRefreshEnabled').checked,
        intervalMinutes,
        tokenRefreshBeforeSeconds: beforeSeconds,
        maxAccountsPerRun: maxAccounts,
        refreshConcurrency: concurrency,
        failureCooldownSeconds: cooldown,
        refreshQueueCapacity: queueCapacity,
        refreshTaskTimeoutSeconds: taskTimeout,
        refreshJitterSeconds: jitterSeconds,
        refreshModels: $('autoRefreshModels').checked,
        modelIntervalMinutes,
        maxModelsPerRun,
        modelRefreshConcurrency
      })
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.autoRefreshSaved'), 'success');
    loadAutoRefreshConfig();
  }
  async function loadRetryConfig() {
    const res = await api('/retry');
    const d = await res.json();
    $('retryMaxAccountAttempts').value = d.maxAccountAttempts ?? 8;
    $('retryMaxUpstreamAttempts').value = d.maxUpstreamAttempts || 12;
    $('retryFirstTokenTimeoutSeconds').value = d.firstTokenTimeoutSeconds || 45;
    $('retryStreamIdleTimeoutSeconds').value = d.streamIdleTimeoutSeconds || 120;
    $('retryEmptyResponseRetries').value = d.emptyResponseRetries ?? 2;
	$('retryEndpointFailureThreshold').value = d.endpointFailureThreshold || 3;
	$('retryEndpointCircuitCooldownSeconds').value = d.endpointCircuitCooldownSeconds || 30;
	$('retryProxyFailureThreshold').value = d.proxyFailureThreshold || 3;
	$('retryProxyCircuitCooldownSeconds').value = d.proxyCircuitCooldownSeconds || 60;
  }
  async function saveRetryConfig() {
    const body = {
      maxAccountAttempts: Math.round(Number($('retryMaxAccountAttempts').value) || 0),
      maxUpstreamAttempts: Math.round(Number($('retryMaxUpstreamAttempts').value) || 0),
      firstTokenTimeoutSeconds: Math.round(Number($('retryFirstTokenTimeoutSeconds').value) || 0),
      streamIdleTimeoutSeconds: Math.round(Number($('retryStreamIdleTimeoutSeconds').value) || 0),
	  emptyResponseRetries: Math.round(Number($('retryEmptyResponseRetries').value) || 0),
	  endpointFailureThreshold: Math.round(Number($('retryEndpointFailureThreshold').value) || 0),
	  endpointCircuitCooldownSeconds: Math.round(Number($('retryEndpointCircuitCooldownSeconds').value) || 0),
	  proxyFailureThreshold: Math.round(Number($('retryProxyFailureThreshold').value) || 0),
	  proxyCircuitCooldownSeconds: Math.round(Number($('retryProxyCircuitCooldownSeconds').value) || 0)
    };
	if (body.maxAccountAttempts < 0 || body.maxAccountAttempts > 100 || body.maxUpstreamAttempts < 1 || body.maxUpstreamAttempts > 200 || body.firstTokenTimeoutSeconds < 5 || body.firstTokenTimeoutSeconds > 600 || body.streamIdleTimeoutSeconds < 15 || body.streamIdleTimeoutSeconds > 3600 || body.emptyResponseRetries < 0 || body.emptyResponseRetries > 20 || body.endpointFailureThreshold < 1 || body.endpointFailureThreshold > 20 || body.endpointCircuitCooldownSeconds < 5 || body.endpointCircuitCooldownSeconds > 900 || body.proxyFailureThreshold < 1 || body.proxyFailureThreshold > 20 || body.proxyCircuitCooldownSeconds < 5 || body.proxyCircuitCooldownSeconds > 900) {
      toast(t('settings.retryInvalid'), 'warning');
      return;
    }
    const res = await api('/retry', { method: 'POST', body: JSON.stringify(body) });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.retrySaved'), 'success');
    loadRetryConfig();
  }
  async function loadResponsesStorageConfig() {
    const res = await api('/responses-storage');
    const d = await res.json();
    $('responsesDefaultStore').checked = d.defaultStore === true;
    $('responsesTTLHours').value = d.ttlHours || 720;
    $('responsesMaxFiles').value = d.maxFiles || 10000;
    $('responsesMaxMegabytes').value = Math.round((d.maxBytes || 1073741824) / 1048576);
    $('responsesMaxHistoryMegabytes').value = (d.maxHistoryBytes || 4194304) / 1048576;
    $('responsesGCIntervalMinutes').value = d.gcIntervalMinutes || 60;
    $('responsesDefaultStore').disabled = !d.encryptionEnabled;
    $('responsesStorageStatus').textContent = d.encryptionEnabled
      ? t('settings.responsesStorageReady', d.fileCount || 0, ((d.storedBytes || 0) / 1048576).toFixed(2))
      : t('settings.responsesStorageKeyMissing');
  }
  async function saveResponsesStorageConfig() {
    const ttlHours = Math.round(Number($('responsesTTLHours').value) || 0);
    const maxFiles = Math.round(Number($('responsesMaxFiles').value) || 0);
    const maxMegabytes = Number($('responsesMaxMegabytes').value);
    const maxHistoryMegabytes = Number($('responsesMaxHistoryMegabytes').value);
    const gcIntervalMinutes = Math.round(Number($('responsesGCIntervalMinutes').value) || 0);
    const maxBytes = Math.round(maxMegabytes * 1048576);
    const maxHistoryBytes = Math.round(maxHistoryMegabytes * 1048576);
    if (ttlHours < 1 || ttlHours > 8760 || maxFiles < 1 || maxFiles > 1000000 ||
      !Number.isFinite(maxMegabytes) || maxBytes < 1048576 || maxBytes > 1099511627776 ||
      !Number.isFinite(maxHistoryMegabytes) || maxHistoryBytes < 65536 || maxHistoryBytes > 67108864 ||
      gcIntervalMinutes < 1 || gcIntervalMinutes > 1440) {
      toast(t('settings.responsesStorageInvalid'), 'warning');
      return;
    }
    const res = await api('/responses-storage', {
      method: 'POST',
      body: JSON.stringify({
        defaultStore: $('responsesDefaultStore').checked,
        ttlHours,
        maxFiles,
        maxBytes,
        maxHistoryBytes,
        gcIntervalMinutes
      })
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.responsesStorageSaved'), 'success');
    loadResponsesStorageConfig();
  }
  async function purgeResponsesStorage() {
    if (!confirm(t('settings.purgeResponsesConfirm'))) return;
    const res = await api('/responses-storage', { method: 'DELETE' });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.purgeResponsesDone', d.deletedFiles || 0), 'success');
    loadResponsesStorageConfig();
  }
  async function loadModelRegistryConfig() {
    const res = await api('/model-registry');
    const d = await res.json();
    $('modelNegativeCacheTTLSeconds').value = d.negativeCacheTtlSeconds || 3600;
    $('modelRegistryJson').value = JSON.stringify(Array.isArray(d.models) ? d.models : [], null, 2);
  }
  async function saveModelRegistryConfig() {
    const ttl = Math.round(Number($('modelNegativeCacheTTLSeconds').value) || 0);
    let models;
    try {
      models = JSON.parse($('modelRegistryJson').value || '[]');
      if (!Array.isArray(models)) throw new Error('models must be an array');
    } catch (e) {
      toast(t('settings.modelRegistryInvalid') + ': ' + e.message, 'warning');
      return;
    }
    if (ttl < 60 || ttl > 604800) {
      toast(t('settings.modelRegistryInvalid'), 'warning');
      return;
    }
    const res = await api('/model-registry', {
      method: 'POST',
      body: JSON.stringify({ negativeCacheTtlSeconds: ttl, models })
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.modelRegistrySaved'), 'success');
    loadModelRegistryConfig();
  }
  async function loadHealthConfig() {
    const res = await api('/health-config');
    const d = await res.json();
    $('healthMinReadyAccounts').value = d.minReadyAccounts ?? 1;
    $('healthMinReadyRatio').value = Math.round((d.minReadyRatio || 0) * 100);
    $('healthWebhookEnabled').checked = d.webhookEnabled === true;
    $('healthWebhookURL').value = d.webhookUrl || '';
    $('healthWebhookCooldownSeconds').value = d.webhookCooldownSeconds || 300;
  }
  async function saveHealthConfig() {
    const body = {
      minReadyAccounts: Math.round(Number($('healthMinReadyAccounts').value) || 0),
      minReadyRatio: Math.max(0, Math.min(100, Number($('healthMinReadyRatio').value) || 0)) / 100,
      webhookEnabled: $('healthWebhookEnabled').checked,
      webhookUrl: $('healthWebhookURL').value.trim(),
      webhookCooldownSeconds: Math.round(Number($('healthWebhookCooldownSeconds').value) || 0)
    };
    if (body.minReadyAccounts < 0 || body.webhookCooldownSeconds < 10 || body.webhookCooldownSeconds > 86400 || (body.webhookEnabled && !/^https?:\/\//i.test(body.webhookUrl))) {
      toast(t('settings.healthInvalid'), 'warning');
      return;
    }
    const res = await api('/health-config', { method: 'POST', body: JSON.stringify(body) });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.healthSaved'), 'success');
    loadHealthConfig();
  }
  async function loadAutoRefreshStatus() {
    const el = $('autoRefreshStatus');
    if (!el) return;
    try {
      const res = await api('/auto-refresh/status');
      const d = await res.json();
      renderAutoRefreshStatus(d.status || {});
    } catch (e) {
      el.innerHTML = '<small class="danger-text">' + escapeHtml(e.message) + '</small>';
    }
  }
  function renderAutoRefreshStatus(status) {
    const el = $('autoRefreshStatus');
    if (!el) return;
    const recent = Object.values(status.recent || {});
    const problemItems = recent
      .filter(item => item.status === 'failed' || item.cooldownUntil)
      .sort((a, b) => (b.lastFailureAt || b.lastStartedAt || 0) - (a.lastFailureAt || a.lastStartedAt || 0))
      .slice(0, 5);
    const finished = status.lastRunFinishedAt ? formatTime(status.lastRunFinishedAt) : '-';
    let html =
      '<div class="request-summary auto-refresh-summary">' +
      '<div class="request-summary-item"><span>' + escapeHtml(t('settings.autoRefreshLastRun')) + '</span><strong>' + escapeHtml(status.running ? t('settings.autoRefreshRunning') : finished) + '</strong></div>' +
      '<div class="request-summary-item"><span>' + escapeHtml(t('settings.autoRefreshSelected')) + '</span><strong>' + escapeHtml(String(status.lastRunSelected || 0)) + '</strong></div>' +
      '<div class="request-summary-item"><span>' + escapeHtml(t('settings.autoRefreshResult')) + '</span><strong>' + escapeHtml((status.lastRunSuccess || 0) + '/' + (status.lastRunFailed || 0) + '/' + (status.lastRunSkipped || 0)) + '</strong></div>' +
      '<div class="request-summary-item"><span>' + escapeHtml(t('settings.autoRefreshCooldowns')) + '</span><strong>' + escapeHtml(String(status.cooldownCount || 0)) + '</strong></div>' +
      '</div>';
    if (problemItems.length) {
      html += '<div class="auto-refresh-problems">';
      for (const item of problemItems) {
        const label = item.email || item.accountId || '-';
        const reason = item.error || item.reason || '';
        const cooldown = item.cooldownUntil ? ' · ' + t('settings.autoRefreshCooldownUntil', formatTime(item.cooldownUntil)) : '';
        html += '<div class="auto-refresh-problem"><span class="badge ' + (item.status === 'failed' ? 'badge-error' : 'badge-warning') + '">' + escapeHtml(item.status || '-') + '</span><span class="auto-refresh-problem-main">' + escapeHtml(label) + '</span><span class="muted-text">' + escapeHtml(reason + cooldown) + '</span></div>';
      }
      html += '</div>';
    }
    el.innerHTML = html;
  }
  async function loadThinkingConfig() {
    const res = await api('/thinking');
    const d = await res.json();
    $('thinkingSuffix').value = d.suffix || '-thinking';
    $('openaiThinkingFormat').value = d.openaiFormat || 'reasoning_content';
    $('claudeThinkingFormat').value = d.claudeFormat || 'thinking';
    $('thinkingDefaultBudget').value = d.defaultBudgetTokens || 4000;
    $('thinkingBudgetCap').value = d.budgetCapTokens == null ? 10000 : d.budgetCapTokens;
    $('defaultMaxOutputTokens').value = d.defaultMaxOutputTokens || 0;
    $('defaultContextWindowTokens').value = d.defaultContextWindowTokens || 0;
    $('bufferToolStreams').checked = d.bufferToolStreams !== false;
    $('enforceAgentToolUse').checked = d.enforceAgentToolUse !== false;
  }
  async function saveThinkingConfig() {
    const defaultBudgetTokens = Math.round(Number($('thinkingDefaultBudget').value) || 0);
    const budgetCapTokens = Math.round(Number($('thinkingBudgetCap').value) || 0);
    const defaultMaxOutputTokens = Math.round(Number($('defaultMaxOutputTokens').value) || 0);
    const defaultContextWindowTokens = Math.round(Number($('defaultContextWindowTokens').value) || 0);
    if (defaultBudgetTokens < 1024 || defaultBudgetTokens > 200000 || budgetCapTokens < 0 || budgetCapTokens > 200000 || (budgetCapTokens > 0 && (budgetCapTokens < 1024 || defaultBudgetTokens > budgetCapTokens))) {
      toast(t('settings.thinkingBudgetInvalid'), 'warning');
      return;
    }
    if (defaultMaxOutputTokens < 0 || defaultMaxOutputTokens > 1000000 || defaultContextWindowTokens < 0 || defaultContextWindowTokens > 10000000 || (defaultContextWindowTokens > 0 && defaultContextWindowTokens < 1024)) {
      toast(t('settings.tokenBudgetInvalid'), 'warning');
      return;
    }
    const res = await api('/thinking', {
      method: 'POST', body: JSON.stringify({
        suffix: $('thinkingSuffix').value || '-thinking',
        openaiFormat: $('openaiThinkingFormat').value,
        claudeFormat: $('claudeThinkingFormat').value,
        defaultBudgetTokens,
        budgetCapTokens,
        defaultMaxOutputTokens,
        defaultContextWindowTokens,
        bufferToolStreams: $('bufferToolStreams').checked,
        enforceAgentToolUse: $('enforceAgentToolUse').checked
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.thinkingSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadEndpointConfig() {
    const res = await api('/endpoint');
    const d = await res.json();
    $('preferredEndpoint').value = d.preferredEndpoint || 'auto';
    $('endpointFallback').checked = d.endpointFallback !== false;
  }
  async function saveEndpointConfig() {
    const res = await api('/endpoint', {
      method: 'POST', body: JSON.stringify({
        preferredEndpoint: $('preferredEndpoint').value,
        endpointFallback: $('endpointFallback').checked
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.endpointSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadProxyConfig() {
    const res = await api('/proxy');
    const d = await res.json();
    const url = d.proxyURL || '';
    if (!url) {
      $('proxyType').value = 'environment';
      $('proxyFields').classList.add('hidden');
      return;
    }
    if (url.toLowerCase() === 'direct') {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
      return;
    }
    try {
      const u = new URL(url);
      const scheme = u.protocol.replace(':', '');
      $('proxyType').value = scheme.startsWith('socks5') ? 'socks5' : 'http';
      $('proxyHost').value = u.hostname;
      $('proxyPort').value = u.port;
      $('proxyUsername').value = decodeURIComponent(u.username);
	  $('proxyPassword').value = '';
	  $('proxyPassword').dataset.configured = d.proxyPasswordSet ? 'true' : 'false';
      $('proxyFields').classList.remove('hidden');
    } catch (e) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
    }
  }
  function onProxyTypeChange() {
    const type = $('proxyType').value;
    $('proxyFields').classList.toggle('hidden', type === 'none' || type === 'environment');
  }
  async function saveProxyConfig() {
    const type = $('proxyType').value;
    let url = type === 'none' ? 'direct' : '';
    if (type !== 'none' && type !== 'environment') {
      const host = $('proxyHost').value.trim();
      const port = $('proxyPort').value.trim();
      if (!host || !port) { toast(t('settings.proxyHostRequired'), 'warning'); return; }
      const u = $('proxyUsername').value.trim();
      const p = $('proxyPassword').value.trim();
      const auth = u ? (p ? encodeURIComponent(u) + ':' + encodeURIComponent(p) + '@' : encodeURIComponent(u) + '@') : '';
      url = type + '://' + auth + host + ':' + port;
    }
    const res = await api('/proxy', { method: 'POST', body: JSON.stringify({ proxyURL: url }) });
    const d = await res.json();
    if (d.success) toast(t('settings.proxySaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function saveRequireApiKey() {
    try {
      const requireApiKey = $('requireApiKey').checked;
      if (requireApiKey) {
        const hasEnabledKey = Array.isArray(apiKeysCache) && apiKeysCache.some(k => k && k.enabled);
        if (!hasEnabledKey) {
          if (!confirm(t('apiKeys.requireWithoutEnabledKeyWarning'))) {
            $('requireApiKey').checked = false;
            return;
          }
        }
      }
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ requireApiKey }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('detail.saved'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function saveOverUsageConfig() {
    const allowOverUsage = $('allowOverUsage').checked;
    await api('/settings', { method: 'POST', body: JSON.stringify({ allowOverUsage }) });
    toast(t('settings.overUsageSaved'), 'success');
  }
  function parseJsonObjectField(id, fallback) {
    const raw = $(id).value.trim();
    if (!raw) return fallback || {};
    const parsed = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      throw new Error(t('settings.jsonObjectRequired'));
    }
    return parsed;
  }
  async function loadUpstreamProtectionConfig() {
    const res = await api('/upstream-protection');
    const d = await res.json();
    $('upstreamProtectionEnabled').checked = d.enabled !== false;
    $('upstreamMaxPerAccountConcurrency').value = d.maxPerAccountConcurrency || 10;
    $('upstreamMaxPerAccountModelConcurrency').value = d.maxPerAccountModelConcurrency || 5;
    $('upstreamRateLimitCooldownMs').value = d.rateLimitCooldownMs || 2000;
    $('upstreamMaxRateLimitCooldownMs').value = d.maxRateLimitCooldownMs || 60000;
    $('upstreamRouteAffinityTTLSeconds').value = d.routeAffinityTtlSeconds || 3600;
    $('upstreamRouteAffinityMaxEntries').value = d.routeAffinityMaxEntries || 20000;
    $('upstreamPerModelConcurrency').value = JSON.stringify(d.perModelConcurrency || {}, null, 2);
    $('upstreamPerProfileModelConcurrency').value = JSON.stringify(d.perProfileModelConcurrency || {}, null, 2);
  }
  async function saveUpstreamProtectionConfig() {
    let perModel, perProfile;
    try {
      perModel = parseJsonObjectField('upstreamPerModelConcurrency', {});
      perProfile = parseJsonObjectField('upstreamPerProfileModelConcurrency', {});
    } catch (e) {
      toast((e && e.message) || t('settings.jsonObjectRequired'), 'warning');
      return;
    }
    const payload = {
      enabled: $('upstreamProtectionEnabled').checked,
      maxPerAccountConcurrency: Math.max(1, Math.round(Number($('upstreamMaxPerAccountConcurrency').value) || 10)),
      maxPerAccountModelConcurrency: Math.max(1, Math.round(Number($('upstreamMaxPerAccountModelConcurrency').value) || 5)),
      rateLimitCooldownMs: Math.max(100, Math.round(Number($('upstreamRateLimitCooldownMs').value) || 2000)),
      maxRateLimitCooldownMs: Math.max(100, Math.round(Number($('upstreamMaxRateLimitCooldownMs').value) || 60000)),
      routeAffinityTtlSeconds: Math.max(60, Math.round(Number($('upstreamRouteAffinityTTLSeconds').value) || 3600)),
      routeAffinityMaxEntries: Math.max(100, Math.round(Number($('upstreamRouteAffinityMaxEntries').value) || 20000)),
      perModelConcurrency: perModel,
      perProfileModelConcurrency: perProfile
    };
    const res = await api('/upstream-protection', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.upstreamProtectionSaved'), 'success');
    loadUpstreamProtectionConfig();
  }
  async function loadPromptCacheConfig() {
    const res = await api('/prompt-cache');
    const d = await res.json();
    $('promptCacheEnabled').checked = d.enabled !== false;
    $('cacheNamespaceMode').value = d.namespaceMode === 'account_api_key' ? 'account_api_key' : 'account';
    const fallback = d.cacheReadEfficiency == null ? 0.87 : d.cacheReadEfficiency;
    $('cacheReadEfficiencyMin').value = Math.round(((d.cacheReadEfficiencyMin == null ? fallback : d.cacheReadEfficiencyMin) * 100));
    $('cacheReadEfficiencyMax').value = Math.round(((d.cacheReadEfficiencyMax == null ? fallback : d.cacheReadEfficiencyMax) * 100));
    $('kvCacheTtlSecs').value = d.kvCacheTtlSecs || 3600;
    $('cacheMaxEntriesPerAccount').value = d.maxEntriesPerAccount || 2048;
    $('cacheMaxEntriesTotal').value = d.maxEntriesTotal || 50000;
    renderPromptCacheStats(d.stats || {});
  }
  function renderPromptCacheStats(stats) {
    $('cacheStatEntries').textContent = formatNumber(stats.entries || 0) + ' / ' + formatNumber(stats.accounts || 0);
    $('cacheStatHitRate').textContent = ((Number(stats.hitRate) || 0) * 100).toFixed(1) + '%';
    $('cacheStatReadTokens').textContent = formatNumber(stats.cacheReadTokens || 0);
    $('cacheStatCreationTokens').textContent = formatNumber(stats.cacheCreationTokens || 0);
  }
  async function savePromptCacheConfig() {
    const efficiencyMinPct = Number($('cacheReadEfficiencyMin').value);
    const efficiencyMaxPct = Number($('cacheReadEfficiencyMax').value);
    const ttl = Number($('kvCacheTtlSecs').value);
    const maxEntriesPerAccount = Math.round(Number($('cacheMaxEntriesPerAccount').value) || 0);
    const maxEntriesTotal = Math.round(Number($('cacheMaxEntriesTotal').value) || 0);
    if (!Number.isFinite(efficiencyMinPct) || !Number.isFinite(efficiencyMaxPct) ||
      efficiencyMinPct < 0 || efficiencyMinPct > 100 || efficiencyMaxPct < 0 || efficiencyMaxPct > 100 ||
      efficiencyMinPct > efficiencyMaxPct) {
      toast(t('settings.cacheEfficiencyInvalid'), 'warning');
      return;
    }
    if (!Number.isFinite(ttl) || ttl < 60) {
      toast(t('settings.cacheTtlInvalid'), 'warning');
      return;
    }
    if (maxEntriesPerAccount < 1 || maxEntriesPerAccount > 100000 || maxEntriesTotal < maxEntriesPerAccount || maxEntriesTotal > 1000000) {
      toast(t('settings.cacheEntryLimitsInvalid'), 'warning');
      return;
    }
    const res = await api('/prompt-cache', {
      method: 'POST',
      body: JSON.stringify({
        enabled: $('promptCacheEnabled').checked,
        namespaceMode: $('cacheNamespaceMode').value,
        cacheReadEfficiencyMin: efficiencyMinPct / 100,
        cacheReadEfficiencyMax: efficiencyMaxPct / 100,
        kvCacheTtlSecs: Math.round(ttl),
        maxEntriesPerAccount,
        maxEntriesTotal
      })
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    if (d.config) {
      $('promptCacheEnabled').checked = d.config.enabled !== false;
      $('cacheNamespaceMode').value = d.config.namespaceMode === 'account_api_key' ? 'account_api_key' : 'account';
      const fallback = d.config.cacheReadEfficiency == null ? 0.87 : d.config.cacheReadEfficiency;
      $('cacheReadEfficiencyMin').value = Math.round(((d.config.cacheReadEfficiencyMin == null ? fallback : d.config.cacheReadEfficiencyMin) * 100));
      $('cacheReadEfficiencyMax').value = Math.round(((d.config.cacheReadEfficiencyMax == null ? fallback : d.config.cacheReadEfficiencyMax) * 100));
      $('kvCacheTtlSecs').value = d.config.kvCacheTtlSecs || 3600;
      $('cacheMaxEntriesPerAccount').value = d.config.maxEntriesPerAccount || 2048;
      $('cacheMaxEntriesTotal').value = d.config.maxEntriesTotal || 50000;
    }
    toast(t('settings.promptCacheSaved'), 'success');
    loadPromptCacheConfig();
  }
  async function clearPromptCache() {
    const ok = await confirmAction(t('settings.clearPromptCacheConfirm'), {
      confirmText: t('settings.clearPromptCache'),
      variant: 'danger'
    });
    if (!ok) return;
    const res = await api('/prompt-cache', { method: 'DELETE' });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.failed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    renderPromptCacheStats({});
    toast(t('settings.promptCacheCleared'), 'success');
  }
  async function loadDiagnosticsConfig() {
    const res = await api('/diagnostics');
    const d = await res.json();
    $('diagnosticsEnabled').checked = d.enabled === true;
    $('diagnosticsIncludeRequest').checked = d.includeRequestSummary === true;
    $('diagnosticsMaxEntries').value = d.maxEntries || 200;
  }
  async function saveDiagnosticsConfig() {
    const maxEntries = Math.round(Number($('diagnosticsMaxEntries').value) || 0);
    if (maxEntries < 1 || maxEntries > 2000) {
      toast(t('settings.diagnosticsInvalid'), 'warning');
      return;
    }
    const res = await api('/diagnostics', {
      method: 'POST',
      body: JSON.stringify({
        enabled: $('diagnosticsEnabled').checked,
        includeRequestSummary: $('diagnosticsIncludeRequest').checked,
        maxEntries
      })
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.diagnosticsSaved'), 'success');
    loadDiagnosticsConfig();
  }
  async function loadRequestLogConfig() {
    const res = await api('/request-log');
    const d = await res.json();
    $('requestLogMaxEntries').value = d.maxEntries || 1000;
  }
  async function saveRequestLogConfig() {
    const maxEntries = Math.round(Number($('requestLogMaxEntries').value) || 0);
    if (maxEntries < 100 || maxEntries > 20000) {
      toast(t('settings.requestLogInvalid'), 'warning');
      return;
    }
    const res = await api('/request-log', {
      method: 'POST',
      body: JSON.stringify({ maxEntries })
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.requestLogSaved'), 'success');
    loadRequestLogConfig();
  }
  async function loadCountTokensProviderConfig() {
    const res = await api('/count-tokens-provider');
    const d = await res.json();
    $('countTokensProviderEnabled').checked = d.enabled === true;
    $('countTokensApiUrl').value = d.apiUrl || '';
	$('countTokensApiKey').value = '';
	$('countTokensApiKey').dataset.configured = d.apiKeyConfigured ? 'true' : 'false';
	$('countTokensApiKey').placeholder = d.apiKeyConfigured ? t('security.secretConfigured') : '';
    $('countTokensAuthType').value = d.authType || 'x-api-key';
  }
  async function saveCountTokensProviderConfig() {
    const payload = {
      enabled: $('countTokensProviderEnabled').checked,
      apiUrl: $('countTokensApiUrl').value.trim(),
      apiKey: $('countTokensApiKey').value.trim(),
      authType: $('countTokensAuthType').value
    };
    if (payload.enabled && !payload.apiUrl) {
      toast(t('settings.countTokensApiUrlRequired'), 'warning');
      return;
    }
    const res = await api('/count-tokens-provider', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.countTokensProviderSaved'), 'success');
    loadCountTokensProviderConfig();
  }
  async function loadWebSearchConfig() {
    const res = await api('/web-search');
    const d = await res.json();
    $('webSearchEnabled').checked = d.enabled === true;
  }
  async function saveWebSearchConfig() {
    const res = await api('/web-search', {
      method: 'POST',
      body: JSON.stringify({ enabled: $('webSearchEnabled').checked })
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.success === false) {
      toast(t('common.saveFailed') + ': ' + (d.error || t('common.unknownError')), 'error');
      return;
    }
    toast(t('settings.webSearchSaved'), 'success');
    loadWebSearchConfig();
  }
  async function changePassword() {
    const np = $('newPassword').value;
    if (!np) return toast(t('settings.passwordRequired'), 'warning');
    try {
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ password: np }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('settings.passwordChanged'), 'success');
      $('newPassword').value = '';
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function resetStats() {
    const ok = await confirmAction(t('settings.confirmReset'), {
      title: t('settings.statistics'),
      confirmText: t('settings.resetStats'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/stats/reset', { method: 'POST' });
      if (!res.ok) throw new Error(t('common.failed'));
      loadStats();
      loadAccounts();
      toastPrimary(t('settings.statsReset'));
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    }
  }
  // Multi API Key management
  let apiKeysCache = [];
  let apiKeyEditingId = '';
  let apiKeyModalSubmitting = false;

  async function loadApiKeys() {
    const list = $('apiKeysList');
    if (!list) return;
    try {
      const res = await api('/api-keys');
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      apiKeysCache = Array.isArray(d.apiKeys) ? d.apiKeys : [];
      renderApiKeys();
    } catch (e) {
      apiKeysCache = [];
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('apiKeys.loadFailed')) + '</div>';
    }
  }

  function formatNumber(n) {
    if (n == null || isNaN(n)) return '0';
    if (Math.abs(n) >= 1 && Math.floor(n) === n) return Number(n).toLocaleString('en-US');
    return Number(n).toLocaleString('en-US', { maximumFractionDigits: 4 });
  }

  function usageBar(used, limit) {
    if (!limit || limit <= 0) return '';
    const ratio = Math.max(0, Math.min(1, used / limit));
    const pct = (ratio * 100).toFixed(1);
    let color = '#3b82f6';
    if (ratio >= 0.95) color = '#ef4444';
    else if (ratio >= 0.8) color = '#f59e0b';
    return '<div style="height:6px;background:rgba(127,127,127,0.2);border-radius:3px;overflow:hidden;margin-top:4px;">' +
      '<div style="height:100%;width:' + pct + '%;background:' + color + ';transition:width 0.3s;"></div>' +
      '</div>';
  }

  function usageLine(label, used, limit, options) {
    options = options || {};
    const fmt = options.fmt || formatNumber;
    if (!limit || limit <= 0) {
      return '<div class="text-xs muted-text">' + escapeHtml(label) + ': ' + escapeHtml(fmt(used)) + ' / ' + escapeHtml(t('apiKeys.unlimited')) + '</div>';
    }
    return '<div class="text-xs muted-text">' + escapeHtml(label) + ': ' + escapeHtml(fmt(used)) + ' / ' + escapeHtml(fmt(limit)) + '</div>' + usageBar(used, limit);
  }

  function renderApiKeys() {
    const list = $('apiKeysList');
    if (!list) return;
    if (!apiKeysCache.length) {
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('apiKeys.empty')) + '</div>';
      return;
    }
    const html = apiKeysCache.map(item => {
      const id = escapeAttr(item.id || '');
      const name = item.name ? escapeHtml(item.name) : '<span class="muted-text">' + escapeHtml(t('apiKeys.unnamed')) + '</span>';
      const masked = escapeHtml(item.keyMasked || '');
      const migrated = item.migrated
        ? '<span class="text-xs" style="background:rgba(59,130,246,0.15);color:#3b82f6;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('apiKeys.migrated')) + '</span>'
        : '';
      const disabled = !item.enabled
        ? '<span class="text-xs" style="background:rgba(239,68,68,0.15);color:#ef4444;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('apiKeys.disabled')) + '</span>'
        : '';
      const tokensLine = usageLine(t('apiKeys.tokens'), item.tokensUsed || 0, item.tokenLimit || 0);
      const creditsLine = usageLine(t('apiKeys.credits'), item.creditsUsed || 0, item.creditLimit || 0);
      const requestsLine = '<div class="text-xs muted-text">' + escapeHtml(t('apiKeys.requests')) + ': ' + escapeHtml(formatNumber(item.requestsCount || 0)) + '</div>';
      return '<div class="card" data-apikey-id="' + id + '" style="margin-top:0.5rem;padding:0.75rem;">' +
        '<div class="flex items-center gap-2" style="flex-wrap:wrap;justify-content:space-between;">' +
          '<div class="flex items-center gap-2" style="flex-wrap:wrap;">' +
            '<span class="font-semibold">' + name + '</span>' +
            migrated +
            disabled +
            '<span class="text-xs muted-text font-mono">' + masked + '</span>' +
          '</div>' +
          '<div class="flex items-center gap-2">' +
            '<label class="switch" title="' + escapeAttr(item.enabled ? t('accounts.disable') : t('accounts.enable')) + '">' +
              '<input type="checkbox" data-apikey-action="toggle" data-id="' + id + '"' + (item.enabled ? ' checked' : '') + ' />' +
              '<span class="slider"></span>' +
            '</label>' +
            '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="edit" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionEdit')) + '</button>' +
            '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="reset" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionReset')) + '</button>' +
            '<button class="btn btn-danger btn-sm" type="button" data-apikey-action="delete" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionDelete')) + '</button>' +
          '</div>' +
        '</div>' +
        '<div style="margin-top:0.5rem;display:grid;gap:0.35rem;">' +
          tokensLine +
          creditsLine +
          requestsLine +
        '</div>' +
      '</div>';
    }).join('');
    list.innerHTML = html;
  }

  function openApiKeyModal(entry) {
    apiKeyEditingId = entry ? (entry.id || '') : '';
    const titleEl = $('apiKeyModalTitle');
    titleEl.textContent = t(apiKeyEditingId ? 'apiKeys.modalTitleEdit' : 'apiKeys.modalTitleCreate');
    $('apiKeyForm_name').value = entry ? (entry.name || '') : '';
    const keyEl = $('apiKeyForm_key');
    if (apiKeyEditingId) {
      keyEl.value = entry.keyMasked || '';
      keyEl.readOnly = true;
    } else {
      keyEl.value = '';
      keyEl.readOnly = false;
    }
    $('apiKeyForm_enabled').checked = entry ? !!entry.enabled : true;
    $('apiKeyForm_tokenLimit').value = entry ? String(entry.tokenLimit || 0) : '0';
    $('apiKeyForm_creditLimit').value = entry ? String(entry.creditLimit || 0) : '0';
	$('apiKeyForm_requestsPerMinute').value = entry ? String(entry.requestsPerMinute || 0) : '0';
	$('apiKeyForm_tokensPerMinute').value = entry ? String(entry.tokensPerMinute || 0) : '0';
	$('apiKeyForm_maxConcurrency').value = entry ? String(entry.maxConcurrency || 0) : '0';
	$('apiKeyForm_queueCapacity').value = entry ? String(entry.queueCapacity || 0) : '0';
	$('apiKeyForm_queueTimeoutMs').value = entry ? String(entry.queueTimeoutMs || 0) : '0';
    apiKeyModalSubmitting = false;
    $('apiKeyModalSaveBtn').disabled = false;
    openDialog('apiKeyModal');
  }

  function closeApiKeyModal() {
    closeDialog('apiKeyModal');
    apiKeyEditingId = '';
    apiKeyModalSubmitting = false;
    $('apiKeyModalSaveBtn').disabled = false;
  }

  async function submitApiKeyModal() {
    if (apiKeyModalSubmitting) return;
    apiKeyModalSubmitting = true;
    const saveBtn = $('apiKeyModalSaveBtn');
    saveBtn.disabled = true;
    try {
      const name = $('apiKeyForm_name').value.trim();
      const enabled = $('apiKeyForm_enabled').checked;
      const tokenLimit = parseInt($('apiKeyForm_tokenLimit').value, 10);
      const creditLimit = parseFloat($('apiKeyForm_creditLimit').value);
	  const requestsPerMinute = parseInt($('apiKeyForm_requestsPerMinute').value, 10);
	  const tokensPerMinute = parseInt($('apiKeyForm_tokensPerMinute').value, 10);
	  const maxConcurrency = parseInt($('apiKeyForm_maxConcurrency').value, 10);
	  const queueCapacity = parseInt($('apiKeyForm_queueCapacity').value, 10);
	  const queueTimeoutMs = parseInt($('apiKeyForm_queueTimeoutMs').value, 10);
      const payload = {
        name: name,
        enabled: enabled,
        tokenLimit: isNaN(tokenLimit) || tokenLimit < 0 ? 0 : tokenLimit,
		creditLimit: isNaN(creditLimit) || creditLimit < 0 ? 0 : creditLimit,
		requestsPerMinute: isNaN(requestsPerMinute) || requestsPerMinute < 0 ? 0 : requestsPerMinute,
		tokensPerMinute: isNaN(tokensPerMinute) || tokensPerMinute < 0 ? 0 : tokensPerMinute,
		maxConcurrency: isNaN(maxConcurrency) || maxConcurrency < 0 ? 0 : maxConcurrency,
		queueCapacity: isNaN(queueCapacity) || queueCapacity < 0 ? 0 : queueCapacity,
		queueTimeoutMs: isNaN(queueTimeoutMs) || queueTimeoutMs < 0 ? 0 : queueTimeoutMs
      };
      let res, d;
      if (apiKeyEditingId) {
        res = await api('/api-keys/' + encodeURIComponent(apiKeyEditingId), { method: 'PUT', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        toast(t('apiKeys.updated'), 'success');
        closeApiKeyModal();
        await loadApiKeys();
      } else {
        const keyVal = $('apiKeyForm_key').value.trim();
        if (keyVal) payload.key = keyVal;
        res = await api('/api-keys', { method: 'POST', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        toast(t('apiKeys.created'), 'success');
        closeApiKeyModal();
        await loadApiKeys();
        if (d.key) showNewApiKey(d.key);
      }
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      apiKeyModalSubmitting = false;
      saveBtn.disabled = false;
    }
  }

  async function toggleApiKeyEntry(id, enabled) {
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'PUT', body: JSON.stringify({ enabled }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      const item = apiKeysCache.find(x => x.id === id);
      if (item) item.enabled = enabled;
      renderApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      await loadApiKeys();
    }
  }

  async function deleteApiKeyEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmDelete', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionDelete'),
      confirmText: t('apiKeys.actionDelete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.deleteSuccess'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function resetApiKeyUsageEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmReset', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionReset'),
      confirmText: t('apiKeys.actionReset')
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id) + '/reset-usage', { method: 'POST' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.usageReset'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  function showNewApiKey(plaintext) {
    $('apiKeyShowValue').value = plaintext || '';
    openDialog('apiKeyShowModal');
    setTimeout(() => {
      const el = $('apiKeyShowValue');
      if (el) { try { el.select(); } catch (_) { } }
    }, 0);
  }

  function closeShowApiKeyModal() {
    closeDialog('apiKeyShowModal');
    $('apiKeyShowValue').value = '';
  }

  async function copyNewApiKey() {
    const val = $('apiKeyShowValue').value;
    if (!val) return;
    try {
      if (await copyText(val)) toast(t('apiKeys.copySuccess'), 'success');
    } catch (e) {
      toast(t('common.failed'), 'error');
    }
  }

  function bindApiKeyEvents() {
    const list = $('apiKeysList');
    if (list) {
      list.addEventListener('click', e => {
        const btn = e.target.closest('[data-apikey-action]');
        if (!btn) return;
        const action = btn.dataset.apikeyAction;
        const id = btn.dataset.id;
        if (!id) return;
        const entry = apiKeysCache.find(x => x.id === id);
        const name = entry ? entry.name : '';
        if (action === 'edit') openApiKeyModal(entry);
        else if (action === 'delete') deleteApiKeyEntry(id, name);
        else if (action === 'reset') resetApiKeyUsageEntry(id, name);
      });
      list.addEventListener('change', e => {
        const cb = e.target.closest('input[data-apikey-action="toggle"]');
        if (!cb) return;
        const id = cb.dataset.id;
        if (!id) return;
        toggleApiKeyEntry(id, cb.checked);
      });
    }
    const addBtn = $('addApiKeyBtn');
    if (addBtn) addBtn.addEventListener('click', () => openApiKeyModal(null));
    const saveBtn = $('apiKeyModalSaveBtn');
    if (saveBtn) saveBtn.addEventListener('click', submitApiKeyModal);
    const cancelBtn = $('apiKeyModalCancelBtn');
    if (cancelBtn) cancelBtn.addEventListener('click', closeApiKeyModal);
    const closeBtn = $('apiKeyModalClose');
    if (closeBtn) closeBtn.addEventListener('click', closeApiKeyModal);
    const showCloseBtn = $('apiKeyShowCloseBtn');
    if (showCloseBtn) showCloseBtn.addEventListener('click', closeShowApiKeyModal);
    const showCloseX = $('apiKeyShowClose');
    if (showCloseX) showCloseX.addEventListener('click', closeShowApiKeyModal);
    const copyBtn = $('apiKeyShowCopyBtn');
    if (copyBtn) copyBtn.addEventListener('click', copyNewApiKey);
    bindDialogBackdropClose('apiKeyModal', closeApiKeyModal);
    bindDialogBackdropClose('apiKeyShowModal', closeShowApiKeyModal);
  }

  // Prompt filter rules
  async function loadPromptFilter() {
    const res = await api('/prompt-filter');
    const d = await res.json();
    $('filterClaudeCode').checked = !!d.filterClaudeCode;
    $('filterEnvNoise').checked = !!d.filterEnvNoise;
    $('filterStripBoundaries').checked = !!d.filterStripBoundaries;
    promptRules = d.rules || [];
    renderPromptRules();
  }
  async function savePromptFilter() {
    const res = await api('/prompt-filter', {
      method: 'POST', body: JSON.stringify({
        filterClaudeCode: $('filterClaudeCode').checked,
        filterEnvNoise: $('filterEnvNoise').checked,
        filterStripBoundaries: $('filterStripBoundaries').checked,
        rules: promptRules
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.promptFilterSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  function renderPromptRules() {
    const c = $('promptFilterRules');
    if (!c) return;
    if (!promptRules.length) {
      c.innerHTML = '<small class="text-xs muted-text">' + escapeHtml(t('promptFilter.noRules')) + '</small>';
      return;
    }
    c.innerHTML = promptRules.map((r, i) => {
      const isContains = r.type === 'lines-containing';
      const typeLabel = isContains ? t('promptFilter.typeContains') : t('promptFilter.typeRegex');
      const matchPh = isContains ? t('promptFilter.matchPlaceholderContains') : t('promptFilter.matchPlaceholderRegex');
      const replaceRow = !isContains
        ? '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.replace')) + '</label>' +
        '<input value="' + escapeAttr(r.replace || '') + '" data-rule-idx="' + i + '" data-rule-field="replace" placeholder="' + escapeAttr(t('promptFilter.emptyRemove')) + '" />' +
        '</div>'
        : '';
      return '<div class="rule-card' + (r.enabled ? '' : ' disabled') + '">' +
        '<div class="rule-header">' +
        '<label class="switch"><input type="checkbox" ' + (r.enabled ? 'checked' : '') + ' data-rule-toggle="' + i + '" /><span class="slider"></span></label>' +
        '<div class="rule-meta">' +
        '<input class="rule-name-input" value="' + escapeAttr(r.name || '') + '" data-rule-idx="' + i + '" data-rule-field="name" placeholder="' + escapeAttr(t('promptFilter.unnamed')) + '" />' +
        '<span class="rule-type">' + escapeHtml(typeLabel) + '</span>' +
        '</div>' +
        '<button class="rule-remove" data-rule-remove="' + i + '" type="button" aria-label="' + escapeAttr(t('common.remove')) + '">&times;</button>' +
        '</div>' +
        '<div class="rule-body">' +
        '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.match')) + '</label>' +
        '<input value="' + escapeAttr(r.match || '') + '" data-rule-idx="' + i + '" data-rule-field="match" placeholder="' + escapeAttr(matchPh) + '" />' +
        '</div>' +
        replaceRow +
        '</div>' +
        '</div>';
    }).join('');
  }
  function addPromptRule(type) {
    promptRules.push({ id: 'rule-' + Date.now(), name: '', type, match: '', replace: '', enabled: true });
    renderPromptRules();
  }

  // Add-account modal templates
  var METHOD_ICONS = {
    builderid: 'fa-solid fa-id-card',
    iam: 'fa-solid fa-key',
    enterprisesso: 'fa-brands fa-microsoft',
    sso: 'fa-solid fa-shield-halved',
    local: 'fa-solid fa-folder-open',
    credentials: 'fa-solid fa-code',
    cookie: 'fa-solid fa-cookie-bite'
  };
  function methodCard(type, title, desc) {
    var icon = METHOD_ICONS[type] || 'fa-solid fa-circle-plus';
    return '<button type="button" class="method-card" data-method="' + escapeAttr(type) + '">' +
      '<span class="method-icon"><i class="' + icon + '" aria-hidden="true"></i></span>' +
      '<span class="method-body">' +
      '<span class="method-title">' + escapeHtml(title) + '</span>' +
      '<span class="method-desc">' + escapeHtml(desc) + '</span>' +
      '</span>' +
      '<span class="method-arrow" aria-hidden="true"><i class="fa-solid fa-chevron-right"></i></span>' +
      '</button>';
  }
  function showModal(type) {
    const modal = $('addModal');
    const title = $('modalTitle');
    const body = $('modalBody');
    if (type === 'add') modalAdd(title, body);
    else if (type === 'builderid') modalBuilderId(title, body);
    else if (type === 'iam') modalIam(title, body);
    else if (type === 'enterprisesso') modalEnterpriseSso(title, body);
    else if (type === 'sso') modalSso(title, body);
    else if (type === 'local') modalLocal(title, body);
    else if (type === 'credentials') modalCredentials(title, body);
    else if (type === 'cookie') modalCookie(title, body);
    if (!modal.classList.contains('active')) openDialog('addModal');
    enhanceCustomSelects(body);
  }
  function closeModal() {
    cancelKiroSsoLogin(false);
    closeDialog('addModal');
    iamSession = '';
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    builderIdSession = '';
  }
  function modalAdd(title, body) {
    title.textContent = t('modal.addAccount');
    body.innerHTML =
      '<div class="method-list">' +
      methodCard('builderid', t('modal.builderIdTitle'), t('modal.builderIdDesc')) +
      methodCard('iam', t('modal.iamTitle'), t('modal.iamDesc')) +
      methodCard('enterprisesso', t('modal.enterpriseSsoTitle'), t('modal.enterpriseSsoDesc')) +
      methodCard('sso', t('modal.ssoTitle'), t('modal.ssoDesc')) +
      methodCard('local', t('modal.localTitle'), t('modal.localDesc')) +
      methodCard('credentials', t('modal.credentialsTitle'), t('modal.credentialsDesc')) +
      methodCard('cookie', t('modal.cookieTitle'), t('modal.cookieDesc')) +
      '</div>' +
      '<div class="modal-footer"><button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>';
  }
  function modalBuilderId(title, body) {
    title.textContent = t('modal.builderIdTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.builderIdDesc')) + '</p>' +
      '<div id="builderIdStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="builderIdRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startBuilderIdBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="builderIdStep2" class="hidden">' +
      '<div class="message message-info message-center"><p class="builder-code" id="builderIdUserCode"></p><p class="text-xs mt-2">' + escapeHtml(t('builderid.verifyCode')) + '</p></div>' +
      '<div class="form-group mt-4"><label>' + escapeHtml(t('builderid.verifyUrl')) + '</label>' +
      '<div class="endpoint"><span id="builderIdVerifyUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p id="builderIdStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="builderIdCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('startBuilderIdBtn').addEventListener('click', startBuilderIdLogin);
  }
  function modalIam(title, body) {
    title.textContent = t('modal.iamTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.iamDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.startUrl')) + '</label><input type="text" id="iamStartUrl" placeholder="https://xxx.awsapps.com/start" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="iamRegion" value="us-east-1" /></div>' +
      '<div id="iamStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.loginUrl')) + '</label>' +
      '<div class="endpoint"><span id="iamAuthUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p class="text-sm mt-3 success-text">' + escapeHtml(t('iam.completeLogin')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.callbackUrl')) + '</label><input type="text" id="iamCallback" placeholder="http://127.0.0.1:xxx/?code=..." /></div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="iamBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>';
    $('iamBtn').addEventListener('click', startIamSso);
  }
  function modalEnterpriseSso(title, body) {
    title.textContent = t('modal.enterpriseSsoTitle');
    body.innerHTML =
      '<div id="kiroSsoStep1">' +
      '<p class="help-block">' + escapeHtml(t('modal.enterpriseSsoDesc')) + '</p>' +
      '<div class="message message-info"><p class="text-xs">' + escapeHtml(t('kirosso.hostNote')) + '</p></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startKiroSsoBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div></div>' +
      '<div id="kiroSsoStep2" class="hidden">' +
      '<p class="help-block">' + escapeHtml(t('kirosso.openInstruction')) + '</p>' +
      '<div class="endpoint"><span id="kiroSsoSignInUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="kiroSsoOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="kiroSsoCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '<p id="kiroSsoStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="kiroSsoCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>' +
      '<div id="kiroSsoStep3" class="hidden">' +
      '<p class="help-block">' + escapeHtml(t('kirosso.chooseProfile')) + '</p>' +
      '<div class="method-list" id="kiroSsoProfileList"></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="kiroSsoProfileCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-primary" id="kiroSsoProfileConfirmBtn" type="button">' + escapeHtml(t('kirosso.useProfile')) + '</button>' +
      '</div></div>';
    $('startKiroSsoBtn').addEventListener('click', startKiroSsoLogin);
  }
  function modalSso(title, body) {
    title.textContent = t('modal.ssoTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<b>' + escapeHtml(t('sso.howToGet')) + '</b>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('sso.step1')) + ' <code class="code-inline">view.awsapps.com/start</code></li>' +
      '<li>' + escapeHtml(t('sso.step2')) + '</li>' +
      '<li>' + escapeHtml(t('sso.step3')) + ' <code class="code-inline">x-amz-sso_authn</code></li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('sso.tokenLabel')) + ' <small>' + escapeHtml(t('sso.tokenHint')) + '</small></label>' +
      '<textarea id="ssoToken" placeholder="' + escapeAttr(t('sso.tokenPlaceholder')) + '"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="ssoRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importSsoBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importSsoBtn').addEventListener('click', importSsoToken);
  }

  function modalLocal(title, body) {
    title.textContent = t('modal.localTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.localDesc')) + '</p>' +
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('local.fileLocation')) + '</b></p>' +
      '<p>' + escapeHtml(t('local.windows')) + ': <code class="code-inline">%USERPROFILE%\\.aws\\sso\\cache\\</code></p>' +
      '<p>' + escapeHtml(t('local.macosLinux')) + ': <code class="code-inline">~/.aws/sso/cache/</code></p>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('local.loginChannel')) + '</label>' +
      '<select id="localProvider">' +
      '<option value="BuilderId">' + escapeHtml(t('local.providerBuilderId')) + '</option>' +
      '<option value="Enterprise">' + escapeHtml(t('local.providerEnterprise')) + '</option>' +
      '<option value="Google">' + escapeHtml(t('local.providerGoogle')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('local.providerGithub')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group">' +
      '<label>' + escapeHtml(t('local.tokenFile')) + ' <small>' + escapeHtml(t('local.tokenRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localTokenJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localTokenFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div id="localClientGroup" class="form-group">' +
      '<label>' + escapeHtml(t('local.clientFile')) + ' <small>' + escapeHtml(t('local.clientRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localClientJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localClientFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importLocalBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('localProvider').addEventListener('change', updateLocalFields);
    $('localTokenFile').addEventListener('change', e => loadLocalFile(e.target, 'localTokenJson'));
    $('localClientFile').addEventListener('change', e => loadLocalFile(e.target, 'localClientJson'));
    $('importLocalBtn').addEventListener('click', importLocalKiro);
  }
  function modalCredentials(title, body) {
    title.textContent = t('modal.credentialsTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.credentialsDesc')) + '</p>' +
      '<p class="help-block">' + escapeHtml(t('credentials.batchHint')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('credentials.label')) + '</label>' +
      '<textarea id="credJson" class="font-mono" placeholder=\'[{"refreshToken":"xxx","provider":"BuilderID"}]&#10;[{"authMethod":"api_key","kiroApiKey":"xxx"}]&#10;or&#10;email----password----refreshToken----clientId----clientSecret\'></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCredBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCredBtn').addEventListener('click', importCredentials);
  }
  function modalCookie(title, body) {
    title.textContent = t('modal.cookieTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('cookie.howToGet')) + '</b></p>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('cookie.step1')) + ' <a href="' + escapeAttr(t('cookie.link')) + '" target="_blank">' + escapeHtml(t('cookie.link')) + '</a></li>' +
      '<li>' + escapeHtml(t('cookie.step2')) + '</li>' +
      '<li>' + escapeHtml(t('cookie.step3')) + '</li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.provider')) + '</label>' +
      '<select id="cookieProvider">' +
      '<option value="Google">' + escapeHtml(t('cookie.google')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('cookie.github')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.refreshToken')) + '</label>' +
      '<textarea id="cookieRefreshToken" class="font-mono" placeholder="' + escapeAttr(t('cookie.refreshTokenPlaceholder')) + '"></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCookieBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCookieBtn').addEventListener('click', importFromCookie);
  }
  function updateLocalFields() {
    const p = $('localProvider').value;
    $('localClientGroup').classList.toggle('hidden', p === 'Google' || p === 'Github');
  }
  function loadLocalFile(input, targetId) {
    const file = input.files[0];
    if (!file) return;
    const r = new FileReader();
    r.onload = e => { $(targetId).value = e.target.result; };
    r.readAsText(file);
  }

  // Import handlers
  async function importLocalKiro() {
    const provider = $('localProvider').value;
    const tokenJson = $('localTokenJson').value.trim();
    const clientJson = $('localClientJson').value.trim();
    const isSocial = provider === 'Google' || provider === 'Github';
    if (!tokenJson) return toastWarning(t('local.tokenMissing'));
    let tokenData, clientData;
    try { tokenData = JSON.parse(tokenJson); } catch { return toastWarning(t('local.tokenInvalid')); }
    if (!tokenData.refreshToken) return toastWarning(t('local.refreshTokenMissing'));
    if (!isSocial) {
      if (!clientJson) return toastWarning(t('local.clientMissing'));
      try { clientData = JSON.parse(clientJson); } catch { return toastWarning(t('local.clientInvalid')); }
      if (!clientData.clientId || !clientData.clientSecret) return toastWarning(t('local.clientSecretMissing'));
    }
    const authMethod = clientData ? 'idc' : 'social';
    const payload = {
      refreshToken: tokenData.refreshToken,
      accessToken: tokenData.accessToken || '',
      clientId: clientData?.clientId || '',
      clientSecret: clientData?.clientSecret || '',
      region: tokenData.region || '',
      authMethod, provider
    };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('local.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importCredentials() {
    const raw = $('credJson').value.trim();
    if (!raw) { toastWarning(t('credentials.jsonError')); return; }
    let items;
    let skipped = 0;
    try {
      const json = JSON.parse(raw);
      if (json.accounts && Array.isArray(json.accounts)) {
        items = json.accounts.map(normalizeImportCredentialItem);
      } else {
        items = (Array.isArray(json) ? json : [json]).map(normalizeImportCredentialItem);
      }
    } catch {
      const parsed = parseLineCredentials(raw);
      items = parsed.items;
      skipped = parsed.skipped;
      if (items.length === 0 && skipped === 0) {
        toastWarning(t('credentials.jsonError'));
        return;
      }
      if (items.length === 0) {
        toastWarning(t('credentials.lineParseAllSkipped', skipped));
        return;
      }
    }
    let ok = 0, fail = 0;
    const payloads = [];
    for (const item of items) {
      const hasKiroApiKey = !!(item.kiroApiKey || (String(item.authMethod || '').toLowerCase().replace(/[-\s]/g, '_') === 'api_key' && item.accessToken));
      if (!item.refreshToken && !hasKiroApiKey) { fail++; continue; }
      let authMethod = item.authMethod || '';
      const normalizedAuth = String(authMethod).toLowerCase().replace(/[-\s]/g, '_');
      const externalIdpAliases = ['external_idp', 'azuread', 'azure', 'entra', 'entra_id', 'microsoft', 'm365', 'office365', 'external'];
      if (hasKiroApiKey || normalizedAuth === 'api_key' || normalizedAuth === 'apikey') authMethod = 'api_key';
      else if (externalIdpAliases.includes(normalizedAuth) || item.tokenEndpoint) authMethod = 'external_idp';
      else if (item.clientId && item.clientSecret) authMethod = 'idc';
      else if (!authMethod || authMethod === 'social') authMethod = 'social';
      else authMethod = authMethod.toLowerCase() === 'idc' ? 'idc' : 'social';
      let provider = item.provider || '';
      if (!provider && authMethod === 'social') provider = 'Google';
      if (!provider && authMethod === 'idc') provider = 'BuilderId';
      if (!provider && authMethod === 'api_key') provider = 'API Key';
      if (!provider && authMethod === 'external_idp') provider = 'AzureAD';
      payloads.push({
        id: item.id || '',
        email: item.email || '',
        userId: item.userId || '',
        machineId: item.machineId || '',
        status: item.status || '',
        refreshToken: item.refreshToken,
        accessToken: item.accessToken || '',
        kiroApiKey: item.kiroApiKey || '',
        clientId: item.clientId || '',
        clientSecret: item.clientSecret || '',
        authMethod, provider,
        region: item.region || 'us-east-1',
        profileArn: item.profileArn || '',
        tokenEndpoint: item.tokenEndpoint || '',
        issuerUrl: item.issuerUrl || '',
        scopes: item.scopes || '',
        subscription: item.subscription || null,
        usage: item.usage || null
      });
    }
    if (payloads.length > 0) {
      try {
        const res = await api('/auth/credentials', {
          method: 'POST',
          body: JSON.stringify({ accounts: payloads })
        });
        const d = await res.json().catch(() => ({}));
        const imported = Array.isArray(d.accounts) ? d.accounts : [];
        const errors = Array.isArray(d.errors) ? d.errors : [];
        ok = imported.length;
        fail += errors.length;
        if (!res.ok && imported.length === 0 && errors.length === 0) fail += payloads.length;
      } catch {
        fail += payloads.length;
      }
    }
    closeModal(); loadAccounts(); loadStats();
    let msg = t('sso.importSuccess', ok);
    if (fail > 0) msg += t('sso.importPartial', fail);
    if (skipped > 0) msg += t('credentials.lineParseSkipped', skipped);
    toastPrimary(msg, { duration: 5200 });
  }
  function normalizeImportCredentialItem(item) {
    const c = (item && item.credentials) || {};
    return {
      ...item,
      accessToken: c.accessToken || item.accessToken,
      refreshToken: c.refreshToken || item.refreshToken,
      kiroApiKey: c.kiroApiKey || item.kiroApiKey,
      clientId: c.clientId || item.clientId,
      clientSecret: c.clientSecret || item.clientSecret,
      region: c.region || item.region,
      authMethod: c.authMethod || item.authMethod,
      provider: c.provider || item.provider || item.idp,
      expiresAt: c.expiresAt || item.expiresAt,
      profileArn: c.profileArn || item.profileArn,
      tokenEndpoint: c.tokenEndpoint || item.tokenEndpoint,
      issuerUrl: c.issuerUrl || item.issuerUrl,
      scopes: c.scopes || item.scopes
    };
  }
  function parseLineCredentials(text) {
    const items = [];
    let skipped = 0;
    for (const line of text.split(/\r?\n/)) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      let parts;
      if (trimmed.includes('----')) {
        parts = trimmed.split('----').map(s => s.trim());
      } else if (trimmed.includes('\t')) {
        parts = trimmed.split(/\t+/).map(s => s.trim());
      } else {
        parts = trimmed.split(/\s+/).map(s => s.trim());
      }
      if (parts.length < 5) { skipped++; continue; }
      const refreshToken = parts[2];
      if (!refreshToken) { skipped++; continue; }
      items.push({
        refreshToken,
        clientId: parts[3],
        clientSecret: parts[4],
      });
    }
    return { items, skipped };
  }
  async function importFromCookie() {
    const refreshToken = $('cookieRefreshToken').value.trim();
    if (!refreshToken) return toastWarning(t('cookie.refreshTokenMissing'));
    const provider = $('cookieProvider').value;
    const payload = { refreshToken, accessToken: '', clientId: '', clientSecret: '', authMethod: 'social', provider };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('cookie.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importSsoToken() {
    const res = await api('/auth/sso-token', {
      method: 'POST', body: JSON.stringify({
        bearerToken: $('ssoToken').value,
        region: $('ssoRegion').value
      })
    });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      const count = d.accounts?.length || 0;
      const errs = d.errors?.length || 0;
      let msg = t('sso.importSuccess', count);
      if (errs > 0) msg += t('sso.importPartial', errs);
      toastPrimary(msg, { duration: 5200 });
      if (d.accounts) d.accounts.forEach(a => autoRefreshNewAccount(a.id));
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function startKiroSsoLogin() {
    if (kiroSsoSession) return;
    const popup = window.open('about:blank', '_blank');
    try {
      const res = await api('/auth/kiro-sso/start', { method: 'POST', body: '{}' });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !data.sessionId || !data.signInUrl) throw new Error(data.error || t('common.failed'));
      kiroSsoSession = data.sessionId;
      $('kiroSsoSignInUrl').textContent = data.signInUrl;
      $('kiroSsoStep1').classList.add('hidden');
      $('kiroSsoStep2').classList.remove('hidden');
      $('kiroSsoOpenBtn').addEventListener('click', () => window.open(data.signInUrl, '_blank'));
      $('kiroSsoCopyBtn').addEventListener('click', async () => {
        if (await copyText(data.signInUrl)) toast(t('common.copied'), 'primary');
      });
      $('kiroSsoCancelBtn').addEventListener('click', () => cancelKiroSsoLogin(true));
      if (popup) popup.location.href = data.signInUrl;
      pollKiroSso(data.interval || 2);
    } catch (e) {
      if (popup) popup.close();
      toastError((e && e.message) || t('common.failed'));
    }
  }
  function pollKiroSso(interval) {
    kiroSsoPollTimer = setTimeout(async () => {
      try {
        const res = await api('/auth/kiro-sso/poll', {
          method: 'POST', body: JSON.stringify({ sessionId: kiroSsoSession })
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || t('common.failed'));
        if (data.status === 'choose_profile') {
          if (!Array.isArray(data.profiles) || data.profiles.length === 0) throw new Error(t('common.failed'));
          showKiroSsoProfileChoice(data.profiles);
          return;
        }
        if (data.completed) {
          kiroSsoSession = '';
          kiroSsoPollTimer = null;
          closeModal();
          loadAccounts(); loadStats();
          toastPrimary(t('builderid.success') + ': ' + (data.account?.email || data.account?.id));
          autoRefreshNewAccount(data.account?.id);
          return;
        }
        if (data.success) {
          $('kiroSsoStatus').textContent = t('builderid.waiting');
          pollKiroSso(interval);
          return;
        }
        throw new Error(data.error || t('common.failed'));
      } catch (e) {
        toastError((e && e.message) || t('common.failed'));
        cancelKiroSsoLogin(true);
      }
    }, interval * 1000);
  }
  function showKiroSsoProfileChoice(profiles) {
    if (kiroSsoPollTimer) { clearTimeout(kiroSsoPollTimer); kiroSsoPollTimer = null; }
    $('kiroSsoStep2').classList.add('hidden');
    $('kiroSsoStep3').classList.remove('hidden');
    $('kiroSsoProfileList').innerHTML = kiroProfileListHtml(profiles, '');
    $('kiroSsoProfileCancelBtn').addEventListener('click', () => cancelKiroSsoLogin(true));
    $('kiroSsoProfileConfirmBtn').addEventListener('click', async e => {
      const selected = document.querySelector('input[name="kiroProfilePick"]:checked');
      if (!selected || e.currentTarget.disabled) return;
      e.currentTarget.disabled = true;
      const res = await api('/auth/kiro-sso/select-profile', {
        method: 'POST', body: JSON.stringify({ sessionId: kiroSsoSession, profileArn: selected.value })
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !data.success) {
        e.currentTarget.disabled = false;
        toastError(data.error || t('common.failed'));
        return;
      }
      kiroSsoSession = '';
      closeModal();
      loadAccounts(); loadStats();
      toastPrimary(t('builderid.success') + ': ' + (data.account?.email || data.account?.id));
      autoRefreshNewAccount(data.account?.id);
    });
  }
  function cancelKiroSsoLogin(returnToAdd) {
    if (kiroSsoPollTimer) { clearTimeout(kiroSsoPollTimer); kiroSsoPollTimer = null; }
    const sessionId = kiroSsoSession;
    kiroSsoSession = '';
    if (sessionId) {
      api('/auth/kiro-sso/cancel', {
        method: 'POST', body: JSON.stringify({ sessionId })
      }).catch(() => {});
    }
    if (returnToAdd) showModal('add');
  }
  async function startBuilderIdLogin() {
    const region = $('builderIdRegion').value || 'us-east-1';
    const res = await api('/auth/builderid/start', { method: 'POST', body: JSON.stringify({ region }) });
    const d = await res.json();
    if (d.sessionId) {
      builderIdSession = d.sessionId;
      $('builderIdUserCode').textContent = d.userCode;
      $('builderIdVerifyUrl').textContent = d.verificationUri;
      $('builderIdStep1').classList.add('hidden');
      $('builderIdStep2').classList.remove('hidden');
      $('builderIdOpenBtn').addEventListener('click', () => window.open($('builderIdVerifyUrl').textContent, '_blank'));
      $('builderIdCopyBtn').addEventListener('click', async () => {
        if (await copyText($('builderIdVerifyUrl').textContent)) toast(t('common.copied'), 'primary');
      });
      $('builderIdCancelBtn').addEventListener('click', cancelBuilderIdLogin);
      pollBuilderIdAuth(d.interval || 5);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  function pollBuilderIdAuth(interval) {
    builderIdPollTimer = setTimeout(async () => {
      const res = await api('/auth/builderid/poll', { method: 'POST', body: JSON.stringify({ sessionId: builderIdSession }) });
      const d = await res.json();
      if (d.completed) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else if (d.success && !d.completed) {
        $('builderIdStatus').textContent = t('builderid.waiting');
        pollBuilderIdAuth(d.interval || interval);
      } else {
        toastError(t('common.failed') + ': ' + (d.error || ''));
        cancelBuilderIdLogin();
      }
    }, interval * 1000);
  }
  function cancelBuilderIdLogin() {
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    builderIdSession = '';
    showModal('add');
  }
  async function startIamSso() {
    if (iamSession) {
      const res = await api('/auth/iam-sso/complete', {
        method: 'POST', body: JSON.stringify({
          sessionId: iamSession, callbackUrl: $('iamCallback').value
        })
      });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
        autoRefreshNewAccount(d.account?.id);
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    } else {
      const res = await api('/auth/iam-sso/start', {
        method: 'POST', body: JSON.stringify({
          startUrl: $('iamStartUrl').value, region: $('iamRegion').value
        })
      });
      const d = await res.json();
      if (d.authorizeUrl) {
        iamSession = d.sessionId;
        $('iamAuthUrl').textContent = d.authorizeUrl;
        $('iamStep2').classList.remove('hidden');
        $('iamBtn').textContent = t('iam.complete');
        $('iamOpenBtn').addEventListener('click', () => window.open($('iamAuthUrl').textContent, '_blank'));
        $('iamCopyBtn').addEventListener('click', async () => {
          if (await copyText($('iamAuthUrl').textContent)) toast(t('common.copied'), 'primary');
        });
      } else toastError(t('common.failed') + ': ' + (d.error || ''));
    }
  }
  async function autoRefreshNewAccount(id) {
    if (!id) return;
    try { await api('/accounts/' + id + '/refresh', { method: 'POST' }); } catch (e) { }
    loadAccounts();
  }

  // Export modal
  function showExportModal() {
    if (!accountsData.length) return toastWarning(t('accounts.empty'));
    exportSelectedIds = new Set(accountsData.map(a => a.id));
    renderExportModal();
    openDialog('exportModal');
  }
  function closeExportModal() { closeDialog('exportModal'); }
  function renderExportModal() {
    const body = $('exportBody');
	const previousPassword = $('exportPassword') ? $('exportPassword').value : '';
    const all = exportSelectedIds.size === accountsData.length;
    body.innerHTML =
      '<div class="flex items-center justify-between mb-3">' +
      '<span class="text-sm muted-text">' + escapeHtml(t('export.selected', exportSelectedIds.size)) + '</span>' +
      '<button class="btn btn-sm btn-outline" id="exportToggleAllBtn" type="button">' + escapeHtml(all ? t('export.deselectAll') : t('export.selectAll')) + '</button>' +
      '</div>' +
      '<div class="export-list">' +
      accountsData.map(a => {
        const checked = exportSelectedIds.has(a.id);
        return '<label class="export-row' + (checked ? ' selected' : '') + '">' +
          '<input type="checkbox" ' + (checked ? 'checked' : '') + ' data-export-toggle="' + escapeAttr(a.id) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(getDisplayEmail(a.email, a.id)) + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + ' · ' + escapeHtml(formatSubscriptionLabel(a.subscriptionType)) + '</div>' +
          '</div>' +
          '</label>';
      }).join('') +
      '</div>' +
	  '<div class="form-group mt-3"><label for="exportPassword">' + escapeHtml(t('security.reauthPassword')) + '</label>' +
	  '<input type="password" id="exportPassword" autocomplete="current-password" /></div>' +
      '<div id="exportJsonPreview" class="hidden mb-3"><textarea id="exportJsonText" readonly class="font-mono"></textarea></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="exportCloseBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-outline" id="exportShowJsonBtn" type="button">' + escapeHtml(t('export.showJson')) + '</button>' +
      '<button class="btn btn-outline" id="exportCopyJsonBtn" type="button">' + escapeHtml(t('export.copyJson')) + '</button>' +
      '<button class="btn btn-primary" id="exportDownloadBtn" type="button">' + escapeHtml(t('export.downloadJson')) + '</button>' +
      '</div>';
	$('exportPassword').value = previousPassword;
    $('exportToggleAllBtn').addEventListener('click', () => {
      if (exportSelectedIds.size === accountsData.length) exportSelectedIds.clear();
      else exportSelectedIds = new Set(accountsData.map(a => a.id));
      renderExportModal();
    });
    $('exportCloseBtn').addEventListener('click', closeExportModal);
    $('exportShowJsonBtn').addEventListener('click', exportShowJson);
    $('exportCopyJsonBtn').addEventListener('click', exportCopyJson);
    $('exportDownloadBtn').addEventListener('click', exportDownloadJson);
    qsa('[data-export-toggle]', body).forEach(cb => cb.addEventListener('change', e => {
      const id = e.target.dataset.exportToggle;
      if (exportSelectedIds.has(id)) exportSelectedIds.delete(id);
      else exportSelectedIds.add(id);
      renderExportModal();
    }));
  }
  async function getExportData() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return null; }
	const password = $('exportPassword').value;
	if (!password) { toastWarning(t('security.reauthRequired')); return null; }
	const res = await api('/export', { method: 'POST', body: JSON.stringify({ ids: Array.from(exportSelectedIds), password }) });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toastError(t('common.failed') + ': ' + (err.error || t('common.unknownError')));
      return null;
    }
    return res.json();
  }
  async function exportShowJson() {
    const data = await getExportData();
    if (!data) return;
    $('exportJsonPreview').classList.remove('hidden');
    $('exportJsonText').value = JSON.stringify(data, null, 2);
  }
  async function exportCopyJson() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return; }
    const jsonPromise = getExportData().then(data => {
      if (!data) throw new Error('no-data');
      const filtered = (data.accounts || []).map(a => {
        const { clientId, clientSecret, accessToken, refreshToken, kiroApiKey, authMethod, provider } = a.credentials || {};
        return { clientId, clientSecret, accessToken, refreshToken, kiroApiKey, authMethod, provider };
      });
      return JSON.stringify(filtered, null, 2);
    });
    try {
      if (await copyText(jsonPromise)) toast(t('export.copied'), 'primary');
    } catch (e) {
      if (e && e.message !== 'no-data') toastError(t('common.failed'));
    }
  }
  async function exportDownloadJson() {
    const data = await getExportData();
    if (!data) return;
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-accounts-' + new Date().toISOString().slice(0, 10) + '.json';
    a.click();
    URL.revokeObjectURL(url);
  }

  // Version and update
  function renderVersionBadge() {
    const badge = $('versionBadge');
    if (badge && currentVersion) badge.textContent = currentVersion.replace(/^v/i, '');
  }
  async function loadVersion() {
    try {
      const res = await api('/version');
      const d = await res.json();
      currentVersion = d.version || '';
      renderVersionBadge();
    } catch (e) { }
  }
  function compareVersions(a, b) {
    const pa = a.split('.').map(Number);
    const pb = b.split('.').map(Number);
    for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
      const na = pa[i] || 0, nb = pb[i] || 0;
      if (na > nb) return 1;
      if (na < nb) return -1;
    }
    return 0;
  }
  function setUpdateButtonLoading(loading) {
    const btn = $('checkUpdateBtn');
    if (!btn) return;
    btn.disabled = loading;
    if (loading) btn.setAttribute('aria-busy', 'true');
    else btn.removeAttribute('aria-busy');
    const label = btn.querySelector('[data-update-label]');
    const icon = btn.querySelector('i');
    if (label) label.textContent = t(loading ? 'update.checking' : 'update.check');
    if (icon) icon.classList.toggle('fa-spin', loading);
  }
  async function checkUpdate(manual) {
    if (manual) setUpdateButtonLoading(true);
    try {
      if (!currentVersion) await loadVersion();
      const current = currentVersion.replace(/^v/i, '');
      if (!current) throw new Error('Current version missing');
      const res = await fetch('https://raw.githubusercontent.com/0a00/Kiro-Go-Plus/main/version.json?t=' + Date.now());
      if (!res.ok) throw new Error('Fetch failed');
      const d = await res.json();
      const latest = (d.version || '').replace(/^v/i, '');
      if (!latest) throw new Error('Latest version missing');
      if (latest && latest !== current && compareVersions(latest, current) > 0) {
        if (manual) showUpdateModal(latest, d.download, d.changelog);
        else showUpdateToast('available', current, latest);
      } else if (manual) {
        showUpdateToast('current', current, latest || current);
      }
    } catch (e) {
      if (manual) showUpdateToast('error', '', '');
    } finally {
      if (manual) setUpdateButtonLoading(false);
    }
  }
  function showUpdateToast(status, current, latest) {
    if (status === 'available') {
      toast(t('update.availableToast') + (latest ? ': ' + latest : ''), 'warning', {
        icon: 'fa-solid fa-arrow-up',
        duration: 5200,
        onClick: function () { checkUpdate(true); }
      });
      return;
    }
    if (status === 'current') {
      toast(t('update.noUpdatesToast'), 'success', {
        icon: 'fa-solid fa-circle-check',
        duration: 3600
      });
      return;
    }
    toast(t('update.checkFailed'), 'error', {
      icon: 'fa-solid fa-triangle-exclamation',
      duration: 4200
    });
  }
  function showUpdateModal(version, url, changelog) {
    const current = currentVersion.replace(/^v/i, '');
    $('updateBody').innerHTML =
      '<div class="update-shell">' +
      '<div class="update-hero">' +
      '<div class="update-result-icon update-result-info"><i class="fa-solid fa-arrow-up"></i></div>' +
      '<div>' +
      '<h3 class="update-hero-title">' + escapeHtml(t('update.newVersion')) + '</h3>' +
      '<p class="update-hero-copy">' + escapeHtml(t('update.newVersionMessage')) + '</p>' +
      '</div>' +
      '</div>' +
      '<div class="update-version-grid">' +
      '<div class="update-version-card update-version-card-current"><p class="update-version-label">' + escapeHtml(t('update.current')) + '</p><p class="update-version-value update-version-value-current">' + escapeHtml(current) + '</p></div>' +
      '<div class="update-version-card update-version-card-latest"><p class="update-version-label">' + escapeHtml(t('update.latest')) + '</p><p class="update-version-value update-version-value-success">' + escapeHtml(version) + '</p></div>' +
      '</div>' +
      (changelog ? '<div class="update-notes"><p class="update-notes-title">' + escapeHtml(t('update.changelog')) + '</p><p class="update-notes-body">' + escapeHtml(changelog) + '</p></div>' : '') +
      '<div class="update-actions"><a href="' + escapeAttr(url) + '" target="_blank" rel="noopener" class="btn btn-primary">' + escapeHtml(t('update.goDownload')) + '</a></div>' +
      '</div>';
    openDialog('updateModal');
  }
  function showUpdateStatusModal(status, title, message, latest) {
    const current = currentVersion.replace(/^v/i, '');
    const isError = status === 'error';
    $('updateBody').innerHTML =
      '<div class="update-shell">' +
      '<div class="text-center mb-5">' +
      '<div class="update-result-icon update-status-icon update-result-' + (isError ? 'error' : 'success') + '">' +
      '<i class="fa-solid ' + (isError ? 'fa-triangle-exclamation' : 'fa-circle-check') + '"></i>' +
      '</div>' +
      '<p class="text-base font-semibold ' + (isError ? 'danger-text' : 'success-text') + '">' + escapeHtml(title) + '</p>' +
      '<p class="text-sm mt-2 muted-text">' + escapeHtml(message) + '</p>' +
      '</div>' +
      '<div class="update-version-grid">' +
      '<div class="update-version-card update-version-card-current"><p class="update-version-label">' + escapeHtml(t('update.current')) + '</p><p class="update-version-value update-version-value-current">' + escapeHtml(current || '-') + '</p></div>' +
      '<div class="update-version-card' + (!isError ? ' update-version-card-latest' : '') + '"><p class="update-version-label">' + escapeHtml(t('update.latest')) + '</p><p class="update-version-value' + (!isError ? ' update-version-value-success' : '') + '">' + escapeHtml(latest || '-') + '</p></div>' +
      '</div>' +
      '</div>';
    openDialog('updateModal');
  }
  function closeUpdateModal() { closeDialog('updateModal'); }

  // Tabs
  function switchTab(tab) {
    qsa('.tab').forEach(el => el.classList.toggle('active', el.dataset.tab === tab));
    qsa('.tab-content').forEach(c => c.classList.add('hidden'));
    $('tab' + tab.charAt(0).toUpperCase() + tab.slice(1)).classList.remove('hidden');
    if (requestRefreshTimer) {
      clearInterval(requestRefreshTimer);
      requestRefreshTimer = null;
    }
    if (tab === 'requests') {
      loadRequests();
      requestRefreshTimer = setInterval(loadRequests, 5000);
    }
  }

  // Event wiring
  function bindLoginEvents() {
    $('loginBtn').addEventListener('click', login);
    $('pwdField').addEventListener('keypress', e => { if (e.key === 'Enter') login(); });

    const pwdToggle = $('pwdToggle');
    if (pwdToggle) {
      pwdToggle.addEventListener('click', () => {
        const f = $('pwdField');
        const willShow = f.type === 'password';
        f.type = willShow ? 'text' : 'password';
        pwdToggle.dataset.shown = String(willShow);
        pwdToggle.setAttribute('aria-label', willShow ? t('login.hidePassword') : t('login.showPassword'));
        pwdToggle.innerHTML = willShow
          ? '<i class="fa-solid fa-eye-slash"></i>'
          : '<i class="fa-solid fa-eye"></i>';
      });
    }
  }

  function bindShellEvents() {
    const checkUpdateBtn = $('checkUpdateBtn');
    if (checkUpdateBtn) checkUpdateBtn.addEventListener('click', () => checkUpdate(true));

    document.body.addEventListener('click', e => {
      if (!e.target.closest('.custom-select')) closeAllCustomSelects();
      const lb = e.target.closest('.lang-btn');
      if (lb) setLang(lb.dataset.lang);
      const lt = e.target.closest('.lang-toggle');
      if (lt) toggleLang();
    });
    window.addEventListener('resize', positionOpenCustomSelects);
    window.addEventListener('scroll', positionOpenCustomSelects, true);

    $('loginThemeToggle').addEventListener('click', toggleTheme);
    $('mainThemeToggle').addEventListener('click', toggleTheme);
    $('logoutBtn').addEventListener('click', logout);

    qsa('#tabBar .tab').forEach(tab => tab.addEventListener('click', () => switchTab(tab.dataset.tab)));

    qsa('[data-copy]').forEach(btn => btn.addEventListener('click', async () => {
      const id = btn.dataset.copy;
      const target = $(id);
      if (!target) return;
      try {
        if (await copyText(target.dataset.rawValue || target.textContent)) toast(t('common.copied'), 'primary');
      } catch (e) {
        toast(t('common.failed'), 'error');
      }
    }));
  }

  function bindAccountEvents() {
    $('privacyModeToggle').addEventListener('change', e => {
      privacyModeEnabled = e.target.checked;
      localStorage.setItem('privacyMode', privacyModeEnabled);
      renderAccounts();
    });

    $('exportBtn').addEventListener('click', showExportModal);
    $('refreshAllModelsBtn').addEventListener('click', refreshAllModels);
    $('addAccountBtn').addEventListener('click', () => showModal('add'));

    $('selectAllCheckbox').addEventListener('change', e => toggleSelectAll(e.target.checked));
    qsa('[data-batch]').forEach(b => b.addEventListener('click', () => {
      const a = b.dataset.batch;
      if (a === 'refreshModels') batchRefreshModels();
      else if (a === 'delete') batchDelete();
      else batchAction(a);
    }));

    $('filterSearch').addEventListener('input', onFilterChange);
    $('filterStatusSelect').addEventListener('change', onFilterChange);

    $('accountsList').addEventListener('click', e => {
      const cb = e.target.closest('.account-checkbox');
      if (cb) {
        toggleSelectAccount(cb.dataset.id);
        const card = cb.closest('.account-card');
        if (card) card.classList.toggle('selected', cb.checked);
        return;
      }
      const btn = e.target.closest('button[data-action]');
      if (!btn) return;
      const id = btn.dataset.id;
      const action = btn.dataset.action;
      if (action === 'refresh') refreshAccount(id, btn.closest('.account-card'));
      else if (action === 'detail') showDetail(id);
      else if (action === 'copyJSON') copyAccountJSON(id, btn);
      else if (action === 'toggle') toggleAccount(id, btn.dataset.enabled === 'true');
      else if (action === 'switchProfile') openSwitchProfileModal(id, btn);
      else if (action === 'test') testAccount(id);
      else if (action === 'delete') deleteAccount(id);
    });
  }

  function bindSettingsEvents() {
    $('saveRequireApiKeyBtn').addEventListener('click', saveRequireApiKey);
    $('saveRuntimeBtn').addEventListener('click', saveRuntimeConfig);
    $('saveRoutingBtn').addEventListener('click', saveRoutingConfig);
    $('saveAutoRefreshBtn').addEventListener('click', saveAutoRefreshConfig);
    $('saveRetryBtn').addEventListener('click', saveRetryConfig);
    $('saveResponsesStorageBtn').addEventListener('click', saveResponsesStorageConfig);
    $('purgeResponsesStorageBtn').addEventListener('click', purgeResponsesStorage);
    $('saveModelRegistryBtn').addEventListener('click', saveModelRegistryConfig);
    $('saveHealthBtn').addEventListener('click', saveHealthConfig);
    $('saveOverUsageBtn').addEventListener('click', saveOverUsageConfig);
    $('saveUpstreamProtectionBtn').addEventListener('click', saveUpstreamProtectionConfig);
    $('savePromptCacheBtn').addEventListener('click', savePromptCacheConfig);
    $('clearPromptCacheBtn').addEventListener('click', clearPromptCache);
    $('saveDiagnosticsBtn').addEventListener('click', saveDiagnosticsConfig);
    $('saveRequestLogBtn').addEventListener('click', saveRequestLogConfig);
    $('saveCountTokensProviderBtn').addEventListener('click', saveCountTokensProviderConfig);
    $('saveWebSearchBtn').addEventListener('click', saveWebSearchConfig);
    $('saveThinkingBtn').addEventListener('click', saveThinkingConfig);
    $('saveEndpointBtn').addEventListener('click', saveEndpointConfig);
    $('changePasswordBtn').addEventListener('click', changePassword);
    $('proxyType').addEventListener('change', onProxyTypeChange);
    $('saveProxyBtn').addEventListener('click', saveProxyConfig);
    $('resetStatsBtn').addEventListener('click', resetStats);
    bindApiKeyEvents();
  }

  function bindRequestEvents() {
    const btn = $('refreshRequestsBtn');
    if (btn) btn.addEventListener('click', loadRequests);
  }

  function bindPromptFilterEvents() {
    $('savePromptFilterBtn').addEventListener('click', savePromptFilter);
    $('addRuleRegexBtn').addEventListener('click', () => addPromptRule('regex'));
    $('addRuleContainsBtn').addEventListener('click', () => addPromptRule('lines-containing'));

    $('promptFilterRules').addEventListener('input', e => {
      const idx = e.target.dataset.ruleIdx;
      const field = e.target.dataset.ruleField;
      if (idx != null && field) promptRules[idx][field] = e.target.value;
    });
    $('promptFilterRules').addEventListener('change', e => {
      if (e.target.dataset.ruleToggle != null) {
        promptRules[e.target.dataset.ruleToggle].enabled = e.target.checked;
        renderPromptRules();
      }
    });
    $('promptFilterRules').addEventListener('click', e => {
      const rm = e.target.closest('[data-rule-remove]');
      if (rm) { promptRules.splice(parseInt(rm.dataset.ruleRemove, 10), 1); renderPromptRules(); }
    });
  }

  function bindModalEvents() {
    $('addModalClose').addEventListener('click', closeModal);
    $('detailModalClose').addEventListener('click', closeDetailModal);
    $('exportModalClose').addEventListener('click', closeExportModal);
    $('testModalClose').addEventListener('click', closeTestModal);
    $('updateModalClose').addEventListener('click', closeUpdateModal);
    $('clipboardFallbackClose').addEventListener('click', closeClipboardFallback);
    $('clipboardFallbackCancel').addEventListener('click', closeClipboardFallback);
    $('clipboardFallbackCopy').addEventListener('click', copyClipboardFallbackValue);
    [
      ['addModal', closeModal],
      ['detailModal', closeDetailModal],
      ['exportModal', closeExportModal],
      ['testModal', closeTestModal],
      ['updateModal', closeUpdateModal],
      ['confirmModal', () => closeConfirm(false)],
      ['clipboardFallbackModal', closeClipboardFallback],
    ].forEach(([id, fn]) => bindDialogBackdropClose(id, fn));

    $('modalBody').addEventListener('click', e => {
      const m = e.target.closest('[data-method]');
      if (m) { showModal(m.dataset.method); return; }
      const g = e.target.closest('[data-modal-goto]');
      if (g) { showModal(g.dataset.modalGoto); return; }
      if (e.target.dataset.closeAdd) closeModal();
    });
  }

  function bindDetailEvents() {
    $('detailBody').addEventListener('click', e => {
      if (e.target.id === 'generateMachineIdBtn') { generateMachineId(); return; }
      const b = e.target.closest('[data-detail-action]');
      if (!b) return;
      const id = b.dataset.id;
      const a = b.dataset.detailAction;
      if (a === 'saveMachineId') saveMachineId(id);
      else if (a === 'saveWeight') saveWeight(id);
      else if (a === 'toggleOverage') toggleOverageSwitch(id, b);
      else if (a === 'refreshOverage') refreshAccountOverage(id);
      else if (a === 'saveProxyURL') saveProxyURL(id);
      else if (a === 'loadModels') loadModels(id);
      else if (a === 'refreshModels') refreshAccountModels(id);
    });
  }

  function bindTestEvents() {
    $('testBody').addEventListener('click', e => {
      if (e.target.id === 'testLogClear') { clearTestLog(); return; }
      if (e.target.id === 'testModalCancelBtn') { closeTestModal(); return; }
      const run = e.target.closest('#testRunBtn');
      if (run) runTestAccount(run.dataset.id, getTestModelValue());
    });
    $('testBody').addEventListener('keydown', e => {
      if (e.key !== 'Enter') return;
      if (!e.target.closest('#testModelChoice')) return;
      const run = $('testRunBtn');
      if (!run || run.disabled) return;
      e.preventDefault();
      runTestAccount(run.dataset.id, getTestModelValue());
    });
  }

  function wireEvents() {
    bindLoginEvents();
    bindShellEvents();
    bindAccountEvents();
    bindSettingsEvents();
    bindRequestEvents();
    bindPromptFilterEvents();
    bindModalEvents();
    bindDetailEvents();
    bindTestEvents();
  }

  // Init
  async function init() {
    initTheme();
    await loadLocale(currentLang);
    if (currentLang !== 'zh') await loadLocale('zh');
    applyTranslations();
    initCustomSelectObserver();
    initPrivacyMode();
    initRememberMe();
    const yr = $('footerYear');
    if (yr) yr.textContent = new Date().getFullYear();
    wireEvents();
    tryAutoLogin();
    setInterval(() => {
      if (!$('mainPage').classList.contains('hidden')) loadStats();
    }, 10000);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();

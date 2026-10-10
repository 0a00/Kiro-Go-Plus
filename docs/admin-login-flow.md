# Admin Login Flow (1.2.98)

## Behavior

There is one password form, in the standalone login document. The authenticated
console no longer includes the old login form. `/admin/` continues to select the
login or console document using the existing HTTP-only session; APIs still check
authentication on every request. `/admin/index-legacy.html` redirects to `/admin/`.

Both pages use `appearance.css` for the same design tokens and `appearance.js`
for early theme/language selection. Existing `kiro_theme`, `kiro_lang` and remember
preferences remain compatible. Only non-secret preferences are persisted; old
password storage entries are removed. Blocked storage falls back to per-page
memory, so preferences may not survive navigation but login remains functional.

The login form supports language/theme controls, password visibility and remember
session. A bounded cookie check after login prevents a redirect loop if the browser
does not retain the session. Passwords are cleared after each submitted attempt.

The console starts with a loading state rather than an empty translated form.
Locale and lightweight session checks run together; missing scripts, malformed
locales, network failures and initialization timeouts expose a reload action.
401 responses redirect to the standalone login once; network errors and 5xx do not
masquerade as bad credentials. Back/forward cache restoration rechecks the page.
Initial reads are bounded, but account imports and other mutations keep their
existing request behavior. A failed logout does not claim the session was revoked.

## Privacy and Deployment

Only login assets, shared appearance assets and exact third-party CSS/solid-font
paths are anonymously available. The application, startup controller, translations
and remaining bundles still require authentication. The login does not display
project branding, version or repository links. Appearance is not an anonymity or
anti-ban guarantee. Use HTTPS or a private tunnel for production credentials.

No account/configuration migration is required. Deploy matching binary and web
assets together. Rollback restores the previous binary and assets together, without
changing credentials or encryption keys. These changes do not affect inference,
streaming, upstream headers, retries or production timeout settings.

## Reference Review

Read-only review on 2026-10-10: Go f8f6071 and zsecducna 7ee2ea4 retain locale-first
startup and an embedded login. Rust 22f36e6 changes model tests, not login startup;
its App component and Zhang 5aa7f56 initialize with a login view and read stored
credentials. That credential-storage design was not adopted. Account-manager's
local boot/loading state was a reference for distinguishing loading from login,
not copied implementation. Gateway a5292ca and helper 8c280d7 have no equivalent
browser flow. AIClient2API 4c4c774 changes tool choice, unrelated to login; its
separate-login/401 transition is a reference, without its localStorage token model.
Comparison worktrees were not modified. Fixes adapt the existing session mechanism.

## Verification

`node --test scripts/admin-ui.test.js` covers preferences, blocked storage, startup
state transitions and removal of the duplicate form. Go tests exercise public
asset allowlists, legacy redirect, sessions and traversal rejection. The quick
quality gate includes these regressions and browser-script syntax checks.

`scripts/admin-ui-e2e.cjs` uses Playwright against an isolated loopback instance.
It covers desktop/mobile/short screens, slow locales, resource/API failures,
retry, session expiry, cookie/storage restrictions, single submission, theme and
language continuity, and logout. Set `KIRO_UI_TEST_PASSWORD`, optionally
`KIRO_UI_TEST_URL`, `PLAYWRIGHT_MODULE`, `CHROMIUM_PATH` and
`KIRO_UI_TEST_REPORT_DIR`. It refuses remote URLs and never contacts Kiro accounts.

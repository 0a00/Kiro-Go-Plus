# Privacy Boundaries in 1.2.96

This change reduces unnecessary product/operational metadata disclosure. It does
not make traffic anonymous, guarantee account safety, or bypass provider policy.
No TLS fingerprint spoofing, device rotation or credential rewriting is introduced.

## Public and Administrator Surfaces

- `/` and `/health` return only status. `/ready` preserves 200/503 readiness and
  returns status/ready, without account counts, reasons or version.
- `/admin/api/health`, `/admin/api/ready` and `/admin/api/version` expose details
  only through existing administrator authentication. Customer-key access is not
  administrator access. Customer statistics omit the service version; customer
  logs retain a generic `upstream` endpoint class and their timing/error categories.
- Anonymous `/admin` visits receive a minimal bilingual login page. Only its HTML,
  JavaScript and CSS are public. Dashboard, locale and application vendor assets
  require the existing HTTP-only session. Logout/session expiry prevents fresh
  bundle downloads. Previously downloaded browser content cannot be recalled.
  In 1.2.98 shared appearance CSS/JS and the exact Font Awesome CSS/solid font
  paths used by the login controls are also public. No wildcard vendor access
  is allowed. The legacy HTML route redirects to the canonical `/admin/` flow.
- Asset lookup rejects traversal, directories and symlinks resolving outside web/.
  Deployment-owned asset files must not be writable by untrusted local users;
  pathname checks do not defend against an attacker replacing files concurrently.
- Client API failures use bounded generic categories while retaining HTTP status,
  SSE failure semantics and retry headers. Full causes stay in administrator logs
  and bounded details under existing redaction/retention settings. Client-side
  validation messages and requested model IDs remain useful for correcting input.
- A missing Responses encryption key yields a storage-unavailable message with
  `store: false` as an alternative; deployment variable names stay in server logs.

## Outbound Requests

The old SSO registration display name `Kiro API Proxy` becomes `API Client`.
Legacy `KiroAPIProxy` suffixes are removed from the user-information request.
Authentication transports add `HTTPClient/1.0` only when an endpoint has not set
an explicit User-Agent. They preserve request ownership, authorization, cancellation
and idle connection cleanup. This fallback identifies an HTTP client; it is not
a claim to be the official application. Login/refresh paths need live compatibility
verification before a broad rollout because providers can enforce header rules.

The caller's X-Request-Id remains in local response headers/context/logs but is no
longer forwarded to Kiro. AWS invocation IDs remain request-scoped. Streaming,
control-plane and MCP protocol-specific headers are unchanged.

Stable account machine IDs, OAuth client IDs, tokens, regions, profile ARNs and
TokenType are preserved. No account-wide identity migration or repeated refresh
is performed. Existing Kiro-version/OS/Node settings remain untouched. Go TLS/HTTP
behavior, traffic timing and the fact that this is a proxy remain observable.

## Deliberately Preserved

Authenticated dashboard branding and manual GitHub update checks remain available.
Source copyright/license notices, module paths, container service names and
encryption derivation/AAD markers remain intact. Renaming encryption domains would
break old Responses data and would not hide network traffic. Credential/cache
fingerprints are internal keys, not an outbound device-identification scheme.

Caller prompts, session continuity and existing long-tool chunking instructions
are not stripped or rewritten for concealment. Complete diagnostics still contain
sanitized prompts/output and must be protected. External Count Tokens sends the
configured request to an operator-selected endpoint when enabled; webhook alerts
send operational data to their configured target. This change does not silently
disable those integrations. HTTPS or a private tunnel remains necessary for admin
credentials, especially on public deployments using plain HTTP.

## Upgrade and Rollback

No stored credentials/configuration migration or master-key change is required.
Deploy the binary and matching web/ assets together. Existing valid in-memory
sessions continue; normal server restarts already require login again. Scripts
depending on anonymous version/account fields must use authenticated admin APIs
or `docker compose exec -T kiro-go ./kiro-go --version`. The version command runs
before config creation, file locking or network setup. Container liveness/updater
checks depend on status/exit code and remain compatible.

Rollback restores previous binary and web/ together. Existing encrypted data,
account identifiers and cookies have not changed. Rollback reopens the previous
public metadata surfaces and restores the legacy auth labels.

## Reference Review and Verification

Read-only review on 2026-10-05: Go f8f6071/7ee2ea4 retain the old SSO labels;
Rust e09625c (request implementation 5ca5703) and Zhang 5aa7f56 retain stable
machine IDs and endpoint-specific headers; account-manager 844aac76 does likewise.
Gateway a5292ca derives an installation fingerprint; that pattern was not copied.
AIClient2API 9a29d60 uses endpoint-specific SDK/app identifiers; helper 8c280d7
registers explicit OAuth clients. None establishes a universal fingerprint-free
or ban-proof request format. No comparison worktree was modified or code copied.

Focused tests cover auth header override/mutation, transport cleanup, SSO request
labels, local-only request ID correlation, public/admin probe separation, asset
session expiry/traversal, and JSON/SSE failure privacy across three protocols with
admin cause preservation. Browser checks exercise login/error/logout and asset
gating on desktop/mobile. Verification on 2026-10-05 passed the full offline gate
(shuffled unit tests, vet, builds, JavaScript, Compose validation, race detector,
20 repeated stream/tool runs, updater regressions and govulncheck), then a fresh
quick gate and five focused race runs after the final storage-error adjustment.
Govulncheck found no reachable vulnerable symbols or imported packages; it reported
21 module-level advisories outside the code's call paths. No Docker image build or
production request was performed. A secret scan of the staged changes is required
before publishing. Live provider login/refresh and ban-rate outcomes are not
established by fake transport tests.

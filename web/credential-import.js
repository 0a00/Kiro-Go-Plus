(function (root, factory) {
  'use strict';

  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  if (root) root.KiroCredentialImport = api;
})(typeof globalThis !== 'undefined' ? globalThis : this, function () {
  'use strict';

  const DEFAULT_MAX_ACCOUNTS = 5000;
  const DEFAULT_MAX_BYTES = 8 * 1024 * 1024;
  const externalAuthAliases = new Set(['external_idp', 'azuread', 'azure_ad', 'azure', 'entra', 'entra_id', 'microsoft', 'm365', 'office365', 'external']);
  const idcAuthAliases = new Set(['idc', 'builderid', 'builder_id', 'enterprise', 'identity_center', 'aws_sso']);
  const socialAuthAliases = new Set(['social', 'google', 'github']);
  const apiKeyAuthAliases = new Set(['api_key', 'apikey', 'kiro_api_key']);

  function codedError(code) {
    const error = new Error(code);
    error.code = code;
    return error;
  }

  function isRecord(value) {
    return value !== null && typeof value === 'object' && !Array.isArray(value);
  }

  function normalizeCredentialAuthLabel(value) {
    return String(value || '').trim().toLowerCase().replace(/[-\s]+/g, '_');
  }

  function firstCredentialValue(...values) {
    for (const value of values) {
      if (value === undefined || value === null) continue;
      if (typeof value === 'string' && value.trim() === '') continue;
      return value;
    }
    return undefined;
  }

  function normalizeImportCredentialItem(item) {
    const record = isRecord(item) ? item : {};
    const credentials = isRecord(record.credentials) ? record.credentials : {};
    return {
      ...record,
      email: firstCredentialValue(record.email, record.username, record.preferred_username, record.upn),
      userId: firstCredentialValue(record.userId, record.user_id),
      machineId: firstCredentialValue(record.machineId, record.machine_id),
      accessToken: firstCredentialValue(credentials.accessToken, credentials.access_token, record.accessToken, record.access_token),
      refreshToken: firstCredentialValue(credentials.refreshToken, credentials.refresh_token, record.refreshToken, record.refresh_token),
      kiroApiKey: firstCredentialValue(credentials.kiroApiKey, credentials.kiro_api_key, record.kiroApiKey, record.kiro_api_key),
      clientId: firstCredentialValue(credentials.clientId, credentials.client_id, record.clientId, record.client_id),
      clientSecret: firstCredentialValue(credentials.clientSecret, credentials.client_secret, record.clientSecret, record.client_secret),
      authMethod: firstCredentialValue(credentials.authMethod, credentials.auth_method, record.authMethod, record.auth_method),
      provider: firstCredentialValue(credentials.provider, credentials.idp, record.provider, record.idp),
      region: firstCredentialValue(credentials.region, record.region),
      authRegion: firstCredentialValue(credentials.authRegion, credentials.auth_region, record.authRegion, record.auth_region),
      startUrl: firstCredentialValue(credentials.startUrl, credentials.start_url, record.startUrl, record.start_url),
      expiresAt: firstCredentialValue(credentials.expiresAt, credentials.expires_at, record.expiresAt, record.expires_at),
      profileArn: firstCredentialValue(credentials.profileArn, credentials.profile_arn, record.profileArn, record.profile_arn),
      tokenEndpoint: firstCredentialValue(credentials.tokenEndpoint, credentials.token_endpoint, record.tokenEndpoint, record.token_endpoint),
      issuerUrl: firstCredentialValue(credentials.issuerUrl, credentials.issuer_url, record.issuerUrl, record.issuer_url),
      scopes: firstCredentialValue(credentials.scopes, record.scopes)
    };
  }

  function classifyCredentialAuthLabel(label) {
    if (apiKeyAuthAliases.has(label)) return 'api_key';
    if (externalAuthAliases.has(label)) return 'external_idp';
    if (idcAuthAliases.has(label)) return 'idc';
    if (socialAuthAliases.has(label)) return 'social';
    return '';
  }

  function isDeclaredCredentialAPIKey(item) {
    const record = isRecord(item) ? item : {};
    return apiKeyAuthAliases.has(normalizeCredentialAuthLabel(record.authMethod)) ||
      apiKeyAuthAliases.has(normalizeCredentialAuthLabel(record.provider));
  }

  function isGenericSocialIDCConflict(item) {
    const record = isRecord(item) ? item : {};
    const method = normalizeCredentialAuthLabel(record.authMethod);
    const provider = normalizeCredentialAuthLabel(record.provider);
    return method === 'social' &&
      (provider === 'builderid' || provider === 'enterprise') &&
      Boolean(String(record.refreshToken || '').trim()) &&
      Boolean(String(record.clientId || '').trim()) &&
      Boolean(String(record.clientSecret || '').trim());
  }

  function inferCredentialAuthMethod(item, hasKiroApiKey) {
    const record = isRecord(item) ? item : {};
    if (hasKiroApiKey || String(record.kiroApiKey || '').trim()) return 'api_key';

    const method = normalizeCredentialAuthLabel(record.authMethod);
    const provider = normalizeCredentialAuthLabel(record.provider);
    if (apiKeyAuthAliases.has(method) || apiKeyAuthAliases.has(provider)) return 'api_key';
    if (externalAuthAliases.has(method) || externalAuthAliases.has(provider)) return 'external_idp';
    if (provider === 'google' || provider === 'github') return 'social';
    if (isGenericSocialIDCConflict(record)) return 'idc';

    const methodClass = classifyCredentialAuthLabel(method);
    if (methodClass) return methodClass;
    const providerClass = classifyCredentialAuthLabel(provider);
    if (providerClass) return providerClass;

    // Endpoint metadata is only an inference hint without an explicit label.
    if (!method && !provider && (record.tokenEndpoint || record.issuerUrl)) return 'external_idp';
    if (method || provider) return record.clientId && record.clientSecret ? 'idc' : 'social';
    return record.clientId ? 'idc' : 'social';
  }

  function extractCredentialRecords(value) {
    let records;
    if (Array.isArray(value)) {
      records = value;
    } else if (isRecord(value)) {
      if (Object.prototype.hasOwnProperty.call(value, 'accounts')) {
        if (!Array.isArray(value.accounts)) throw codedError('invalid_structure');
        records = value.accounts;
      } else {
        records = [value];
      }
    } else {
      throw codedError('invalid_structure');
    }

    if (records.length === 0) throw codedError('empty_accounts');
    if (records.some(record => !isRecord(record))) throw codedError('invalid_structure');
    return records;
  }

  function utf8ByteLength(value) {
    const text = String(value == null ? '' : value);
    let bytes = 0;
    for (let index = 0; index < text.length; index++) {
      const code = text.charCodeAt(index);
      if (code < 0x80) {
        bytes += 1;
      } else if (code < 0x800) {
        bytes += 2;
      } else if (code >= 0xd800 && code <= 0xdbff && index + 1 < text.length) {
        const next = text.charCodeAt(index + 1);
        if (next >= 0xdc00 && next <= 0xdfff) {
          bytes += 4;
          index++;
        } else {
          bytes += 3;
        }
      } else {
        bytes += 3;
      }
    }
    return bytes;
  }

  function readFileText(file) {
    if (file && typeof file.text === 'function') return file.text();
    if (typeof FileReader === 'undefined') return Promise.reject(codedError('read_failed'));
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(String(reader.result || ''));
      reader.onerror = () => reject(codedError('read_failed'));
      reader.readAsText(file);
    });
  }

  async function analyzeCredentialFiles(files, options) {
    const selected = Array.from(files || []);
    const settings = options || {};
    const maxAccounts = Number(settings.maxAccounts) || DEFAULT_MAX_ACCOUNTS;
    const maxBytes = Number(settings.maxBytes) || DEFAULT_MAX_BYTES;
    const result = {
      fileCount: selected.length,
      accountCount: 0,
      sourceBytes: 0,
      items: [],
      errors: []
    };

    const declaredBytes = selected.reduce((total, file) => {
      const size = Number(file && file.size);
      return total + (Number.isFinite(size) && size > 0 ? size : 0);
    }, 0);
    if (declaredBytes > maxBytes) {
      result.sourceBytes = declaredBytes;
      result.errors.push({ code: 'selection_too_large' });
      return result;
    }

    for (let index = 0; index < selected.length; index++) {
      let text;
      try {
        text = await readFileText(selected[index]);
      } catch (error) {
        result.errors.push({ index, code: 'read_failed' });
        continue;
      }

      result.sourceBytes += utf8ByteLength(text);
      if (result.sourceBytes > maxBytes) {
        result.items = [];
        result.accountCount = 0;
        result.errors = [{ code: 'selection_too_large' }];
        return result;
      }

      let value;
      try {
        value = JSON.parse(text.replace(/^\uFEFF/, ''));
      } catch (error) {
        result.errors.push({ index, code: 'invalid_json' });
        continue;
      }

      try {
        result.items.push(...extractCredentialRecords(value));
      } catch (error) {
        result.errors.push({ index, code: error && error.code ? error.code : 'invalid_structure' });
      }
    }

    result.accountCount = result.items.length;
    if (result.accountCount > maxAccounts) result.errors.push({ code: 'too_many_accounts' });
    return result;
  }

  function validateCredentialBatch(items, options) {
    const records = Array.from(items || []);
    const settings = options || {};
    const maxAccounts = Number(settings.maxAccounts) || DEFAULT_MAX_ACCOUNTS;
    const maxBytes = Number(settings.maxBytes) || DEFAULT_MAX_BYTES;
    if (records.length === 0) return { code: 'empty_accounts', bytes: 0 };
    if (records.length > maxAccounts) return { code: 'too_many_accounts', bytes: 0 };
    const bytes = utf8ByteLength(JSON.stringify({ accounts: records }));
    if (bytes > maxBytes) return { code: 'payload_too_large', bytes };
    return { code: '', bytes };
  }

  return Object.freeze({
    DEFAULT_MAX_ACCOUNTS,
    DEFAULT_MAX_BYTES,
    analyzeCredentialFiles,
    extractCredentialRecords,
    inferCredentialAuthMethod,
    isDeclaredCredentialAPIKey,
    isGenericSocialIDCConflict,
    normalizeImportCredentialItem,
    normalizeCredentialAuthLabel,
    utf8ByteLength,
    validateCredentialBatch
  });
});

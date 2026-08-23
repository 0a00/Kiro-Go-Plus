(function (root, factory) {
  'use strict';

  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  if (root) root.KiroCredentialImport = api;
})(typeof globalThis !== 'undefined' ? globalThis : this, function () {
  'use strict';

  const DEFAULT_MAX_ACCOUNTS = 5000;
  const DEFAULT_MAX_BYTES = 8 * 1024 * 1024;

  function codedError(code) {
    const error = new Error(code);
    error.code = code;
    return error;
  }

  function isRecord(value) {
    return value !== null && typeof value === 'object' && !Array.isArray(value);
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
    utf8ByteLength,
    validateCredentialBatch
  });
});

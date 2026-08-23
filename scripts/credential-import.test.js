'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const credentialImport = require('../web/credential-import.js');

function jsonFile(value) {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return {
    size: Buffer.byteLength(text),
    text: async () => text
  };
}

test('extractCredentialRecords accepts supported export shapes', () => {
  const flat = { refreshToken: 'refresh-1' };
  const nested = { credentials: { refreshToken: 'refresh-2' } };
  assert.deepEqual(credentialImport.extractCredentialRecords(flat), [flat]);
  assert.deepEqual(credentialImport.extractCredentialRecords([flat, nested]), [flat, nested]);
  assert.deepEqual(credentialImport.extractCredentialRecords({ accounts: [nested] }), [nested]);
});

test('extractCredentialRecords rejects empty and non-object records', () => {
  for (const value of [null, 'token', 1, [], { accounts: [] }, { accounts: 'invalid' }, [null]]) {
    assert.throws(() => credentialImport.extractCredentialRecords(value), error => Boolean(error.code));
  }
});

test('analyzeCredentialFiles aggregates valid files without exposing names', async () => {
  const result = await credentialImport.analyzeCredentialFiles([
    jsonFile('\uFEFF{"refreshToken":"refresh-1"}'),
    jsonFile({ accounts: [{ credentials: { refreshToken: 'refresh-2' } }] })
  ]);
  assert.equal(result.fileCount, 2);
  assert.equal(result.accountCount, 2);
  assert.deepEqual(result.errors, []);
  assert.equal(Object.prototype.hasOwnProperty.call(result, 'fileNames'), false);
});

test('analyzeCredentialFiles reports malformed files and keeps valid records isolated', async () => {
  const result = await credentialImport.analyzeCredentialFiles([
    jsonFile({ refreshToken: 'refresh-1' }),
    jsonFile('{broken'),
    jsonFile({ accounts: [null] })
  ]);
  assert.equal(result.accountCount, 1);
  assert.deepEqual(result.errors.map(error => error.code), ['invalid_json', 'invalid_structure']);
});

test('analyzeCredentialFiles enforces source and account limits', async () => {
  const oversized = await credentialImport.analyzeCredentialFiles([
    { size: 20, text: async () => '{}' }
  ], { maxBytes: 10 });
  assert.deepEqual(oversized.errors.map(error => error.code), ['selection_too_large']);

  const tooMany = await credentialImport.analyzeCredentialFiles([
    jsonFile([{ refreshToken: 'one' }, { refreshToken: 'two' }])
  ], { maxAccounts: 1 });
  assert.deepEqual(tooMany.errors.map(error => error.code), ['too_many_accounts']);
});

test('validateCredentialBatch measures UTF-8 payload bytes', () => {
  assert.equal(credentialImport.utf8ByteLength('A\u4e2d\u{1f600}'), 8);
  assert.equal(credentialImport.validateCredentialBatch([{ refreshToken: 'ok' }]).code, '');
  assert.equal(credentialImport.validateCredentialBatch([{ refreshToken: 'too-large' }], { maxBytes: 8 }).code, 'payload_too_large');
  assert.equal(credentialImport.validateCredentialBatch([{}, {}], { maxAccounts: 1 }).code, 'too_many_accounts');
});

test('inferCredentialAuthMethod repairs only complete generic Social IDC exports', () => {
  for (const provider of ['BuilderId', 'Enterprise']) {
    const item = {
      authMethod: 'social',
      provider,
      refreshToken: 'refresh',
      clientId: 'client',
      clientSecret: 'secret'
    };
    assert.equal(credentialImport.isGenericSocialIDCConflict(item), true);
    assert.equal(credentialImport.inferCredentialAuthMethod(item, false), 'idc');
  }

  assert.equal(credentialImport.inferCredentialAuthMethod({
    authMethod: 'social', provider: 'BuilderId', refreshToken: 'refresh', clientId: 'client'
  }, false), 'social');
});

test('inferCredentialAuthMethod preserves specific and higher-priority providers', () => {
  for (const provider of ['Google', 'GitHub']) {
    assert.equal(credentialImport.inferCredentialAuthMethod({
      authMethod: 'social', provider, refreshToken: 'refresh', clientId: 'client', clientSecret: 'secret'
    }, false), 'social');
  }
  assert.equal(credentialImport.inferCredentialAuthMethod({
    authMethod: 'social', provider: 'Microsoft', refreshToken: 'refresh', clientId: 'client', clientSecret: 'secret'
  }, false), 'external_idp');
  assert.equal(credentialImport.inferCredentialAuthMethod({
    authMethod: 'social', provider: 'Google', kiroApiKey: 'ksk_example'
  }, true), 'api_key');
});

test('explicit Social ignores incidental endpoint metadata', () => {
  assert.equal(credentialImport.inferCredentialAuthMethod({
    authMethod: 'social',
    provider: 'Kiro SSO',
    refreshToken: 'refresh',
    tokenEndpoint: 'https://login.microsoftonline.com/tenant/oauth2/v2.0/token'
  }, false), 'social');
});

test('normalizeImportCredentialItem preserves snake_case credential exports', () => {
  const normalized = credentialImport.normalizeImportCredentialItem({
    preferred_username: 'snake@example.com',
    user_id: 'user-1',
    machine_id: 'machine-1',
    credentials: {
      access_token: 'access',
      refresh_token: 'refresh',
      client_id: 'client',
      client_secret: 'secret',
      auth_method: 'social',
      auth_region: 'eu-north-1',
      start_url: 'https://example.awsapps.com/start',
      profile_arn: 'arn:profile',
      token_endpoint: 'https://login.microsoftonline.com/tenant/oauth2/v2.0/token',
      issuer_url: 'https://login.microsoftonline.com/tenant/v2.0'
    },
    provider: 'BuilderId'
  });

  assert.equal(normalized.email, 'snake@example.com');
  assert.equal(normalized.userId, 'user-1');
  assert.equal(normalized.machineId, 'machine-1');
  assert.equal(normalized.refreshToken, 'refresh');
  assert.equal(normalized.clientId, 'client');
  assert.equal(normalized.clientSecret, 'secret');
  assert.equal(normalized.authMethod, 'social');
  assert.equal(normalized.authRegion, 'eu-north-1');
  assert.equal(normalized.profileArn, 'arn:profile');
  assert.equal(credentialImport.inferCredentialAuthMethod(normalized, false), 'idc');
});

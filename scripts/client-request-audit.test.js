const { test } = require('node:test');
const assert = require('node:assert/strict');
const { sanitizeSnapshot, compareSnapshots, snapshot } = require('./client-request-audit');

const row = (id, code = 200, status = 'success') => ({ requestId: id, statusCode: code, status, error: 'private', accountEmail: 'private', requestBody: 'private' });
const snap = rows => sanitizeSnapshot({ logs: rows });
test('recovered request rejection cannot be a clean PASS', () => {
  const before = snap([row('old')]), after = snap([row('retry'), row('reject', 400, 'rejected'), row('old')]);
  const result = compareSnapshots(before, after);
  assert.equal(result.warning, true); assert.equal(result.unexpected, 1); assert.equal(result.observed, 2);
  assert.equal(JSON.stringify(after).includes('private'), false);
});
test('old errors and deliberate cancellation do not taint a successful scenario', () => {
  const old = row('old', 400, 'rejected');
  assert.equal(compareSnapshots(snap([old]), snap([row('new'), row('cancel', 499, 'failed'), old])).warning, false);
  assert.equal(compareSnapshots(snap([]), snap([row('write', 200, 'delivery_failed')])).unexpected, 1);
});
test('missing logs, retention rollover and reset are incomplete evidence', () => {
  assert.equal(compareSnapshots(snap([]), snap([])).incomplete, true);
  assert.equal(compareSnapshots({ ok: false }, snap([])).unavailable, true);
  assert.equal(compareSnapshots(snap([row('old')]), snap([row('new')])).incomplete, true);
  assert.equal(compareSnapshots(snap([]), snap(Array.from({ length: 500 }, (_, i) => row('r' + i)))).incomplete, true);
  assert.throws(() => snap([{ requestId: 'secret\nheader', status: 'success', statusCode: 200 }]));
});
test('remote guard refuses accidental credential transmission', async () => {
  await assert.rejects(snapshot({ KIRO_DEV_BASE_URL: 'https://example.invalid', KIRO_DEV_API_KEY: 'fixture' }));
  await assert.rejects(snapshot({ KIRO_DEV_BASE_URL: 'http://user:pass@127.0.0.1', KIRO_DEV_API_KEY: 'fixture' }));
});

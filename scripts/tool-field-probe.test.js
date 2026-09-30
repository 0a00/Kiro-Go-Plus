const assert = require('node:assert/strict');
const { test } = require('node:test');
const { createProbe, ProbeEvidence, runProbe } = require('./tool-field-probe');

function fixture() {
  const request = createProbe({ lines: 2, copy: true });
  const text = request.messages[0].content.split('<file_text>\n')[1].split('</file_text>')[0];
  const input = { file_path: '/workspace/probe.txt', old_string: '', new_string: text };
  return { text, input, events: [
    { type: 'content_block_start', index: 0, content_block: { type: 'tool_use', name: 'Edit', input: {} } },
    { type: 'content_block_delta', index: 0, delta: { type: 'input_json_delta', partial_json: JSON.stringify(input) } },
    { type: 'content_block_stop', index: 0 },
    { type: 'message_delta', delta: { stop_reason: 'tool_use' } },
    { type: 'message_stop' },
  ] };
}

test('tool probe requires valid complete content without persisting arguments', () => {
  const { text, events } = fixture(), evidence = new ProbeEvidence();
  events.forEach((e, i) => evidence.record(e, i * 100));
  const summary = evidence.summary(2, text);
  assert.equal(summary.valid, true);
  assert.equal(summary.toolDelivered, true);
  assert.equal(summary.tools[0].maxDeltaBytes, summary.tools[0].bytes);
  assert.equal(summary.tools[0].lines, 2);
  assert.equal(JSON.stringify(summary).includes(text), false);
  assert.equal(JSON.stringify(summary).includes('/workspace/probe.txt'), false);
});

test('missing terminal, invalid JSON, wrong target and copy mismatch do not pass', () => {
  for (const mode of ['terminal', 'json', 'target', 'copy', 'error']) {
    const { text, input, events } = fixture();
    if (mode === 'terminal') events.pop();
    if (mode === 'json') events[1].delta.partial_json = '{';
    if (mode === 'target') events[1].delta.partial_json = JSON.stringify({ ...input, file_path: '/wrong' });
    if (mode === 'copy') events[1].delta.partial_json = JSON.stringify({ ...input, new_string: text.replace('Verify', 'Ensure') });
    if (mode === 'error') events.push({ type: 'error', error: { message: 'private' } });
    const evidence = new ProbeEvidence();
    events.forEach((e, i) => evidence.record(e, i));
    assert.equal(evidence.summary(2, text).valid, false, mode);
    if (mode === 'copy') assert.equal(evidence.summary(2, text).toolDelivered, true);
  }
});

test('probe imposes finite payload and tool-count limits', () => {
  assert.throws(() => createProbe({ lines: 421 }));
  assert.throws(() => createProbe({ lines: 0 }));
  const e = new ProbeEvidence();
  assert.throws(() => e.record({ type: 'content_block_start', index: 0, content_block: { type: 'tool_use', input: { content: 'x'.repeat(1024 * 1024) } } }, 0), /size-limit/);
  e.record(fixture().events[0], 0);
  assert.throws(() => e.record({ type: 'content_block_delta', index: 0, delta: { type: 'input_json_delta', partial_json: 'x'.repeat(1024 * 1024 + 1) } }, 1), /size-limit/);
});

test('probe parses CRLF multiline SSE, performs one request and reports no secrets', async () => {
  const { events } = fixture();
  let calls = 0;
  const wire = events.map(e => `event: ${e.type}\r\ndata: ${JSON.stringify(e)}\r\n\r\n`).join('');
  const result = await runProbe({ baseURL: 'http://127.0.0.1:1', key: 'private-fixture', lines: 2, copy: true,
    fetchImpl: async (url, init) => {
      calls++;
      assert.equal(init.redirect, 'error');
      assert.equal(init.headers['x-api-key'], 'private-fixture');
      assert.equal(JSON.parse(init.body).thinking.type, 'disabled');
      return new Response(wire, { headers: { 'x-request-id': 'req_fixture' } });
    } });
  assert.equal(calls, 1);
  assert.equal(result.valid, true);
  assert.equal(result.requestId, 'req_fixture');
  assert.equal(JSON.stringify(result).includes('private-fixture'), false);
});

test('transport errors and HTTP errors are not retried or echoed', async () => {
  let calls = 0;
  const result = await runProbe({ baseURL: 'http://127.0.0.1:1', key: 'fixture', lines: 1,
    fetchImpl: async () => { calls++; throw Error('private-url-and-token'); } });
  assert.equal(calls, 1);
  assert.equal(result.failure, 'transport-or-protocol');
  assert.equal(result.valid, false);
  assert.equal(JSON.stringify(result).includes('private-url'), false);
  const http = await runProbe({ baseURL: 'http://127.0.0.1:1', key: 'fixture', lines: 1,
    fetchImpl: async () => new Response('private-error-body', { status: 503 }) });
  assert.equal(http.failure, 'http');
  assert.equal(JSON.stringify(http).includes('private-error-body'), false);
  await assert.rejects(runProbe({ baseURL: 'https://user:password@example.invalid', key: 'fixture', lines: 1 }));
});

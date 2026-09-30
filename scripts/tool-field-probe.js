const fs = require('node:fs');
const { performance } = require('node:perf_hooks');

const MAX_WIRE_BYTES = 16 * 1024 * 1024;
const MAX_TOOL_BYTES = 1024 * 1024;

function createProbe({ lines = 42, copy = false, thinking = false, model = 'claude-sonnet-4-5' } = {}) {
  if (!Number.isInteger(lines) || lines < 1 || lines > 420) throw Error('lines must be 1..420');
  const content = Array.from({ length: lines }, (_, i) =>
    `${String(i + 1).padStart(4, '0')}: Verify independent software checks, observable tool results, readable diagnostics and complete file contents before completion.`).join('\n') + '\n';
  const task = copy ? `Copy the following literal text exactly into new_string, without delimiters:\n<file_text>\n${content}</file_text>` :
    `Generate exactly ${lines} lines about ordinary software testing, each at least 105 printable ASCII characters, numbered 0001: onwards with a space after the colon.`;
  return {
    model, max_tokens: 32000, stream: true,
    thinking: thinking ? { type: 'enabled', budget_tokens: 16000 } : { type: 'disabled' },
    system: 'You are a coding assistant. This is a synthetic file-tool protocol test. The caller will validate the tool and will not execute it.',
    messages: [{ role: 'user', content: `Call Edit exactly once to create /workspace/probe.txt using old_string="". ${task} Do not call any other tools. Do not use generation shortcuts or ask for confirmation.` }],
    tools: [{ name: 'Edit', description: 'Modify file contents in place; an empty old_string creates a file.', input_schema: {
      type: 'object', properties: { file_path: { type: 'string' }, old_string: { type: 'string' }, new_string: { type: 'string' }, replace_all: { type: 'boolean' } },
      required: ['file_path', 'old_string', 'new_string'],
    } }],
  };
}

class ProbeEvidence {
  constructor() { this.tools = new Map(); this.events = 0; this.heartbeats = 0; this.terminal = false; this.errors = 0; this.firstTextMs = null; this.firstThinkingMs = null; }
  record(event, ms) {
    this.events++;
    if (event.type === 'ping') this.heartbeats++;
    if (event.type === 'error') this.errors++;
    if (event.type === 'message_stop') this.terminal = true;
    if (event.type === 'message_delta') this.stopReason = event.delta?.stop_reason;
    if (event.type === 'content_block_start' && event.content_block?.type === 'tool_use') {
      if (this.tools.size >= 16 || this.tools.has(event.index)) throw Error('tool-count-or-index');
      if (Buffer.byteLength(JSON.stringify(event.content_block.input ?? {})) > MAX_TOOL_BYTES) throw Error('probe-tool-size-limit');
      this.tools.set(event.index, { name: event.content_block.name, initial: event.content_block.input, startMs: ms, firstDeltaMs: null, lastDeltaMs: null, deltas: [], bytes: 0, deltaCount: 0, stopped: false, maxGapMs: 0, maxDeltaBytes: 0 });
    }
    if (event.type === 'content_block_delta') {
      if (event.delta?.type === 'text_delta' && event.delta.text) this.firstTextMs ??= ms;
      if (event.delta?.type === 'thinking_delta' && event.delta.thinking) this.firstThinkingMs ??= ms;
      const tool = this.tools.get(event.index);
      if (tool && event.delta?.type === 'input_json_delta') {
        const s = event.delta.partial_json;
        if (typeof s !== 'string') throw Error('invalid-tool-delta');
        tool.bytes += Buffer.byteLength(s);
        if (tool.bytes > MAX_TOOL_BYTES) throw Error('probe-tool-size-limit');
        tool.maxGapMs = Math.max(tool.maxGapMs, ms - (tool.lastDeltaMs ?? tool.startMs));
        tool.firstDeltaMs ??= ms;
        tool.lastDeltaMs = ms;
        tool.maxDeltaBytes = Math.max(tool.maxDeltaBytes, Buffer.byteLength(s));
        tool.deltaCount++;
        tool.deltas.push(s);
      }
    }
    if (event.type === 'content_block_stop' && this.tools.has(event.index)) {
      const tool = this.tools.get(event.index);
      tool.stopped = true; tool.stopMs = ms;
      tool.maxGapMs = Math.max(tool.maxGapMs, ms - (tool.lastDeltaMs ?? tool.startMs));
    }
  }
  summary(expectedLines, expectedContent) {
    const tools = [...this.tools.values()].map(tool => {
      let input, jsonValid = false;
      try { input = tool.deltas.length ? JSON.parse(tool.deltas.join('')) : tool.initial; jsonValid = !!input && typeof input === 'object' && !Array.isArray(input); } catch { /* no raw parser error */ }
      const content = typeof input?.new_string === 'string' ? input.new_string : '';
      const rows = content.replace(/\r\n/g, '\n').replace(/\n$/, '').split('\n');
      const contentValid = rows.length === expectedLines && rows.every((row, i) => /^[\x20-\x7e]{105,}$/.test(row) && row.startsWith(`${String(i + 1).padStart(4, '0')}: `));
      return { name: tool.name === 'Edit' ? 'Edit' : 'unexpected-tool', bytes: tool.bytes, deltaCount: tool.deltaCount,
        startMs: tool.startMs, firstDeltaMs: tool.firstDeltaMs, lastDeltaMs: tool.lastDeltaMs, stopMs: tool.stopMs,
        maxGapMs: tool.maxGapMs, maxDeltaBytes: tool.maxDeltaBytes, stopped: tool.stopped, jsonValid, contentBytes: Buffer.byteLength(content),
        lines: content ? rows.length : 0, contentValid, exactCopy: expectedContent === undefined ? undefined : content === expectedContent,
        targetValid: input?.file_path === '/workspace/probe.txt' && input?.old_string === '' };
    });
    const toolDelivered = this.terminal && this.errors === 0 && this.stopReason === 'tool_use' && tools.length === 1 && tools.every(t => t.name === 'Edit' && t.stopped && t.jsonValid && t.targetValid);
    return { events: this.events, heartbeats: this.heartbeats, firstTextMs: this.firstTextMs, firstThinkingMs: this.firstThinkingMs,
      terminal: this.terminal, errors: this.errors, stopReason: ['tool_use', 'end_turn', 'max_tokens'].includes(this.stopReason) ? this.stopReason : 'other', tools,
      toolDelivered, valid: toolDelivered && tools.every(t => t.contentValid && t.exactCopy !== false) };
  }
}

async function runProbe({ baseURL, key, lines = 42, copy = false, thinking = false, model, timeoutMs = 330000, fetchImpl = fetch }) {
  const url = new URL(baseURL);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw Error('invalid-base-url');
  if (!Number.isFinite(timeoutMs) || timeoutMs < 1000 || timeoutMs > 900000) throw Error('invalid-timeout');
  const request = createProbe({ lines, copy, thinking, model });
  const expectedContent = copy ? request.messages[0].content.split('<file_text>\n')[1].split('</file_text>')[0] : undefined;
  const started = performance.now(), evidence = new ProbeEvidence();
  let bytes = 0, httpStatus = 0, requestId, failure, pending = '';
  try {
    const res = await fetchImpl(baseURL.replace(/\/$/, '') + '/v1/messages', { method: 'POST', redirect: 'error',
      headers: { 'Content-Type': 'application/json', 'x-api-key': key, 'anthropic-version': '2023-06-01', 'User-Agent': 'claude-cli/2.1.284 (tool-field-probe)' },
      body: JSON.stringify(request), signal: AbortSignal.timeout(timeoutMs) });
    httpStatus = res.status;
    const id = res.headers.get('x-request-id') || '';
    if (/^[A-Za-z0-9_-]{1,128}$/.test(id)) requestId = id;
    if (!res.ok) { await res.body?.cancel(); failure = 'http'; }
    else {
      const decoder = new TextDecoder();
      let data = [];
      const line = row => {
        row = row.replace(/\r$/, '');
        if (row === '') {
          if (data.length) evidence.record(JSON.parse(data.join('\n')), Math.round(performance.now() - started));
          data = [];
        } else if (row.startsWith('data:')) data.push(row.slice(5).trimStart());
      };
      for await (const chunk of res.body) {
        bytes += chunk.length;
        if (bytes > MAX_WIRE_BYTES) throw Error('probe-wire-size-limit');
        pending += decoder.decode(chunk, { stream: true });
        let index;
        while ((index = pending.indexOf('\n')) >= 0) { line(pending.slice(0, index)); pending = pending.slice(index + 1); }
      }
      pending += decoder.decode();
      if (pending) line(pending);
      line('');
    }
  } catch (error) { failure = error.name === 'TimeoutError' ? 'deadline' : 'transport-or-protocol'; }
  const summary = evidence.summary(lines, expectedContent);
  return { lines, copy, thinking, httpStatus, requestId, totalMs: Math.round(performance.now() - started), wireBytes: bytes, failure,
    ...summary, valid: !failure && summary.valid };
}

if (require.main === module) {
  (async () => {
    const baseURL = process.env.KIRO_DEV_BASE_URL || 'http://127.0.0.1:8080';
    const host = new URL(baseURL).hostname;
    if (!process.argv.includes('--confirm-live')) throw Error('requires --confirm-live');
    if (!['127.0.0.1', 'localhost', '[::1]'].includes(host) && process.env.KIRO_DEV_ALLOW_REMOTE !== '1') throw Error('remote requires KIRO_DEV_ALLOW_REMOTE=1');
    const key = process.env.KIRO_DEV_API_KEY;
    if (!key || /[\r\n]/.test(key)) throw Error('API key required');
    const lines = Number(process.env.KIRO_TOOL_PROBE_LINES || '42');
    const result = await runProbe({ baseURL, key, lines, copy: process.env.KIRO_TOOL_PROBE_COPY === '1', thinking: process.env.KIRO_TOOL_PROBE_THINKING === '1',
      model: process.env.KIRO_DEV_MODEL || 'claude-sonnet-4-5', timeoutMs: Number(process.env.KIRO_TOOL_PROBE_TIMEOUT_MS || '330000') });
    if (process.env.KIRO_TOOL_PROBE_REPORT) fs.writeFileSync(process.env.KIRO_TOOL_PROBE_REPORT, JSON.stringify(result, null, 2) + '\n', { mode: 0o600, flag: 'wx' });
    console.log(JSON.stringify(result));
    process.exitCode = result.valid && !result.failure ? 0 : 1;
  })().catch(() => { console.error('Probe failed; check options, credentials and output path. No raw error was logged.'); process.exitCode = 1; });
}

module.exports = { createProbe, ProbeEvidence, runProbe };

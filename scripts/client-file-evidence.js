const fs = require('node:fs');
const path = require('node:path');

const MIN_FILE_BYTES = 45 * 1024;
const MAX_FILE_BYTES = 60 * 1024;
const MIN_CHUNK_BYTES = 4 * 1024;
const MAX_CHUNK_BYTES = 6 * 1024;

class EvidenceError extends Error {
  constructor(code, metrics) {
    super(code);
    this.metrics = metrics;
  }
}

function requireEvidence(condition, code, metrics) {
  if (!condition) throw new EvidenceError(code, metrics);
}

function sizeMetrics(actualBytes, minBytes, maxBytes) {
  return { actualBytes, minBytes, maxBytes, overBytes: Math.max(0, actualBytes - maxBytes), underBytes: Math.max(0, minBytes - actualBytes) };
}

function lines(text) {
  return text.replace(/\r\n/g, '\n').replace(/\n$/, '').split('\n');
}

function numberedLines(text, count, start = 1) {
  const rows = lines(text);
  requireEvidence(rows.length === count, 'line-count');
  rows.forEach((row, i) => {
    requireEvidence(/^[\x20-\x7e]{105,}$/.test(row), 'line-format');
    requireEvidence(row.startsWith(`${String(start + i).padStart(4, '0')}: `), 'line-number');
  });
  return rows;
}

function resultText(content) {
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) return '';
  return content.filter(block => block?.type === 'text' && typeof block.text === 'string')
    .map(block => block.text).join('\n');
}

// Supplemental to client-e2e-evidence.jq: replay only the two fixed file-only
// fixtures. This is not a general interpreter for model-generated commands.
function validateFileEvidence(records, content, { workspace, scenario, writer = 'Write' }) {
  requireEvidence(scenario === 'large' || scenario === 'chunked', 'scenario');
  requireEvidence(writer === 'Write' || writer === 'Edit', 'writer');
  const chunked = scenario === 'chunked';
  const target = path.resolve(workspace, chunked ? 'chunked-stream.txt' : 'large-stream.txt');
  const bytes = Buffer.byteLength(content);
  requireEvidence(bytes >= MIN_FILE_BYTES && bytes <= MAX_FILE_BYTES, 'file-size', sizeMetrics(bytes, MIN_FILE_BYTES, MAX_FILE_BYTES));
  const rows = numberedLines(content, 420);
  const calls = [], replies = new Map();
  let position = 0;
  for (const record of records) {
    if (record.type !== 'assistant' && record.type !== 'user') continue;
    if (!Array.isArray(record.message?.content)) continue;
    for (const block of record.message.content) {
      position++;
      if (record.type === 'assistant' && block.type === 'tool_use') calls.push({ ...block, position });
      if (record.type === 'user' && block.type === 'tool_result') {
        requireEvidence(!replies.has(block.tool_use_id), 'duplicate-result');
        replies.set(block.tool_use_id, { ...block, position });
      }
    }
  }
  const allowed = new Set(['Read', chunked ? 'Edit' : writer]);
  const ids = new Set();
  for (const call of calls) {
    requireEvidence(allowed.has(call.name), 'unexpected-tool');
    requireEvidence(typeof call.id === 'string' && call.id.length > 0 && !ids.has(call.id), 'tool-id');
    ids.add(call.id);
    requireEvidence(typeof call.input?.file_path === 'string' &&
      path.resolve(workspace, call.input.file_path) === target, 'wrong-target');
    const reply = replies.get(call.id);
    requireEvidence(reply && reply.position > call.position, 'tool-result-order');
  }
  requireEvidence(ids.size === replies.size, 'orphan-result');
  const succeeded = calls.filter(call => replies.get(call.id).is_error !== true);
  const mutations = succeeded.filter(call => call.name !== 'Read');
  requireEvidence(mutations.length === (chunked ? 10 : 1), 'mutation-count');
  let replay = chunked ? Array.from({ length: 10 }, (_, i) => `CHUNK_${String(i + 1).padStart(2, '0')}\n`).join('') : '';
  let lastMutationResult = -1;
  const placeholders = new Set();
  for (const call of mutations) {
    requireEvidence(call.position > lastMutationResult, 'mutation-order');
    const input = call.input;
    if (chunked) {
      requireEvidence(typeof input.old_string === 'string' && /^CHUNK_(0[1-9]|10)$/.test(input.old_string) &&
        !placeholders.has(input.old_string), 'chunk-placeholder');
      requireEvidence(typeof input.new_string === 'string', 'mutation-content');
      const size = Buffer.byteLength(input.new_string);
      requireEvidence(size >= MIN_CHUNK_BYTES && size <= MAX_CHUNK_BYTES, 'chunk-size', {
        ...sizeMetrics(size, MIN_CHUNK_BYTES, MAX_CHUNK_BYTES), chunkIndex: Number(input.old_string.slice(-2)),
      });
      numberedLines(input.new_string, 42, (Number(input.old_string.slice(-2)) - 1) * 42 + 1);
      placeholders.add(input.old_string);
      // Callback replacement keeps literal $&, $` and $' in generated text.
      replay = replay.replace(input.old_string, () => input.new_string);
    } else {
      requireEvidence(call.name === 'Write' || input.old_string === '', 'creation-input');
      replay = call.name === 'Write' ? input.content : input.new_string;
      requireEvidence(typeof replay === 'string', 'mutation-content');
    }
    lastMutationResult = replies.get(call.id).position;
  }
  requireEvidence(replay === content, 'disk-mismatch');

  // A final Read may be paginated and include Claude Code's cat -n prefixes.
  // Require all actual file lines, not just a "verified" tool response.
  const expected = new Set(rows), observed = new Set();
  const reads = succeeded.filter(call => call.name === 'Read' && call.position > lastMutationResult);
  for (const call of reads) {
    for (const row of lines(resultText(replies.get(call.id).content))) {
      const unnumbered = row.replace(/^\s*\d+(?:\t|\u2192| +\|) ?/, '');
      if (expected.has(row)) observed.add(row);
      else if (expected.has(unnumbered)) observed.add(unnumbered);
    }
  }
  requireEvidence(reads.length > 0, 'missing-final-read');
  requireEvidence(observed.size === expected.size, 'readback-content');
  return { bytes, lines: rows.length, mutations: mutations.length, verifiedLines: observed.size };
}

function readRegular(file, maxBytes, sizeCode, minBytes = 0) {
  const fd = fs.openSync(file, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
  try {
    const stat = fs.fstatSync(fd);
    requireEvidence(stat.isFile(), 'input-type');
    requireEvidence(stat.size <= maxBytes, sizeCode, sizeMetrics(stat.size, minBytes, maxBytes));
    const buffer = Buffer.alloc(maxBytes + 1);
    let size = 0, n;
    while (size <= maxBytes && (n = fs.readSync(fd, buffer, size, buffer.length - size, null)) > 0) size += n;
    requireEvidence(size <= maxBytes, sizeCode, sizeMetrics(size, minBytes, maxBytes));
    return buffer.subarray(0, size).toString('utf8');
  } finally { fs.closeSync(fd); }
}

function checkFileEvidence(trace, workspace, scenario, writer) {
  requireEvidence(scenario === 'large' || scenario === 'chunked', 'scenario');
  const records = readRegular(trace, 32 * 1024 * 1024, 'trace-size').split('\n').filter(row => row.trim()).map(row => JSON.parse(row));
  requireEvidence(records.every(record => record && typeof record === 'object' && !Array.isArray(record)), 'invalid-record');
  const filename = scenario === 'large' ? 'large-stream.txt' : 'chunked-stream.txt';
  return validateFileEvidence(records, readRegular(path.join(workspace, filename), MAX_FILE_BYTES, 'file-size', MIN_FILE_BYTES), { workspace, scenario, writer });
}

if (require.main === module) {
  try {
    const [, , trace, workspace, scenario, writer] = process.argv;
    process.stdout.write(JSON.stringify({ ok: true, ...checkFileEvidence(trace, workspace, scenario, writer) }) + '\n');
  } catch (error) {
    // Never include parser errors, file paths, tool arguments or file content.
    const reason = error instanceof EvidenceError ? error.message : 'invalid-input';
    const metrics = error instanceof EvidenceError ? error.metrics : undefined;
    process.stdout.write(JSON.stringify({ ok: false, reason, metrics }) + '\n');
    process.exitCode = 1;
  }
}

module.exports = { validateFileEvidence, checkFileEvidence };

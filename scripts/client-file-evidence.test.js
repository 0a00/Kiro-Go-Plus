const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { test } = require('node:test');
const { validateFileEvidence, checkFileEvidence } = require('./client-file-evidence');

function fixture(scenario = 'large', writer = 'Write') {
  const options = { scenario, writer, workspace: '/workspace' };
  const file = scenario === 'large' ? 'large-stream.txt' : 'chunked-stream.txt';
  const rows = Array.from({ length: 420 }, (_, i) => `${String(i + 1).padStart(4, '0')}: ${'Testing actual file evidence. '.repeat(4)}`);
  const content = rows.join('\n') + '\n';
  const records = [];
  const pair = (id, name, input, result = 'done') => {
    records.push({ type: 'assistant', message: { content: [{ type: 'tool_use', id, name, input: { file_path: file, ...input } }] } });
    records.push({ type: 'user', message: { content: [{ type: 'tool_result', tool_use_id: id, content: result }] } });
  };
  if (scenario === 'large') {
    pair('write', writer, writer === 'Write' ? { content } : { old_string: '', new_string: content });
  } else {
    for (let i = 0; i < 10; i++) pair(`edit-${i}`, 'Edit', {
      old_string: `CHUNK_${String(i + 1).padStart(2, '0')}`,
      new_string: rows.slice(i * 42, (i + 1) * 42).join('\n'),
    });
  }
  pair('read', 'Read', {}, content);
  return { records, content, options, pair };
}

function check(f) { return validateFileEvidence(f.records, f.content, f.options); }

test('large Write, Edit creation and ten chunked edits prove actual file content', () => {
  for (const f of [fixture(), fixture('large', 'Edit'), fixture('chunked')]) {
    assert.deepEqual(check(f), { bytes: Buffer.byteLength(f.content), lines: 420,
      mutations: f.options.scenario === 'large' ? 1 : 10, verifiedLines: 420 });
  }
});

test('final readback accepts paginated text blocks and Claude Code line prefixes', () => {
  const f = fixture();
  f.records.splice(-2);
  const rows = f.content.replace(/\n$/, '').split('\n');
  f.pair('r1', 'Read', { offset: 1, limit: 210 }, [{ type: 'text', text: rows.slice(0, 210).map((r, i) => `${i + 1}\u2192${r}`).join('\n') }]);
  f.pair('r2', 'Read', { offset: 211 }, rows.slice(210).map((r, i) => `   ${i + 211}\t${r}`).join('\n'));
  assert.equal(check(f).verifiedLines, 420);
});

test('file-only evidence rejects shell writes, wrong paths and marker-only completion', () => {
  for (const target of ['../large-stream.txt', '/outside/large-stream.txt', 'other.txt', '']) {
    const f = fixture();
    f.records[0].message.content[0].input.file_path = target;
    assert.throws(() => check(f), /wrong-target/);
  }
  const shell = fixture();
  shell.records[0].message.content[0].name = 'Bash';
  assert.throws(() => check(shell), /unexpected-tool/);
  const prose = fixture();
  prose.records = [{ type: 'result', subtype: 'success', result: 'LARGE_WRITE_PROGRESS_OK' }];
  assert.throws(() => check(prose), /mutation-count/);
});

test('large byte counts do not substitute for numbered lines or exact replay', () => {
  for (const [change, reason] of [
    [f => { f.content = 'x'.repeat(50000); }, /line-count/],
    [f => { f.content = f.content.replace('0002:', '0001:'); }, /line-number/],
    [f => { f.content = f.content.replace('Testing', 'Changed'); }, /disk-mismatch/],
    [f => { f.content = f.content.replace('Testing', '\u6d4b\u8bd5'); }, /line-format/],
    [f => { f.records[0].message.content[0].input.content = 'different'; }, /disk-mismatch/],
    [f => { f.content = f.content.slice(0, 40000); }, /file-size/],
  ]) {
    const f = fixture();
    change(f);
    assert.throws(() => check(f), reason);
  }
});

test('readback must succeed after the final mutation and cover every file line', () => {
  for (const [change, reason] of [
    [f => { f.records.splice(-2); }, /missing-final-read/],
    [f => { f.records.unshift(...f.records.splice(-2)); }, /missing-final-read/],
    [f => { f.records.at(-1).message.content[0].is_error = true; }, /missing-final-read/],
    [f => { f.records.at(-1).message.content[0].content = 'verified'; }, /readback-content/],
    [f => { f.records.at(-1).message.content[0].content = f.content.split('\n').slice(0, 400).join('\n'); }, /readback-content/],
  ]) {
    const f = fixture();
    change(f);
    assert.throws(() => check(f), reason);
  }
});

test('chunk mutations must replace unique placeholders in order with bounded content', () => {
  for (const [change, reason] of [
    [f => { f.records[2].message.content[0].input.old_string = 'CHUNK_01'; }, /chunk-placeholder/],
    [f => { f.records[0].message.content[0].input.new_string = 'x'.repeat(13000); }, /chunk-size/],
    [f => { f.records[0].message.content[0].input.new_string = f.records[0].message.content[0].input.new_string.replace('0001:', '0002:'); }, /line-number/],
    [f => { [f.records[1], f.records[2]] = [f.records[2], f.records[1]]; }, /mutation-order/],
  ]) {
    const f = fixture('chunked');
    change(f);
    assert.throws(() => check(f), reason);
  }
});

test('chunk replay preserves replacement metacharacters literally', () => {
  const f = fixture('chunked');
  f.content = f.content.replaceAll('Testing', () => "$& $` $'");
  for (const record of f.records) {
    const block = record.message.content[0];
    if (block.input?.new_string) block.input.new_string = block.input.new_string.replaceAll('Testing', () => "$& $` $'");
    if (block.tool_use_id === 'read') block.content = f.content;
  }
  assert.equal(check(f).mutations, 10);
});

test('failed tools cannot prove mutations and stream deltas are not duplicate calls', () => {
  const f = fixture();
  f.records.unshift({ type: 'stream_event', event: { type: 'content_block_start', content_block: f.records[0].message.content[0] } });
  assert.equal(check(f).mutations, 1);
  f.records[2].message.content[0].is_error = true;
  assert.throws(() => check(f), /mutation-count/);
});

test('file evidence checks reject malformed pairing', () => {
  for (const [change, reason] of [
    [f => { f.records.splice(1, 1); }, /tool-result-order/],
    [f => { f.records.push(f.records[1]); }, /duplicate-result/],
    [f => { f.records.unshift(f.records.splice(1, 1)[0]); }, /tool-result-order/],
    [f => { f.records.push({ type: 'user', message: { content: [{ type: 'tool_result', tool_use_id: 'orphan' }] } }); }, /orphan-result/],
  ]) {
    const f = fixture();
    change(f);
    assert.throws(() => check(f), reason);
  }
});

test('CLI bounds inputs, rejects symlinks, and never prints private parse errors', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'kiro-file-evidence.'));
  try {
    const f = fixture();
    const trace = path.join(dir, 'events.jsonl'), file = path.join(dir, 'large-stream.txt');
    fs.writeFileSync(trace, f.records.map(r => JSON.stringify(r)).join('\n'));
    fs.writeFileSync(file, f.content);
    assert.equal(checkFileEvidence(trace, dir, 'large', 'Write').lines, 420);
    fs.unlinkSync(file);
    fs.symlinkSync(trace, file);
    assert.throws(() => checkFileEvidence(trace, dir, 'large', 'Write'));
    fs.unlinkSync(file);
    fs.writeFileSync(file, f.content);
    fs.truncateSync(trace, 33 * 1024 * 1024);
    assert.throws(() => checkFileEvidence(trace, dir, 'large', 'Write'), /input-size-or-type/);
    fs.writeFileSync(trace, '{private_fixture_text: invalid}');
    const result = spawnSync(process.execPath, [path.join(__dirname, 'client-file-evidence.js'), trace, dir, 'large', 'Write'], { encoding: 'utf8' });
    assert.equal(result.status, 1);
    assert.deepEqual(JSON.parse(result.stdout), { ok: false, reason: 'invalid-input' });
    assert.equal(result.stderr, '');
    assert.doesNotMatch(result.stdout, /private_fixture_text/);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

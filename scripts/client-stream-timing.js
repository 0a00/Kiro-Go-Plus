const fs = require('node:fs');
const readline = require('node:readline');

class ToolTiming {
  constructor() { this.blocks = new Map(); this.tools = []; }
  record(row, ms) {
    const event = row.type === 'stream_event' ? row.event : null;
    if (!event) return;
    if (event.type === 'message_start') this.blocks.clear();
    if (event.type === 'content_block_start' && event.content_block?.type === 'tool_use') {
      const tool = { name: event.content_block.name, startMs: ms, firstDeltaMs: null,
        lastDeltaMs: null, stopMs: null, deltaCount: 0, bytes: 0, maxDeltaGapMs: 0 };
      this.blocks.set(event.index, tool);
      this.tools.push(tool);
    }
    const tool = this.blocks.get(event.index);
    if (!tool) return;
    if (event.type === 'content_block_delta' && event.delta?.type === 'input_json_delta') {
      tool.maxDeltaGapMs = Math.max(tool.maxDeltaGapMs, ms - (tool.lastDeltaMs ?? tool.startMs));
      tool.firstDeltaMs ??= ms;
      tool.lastDeltaMs = ms;
      tool.deltaCount++;
      tool.bytes += Buffer.byteLength(event.delta.partial_json || '');
    }
    if (event.type === 'content_block_stop') {
      tool.maxDeltaGapMs = Math.max(tool.maxDeltaGapMs, ms - (tool.lastDeltaMs ?? tool.startMs));
      tool.stopMs = ms;
      this.blocks.delete(event.index);
    }
  }
}

if (require.main === module) {
  const target = process.argv[2];
  const timing = new ToolTiming();
  const started = performance.now();
  let lastSaved = 0;
  const save = () => {
    fs.writeFileSync(target, JSON.stringify({ tools: timing.tools }, null, 2), { mode: 0o600 });
    lastSaved = performance.now();
  };
  // Observe Claude Code partial messages without retaining tool arguments.
  const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  lines.on('line', line => {
    try { timing.record(JSON.parse(line), Math.round(performance.now() - started)); } catch { /* CLI diagnostics are not JSON. */ }
    if (performance.now() - lastSaved >= 1000) save();
  });
  process.stdin.pipe(process.stdout);
  lines.on('close', save);
  process.on('SIGTERM', () => { save(); process.exit(143); });
}
module.exports = { ToolTiming };

const fs = require('node:fs');
const readline = require('node:readline');

class ToolTiming {
  constructor() { this.blocks = new Map(); this.arguments = new Map(); this.tools = []; this.messageStart = 0; this.stopReason = null; }
  interrupt(ms, all = false) {
    for (const tool of all ? this.tools.slice(this.messageStart) : this.blocks.values()) {
      tool.interrupted = true;
      tool.maxDeltaGapMs = Math.max(tool.maxDeltaGapMs, ms - (tool.lastDeltaMs ?? tool.startMs));
    }
    this.blocks.clear(); this.arguments.clear();
  }
  record(row, ms) {
    if (row.type === 'result' && (row.is_error || row.subtype !== 'success')) this.interrupt(ms, true);
    const event = row.type === 'stream_event' ? row.event : null;
    if (!event) return;
    if (event.type === 'error') { this.interrupt(ms, true); return; }
    if (event.type === 'message_start') { this.interrupt(ms); this.messageStart = this.tools.length; this.stopReason = null; }
    if (event.type === 'message_delta') this.stopReason = event.delta?.stop_reason ?? this.stopReason;
    if (event.type === 'message_stop' && this.stopReason == null) this.interrupt(ms, true);
    if (event.type === 'content_block_start' && event.content_block?.type === 'tool_use') {
      const tool = { name: event.content_block.name, startMs: ms, firstDeltaMs: null,
        lastDeltaMs: null, stopMs: null, deltaCount: 0, bytes: 0, maxDeltaGapMs: 0,
        jsonValid: null, argumentState: 'absent', interrupted: false };
      this.blocks.set(event.index, tool);
      this.tools.push(tool);
    }
    const tool = this.blocks.get(event.index);
    if (!tool) return;
    if (event.type === 'content_block_delta' && event.delta?.type === 'input_json_delta') {
      if (typeof event.delta.partial_json !== 'string') {
        tool.argumentState = 'invalid-type'; tool.jsonValid = false;
        this.arguments.delete(event.index); return;
      }
      tool.maxDeltaGapMs = Math.max(tool.maxDeltaGapMs, ms - (tool.lastDeltaMs ?? tool.startMs));
      tool.firstDeltaMs ??= ms;
      tool.lastDeltaMs = ms;
      tool.deltaCount++;
      tool.bytes += Buffer.byteLength(event.delta.partial_json || '');
      if (tool.argumentState === 'invalid-type') return;
      if (tool.bytes <= 8 * 1024 * 1024) {
        if (tool.bytes > 0) tool.argumentState = 'received';
        this.arguments.set(event.index, (this.arguments.get(event.index) || '') + (event.delta.partial_json || ''));
      } else { tool.argumentState = 'oversize'; tool.jsonValid = false; this.arguments.delete(event.index); }
    }
    if (event.type === 'content_block_stop') {
      tool.maxDeltaGapMs = Math.max(tool.maxDeltaGapMs, ms - (tool.lastDeltaMs ?? tool.startMs));
      tool.stopMs = ms;
      if (tool.argumentState === 'received') {
        try {
          const value = JSON.parse(this.arguments.get(event.index));
          tool.jsonValid = value !== null && !Array.isArray(value) && typeof value === 'object';
        } catch { tool.jsonValid = false; }
        tool.argumentState = tool.jsonValid ? 'valid' : 'invalid';
      }
      this.arguments.delete(event.index);
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
  lines.on('close', () => { timing.interrupt(Math.round(performance.now() - started)); save(); });
  process.on('SIGTERM', () => { timing.interrupt(Math.round(performance.now() - started)); save(); process.exit(143); });
}
module.exports = { ToolTiming };

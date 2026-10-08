// Optional, dedicated-key audit: retain IDs/statuses, never prompts or diagnostics.
const fs = require('node:fs');

function sanitizeSnapshot(value) {
  if (!value || !Array.isArray(value.logs) || value.logs.length > 500) throw Error('invalid log snapshot');
  const rows = value.logs.map(row => {
    if (!row || typeof row.requestId !== 'string' || !/^[A-Za-z0-9_.:-]{1,128}$/.test(row.requestId) ||
        !Number.isInteger(row.statusCode) || typeof row.status !== 'string') throw Error('invalid log row');
    return { id: row.requestId, code: row.statusCode, status: ['success', 'failed', 'rejected', 'canceled', 'delivery_failed'].includes(row.status) ? row.status : 'other' };
  });
  return { ok: true, saturated: rows.length === 500, rows };
}

function compareSnapshots(before, after) {
  if (!before?.ok || !after?.ok) return { warning: true, unavailable: true, unexpected: 0, incomplete: true };
  const ids = new Set(before.rows.map(row => row.id));
  const fresh = after.rows.filter(row => !ids.has(row.id));
  const incomplete = fresh.length === 0 || (after.saturated && !after.rows.some(row => ids.has(row.id))) ||
    (before.rows.length > 0 && !after.rows.some(row => ids.has(row.id)));
  const unexpected = fresh.filter(row => row.code !== 499 && (row.code < 200 || row.code >= 300 || row.status !== 'success')).length;
  return { warning: incomplete || unexpected > 0, unavailable: false, observed: fresh.length, unexpected, incomplete };
}

async function snapshot(env) {
  const base = new URL(env.KIRO_DEV_BASE_URL || 'http://127.0.0.1:8080');
  const local = ['127.0.0.1', 'localhost', '[::1]'].includes(base.hostname);
  if (!['http:', 'https:'].includes(base.protocol) || base.username || base.password || base.search || base.hash ||
      (!local && env.KIRO_DEV_ALLOW_REMOTE !== '1') || !env.KIRO_DEV_API_KEY) throw Error('invalid audit target');
  const url = new URL(base.toString().replace(/\/$/, '') + '/api/logs?limit=500');
  const r = await fetch(url, { headers: { Authorization: 'Bearer ' + env.KIRO_DEV_API_KEY },
    signal: AbortSignal.timeout(10000), redirect: 'error' });
  if (!r.ok) throw Error('audit unavailable');
  const chunks = []; let size = 0;
  for await (const chunk of r.body) {
    size += chunk.length;
    if (size > 2 * 1024 * 1024) throw Error('audit response too large');
    chunks.push(chunk);
  }
  return sanitizeSnapshot(JSON.parse(Buffer.concat(chunks).toString('utf8')));
}

if (require.main === module) {
  (async () => {
    if (process.argv[2] === 'snapshot') return snapshot(process.env);
    if (process.argv[2] === 'compare') {
      const read = name => {
        if (fs.statSync(name).size > 256 * 1024) throw Error('snapshot too large');
        return JSON.parse(fs.readFileSync(name, 'utf8'));
      };
      return compareSnapshots(read(process.argv[3]), read(process.argv[4]));
    }
    throw Error('invalid mode');
  })().then(result => process.stdout.write(JSON.stringify(result) + '\n')).catch(() => {
    process.stdout.write(JSON.stringify({ ok: false, warning: true, unavailable: true, incomplete: true }) + '\n');
  });
}

module.exports = { sanitizeSnapshot, compareSnapshots, snapshot };

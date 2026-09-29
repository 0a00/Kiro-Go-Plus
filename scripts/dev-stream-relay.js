// Loopback-only experiment: feed it an isolated Kiro-Go instance in live mode.
// /live, /balanced and /early-start change tool delivery, not prompts or models.
const http = require('node:http');
const { once } = require('node:events');
const MAX_BYTES = 8 * 1024 * 1024;

class ToolDelivery {
  constructor(mode) {
    if (!['live', 'balanced', 'early-start'].includes(mode)) throw Error('Invalid mode');
    this.mode = mode;
    this.tools = new Map();
    this.bytes = 0;
  }
  accept(packet) {
    const data = packet.split('\n').filter(x => x.startsWith('data:')).map(x => x.slice(5).trimStart()).join('\n');
    if (!data || data === '[DONE]') return [packet];
    const event = JSON.parse(data);
    if (event.type === 'error') { this.tools.clear(); this.bytes = 0; return [packet]; }
    if (event.type === 'message_start' && this.tools.size) throw Error('Unfinished tool before new message');
    if (event.type === 'content_block_start' && event.content_block?.type === 'tool_use') {
      if (this.tools.has(event.index)) throw Error('Duplicate tool start');
      this.tools.set(event.index, { start: packet, chunks: [], bytes: 0 });
      return this.mode === 'balanced' ? [] : [packet];
    }
    const tool = this.tools.get(event.index);
    if (event.type === 'content_block_delta' && event.delta?.type === 'input_json_delta') {
      if (!tool) throw Error('Tool delta without start');
      const input = event.delta.partial_json || '';
      const size = Buffer.byteLength(input);
      this.bytes += size;
      tool.bytes += size;
      if (this.bytes > MAX_BYTES) throw Error('Tool buffer limit exceeded');
      tool.chunks.push(input);
      return this.mode === 'live' ? [packet] : [];
    }
    if (event.type === 'content_block_stop' && tool) {
      const input = tool.chunks.join('') || '{}';
      const value = JSON.parse(input);
      if (!value || Array.isArray(value) || typeof value !== 'object') throw Error('Invalid tool JSON object');
      this.tools.delete(event.index);
      this.bytes -= tool.bytes;
      if (this.mode === 'live') return [packet];
      const delta = 'event: content_block_delta\ndata: ' + JSON.stringify({type:'content_block_delta',index:event.index,delta:{type:'input_json_delta',partial_json:input}});
      return [...(this.mode === 'balanced' ? [tool.start] : []), delta, packet];
    }
    if (event.type === 'message_stop' && this.tools.size) throw Error('Unfinished tool at message stop');
    return [packet];
  }
  finish() { if (this.tools.size) throw Error('Upstream ended with an unfinished tool'); }
}

function loopbackURL(value) {
  const url = new URL(value);
  if (url.protocol !== 'http:' || !['127.0.0.1', '[::1]'].includes(url.hostname) || url.username || url.password || url.pathname !== '/' || url.search || url.hash) {
    throw Error('Only an explicit loopback HTTP origin is allowed');
  }
  return url;
}

function createRelay(upstream) {
  const origin = loopbackURL(upstream);
  return http.createServer(async (req, res) => {
    let streaming = false;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 10 * 60 * 1000);
    req.on('aborted', () => controller.abort());
    res.on('close', () => controller.abort());
    const write = async text => {
      if (res.destroyed) throw Error('Client disconnected');
      if (!res.write(text)) await once(res, 'drain', {signal: controller.signal});
    };
    try {
      const incoming = new URL(req.url, 'http://127.0.0.1');
      const match = incoming.pathname.match(/^\/(live|balanced|early-start)(\/v1\/(?:messages(?:\/count_tokens)?|models)|\/health)$/);
      if (!match || !['GET','POST'].includes(req.method)) { res.writeHead(404); res.end(); return; }
      const chunks = [];
      let size = 0;
      for await (const chunk of req) {
        size += chunk.length;
        if (size > MAX_BYTES) { res.writeHead(413); res.end(); return; }
        chunks.push(chunk);
      }
      const headers = {};
      for (const name of ['authorization','x-api-key','content-type','anthropic-version','anthropic-beta','user-agent']) {
        if (typeof req.headers[name] === 'string') headers[name] = req.headers[name];
      }
      const response = await fetch(new URL(match[2] + incoming.search, origin), {method:req.method, headers,
        body:req.method === 'POST' ? Buffer.concat(chunks) : undefined, signal:controller.signal, redirect:'error'});
      const contentType = response.headers.get('content-type') || 'application/json';
      streaming = contentType.includes('text/event-stream');
      res.writeHead(response.status, {'Content-Type':contentType, 'Cache-Control':'no-cache', 'X-Accel-Buffering':'no',
        ...(response.headers.get('x-request-id') ? {'X-Request-Id':response.headers.get('x-request-id')} : {})});
      res.flushHeaders();
      if (!contentType.includes('text/event-stream')) {
        let total = 0;
        for await (const chunk of response.body) {
          total += chunk.length;
          if (total > MAX_BYTES) throw Error('Response limit exceeded');
          await write(chunk);
        }
      } else {
        const delivery = new ToolDelivery(match[1]);
        const decoder = new TextDecoder();
        let pending = '';
        for await (const chunk of response.body) {
          pending += decoder.decode(chunk, {stream:true});
          pending = pending.replace(/\r\n/g, '\n');
          let boundary;
          while ((boundary = pending.indexOf('\n\n')) >= 0) {
            const packet = pending.slice(0, boundary);
            if (Buffer.byteLength(packet) > MAX_BYTES) throw Error('SSE packet limit exceeded');
            pending = pending.slice(boundary + 2);
            for (const output of delivery.accept(packet)) await write(output + '\n\n');
          }
          if (Buffer.byteLength(pending) > MAX_BYTES) throw Error('SSE packet limit exceeded');
        }
        if (pending.trim() || decoder.decode()) throw Error('Incomplete SSE event');
        delivery.finish();
      }
      res.end();
    } catch {
      controller.abort();
      if (!res.destroyed) {
        if (!res.headersSent) { res.writeHead(502); res.end('Local streaming experiment failed'); }
        else if (streaming) {
          res.end('event: error\ndata: {"type":"error","error":{"type":"api_error","message":"Local streaming experiment failed"}}\n\n');
        } else res.destroy();
      }
    } finally { clearTimeout(timer); }
  });
}

if (require.main === module) {
  const upstream = process.env.KIRO_RELAY_UPSTREAM || 'http://127.0.0.1:18089';
  const port = Number(process.env.KIRO_RELAY_PORT || 18090);
  if (!Number.isInteger(port) || port < 1024 || port > 65535) throw Error('Invalid loopback relay port');
  const server = createRelay(upstream);
  server.listen(port, '127.0.0.1', () => console.log(`Local experiment listening on 127.0.0.1:${port}`));
  process.on('SIGTERM', () => { server.close(); server.closeAllConnections(); });
}
module.exports = { ToolDelivery, loopbackURL, createRelay };

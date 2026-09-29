const assert = require('node:assert/strict');
const { test } = require('node:test');
const http = require('node:http');
const { once } = require('node:events');
const { ToolDelivery, loopbackURL, createRelay } = require('./dev-stream-relay');
const packet = e => `event: ${e.type}\ndata: ${JSON.stringify(e)}`;
const start = i => packet({type:'content_block_start',index:i,content_block:{type:'tool_use',id:`t${i}`,name:'Edit',input:{}}});
const delta = (i,s) => packet({type:'content_block_delta',index:i,delta:{type:'input_json_delta',partial_json:s}});
const stop = i => packet({type:'content_block_stop',index:i});

test('three strategies have distinct early start/delta behavior and validate completion', () => {
  for (const mode of ['live','balanced','early-start']) {
    const d = new ToolDelivery(mode);
    assert.equal(d.accept(start(0)).length, mode === 'balanced' ? 0 : 1);
    assert.equal(d.accept(delta(0,'{"new_string":"abc')).length, mode === 'live' ? 1 : 0);
    const ping='event: ping\ndata: {"type":"ping"}';
    assert.deepEqual(d.accept(ping),[ping]);
    d.accept(delta(0,'"}'));
    assert.equal(d.accept(stop(0)).length,mode === 'live' ? 1 : mode === 'balanced' ? 3 : 2);
    d.finish();
  }
});
test('malformed and unfinished tools never produce a completion', () => {
  for(const mode of ['live','balanced','early-start']) {
    const d=new ToolDelivery(mode);d.accept(start(0));d.accept(delta(0,'{"bad":'));
    assert.throws(()=>d.accept(stop(0)));
    assert.throws(()=>d.finish());
    assert.equal(d.accept(packet({type:'error',error:{type:'api_error'}})).length,1);
    d.finish();
  }
});
test('interleaved tools and text preserve indexes without mixing arguments', () => {
  const d=new ToolDelivery('early-start');
  d.accept(start(0));d.accept(start(1));d.accept(delta(0,'{"x":'));d.accept(delta(1,'{"y":2}'));
  assert.match(d.accept(stop(1))[0],/\\"y\\":2/);
  d.accept(delta(0,'1}'));assert.match(d.accept(stop(0))[0],/\\"x\\":1/);
  d.finish();
});
test('loopback-only relay rejects credentials, nonlocal targets and oversized buffers', () => {
  for(const s of ['https://example.com','http://localhost','http://127.0.0.1.evil','http://user:pass@127.0.0.1','http://127.0.0.1/private'])assert.throws(()=>loopbackURL(s));
  assert.equal(loopbackURL('http://127.0.0.1:18089').port,'18089');
  const d=new ToolDelivery('balanced');d.accept(start(0));assert.throws(()=>d.accept(delta(0,'a'.repeat(8*1024*1024+1))));
});

test('relay forwards Claude beta query/auth and emits an error for a truncated tool', async t => {
  const upstream=http.createServer((req,res)=>{
    assert.equal(req.url,'/v1/messages?beta=true');
    assert.equal(req.headers['x-api-key'],'fixture-key');
    res.writeHead(200,{'Content-Type':'text/event-stream'});
    res.end(start(0)+'\n\n'+delta(0,'{"x":')+'\n\n');
  });
  upstream.listen(0,'127.0.0.1');await once(upstream,'listening');
  const relay=createRelay(`http://127.0.0.1:${upstream.address().port}`);
  relay.listen(0,'127.0.0.1');await once(relay,'listening');
  t.after(()=>{relay.close();relay.closeAllConnections();upstream.close();upstream.closeAllConnections();});
  const r=await fetch(`http://127.0.0.1:${relay.address().port}/early-start/v1/messages?beta=true`,{method:'POST',headers:{'x-api-key':'fixture-key'},body:'{}'});
  const body=await r.text();assert.equal(r.status,200);
  assert.match(body,/content_block_start/);assert.match(body,/event: error/);
  assert.doesNotMatch(body,/content_block_stop|partial_json|message_stop/);
});

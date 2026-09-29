const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { test } = require('node:test');
const { ToolTiming } = require('./client-stream-timing');

test('large-tool timing separates early streaming from completed or bursty output', () => {
  const t = new ToolTiming();
  const event = (e, ms) => t.record({type: 'stream_event', event: e}, ms);
  event({type:'content_block_start',index:0,content_block:{type:'tool_use',name:'Write'}}, 100);
  event({type:'content_block_delta',index:0,delta:{type:'input_json_delta',partial_json:'{"content":"'}}, 150);
  event({type:'content_block_delta',index:0,delta:{type:'input_json_delta',partial_json:'data"}'}}, 5000);
  event({type:'content_block_stop',index:0}, 5100);
  event({type:'message_start'}, 6000);
  event({type:'content_block_start',index:0,content_block:{type:'tool_use',name:'Read'}}, 6100);
  assert.equal(t.tools[0].firstDeltaMs, 150);
  assert.equal(t.tools[0].stopMs, 5100);
  assert.equal(t.tools[0].deltaCount, 2);
  assert.equal(t.tools[0].maxDeltaGapMs, 4850);
  assert.equal(t.tools[0].jsonValid, true);
  assert.equal(t.tools[1].stopMs, null);
  assert.equal(JSON.stringify(t.tools).includes('content'), false);
});

test('client-generated stop on incomplete arguments does not prove a valid tool', () => {
  const t=new ToolTiming();
  t.record({type:'stream_event',event:{type:'content_block_start',index:0,content_block:{type:'tool_use',name:'Edit'}}},0);
  t.record({type:'stream_event',event:{type:'content_block_delta',index:0,delta:{type:'input_json_delta',partial_json:'{"new_string":"partial'}}},10);
  t.record({type:'stream_event',event:{type:'content_block_stop',index:0}},20);
  assert.equal(t.tools[0].jsonValid,false);
  assert.equal(t.arguments.size,0);
});

const evidencePath = path.join(__dirname, 'client-e2e-evidence.jq');
const init = (tools = ['Read', 'Edit']) => ({ type: 'system', subtype: 'init', tools });
const done = (result = 'OK', subtype = 'success') => ({ type: 'result', subtype, is_error: subtype !== 'success', result });
const call = (id, name, file = '/workspace/workflow.sh') => ({ type: 'assistant', message: {
  content: [{ type: 'tool_use', id, name, input: { file_path: file } }],
} });
const reply = (id, content = 'completed', is_error = false) => ({ type: 'user', message: {
  content: [{ type: 'tool_result', tool_use_id: id, content, is_error }],
} });
function evidence(records) {
  const result = spawnSync('jq', ['-s', '-f', evidencePath], { input: records.map(r => JSON.stringify(r)).join('\n'), encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  return JSON.parse(result.stdout);
}

test('Edit-based creation proves the complete file workflow without Write', () => {
  const rows = [init(), call('create', 'Edit'), reply('create'), call('r1', 'Read'), reply('r1', '1\tFILE_WRITE_OK'),
    call('edit', 'Edit'), reply('edit'), call('r2', 'Read'), reply('r2', '1\tFILE_EDIT_OK'), done('FILE_TOOLS_OK')];
  const result = evidence(rows);
  assert.equal(result.fileRoundtrip, true);
  assert.equal(result.paired, true);
  assert.equal(result.terminalSuccess, true);
  assert.equal(evidence([init(), done('FILE_WRITE_OK FILE_EDIT_OK FILE_TOOLS_OK')]).fileRoundtrip, false);
  const wrongFile = structuredClone(rows);
  wrongFile[7].message.content[0].input.file_path = '/workspace/other.txt';
  assert.equal(evidence(wrongFile).fileRoundtrip, false);
});

test('only a later successful retry on the same file counts as recovered', () => {
  const rows = [init(), call('bad', 'Edit'), reply('bad', 'Read first', true),
    call('read', 'Read'), reply('read'), call('good', 'Edit'), reply('good'), done()];
  assert.equal(evidence(rows).recoveredErrors, 1);
  assert.equal(evidence(rows).unrecoveredErrors, 0);
  const unrelated = structuredClone(rows);
  unrelated[5].message.content[0].input.file_path = '/workspace/other.sh';
  assert.equal(evidence(unrelated).unrecoveredErrors, 1);
  assert.equal(evidence([init(), call('good', 'Edit'), reply('good'), call('bad', 'Edit'), reply('bad', 'failed', true), done()]).unrecoveredErrors, 1);
  const bashError = call('bad', 'Bash');
  bashError.message.content[0].input = { command: 'missing-command' };
  const unrelatedCommand = call('good', 'Bash');
  unrelatedCommand.message.content[0].input = { command: 'pwd' };
  assert.equal(evidence([bashError, reply('bad', 'failed', true), unrelatedCommand, reply('good'), done()]).unrecoveredErrors, 1);
});

test('tool pairing rejects missing, duplicate, orphan and early results', () => {
  for (const records of [
    [call('a', 'Edit')],
    [call('a', 'Edit'), reply('other')],
    [call('a', 'Edit'), call('a', 'Edit'), reply('a'), reply('a')],
    [reply('a'), call('a', 'Edit')],
  ]) assert.equal(evidence(records).paired, false);
  assert.equal(evidence([call('a', 'Edit'), reply('a'), { type: 'stream_event', event: { type: 'content_block_start' } }]).paired, true);
});

test('search needs a real successful tool result with a source, not prose or a marker', () => {
  const rows = [init(['WebSearch']), call('s', 'WebSearch'), reply('s', 'Links: https://example.invalid/docs'), done('CLAUDE_WEB_SEARCH_OK')];
  assert.equal(evidence(rows).searched, true);
  assert.equal(evidence([init(), done('<search_web>https://example.invalid/docs</search_web> CLAUDE_WEB_SEARCH_OK')]).searched, false);
  rows[2] = reply('s', 'Search failed; see https://example.invalid/error', true);
  assert.equal(evidence(rows).searched, false);
  rows[2] = reply('s', 'No results');
  assert.equal(evidence(rows).searched, false);
});

test('budget failures and stream errors are not successful task completion', () => {
  assert.equal(evidence([done('OK', 'error_max_budget_usd')]).terminalSuccess, false);
  assert.equal(evidence([init(), { type: 'stream_event', event: { type: 'error' } }, done()]).protocolError, true);
});

// The fake CLI never contacts the network. It exercises launcher options,
// scenario exit codes, real file assertions and skip policy through the shell.
const fakeCLI = `#!/usr/bin/env node
const fs = require('node:fs'), path = require('node:path');
const args = process.argv.slice(2), mode = process.env.KIRO_E2E_FIXTURE;
const emit = r => process.stdout.write(JSON.stringify(r)+'\\n');
const call = (id,name,file,input={}) => emit({type:'assistant',message:{content:[{type:'tool_use',id,name,input:{file_path:file,...input}}]}});
const reply = (id,content) => emit({type:'user',message:{content:[{type:'tool_result',tool_use_id:id,content}]}});
const numbered = (start,count) => Array.from({length:count},(_,i)=>String(start+i).padStart(4,'0')+': '+'Test assertions check file contents and actual tool results before declaring the workflow complete.'.padEnd(105,'.')).join('\\n');
if (process.env.ANTHROPIC_AUTH_TOKEN) process.exit(43);
const last = args.at(-1);
if (args.at(-2)!=='--') process.exit(44);
if (last.includes('CLIENT_CAPABILITY_PROBE_OK')) {
  if (!args.includes('--restricted')) process.exit(48);
  emit({type:'system',subtype:'init',tools:mode==='capability-missing'?['Read']:['Read','Edit']});
  emit({type:'result',subtype:mode==='capability-auth'?'error_during_execution':'success',is_error:mode==='capability-auth',result:'CLIENT_CAPABILITY_PROBE_OK'});
  if(mode==='capability-auth') process.exit(1);
} else if (last.includes('large-stream.txt')) {
  if(mode==='capability-missing'||mode==='capability-auth') process.exit(49);
  if(!last.includes('one Edit call') || !args.includes('Read,Edit')) process.exit(50);
  emit({type:'system',subtype:'init',tools:mode==='capability-changed'?['Read']:['Read','Edit']});
  const file=path.join(process.cwd(),'large-stream.txt');
  const content=mode==='large-filler'?'x'.repeat(47040):numbered(1,420)+'\\n';
  const input={old_string:'',new_string:content}, raw=JSON.stringify({file_path:file,...input});
  if(mode!=='large-no-file') fs.writeFileSync(file,content);
  emit({type:'stream_event',event:{type:'content_block_start',index:0,content_block:{type:'tool_use',name:'Edit',id:'create',input:{}}}});
  emit({type:'stream_event',event:{type:'content_block_delta',index:0,delta:{type:'input_json_delta',partial_json:raw}}});
  emit({type:'stream_event',event:{type:'content_block_stop',index:0}});
  call('create','Edit',mode==='large-wrong-target'?file+'.other':file,input);reply('create','created');
  if(mode!=='large-no-read'){call('read','Read',file);reply('read',mode==='large-fake-read'?'verified':content);}
  emit({type:'result',subtype:'success',is_error:false,result:'LARGE_WRITE_PROGRESS_OK'});
} else if (last.includes('chunked-stream.txt')) {
  emit({type:'system',subtype:'init',tools:['Read','Edit']});
  const file=path.join(process.cwd(),'chunked-stream.txt');
  call('read-before','Read',file);reply('read-before','placeholders');
  const count=mode==='chunks-fewer'?9:10;
  let content=fs.readFileSync(file,'utf8');
  for(let i=0;i<count;i++){
    const input={old_string:'CHUNK_'+String(i+1).padStart(2,'0'),new_string:mode==='chunks-oversized'?'x'.repeat(13000):numbered(i*42+1,42)};
    const raw=JSON.stringify({file_path:file,...input});
    emit({type:'stream_event',event:{type:'content_block_start',index:i,content_block:{type:'tool_use',name:'Edit',id:'edit-'+i,input:{}}}});
    emit({type:'stream_event',event:{type:'content_block_delta',index:i,delta:{type:'input_json_delta',partial_json:raw}}});
    emit({type:'stream_event',event:{type:'content_block_stop',index:i}});
    call('edit-'+i,'Edit',file,input);reply('edit-'+i,'edited');
    content=content.replace(input.old_string,()=>input.new_string);
  }
  fs.writeFileSync(file,content+(mode==='chunks-placeholder'?'CHUNK_10':''));
  call('read-after','Read',file);reply('read-after',mode==='chunks-fake-read'?'verified':content);
  emit({type:'result',subtype:'success',is_error:false,result:'CHUNKED_EDIT_PROGRESS_OK'});
} else if (last.includes('claude-file-e2e.txt')) {
  if (!args.includes('--bare')) process.exit(45);
  emit({type:'system',subtype:'init',tools:['Read','Edit']});
  const file = path.join(process.cwd(),'claude-file-e2e.txt');
  fs.writeFileSync(file,mode==='file-wrong'?'WRONG':'FILE_EDIT_OK');
  call('create','Edit',file);reply('create','created');
  call('r1','Read',file);reply('r1','FILE_WRITE_OK');
  call('edit','Edit',file);reply('edit','edited');
  call('r2','Read',file);reply('r2','FILE_EDIT_OK');
  emit({type:'result',subtype:'success',is_error:false,result:'FILE_TOOLS_OK'});
} else if (last.includes('LONG_TOOL_STRESS_OK')) {
  emit({type:'system',subtype:'init',tools:['Read','Edit','Bash']});
  const file=path.join(process.cwd(),'first.txt');
  for(let i=0;i<(mode==='long-few-files'?1:12);i++) fs.writeFileSync(path.join(process.cwd(),'file-'+i+'.txt'),'checked');
  call('bad','Edit',file);
  emit({type:'user',message:{content:[{type:'tool_result',tool_use_id:'bad',content:'Read first',is_error:true}]}});
  call('read','Read',file);reply('read','readback');
  call('retry','Edit',mode==='long-unrecovered'?file+'.other':file);reply('retry','updated');
  for(let i=0;i<20;i++){call('read-'+i,'Read',file);reply('read-'+i,'verified');}
  if(mode==='long-protocol') emit({type:'stream_event',event:{type:'error'}});
  emit({type:'result',subtype:mode==='long-budget'?'error_max_budget_usd':'success',is_error:mode==='long-budget',result:'LONG_TOOL_STRESS_OK'});
} else if (last.includes('workflow.sh')) {
  const file=path.join(process.cwd(),'workflow.sh');
  emit({type:'system',subtype:'init',tools:['Read','Edit'],session_id:'fixture-session'});
  if (!args.includes('--resume')) {
    fs.writeFileSync(file,'#!/bin/sh\\necho MULTITURN_OK\\n');
    call('create','Edit',file);reply('create','created');
    emit({type:'result',subtype:'success',is_error:false,result:'MULTITURN_CREATED_OK',session_id:'fixture-session'});
  } else {
    call('bad','Edit',file);
    emit({type:'user',message:{content:[{type:'tool_result',tool_use_id:'bad',content:'Read first',is_error:true}]}});
    call('read','Read',file);reply('read','script');
    call('retry','Edit',mode==='multi-unresolved'?file+'.other':file);reply('retry','edited');
    if(mode!=='multi-unchanged')fs.appendFileSync(file,'# expanded\\n# more content\\n');
    emit({type:'result',subtype:mode==='multi-budget'?'error_max_budget_usd':'success',is_error:mode==='multi-budget',result:'MULTITURN_EDITED_OK'});
  }
} else if (last.includes('WebSearch')) {
  if (args.includes('--bare') || process.env.CLAUDE_CODE_SIMPLE || !args.includes('--strict-mcp-config') || args[args.indexOf('--setting-sources')+1] !== '') process.exit(46);
  emit({type:'system',subtype:'init',tools:mode==='search-missing'||mode==='search-auth'?[]:['WebSearch']});
  if(mode==='search-auth'){emit({type:'result',subtype:'error_during_execution',is_error:true});process.exit(1);}
  if(mode==='search-ok'){call('search','WebSearch');reply('search','Links: https://example.invalid/docs');}
  emit({type:'result',subtype:'success',is_error:false,result:'<search_web>fake</search_web> CLAUDE_WEB_SEARCH_OK'});
} else process.exit(47);
`;
function runFixture(mode, scenario, options = []) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'kiro-client-script-test.'));
  try {
    fs.writeFileSync(path.join(tmp, 'claude'), fakeCLI, { mode: 0o700 });
    const env = { ...process.env, PATH: `${tmp}:${process.env.PATH}`, KIRO_E2E_FIXTURE: mode,
      KIRO_DEV_API_KEY: 'test-key', KIRO_DEV_BASE_URL: 'http://127.0.0.1:1',
      ANTHROPIC_AUTH_TOKEN: 'inherited-test-token', CLAUDE_CODE_SIMPLE: '1', KIRO_DEV_CLIENT_FAIL_ON_WARNING: '0' };
    delete env.KIRO_DEV_CLIENT_ARTIFACT_DIR;
    return spawnSync('bash', [path.join(__dirname, 'client-e2e.sh'), '--scenarios', scenario, '--model', 'fixture-model', '--timeout', '5s', ...options],
      { env, encoding: 'utf8', timeout: 60000 });
  } finally { fs.rmSync(tmp, { recursive: true, force: true }); }
}
test('file scenario passes with Edit creation but fails if the disk content is wrong', () => {
  const ok = runFixture('file-ok', 'file-tools');
  assert.equal(ok.status, 0, ok.stdout + ok.stderr);
  assert.match(ok.stdout, /file-tools\s+PASS/);
  const bad = runFixture('file-wrong', 'file-tools');
  assert.equal(bad.status, 1, bad.stdout + bad.stderr);
});
test('native search launcher enables the real capability and rejects fake search output', () => {
  const ok = runFixture('search-ok', 'web-search', ['--require-web-search']);
  assert.equal(ok.status, 0, ok.stdout + ok.stderr);
  const fake = runFixture('search-fake', 'web-search');
  assert.equal(fake.status, 1, fake.stdout + fake.stderr);
  const missing = runFixture('search-missing', 'web-search');
  assert.equal(missing.status, 0, missing.stdout + missing.stderr);
  assert.match(missing.stdout, /web-search\s+SKIP/);
  const required = runFixture('search-missing', 'web-search', ['--require-web-search']);
  assert.equal(required.status, 1, required.stdout + required.stderr);
  const auth = runFixture('search-auth', 'web-search');
  assert.equal(auth.status, 1, auth.stdout + auth.stderr);
  assert.doesNotMatch(auth.stdout, /web-search\s+SKIP/);
});

test('resumed workflow requires disk changes, recovered errors and successful termination', () => {
  const recovered = runFixture('multi-recovered', 'workspace-multiturn');
  assert.equal(recovered.status, 0, recovered.stdout + recovered.stderr);
  assert.match(recovered.stdout, /workspace-multiturn\s+PASS/);
  assert.match(recovered.stdout, /1 recovered tool errors/);
  for (const mode of ['multi-unresolved', 'multi-unchanged', 'multi-budget']) {
    const failed = runFixture(mode, 'workspace-multiturn');
    assert.equal(failed.status, 1, failed.stdout + failed.stderr);
    assert.match(failed.stdout, /workspace-multiturn\s+FAIL/);
  }
});

test('large-file preflight skips unavailable tools but does not hide authentication failures', () => {
  const missing=runFixture('capability-missing','workspace-large-write-progress');
  assert.equal(missing.status,0,missing.stdout+missing.stderr);
  assert.match(missing.stdout,/SKIP/);
  const auth=runFixture('capability-auth','workspace-large-write-progress');
  assert.equal(auth.status,1,auth.stdout+auth.stderr);
  assert.doesNotMatch(auth.stdout,/SKIP/);
});

test('large file supports Edit creation, warns on buffered progress, and rejects missing evidence', () => {
  const ok=runFixture('large-edit','workspace-large-write-progress');
  assert.equal(ok.status,0,ok.stdout+ok.stderr);
  assert.match(ok.stdout,/WARN.*large Edit completed/);
  const strict=runFixture('large-edit','workspace-large-write-progress',['--fail-on-warning']);
  assert.equal(strict.status,1,strict.stdout+strict.stderr);
  for(const mode of ['large-no-file','capability-changed','large-filler','large-wrong-target','large-no-read','large-fake-read']) {
    const bad=runFixture(mode,'workspace-large-write-progress');
    assert.equal(bad.status,1,bad.stdout+bad.stderr);
  }
});

test('chunked workflow requires ten bounded mutations and no remaining placeholders', () => {
  const ok=runFixture('chunks-ok','workspace-chunked-edit-progress');
  assert.equal(ok.status,0,ok.stdout+ok.stderr);
  assert.match(ok.stdout,/PASS/);
  for(const mode of ['chunks-fewer','chunks-oversized','chunks-placeholder','chunks-fake-read']){
    const bad=runFixture(mode,'workspace-chunked-edit-progress');
    assert.equal(bad.status,1,bad.stdout+bad.stderr);
  }
});

test('long workflow accepts recovered errors but requires complete evidence and files', () => {
  const ok=runFixture('long-recovered','workspace-long-tools');
  assert.equal(ok.status,0,ok.stdout+ok.stderr);
  assert.match(ok.stdout,/PASS.*1 recovered tool errors/);
  const budget=runFixture('long-budget','workspace-long-tools');
  assert.equal(budget.status,0,budget.stdout+budget.stderr);
  assert.match(budget.stdout,/WARN/);
  for(const mode of ['long-unrecovered','long-protocol','long-few-files']){
    const bad=runFixture(mode,'workspace-long-tools');
    assert.equal(bad.status,1,bad.stdout+bad.stderr);
  }
});

// Run with PLAYWRIGHT_MODULE set to an installed playwright module path.
// Uses synthetic API fixtures and local embedded assets; no server or credentials.
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const fs = require('fs');
const path = require('path');
const assert = require('node:assert/strict');
const root = path.resolve(__dirname, '..');
const output = path.resolve(process.argv[2] || path.join(root, 'data/prompt-ui-check'));
fs.mkdirSync(output, {recursive:true});
const entries = [
  {time:'2026-10-19T10:02:00Z', request_id:'new', model:'fixture', status:403, fresh_state:'new', messages:[{role:'user',content:'继续'}]},
  {time:'2026-10-19T10:01:00Z', request_id:'continuation', model:'fixture', status:403, fresh_state:'no_new', messages:[]},
  {time:'2026-10-19T10:00:00Z', request_id:'first', model:'fixture', status:403, fresh_state:'unknown',fresh_reason:'window_start', messages:[{role:'user',content:'交付状态: 这是一条用户粘贴的报告'}]},
];
(async()=>{
  const browser = await chromium.launch({headless:true});
  try {
    const page = await browser.newPage({viewport:{width:1440,height:1050}});
    const errors=[];page.on('pageerror',e=>errors.push(e.message));
    await page.route('https://prompt-monitor.test/**', async route=>{
      const url = new URL(route.request().url());
      if (url.pathname==='/panel/') return route.fulfill({contentType:'text/html',body:fs.readFileSync(path.join(root,'internal/panel/index.html'),'utf8')});
      if (url.pathname==='/panel/app.js') return route.fulfill({contentType:'application/javascript',body:fs.readFileSync(path.join(root,'internal/panel/app.js'),'utf8')});
      let data={};
      if (url.pathname==='/panel/api/overview') data={version:'fixture',uptime_sec:120,auth_required:false,accounts:[],total:0,healthy:0};
      if (url.pathname==='/panel/api/keys') data={keys:[{id:'a',name:'合成会话 A',capture_prompts:true,prompt_count:3},{id:'b',name:'合成会话 B',capture_prompts:true,prompt_count:1}]};
      if (/\/keys\/[^/]+\/prompts$/.test(url.pathname)) {
        const key = url.pathname.split('/')[4];
        if (key==='b') data={entries:[{...entries[0],messages:[{role:'user',content:'B 的独立输入'}]}]};
        else if(url.searchParams.get('role')==='fresh') data={entries};
        else data={entries:[{...entries[0],messages:[{role:'user',content:'原始任务'},{role:'user',content:'继续'},{role:'user',content:'继续'},...(url.searchParams.get('role')==='real'?[]:[{role:'user',content:'Current runtime context: fixture'}]),...(!url.searchParams.has('role')?[{role:'assistant',content:null,tool_calls:[{id:'call_fixture',function:{name:'read_file',arguments:'{}'}}]}]:[])]}]};
      }
      return route.fulfill({contentType:'application/json',body:JSON.stringify(data)});
    });
    await page.goto('https://prompt-monitor.test/panel/#keys');
    await page.locator('#keysBody tr[data-id="a"] button[data-act="prompts"]').click();
    await page.locator('#keyPromptNote').filter({hasText:'1 条有新增输入'}).waitFor();
    assert.equal(await page.locator('#keyPromptList > div').count(),2);
    assert.match(await page.locator('#keyPromptList').innerText(),/新增无法判定/);
    assert.match(await page.locator('#keyPromptList').innerText(),/交付状态/);
    await fs.promises.writeFile(path.join(output,'fresh.aria.txt'),await page.locator('#keyPromptBox').ariaSnapshot());
    await page.screenshot({path:path.join(output,'fresh.png'),fullPage:true});
    await page.selectOption('#keyPromptScope','real');
    await page.locator('#keyPromptList pre').filter({hasText:'原始任务'}).waitFor();
    assert.equal(await page.locator('#keyPromptList pre').filter({hasText:/^继续$/}).count(),2);
    assert(!((await page.locator('#keyPromptList').innerText()).includes('Current runtime context')));
    await page.selectOption('#keyPromptScope','user');
    await page.locator('#keyPromptList pre').filter({hasText:'Current runtime context'}).waitFor();
    await page.selectOption('#keyPromptScope','all');
    await page.locator('#keyPromptList details').waitFor();
    await page.locator('#keyPromptList summary').click();
    assert.match(await page.locator('#keyPromptList details').innerText(),/tool_calls/);
    await page.screenshot({path:path.join(output,'all.png'),fullPage:true});
    await page.selectOption('#keyPromptScope','fresh');
    await page.locator('#keysBody tr[data-id="b"] button[data-act="prompts"]').click();
    await page.locator('#keyPromptList pre').filter({hasText:'B 的独立输入'}).waitFor();
    assert(!((await page.locator('#keyPromptList').innerText()).includes('交付状态')));
    await page.locator('#btnKeyPromptClose').click();
    assert(await page.locator('#keyPromptBox').isHidden());
    assert.deepEqual(errors,[]);
    fs.writeFileSync(path.join(output,'result.json'),JSON.stringify({ok:true,fixtures:true,checks:['fresh','unknown','repeated-input','real','user','all-tool-calls','switch-key','close'],page_errors:errors},null,2)+'\n');
    console.log('prompt viewer browser checks passed (synthetic fixtures)');
  } finally {await browser.close();}
})().catch(e=>{console.error(e);process.exitCode=1});

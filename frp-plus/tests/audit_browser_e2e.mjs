// Optional helper for TestAuditBrowserEndToEnd. No API or resource routes mocked.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import {pathToFileURL} from 'node:url';

let input = '';
for await (const part of process.stdin) input += part;
const fixture = JSON.parse(input); input = '';
const report = {ok:false,stage:'launch'};
let browser;
try {
  const {chromium} = await import(pathToFileURL(process.env.PLAYWRIGHT_MODULE).href);
  browser = await chromium.launch({headless:true,executablePath:process.env.BROWSER_EXECUTABLE});
  const context = await browser.newContext({viewport:{width:390,height:900},colorScheme:'dark',acceptDownloads:true});
  await context.addCookies([{...fixture.cookie,url:fixture.url,httpOnly:true,sameSite:'Strict'}]);
  const page = await context.newPage(); page.setDefaultTimeout(10000);
  let exports = 0, errors = 0;
  page.on('pageerror',()=>errors++);
  page.on('request',request=>{if(request.method()==='POST'&&new URL(request.url()).pathname.endsWith('/audit/export'))exports++;});
  report.stage='query';
  await page.goto(fixture.url+'/admin/#audit');
  await page.locator('.fa-audit-record').first().waitFor();
  assert.equal(await page.locator('.fa-audit-record').count(),50);assert.equal(exports,0);
  const first=await page.locator('.fa-audit-record').first().getAttribute('data-audit-i-d');
  report.stage='detail';
  await page.locator('.fa-audit-record').first().click();
  await page.locator('#audit-detail').getByText('primary',{exact:true}).waitFor();
  assert((await page.locator('#audit-detail').innerText()).includes('未知操作者'));
  assert((await page.locator('#audit-detail').innerText()).includes('观察记录者'));
  report.stage='pagination';
  await page.locator('#audit-next').click();
  await page.waitForFunction(()=>document.querySelectorAll('.fa-audit-record').length===3);
  assert(await page.locator('#audit-next').isDisabled());
  await page.locator('#audit-previous').click();
  assert.equal(await page.locator('.fa-audit-record').count(),50);
  assert.equal(await page.locator('.fa-audit-record').first().getAttribute('data-audit-i-d'),first);
  report.stage='export';
  const downloading=page.waitForEvent('download');await page.locator('#audit-export').click();const download=await downloading;
  const data=await fs.readFile(await download.path(),'utf8');const rows=data.trimEnd().split('\n').map(JSON.parse);
  assert.equal(download.suggestedFilename(),'frp-plus-config-audit.jsonl');assert.equal(exports,1);
  assert.equal(rows[0].kind,'audit_export');assert.equal(rows.length,fixture.expected_count+1);
  assert(rows.slice(1).every(row=>row.kind==='audit_entry'&&row.changes.every(change=>!('before'in change)&&!('after'in change))));
  report.stage='filter';
  await page.locator('#audit-filters summary').click();
  await page.locator('[name=actor_kind]').selectOption('unknown');
  await page.locator('[name=service_id]').fill('primary');await page.locator('#audit-search').click();
  await page.waitForFunction(()=>document.querySelectorAll('.fa-audit-record').length===1);
  assert(await page.locator('#audit-next').isDisabled());
  const overflow=await page.evaluate(()=>[...document.querySelectorAll('#audit-panel,#audit-panel *')].some(e=>e.getClientRects().length&&e.scrollWidth>e.clientWidth+2&&getComputedStyle(e).overflowX==='visible'));
  assert.equal(overflow,false);
  report.stage='logout';await page.locator('#logout').click();await page.locator('#login-panel').waitFor();
  assert.equal(await page.locator('#audit-list').innerText(),'');assert.equal(await page.locator('[name=service_id]').inputValue(),'');assert.equal(errors,0);
  report.ok=true;report.stage='complete';
} catch { process.exitCode=1; }
finally {if(browser)await browser.close();process.stdout.write(JSON.stringify(report));}

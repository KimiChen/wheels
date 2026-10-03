// Optional helper for TestConfigRestoreNativeEndToEnd. No routes are mocked.
import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';

let input = '';
for await (const part of process.stdin) input += part;
const fixture = JSON.parse(input);
input = '';
const report = {ok: false, stage: 'launch'};
let browser;
try {
  const {chromium} = await import(pathToFileURL(process.env.PLAYWRIGHT_MODULE).href);
  browser = await chromium.launch({headless: true, executablePath: process.env.BROWSER_EXECUTABLE});
  const context = await browser.newContext({viewport: {width: 390, height: 900}, colorScheme: 'dark'});
  await context.addCookies([{...fixture.cookie, url: fixture.url, httpOnly: true, sameSite: 'Strict'}]);
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  let acknowledgements = 0, mutations = 0, errors = 0;
  page.on('pageerror', () => errors++);
  page.on('request', request => {
    if (request.method() !== 'POST') return;
    const path = new URL(request.url()).pathname;
    if (path.endsWith('/restore/acknowledge')) acknowledgements++;
    if (path.includes('/configuration/operations')) mutations++;
  });
  report.stage = 'open';
  await page.goto(fixture.url + '/admin/#nodes');
  await page.locator('#node-list tr').first().locator('[data-node-action="settings"]').first().click();
  await page.locator('[data-editor-pane="configuration"]').click();
  const panel = page.locator('#node-configuration'), box = panel.locator('.fa-config-restoration');
  await panel.getByRole('heading', {name: '最近配置操作', exact: true}).waitFor();
  assert.equal(acknowledgements, 0);
  report.stage = 'inspect';
  // Inspection shares the production command budget with inventory requests.
  await page.waitForTimeout(2200);
  await box.getByRole('button', {name: '恢复核对', exact: true}).click();
  await box.getByRole('heading', {name: '本机已验证，等待管理员接管', exact: true}).waitFor();
  const acknowledge = box.getByRole('button', {name: '确认接管恢复状态', exact: true});
  assert(await acknowledge.isDisabled());
  assert(await panel.getByRole('button', {name: '新建 Proxy', exact: true}).isDisabled());
  assert.equal(acknowledgements, 0);
  report.stage = 'acknowledge';
  await box.locator('input[type="checkbox"]').check();
  await acknowledge.click();
  await box.getByRole('heading', {name: '接管已确认，等待离线确认', exact: true}).waitFor();
  assert((await box.innerText()).includes('restore-confirm'));
  assert(await panel.getByRole('button', {name: '新建 Proxy', exact: true}).isDisabled());
  assert.equal(acknowledgements, 1);
  assert.equal(mutations, 0);
  assert.equal(errors, 0);
  report.stage = 'complete';
  report.ok = true;
} catch {
  // Exception text may contain session values, private DOM or request data.
  process.exitCode = 1;
} finally {
  await browser?.close();
  process.stdout.write(JSON.stringify(report));
}

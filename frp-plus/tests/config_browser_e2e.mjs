// Run only through TestConfigNativeBrowserEndToEnd. No API routes are mocked.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import net from 'node:net';
import {pathToFileURL} from 'node:url';

let input = '';
for await (const part of process.stdin) input += part;
const fixture = JSON.parse(input);
input = '';
const report = {ok: false, stage: 'launch', completed: [], observations: []};
let browser;
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
async function eventually(check, timeout = 8000) {
  const until = Date.now() + timeout;
  while (Date.now() < until) { if (await check()) return; await delay(100); }
  throw new Error('bounded condition was not met');
}
function echo(port, expectReply) {
  return new Promise(resolve => {
    const socket = net.createConnection({host: '127.0.0.1', port});
    const payload = Buffer.from('frp-plus-browser-native-e2e\n');
    let received = Buffer.alloc(0), connected = false, done = false;
    const finish = result => { if (done) return; done = true; socket.destroy(); resolve(result); };
    socket.setTimeout(500);
    socket.on('connect', () => { connected = true; socket.write(payload); });
    socket.on('data', part => { received = Buffer.concat([received, part]); if (received.length >= payload.length) finish(expectReply && received.equals(payload)); });
    socket.on('timeout', () => finish(false));
    socket.on('error', () => finish(!expectReply && !connected));
    socket.on('end', () => finish(false));
  });
}
try {
  const {chromium} = await import(pathToFileURL(process.env.PLAYWRIGHT_MODULE).href);
  browser = await chromium.launch({headless: true, executablePath: process.env.BROWSER_EXECUTABLE});
  const context = await browser.newContext({viewport: {width: 390, height: 900}, colorScheme: 'dark'});
  await context.addCookies([{...fixture.cookie, url: fixture.url, httpOnly: true, sameSite: 'Strict'}]);
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', () => errors.push('page_error'));
  page.on('response', async response => {
    if (!new URL(response.url()).pathname.includes('/configuration')) return;
    try {
      const value = await response.json();
      const observation = [response.status(), value.code, value.operation?.state ?? value.inventory?.state]
        .filter(item => typeof item === 'number' || typeof item === 'string' && /^[a-z_]{1,48}$/.test(item));
      report.observations.push(observation.join(':'));
      report.observations = report.observations.slice(-16);
    } catch { /* Responses without safe JSON facts add no diagnostic values. */ }
  });
  const original = fs.readFileSync(fixture.store);
  const prepared = [], operations = [];
  const panel = page.locator('#node-configuration');
  const result = panel.locator('.fa-config-result');
  report.stage = 'open';
  await page.goto(fixture.url + '/admin/#nodes');
  await page.locator('#node-list tr').first().locator('[data-node-action="settings"]').first().click();
  await page.locator('[data-editor-pane="configuration"]').click();
  await panel.getByRole('button', {name: '新建 Proxy', exact: true}).waitFor();
  await eventually(async () => !(await panel.getByRole('button', {name: '新建 Proxy', exact: true}).isDisabled()));
  async function reload() {
    report.stage = 'reload_inventory';
    // The real control channel admits a bounded burst then one command/second.
    // A human reviews the previous result before starting another operation;
    // this fixture paces that transition instead of weakening production limits.
    await delay(2200);
    await panel.getByRole('button', {name: '重新读取配置', exact: true}).click();
    await eventually(async () => !(await panel.getByRole('button', {name: '新建 Proxy', exact: true}).isDisabled()));
  }
  async function form(kind, type, name, fields, secret = false) {
    await panel.getByRole('button', {name: `新建 ${kind}`, exact: true}).click();
    await panel.locator('[name="type"]').selectOption(type);
    await panel.locator('[name="name"]').fill(name);
    for (const [path, value] of Object.entries(fields)) await panel.locator(`[data-config-field="${path}"]`).fill(String(value));
    if (secret) {
      const group = panel.locator('[data-config-secret="secretKey"]');
      await group.locator('select').selectOption('replace');
      await group.locator('input').fill(fixture.secret);
    }
  }
  async function preview() {
    const response = page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/configuration/operations'));
    await panel.getByRole('button', {name: '校验并生成预览', exact: true}).click();
    const value = await (await response).json();
    assert(!JSON.stringify(value).includes(fixture.secret));
    return value;
  }
  report.stage = 'invalid_candidate';
  await form('Proxy', 'tcp', 'browser-invalid', {localPort: fixture.local_port, remotePort: fixture.remote_port, 'transport.bandwidthLimitMode': 'invalid'});
  const invalid = await preview();
  assert.equal(invalid.operation.state, 'rejected');
  assert.deepEqual(fs.readFileSync(fixture.store), original);
  await eventually(() => echo(fixture.remote_port, false));
  await reload();
  report.completed.push('native_validation_without_side_effects');
  async function create(kind, type, name, fields, secret = false) {
    report.stage = `prepare_${type}_${kind.toLowerCase()}`;
    const before = fs.readFileSync(fixture.store);
    await form(kind, type, name, fields, secret);
    const value = await preview();
    assert.equal(value.operation.state, 'prepared');
    assert(value.preview && value.agent);
    const apply = panel.getByRole('button', {name: '应用本次预览', exact: true});
    assert(await apply.isDisabled());
    assert.deepEqual(fs.readFileSync(fixture.store), before);
    assert(!(await panel.innerText()).includes(fixture.secret));
    assert(!(await page.evaluate(() => JSON.stringify({...localStorage, ...sessionStorage}))).includes(fixture.secret));
    for (const element of await panel.locator('input[type="password"]').all()) assert.equal(await element.inputValue(), '');
    prepared.push(before);
    operations.push(value.operation.operation_id);
    report.stage = `apply_${type}_${kind.toLowerCase()}`;
    await panel.locator('.fa-config-confirm input').check();
    await apply.click();
    await result.getByRole('heading', {name: '配置已确认', exact: true}).waitFor();
    assert((await result.innerText()).includes('业务'));
    await panel.getByRole('button', {name: '查询实际结果', exact: true}).click();
    await result.getByRole('heading', {name: '配置已确认', exact: true}).waitFor();
    report.completed.push(`${type}_${kind.toLowerCase()}_confirmed`);
  }
  await create('Proxy', 'tcp', 'browser-tcp', {localPort: fixture.local_port, remotePort: fixture.remote_port});
  await eventually(() => echo(fixture.remote_port, true));
  await reload();
  await create('Proxy', 'stcp', 'browser-private', {localPort: fixture.local_port}, true);
  await reload();
  await create('Visitor', 'stcp', 'browser-visitor', {serverName: 'browser-private', bindPort: fixture.visitor_port}, true);
  report.stage = 'business';
  await eventually(() => echo(fixture.remote_port, true));
  await eventually(() => echo(fixture.visitor_port, true));
  report.completed.push('tcp_and_stcp_visitor_payloads');
  for (let index = operations.length - 1; index >= 0; index--) {
    report.stage = `rollback_${index}`;
    // Read the real ordering, then use the displayed history button. Mutations
    // stay exclusively in the browser's normal form and confirmation controls.
    const historyResponse = await page.request.get(fixture.url + '/api/admin/v1/nodes/1/configuration/operations');
    assert.equal(historyResponse.status(), 200);
    const history = await historyResponse.json();
    const position = history.operations.findIndex(item => item.operation_id === operations[index]);
    assert(position >= 0);
    await panel.locator('.fa-config-history-item').nth(position).click();
    await result.getByRole('heading', {name: '配置已确认', exact: true}).waitFor();
    await panel.getByRole('button', {name: '准备回退', exact: true}).click();
    await panel.getByRole('button', {name: '确认恢复此操作前的配置', exact: true}).click();
    await result.getByRole('heading', {name: '已回退', exact: true}).waitFor();
    assert.deepEqual(fs.readFileSync(fixture.store), prepared[index]);
    await eventually(() => echo(fixture.visitor_port, false));
    if (index > 0) await eventually(() => echo(fixture.remote_port, true));
    report.completed.push(`rollback_${index}_exact_bytes`);
  }
  await eventually(() => echo(fixture.remote_port, false));
  assert.deepEqual(fs.readFileSync(fixture.store), original);
  if (fixture.migration_port) {
    await reload();
    report.stage = 'migrated_source_delete';
    const card = panel.locator('.fa-config-inventory .fa-config-object').filter({hasText: 'Proxy · migration-tcp'});
    assert((await card.innerText()).includes('Store'));
    await card.getByRole('button', {name: '删除', exact: true}).click();
    const value = await preview();
    assert.equal(value.operation.state, 'prepared');
    assert.deepEqual(fs.readFileSync(fixture.store), original);
    await panel.locator('.fa-config-confirm input').check();
    await panel.getByRole('button', {name: '应用本次预览', exact: true}).click();
    await result.getByRole('heading', {name: '配置已确认', exact: true}).waitFor();
    // If the original file definition remained, removing the Store object
    // would incorrectly revive that file proxy instead of closing the port.
    await eventually(() => echo(fixture.migration_port, false));
    report.stage = 'migrated_source_restore';
    await panel.getByRole('button', {name: '准备回退', exact: true}).click();
    await panel.getByRole('button', {name: '确认恢复此操作前的配置', exact: true}).click();
    await result.getByRole('heading', {name: '已回退', exact: true}).waitFor();
    assert.deepEqual(fs.readFileSync(fixture.store), original);
    await eventually(() => echo(fixture.migration_port, true));
    report.completed.push('migrated_store_delete_without_file_fallback');
  }
  assert.deepEqual(errors, []);
  report.stage = 'complete';
  report.ok = true;
} catch {
  // Keep diagnostics to fixed fixture stage names. Browser exceptions may carry
  // sensitive DOM, private paths or request data and are deliberately omitted.
  process.exitCode = 1;
} finally {
  await browser?.close();
  process.stdout.write(JSON.stringify(report));
}

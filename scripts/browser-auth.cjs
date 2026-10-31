// Real UI + authenticated development API; all credentials and records are fixtures.
const { chromium } = require('playwright-core');
const assert = require('node:assert/strict');

(async () => {
  const base = process.env.RECORDER_TEST_BASE || 'http://127.0.0.1:8787';
  const key = process.env.RECORDER_MANAGEMENT_KEY;
  if (!key) throw new Error('Set RECORDER_MANAGEMENT_KEY');
  const origin = new URL(base).origin;
  const entry = base + '/v0/resource/plugins/request-recorder/index.html';
  const savedKey = 'request-recorder:connection:v1';
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true, args: ['--no-sandbox'] });
  const errors = [];
  async function newPage(options = {}) {
    const context = await browser.newContext(options);
    await context.route('**/v8/management/config/access/api-keys', route => route.fulfill({ json: ['sk-recorder-integration-client'] }));
    const page = await context.newPage();
    page.on('pageerror', error => errors.push(error.message));
    return { context, page };
  }
  async function submit(page, remember, enteredKey = key) {
    if (!(await page.locator('#login').evaluate(dialog => dialog.open)))
      await page.locator('#login-open').click();
    await page.locator('#management-key').fill(enteredKey);
    await page.locator('#remember-key').setChecked(remember);
    await page.locator('#login-form button[type=submit]').click();
  }
  async function connected(page) {
    await page.locator('#rows tr').first().waitFor();
    await page.waitForFunction(() => document.querySelector('#connection').textContent === '● 已连接');
  }
  try {
    const { context, page } = await newPage();
    await page.goto(entry);
    await page.locator('#login-open').click();
    assert.equal(await page.locator('#remember-key').isChecked(), false);
    await submit(page, true, 'wrong-management-key');
    await page.locator('#login-error').getByText(/管理鉴权失败/).waitFor();
    assert.equal(await page.evaluate(name => localStorage.getItem(name), savedKey), null, 'failed login must not save its key');
    await submit(page, true);
    await connected(page);
    await page.locator('#login').waitFor({ state: 'hidden' });
    const stored = await page.evaluate(name => localStorage.getItem(name), savedKey);
    assert(stored.startsWith('enc::v2::'));
    assert(!stored.includes(key));
    // Decode independently using the exact host-only algorithm from upstream.
    const bytes = Buffer.from(stored.slice('enc::v2::'.length), 'base64');
    const secret = Buffer.from('cli-proxy-api-webui::secure-storage|' + new URL(base).host);
    for (let i = 0; i < bytes.length; i++) bytes[i] ^= secret[i % secret.length];
    assert.deepEqual(JSON.parse(bytes.toString()), { version: 1, apiBase: origin, managementKey: key });
    assert.equal(await page.evaluate(() => localStorage.getItem('cli-proxy-auth')), null, 'manual remember must not modify CPAMC');
    assert.equal(await page.locator('#management-key').inputValue(), '');
    await page.reload();
    await connected(page);
    assert.equal(await page.locator('#login').evaluate(dialog => dialog.open), false);
    const persistedState = await context.storageState();
    // A new browser context with a different UA can still restore current v2.
    const changed = await newPage({ storageState: persistedState, userAgent: 'recorder-updated-browser' });
    await changed.page.goto(entry);
    await connected(changed.page);
    await submit(changed.page, true, 'wrong-replacement-key');
    await changed.page.locator('#login-error').getByText(/管理鉴权失败/).waitFor();
    assert.equal(await changed.page.evaluate(name => localStorage.getItem(name), savedKey), stored, 'a failed replacement must preserve the previous saved key');
    await submit(changed.page, false);
    await connected(changed.page);
    assert.equal(await changed.page.evaluate(name => localStorage.getItem(name), savedKey), null, 'unchecking must remove the previous saved key');
    await changed.page.reload();
    assert.equal(await changed.page.locator('#rows tr').count(), 0);
    await changed.context.close();
    // Forgetting clears the plugin key and the view; no re-login after reload.
    const secondTab = await context.newPage();
    secondTab.on('pageerror', error => errors.push(error.message));
    await secondTab.goto(entry);
    await connected(secondTab);
    await page.locator('#login-open').click();
    assert.equal(await page.locator('#remember-key').isChecked(), true);
    await page.locator('#forget-key').click();
    assert.equal(await page.locator('#rows tr').count(), 0);
    await secondTab.locator('#rows tr').first().waitFor({ state: 'detached' });
    assert.equal(await secondTab.locator('#connection').textContent(), '未连接');
    await secondTab.close();
    assert.equal(await page.evaluate(name => localStorage.getItem(name), savedKey), null);
    await page.reload();
    assert.equal(await page.locator('#rows tr').count(), 0);
    await submit(page, false);
    await connected(page);
    assert.equal(await page.evaluate(() => localStorage.length + sessionStorage.length), 0);
    await page.reload();
    assert.equal(await page.locator('#rows tr').count(), 0);
    await context.close();

    // Reject keys for another origin, as well as malformed stored credentials.
    for (const value of [JSON.stringify({ version: 1, apiBase: 'https://another-cpa.invalid', managementKey: key }), 'enc::v2::invalid-base64!']) {
      const isolated = await newPage();
      const requests = [];
      await isolated.page.addInitScript(({ name, value }) => localStorage.setItem(name, value), { name: savedKey, value });
      isolated.page.on('request', request => { if (new URL(request.url()).pathname.includes('/management/')) requests.push(request.url()); });
      await isolated.page.goto(entry);
      await isolated.page.waitForTimeout(300);
      assert.deepEqual(requests, [], 'unusable saved credentials must not be sent');
      await isolated.context.close();
    }

    // Explicit authentication failure forgets an invalid saved key; transient
    // server failures retain it for the next visit and never fetch records.
    for (const status of [401, 403, 503]) {
      const failed = await newPage({ storageState: persistedState });
      const requests = [];
      await failed.page.route('**/v0/management/plugins/request-recorder/status', route => route.fulfill({ status, json: { error: 'fixture status failure' } }));
      failed.page.on('request', request => { if (new URL(request.url()).pathname.includes('/management/plugins/')) requests.push(new URL(request.url()).pathname); });
      await failed.page.goto(entry);
      await failed.page.locator('#login-error').getByText(/已保存密钥无法连接/).waitFor();
      assert.equal(Boolean(await failed.page.evaluate(name => localStorage.getItem(name), savedKey)), status === 503);
      assert(requests.every(path => path.endsWith('/status')), 'validation must precede record access');
      assert.equal(await failed.page.locator('#rows tr').count(), 0);
      await failed.context.close();
    }

    // Browsers blocking persistent storage must still permit a manual session.
    const blocked = await newPage();
    await blocked.page.addInitScript(() => {
      Storage.prototype.getItem = () => { throw new DOMException('blocked', 'SecurityError'); };
      Storage.prototype.setItem = Storage.prototype.removeItem = () => { throw new DOMException('blocked', 'SecurityError'); };
    });
    await blocked.page.goto(entry);
    await submit(blocked.page, false);
    await connected(blocked.page);
    await blocked.page.locator('#login-error').getByText(/已连接，但浏览器无法移除/).waitFor();
    await blocked.context.close();

    // A late response to panel auto-login cannot undo a newer manual login.
    const raced = await newPage();
    await raced.page.addInitScript(base => localStorage.setItem('cli-proxy-auth', JSON.stringify({ state: { apiBase: base, managementKey: 'old-panel-key' } })), base);
    let finishOld, sawOld;
    const oldStarted = new Promise(resolve => { sawOld = resolve; });
    await raced.page.route('**/v0/management/plugins/request-recorder/status', async route => {
      if (route.request().headers().authorization !== 'Bearer old-panel-key') return route.continue();
      sawOld();
      await new Promise(resolve => { finishOld = resolve; });
      await route.fulfill({ status: 401, json: { error: 'old panel key' } });
    });
    await raced.page.goto(entry);
    await oldStarted;
    await submit(raced.page, true);
    await connected(raced.page);
    finishOld();
    await raced.page.waitForTimeout(300);
    assert.equal(await raced.page.locator('#connection').textContent(), '● 已连接');
    assert.equal(await raced.page.evaluate(name => Boolean(localStorage.getItem(name)), savedKey), true);
    await raced.context.close();
    assert.deepEqual(errors, []);
    console.log('PASS: opt-in remembered login, independent v2 decoding, reload/new-UA restore, no-save manual login, forget, failed-key rejection, origin isolation, transient failures, blocked storage, stale-login isolation');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });

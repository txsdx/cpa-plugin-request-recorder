// Exercise the real recorder UI inside a same-origin CPAMC-shaped parent.
const { chromium } = require('playwright-core');
const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const hash = text => createHash('sha256').update(text).digest('hex');

(async () => {
  const base = process.env.RECORDER_TEST_BASE || 'http://127.0.0.1:8787';
  const key = process.env.RECORDER_MANAGEMENT_KEY;
  if (!key) throw new Error('Set RECORDER_MANAGEMENT_KEY');
  const clientKey = 'sk-recorder-integration-client';
  const keyID = hash(clientKey);
  const nameID = hash(JSON.stringify(['api-key-name', base, clientKey]));
  const browser = await chromium.launch({executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true, args: ['--no-sandbox']});
  try {
    for (const version of [1, 2]) {
      const context = await browser.newContext();
      const page = await context.newPage();
      const errors = [], synced = [], requests = [];
      page.on('pageerror', error => errors.push(error.message));
      page.on('request', request => {
        requests.push(request);
        if (new URL(request.url()).pathname.endsWith('/aliases/sync')) synced.push(JSON.parse(request.postData()));
      });
      await page.route('**/v8/management/config/access/api-keys', route => route.fulfill({json: [clientKey]}));
      await page.route('**/panel-test', route => route.fulfill({contentType: 'text/html', body: '<iframe id="plugin" src="/v0/resource/plugins/request-recorder/index.html"></iframe>'}));
      // Match upstream encryption.ts: v1 includes UA; current v2 uses host only.
      await page.addInitScript(({base, key, nameID, version}) => {
        if (window.parent !== window) return;
        window.encodePanel = value => {
          const input = new TextEncoder().encode(JSON.stringify(value));
          const secret = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + (version === 1 ? '|' + navigator.userAgent : ''));
          const result = Array.from(input, (b, i) => String.fromCharCode(b ^ secret[i % secret.length])).join('');
          return 'enc::v' + version + '::' + btoa(result);
        };
        localStorage.setItem('cli-proxy-auth', window.encodePanel({state: {apiBase: base, managementKey: key, rememberPassword: true}, version: 0}));
        localStorage.setItem('api-key-names:v1:' + base, window.encodePanel({[nameID]: 'CPAMC 自动备注'}));
      }, {base, key, nameID, version});
      // Clear the previous smoke test's explicit alias before checking inheritance.
      const auth = {Authorization: 'Bearer ' + key};
      await context.request.post(base + '/v0/management/plugins/request-recorder/aliases', {headers: auth, data: {key_id: keyID, alias: ''}});
      await page.goto(base + '/panel-test');
      const frame = page.frameLocator('#plugin');
      await frame.locator('#rows tr').first().waitFor();
      const fixtureCount=await frame.locator('#total').textContent();
      assert.equal(await frame.locator('#login').evaluate(dialog => dialog.open), false);
      assert((await frame.locator('#rows').textContent()).includes('CPAMC 自动备注'));
      assert(synced.length > 0);
      assert.deepEqual(synced[0].aliases, {[keyID]: 'CPAMC 自动备注'});
      assert(!JSON.stringify(synced).includes(clientKey));
      await frame.locator('#filters input[name=alias]').fill('CPAMC 自动备注');
      await frame.locator('#filters button[type=submit]').click();
      await frame.locator('#rows tr').first().waitFor();
      assert.equal(await frame.locator('#total').textContent(), fixtureCount);
      await frame.locator('#filters button[type=reset]').click();
      // Actual cross-document storage event: edit the parent, observe iframe sync.
      await page.evaluate(({base, nameID}) => localStorage.setItem('api-key-names:v1:' + base, window.encodePanel({[nameID]: 'CPAMC 已修改备注'})), {base, nameID});
      await frame.locator('#rows').getByText('CPAMC 已修改备注').first().waitFor();
      await context.request.post(base + '/v0/management/plugins/request-recorder/aliases', {headers: auth, data: {key_id: keyID, alias: '保留手动别名'}});
      await page.evaluate(({base, nameID}) => localStorage.setItem('api-key-names:v1:' + base, window.encodePanel({[nameID]: 'CPAMC 不覆盖手动'})), {base, nameID});
      await frame.locator('#rows').getByText('保留手动别名').first().waitFor();
      // CPAMC logout clears the automatically borrowed credential and record view.
      await page.evaluate(() => localStorage.removeItem('cli-proxy-auth'));
      await frame.locator('#rows tr').first().waitFor({state: 'detached'});
      assert.equal(await frame.locator('#connection').textContent(), '未连接');
      assert.equal(await page.evaluate(() => localStorage.length + sessionStorage.length), 1);
      // A different server's saved credential must never be tried on this server.
      const other = await browser.newContext();
      const otherPage = await other.newPage();
      const otherDataRequests = [];
      await otherPage.addInitScript(() => localStorage.setItem('cli-proxy-auth', JSON.stringify({state: {apiBase: 'https://another-cpa.invalid', managementKey: 'wrong-server-secret'}})));
      otherPage.on('request', request => {if (request.url().includes('/management/')) otherDataRequests.push(request);});
      await otherPage.goto(base + '/v0/resource/plugins/request-recorder/index.html');
      await otherPage.waitForTimeout(400);
      assert.deepEqual(otherDataRequests, []);
      assert.equal(await otherPage.locator('#rows tr').count(), 0);
      assert(requests.filter(r => r.url().includes('/management/')).every(r => r.headers()['authorization'] === 'Bearer ' + key));
      assert.deepEqual(errors, []);
      await other.close();
      await context.close();
      console.log(`PASS: CPAMC enc::v${version}:: remembered login, Unicode notes, hash-only alias sync, filtering, note updates, overrides, logout cleanup, wrong-server credential rejection`);
    }
  } finally { await browser.close(); }
})().catch(error => {console.error(error); process.exit(1);});

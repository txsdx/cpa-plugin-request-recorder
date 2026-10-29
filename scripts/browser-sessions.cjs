// Use the five-conversation database produced by scripts/integration.py.
const { chromium } = require('playwright-core');
const assert = require('node:assert/strict');

(async () => {
  const base = process.env.RECORDER_TEST_BASE || 'http://127.0.0.1:8787';
  const key = process.env.RECORDER_MANAGEMENT_KEY;
  if (!key) throw new Error('Set RECORDER_MANAGEMENT_KEY');
  const browser = await chromium.launch({executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true, args: ['--no-sandbox']});
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(base + '/v0/resource/plugins/request-recorder/index.html');
    await page.locator('#login-open').click();
    await page.locator('#management-key').fill(key);
    await page.locator('#login-form button[type=submit]').click();
    await page.locator('#login').waitFor({state: 'hidden'});
    await page.locator('#filters details summary').click();
    const ids = new Set();
    for (const [session, label] of [
      ['pck:cherry-agent:0123456789abcdef0123456789abcdef', 'cherry'],
      ['codex:integration-parent', 'codex-parent'],
      ['codex:integration-child', 'codex-child']
    ]) {
      await page.locator('#filters input[name=session]').fill(session);
      const response = page.waitForResponse(r => new URL(r.url()).pathname.endsWith('/records') && new URL(r.url()).searchParams.get('session') === session);
      await page.locator('#filters button[type=submit]').click();
      assert((await response).ok());
      await page.waitForFunction(label => document.querySelector('#total').textContent === '1' && document.querySelector('#rows').textContent.includes(label + ' question 2'), label);
      await page.locator('#rows tr').click();
      await page.locator('#detail').waitFor({state: 'visible'});
      for (const turn of [1, 2]) assert((await page.locator('#detail-body').textContent()).includes(label + ' question ' + turn));
      assert.equal(await page.locator('[data-user-prompt]').count(), 2);
      assert((await page.locator('#detail-sub').textContent()).includes('总消耗 token 520'));
      const download = page.waitForEvent('download');
      await page.locator('#export').click();
      const result = await download;
      assert(!ids.has(result.suggestedFilename()), 'parent and child must export distinct conversations');
      ids.add(result.suggestedFilename());
      await page.locator('[data-close=detail]').click();
    }
    assert.deepEqual(errors, []);
    console.log('PASS: Billing session ID filters, Cherry/Codex incremental history replay, parent/child separation, two prompt targets, cumulative totals and distinct conversation exports');
  } finally {
    await browser.close();
  }
})().catch(error => {console.error(error); process.exit(1);});

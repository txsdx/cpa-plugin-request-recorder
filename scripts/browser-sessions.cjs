// Use the seven-conversation database produced by scripts/integration.py.
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
    for (const label of ['fork-a', 'fork-b']) {
      const session = 'codex:' + label;
      await page.locator('#filters input[name=session]').fill(session);
      const response = page.waitForResponse(r => new URL(r.url()).pathname.endsWith('/records') && new URL(r.url()).searchParams.get('session') === session);
      await page.locator('#filters button[type=submit]').click();
      assert((await response).ok());
      await page.waitForFunction(label => document.querySelector('#total').textContent === '1' && document.querySelector('#rows').textContent.includes(label + ' question'), label);
      await page.locator('#rows tr').click();
      await page.locator('#detail').waitFor({state: 'visible'});
      const content = await page.locator('#detail-body').textContent();
      assert(content.includes('图里是什么？') && content.includes(label + ' question'));
      assert(!content.includes('继续解释这张图'), 'fork must exclude later parent turns');
      assert((await page.locator('.branch-note').textContent()).includes('已继承到指定父响应'));
      assert((await page.locator('#detail-sub').textContent()).includes('总消耗 token 260'));
      assert.equal(await page.locator('[data-user-prompt]').count(), 2);
      await page.waitForFunction(() => [...document.querySelectorAll('#detail-body img.original-image')].some(img => img.complete && img.naturalWidth > 0));
      const downloadEvent = page.waitForEvent('download');
      await page.locator('#export-markdown').click();
      const download = await downloadEvent;
      const chunks = [];
      for await (const chunk of await download.createReadStream()) chunks.push(chunk);
      const markdown = Buffer.concat(chunks).toString('utf8');
      assert(markdown.includes('分支历史：已继承到父响应') && markdown.includes('data:image/png;base64,'));
      assert(!markdown.includes('继续解释这张图'));
      if (label === 'fork-b') {
        await page.setViewportSize({width: 390, height: 844});
        assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
        assert(await page.locator('[data-parent-conversation]').isVisible());
      }
      await page.locator('[data-parent-conversation]').click();
      await page.waitForFunction(() => document.querySelector('#detail-body').textContent.includes('继续解释这张图'));
      assert((await page.locator('#detail-sub').textContent()).includes('总消耗 token 520'));
      await page.locator('[data-close=detail]').click();
    }
    assert.deepEqual(errors, []);
    console.log('PASS: Billing session filters, incremental history, sibling fork boundaries, inherited image preview and Markdown, separate totals, parent navigation and mobile branch controls');
  } finally {
    await browser.close();
  }
})().catch(error => {console.error(error); process.exit(1);});

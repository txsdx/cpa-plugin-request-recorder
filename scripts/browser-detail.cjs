// Use the real two-round Responses conversation from scripts/integration.py.
const { chromium } = require('playwright-core');
const assert = require('node:assert/strict');

(async () => {
  const base = process.env.RECORDER_TEST_BASE || 'http://127.0.0.1:8787';
  const key = process.env.RECORDER_MANAGEMENT_KEY;
  if (!key) throw new Error('Set RECORDER_MANAGEMENT_KEY');
  const browser = await chromium.launch({executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true, args: ['--no-sandbox']});
  try {
    const page = await browser.newPage({viewport: {width: 1000, height: 500}});
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(base + '/v0/resource/plugins/request-recorder/index.html');
    await page.locator('#login-open').click();
    await page.locator('#management-key').fill(key);
    await page.locator('#login-form button[type=submit]').click();
    await page.locator('#login').waitFor({state: 'hidden'});
    await page.locator('#rows tr').filter({hasText: 'responses-model'}).click();
    await page.locator('#detail').waitFor({state: 'visible'});
    const bounds = await page.locator('#detail').boundingBox();
    assert.deepEqual(bounds, {x: 0, y: 0, width: 1000, height: 500});
    assert.equal(await page.locator('#detail-metrics').count(), 0);
    const sub = await page.locator('#detail-sub').textContent();
    assert(sub.includes('会话总耗时') && sub.includes('总消耗 token 520'));
    assert(sub.includes('最新') && /\d{4}\/\d{1,2}\/\d{1,2}/.test(sub));
    assert(!sub.includes('conv_') && !sub.includes('次模型请求'));
    assert.equal(await page.locator('[data-user-prompt]').count(), 2, 'input text and image must share one prompt target');
    await page.waitForFunction(() => [...document.querySelectorAll('#detail-body img.original-image')].every(img => img.complete && img.naturalWidth > 0));
    const next = page.getByRole('button', {name: '↓ 下一条用户提示词', exact: true});
    const prev = page.getByRole('button', {name: '↑ 上一条用户提示词', exact: true});
    if ((await page.locator('#prompt-position').textContent()).startsWith('—')) await next.click();
    await next.click();
    assert.equal(await page.locator('#prompt-position').textContent(), '2 / 2');
    assert.equal(await page.locator('.prompt-current').textContent().then(text => text.includes('继续解释这张图')), true);
    assert.equal(await next.isDisabled(), true);
    assert.equal(await prev.isDisabled(), false);
    const scrolled = await page.locator('#detail-body').evaluate(el => el.scrollTop);
    assert(scrolled > 0, 'navigation must scroll the conversation');
    assert.equal(await page.locator('#prompt-next').evaluate(el => el.getBoundingClientRect().bottom < innerHeight), true);
    await page.locator('#refresh').evaluate(button => button.click());
    await page.waitForTimeout(1200);
    assert.equal(await page.locator('#detail-body').evaluate(el => el.scrollTop), scrolled, 'list refresh must preserve reading position');
    await prev.click();
    assert.equal(await page.locator('#prompt-position').textContent(), '1 / 2');
    assert((await page.locator('#detail-body').evaluate(el => el.scrollTop)) < scrolled);
    await page.locator('[data-tab=metadata]').click();
    assert.equal(await page.locator('#prompt-nav').isVisible(), false);
    await page.locator('[data-tab=conversation]').click();
    await page.setViewportSize({width: 390, height: 844});
    const mobile = await page.locator('#detail').boundingBox();
    assert.deepEqual(mobile, {x: 0, y: 0, width: 390, height: 844});
    assert(await page.evaluate(() => document.querySelector('#detail').scrollWidth <= innerWidth), 'mobile dialog must not overflow horizontally');
    await page.screenshot({path: '/tmp/recorder-fullscreen-mobile.png'});
    await page.setViewportSize({width: 1440, height: 1050});
    const title = await page.locator('#detail-title').boundingBox();
    const summary = await page.locator('#detail-sub').boundingBox();
    assert(summary.x > title.x + title.width && summary.y < title.y + title.height, 'model and summary must share one desktop row');
    await page.screenshot({path: '/tmp/recorder-fullscreen-detail.png'});
    assert.deepEqual(errors, []);
    console.log('PASS: inline model/summary/latest-time header, fullscreen desktop/mobile detail, no metric strip, cumulative tokens, grouped prompt navigation, boundary buttons, preserved reading position, tab visibility');
  } finally {
    await browser.close();
  }
})().catch(error => {console.error(error); process.exit(1);});

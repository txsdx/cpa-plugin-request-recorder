// Use a running standalone development server with the integration fixture DB.
const {chromium} = require('playwright-core');
const assert = require('node:assert/strict');
(async()=>{
 const base=process.env.RECORDER_TEST_BASE||'http://127.0.0.1:8787';
 const key=process.env.RECORDER_MANAGEMENT_KEY;if(!key)throw new Error('Set RECORDER_MANAGEMENT_KEY');
 const browser=await chromium.launch({executablePath:process.env.CHROMIUM_PATH||'/usr/bin/chromium',headless:true,args:['--no-sandbox']});
 try{
  const page=await browser.newPage({viewport:{width:1440,height:1050}});const errors=[];page.on('pageerror',e=>errors.push(e.message));
  await page.goto(base+'/v0/resource/plugins/request-recorder/index.html');
  await page.locator('#login-open').click();await page.locator('#management-key').fill(key);await page.locator('#login-form button[type=submit]').click();
  await page.locator('#login').waitFor({state:'hidden'});await page.waitForFunction(()=>document.querySelectorAll('#rows tr').length>=2);
  await page.screenshot({path:process.env.RECORDER_SCREENSHOT_LIST||'/tmp/recorder-list.png',fullPage:true});
  await page.locator('#filters input[name=model]').fill('not-a-real-model');await page.locator('#filters button[type=submit]').click();await page.waitForFunction(()=>document.querySelectorAll('#rows tr').length===0);assert.equal(await page.locator('#total').textContent(),'0');
  await page.locator('#filters button[type=reset]').click();await page.waitForFunction(()=>document.querySelectorAll('#rows tr').length>=2);
  // The nonstream fixture has input and output images. Open each record until found.
  const count=await page.locator('#rows tr').count();let found=false;
  for(let i=0;i<count;i++){
   await page.locator('#rows tr').nth(i).click();await page.locator('#detail').waitFor({state:'visible'});
   const images=page.locator('#detail-body img.original-image');
   if(await images.count()>=3){found=true;await page.waitForFunction(()=>[...document.querySelectorAll('#detail-body img')].every(i=>i.complete&&i.naturalWidth>0));break;}
   await page.locator('[data-close=detail]').click();
  }
  assert(found,'input/downstream/upstream original images must render');
  await page.locator('#detail-body img.original-image').first().click();await page.locator('#image-view').waitFor({state:'visible'});assert((await page.locator('#full-image').getAttribute('src')).startsWith('data:image/png;base64,'));await page.locator('[data-close=image-view]').click();
  await page.screenshot({path:process.env.RECORDER_SCREENSHOT_DETAIL||'/tmp/recorder-detail.png',fullPage:true});
  await page.locator('[data-tab=request]').click();assert((await page.locator('#detail-body').textContent()).includes('data:image/png;base64,'));
  await page.locator('[data-tab=upstream]').click();assert((await page.locator('#detail-body').textContent()).includes('API RESPONSE 1'));
  await page.locator('[data-tab=metadata]').click();assert((await page.locator('#detail-body').textContent()).includes('trace_id'));
  await page.locator('#detail-body button').first().click();await page.locator('#alias-name').fill('浏览器测试 key');await page.locator('#alias-form button[type=submit]').click();await page.locator('#alias-dialog').waitFor({state:'hidden'});await page.waitForFunction(()=>document.querySelector('#detail-metrics')?.textContent.includes('浏览器测试 key'));
  await page.locator('[data-close=detail]').click();await page.setViewportSize({width:390,height:844});
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),'mobile page must not overflow horizontally');
  assert.deepEqual(errors,[]);
  // Neither credentials nor record content is kept in browser persistent storage.
  assert.equal(await page.evaluate(()=>localStorage.length+sessionStorage.length),0);
  console.log('PASS: login, filtering, detail tabs, three original image previews, aliases, mobile layout, no JS errors, no persistent browser storage');
 }finally{await browser.close();}
})().catch(e=>{console.error(e);process.exit(1);});

// Use a running standalone development server with the integration fixture DB.
const {chromium} = require('playwright-core');
const assert = require('node:assert/strict');
(async()=>{
 const base=process.env.RECORDER_TEST_BASE||'http://127.0.0.1:8787';
 const key=process.env.RECORDER_MANAGEMENT_KEY;if(!key)throw new Error('Set RECORDER_MANAGEMENT_KEY');
 const browser=await chromium.launch({executablePath:process.env.CHROMIUM_PATH||'/usr/bin/chromium',headless:true,args:['--no-sandbox']});
 try{
  const page=await browser.newPage({viewport:{width:1440,height:1050}});const errors=[],externalRequests=[],imageRequests=[];let updatedHead=null;page.on('pageerror',e=>errors.push(e.message));
  await page.addInitScript(()=>{window.revokedImageURLs=[];const original=URL.revokeObjectURL.bind(URL);URL.revokeObjectURL=url=>{window.revokedImageURLs.push(url);return original(url);};});
  page.on('request',request=>{const url=new URL(request.url());if(['http:','https:'].includes(url.protocol)&&url.origin!==new URL(base).origin)externalRequests.push(request.url());if(url.pathname.endsWith('/image'))imageRequests.push(request);});
  await page.route('**/v8/management/config/access/api-keys', route=>route.fulfill({json:['sk-recorder-integration-client']}));
  // Even if a response includes an adversarial original URL, rendering must
  // only use the authenticated archive ID and never visit that URL.
  await page.route('**/v0/management/plugins/request-recorder/record?*',async route=>{const response=await route.fetch();if(response.ok()){const data=await response.json();if(data.record.id===updatedHead){data.record.request_id+='-updated';data.conversation.push({role:'assistant',kind:'text',content:'UI 自动更新的最新回复',source:'response'});}for(const m of [...(data.conversation||[])])for(const image of m.images||[])image.url='https://image-tracker.invalid/original.png';await route.fulfill({response,json:data});}else await route.fulfill({response});});
  await page.route('**/v0/management/plugins/request-recorder/records?*',async route=>{const response=await route.fetch();if(response.ok()){const data=await response.json();for(const item of data.items)if(item.id===updatedHead)item.request_id+='-updated';await route.fulfill({response,json:data});}else await route.fulfill({response});});
  await page.goto(base+'/v0/resource/plugins/request-recorder/index.html');
  await page.locator('#login-open').click();await page.locator('#management-key').fill(key);await page.locator('#login-form button[type=submit]').click();
  await page.locator('#login').waitFor({state:'hidden'});await page.waitForFunction(()=>document.querySelectorAll('#rows tr').length>=2);
  await page.locator('#storage-open').click();await page.locator('#storage-body').getByText('去重压缩后正文',{exact:true}).waitFor();assert((await page.locator('#storage-body').textContent()).includes('自动淘汰最旧记录'));await page.locator('[data-close=storage-view]').click();
  assert.equal(await page.locator('#auto').isChecked(),true);assert.equal(await page.locator('#refresh-interval').inputValue(),'1');assert.equal(await page.locator('.toolbar #refresh').count(),1);assert.equal(await page.locator('thead th').count(),8);const widthRatio=await page.locator('main').evaluate(el=>el.getBoundingClientRect().width/window.innerWidth);assert(widthRatio>0.89&&widthRatio<0.91);assert((await page.locator('#rows').textContent()).includes('图里是什么？'));
  const ignored=page.getByLabel('忽略已拒绝条目');
  assert.equal(await ignored.isChecked(),false);
  await ignored.check();
  await page.waitForFunction(()=>!document.querySelector('#ignore-rejected').disabled);
  const savedSettings=await page.request.get(base+'/v0/management/plugins/request-recorder/status',{headers:{Authorization:'Bearer '+key}});
  assert.equal((await savedSettings.json()).ignore_rejected,true);
  await page.reload();
  await page.locator('#login-open').click();await page.locator('#management-key').fill(key);await page.locator('#login-form button[type=submit]').click();
  await page.locator('#login').waitFor({state:'hidden'});await page.waitForFunction(()=>document.querySelectorAll('#rows tr').length===2);
  assert.equal(await ignored.isChecked(),true);
  assert.equal(await page.locator('#auto').isChecked(),true);
  assert.equal(await page.locator('#refresh-interval').inputValue(),'1');
  await ignored.uncheck();
  await page.waitForFunction(()=>!document.querySelector('#ignore-rejected').disabled);
  const resetSettings=await page.request.get(base+'/v0/management/plugins/request-recorder/status',{headers:{Authorization:'Bearer '+key}});
  assert.equal((await resetSettings.json()).ignore_rejected,false);
  const savePeriod=page.waitForResponse(r=>r.url().endsWith('/settings')&&r.request().method()==='POST');await page.locator('#refresh-interval').fill('2');await page.locator('#refresh-interval').press('Tab');assert((await savePeriod).ok());const resetPeriod=page.waitForResponse(r=>r.url().endsWith('/settings')&&r.request().method()==='POST');await page.locator('#refresh-interval').fill('1');await page.locator('#refresh-interval').press('Tab');assert((await resetPeriod).ok());
  await page.screenshot({path:process.env.RECORDER_SCREENSHOT_LIST||'/tmp/recorder-list.png',fullPage:true});
  await page.locator('#filters input[name=model]').fill('not-a-real-model');await page.locator('#filters button[type=submit]').click();await page.waitForFunction(()=>document.querySelectorAll('#rows tr').length===0);assert.equal(await page.locator('#total').textContent(),'0');
  await page.locator('#filters button[type=reset]').click();await page.waitForFunction(()=>document.querySelectorAll('#rows tr').length>=2);
  await page.locator('#filters details summary').click();await page.locator('#filters input[name=key]').fill('sk-recorder-integration-client');await page.locator('#filters button[type=submit]').click();await page.waitForFunction(()=>document.querySelector('#total').textContent==='2');
  await page.locator('#filters input[name=key]').fill('missing-client-key');await page.locator('#filters button[type=submit]').click();await page.waitForFunction(()=>document.querySelector('#total').textContent==='0');await page.locator('#filters button[type=reset]').click();await page.waitForFunction(()=>document.querySelectorAll('#rows tr').length===2);
  // The nonstream fixture has input and output images. Open each record until found.
  const count=await page.locator('#rows tr').count();let found=false;
  for(let i=0;i<count;i++){
   await page.locator('#rows tr').nth(i).click();await page.locator('#detail').waitFor({state:'visible'});
   const images=page.locator('#detail-body img.original-image');
   await page.waitForTimeout(1200);
   if(await images.count()>=2){found=true;for(let n=0;n<await images.count();n++){await images.nth(n).scrollIntoViewIfNeeded();await images.nth(n).evaluate(img=>img.complete&&img.naturalWidth>0||new Promise((resolve,reject)=>{const timeout=setTimeout(()=>reject(new Error('preview image did not decode within 10s')),10000);img.addEventListener('load',()=>{clearTimeout(timeout);resolve();},{once:true});img.addEventListener('error',()=>{clearTimeout(timeout);reject(new Error('preview image decode failed'));},{once:true});}));}break;}
   await page.locator('[data-close=detail]').click();
  }
  assert(found,'input and model output original images must render');
  assert(imageRequests.every(request=>new URL(request.url()).searchParams.get('variant')==='preview'),'opening a conversation must only load WebP previews');
  await page.locator('#detail-body img.original-image').first().click();await page.locator('#image-view').waitFor({state:'visible'});assert((await page.locator('#full-image').getAttribute('src')).startsWith('blob:'));const downloadEvent=page.waitForEvent('download');await page.locator('#image-download').click();const originalDownload=await downloadEvent;assert(originalDownload.suggestedFilename().endsWith('.png'));const originalStream=await originalDownload.createReadStream();const originalChunks=[];for await(const chunk of originalStream)originalChunks.push(chunk);assert.equal(Buffer.concat(originalChunks).toString('base64'),'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==');assert(imageRequests.some(request=>new URL(request.url()).searchParams.get('variant')==='original'));
  await page.locator('[data-close=image-view]').click();
  const markdownEvent=page.waitForEvent('download');const markdownRequest=page.waitForRequest(r=>new URL(r.url()).pathname.endsWith('/export/markdown'));await page.locator('#export-markdown').click();const markdownDownload=await markdownEvent;assert(markdownDownload.suggestedFilename().endsWith('.md'));assert.equal((await markdownRequest).headers()['authorization'],'Bearer '+key);const markdownStream=await markdownDownload.createReadStream();const markdownChunks=[];for await(const chunk of markdownStream)markdownChunks.push(chunk);const markdown=Buffer.concat(markdownChunks).toString('utf8');assert(markdown.startsWith('# 完整会话记录'));assert(markdown.includes('图里是什么？'));assert(markdown.includes('看到了这张图片。'));assert.equal(markdown.split('data:image/png;base64,').length-1,1,'repeated original images must be embedded once');assert(markdown.includes('data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=='));assert(!markdown.includes('image-tracker.invalid'));assert(!markdown.includes(key));
  const jsonEvent=page.waitForEvent('download');await page.locator('#export').click();const jsonDownload=await jsonEvent;assert(jsonDownload.suggestedFilename().endsWith('.json'));const jsonStream=await jsonDownload.createReadStream();const jsonChunks=[];for await(const chunk of jsonStream)jsonChunks.push(chunk);const exportedJSON=JSON.parse(Buffer.concat(jsonChunks).toString());assert(JSON.parse(exportedJSON.record.request_body).messages[0].content.some(block=>block.text==='图里是什么？'));assert(exportedJSON.conversation.some(message=>message.content.includes('看到了这张图片。')));
  updatedHead=exportedJSON.record.id;await page.locator('#detail-body').getByText('UI 自动更新的最新回复',{exact:true}).waitFor();
  await page.screenshot({path:process.env.RECORDER_SCREENSHOT_DETAIL||'/tmp/recorder-detail.png',fullPage:true});
  await page.locator('[data-tab=request]').click();await page.waitForFunction(()=>document.querySelector('#detail-body').textContent.includes('data:image/png;base64,'));assert((await page.locator('#detail-body').textContent()).includes('data:image/png;base64,'));
  await page.locator('[data-tab=metadata]').click();assert(!(await page.locator('#detail-body').textContent()).includes('key_id'));assert.equal(await page.locator('input[name=ip],input[name=key_id]').count(),0);assert.equal(await page.locator('#caller-keys option').first().getAttribute('value'),'sk-recorder-integration-client');assert((await page.locator('#detail-body').textContent()).includes('trace_id'));
  await page.locator('#detail-body button').first().click();await page.locator('#alias-name').fill('浏览器测试 key');await page.locator('#alias-form button[type=submit]').click();await page.locator('#alias-dialog').waitFor({state:'hidden'});await page.waitForFunction(()=>document.querySelector('#rows').textContent.includes('浏览器测试 key'));
  await page.locator('[data-close=detail]').click();await page.setViewportSize({width:390,height:844});
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),'mobile page must not overflow horizontally');
  assert.deepEqual(errors,[]);assert.deepEqual(externalRequests,[],'browser must never contact image origins');assert(imageRequests.length>0);assert(imageRequests.every(r=>r.headers()['authorization']==='Bearer '+key),'image requests must use management auth');assert(await page.evaluate(()=>window.revokedImageURLs.length>0),'image object URLs must be released');
  // Neither credentials nor record content is kept in browser persistent storage.
  assert.equal(await page.evaluate(()=>localStorage.length+sessionStorage.length),0);
  console.log('PASS: login, persisted ignore-rejected checkbox, actual-key filtering, live conversation refresh, detail tabs, Markdown and JSON downloads with original images, WebP-only previews and byte-identical original downloads, aliases, mobile layout, no JS errors, no persistent browser storage, no external image requests, authenticated downloads, released Blob URLs');
 }finally{await browser.close();}
})().catch(e=>{console.error(e);process.exit(1);});

"""Exercise the built shared library in a real CPA host against a local mock.
Usage: CPA_BIN=/path/to/cli-proxy-api python3 scripts/integration.py
No real upstream account/key or paid API calls are used.
"""
import base64
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import uuid
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
PNG = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=='
IMAGE = 'data:image/png;base64,' + PNG
KEY = 'sk-recorder-integration-client'
MANAGEMENT = 'recorder-integration-management'

class Mock(BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if self.path.endswith('/responses'):
            self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
            response_id='resp_'+uuid.uuid4().hex
            text='完整最终快照：'+''.join(f'{i},' for i in range(160))
            message={'type':'message','id':'msg_'+response_id,'role':'assistant','content':[{'type':'output_text','text':text}]}
            events=[{'type':'response.created','response':{'id':response_id,'model':'responses-model','output':[]}}]
            events += [{'type':'response.output_text.delta','output_index':0,'content_index':0,'delta':f'{i},'} for i in range(160)]
            events += [{'type':'response.output_text.done','output_index':0,'content_index':0,'text':text},{'type':'response.output_item.done','output_index':0,'item':message},{'type':'response.completed','response':{'id':response_id,'object':'response','model':'responses-model','status':'completed','output':[message],'usage':{'input_tokens':100,'output_tokens':160,'total_tokens':260,'input_tokens_details':{'cached_tokens':50}}}}]
            for event in events:
                self.wfile.write(('event: '+event['type']+'\ndata: '+json.dumps(event)+'\n\n').encode());self.wfile.flush()
            return
        self.send_response(200)
        stream = body.get('stream', False)
        self.send_header('Content-Type', 'text/event-stream' if stream else 'application/json')
        self.end_headers()
        if stream:
            for i in range(160):
                chunk = {'id': 'mock-stream', 'object': 'chat.completion.chunk', 'model': 'mock-model', 'choices': [{'index': 0, 'delta': {'content': f'{i},'}, 'finish_reason': None}]}
                self.wfile.write(('data: '+json.dumps(chunk)+'\n\n').encode()); self.wfile.flush()
            self.wfile.write(('data: '+json.dumps({'id':'mock-stream','model':'mock-model','choices':[], 'usage': {'prompt_tokens':100,'completion_tokens':160,'total_tokens':260,'prompt_tokens_details': {'cached_tokens':50}}})+'\n\ndata: [DONE]\n\n').encode())
        else:
            self.wfile.write(json.dumps({'id':'mock-json','object':'chat.completion','model':'mock-model','choices':[{'index':0,'message':{'role':'assistant','content':[{'type':'text','text':'看到了这张图片。'},{'type':'image_url','image_url':{'url':IMAGE}}]},'finish_reason':'stop'}],'usage':{'prompt_tokens':100,'completion_tokens':10,'total_tokens':110,'prompt_tokens_details':{'cached_tokens':50}}}).encode())
        self.wfile.flush()

def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1',0)); return s.getsockname()[1]

def run():
    binary = os.environ.get('CPA_BIN')
    if not binary: raise SystemExit('Set CPA_BIN to a schema >= 6 CPA binary')
    mock = ThreadingHTTPServer(('127.0.0.1',0), Mock)
    threading.Thread(target=mock.serve_forever,daemon=True).start()
    directory = os.environ.get('INTEGRATION_DIR')
    work = Path(directory) if directory else Path(tempfile.mkdtemp(prefix='recorder-integration-'))
    work.mkdir(parents=True, exist_ok=True)
    port = free_port()
    plugins = work/'plugins'; plugins.mkdir(exist_ok=True)
    shutil.copyfile(ROOT/'dist/request-recorder.so', plugins/'request-recorder.so')
    config = f'''host: "127.0.0.1"
port: {port}
auth-dir: "{work / 'auth'}"
remote-management:
  allow-remote: false
  secret-key: "{MANAGEMENT}"
  disable-control-panel: true
api-keys: ["{KEY}"]
request-log: false
usage-statistics-enabled: true
request-retry: 0
openai-compatibility:
  - name: "mock"
    base-url: "http://127.0.0.1:{mock.server_port}/v1"
    api-key-entries:
      - api-key: "upstream-test-key"
    models:
      - name: "mock-model"
        alias: "mock-model"
codex-api-key:
  - api-key: "mock-codex-test-key"
    base-url: "http://127.0.0.1:{mock.server_port}/v1"
    models:
      - name: "responses-model"
        alias: "responses-model"
plugins:
  enabled: true
  dir: "{plugins}"
  configs:
    request-recorder:
      enabled: true
      db_path: "{work / 'recorder.db'}"
'''
    (work/'config.yaml').write_text(config)
    def request(path, data=None, key=MANAGEMENT, session=None):
        headers={'Content-Type':'application/json'}
        if key is not None: headers['Authorization']='Bearer '+key
        if session: headers['X-Session-ID']=session
        req=urllib.request.Request(f'http://127.0.0.1:{port}'+path, data=None if data is None else json.dumps(data).encode(),headers=headers)
        # Local HTTP checks must not go through the remote egress proxy.
        return urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req,timeout=15).read()
    output=(work/'host.log').open('w')
    proc=subprocess.Popen([str(Path(binary).resolve()),'-config',str(work/'config.yaml'),'--local-model'],cwd=work,stdout=output,stderr=subprocess.STDOUT)
    try:
        for _ in range(100):
            if proc.poll() is not None: raise AssertionError('CPA exited during startup')
            try:
                info=json.loads(request('/v0/management/plugins'))
                if 'request-recorder' in json.dumps(info): break
            except (urllib.error.URLError, TimeoutError): pass
            time.sleep(.2)
        else: raise AssertionError('plugin not registered')
        public=request('/v0/resource/plugins/request-recorder/index.html',key=None)
        assert b'app.js' in public
        try: request('/v0/management/plugins/request-recorder/records',key=None)
        except urllib.error.HTTPError as e: assert e.code in (401,403)
        else: raise AssertionError('unauthenticated record access')
        for stream in [False,True]:
            body={'model':'mock-model','stream':stream,'messages':[{'role':'user','content':[{'type':'text','text':'图里是什么？'},{'type':'image_url','image_url':{'url':IMAGE}}]}]}
            if stream:
                body={'model':'responses-model','stream':True,'input':[{'role':'user','content':[{'type':'input_text','text':'图里是什么？'},{'type':'input_image','image_url':IMAGE}]}]}
            data=request('/v1/responses' if stream else '/v1/chat/completions',body,key=KEY,session='stream' if stream else 'json')
            if stream: assert b'159,' in data
        records=None
        for _ in range(150):
            records=json.loads(request('/v0/management/plugins/request-recorder/records'))
            if records['total']==2 and all(x['key_id'] and x['latest_response'] for x in records['items']):break
            time.sleep(.2)
        assert records['total']==2,records
        for item in records['items']:
            assert item['outcome']=='succeeded',item
            assert item['key_id']==hashlib.sha256(KEY.encode()).hexdigest(),item
            d=json.loads(request('/v0/management/plugins/request-recorder/record?id='+item['id']))
            assert IMAGE in d['record']['request_body']
            for _ in range(100):
                state=json.loads(request('/v0/management/plugins/request-recorder/images?record_id='+item['id']))
                if state['images'] and all(im['status']=='ready' for im in state['images']): break
                time.sleep(.05)
            assert state['images'] and all(im['status']=='ready' for im in state['images']),state
            image=state['images'][0]
            image_path='/v0/management/plugins/request-recorder/image?record_id='+item['id']+'&id='+image['id']
            assert request(image_path)==base64.b64decode(PNG)
            preview=request(image_path+'&variant=preview')
            assert preview.startswith(b'RIFF') and preview[8:12]==b'WEBP'
            try: request(image_path+'&variant=preview',key=None)
            except urllib.error.HTTPError as e: assert e.code in (401,403)
            else: raise AssertionError('public preview access allowed')
            try: request(image_path,key=None)
            except urllib.error.HTTPError as e: assert e.code in (401,403)
            else: raise AssertionError('public image access allowed')
            assert (work/'recorder-images'/(image['hash']+'.png')).read_bytes()==base64.b64decode(PNG)
            assert '图里是什么？' in d['record']['latest_prompt']
            assert d['record']['latest_response']
            if item['stream']:
                assert '159,' in d['record']['response_body'] and not d['record']['response_body'].startswith('data: ')
                assert item['tokens']['TotalTokens']==260,item
                assert d['record']['response_source']=='cpa_final_snapshot',d['record']['response_source']
                assert json.loads(d['record']['response_body'])['object']=='response'
                assert '完整最终快照：' in d['record']['latest_response']
            else:
                assert any(m.get('images') for m in d['conversation'] if m['source']=='request')
        # An incremental turn in the same explicit client session must replace
        # its row while retaining the prior text and image history.
        stream_head=next(item for item in records['items'] if item['stream'])
        request('/v1/responses',{'model':'responses-model','stream':True,'input':'继续解释这张图'},key=KEY,session='stream')
        for _ in range(150):
            updated=json.loads(request('/v0/management/plugins/request-recorder/records'))
            newest=next((item for item in updated['items'] if item['id']==stream_head['id']),{})
            if updated['total']==2 and newest.get('conversation_turns')==2: break
            time.sleep(.2)
        assert updated['total']==2 and newest['request_id']!=stream_head['request_id'] and newest['conversation_turns']==2,updated
        history=json.loads(request('/v0/management/plugins/request-recorder/record?id='+stream_head['id']))
        assert any('继续解释这张图' in message.get('content','') for message in history['conversation'])
        assert any('图里是什么？' in message.get('content','') for message in history['conversation'])
        assert sum('159,' in message.get('content','') for message in history['conversation'])==2
        assert any(message.get('images') for message in history['conversation'])
        markdown_path='/v0/management/plugins/request-recorder/export/markdown?id='+stream_head['id']
        markdown=request(markdown_path).decode('utf-8')
        assert markdown.startswith('# 完整会话记录')
        assert '图里是什么？' in markdown and '继续解释这张图' in markdown
        assert markdown.count('完整最终快照：')==2
        assert markdown.count(IMAGE)==1, 'original image must be embedded once'
        assert KEY not in markdown and MANAGEMENT not in markdown
        try: request(markdown_path,key=None)
        except urllib.error.HTTPError as e: assert e.code in (401,403)
        else: raise AssertionError('unauthenticated Markdown export allowed')
        status=json.loads(request('/v0/management/plugins/request-recorder/status'))
        assert status['refresh_interval_seconds']==1 and status['max_database_bytes']==1073741824
        assert 'logs_enabled' not in status and 'pending_logs' not in status
        assert all(not name.startswith('upstream_') for name in history['record'])
        assert 'upstream_conversation' not in history
        storage=json.loads(request('/v0/management/plugins/request-recorder/storage'))
        assert storage['body_chunks']>0 and storage['body_stored_bytes']>0
        key_id=hashlib.sha256(KEY.encode()).hexdigest()
        sync=json.loads(request('/v0/management/plugins/request-recorder/aliases/sync',{'aliases':{key_id:'CPAMC 自动备注'}}))
        assert sync['synced']==1 and sync['manual_aliases_preserved']
        assert json.loads(request('/v0/management/plugins/request-recorder/records?alias=CPAMC'))['total']==2
        try: request('/v0/management/plugins/request-recorder/aliases/sync',{'aliases':{}},key=None)
        except urllib.error.HTTPError as e: assert e.code in (401,403),e.code
        else: raise AssertionError('unauthenticated alias sync allowed')
        alias=json.loads(request('/v0/management/plugins/request-recorder/aliases',{'key':KEY,'alias':'集成测试'}))
        assert alias['ok']
        page=json.loads(request('/v0/management/plugins/request-recorder/records?alias='+urllib.parse.quote('集成测试')))
        assert page['total']==2
        print(json.dumps({'result':'PASS','records':2,'stream_chunks':160,'checks':['native CPA loading','public GUI registration','management authentication','nonstream and SSE capture','usage join','native Responses complete final snapshot without logs','latest conversation replaces prior snapshot','authenticated full-history Markdown export with original images','input and output images','authenticated local image archive','image file persistence','compressed WebP preview and original download','CPAMC alias sync authentication','alias filtering'],'workdir':str(work),'port':port},ensure_ascii=False))
        if os.environ.get('KEEP_RUNNING')=='1':
            print('KEEP_RUNNING: press Ctrl+C to stop',flush=True)
            while proc.poll() is None: time.sleep(1)
    except BaseException:
        output.flush();print((work/'host.log').read_text()[-15000:]);raise
    finally:
        proc.terminate()
        try: proc.wait(timeout=10)
        except subprocess.TimeoutExpired: proc.kill();proc.wait()
        output.close();mock.shutdown()
        if not directory: shutil.rmtree(work)

if __name__=='__main__': run()

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
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
PNG = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jvXkAAAAASUVORK5CYII='
IMAGE = 'data:image/png;base64,' + PNG
KEY = 'sk-recorder-integration-client'
MANAGEMENT = 'recorder-integration-management'

class Mock(BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
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
    (work/'logs').mkdir(exist_ok=True)
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
request-log: true
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
plugins:
  enabled: true
  dir: "{plugins}"
  configs:
    request-recorder:
      enabled: true
      db_path: "{work / 'recorder.db'}"
      logs_dir: "{work / 'logs'}"
'''
    (work/'config.yaml').write_text(config)
    def request(path, data=None, key=MANAGEMENT):
        headers={'Content-Type':'application/json'}
        if key is not None: headers['Authorization']='Bearer '+key
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
            data=request('/v1/chat/completions',body,key=KEY)
            if stream: assert b'159,' in data
        records=None
        for _ in range(150):
            records=json.loads(request('/v0/management/plugins/request-recorder/records'))
            if records['total']==2 and all(x['upstream_state']=='log_captured' and x['key_id'] for x in records['items']):break
            time.sleep(.2)
        assert records['total']==2,records
        for item in records['items']:
            assert item['outcome']=='succeeded',item
            assert item['upstream_state']=='log_captured',item
            assert item['key_id']==hashlib.sha256(KEY.encode()).hexdigest(),item
            d=json.loads(request('/v0/management/plugins/request-recorder/record?id='+item['id']))
            assert IMAGE in d['record']['request_body']
            assert IMAGE in d['record']['upstream_requests']
            assert 'API RESPONSE 1' in d['record']['upstream_responses']
            if item['stream']:
                assert '159,' in d['record']['response_body']
                assert item['tokens']['TotalTokens']==260,item
            else:
                assert any(m.get('images') for m in d['conversation'] if m['source']=='request')
                assert any(m.get('images') for m in d['upstream_conversation']),d['upstream_conversation']
        alias=json.loads(request('/v0/management/plugins/request-recorder/aliases',{'key':KEY,'alias':'集成测试'}))
        assert alias['ok']
        page=json.loads(request('/v0/management/plugins/request-recorder/records?alias='+urllib.parse.quote('集成测试')))
        assert page['total']==2
        print(json.dumps({'result':'PASS','records':2,'stream_chunks':160,'checks':['native CPA loading','public GUI registration','management authentication','nonstream and SSE capture','usage join','upstream log enrichment','input and output images','alias filtering'],'workdir':str(work),'port':port},ensure_ascii=False))
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

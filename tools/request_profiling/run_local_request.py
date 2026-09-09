#!/usr/bin/env python3
import argparse,json,pathlib,time,urllib.request,urllib.error,uuid
ap=argparse.ArgumentParser();ap.add_argument('--body-mb',type=int,default=0);ap.add_argument('--repair-retry',action='store_true');args=ap.parse_args();assert 0<=args.body_mb<=40
p=pathlib.Path(__file__).parent/'.runtime';s=json.loads((p/'scenario.json').read_text());rid=str(uuid.uuid4())
body=json.dumps({'model':'gpt-6-astra','input':'Reply local profiling ok. '+('A'*(args.body_mb*1000000)),'stream':True,**({'truncation':'auto'} if args.repair_retry else {})}).encode()
r=urllib.request.Request('http://127.0.0.1:8187/v1/responses',data=body,headers={'Authorization':'Bearer '+s['api_key'],'Content-Type':'application/json','X-Request-ID':rid})
start=time.monotonic()
try:
 response=urllib.request.urlopen(r,timeout=60);data=response.read();result={'http':response.status,'completed':b'response.completed' in data,'content_present':b'local profiling ok' in data,'duration_ms':round((time.monotonic()-start)*1000),'body_bytes':len(body),'request_id_sent':rid};(p/'last-request-result.json').write_text(json.dumps(result));print(json.dumps(result));assert result['completed'] and result['content_present']
except urllib.error.HTTPError as e:
 d=json.load(e);raise RuntimeError(str(e.code)+' '+str(d.get('error',{}).get('message','request failed')))

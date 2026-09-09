#!/usr/bin/env python3
"""Disconnect before output, then inspect the REAL gateway's later profile."""
import http.client,json,pathlib,time,uuid,urllib.request,urllib.parse
p=pathlib.Path(__file__).parent/'.runtime';scenario=json.loads((p/'scenario.json').read_text());rid=str(uuid.uuid4())
conn=http.client.HTTPConnection('127.0.0.1',8187,timeout=.7)
started=time.monotonic();resp=None
try:
 conn.request('POST','/v1/responses',body=json.dumps({'model':'gpt-6-astra','input':'LOCAL_PROFILE_SLOW reply after thinking','stream':True}),headers={'Authorization':'Bearer '+scenario['api_key'],'Content-Type':'application/json','X-Request-ID':rid})
 resp=conn.getresponse()
 while resp.read(1):pass
except (TimeoutError,OSError):pass
finally:
 if resp:resp.close()
 conn.close()
disconnect_ms=round((time.monotonic()-started)*1000)
# Poll this exact request; log ingestion is asynchronous.
url='http://127.0.0.1:8187/api/v1/admin/ops/request-profiles?'+urllib.parse.urlencode({'evidence':'measured','protocol':'sse','request_id':rid})
rows=[]
for _ in range(20):
 time.sleep(.5)
 req=urllib.request.Request(url,headers={'Authorization':'Bearer '+(p/'admin-token').read_text()})
 data=json.load(urllib.request.urlopen(req))['data'];rows=data['rows']
 if rows:break
assert len(rows)==1,'profile did not arrive'
s=rows[0]['profile'];report={'request_id':rid,'caller_disconnect_ms':disconnect_ms,'http_status':rows[0]['status'],'client_outcome':s.get('client_outcome'),'client_disconnected_us':s.get('client_disconnected_us'),'backend_total_us':s['total_us'],'drain_after_disconnect_us':s.get('drain_after_disconnect_us'),'downstream_first_output_us':s.get('downstream_first_output_us'),'downstream_complete_us':s.get('downstream_complete_us')}
(p/'disconnect-acceptance.json').write_text(json.dumps({'report':report,'profile':rows[0]},ensure_ascii=False,indent=2));print(json.dumps(report))
assert report['client_outcome']=='disconnected',report
assert report['client_disconnected_us'] is not None and report['drain_after_disconnect_us']>0,report
assert report['downstream_complete_us'] is None and report['downstream_first_output_us'] is None,report

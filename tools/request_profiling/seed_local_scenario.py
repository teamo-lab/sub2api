#!/usr/bin/env python3
"""Seed isolated QA resources using the local app's real admin API."""
import json,pathlib,urllib.request,urllib.error,subprocess,os
root=pathlib.Path(__file__).parent/'.runtime';token=(root/'admin-token').read_text();base='http://127.0.0.1:8187/api/v1'
def call(path,payload):
 r=urllib.request.Request(base+path,data=json.dumps(payload).encode(),headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'})
 try:return json.load(urllib.request.urlopen(r))['data']
 except urllib.error.HTTPError as e:
  d=json.load(e);raise RuntimeError(str(e.code)+' '+str(d.get('message','request failed')))
state=root/'scenario.json'
if state.exists():print('local scenario already seeded');raise SystemExit
cfg=json.loads((root/'local-env.json').read_text());assert cfg['DATABASE_HOST']=='127.0.0.1' and cfg['DATABASE_DBNAME'].startswith('sub2api_profile_')
# This balance belongs solely to the newly created synthetic local admin.
subprocess.run(['psql','-h','127.0.0.1','-U',cfg['DATABASE_USER'],'-d',cfg['DATABASE_DBNAME'],'-v','ON_ERROR_STOP=1'],input="UPDATE users SET balance=1000,concurrency=10 WHERE email='profiling-local@example.test';",text=True,check=True,stdout=subprocess.DEVNULL,env=dict(os.environ,PGPASSWORD=cfg['DATABASE_PASSWORD']))
g=call('/admin/groups',{'name':'Local Profiling QA','platform':'openai','rate_multiplier':1,'is_exclusive':False,'subscription_type':'standard'})
accounts=[]
for name,priority,route in [('Controlled 503',1,'first'),('Healthy local stream',2,'second')]:
 a=call('/admin/accounts',{'name':name,'platform':'openai','type':'apikey','credentials':{'api_key':'local-fixture-only','base_url':'http://127.0.0.1:9187/'+route,'model_mapping':{'gpt-6-astra':'gpt-6-astra'}},'extra':{'openai_responses_supported':True,'openai_responses_mode':'force_responses'},'concurrency':3,'priority':priority,'group_ids':[g['id']],'upstream_billing_probe_enabled':False});accounts.append(a['id'])
k=call('/keys',{'name':'Local profiling acceptance','group_id':g['id']})
state.write_text(json.dumps({'group_id':g['id'],'account_ids':accounts,'api_key':k.get('key'),'key_id':k['id']}));state.chmod(0o600)
print('Seeded local group',g['id'],'account IDs',accounts,'key ID',k['id'])

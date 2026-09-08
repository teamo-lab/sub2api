#!/usr/bin/env python3
"""Import metadata-only historical reconstructions into an isolated LOCAL QA DB.
Never sends traffic upstream, imports credentials or fabricates missing spans.
"""
import argparse,datetime as dt,json,pathlib,subprocess,os,uuid
ap=argparse.ArgumentParser();ap.add_argument('--source-dir',type=pathlib.Path,required=True);args=ap.parse_args()
rows=json.loads((args.source_dir/'current-phase-reconstruction.json').read_text())
logs=json.loads((args.source_dir/'hk43-paired-body-metadata.json').read_text())
cfg=json.loads((pathlib.Path(__file__).parent/'.runtime/local-env.json').read_text())
assert cfg['DATABASE_HOST']=='127.0.0.1' and cfg['DATABASE_DBNAME'].startswith('sub2api_profile_')
statements=[]
for r in rows:
 cid=str(uuid.UUID(r['request43'].removeprefix('client:')));a=r['raw_timestamps']['hk43'];b=r['raw_timestamps']['hk101']
 complete=next(x for x in logs if x.get('client_request_id')==cid and x.get('component')=='http.access')
 end=dt.datetime.strptime(complete['completed_at'],'%Y-%m-%dT%H:%M:%S.%f%z').timestamp();start=a['entry'];total=round((end-start)*1e6)
 boundaries=[(start,'historical_before_audit'),(a['audit'],'historical_before_forward'),(a['forward'],'historical_43_to_101'),(b['entry'],'historical_101_before_audit'),(b['audit'],'historical_101_before_forward'),(b['forward'],'historical_101_first_output'),(b['semantic'],'historical_first_output_relay'),(a['semantic'],'historical_after_first_output'),(end,'end')]
 if any(boundaries[i+1][0]<boundaries[i][0] for i in range(len(boundaries)-1)):raise ValueError('nonmonotonic historical boundaries')
 spans=[];segments=[]
 for i,((left,name),(right,_)) in enumerate(zip(boundaries,boundaries[1:])):
  lo=round((left-start)*1e6);hi=round((right-start)*1e6)
  spans.append({'id':i+1,'name':name,'start_us':lo,'end_us':hi});segments.append({'name':name,'start_us':lo,'duration_us':hi-lo})
 assert sum(s['duration_us'] for s in segments)==total
 profile={'version':1,'evidence':'historical','total_us':total,'spans':spans,'segments':segments,'events':[{'kind':'first_semantic','at_us':round((a['semantic']-start)*1e6)}],'dropped':0,'attempts':0,'group_id':38,'model':r['model'],'protocol':'sse','body_bytes':r['body43_bytes'],'notes':['原始服务器日志与 usage 计时区间重建；不是函数 profiler','跨机子区间可能受时钟偏差影响','未记录原始 retry/fallback 次数；未导入请求正文或凭据','仅用于本地验收，与当前实测轨迹分开聚合']}
 model_sql=r['model'].replace("'","''")
 extra={'status_code':200,'request_profile':profile};raw=json.dumps(extra,ensure_ascii=False).encode().hex();rid='history:'+cid
 statements.append(f"UPDATE ops_system_logs SET account_id=72 WHERE host='local-profile-history' AND request_id='{rid}'; INSERT INTO ops_system_logs(created_at,host,level,component,message,request_id,client_request_id,account_id,model,extra) SELECT '{complete['completed_at']}'::timestamptz,'local-profile-history','info','http.access','http request completed','{rid}','{cid}',72,'{model_sql}',convert_from(decode('{raw}','hex'),'UTF8')::jsonb WHERE NOT EXISTS(SELECT 1 FROM ops_system_logs WHERE request_id='{rid}' AND host='local-profile-history');")
subprocess.run(['psql','-h','127.0.0.1','-p',cfg['DATABASE_PORT'],'-U',cfg['DATABASE_USER'],'-d',cfg['DATABASE_DBNAME'],'-v','ON_ERROR_STOP=1'],input='BEGIN;\n'+'\n'.join(statements)+'\nCOMMIT;',text=True,check=True,stdout=subprocess.DEVNULL,env=dict(os.environ,PGPASSWORD=cfg['DATABASE_PASSWORD']))
print(f'Imported {len(rows)} historical metadata profiles into isolated local QA database.')

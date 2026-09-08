#!/usr/bin/env python3
"""Loopback-only upstream for real local gateway retry/fallback acceptance."""
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
import json,time
class Handler(BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_POST(self):
  n=int(self.headers.get('Content-Length','0'));payload=json.loads(self.rfile.read(n))
  if self.path.startswith('/first'):
   body=json.dumps({'error':{'type':'server_error','message':'local controlled 503'}}).encode();self.send_response(503);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body);return
  if 'truncation' in payload:
   body=json.dumps({'error':{'code':'unknown_parameter','type':'invalid_request_error','message':"Unknown parameter: 'truncation'.",'param':'truncation'}}).encode();self.send_response(400);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body);return
  self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
  events=[{'type':'response.created','response':{'id':'resp_local_profile','object':'response','status':'in_progress','model':'gpt-6-astra','output':[]}}, {'type':'response.output_text.delta','item_id':'msg_local','output_index':0,'content_index':0,'delta':'local profiling ok'}, {'type':'response.completed','response':{'id':'resp_local_profile','object':'response','status':'completed','model':'gpt-6-astra','output':[{'id':'msg_local','type':'message','role':'assistant','content':[{'type':'output_text','text':'local profiling ok'}]}],'usage':{'input_tokens':10,'output_tokens':4}}}]
  try:
   for e in events:time.sleep(.15);self.wfile.write(('event: '+e['type']+'\ndata: '+json.dumps(e)+'\n\n').encode());self.wfile.flush()
  except (BrokenPipeError,ConnectionResetError):pass
ThreadingHTTPServer(('127.0.0.1',9187),Handler).serve_forever()

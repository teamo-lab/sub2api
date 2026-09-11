"""Verify release safety ordering with fake host tools; never access Docker/DB."""
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('stage-source-release')
HASH = 'a' * 64


class SourceReferenceTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in ['bin', 'rollout/runtime', 'backups']:
            (self.root / name).mkdir(parents=True)
        self.env = dict(os.environ, SUB2API_DEPLOY_ROOT=str(self.root),
                        SUB2API_BACKUP_DIR=str(self.root / 'backups'),
                        PATH=str(self.root / 'bin') + os.pathsep + os.environ['PATH'])
        self.archive = self.root / 'rollout/runtime/incoming-fixture.tar.gz'
        with tarfile.open(self.archive, 'w:gz') as tf:
            data = b'FROM scratch\n'
            item = tarfile.TarInfo('Dockerfile'); item.size = len(data)
            tf.addfile(item, io.BytesIO(data))
        self.executable('bin/sub2api-rollout', '''#!/usr/bin/env python3
import json,os
print(json.dumps({'phase':'rolled_back','candidate_slot':'blue','blue':{'health':'healthy','weight':0,'active_streams':int(os.getenv('TEST_STREAMS','0'))}}))
''')
        self.executable('bin/docker', '''#!/usr/bin/env python3
import sys,os,json
from pathlib import Path
r=Path(os.environ['SUB2API_DEPLOY_ROOT']);a=sys.argv[1:]
with (r/'actions').open('a') as f:f.write(json.dumps(['docker']+a)+'\\n')
if a[:2]==['image','inspect']:print('sha256:'+'b'*64)
elif a[0]=='inspect':print('healthy' if a[-1]=='sub2api-haproxy' else '')
elif a[0]=='build':pass
elif a[0]=='run':print(os.getenv('TEST_IMAGE_HASH','a'*64)+'  /app/sub2api')
elif a[0]=='exec':print(os.getenv('TEST_RUNNING_HASH','a'*64)+'  /app/sub2api')
else:raise SystemExit(2)
''')
        self.executable('bin/install', '''#!/usr/bin/env python3
import pathlib,sys
pathlib.Path(sys.argv[-1]).mkdir(parents=True,exist_ok=True)
if sys.argv[-2].startswith('/'):pathlib.Path(sys.argv[-2]).mkdir(parents=True,exist_ok=True)
''')
        self.executable('bin/df', '#!/bin/sh\nprintf "Filesystem 1024-blocks Used Available Capacity Mounted\\nfake 20000000 100 19999900 1%% /\\n"\n')
        self.executable('bin/awk', '''#!/usr/bin/env python3
import os,sys
if '/proc/meminfo' in sys.argv:print('4194304')
else:os.execv('/usr/bin/awk',['awk']+sys.argv[1:])
''')
        self.executable('rollout/dependency-client', '''#!/usr/bin/env python3
import sys,os,json
from pathlib import Path
r=Path(os.environ['SUB2API_DEPLOY_ROOT'])
with (r/'actions').open('a') as f:f.write(json.dumps(['dependency']+sys.argv[1:])+'\\n')
if os.getenv('TEST_DEPENDENCY_FAIL')==sys.argv[1]:raise SystemExit(1)
if sys.argv[1]=='backup':Path(sys.argv[2]).write_text('fixture backup')
''')
        self.executable('bin/sub2api-validate-nat-entry', '''#!/usr/bin/env python3
import json,os
from pathlib import Path
r=Path(os.environ['SUB2API_DEPLOY_ROOT'])
with (r/'actions').open('a') as f:f.write(json.dumps(['nat_validation'])+'\\n')
raise SystemExit(1 if os.getenv('TEST_NAT_FAIL') else 0)
''')
        self.executable('bin/sub2api-stage-image-release', '''#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
r=Path(os.environ['SUB2API_DEPLOY_ROOT'])
with (r/'actions').open('a') as f:f.write(json.dumps(['stage']+sys.argv[1:])+'\\n')
result={'ok':True,'phase':'staged','candidate_slot':'blue'}
if os.getenv('TEST_EMPTY_STAGE_REFERENCES'):
 result.update(source_commit='',binary_sha256='')
print(json.dumps(result))
''')

    def executable(self, name, text):
        p = self.root / name; p.write_text(text); p.chmod(0o755)

    def run_stage(self, mode='single_host_image', expected=''):
        result = subprocess.run(['bash', str(SCRIPT), 'fixture', 'fixture-version',
                                 'c' * 40, str(self.archive), 'false', '2026-09-08T00:00:00Z',
                                 expected, 'true', 'false', '3', mode],
                                env=self.env, capture_output=True, text=True)
        actions = self.root / 'actions'
        return result, [json.loads(line) for line in actions.read_text().splitlines()] if actions.exists() else []

    def test_single_host_keeps_nat_backup_and_immutable_binary_checks(self):
        result, actions = self.run_stage()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        output = json.loads(result.stdout)
        self.assertEqual(output['binary_reference'], 'single_host_image')
        self.assertEqual(output['binary_sha256'], HASH)
        self.assertEqual(output['source_commit'], 'c' * 40)
        names = [(a[0], a[1] if len(a) > 1 else '') for a in actions]
        ordered = [('dependency','check'), ('nat_validation',''), ('dependency','backup'),
                   ('docker','build'), ('docker','run'), ('stage','fixture'), ('docker','exec')]
        self.assertEqual(sorted(names.index(n) for n in ordered), [names.index(n) for n in ordered])
        reference = next(a for a in actions if a[:2] == ['docker','run'])
        self.assertIn('--read-only', reference)
        self.assertIn('--network', reference)
        self.assertIn('none', reference)
        self.assertIn('sha256sum', reference)

    def test_fleet_mode_still_requires_validated_external_hash(self):
        result, actions = self.run_stage('fleet_binary')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(result.stdout)['error_code'], 'expected_binary_required')
        self.assertEqual(actions, [])

    def test_stage_metadata_cannot_erase_verified_references(self):
        self.env['TEST_EMPTY_STAGE_REFERENCES'] = '1'
        result, _ = self.run_stage()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        output = json.loads(result.stdout)
        self.assertEqual(output['source_commit'], 'c' * 40)
        self.assertEqual(output['binary_sha256'], HASH)
        self.assertEqual(output['stage']['source_commit'], '')
        self.assertEqual(output['stage']['binary_sha256'], '')

    def test_fleet_mismatch_stops_before_slot_changes(self):
        result, actions = self.run_stage('fleet_binary', 'd' * 64)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(result.stdout)['error_code'], 'binary_sha256_mismatch')
        self.assertFalse(any(a[0] == 'stage' for a in actions))

    def test_running_binary_mismatch_is_failure_after_zero_weight_stage(self):
        self.env['TEST_RUNNING_HASH'] = 'd' * 64
        result, actions = self.run_stage()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(result.stdout)['error_code'], 'binary_sha256_mismatch')
        self.assertTrue(any(a[0] == 'stage' for a in actions))
        self.assertFalse(any('canary' in a or 'promote' in a for a in actions))

    def test_nat_failure_does_not_build_or_change_candidate(self):
        self.env['TEST_NAT_FAIL'] = '1'
        result, actions = self.run_stage()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(result.stdout)['error_code'], 'nat_entry_not_ready')
        self.assertFalse(any(a[:2] == ['docker','build'] or a[0] == 'stage' for a in actions))

    def test_live_streams_block_before_backup_or_build(self):
        self.env['TEST_STREAMS'] = '1'
        result, actions = self.run_stage()
        self.assertEqual(json.loads(result.stdout)['error_code'], 'candidate_has_streams')
        self.assertEqual(actions, [])

    def test_backup_failure_blocks_build(self):
        self.env['TEST_DEPENDENCY_FAIL'] = 'backup'
        result, actions = self.run_stage()
        self.assertEqual(json.loads(result.stdout)['error_code'], 'backup_failed')
        self.assertFalse(any(a[:2] == ['docker','build'] or a[0] == 'stage' for a in actions))


if __name__ == '__main__':
    unittest.main()

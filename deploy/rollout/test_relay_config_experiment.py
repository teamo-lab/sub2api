"""Exercise only fake host tools: no Docker daemon, network or database."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("stage-image-release")
DIGEST = "sha256:" + "a" * 64
SOURCE = "c" * 40
BINARY = "d" * 64


class RelayConfigExperimentTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for directory in ("bin", "rollout", "backups"):
            (self.root / directory).mkdir()
        (self.root / "docker-compose.bluegreen.yml").write_text("services: {}\n")
        self.env = dict(os.environ, SUB2API_DEPLOY_ROOT=str(self.root),
                        SUB2API_BACKUP_DIR=str(self.root / "backups"),
                        PATH=str(self.root / "bin") + os.pathsep + os.environ["PATH"])
        self.executable("bin/sub2api-rollout", r'''#!/usr/bin/env python3
import json,os,pathlib,sys
r=pathlib.Path(os.environ['SUB2API_DEPLOY_ROOT']);action=sys.argv[1]
with (r/'actions').open('a') as f:f.write(json.dumps(['controller',action])+'\n')
c=os.getenv('TEST_CANDIDATE','blue');s='green' if c=='blue' else 'blue'
d={'phase':os.getenv('TEST_PHASE','complete'),'stable_slot':s,'candidate_slot':c,
   c:{'health':'healthy','weight':int(os.getenv('TEST_WEIGHT','0')),'active_streams':int(os.getenv('TEST_STREAMS','0'))},
   s:{'health':'healthy','weight':100,'active_streams':17}}
if action=='stage':d.update(phase='staged',action='stage',result='staged')
print(json.dumps(d))
''')
        self.executable("bin/docker", r'''#!/usr/bin/env python3
import json,os,pathlib,sys
r=pathlib.Path(os.environ['SUB2API_DEPLOY_ROOT']);a=sys.argv[1:]
c='sub2api-'+os.getenv('TEST_CANDIDATE','blue');created=(r/'created.json').exists()
with (r/'actions').open('a') as f:f.write(json.dumps(['docker']+a)+'\n')
def values(name):
 v={'SUB2API_DEPLOYMENT_VERSION':os.getenv('TEST_VERSION','test-version'),'SUB2API_RELEASE_ID':'stable-release',
    'SUB2API_DEPLOYMENT_SLOT':name.rsplit('-',1)[-1],'SUB2API_DEPLOYMENT_DIGEST':'sha256:'+'a'*64,
    'GATEWAY_TEAMO_RELAY_ENABLED':os.getenv('TEST_STABLE_ENABLED','false'),
    'GATEWAY_TEAMO_RELAY_GROUP_IDS':os.getenv('TEST_STABLE_GROUPS','3,29'),
    'GATEWAY_OPENAI_FIRST_OUTPUT_TIMEOUT_SECONDS':'30','DATABASE_PASSWORD':'fixture-secret'}
 if name==c and created:
  v.update(json.loads((r/'created.json').read_text()))
  if os.getenv('TEST_EXTRA_DRIFT'):v['GATEWAY_OPENAI_FIRST_OUTPUT_TIMEOUT_SECONDS']='60'
 return v
if a[:2]==['image','inspect']:sys.exit(0)
if a[0]=='run':
 if a[a.index('--entrypoint')+1]=='sha256sum':print(os.getenv('TEST_IMAGE_BINARY','d'*64)+'  /app/sub2api')
 else:print('Sub2API test-version (commit: '+os.getenv('TEST_SOURCE','c'*40)+', built: fixture)')
 sys.exit(0)
if a[0]=='exec':
 print(os.getenv('TEST_CANDIDATE_BINARY' if a[1]==c and created else 'TEST_STABLE_BINARY','d'*64)+'  /app/sub2api');sys.exit(0)
if a[0]=='compose':
 overlays=[a[i+1] for i,x in enumerate(a) if x=='-f']
 overlay=json.loads(pathlib.Path(overlays[-1]).read_text())
 v=overlay['services'][c]['environment'];v['SUB2API_RELEASE_ID']='relay-test'
 (r/'created.json').write_text(json.dumps(v));(r/'up-args.json').write_text(json.dumps(a));sys.exit(0)
name=a[-1]
if '--format' not in a:
 print(json.dumps([{'Config':{'Env':[k+'='+v for k,v in values(name).items()]}}]));sys.exit(0)
fmt=a[a.index('--format')+1]
if 'Config.Env' in fmt:print('\n'.join(k+'='+v for k,v in values(name).items()))
elif fmt=='{{.Image}}':print('sha256:'+'a'*64)
elif 'State.Health.Status' in fmt:print('' if name in ('sub2api','sub2api-haproxy-final') else 'healthy')
elif 'RestartCount' in fmt:print('0')
elif 'IPAddress' in fmt:print('127.0.0.1')
else:sys.exit(2)
''')
        self.executable("rollout/dependency-client", r'''#!/usr/bin/env python3
import json,os,pathlib,sys
r=pathlib.Path(os.environ['SUB2API_DEPLOY_ROOT'])
with (r/'actions').open('a') as f:f.write(json.dumps(['dependency',sys.argv[1]])+'\n')
if os.getenv('TEST_DEPENDENCY_FAIL')==sys.argv[1]:raise SystemExit(1)
if sys.argv[1]=='backup':pathlib.Path(sys.argv[2]).write_text('fixture backup')
''')
        self.executable("bin/sub2api-validate-nat-entry", r'''#!/usr/bin/env python3
import json,os,pathlib
r=pathlib.Path(os.environ['SUB2API_DEPLOY_ROOT'])
with (r/'actions').open('a') as f:f.write(json.dumps(['nat','check'])+'\n')
raise SystemExit(1 if os.getenv('TEST_NAT_FAIL') else 0)
''')
        self.executable("bin/df", '#!/bin/sh\nprintf "Filesystem 1024-blocks Used Available Capacity Mounted\\nfake 20000000 100 19999900 1%% /\\n"\n')
        self.executable("bin/awk", r'''#!/usr/bin/env python3
import os,sys
if '/proc/meminfo' in sys.argv:print('4194304')
else:os.execv('/usr/bin/awk',['awk']+sys.argv[1:])
''')
        self.executable("bin/curl", '#!/bin/sh\nprintf "HTTP/1.1 200 OK\\r\\nX-Sub2API-Deployment-Slot: %s\\r\\n\\r\\n" "${TEST_CANDIDATE:-blue}"\n')

    def executable(self, name, content):
        path = self.root / name
        path.write_text(content)
        path.chmod(0o755)

    def run_stage(self, enabled="true", groups="3,29", mode="relay-off-on", digest=DIGEST):
        result = subprocess.run(["bash", str(SCRIPT), "relay-test", "test-version", digest,
                                 "relay-test", enabled, groups, mode, SOURCE, BINARY, "true"],
                                env=self.env, capture_output=True, text=True)
        path = self.root / "actions"
        actions = [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []
        return result, actions

    def assert_no_slot_write(self, result, actions, code):
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(result.stdout)["error_code"], code)
        self.assertNotIn(["controller", "prepare-stage"], actions)
        self.assertFalse(any(a[:2] == ["docker", "compose"] for a in actions))

    def test_explicit_same_image_off_on_keeps_all_stage_gates(self):
        result, actions = self.run_stage()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        report = json.loads(result.stdout)
        self.assertEqual(report["experiment_mode"], "relay-off-on")
        self.assertEqual(report["source_commit"], SOURCE)
        self.assertEqual(report["binary_sha256"], BINARY)
        self.assertTrue(report["configuration_snapshot"]["environment_fingerprints_match"])
        self.assertNotIn("fixture-secret", result.stdout + result.stderr)
        ordered = [["dependency", "check"], ["nat", "check"], ["dependency", "backup"],
                   ["controller", "prepare-stage"], ["controller", "stage"]]
        self.assertEqual([actions.index(a) for a in ordered], sorted(actions.index(a) for a in ordered))
        for action in actions:
            self.assertNotIn("attest-identical", action)
            self.assertNotIn("promote", action)
            self.assertNotIn("canary", action)
        args = json.loads((self.root / "up-args.json").read_text())
        self.assertEqual(args[-1], "sub2api-blue")
        self.assertIn("--no-deps", args)

    def test_green_candidate_keeps_profile(self):
        self.env["TEST_CANDIDATE"] = "green"
        result, _ = self.run_stage()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        args = json.loads((self.root / "up-args.json").read_text())
        self.assertIn("--profile", args)
        self.assertEqual(args[-1], "sub2api-green")

    def test_same_image_without_explicit_mode_still_rejected(self):
        result, actions = self.run_stage(mode="standard")
        self.assert_no_slot_write(result, actions, "candidate_matches_stable")

    def test_noop_or_already_enabled_is_not_an_experiment(self):
        result, actions = self.run_stage(enabled="false")
        self.assert_no_slot_write(result, actions, "invalid_relay_experiment")
        self.env["TEST_STABLE_ENABLED"] = "true"
        result, actions = self.run_stage()
        self.assert_no_slot_write(result, actions, "experiment_stable_not_off")

    def test_different_groups_rejected(self):
        result, actions = self.run_stage(groups="3,31")
        self.assert_no_slot_write(result, actions, "experiment_groups_mismatch")

    def test_different_version_rejected(self):
        self.env["TEST_VERSION"] = "other-version"
        result, actions = self.run_stage()
        self.assert_no_slot_write(result, actions, "experiment_version_mismatch")

    def test_different_source_rejected(self):
        self.env["TEST_SOURCE"] = "e" * 40
        result, actions = self.run_stage()
        self.assert_no_slot_write(result, actions, "experiment_source_mismatch")

    def test_different_image_rejected(self):
        result, actions = self.run_stage(digest="sha256:" + "b" * 64)
        self.assert_no_slot_write(result, actions, "experiment_image_mismatch")

    def test_stable_binary_mismatch_rejected(self):
        self.env["TEST_STABLE_BINARY"] = "e" * 64
        result, actions = self.run_stage()
        self.assert_no_slot_write(result, actions, "experiment_binary_mismatch")

    def test_nonzero_weight_rejected(self):
        self.env["TEST_WEIGHT"] = "10"
        result, actions = self.run_stage()
        self.assert_no_slot_write(result, actions, "candidate_weight_invalid")

    def test_active_stream_rejected(self):
        self.env["TEST_STREAMS"] = "1"
        result, actions = self.run_stage()
        self.assert_no_slot_write(result, actions, "candidate_has_streams")

    def test_nat_failure_rejected(self):
        self.env["TEST_NAT_FAIL"] = "1"
        result, actions = self.run_stage()
        self.assert_no_slot_write(result, actions, "nat_entry_not_ready")

    def test_backup_failure_rejected(self):
        self.env["TEST_DEPENDENCY_FAIL"] = "backup"
        result, actions = self.run_stage()
        self.assert_no_slot_write(result, actions, "backup_failed")

    def test_candidate_binary_mismatch_cannot_mark_staged(self):
        self.env["TEST_CANDIDATE_BINARY"] = "e" * 64
        result, actions = self.run_stage()
        self.assertEqual(json.loads(result.stdout)["error_code"], "experiment_binary_mismatch")
        self.assertIn(["controller", "prepare-stage"], actions)
        self.assertNotIn(["controller", "stage"], actions)

    def test_unrelated_env_drift_is_reported_for_release_acceptance(self):
        self.env["TEST_EXTRA_DRIFT"] = "1"
        result, _ = self.run_stage()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        report = json.loads(result.stdout)
        self.assertFalse(report["configuration_snapshot"]["environment_fingerprints_match"])


if __name__ == "__main__":
    unittest.main()

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("stage-image-release")
DIGEST = "sha256:" + "b" * 64


class CandidateRelayEnvironmentTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "bin").mkdir()
        (self.root / "rollout").mkdir()
        (self.root / "docker-compose.bluegreen.yml").write_text("services: {}\n")
        self.env = dict(os.environ, SUB2API_DEPLOY_ROOT=str(self.root),
                        PATH=str(self.root / "bin") + os.pathsep + os.environ["PATH"])
        self.write_executable("sub2api-rollout", r'''#!/usr/bin/env python3
import json, os, pathlib, sys
r=pathlib.Path(os.environ['SUB2API_DEPLOY_ROOT'])
with (r/'actions').open('a') as f:f.write(sys.argv[1]+'\n')
candidate=os.environ.get('TEST_CANDIDATE','blue');stable='green' if candidate=='blue' else 'blue'
d={'stable_slot':stable,'candidate_slot':candidate,candidate:{'health':'healthy','weight':0,'active_streams':0},stable:{'health':'healthy','weight':100,'active_streams':0}}
print(json.dumps(d))
''')
        self.write_executable("docker", r'''#!/usr/bin/env python3
import json, os, pathlib, sys
r=pathlib.Path(os.environ['SUB2API_DEPLOY_ROOT']);a=sys.argv[1:];candidate='sub2api-'+os.environ.get('TEST_CANDIDATE','blue')
if a[:2]==['image','inspect']:sys.exit(0)
if a[0]=='compose':
 overlays=[a[i+1] for i,x in enumerate(a) if x=='-f']
 overlay=json.loads(pathlib.Path(overlays[-1]).read_text())
 (r/'overlay.json').write_text(json.dumps(overlay))
 (r/'up-args.json').write_text(json.dumps(a))
 sys.exit(0)
fmt=a[a.index('--format')+1];name=a[-1];created=(r/'overlay.json').exists()
if 'Config.Env' in fmt:
 values={'SUB2API_RELEASE_ID':'old','SUB2API_DEPLOYMENT_VERSION':'old',
         'GATEWAY_TEAMO_RELAY_ENABLED':os.environ.get('TEST_STABLE_ENABLED','false'),
         'GATEWAY_TEAMO_RELAY_GROUP_IDS':os.environ.get('TEST_STABLE_GROUPS','')}
 if name==candidate and created and not os.environ.get('TEST_DROP_CONFIG'):
  values.update(json.loads((r/'overlay.json').read_text())['services'][name]['environment'])
 print('\n'.join(k+'='+v for k,v in values.items()))
elif fmt=='{{.Image}}':print('sha256:'+('b' if name==candidate and created else 'a')*64)
elif 'State.Health.Status' in fmt:print('healthy')
elif 'RestartCount' in fmt:print('0')
elif 'IPAddress' in fmt:print('127.0.0.1')
else:sys.exit(2)
''')
        self.write_executable("curl", "#!/bin/sh\nprintf 'HTTP/1.1 200 OK\\r\\nX-Sub2API-Deployment-Slot: %s\\r\\n\\r\\n' \"${TEST_CANDIDATE:-blue}\"\n")

    def write_executable(self, name, text):
        p = self.root / "bin" / name
        p.write_text(text)
        p.chmod(0o755)

    def run_stage(self, *args):
        return subprocess.run(["bash", str(SCRIPT), "relay-test", "test-version", DIGEST,
                               "relay-test", *args], env=self.env, text=True, capture_output=True)

    @unittest.skipUnless(shutil.which("jq"), "jq is required by the stage helper")
    def test_explicit_profile_only_recreates_idle_candidate(self):
        result = self.run_stage("true", "3,29")
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        overlay = json.loads((self.root / "overlay.json").read_text())
        self.assertEqual(list(overlay["services"]), ["sub2api-blue"])
        self.assertEqual(overlay["services"]["sub2api-blue"]["environment"], {
            "GATEWAY_TEAMO_RELAY_ENABLED": "true", "GATEWAY_TEAMO_RELAY_GROUP_IDS": "3,29", "GATEWAY_REQUEST_PROFILING_ENABLED":"false", "GATEWAY_REQUEST_PROFILING_GROUP_IDS":"", "GATEWAY_REQUEST_PROFILING_RETENTION_HOURS":"6"})
        args = json.loads((self.root / "up-args.json").read_text())
        self.assertEqual(args[-1], "sub2api-blue")
        self.assertIn("--no-deps", args)
        self.assertFalse(list((self.root / "rollout").glob("relay-env.*")))

    @unittest.skipUnless(shutil.which("jq"), "jq is required by the stage helper")
    def test_next_release_inherits_stable_profile(self):
        self.env.update(TEST_STABLE_ENABLED="true", TEST_STABLE_GROUPS="3")
        result = self.run_stage()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        report = json.loads(result.stdout)
        self.assertEqual(report["relay"], {"enabled": True, "group_ids": "3"})

    @unittest.skipUnless(shutil.which("jq"), "jq required")
    def test_profiling_overlay_is_explicit_and_candidate_only(self):
        result=self.run_stage("inherit","inherit","standard","","","false","true","38","6")
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        overlay=json.loads((self.root/"overlay.json").read_text())
        self.assertEqual(list(overlay["services"]),["sub2api-blue"])
        values=overlay["services"]["sub2api-blue"]["environment"]
        self.assertEqual(values["GATEWAY_REQUEST_PROFILING_ENABLED"],"true")
        self.assertEqual(values["GATEWAY_REQUEST_PROFILING_GROUP_IDS"],"38")
        self.assertEqual(values["GATEWAY_TEAMO_RELAY_ENABLED"],"false")

    @unittest.skipUnless(shutil.which("jq"), "jq is required by the stage helper")
    def test_green_candidate_uses_its_own_profile(self):
        self.env["TEST_CANDIDATE"] = "green"
        result = self.run_stage("true", "3")
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        overlay = json.loads((self.root / "overlay.json").read_text())
        self.assertEqual(list(overlay["services"]), ["sub2api-green"])
        args = json.loads((self.root / "up-args.json").read_text())
        self.assertEqual(args[-1], "sub2api-green")
        self.assertIn("--profile", args)

    @unittest.skipUnless(shutil.which("jq"), "jq is required by the stage helper")
    def test_enabling_without_groups_cannot_prepare_stage(self):
        result = self.run_stage("true", "")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("relay_groups_required", result.stdout)
        self.assertNotIn("prepare-stage", (self.root / "actions").read_text().splitlines())

    def test_invalid_profile_rejected_before_controller(self):
        result = self.run_stage("true", "3;touch nope")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / "actions").exists())

    @unittest.skipUnless(shutil.which("jq"), "jq is required by the stage helper")
    def test_missing_readback_never_marks_candidate_staged(self):
        self.env["TEST_DROP_CONFIG"] = "1"
        result = self.run_stage("true", "3")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("relay_config_mismatch", result.stdout)
        actions = (self.root / "actions").read_text().splitlines()
        self.assertIn("prepare-stage", actions)
        self.assertNotIn("stage", actions)


if __name__ == "__main__":
    unittest.main()

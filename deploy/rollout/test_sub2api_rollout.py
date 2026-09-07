import importlib.util
import importlib.machinery
import json
import pathlib
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("sub2api-rollout")
LOADER = importlib.machinery.SourceFileLoader("sub2api_rollout", str(SCRIPT))
SPEC = importlib.util.spec_from_loader(LOADER.name, LOADER)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class RuntimeSocketResolutionTests(unittest.TestCase):
    def setUp(self):
        self.old_rollout_dir = MODULE.ROLLOUT_DIR
        self.old_state_path = MODULE.STATE_PATH
        self.old_configured = MODULE.CONFIGURED_SOCKET_PATH
        self.temp = tempfile.TemporaryDirectory()
        root = pathlib.Path(self.temp.name)
        MODULE.ROLLOUT_DIR = root / "rollout"
        MODULE.ROLLOUT_DIR.mkdir()
        MODULE.STATE_PATH = MODULE.ROLLOUT_DIR / "state.json"
        MODULE.CONFIGURED_SOCKET_PATH = ""

    def tearDown(self):
        MODULE.ROLLOUT_DIR = self.old_rollout_dir
        MODULE.STATE_PATH = self.old_state_path
        MODULE.CONFIGURED_SOCKET_PATH = self.old_configured
        self.temp.cleanup()

    def write_state(self, **overrides):
        state = {"topology_mode": "blue_green", "entry_mode": "bootstrap", **overrides}
        MODULE.STATE_PATH.write_text(json.dumps(state))

    def test_direct_final_entry_ignores_recreated_bootstrap_socket(self):
        self.write_state(entry_mode="direct", entry_proxy_container="sub2api-haproxy-final")
        self.assertEqual(
            MODULE.resolve_haproxy_socket_path(),
            MODULE.ROLLOUT_DIR / "runtime/admin-final.sock",
        )

    def test_bootstrap_entry_uses_bootstrap_socket(self):
        self.write_state(entry_mode="bootstrap", entry_proxy_container="sub2api-haproxy")
        self.assertEqual(
            MODULE.resolve_haproxy_socket_path(),
            MODULE.ROLLOUT_DIR / "runtime/admin.sock",
        )

    def test_explicit_override_wins(self):
        MODULE.CONFIGURED_SOCKET_PATH = "/tmp/custom.sock"
        self.write_state(entry_mode="direct", entry_proxy_container="sub2api-haproxy-final")
        self.assertEqual(MODULE.resolve_haproxy_socket_path(), pathlib.Path("/tmp/custom.sock"))


if __name__ == "__main__":
    unittest.main()

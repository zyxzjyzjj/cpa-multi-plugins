"""Optional real-CPA smoke test: CPA_HOST_EXE + CPA_BUNDLE_DIR enable it."""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


@unittest.skipUnless(os.environ.get("CPA_HOST_EXE") and os.environ.get("CPA_BUNDLE_DIR"),
                     "set CPA_HOST_EXE and CPA_BUNDLE_DIR for real-host bundle validation")
class HostBundleTests(unittest.TestCase):
    def test_every_provider_loads_with_unique_identity(self):
        plugins = json.loads((ROOT / "registry.json").read_text(encoding="utf-8"))["plugins"]
        with tempfile.TemporaryDirectory(prefix="cpa-bundle-check-") as directory:
            root = Path(directory)
            (root / "auth").mkdir()
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            config = (
                f"host: 127.0.0.1\nport: {port}\nauth-dir: {json.dumps(str(root / 'auth'))}\n"
                "api-keys: [fixture-client]\nremote-management:\n  allow-remote: false\n"
                "  secret-key: fixture-admin\n  disable-control-panel: true\n"
                f"plugins:\n  enabled: true\n  dir: {json.dumps(os.environ['CPA_BUNDLE_DIR'])}\n  configs:\n"
            )
            for plugin in plugins:
                config += (f"    {plugin['id']}:\n      enabled: true\n      checkin_auto: false\n"
                           "      lifecycle_auto: false\n      token_keepalive: false\n")
            config_path = root / "config.yaml"
            config_path.write_text(config, encoding="utf-8")
            with (root / "host.log").open("wb") as log:
                process = subprocess.Popen(
                    [os.environ["CPA_HOST_EXE"], "-config", str(config_path)],
                    cwd=root, stdout=log, stderr=log,
                    creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
                try:
                    deadline = time.monotonic() + 25
                    while time.monotonic() < deadline:
                        try:
                            request = urllib.request.Request(
                                f"http://127.0.0.1:{port}/v0/management/plugins",
                                headers={"Authorization": "Bearer fixture-admin"})
                            with urllib.request.urlopen(request, timeout=2) as response:
                                data = json.load(response)
                            break
                        except OSError:
                            if process.poll() is not None:
                                self.fail((root / "host.log").read_text(encoding="utf-8", errors="replace"))
                            time.sleep(.1)
                    else:
                        self.fail("CPA did not start in 25 seconds")
                    loaded = {p["id"]: p for p in data["plugins"]}
                    self.assertEqual(set(loaded), {p["id"] for p in plugins})
                    for plugin in plugins:
                        with self.subTest(provider=plugin["id"]):
                            actual = loaded[plugin["id"]]
                            self.assertTrue(actual["registered"])
                            self.assertTrue(actual["effective_enabled"])
                            self.assertEqual(actual["oauth_provider"], plugin["id"])
                            self.assertEqual(actual["metadata"]["version"], plugin["version"])
                            self.assertEqual(actual["metadata"]["github_repository"], plugin["repository"])
                finally:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)


if __name__ == "__main__":
    unittest.main()

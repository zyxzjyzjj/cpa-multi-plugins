import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

spec = importlib.util.spec_from_file_location("packager", Path(__file__).with_name("package-release.py"))
packager = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packager)


class StorePackageTests(unittest.TestCase):
    def test_registry_provider_identity_and_release_version(self):
        registry = json.loads((packager.ROOT / "registry.json").read_text(encoding="utf-8"))
        version = (packager.ROOT / "VERSION").read_text().strip()
        ids = [p["id"] for p in registry["plugins"]]
        self.assertEqual(len(ids), len(set(ids)), "provider IDs must be unique")
        self.assertEqual(set(ids), {"workbuddy", "qoder", "trae", "zcode", "codearts-provider"})
        for plugin in registry["plugins"]:
            self.assertEqual(plugin["version"], version)
            self.assertEqual(plugin["repository"], "https://github.com/zyxzjyzjj/cpa-multi-plugins")
            self.assertTrue((packager.ROOT / "plugins" / plugin["id"] / "go.mod").is_file())

    def test_store_archives_and_checksums(self):
        plugins = json.loads((packager.ROOT / "registry.json").read_text(encoding="utf-8"))["plugins"]
        for target, ext in [("linux", "so"), ("windows", "dll"), ("darwin", "dylib")]:
            with self.subTest(target=target), tempfile.TemporaryDirectory() as directory:
                build = Path(directory) / "build"
                output = Path(directory) / "out"
                build.mkdir()
                for plugin in plugins:
                    (build / f"{plugin['id']}.{ext}").write_bytes(b"fixture library")
                archives = packager.package("0.12.86", target, "amd64", build, output)
                self.assertEqual(len(archives), len(plugins) + 1)
                for plugin, archive in zip(plugins, archives):
                    with zipfile.ZipFile(archive) as bundle:
                        self.assertEqual(bundle.namelist(), [f"{plugin['id']}.{ext}"])
                for line in (output / f"checksums-{target}-amd64.txt").read_text().splitlines():
                    digest, filename = line.split()
                    self.assertEqual(digest, hashlib.sha256((output / filename).read_bytes()).hexdigest())

    def test_missing_library_fails_before_packaging(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaises(FileNotFoundError):
                packager.package("0.12.86", "windows", "amd64", root, root / "out")
            self.assertFalse((root / "out").exists())


if __name__ == "__main__":
    unittest.main()

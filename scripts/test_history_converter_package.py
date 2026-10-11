import hashlib
import importlib.util
import json
from pathlib import Path
import tarfile
import tempfile
import unittest


spec = importlib.util.spec_from_file_location("converter_package", Path(__file__).with_name("package-history-converter.py"))
packager = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packager)


class ConverterPackageTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.binary = self.bin / "ftw-history-migrate"
        self.binary.write_bytes(b"\x7fELF\x02\x01" + bytes(12) + (183).to_bytes(2, "little") + b"fixture")
        for name, path in packager.RESOURCES.items():
            resource = self.root / path
            resource.parent.mkdir(parents=True, exist_ok=True)
            resource.write_text(name)
        (self.root / "state-schema.json").write_text('{"version":7}')
        self.tag, self.commit = "v0.140.9-beta.1", "a" * 40
        self.receipt = self.root / "native.json"
        self.native = {"schema": 1, "channel": "beta", "tag": self.tag, "commit": self.commit, "state_schema": 7}
        self.receipt.write_text(json.dumps(self.native))

    def build(self, arch="arm64"):
        return packager.package(self.root, self.bin, self.root / "out", arch, self.tag, self.commit, self.receipt)

    def test_archive_binds_binary_to_native_beta_and_includes_licenses(self):
        archive = self.build()
        first = archive.read_bytes()
        with tarfile.open(archive) as tar:
            manifest = json.load(tar.extractfile("history-converter.json"))
            self.assertEqual(manifest["source_commit"], self.commit)
            self.assertEqual(manifest["source_beta"], self.tag)
            self.assertEqual(manifest["binary_sha256"], hashlib.sha256(self.binary.read_bytes()).hexdigest())
            self.assertEqual(tar.getmember("ftw-history-migrate").mode, 0o755)
            for name in packager.RESOURCES:
                self.assertIsNotNone(tar.extractfile(name))
        self.binary.touch()
        self.assertEqual(first, self.build().read_bytes())
        self.assertTrue(archive.with_name(archive.name + ".sha256").read_text().startswith(hashlib.sha256(first).hexdigest() + "  "))

    def test_wrong_source_channel_tag_commit_or_schema_cannot_package(self):
        for key, value in [("channel", "stable"), ("tag", "v0.140.8-beta.1"), ("commit", "b" * 40), ("state_schema", 8)]:
            with self.subTest(key=key):
                self.receipt.write_text(json.dumps({**self.native, key: value}))
                with self.assertRaises(ValueError):
                    self.build()

    def test_wrong_architecture_and_linked_binary_cannot_package(self):
        with self.assertRaises(ValueError):
            self.build("amd64")
        target = self.root / "other"
        self.binary.rename(target)
        self.binary.symlink_to(target)
        with self.assertRaises(ValueError):
            self.build()


if __name__ == "__main__":
    unittest.main()

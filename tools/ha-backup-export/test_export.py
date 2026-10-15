import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from securetar import SecureTarArchive
import ftw_ha_export as tool


class ExportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.backup = self.root / "backup.tar"
        self.destination = self.root / "export"
        self.key = "synthetic-test-key"
        self.files = {"data/config.yaml": b"site: {name: test}\n",
                      "data/state.db": b"synthetic database bytes",
                      "data/nova.key": b"synthetic site key",
                      "data/driver-repository/cache/proof.lua": b"-- managed driver bytes\n"}

    def tearDown(self):
        self.temp.cleanup()

    def make_backup(self, extra=(), protected=False, version=3, compressed=True, metadata="backup.json", apps=None):
        apps = apps or [{"slug": "test_ftw-beta", "name": "FTW (beta)", "version": "3.8.0-beta.1"}]
        info = {"addons": apps, "protected": protected, "compressed": compressed}
        with SecureTarArchive(self.backup, "w", password=self.key if protected else None, create_version=version) as outer:
            with outer.create_tar("./test_ftw-beta.tar.gz" if compressed else "./test_ftw-beta.tar", gzip=compressed) as inner:
                files = {"./addon.json": json.dumps({"version": "3.8.0-beta.1"}).encode(), **self.files}
                for name, content in files.items():
                    member = tarfile.TarInfo(name)
                    member.size = len(content)
                    member.mode = 0o640
                    inner.addfile(member, io.BytesIO(content))
                for member, content in extra:
                    inner.addfile(member, io.BytesIO(content) if content is not None else None)
            data = json.dumps(info).encode()
            member = tarfile.TarInfo("./" + metadata)
            member.size = len(data)
            outer.tar.addfile(member, io.BytesIO(data))

    def link(self, name, target, hard=False):
        member = tarfile.TarInfo(name)
        member.type = tarfile.LNKTYPE if hard else tarfile.SYMTYPE
        member.linkname = target
        member.mode = 0o640
        return member, None

    def assert_stopped(self):
        with self.assertRaises(Exception):
            tool.export_backup(self.backup, self.destination, password=self.key)
        self.assertFalse(self.destination.exists())
        self.assertFalse(list(self.root.glob(".ftw-ha-export-*")))

    def test_real_plain_encrypted_v2_v3_and_legacy_metadata_preserve_files(self):
        for protected, version, compressed, metadata in [(False, 3, True, "backup.json"),
                                                       (False, 2, False, "snapshot.json"),
                                                       ("legacy protected", 2, True, "snapshot.json"),
                                                       (True, 2, True, "backup.json"),
                                                       (True, 3, True, "backup.json")]:
            with self.subTest(protected=protected, version=version):
                self.make_backup(protected=protected, version=version, compressed=compressed, metadata=metadata)
                digest = hashlib.sha256(self.backup.read_bytes()).hexdigest()
                result = tool.export_backup(self.backup, self.destination, password=self.key, expected_sha=digest)
                for name, content in self.files.items():
                    self.assertEqual((self.destination / name).read_bytes(), content)
                    self.assertEqual((self.destination / name).stat().st_mode & 0o777, 0o640)
                self.assertEqual(hashlib.sha256(self.backup.read_bytes()).hexdigest(), digest)
                self.assertEqual(self.destination.stat().st_mode & 0o777, 0o700)
                self.assertEqual(result["source_version"], "3.8.0-beta.1")
                receipt = json.loads((self.destination / "ftw-export.json").read_text())
                self.assertEqual(receipt["backup_sha256"], digest)
                import shutil
                shutil.rmtree(self.destination)

    def test_managed_absolute_and_relative_links_and_hardlinks(self):
        self.make_backup(extra=[self.link("data/drivers/proof.lua", "/data/driver-repository/cache/proof.lua"),
                                self.link("data/drivers/alias.lua", "proof.lua"),
                                self.link("data/driver-copy.lua", "data/driver-repository/cache/proof.lua", hard=True)])
        tool.export_backup(self.backup, self.destination)
        data = self.destination / "data"
        self.assertTrue((data / "drivers/proof.lua").is_symlink())
        self.assertEqual((data / "drivers/alias.lua").read_bytes(), self.files["data/driver-repository/cache/proof.lua"])
        self.assertEqual((data / "driver-copy.lua").read_bytes(), self.files["data/driver-repository/cache/proof.lua"])
        self.assertFalse((data / "driver-copy.lua").is_symlink())
        self.assertFalse((data / "drivers/proof.lua").readlink().is_absolute())

    def test_internal_directory_link(self):
        self.make_backup(extra=[self.link("data/cache-alias", "driver-repository/cache")])
        tool.export_backup(self.backup, self.destination)
        self.assertEqual((self.destination / "data/cache-alias/proof.lua").read_bytes(), self.files["data/driver-repository/cache/proof.lua"])

    def test_parent_after_directory_link_uses_its_actual_target(self):
        self.files["data/shared/child/keep"] = b"directory exists"
        self.files["data/shared/parent.lua"] = b"correct file"
        self.files["data/drivers/parent.lua"] = b"wrong lexical parent"
        self.make_backup(extra=[self.link("data/drivers/subdir", "../shared/child"),
                                self.link("data/drivers/alias.lua", "subdir/../parent.lua")])
        tool.export_backup(self.backup, self.destination)
        self.assertEqual((self.destination / "data/drivers/alias.lua").read_bytes(), b"correct file")

    def test_external_foreign_missing_and_cyclic_links_stop(self):
        for links in [[self.link("data/bad", "/etc/passwd")],
                      [self.link("data/bad", "../../etc/passwd")],
                      [self.link("data/bad", "C:\\private\\key")],
                      [self.link("data/bad", "missing")],
                      [self.link("data/a", "b"), self.link("data/b", "a")],
                      [self.link("data/bad", "addon.json", hard=True)]]:
            with self.subTest(links=[m.linkname for m, _ in links]):
                self.make_backup(extra=links)
                self.assert_stopped()

    def test_files_below_links_stop_in_either_tar_order(self):
        member = tarfile.TarInfo("data/alias/file")
        member.size = 1
        items = [self.link("data/alias", "driver-repository/cache"), (member, b"x")]
        for extra in [items, list(reversed(items))]:
            self.make_backup(extra=extra)
            self.assert_stopped()

    def test_duplicate_and_traversal_and_device_members_stop(self):
        for name, kind in [("data/state.db", tarfile.REGTYPE), ("../escape", tarfile.REGTYPE),
                           ("/outside", tarfile.REGTYPE), ("data/device", tarfile.CHRTYPE),
                           ("data/pipe", tarfile.FIFOTYPE)]:
            member = tarfile.TarInfo(name)
            member.type = kind
            member.size = 1 if kind == tarfile.REGTYPE else 0
            self.make_backup(extra=[(member, b"x" if member.size else None)])
            self.assert_stopped()
        self.assertFalse((self.root / "escape").exists())

    def test_existing_destination_and_concurrent_publication_keep_data(self):
        self.make_backup()
        self.destination.mkdir()
        (self.destination / "keep").write_bytes(b"original")
        with self.assertRaises(tool.ExportError):
            tool.export_backup(self.backup, self.destination)
        self.assertEqual((self.destination / "keep").read_bytes(), b"original")
        self.destination.rename(self.root / "old")
        original_publish = tool.publish

        def compete(staging, destination):
            destination.mkdir()
            (destination / "keep").write_bytes(b"concurrent")
            original_publish(staging, destination)

        with patch.object(tool, "publish", compete):
            with self.assertRaises(tool.ExportError):
                tool.export_backup(self.backup, self.destination)
        self.assertEqual((self.destination / "keep").read_bytes(), b"concurrent")
        self.assertFalse(list(self.root.glob(".ftw-ha-export-*")))

    def test_wrong_key_and_truncated_protected_stream_stop(self):
        self.make_backup(protected=True)
        with self.assertRaises(Exception):
            tool.export_backup(self.backup, self.destination, password="wrong key")
        self.assertFalse(self.destination.exists())
        with tarfile.open(self.backup) as archive:
            found = tool.members(archive)
            metadata = archive.extractfile(found["backup.json"]).read()
            payload = archive.extractfile(found["test_ftw-beta.tar.gz"]).read()
        with tarfile.open(self.backup, "w") as archive:
            for name, data in [("backup.json", metadata), ("test_ftw-beta.tar.gz", payload[:-64])]:
                member = tarfile.TarInfo(name)
                member.size = len(data)
                archive.addfile(member, io.BytesIO(data))
        self.assert_stopped()

    def test_backup_mutation_before_publication_stops(self):
        self.make_backup()
        original_unpack = tool.unpack

        def changed(archive, staging):
            result = original_unpack(archive, staging)
            with self.backup.open("ab") as stream:
                stream.write(b"changed")
            return result

        with patch.object(tool, "unpack", changed):
            self.assert_stopped()

    def test_ambiguous_app_requires_slug_and_incomplete_layout_stops(self):
        apps = [{"slug": "test_ftw-beta", "name": "FTW (beta)", "version": "3.8.0-beta.1"},
                {"slug": "other_ftw", "name": "FTW", "version": "3.8.0-beta.1"}]
        self.make_backup(apps=apps)
        self.assert_stopped()
        tool.export_backup(self.backup, self.destination, slug="test_ftw-beta")
        import shutil
        shutil.rmtree(self.destination)
        del self.files["data/state.db"]
        self.make_backup()
        self.assert_stopped()


if __name__ == "__main__":
    unittest.main()

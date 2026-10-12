from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parent / "create-release-tag.sh"


class TagTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.remote, self.work = self.root / "remote.git", self.root / "work"
        self.run_git(["init", "--bare", str(self.remote)], self.root)
        self.run_git(["init", str(self.work)], self.root)
        self.run_git(["config", "user.name", "fixture"], self.work)
        self.run_git(["config", "user.email", "fixture@example.invalid"], self.work)
        (self.work / "data").write_text("first")
        self.run_git(["add", "data"], self.work)
        self.run_git(["commit", "-m", "first"], self.work)
        self.first = self.run_git(["rev-parse", "HEAD"], self.work)
        (self.work / "data").write_text("second")
        self.run_git(["commit", "-am", "second"], self.work)
        self.second = self.run_git(["rev-parse", "HEAD"], self.work)
        self.run_git(["remote", "add", "origin", str(self.remote)], self.work)

    def tearDown(self):
        self.temp.cleanup()

    def run_git(self, args, cwd):
        return subprocess.check_output(["git", *args], cwd=cwd, stderr=subprocess.DEVNULL, text=True).strip()

    def create(self):
        return subprocess.run(["bash", str(SCRIPT), "ha-export-v1-beta.1", self.second], cwd=self.work, capture_output=True, text=True)

    def test_new_tag_binds_exact_commit(self):
        self.assertEqual(self.create().returncode, 0)
        self.assertEqual(self.run_git(["rev-parse", "refs/tags/ha-export-v1-beta.1"], self.remote), self.second)

    def test_existing_tag_on_another_commit_is_unchanged(self):
        self.run_git(["push", "origin", self.first + ":refs/tags/ha-export-v1-beta.1"], self.work)
        self.assertNotEqual(self.create().returncode, 0)
        self.assertEqual(self.run_git(["rev-parse", "refs/tags/ha-export-v1-beta.1"], self.remote), self.first)

    def test_existing_matching_tag_is_also_refused(self):
        self.assertEqual(self.create().returncode, 0)
        self.assertNotEqual(self.create().returncode, 0)


if __name__ == "__main__":
    unittest.main()

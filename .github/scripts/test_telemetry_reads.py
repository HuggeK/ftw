import datetime as dt
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("check-telemetry-reads.sh")
HEADER = "package site\ntype Reading struct { RawW, SmoothedW float64 }\n"


class TelemetryReads(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git("init", "-q")
        for key, value in [("user.name", "Fixture"), ("user.email", "fixture@example.invalid"),
                           ("core.hooksPath", "/dev/null"), ("commit.gpgsign", "false")]:
            self.git("config", key, value)
        self.write("go/site.go", HEADER + "func power(r Reading) float64 { return r.RawW }\n")
        self.commit()
        self.base = self.git("rev-parse", "HEAD").strip()

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root, text=True)

    def write(self, path, text):
        path = self.root / path
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def commit(self):
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")

    def check(self, expected, base=None):
        result = subprocess.run(["bash", str(SCRIPT), base or self.base], cwd=self.root, text=True, capture_output=True)
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        return result.stdout + result.stderr

    def test_unchanged(self):
        self.assertIn("1, none added", self.check(0))

    def test_second_field_on_same_line(self):
        self.write("go/site.go", HEADER + "func power(r Reading) float64 { return r.RawW + r.SmoothedW }\n")
        self.commit()
        self.assertIn("1 -> 2", self.check(1))

    def test_second_call_on_same_line(self):
        self.write("go/site.go", "package site\nfunc f(s Store) { s.ReadingsByType() }\n")
        self.commit()
        base = self.git("rev-parse", "HEAD").strip()
        self.write("go/site.go", "package site\nfunc f(s Store) { s.ReadingsByType(s.ReadingsByDriver()) }\n")
        self.commit()
        self.assertIn("1 -> 2", self.check(1, base))

    def test_new_file(self):
        self.write("go/other.go", HEADER + "func extra(r Reading) float64 { return r.SmoothedW }\n")
        self.commit()
        self.assertIn("0 -> 1", self.check(1))

    def test_exclusions(self):
        for path in ["go/internal/telemetry/store.go", "go/site_test.go"]:
            self.write(path, HEADER + "func extra(r Reading) float64 { return r.SmoothedW }\n")
        self.commit()
        self.check(0)

    def test_valid_exception(self):
        expiry = dt.datetime.now(dt.timezone.utc).date() + dt.timedelta(days=30)
        self.write("go/other.go", HEADER + f"func f(r Reading) float64 {{ return r.RawW }} // telemetry-read-ok: diagnostics; expires {expiry}; approved-by @frahlg\n")
        self.commit()
        self.check(0)

    def test_invalid_exceptions(self):
        for suffix in ["expires 2000-01-01", "reason; expires 2000-01-01; approved-by owner",
                       "; expires 2999-01-01; approved-by owner", "reason; expires 2999-01-01",
                       "reason; expires 2999-02-30; approved-by owner"]:
            with self.subTest(suffix=suffix):
                self.write("go/other.go", HEADER + f"func f(r Reading) float64 {{ return r.RawW }} // telemetry-read-ok: {suffix}\n")
                self.commit()
                self.assertIn("invalid or expired", self.check(1))

    def test_git_errors_fail(self):
        self.check(1, "missing-ref")


if __name__ == "__main__":
    unittest.main()

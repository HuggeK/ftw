"""Ratchet cached telemetry reads; exceptions need a reason, owner and expiry."""
import collections
import datetime as dt
import os
import re
import subprocess
import sys

# git uses the host's regex library. Keep this POSIX ERE portable to Apple Git.
GIT_PATTERN = r"\.(RawW|SmoothedW)([^[:alnum:]_]|$)|\.(ReadingsByType|ReadingsByDriver)[[:space:]]*\("
READ = re.compile(r"\.(?:RawW|SmoothedW)\b|\.(?:ReadingsByType|ReadingsByDriver)\s*\(")
EXCEPTION = re.compile(
    r"//\s*telemetry-read-ok:\s*(?P<reason>[^;]+);\s*"
    r"expires (?P<date>\d{4}-\d{2}-\d{2});\s*approved-by (?P<owner>@?[\w.-]+)\s*$"
)


def git(*args):
    return subprocess.run(["git", *args], text=True, capture_output=True, check=True).stdout.strip()


def count(ref, today, validate):
    result = subprocess.run(
        ["git", "grep", "-n", "-E", GIT_PATTERN, ref, "--", "go/*.go",
         ":!go/internal/telemetry/", ":!*_test.go"], text=True, capture_output=True,
    )
    if result.returncode not in (0, 1):
        raise RuntimeError(result.stderr.strip() or "git grep failed")
    counts = collections.Counter()
    for line in result.stdout.splitlines():
        _, path, number, source = line.split(":", 3)
        matches = READ.findall(source)
        if "telemetry-read-ok:" in source:
            exception = EXCEPTION.search(source)
            valid = False
            if exception and exception["reason"].strip():
                try:
                    valid = dt.date.fromisoformat(exception["date"]) >= today
                except ValueError:
                    pass
            if valid:
                continue
            if validate:
                raise ValueError(
                    f"{path}:{number}: invalid or expired telemetry exception; use "
                    "// telemetry-read-ok: <reason>; expires YYYY-MM-DD; approved-by <name>"
                )
        counts[path] += len(matches)
    return counts


def main():
    os.chdir(git("rev-parse", "--show-toplevel"))
    base = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("TELEMETRY_READS_BASE_SHA") or "origin/master"
    base = git("merge-base", base, "HEAD")
    today = dt.datetime.now(dt.timezone.utc).date()
    before, after = count(base, today, False), count("HEAD", today, True)
    grew = [path for path in sorted(after) if after[path] > before[path]]
    if grew:
        print("This change adds direct reads of cached telemetry:", file=sys.stderr)
        for path in grew:
            print(f"  {path}: {before[path]} -> {after[path]}", file=sys.stderr)
        print(
            "A cached reading survives its driver going stale, offline or faulted.\n"
            "Use a health-gated accessor in go/internal/telemetry (existing examples:\n"
            "Store.SumOnlineEVW and Store.SumOnlineV2XW). Put the health check in the\n"
            "accessor so each caller does not have to repeat it. Exceptions require\n"
            "a reason, an expiry date and approved-by on the same line; the named\n"
            "owner must review the exception in the PR.", file=sys.stderr,
        )
        return 1
    print(f"telemetry read occurrences outside go/internal/telemetry: {sum(after.values())}, none added")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, RuntimeError, subprocess.CalledProcessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)

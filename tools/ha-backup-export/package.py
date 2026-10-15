"""Package the tested offline tool; this does not change native artifacts."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

source = Path(__file__).resolve().parent
repo = source.parents[1]
commit, tag = os.environ["COMMIT"], os.environ["TAG"]
if not re.fullmatch(r"[0-9a-f]{40}", commit) or not re.fullmatch(r"ha-export-v[1-9][0-9]*-beta\.[1-9][0-9]*", tag):
    raise ValueError("Invalid exact source or separate tool tag")
actual = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=repo, text=True).strip()
if actual != commit or subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=no"], cwd=repo, text=True):
    raise ValueError("Package needs the exact clean tested commit")
version = re.search(r'^TOOL_VERSION = "([0-9]+)"$', (source / "ftw_ha_export.py").read_text(), re.M).group(1)
if not tag.startswith("ha-export-v" + version + "-beta."):
    raise ValueError("Tool tag does not match the source version")
output = repo / "bin/ha-export"
output.mkdir(parents=True, exist_ok=True)
archive = output / (tag + ".tar.gz")
if archive.exists():
    raise ValueError("Refusing to replace a package")
with tempfile.TemporaryDirectory() as directory:
    receipt = Path(directory) / "source.json"
    files = {name: hashlib.sha256((source / name).read_bytes()).hexdigest()
             for name in ["ftw_ha_export.py", "requirements.txt", "README.md"]}
    receipt.write_text(json.dumps({"tag": tag, "commit": commit, "tool_version": version,
                                 "python_minimum": "3.11", "platform": "Linux",
                                 "files": files}, indent=2) + "\n")
    with tarfile.open(archive, "w:gz") as package:
        for name in files:
            package.add(source / name, arcname=name)
        package.add(receipt, arcname="source.json")
        package.add(repo / "LICENSE", arcname="LICENSE")
(output / "checksums.txt").write_text(hashlib.sha256(archive.read_bytes()).hexdigest() + "  " + archive.name + "\n")

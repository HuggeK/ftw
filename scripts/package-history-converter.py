#!/usr/bin/env python3
"""Package the separate converter against a published native beta receipt."""

import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import re
import tarfile


MACHINES = {"amd64": 62, "arm64": 183}
RESOURCES = {
    "LICENSE": "LICENSE",
    "NOTICE": "NOTICE",
    "THIRD-PARTY-NOTICES.txt": "go/tools/history-migrate/THIRD-PARTY-NOTICES.txt",
}


def sha256(source):
    digest = hashlib.sha256()
    for chunk in iter(lambda: source.read(1 << 20), b""):
        digest.update(chunk)
    return digest.hexdigest()


def package(root, binaries, output, arch, tag, commit, receipt):
    if not re.fullmatch(r"v0\.\d+\.\d+-beta\.\d+", tag):
        raise ValueError("converter requires a native beta tag")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("converter requires an exact source commit")
    native = json.loads(receipt.read_text())
    schema = json.loads((root / "state-schema.json").read_text())["version"]
    if (native.get("schema") != 1 or native.get("channel") != "beta"
            or native.get("tag") != tag or native.get("commit") != commit
            or native.get("state_schema") != schema):
        raise ValueError("converter source does not match the native beta receipt")
    binary = binaries / "ftw-history-migrate"
    if binary.is_symlink():
        raise ValueError("converter binary must not be a link")
    with binary.open("rb") as source:
        header = source.read(20)
        source.seek(0)
        digest = sha256(source)
    if (len(header) != 20 or header[:6] != b"\x7fELF\x02\x01"
            or int.from_bytes(header[18:20], "little") != MACHINES[arch]):
        raise ValueError("converter binary has the wrong Linux architecture")
    manifest = {
        "schema": 1, "source_beta": tag, "source_commit": commit,
        "state_schema": schema, "platform": "linux/" + arch,
        "binary_sha256": digest,
        "source_repository": "https://github.com/srcfl/ftw",
        "runtime": "glibc >= 2.34 and libstdc++",
    }
    output.mkdir(parents=True, exist_ok=True)
    archive = output / f"ftw-history-migrate-{tag}-linux-{arch}.tar.gz"
    with archive.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as zipped:
            with tarfile.open(fileobj=zipped, mode="w", format=tarfile.PAX_FORMAT) as tar:
                for name, path in {"ftw-history-migrate": binary, **{
                        name: root / path for name, path in RESOURCES.items()}}.items():
                    if path.is_symlink() or not path.is_file():
                        raise ValueError(f"missing or linked converter resource: {name}")
                    info = tarfile.TarInfo(name)
                    info.size = path.stat().st_size
                    info.mode = 0o755 if name == "ftw-history-migrate" else 0o644
                    with path.open("rb") as resource:
                        tar.addfile(info, resource)
                data = (json.dumps(manifest, sort_keys=True, indent=2) + "\n").encode()
                info = tarfile.TarInfo("history-converter.json")
                info.size, info.mode = len(data), 0o644
                tar.addfile(info, io.BytesIO(data))
    with archive.open("rb") as source:
        digest = sha256(source)
    archive.with_name(archive.name + ".sha256").write_text(f"{digest}  {archive.name}\n")
    return archive


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("arch", choices=MACHINES)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parent.parent)
    parser.add_argument("--binaries", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--receipt", type=Path, required=True)
    args = parser.parse_args()
    print(package(args.root, args.binaries, args.output, args.arch, args.tag, args.commit, args.receipt))

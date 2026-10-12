#!/usr/bin/env python3
"""Export one FTW app from a Home Assistant backup without starting Core."""

import argparse
import ctypes
import errno
import getpass
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import sys
import tarfile
import tempfile
import time

TOOL_VERSION = "1"
SECURETAR_VERSION = "2026.10.0"


class ExportError(ValueError):
    pass


def archive_path(name):
    if not name or "\\" in name or re.match(r"^[A-Za-z]:", name):
        raise ExportError("Archive has a foreign or empty path")
    path = PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts:
        raise ExportError("Archive path leaves the backup")
    return path.as_posix()


def members(archive):
    found = {}
    for member in archive:
        name = archive_path(member.name)
        if name in found:
            if member.isdir() and found[name].isdir():
                continue
            raise ExportError("Archive has duplicate file names")
        found[name] = member
    return found


def small_json(archive, member):
    if not member.isfile() or member.size > 2 * 1024 * 1024:
        raise ExportError("Invalid backup metadata")
    with archive.extractfile(member) as stream:
        result = json.load(stream)
    if not isinstance(result, dict):
        raise ExportError("Invalid backup metadata")
    return result


def backup_metadata(archive, entries):
    names = [name for name in ("backup.json", "snapshot.json") if name in entries]
    if len(names) != 1:
        raise ExportError("Missing or ambiguous Home Assistant backup metadata")
    info = small_json(archive, entries[names[0]])
    if not isinstance(info.get("addons"), list):
        raise ExportError("Backup does not list its apps")
    # Supervisor's legacy v1_protected treats string values as protected.
    # The string is metadata, never the user's key.
    if isinstance(info.get("protected"), str):
        info["protected"] = True
    if not isinstance(info.get("compressed", True), bool) or not isinstance(info.get("protected", False), bool):
        raise ExportError("Invalid backup compression or encryption flag")
    return info


def select_app(info, slug):
    apps = info["addons"]
    if slug is None:
        apps = [a for a in apps if isinstance(a, dict) and re.match(r"^FTW\b", a.get("name", ""), re.I)]
    else:
        apps = [a for a in apps if isinstance(a, dict) and a.get("slug") == slug]
    if len(apps) != 1:
        raise ExportError("Select one FTW app with --slug; use --list to see the backup's apps")
    app = apps[0]
    if not re.fullmatch(r"[A-Za-z0-9_-]+", app.get("slug", "")) or not isinstance(app.get("version"), str):
        raise ExportError("Invalid app slug or version")
    return app


def link_path(name, member):
    target = member.linkname
    if not target or "\\" in target or re.match(r"^[A-Za-z]:", target):
        raise ExportError("Link has a foreign or empty target")
    if target.startswith("/"):
        if member.islnk() or not target.startswith("/data/"):
            raise ExportError("Link leaves the FTW data directory")
        parts = PurePosixPath(target[1:]).parts
    elif member.islnk():
        parts = PurePosixPath(target).parts
    else:
        parts = (PurePosixPath(name).parent / target).parts
    if not parts or parts[0] != "data":
        raise ExportError("Link leaves the FTW data directory")
    # Preserve '..' until link expansion: a directory link can change which
    # parent a later '..' names. Lexical cleanup can select the wrong file.
    return "/".join(parts)


def resolve_link(path, links, entries):
    seen, resolved = set(), []
    pending = list(PurePosixPath(path).parts)
    expansions = 0
    while pending:
        state = (tuple(resolved), tuple(pending))
        if state in seen:
            raise ExportError("Archive has a link cycle or an excessive link chain")
        seen.add(state)
        part = pending.pop(0)
        if part == "..":
            if len(resolved) <= 1:
                raise ExportError("Link leaves the FTW data directory")
            resolved.pop()
            continue
        resolved.append(part)
        prefix = "/".join(resolved)
        if prefix in links:
            expansions += 1
            if expansions > 64:
                raise ExportError("Archive has an excessive link chain")
            pending = [*PurePosixPath(links[prefix]).parts, *pending]
            resolved = []
        elif pending and entries.get(prefix, {}).get("kind") != "directory":
            raise ExportError("Archive link crosses a missing directory or file")
    result = "/".join(resolved)
    if result not in entries:
        raise ExportError("Archive link target is missing")
    return result


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def publish(staging, destination):
    # Linux renameat2 refuses even an empty existing destination. No overwrite
    # fallback: a concurrent export must not replace another owner's files.
    libc = ctypes.CDLL(None, use_errno=True)
    rename = getattr(libc, "renameat2", None)
    if rename is None:
        raise ExportError("This host lacks renameat2; export on Linux")
    rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    rename.restype = ctypes.c_int
    if rename(-100, os.fsencode(staging), -100, os.fsencode(destination), 1):
        code = ctypes.get_errno()
        if code == errno.EEXIST:
            raise ExportError("Destination exists; keep its data and choose a new directory")
        raise OSError(code, os.strerror(code))
    sync_directory(destination.parent)


def unpack(archive, staging):
    entries, links, hashes = {}, {}, {}
    byte_count, last_progress = 0, time.monotonic()
    for member in archive:
        name = archive_path(member.name)
        if name in entries:
            if member.isdir() and entries[name]["kind"] == "directory":
                continue
            raise ExportError("Archive has duplicate file names")
        if name == ".":
            if not member.isdir():
                raise ExportError("Invalid app root")
            continue
        if not (member.isdir() or member.isfile() or member.issym() or member.islnk()):
            raise ExportError("Archive contains a device, pipe or unsupported member")
        for parent in PurePosixPath(name).parents:
            if str(parent) in links:
                raise ExportError("Archive writes through a link")
        # Links wait until all file data is copied and the full graph is checked.
        target = staging / name
        target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        if target.exists() and not (member.isdir() and target.is_dir()):
            raise ExportError("Archive path conflicts with another member")
        mode = member.mode & 0o777
        if member.isdir():
            target.mkdir(exist_ok=True, mode=0o700)
            kind = "directory"
        elif member.issym() or member.islnk():
            links[name] = link_path(name, member)
            kind = "hardlink" if member.islnk() else "symlink"
        else:
            if member.size < 0 or shutil.disk_usage(staging).free < member.size + 1024 * 1024:
                raise ExportError("Not enough space for the app data; keep the backup")
            kind = "file"
            digest, copied = hashlib.sha256(), 0
            with archive.extractfile(member) as source, target.open("xb") as output:
                while chunk := source.read(1024 * 1024):
                    output.write(chunk)
                    digest.update(chunk)
                    copied += len(chunk)
                    byte_count += len(chunk)
                    if time.monotonic() - last_progress >= 5:
                        print(json.dumps({"phase": "extracting", "bytes_done": byte_count, "files_done": len(hashes), "total_known": False}), file=sys.stderr, flush=True)
                        last_progress = time.monotonic()
                if copied != member.size:
                    raise ExportError("Incomplete app data")
                output.flush()
                os.fchmod(output.fileno(), mode)
                os.fsync(output.fileno())
            hashes[name] = digest.hexdigest()
        entries[name] = {"kind": kind, "mode": mode, "uid": member.uid, "gid": member.gid}
    # Include implicit directories, then reject file paths below any link,
    # including a link that appeared after a file in tar order.
    for name in list(entries):
        for parent in PurePosixPath(name).parents:
            if str(parent) in links:
                raise ExportError("Archive writes through a link")
            if str(parent) != ".":
                entries.setdefault(str(parent), {"kind": "directory"})
    for name, path in links.items():
        resolved = resolve_link(path, links, entries)
        target = staging / name
        if entries[name]["kind"] == "hardlink":
            if entries[resolved]["kind"] != "file":
                raise ExportError("Hardlink target is not a regular file")
            # Preserve bytes without tying the new copy's inode to another file.
            shutil.copyfile(staging / resolved, target)
            target.chmod(entries[name]["mode"])
            with target.open("rb") as stream:
                os.fsync(stream.fileno())
            hashes[name] = hashes[resolved]
        else:
            target.symlink_to(os.path.relpath(staging / resolved, target.parent))
        entries[name]["target"] = resolved
    return entries, hashes, byte_count


def export_backup(backup, destination, slug=None, password=None, expected_sha=None):
    if sys.platform != "linux":
        raise ExportError("Export on a Linux host; keep the backup on its original host")
    backup, destination = Path(backup).resolve(), Path(destination).absolute()
    if destination.exists() or destination.is_symlink():
        raise ExportError("Destination exists; keep its data and choose a new directory")
    with backup.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    if expected_sha is not None and digest != expected_sha.lower():
        raise ExportError("Backup checksum does not match")
    staging = Path(tempfile.mkdtemp(prefix=".ftw-ha-export-", dir=destination.parent))
    try:
        with tarfile.open(backup, "r:") as outer:
            found = members(outer)
            info = backup_metadata(outer, found)
            app = select_app(info, slug)
            compressed, protected = info.get("compressed", True), info.get("protected", False)
            name = app["slug"] + (".tar.gz" if compressed else ".tar")
            if name not in found or not found[name].isfile():
                raise ExportError("Backup is missing the selected app")
            if protected:
                if password is None:
                    raise ExportError("Protected backup needs its key; use --password-file or the prompt")
                from importlib.metadata import version
                if version("securetar") != SECURETAR_VERSION:
                    raise ExportError("Install the tool's pinned SecureTar dependency")
                from securetar import SecureTarFile
                with outer.extractfile(found[name]) as stream:
                    if not SecureTarFile(fileobj=stream, password=password, gzip=compressed).validate():
                        raise ExportError("Wrong key or incomplete protected backup")
                with outer.extractfile(found[name]) as stream:
                    with SecureTarFile(fileobj=stream, password=password, gzip=compressed) as inner:
                        entries, hashes, size = unpack(inner, staging)
            else:
                with outer.extractfile(found[name]) as stream:
                    with tarfile.open(fileobj=stream, mode="r:gz" if compressed else "r:") as inner:
                        entries, hashes, size = unpack(inner, staging)
            app_metadata = json.loads((staging / "addon.json").read_text())
            if app_metadata.get("version") != app["version"]:
                raise ExportError("App versions in the backup disagree")
            for name in ("data/config.yaml", "data/state.db"):
                if not (staging / name).is_file():
                    raise ExportError("Incomplete FTW app layout; keep the backup for a layout-specific export")
            result = {"tool_version": TOOL_VERSION, "backup_sha256": digest, "slug": app["slug"],
                      "source_version": app["version"], "source_data_root": "/data",
                      "files": entries, "sha256": hashes, "bytes_done": size}
            with (staging / "ftw-export.json").open("x") as stream:
                json.dump(result, stream, indent=2)
                stream.write("\n")
                stream.flush()
                os.fsync(stream.fileno())
            for directory, _, _ in os.walk(staging, followlinks=False, topdown=False):
                sync_directory(directory)
            with backup.open("rb") as stream:
                if hashlib.file_digest(stream, "sha256").hexdigest() != digest:
                    raise ExportError("Backup changed during export; keep it and start from an unchanged copy")
            publish(staging, destination)
            return {"destination": str(destination), "source_version": app["version"],
                    "files_done": len(hashes), "bytes_done": size, "links_done": sum(v["kind"] in ("symlink", "hardlink") for v in entries.values())}
    finally:
        if staging.exists():
            shutil.rmtree(staging)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", action="version", version="FTW HA export " + TOOL_VERSION)
    parser.add_argument("backup", type=Path)
    parser.add_argument("destination", type=Path, nargs="?")
    parser.add_argument("--slug")
    parser.add_argument("--list", action="store_true", help="list app slugs and versions without extracting")
    parser.add_argument("--sha256", help="expected checksum of the downloaded backup")
    parser.add_argument("--password-file", type=Path, help="local backup-key file; the key is never printed")
    args = parser.parse_args()
    try:
        with tarfile.open(args.backup, "r:") as archive:
            metadata = backup_metadata(archive, members(archive))
        if args.list:
            print(json.dumps([{k: app.get(k) for k in ("slug", "name", "version")} for app in metadata["addons"]]))
            return
        if args.destination is None:
            parser.error("destination is required unless --list is used")
        password = None
        if metadata.get("protected"):
            password = args.password_file.read_text().rstrip("\r\n") if args.password_file else getpass.getpass("Home Assistant backup key: ")
        print(json.dumps(export_backup(args.backup, args.destination, args.slug, password, args.sha256)))
    except Exception as error:
        # Dependency and archive errors report no source values or backup key.
        reason = str(error) if isinstance(error, (ExportError, OSError)) else "Invalid archive, key or tool dependency."
        print("Export stopped. Keep the backup and existing FTW data. " + reason, file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()

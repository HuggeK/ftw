"""Write release notes from the candidate's changeset changelog."""

import argparse
import json
from pathlib import Path
import re


def build_notes(receipt, changelog, repo="srcfl/ftw"):
    tag = receipt["tag"]
    match = re.fullmatch(r"v(0\.\d+\.\d+)(-beta\.\d+)?", tag)
    if not match:
        raise ValueError(f"invalid native release tag: {tag}")
    version, beta = match.groups()
    lines = changelog.splitlines()
    try:
        start = lines.index(f"## {version}") + 1
    except ValueError:
        raise ValueError(f"CHANGELOG.md has no entry for {version}") from None
    end = next((i for i in range(start, len(lines))
                if re.match(r"^## \[?\d", lines[i])), len(lines))
    changes = "\n".join(lines[start:end]).strip()
    changes = re.sub(r"<!--.*?-->", "", changes, flags=re.DOTALL).strip()
    if not re.sub(r"(?m)^#+.*$", "", changes).strip():
        raise ValueError(f"CHANGELOG.md has no changes for {version}")
    changes = re.sub(r"(?m)^- [0-9a-f]{7,40}: ", "- ", changes)
    heading = "Changes in this beta" if beta else "Changes in this release"
    notes = f"## {heading}\n\n{changes}\n\n"
    source_beta = receipt.get("source_beta")
    if source_beta:
        notes += f"Promoted from site-tested [{source_beta}](https://github.com/{repo}/releases/tag/{source_beta}).\n\n"
    notes += (f"[Full changelog](https://github.com/{repo}/blob/{tag}/CHANGELOG.md)"
              f" · [Source commit](https://github.com/{repo}/commit/{receipt['commit']})\n\n"
              f"<!-- ftw-state-schema-v2:{receipt['state_schema']} -->\n")
    return notes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("receipt", type=Path)
    parser.add_argument("changelog", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--repo", default="srcfl/ftw")
    args = parser.parse_args()
    notes = build_notes(json.loads(args.receipt.read_text()), args.changelog.read_text(), args.repo)
    args.output.write_text(notes)


if __name__ == "__main__":
    main()

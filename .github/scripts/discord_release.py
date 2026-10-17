"""Announce a published native Core release through the existing Discord webhook."""

import argparse
import json
import os
from pathlib import Path
import re
import sys
from urllib.error import HTTPError, URLError
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit
from urllib.request import Request, urlopen


def payload(release):
    tag = release.get("tag_name", "")
    match = re.fullmatch(r"v0\.(\d+)\.\d+(-beta\.\d+)?", tag)
    if not match or int(match[1]) < 131 or release.get("draft", True):
        raise ValueError("Only published native 0.x Core releases can be announced")
    beta = bool(match[2])
    if release.get("prerelease") != beta:
        raise ValueError("Release channel does not match its tag")
    expected = {"ftw-native-release.json"}
    for arch in ("amd64", "arm64"):
        expected.update((f"ftw-linux-{arch}.tar.gz", f"ftw-linux-{arch}.tar.gz.sha256"))
    if not expected.issubset({a["name"] for a in release.get("assets", [])}):
        raise ValueError("Release packages are incomplete")
    url = f"https://github.com/srcfl/ftw/releases/tag/{tag}"
    if release.get("html_url") != url:
        raise ValueError("Release must belong to srcfl/ftw")
    body = re.sub(r"<!--.*?-->", "", release.get("body") or "", flags=re.DOTALL).strip()
    if not body:
        raise ValueError("Release notes are empty")
    # Count UTF-16 units too, so emoji cannot exceed Discord's embed limit.
    if len(body.encode("utf-16-le")) > 3500 * 2:
        body = body.encode("utf-16-le")[:3500 * 2].decode("utf-16-le", errors="ignore").rstrip()
        body += f"\n\n[Read the full release notes]({url})"
    return {
        "username": "FTW",
        "allowed_mentions": {"parse": []},
        "embeds": [{
            "title": f"FTW {tag}",
            "url": url,
            "description": body,
            "color": 16753920 if beta else 3066993,
            "footer": {"text": "Beta release" if beta else "Stable release"},
        }],
    }


def send(message, webhook):
    if not webhook:
        raise ValueError("DISCORD_WEBHOOK is not set")
    parts = urlsplit(webhook)
    if (parts.scheme != "https" or parts.hostname not in ("discord.com", "discordapp.com")
            or any(c.isspace() for c in webhook)
            or not re.fullmatch(r"/api/(?:v\d+/)?webhooks/\d+/[A-Za-z0-9_.-]+", parts.path)):
        raise ValueError("DISCORD_WEBHOOK must be a Discord webhook URL")
    query = dict(parse_qsl(parts.query))
    query["wait"] = "true"
    url = urlunsplit(parts._replace(query=urlencode(query)))
    request = Request(url, data=json.dumps(message).encode(), method="POST",
                      headers={"Content-Type": "application/json", "User-Agent": "FTW-release"})
    try:
        with urlopen(request, timeout=30) as response:
            result = json.load(response)
    except HTTPError as error:
        raise ValueError(f"Discord rejected the announcement (HTTP {error.code})") from None
    except (URLError, TimeoutError):
        raise ValueError("Could not confirm delivery to Discord") from None
    if not result.get("id"):
        raise ValueError("Discord did not confirm a saved message")
    return result["id"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("release", type=Path)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    try:
        message = payload(json.loads(args.release.read_text()))
        if args.dry_run:
            print(json.dumps(message, ensure_ascii=False, indent=2))
        else:
            message_id = send(message, os.environ.get("DISCORD_WEBHOOK"))
            print(f"Discord saved release announcement {message_id}")
    except (ValueError, OSError) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

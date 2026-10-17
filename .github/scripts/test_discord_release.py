import copy
import io
import json
from pathlib import Path
import unittest
from unittest.mock import patch
from urllib.error import HTTPError

import discord_release


class DiscordReleaseTest(unittest.TestCase):
    def release(self, beta=True):
        tag = "v0.148.0-beta.1" if beta else "v0.148.0"
        names = ["ftw-native-release.json"]
        for arch in ("amd64", "arm64"):
            names += [f"ftw-linux-{arch}.tar.gz", f"ftw-linux-{arch}.tar.gz.sha256"]
        return dict(tag_name=tag, draft=False, prerelease=beta,
                    html_url=f"https://github.com/srcfl/ftw/releases/tag/{tag}",
                    body="## Changes\n\n- Keep the car charging.\n\n<!-- ftw-state-schema-v2:7 -->",
                    assets=[dict(name=name) for name in names])

    def test_beta_and_stable_include_the_actual_notes_and_release_link(self):
        for beta in (True, False):
            with self.subTest(beta=beta):
                release = self.release(beta)
                result = discord_release.payload(release)
                embed = result["embeds"][0]
                self.assertIn(release["tag_name"], embed["title"])
                self.assertEqual(embed["url"], release["html_url"])
                self.assertIn("Keep the car charging.", embed["description"])
                self.assertNotIn("ftw-state-schema", embed["description"])
                self.assertEqual(embed["footer"]["text"], "Beta release" if beta else "Stable release")
                self.assertEqual(result["allowed_mentions"], {"parse": []})

    def test_long_unicode_notes_fit_and_link_to_the_full_text(self):
        release = self.release()
        release["body"] = "@everyone " + "⚡🚗" * 5000
        embed = discord_release.payload(release)["embeds"][0]
        self.assertLessEqual(len(embed["description"].encode("utf-16-le")), 4096 * 2)
        self.assertIn(release["html_url"], embed["description"])

    def test_retired_components_drafts_and_incomplete_releases_do_not_post(self):
        original = self.release()
        variants = [dict(tag_name=tag) for tag in
                    ("v3.8.0-beta.1", "v0.130.4", "optimizer-v1.3.2", "drivers-stable")]
        variants += [dict(draft=True), dict(prerelease=False), dict(assets=[]), dict(body="<!-- marker -->")]
        for change in variants:
            release = copy.deepcopy(original)
            release.update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                discord_release.payload(release)

    def test_sender_waits_for_a_saved_message_and_keeps_thread_selection(self):
        with patch.object(discord_release, "urlopen", return_value=io.StringIO('{"id":"123"}')) as post:
            message = discord_release.payload(self.release())
            self.assertEqual(discord_release.send(message, "https://discord.com/api/webhooks/1/secret?thread_id=42"), "123")
            request = post.call_args.args[0]
            self.assertIn("wait=true", request.full_url)
            self.assertIn("thread_id=42", request.full_url)
            self.assertEqual(json.loads(request.data), message)

    def test_webhook_errors_fail_without_printing_the_secret_url(self):
        webhook = "https://discord.com/api/webhooks/1/secret"
        error = HTTPError(webhook, 404, "not found", {}, None)
        with patch.object(discord_release, "urlopen", side_effect=error):
            with self.assertRaisesRegex(ValueError, "HTTP 404") as raised:
                discord_release.send({}, webhook)
        self.assertNotIn("secret", str(raised.exception))
        with patch.object(discord_release, "urlopen", return_value=io.StringIO('{}')):
            with self.assertRaisesRegex(ValueError, "saved message"):
                discord_release.send({}, webhook)

    def test_native_workflow_only_announces_after_new_publication(self):
        workflow = (Path(__file__).parents[1] / "workflows/native-release.yml").read_text()
        self.assertIn("newly_published: ${{ steps.publication.outputs.newly_published }}", workflow)
        self.assertIn("needs.publish.outputs.newly_published == 'true'", workflow)
        self.assertIn("uses: ./.github/workflows/release-discord.yml", workflow)
        self.assertIn('echo "newly_published=false"', workflow)


if __name__ == "__main__":
    unittest.main()

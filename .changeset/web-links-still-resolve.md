---
"ftw": patch
---

Report on every pull request whether the documentation followed the change. `.github/check-docs-follow-change.sh` resolves the website's links into this repository against the checkout, compares the install command the site publishes against this README's, and — for any change carrying a changeset — names the documents that describe the paths it touched and the website sections that make promises about them. Advisory: `srcfl/ftw-web` is where the site half of the work happens, and `no-website-change` in the pull request description records a deliberate no.

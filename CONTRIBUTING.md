# Contributing to FTW

Thanks for helping build the local energy coordination layer. This document
covers the legal bits — for how the code is organized and how to add a driver,
start with `AGENTS.md` and `docs/writing-a-driver.md`.

## Website

The public website (<https://ftw.sourceful.energy>) lives in its own
repository, [`srcfl/ftw-web`](https://github.com/srcfl/ftw-web). Landing-page
copy, install instructions and other site content are edited there, not in this
repo — open a pull request against `srcfl/ftw-web` for website changes.

That does not make the website somebody else's problem. A change that a user can
see is finished in three places: the code, the documentation in this repository,
and the description of FTW that people read before they ever clone it.

- **Here.** Update the documents that describe what you changed. A document that
  names the path you touched and did not change with it is the next reader's
  wrong answer.
- **There.** If the change alters what FTW does, how it installs, or which
  hardware it drives, open a pull request against `srcfl/ftw-web` saying what it
  means for somebody deciding whether to run FTW — not what it does in the code.
  A capability described only under `docs/` exists for people who already cloned
  the repository, which is nobody who is still deciding to.
- **Both directions.** The site also deep-links into this repository: a README
  anchor behind each install button, `docs/` pages, the raw `scripts/install.sh`
  URL it tells people to pipe into bash. Renaming a heading breaks those quietly,
  because GitHub drops a fragment it cannot find instead of erroring.

The `repo hygiene` workflow reports all of that on every pull request: links that
stopped resolving, an install command that no longer matches this README, and —
for any change carrying a changeset — the documents that describe what you
touched and the website sections that make promises about it. It never fails the
merge. When a change genuinely does not reach the website, write
`no-website-change` in the pull request description and that part goes quiet.

The same report is available locally:

```bash
.github/check-docs-follow-change.sh
WEB_SITE_FILE=../ftw-web/index.html .github/check-docs-follow-change.sh
```

## License of contributions

This project is licensed under the **Apache License, Version 2.0** (see
[`LICENSE`](LICENSE)). By submitting a contribution, you agree that your
contribution is licensed under the Apache License, Version 2.0.

## Developer Certificate of Origin (DCO)

We use the [Developer Certificate of Origin](https://developercertificate.org/)
instead of a CLA. It is a lightweight way for you to certify that you wrote, or
otherwise have the right to submit, the code you are contributing.

Every commit must be signed off. Add a `Signed-off-by` line with your real name
and email by committing with the `-s` flag:

```bash
git commit -s -m "feat(drivers): add my new driver"
```

This appends a line like:

```
Signed-off-by: Your Name <you@example.com>
```

The full text of the DCO you are certifying against:

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

## Pull requests

- Keep PRs focused on one logical change.
- New code needs tests; `make verify` must pass before review.
- User-visible changes need a Changeset entry (`npx changeset`) — see the
  Release Process section in `README.md`.

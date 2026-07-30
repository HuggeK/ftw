# Contributing to FTW

Thanks for helping build the local energy coordination layer. This document
covers the legal bits — for how the code is organized and how to add a driver,
start with `AGENTS.md` and `docs/writing-a-driver.md`.

## Website

The public website (<https://ftw.sourceful.energy>) lives in its own
repository, [`srcfl/ftw-web`](https://github.com/srcfl/ftw-web). Landing-page
copy, install instructions and other site content are edited there, not in this
repo — open a pull request against `srcfl/ftw-web` for website changes.

The traffic runs the other way too: the site deep-links into this repository, a
README anchor behind each install button, `docs/` pages, the raw
`scripts/install.sh` URL. Renaming a heading or moving one of those files breaks
the site quietly, so the `repo hygiene` workflow resolves every one of those
links on each pull request and warns when one stops resolving. It never fails
the merge — the fix belongs in `srcfl/ftw-web`, and the warning exists so that
pull request gets opened while you still know what replaced what. The same
report is available locally:

```bash
.github/check-web-links.sh
WEB_SITE_FILE=../ftw-web/index.html .github/check-web-links.sh
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

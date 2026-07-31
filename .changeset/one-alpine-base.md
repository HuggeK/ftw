---
"ftw": minor
---

Run core and the updater sidecar on one shared base layer, and move the optimizer off oldstable.

The updater sidecar was `docker:27-cli`. That image is alpine-based too, but it
is a *different* alpine — its own base layer on its own cadence — so a host
pulled two rootfs blobs and tracked two upgrade streams for what looked like one
distribution. The sidecar now sits on the same `alpine:3.22` tag as core, with
the `docker` CLI and the compose plugin copied straight out of the official CLI
image. That is the same upstream artifact, with the version pinned explicitly
and no package repository added; both are statically linked Go binaries, so
neither depends on the base's libc.

The third image cannot join them. CVXPY publishes no musllinux wheels — the
newest that exists is 0.4.10 — and neither do clarabel, osqp or ecos, so the
optimizer cannot be built on alpine without compiling a Rust solver and two C++
solvers from source on every release, arm64 under emulation included. It stays
on Debian slim, but moves from bookworm to trixie, because bookworm is oldstable
and part of the deployment was already off full security support. The container
boundary test now asserts all of this instead of grepping one file for a fixed
string with no error message.

The optimizer is versioned independently and moves to 1.4.0: its image is a
materially different artifact once the base changes, and its release workflow
verifies that a published image's revision label matches the commit it claims,
so the new base could not ship under the old version number.

The zoneinfo database is also embedded in the core binary as a fallback. It was
already installed in the image; the point is that a base without it can no
longer silently push `time.Local` to UTC and mis-time price and plan windows.

# Contributing

Patches welcome. House rules:

- TDD fail-first: write the failing test, watch it fail, then fix.
- `go test ./...` green and `make lint` zero findings before you push.
- Commits are unsigned; history stays linear, squash-merge via PR.
- Docs stay laconic: short lines, no fluff, link instead of duplicating.
- Staging deploys go through the on-call buddy, never direct.

## Developer Certificate of Origin

By contributing you certify the
[Developer Certificate of Origin 1.1](https://developercertificate.org/):
the contribution is yours to give and you license it under this repo's
`LICENSE` (Apache-2.0). No sign-off trailer required — opening the PR
is the attestation.

# Security Policy

## Supported Versions

tezzer is pre-1.0 software. Security fixes are applied to the latest released
version and to `main`. Older versions are not maintained.

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, report them privately through GitHub's
[private vulnerability reporting][gh-pvr]:

1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability**.
3. Provide a description, affected versions, and reproduction steps if possible.

[gh-pvr]: https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability

If you prefer email, you can instead contact the maintainer privately at
<kuriyama@s2factory.co.jp>.

> Note: private vulnerability reporting must be enabled on the repository
> (Settings → Code security and analysis). Enable it before public release.

## What to Expect

- We aim to acknowledge a report within a few days.
- We will work with you to understand and validate the issue.
- Once a fix is available, we will coordinate disclosure and credit you in the
  release notes if you wish.

## Scope

tezzer transports terminal I/O over UDP with an authenticated, encrypted
channel and falls back to a Unix-domain-socket / TCP control channel. Reports
that are particularly relevant include, but are not limited to:

- Weaknesses in the packet authentication or encryption (see `internal/qtransport`
  and [docs/security-model.md](docs/security-model.md)).
- Issues allowing session hijacking, replay, or unauthorized reattach.
- Denial of service reachable by a remote peer.

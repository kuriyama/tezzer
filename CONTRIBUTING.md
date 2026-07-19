# Contributing to tezzer

Thanks for your interest in contributing! This document describes how to build,
test, and submit changes.

## Code of Conduct

This project follows a [Code of Conduct](CODE_OF_CONDUCT.md). By participating,
you are expected to uphold it.

## Prerequisites

- Go 1.25 or newer
- A POSIX environment (Linux or macOS). tezzer uses PTYs and Unix domain sockets.

## Build

```sh
make build          # builds bin/tezzerd and bin/tezzer
```

## Test

```sh
make test           # all tests (go test ./...)
make ci             # fast pre-push check: gofmt + go vet + go test -short  (~30s)
make ci-nightly     # race detector + a longer randomized simulation
make test-sim       # the randomized simulation, many seeds (TEZZER_SIM_ITER=N)
make e2e            # real-binary + pty end-to-end smoke (manual)
make e2e-docker     # sleep-recovery scenario under rootless docker (manual)
```

CI runs `make ci` on every push and pull request, and `make ci-nightly` on a
schedule. Please run `make ci` locally before opening a pull request.

The test suite includes a deterministic, virtual-time simulation
(`go test`'s `testing/synctest`) for the UDP transport. When a simulation test
fails it prints a `seed=`; rerun the exact case with `TEZZER_TEST_SEED=<seed>`.

## Submitting Changes

1. Fork the repository and create a topic branch.
2. Make focused commits; keep formatting-only changes separate from behavior
   changes.
3. Add or update tests for any behavior change. For a bug fix, add a regression
   test that fails before the fix.
4. Ensure `make ci` passes.
5. Open a pull request describing the motivation and the change. Link any
   related issue.

### Commit messages

Use clear, imperative commit subjects. A `type: summary` prefix
(`fix:`, `test:`, `docs:`, `refactor:`, `build:`) is appreciated but not required.

### Comment language

Documentation and the godoc surface (package comments and doc comments on
exported identifiers) are in English. Inline implementation comments are
currently written primarily in Japanese — the maintainer's working language —
and you will encounter them throughout the code. This is a deliberate
trade-off, not an oversight: many of these comments carry detailed design
rationale that is most reliably maintained in the language it was thought in.
English is welcome in contributions; there is no need to write Japanese.

## Project Layout

- `cmd/tezzerd` — server binary
- `cmd/tezzer` — client binary
- `internal/qtransport` — QUIC transport: streams, TCP/agent forwarding,
  migration/reconnect
- `internal/session` — PTY session management and output buffering
- `internal/proto`, `internal/netx`, `internal/stun` — control protocol, framing,
  STUN
- `docs/` — architecture overview and Architecture Decision Records (`docs/adr`)

## Reporting Bugs and Requesting Features

Use the issue templates. For security issues, follow [SECURITY.md](SECURITY.md)
instead of opening a public issue.

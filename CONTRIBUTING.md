# Contributing to cob

Thanks for helping improve `cob`. This is a small, focused tool; the bar is
correctness, clear errors, and tests that pin behavior.

## Prerequisites

- Go matching the version in [`go.mod`](go.mod) (currently `go 1.25`).
- AWS credentials are **not** required to build or run the unit tests — every
  AWS call is exercised through in-memory fakes. They're only needed to run
  `cob` against real CodeArtifact.

## The validate gate

A change is not done until the same checks CI runs
([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) pass locally. Run them
before opening a PR:

```sh
go build ./...                                              # compiles
GOOS=windows GOARCH=amd64 go build ./...                   # cross-compiles
GOOS=darwin  GOARCH=arm64 go build ./...                   # (cob ships these)
go vet ./...                                               # vet
gofmt -l .                                                 # must print nothing
go test -race ./...                                        # tests, race detector on
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...     # staticcheck
go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...      # known-CVE scan
```

Do not weaken the gate to go green: don't disable a lint, drop `-race`, skip a
test, or loosen a threshold. If a lint must be suppressed, do it inline and
narrowly with a `//nolint:<name> // reason` and explain why.

## How we work

- **Tests first.** Features and bug fixes start with a failing test that
  reproduces the gap, committed as its own checkpoint, then the minimal change
  to make it pass. A bug fix without a regression test is incomplete.
- **Never edit a test to make it pass.** Fix the implementation. If a test
  genuinely must change, say so explicitly and explain why.
- **Fakes, not mocks.** Collaborators are exercised through small hand-rolled
  in-memory fakes (see [`internal/cliutil/clitest`](internal/cliutil/clitest)
  and the `*_test.go` fakes in `internal/cob`). Don't add a mock-generation
  framework, and never fake production code to pass a test.
- **Errors as values.** Wrap with `%w`, detect with `errors.Is`/`errors.As`
  (never string matching), and make messages tell the operator what to do next.
- **Keep changes small and idiomatic.** Match the surrounding code; prefer the
  smallest change that satisfies the task over the cleverest one. List adjacent
  work rather than doing it.

## Commits and PRs

- Keep commits small and message them clearly; one logical change per commit.
- Commit a failing test separately from the implementation that fixes it.
- Don't commit machine-local files or build artifacts (the `cob` binary and
  `dist/` are git-ignored — keep them that way).

## Releases

Releases are cut by pushing a `vX.Y.Z` tag, which runs
[`.github/workflows/release.yml`](.github/workflows/release.yml): it tests,
scans, cross-compiles the five target binaries, writes `checksums.txt`, and
signs it with cosign (keyless, via GitHub OIDC). See the README's install
section for how to verify a downloaded binary against that signature.

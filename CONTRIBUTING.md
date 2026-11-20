# Contributing

Thanks for helping. Read the project rules in [AGENTS.md](AGENTS.md) and the
architecture and isolation requirements in [docs/SPEC.md](docs/SPEC.md).

## Setup

```sh
brew install go go-task golangci-lint
task check
```

Running jobs also needs Apple `container` (`brew install container`, then
`container system start`), macOS 26 or later on Apple Silicon.

## Changes

- Branch from `main` and open a pull request back to `main`.
- Use Conventional Commit messages (`feat:`, `fix:`, `docs:`, `test:`, `ci:`, `chore:`).
- Run `task check` before pushing; add tests for new behaviour.
- Use synthetic fixtures, never real credentials or personal data.
- Describe the observable problem, the change, and the checks you ran.

Pull requests are squash-merged.

## Releases

Maintainers release locally with release-tools. See the build and release
instructions in [README.md](README.md). Contributors never need signing credentials.

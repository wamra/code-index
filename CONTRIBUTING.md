# Contributing

## Build and test

You need the Go version named in `go.mod`. No CGO is used.

```sh
go build -o code-index ./cmd/code-index
go vet ./...
go test ./...
```

The tests use a fake language server, recorded extractor output and a fake
embedding server, so they need no network and no real tools. Run them with a
throwaway `HOME` if you do not want them near your own configuration:
`HOME=$(mktemp -d) go test ./...`.

The plugin's TypeScript tests (`plugins/code-index-search/hooks/register.test.ts`)
run in Claude Code's plugin test harness.

`scripts/gen-notices.sh` regenerates `THIRD_PARTY_NOTICES.md`; run it when
`go.mod` changes. `scripts/build-plugin.sh OUT` and `scripts/check-plugin.sh OUT`
stage and check the installable plugin tree.

## Pull requests

- Keep a change small and focused, with a test for new behavior.
- Anything that starts a process, reads a path from the index or a repo file, or
  chooses a network destination must respect the trust rules in the README
  ("Repo configuration is untrusted by default"). Add a test for it.
- Commit messages: a plain imperative summary line.
- No attribution trailers or sign-off lines are required, and there is no DCO or
  CLA. By contributing you agree that your work is released under the MIT license
  in `LICENSE`.

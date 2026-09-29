# Contributing

## Ground rules

- `master` is always releasable: CI (gofmt, `go vet`, `go test -race ./...`)
  must pass.
- No secrets in the repo, ever: not in code, tests, docs, or examples.
  Test secrets like `"s3cret"` are fine; real ones are not.

## Workflow

1. Fork, branch from `master` (`feat/...`, `fix/...`).
2. Keep `cmd/` thin: real logic lives in `internal/` packages and is unit
   tested. New protocol behavior needs a test in `internal/proto` and an
   update to `PROTOCOL.md` **in the same PR**.
3. Run `make fmt vet test` before pushing.
4. Update `CHANGELOG.md` under `[Unreleased]`.

## Protocol changes

`PROTOCOL.md` is the contract between client and server, which may be
deployed independently. Rules:

- v1 frame types and semantics are frozen.
- New features use **reserved** type bytes; receivers must ignore unknown
  types (forward compatibility is tested).
- Bump the protocol version in `PROTOCOL.md` and note it in the changelog.

## Code style

- Standard `gofmt`; no extra linters required, but keep functions small and
  errors wrapped with context (`fmt.Errorf("...: %w", err)`).
- Logs go to the standard logger with stream ids where relevant; never log
  the secret or full URLs.

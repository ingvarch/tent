# Development

How to build, check and test tent from a clone of the repository, and how a release is made. To use a released tent,
see the [quick start](quickstart.md).

## What you need

- Go 1.26, the version in `go.mod`. CI tests with it on Linux, macOS and Windows, and with the newest Go on Linux.
- golangci-lint v2.14.0, the version pinned in the `Makefile`. `make fmt` and `make lint` refuse another version,
  since it finds and formats other things.
- make.
- For `make dist` only: goreleaser at the version in `.tool-versions`, and syft.
- For development builds that run on Vultr and for the E2E suite: a Vultr API key, an R2 API token and the bucket URL
  (see [Development builds](#development-builds)).

## Build

```sh
make build
bin/tent version
```

`make build` writes `bin/tent` and `bin/tent-node_linux_amd64`, both with the version from `git describe`.

## Checks

```sh
make check
```

`make check` runs `fmt`, `lint`, `licenses`, `test` and `build`. `fmt` rewrites files that are not formatted, and
`licenses` fails when a module that tent or tent-node links has a licence outside the allowed list. On every pull
request CI runs the lint, the licence check, the tests on Linux, macOS and Windows, and a release snapshot.

## Tests

```sh
go test -race ./...                                   # what make test runs
go test -race -count=1 -run TestName ./internal/app/  # one test
go test ./internal/<package> -update                  # rewrite the golden files of a package
```

Unit tests sit next to the code, with golden files under `testdata/`. The flow tests of `internal/app` run each command
against fakes of Vultr and Nomad, and cut runs short to show that a run started again finishes the work.
[Architecture §15](architecture.md#15-testing) describes the testing strategy.

The s3 state store runs its tests against a fake. To run them against a real bucket, set `TENT_TEST_S3_URL` and run
`go test -race -count=1 -run S3 ./internal/statestore/`; CI does that against Cloudflare R2.

`make generate` regenerates the JSON Schema of the specs, `api/v1alpha1/tent.schema.json`.

## Development builds

A build from `main` is a development build: no release holds the tent-node that its nodes run. Its nodes need a
tent-node built from the same commit, which you give with `TENT_NODE_URL` and `TENT_NODE_SHA256`. Without them
`tent update cluster` fails and says so.

`make dev-upload` builds both binaries, uploads tent-node to the R2 bucket in `TENT_DEV_S3_URL` and prints the lines
that set the two variables, for fish or for sh:

```sh
make dev-upload | source   # fish
eval "$(make dev-upload)"  # sh
```

[hack/tent-node-upload](../hack/tent-node-upload/README.md) explains the setup of the bucket and the token.

## E2E suite

```sh
make e2e
make e2e E2E_RUN='TestHA|TestUpgrade'
```

`make e2e` runs the black-box suite on Vultr from the maintainer's machine, not in CI. It needs `VULTR_API_KEY`, the
R2 keys and `TENT_DEV_S3_URL`, and it can take up to 180 minutes. [test/e2e](../test/e2e/README.md) describes its
scenarios and its janitor. [hack/vultr-spike](../hack/vultr-spike/README.md) checks undocumented Vultr behaviour on a
real account.

## Releases

A tag `v*` starts the release workflow: GoReleaser builds the archives, the `.deb` and `.rpm` packages, the Homebrew
cask and the tent-node binaries, and signs them ([ADR-0020](adr/0020-release-channels-and-ci-conventions.md),
[architecture §16](architecture.md#16-technology-stack-and-releases)). `make dist` builds the same files locally,
without publishing or signing them.

## Project structure

| Path | What it holds |
| --- | --- |
| `cmd/tent`, `cmd/tent-node` | the CLI and the agent on each node |
| `api/v1alpha1` | the public API types of the specs and their JSON Schema |
| `internal/cli`, `internal/app` | the commands and the use cases behind them |
| `internal/engine`, `internal/model`, `internal/rollout` | the reconciliation engine, the model of a cluster, the rollout decisions |
| `internal/cloud` | the provider interface and the Vultr provider |
| `internal/nomadops` | the client of Nomad's API |
| `internal/nodeup`, `internal/nodeconfig` | tent-node's phases and the configuration it gets |
| `internal/statestore` | the state store and its locks |
| `test/e2e` | the E2E suite |
| `hack/` | development tools: the spike, the tent-node upload, the E2E janitor |

[Architecture §5](architecture.md#5-repository-layout-and-dependency-rules) has the full layout and the import rules
that golangci-lint enforces.

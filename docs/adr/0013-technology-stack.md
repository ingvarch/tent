# ADR-0013: Technology stack and release engineering

- **Status:** Accepted; govultr added by [ADR-0018](0018-vultr-provider-design.md); extended by
  [ADR-0020](0020-release-channels-and-ci-conventions.md); JSON Schema generator and spec decoding
  libraries added by [ADR-0022](0022-json-schema-from-go-types.md); on 2026-09-26 the state store (M0.3) added
  `github.com/gofrs/flock` and `golang.org/x/mod`, and `github.com/aws/smithy-go` as a direct dependency; on
  2026-09-27 the licence check added `github.com/google/licenseclassifier/v2`, which tent does not link
  ([architecture §16](../architecture.md#16-technology-stack-and-releases))
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0006](0006-two-binaries-and-nodeconfig.md), [ADR-0011](0011-nomad-only-scope-and-licensing.md),
  [architecture §16](../architecture.md#16-technology-stack-and-releases)

## Context

- **Go** is a given: it is the language of the Nomad and Hetzner ecosystems and produces static binaries.
- **Current versions:**
  - Go 1.27.1 is current; 1.26.x is still supported.
  - `github.com/hashicorp/nomad/api` requires Go 1.26 or newer.
  - hcloud-go v2.49.0 requires Go 1.25 or newer.
- **Dependencies.** We want few of them, all well maintained, and with no licence surprises.

## Decision

- **Language.** `go.mod` declares `go 1.26`. CI tests on the two supported Go releases.
- **Libraries:**

  | Purpose | Choice | Notes |
  |---|---|---|
  | CLI | `github.com/spf13/cobra` | no viper; flags → env → config file handled explicitly |
  | Hetzner | `github.com/hetznercloud/hcloud-go/v2` | pinned; `WithPollOpts` exponential, custom RoundTripper for rate limits |
  | Nomad | `github.com/hashicorp/nomad/api` | pseudo-version; only in `internal/nomadops` |
  | S3 | `github.com/aws/aws-sdk-go-v2/service/s3` | also the future AWS provider's SDK family |
  | YAML | `sigs.k8s.io/yaml` | JSON tags, strict decoding |
  | OpenPGP | `github.com/ProtonMail/go-crypto` | verifying HashiCorp release signatures |
  | Concurrency | `golang.org/x/sync/errgroup` | engine worker pool |
  | Logging | `log/slog` | text for humans, JSON with `--log-format json` |
  | Tests | stdlib `testing` + `github.com/google/go-cmp` | golden files under `testdata/` |

  Explicitly not used: viper, Kubernetes apimachinery, testify-style assertion frameworks and any Nomad root-module
  package.
- **Nomad configuration** is rendered as HCL with `text/template` and strict escaping, and covered by golden tests.
  The agent parses its configuration with HCL1, so we keep to plain blocks and attributes.
- **Quality gates in CI:**
  - golangci-lint, with depguard layer rules;
  - `go vet`;
  - `go test -race ./...`;
  - govulncheck;
  - Renovate for dependency updates.
- **Releases** use GoReleaser on tags:
  - `tent` for linux, darwin and windows on amd64 and arm64;
  - `tent-node` for linux on amd64 and arm64, published as raw binaries;
  - `checksums.txt`, keyless cosign signatures and SBOMs.
- **Build info.** Version, commit and date are injected through ldflags into `internal/buildinfo`.
- **Version pinning.** The CLI finds tent-node's sha256 in `checksums.txt` of its own release, so nodes always run
  the CLI's exact version.

## Consequences

### Positive

- A small, conventional stack that is easy for any Go developer, human or AI, to work in.
- Supply-chain hygiene (signatures, SBOMs, checksums) from the first release.

### Negative / trade-offs

- Handling flags, environment and config precedence explicitly costs a little code compared with viper.
- `nomad/api` has no semver tags, so updates are manual pseudo-version bumps. That is acceptable because it changes
  slowly.

### Follow-ups

- M0 sets up `go.mod`, a Makefile, `.golangci.yml` with the depguard rules, and CI.
- M2 adds `.goreleaser.yaml`.
- tent is licensed under Apache-2.0 (decided 2026-09-25). M0 adds the `LICENSE` file.

## Alternatives considered

- **urfave/cli or kong** instead of cobra. Both are fine, but cobra is the de facto standard for kops- and
  kubectl-like command trees.
- **gocloud.dev/blob** for the state store. It is a nice abstraction, but conditional-write support differs by
  driver, and we need aws-sdk-go-v2 anyway.
- **Generating Nomad configuration as JSON.** Nomad accepts it, but HCL is what operators read on nodes, and golden
  tests keep it correct.

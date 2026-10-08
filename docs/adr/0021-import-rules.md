# ADR-0021: Import rules that list the allowed importers

- **Status:** Accepted; extended by [ADR-0025](0025-stdlib-only-helper-packages.md) (standard-library-only helper
  packages), [ADR-0026](0026-channels-and-release-assets.md) (go-crypto only in `internal/assets`; assets and
  channels kept out of tent-node; `internal/channels` limited to the standard library, the YAML decoder and semver)
  and [ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md) (`internal/nodeconfig` limited to
  the standard library, `internal/secret` and the API types; `github.com/hashicorp/hcl` only in tests;
  `nodeup-no-cloud` covers `internal/nodeconfig`) and [ADR-0028](0028-tent-node-agent-units-and-delivery.md)
  (`tent-node-allowed` lists what `internal/nodeup` and `cmd/tent-node` may import; `nodeup-no-cloud` covers
  `cmd/tent-node`; `nodeuptest` and `s3urltest` only in tests; the follow-up `go list -deps ./cmd/tent-node` test is
  built; `internal/s3url` imports only the standard library and the AWS SDK's S3 client) and by
  [ADR-0031](0031-bootstrap-in-update.md) (`internal/assets/assetstest` imports only the standard library; only
  tests import it and `hack/internal/shellenv/shellenvtest`) and by [ADR-0033](0033-operator-commands.md)
  (`internal/shellenv` and `internal/shellenv/shellenvtest` replace `hack/internal/shellenv`, since tent prints shell
  lines and `cmd/tent` cannot import a package under `hack/internal`; only tests import `shellenvtest`) and by
  [ADR-0034](0034-e2e-suite-on-vultr.md) (`e2e-black-box`: `test/e2e` and `hack/e2e-janitor`, tests included, import
  nothing under `internal`, so E2E code gets no exemption; only tests import `test/e2e/janitor/janitortest`) and by
  [ADR-0035](0035-rollout-decisions.md) (`rollout-pure`: `internal/rollout` imports only the standard library,
  `api/v1alpha1`, `internal/english` and `golang.org/x/mod/semver`; its tests are exempt)
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0004](0004-layered-architecture.md), [ADR-0006](0006-two-binaries-and-nodeconfig.md),
  [ADR-0011](0011-nomad-only-scope-and-licensing.md),
  [architecture §5](../architecture.md#5-repository-layout-and-dependency-rules)

## Context

- [ADR-0004](0004-layered-architecture.md) states the import rules by the packages they forbid:
  - `api/` is stdlib-only;
  - core packages never import provider packages or cloud SDKs;
  - `internal/nodeup` never imports `internal/cloud/...`;
  - only `internal/nomadops` imports the Nomad API module.
- depguard checks the direct imports of each file. If a helper package outside the core imports a provider and the
  core imports the helper, no rule fires. The same path could carry a provider into `tent-node`
  ([ADR-0006](0006-two-binaries-and-nodeconfig.md)).
- `api/` is public and may need subpackages, which a stdlib-only rule forbids.

## Decision

The depguard rules in `.golangci.yml` name the packages that may import providers, cloud SDKs and Nomad. Every other
package is denied.

1. **Providers.** Only `cmd/tent` imports provider packages (`internal/cloud/<provider>/...`), to register them.
   Every other package, `internal/cloud` and the core included, uses the interfaces in `internal/cloud`. Tests are
   exempt, and so is code under `internal/cloud/<provider>/`, so a provider can have subpackages.
2. **Cloud SDKs.** govultr is imported only under `internal/cloud/vultr`, and hcloud-go only under
   `internal/cloud/hetzner`. Tests are included.
3. **Nodes.** `internal/nodeup` and its tests import nothing from `internal/cloud`: neither the interfaces nor a
   provider.
4. **Nomad.** Only `internal/nomadops` imports `github.com/hashicorp/nomad/api`. No package imports anything else
   from `github.com/hashicorp/nomad`, so the BUSL-licensed root module stays out
   ([ADR-0011](0011-nomad-only-scope-and-licensing.md)). Tests are included.
5. **Public API.** `api/` imports only the standard library and other `api/` packages. Its tests are exempt.
6. **Scope.** File globs start at `${base-path}`, the repository root, so a directory named `api` or `cmd` deeper in
   the tree does not match. Lint runs with the `e2e` build tag, so the rules cover E2E code too.
7. **Other lint rules.**
   - A `//nolint` directive names the linter and gives a reason (nolintlint).
   - Library code does not panic (forbidigo). Tests and `cmd/` are excepted.

## Consequences

### Positive

- A provider or a cloud SDK cannot reach the core or `tent-node` through another package: only `cmd/tent` imports a
  provider, and only the provider imports its SDK.
- A new provider package falls under rule 1 without a config change. Only its SDK needs a new rule.
- `api/` can have subpackages.

### Negative / trade-offs

- Tests may import provider packages, so a core test could depend on a real provider.
- No rule stops one provider from importing another: the exemption in rule 1 covers everything under
  `internal/cloud/*/`.
- Rule 3 checks the direct imports of `internal/nodeup`. The interfaces in `internal/cloud` could still reach it
  through another package.

### Follow-ups

- M1: the first core integration test decides where provider fakes live. Decided 2026-09-27: the Vultr fake is
  `internal/cloud/vultr/vultrfake` ([ADR-0012](0012-testing-strategy.md)).
  - Core tests may import the fake and the Vultr provider, since rule 1 exempts tests.
  - Rule 2 covers tests too, so a core test cannot import govultr. It cannot build govultr values either, such as
    the objects that seed the fake or the `*Req` values of its calls.
  - The first core integration test decides between seed helpers of the fake that take tent's own types and a
    scoped exemption from rule 2 for tests.
- Code outside tests that imports a provider, such as an E2E helper under `test/e2e` or a Go janitor under `hack/`,
  fails lint. It gets an explicit exemption when it appears.
- When `cmd/tent-node` exists, a test that runs `go list -deps ./cmd/tent-node` can check the binary's whole
  dependency tree for `internal/cloud` and cloud SDKs.

## Alternatives considered

- **Keep ADR-0004's list of forbidden importers.** depguard checks direct imports only, so a provider could reach the
  core through a package that is not on the list.
- **Check dependencies only with `go list -deps`.** There is no `tent-node` binary to check yet, and depguard reports
  the file and the import that break a rule.

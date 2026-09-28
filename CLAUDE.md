# CLAUDE.md

Guidance for Claude Code in this repository.

## Project

tent is a Go CLI that provisions and operates HashiCorp Nomad clusters on cloud providers: kops for Nomad. The module
path is `github.com/ingvarch/tent`.

Providers, in order:
1. **Vultr** is implemented first and runs the E2E suite (`docs/adr/0014-vultr-first-provider-and-e2e.md`).
2. **Hetzner Cloud** is second.
3. **AWS** comes later. It must remain possible without changes to the core.

**Status:** M0 Foundation (2026-09-27) and M1 Vultr infrastructure (2026-09-28) are complete.
- The skeleton is in place: Go module, `tent version`, Makefile, lint rules, CI on Linux, macOS and Windows, and a
  GoReleaser release pipeline. The repository is public, the release secrets are set, and the archives and packages
  ship third-party licence notices (ADR-0020). Renovate updates the Go modules and GitHub Actions.
- M0.2 added the API types with defaults and validation (`api/v1alpha1`), the spec file reader and writer
  (`internal/spec`) and the JSON Schema (`api/v1alpha1/tent.schema.json`, ADR-0022).
- M0.3 added the state store (`internal/statestore`): the `file://` and `s3://` backends, the cluster layout, the
  version guard and cluster locks (`docs/architecture.md` §10). CI tests the s3 backend against Cloudflare R2.
- M0.4 added the spec commands of the CLI (`internal/cli` over the use cases in `internal/app`): global flags, a config
  file, `tent create`, `get`, `edit`, `replace`, `delete cluster` (state only) and `state unlock`
  (`docs/architecture.md` §14). `cmd/tent/exit_test.go` checks the M0 exit criteria.
- The Vultr spike has run (2026-09-25).
- M1 added the reconciliation engine (`internal/engine`, M1.1), the Vultr API client, the label codec and the fake
  `vultrfake` (M1.2), the model, the provider interface and the Vultr infrastructure tasks (M1.3), the Vultr node
  primitives (M1.4), and `tent update cluster`, `tent delete cluster` and `create --yes` (M1.5,
  `docs/architecture.md` §13). The nodes are empty machines without Nomad. The exit criteria were met in the
  integration tests (`internal/app/integration_test.go`, `interrupt_test.go`) and on a real Vultr account.
- M2 Nomad bootstrap is in progress, in parts M2.1 to M2.9 (`docs/roadmap.md`).
  - M2.1 added the cluster PKI and secrets (`internal/pki`, ADR-0024): `update` makes the CA, the gossip key and the
    ACL bootstrap secret once and keeps them in the state store; `delete` removes them.
  - M2.2 added the release channels and assets (`internal/channels`, `internal/assets`, ADR-0026): the embedded
    `stable` channel, the Nomad version pin in `cluster.completed.yaml`, and Nomad, CNI and tent-node downloads
    verified by signature or sha256. Nothing downloads on nodes yet.
  - Next: M2.3 NodeConfig.

## Read before changing anything

1. `docs/architecture.md`: the design and the source of truth.
2. `docs/adr/`: accepted decisions. Do not diverge silently. If an implementation must deviate, write a superseding
   ADR first (see `docs/adr/README.md`).
3. `docs/platform-notes.md`: verified Nomad, Hetzner and Vultr API facts and quirks as of 2026-09-25.
   - Re-verify items marked ⏳ (prices, availability, versions) before relying on them.
   - Items marked 🔬 are unverified until `hack/vultr-spike` has run.
4. `docs/roadmap.md`: milestone goals and exit criteria. The work items are GitHub issues in milestones M0–M6
   (project "tent roadmap"); close them as they land.

## Conventions

- **Language.** Everything committed is in English: code, comments, log messages, CLI output, docs and commit
  messages.
- **Go.** `go 1.26`, because `github.com/hashicorp/nomad/api` requires it.
  - Standard library first; `log/slog` for logging.
  - Wrap errors with context (`fmt.Errorf("…: %w", err)`). No panics in library code.
- **Layering** (ADR-0021, enforced by depguard in `.golangci.yml`):
  - `api/...` imports only the standard library and other `api/` packages.
  - Only `cmd/tent` imports provider packages (`internal/cloud/vultr`, `internal/cloud/hetzner`, …); everything else,
    `internal/model`, `internal/rollout` and `internal/app` included, uses the interfaces in `internal/cloud`.
    Tests and the provider packages themselves are exempt.
  - Cloud SDKs (govultr, hcloud-go) are imported only by their provider package, tests included.
  - `internal/nodeup` (the tent-node agent) never imports `internal/cloud/...`.
  - Only `internal/nomadops` imports `github.com/hashicorp/nomad/api`.
  - Never import the root module `github.com/hashicorp/nomad`; it is BUSL-licensed.
  - `internal/pki` imports only the standard library, `internal/uuid` and `api/v1alpha1`; `internal/uuid`,
    `internal/english` and `internal/secrettest` import only the standard library. Their tests are exempt (ADR-0025).
  - Only tests import `internal/secrettest`.
  - Only `internal/assets` imports `github.com/ProtonMail/go-crypto`, tests included (ADR-0026).
  - `internal/nodeup`, `internal/nodeconfig` and `cmd/tent-node`, tests included, import neither `internal/assets`
    nor `internal/channels`; tent-node gets its assets in NodeConfig (ADR-0026).
  - `internal/channels` imports only the standard library, `sigs.k8s.io/yaml`, `sigs.k8s.io/json` and
    `golang.org/x/mod/semver`; its tests are exempt (ADR-0026).
- **Visibility.** Everything is under `internal/` except the public API types in `api/`.
- **Weakest primitives.** Core mechanisms assume the weakest cloud primitives: non-unique names, no fixed IPs, no
  graceful shutdown. Richer primitives are optimizations behind `Capabilities` (ADR-0015 to ADR-0017).
- **Credentials.** Cloud credentials (`VULTR_API_KEY`, `HCLOUD_TOKEN`) never go into specs, the state store, logs or
  nodes.
- **Idempotency.** Every mutating operation must be safe to re-run after an interruption.
  - Deterministic names.
  - Adopt a resource only when its ownership markers match.
  - Operation ids where names are not unique; never let an SDK blindly retry a create.
  - Create the replacement before removing the old node.
- **Tests.**
  - Unit tests sit next to the code, with golden files under `testdata/`.
  - Every engine task needs an "apply → re-plan → no-op" test.
  - E2E tests use the `e2e` build tag and never run by default.
- **Checks.** `make check` runs fmt, lint, licenses, test and build; `make fmt` and `make lint` need golangci-lint at
  the version pinned in the Makefile. Releases follow ADR-0020.

## Maintainer decisions

Decided on 2026-09-25 and 2026-09-28 (`docs/architecture.md` §18, the table in `docs/roadmap.md`):

- labels use the prefix `tent/`, and the API group is `tent/v1alpha1`;
- `access.api` defaults to `[0.0.0.0/0]` (mTLS + ACL), with a loud warning while it is open;
- node group roles are `server`, `client` and `combined` (ADR-0019);
- the default OS image is `ubuntu-24.04`; E2E also runs on `ubuntu-26.04`;
- Consul and Vault are out of v1;
- tent is licensed under Apache-2.0;
- a cluster's provider and region never change; a cluster moves by creating a new one (decided on 2026-09-28);
- the cluster CA is valid for 10 years, until CA rotation exists (decided on 2026-09-28).

Anything else that only the maintainer can decide goes into `docs/architecture.md` §18. Ask before implementing it.

## Vultr spike

`hack/vultr-spike/spike.sh` checks undocumented Vultr behaviour against a real account (see its README).

- Never paste API keys into chat or commit them. The script reads `VULTR_API_KEY` from the environment.
- After a run, copy the findings into `docs/platform-notes.md` §3 and resolve the provisional items of ADR-0018.

## ADR workflow

For a new significant decision:
1. Copy `docs/adr/template.md` to `docs/adr/NNNN-short-title.md`.
2. Add it to the index in `docs/adr/README.md`.
3. Update `docs/architecture.md` in the same change.

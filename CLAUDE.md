# CLAUDE.md

Guidance for Claude Code in this repository.

## Project

tent is a Go CLI that provisions and operates HashiCorp Nomad clusters on cloud providers: kops for Nomad. The module
path is `github.com/ingvarch/tent`.

Providers, in order:
1. **Vultr** is implemented first and runs the E2E suite (`docs/adr/0014-vultr-first-provider-and-e2e.md`).
2. **Hetzner Cloud** is second.
3. **AWS** comes later. It must remain possible without changes to the core.

**Status:** M0 Foundation (2026-09-27), M1 Vultr infrastructure (2026-09-28) and M2 Nomad bootstrap (2026-10-07) are
complete. v0.1.0, the first release that builds a Nomad cluster, was tagged on 2026-10-07: archives, `.deb` and `.rpm`
packages, a Homebrew cask (`ingvarch/tap/tent`), and the tent-node binaries that its nodes download.
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
  `docs/architecture.md` §13). Until M2.7a the nodes were empty machines without Nomad. The
  exit criteria were met in the integration tests (`internal/app/integration_test.go`, `interrupt_test.go`) and on a
  real Vultr account.
- M2 Nomad bootstrap was built in parts M2.1 to M2.9 (`docs/roadmap.md`).
  - M2.1 added the cluster PKI and secrets (`internal/pki`, ADR-0024): `update` makes the CA, the gossip key and the
    ACL bootstrap secret once and keeps them in the state store; `delete` removes them.
  - M2.2 added the release channels and assets (`internal/channels`, `internal/assets`, ADR-0026): the embedded
    `stable` channel, the Nomad version pin in `cluster.completed.yaml`, and Nomad, CNI and tent-node downloads
    verified by signature or sha256.
  - M2.3 added NodeConfig (`internal/nodeconfig`, `internal/app/nodeconfig.go`, ADR-0027): the model's join strategy
    and rules between nodes, the NodeConfig types and strict codec, the Nomad agent configuration with golden files,
    the spec hash and the cloud-config within 24 KiB.
  - M2.4 added nomadops (`internal/nomadops`, `nomadfake`): the mTLS client of a server's HTTP API, the ACL
    bootstrap that is safe to repeat, intro tokens, the leader, the nodes with their addresses, the servers of the Raft
    configuration (`Peers`, M2.7b), autopilot health and waits over them.
    `nomad/api` is pinned at the commit of Nomad v2.0.7 and moved by hand.
  - M2.5 added tent-node (`cmd/tent-node`, `internal/nodeup`, ADR-0028): `install`, `up`, `version` and a stub
    `refresh-join`; the phase runner over a filesystem and exec, with fakes in `nodeuptest`; the Vultr metadata
    client; the systemd units; `status.json`; and the phases `preflight`, `system` and `verify`, the others being
    stubs. NodeConfig names the provider. The release ships `tent-node_linux_amd64` and `_arm64`; `make dev-upload`
    puts a development build into the CI R2 bucket (`hack/tent-node-upload`).
  - M2.6a added the machine phases of tent-node (`internal/nodeup`, ADR-0029): `hostfirewall` (tent's nftables table,
    ufw off, the metadata service reachable only from tent-node's marked socket, maintainer decision 21), `runtime`
    (Ubuntu's `docker.io` and `daemon.json`) and `cni` (the CNI plugins from a verified, cached download).
    `ExecRunner` stops a program's whole process group. `internal/app` gives client and combined nodes host firewall
    rules for traffic from Nomad's and Docker's bridges. `join` and `nomad` were still stubs.
  - M2.6b added the Nomad phases of tent-node (`internal/nodeup`, ADR-0030). NodeConfig gains `region` (maintainer
    decision 22) and `nomad.service` as a NodeConfig file (`internal/nodeconfig`); `00-tent.hcl` drops
    `drain_on_shutdown` (maintainer decision 25), so clients come back eligible after a reboot. The phases `join`
    (`05-join.hcl` from the servers' live peers over mTLS, or else the last known peers and the seed), `nomad` (the
    binary from the asset cache, the agent's files, a start, and a restart only after a change; never enabled for
    boot) and `verify` (the local agent's health); the command `refresh-join` (server and combined nodes ask their own
    agent first), with a lock between `up` and `refresh-join`. The weekly online job runs `nomad config validate` on
    the goldens (`internal/assets`, maintainer decision 23). `hack/tent-node-userdata` builds its node through
    `app.NodeConfigOf`, and the spike's `tentnode` check (v8) boots Nomad. The VM check ran on 2026-10-05 with one
    finding, accepted as a trade-off: a restart of Docker restarts Nomad's docker tasks. The online job passed
    on Linux in the scheduled CI run of 2026-10-05.
  - M2.7a added the bootstrap in `update` (`internal/app`, `internal/nomadops`, ADR-0031): `tent update cluster --yes`
    builds a Nomad cluster: servers, then the Nomad step (leader, ACL bootstrap with the stored secret, healthy
    servers, the mark `nomad/bootstrapped`), then clients with intro tokens until Nomad lists them. `cmd/tent` reads
    `TENT_NODE_URL` and `TENT_NODE_SHA256`. Maintainer decision 26: `leave_on_terminate` is false on server and
    combined agents, so `nomad.service` ends as failed after a stop of a server (seen on a real server on 2026-10-05).
    The real-cloud check of the whole flow passed on Vultr (spike v9, run `qypvsk`).
  - M2.7b added the scrub and the delete guard (`internal/app`, `internal/cloud`, `internal/nomadops`, ADR-0032): once a
    node has joined, `update` replaces its user data with a stub and labels its machine `tent/joined=true`
    (`Nodes.MarkJoined`, one PATCH on Vultr); a machine without the label is waited for; a client without it that is
    older than 31 minutes and that Nomad does not list is deleted and created again (decision 28); and `update` refuses
    to delete a node that joined (decision 27). A node is told by its name and private address. The real-cloud check
    ran on Vultr on 2026-10-06 (spike v10 run `rugw2m`, v11 runs `sv3vwb` and `rgfckj`). It found that cloud-init
    read the stub as degraded and that the account's instance limit refused the replacement's create right after
    the delete; both are fixed in the code (a line `{}` in the stub, a retry in the Vultr provider).
  - M2.8 added `tent export nomad`, `tent ui` and `tent validate cluster` (`internal/app`, `internal/cli`,
    `internal/nomadops`, `internal/shellenv`, ADR-0033; maintainer decisions 29 to 32), a warning about combined
    clusters, and removed `hack/tent-operator`. The three commands need `VULTR_API_KEY`. The real-cloud check ran on
    Vultr on 2026-10-07 (spike v12, run `9pxbqn`); its one unexpected row was a fault of the script, which is fixed.
  - M2.9 added the E2E suite on Vultr (`test/e2e`, `test/e2e/vultrapi`, `test/e2e/janitor`, `hack/e2e-janitor`,
    ADR-0034): the scenario `smoke` builds a cluster per image (ubuntu-24.04 and ubuntu-26.04) and is built to check
    the four exit criteria of M2, and a janitor deletes the objects of `e2e-` clusters that are older than 3 hours.
    `make e2e` runs it from the maintainer's machine, not in CI. The labels `tent/e2e` and `tent/e2e-run` are removed.
    The runs of 2026-10-07 found four faults, all fixed: a node refuses a tent-node of another version than its
    tent's (`make e2e` builds both with one version); `update` asked for an intro token before Nomad's keyring had a
    key (the Nomad step now waits for it); the suite's `validate` lacked `--allow-single-server`; and the probe's one
    control site, `archive.ubuntu.com`, did not answer over IPv4 (it tries three now). The next three runs passed on
    both images (`r7l48w`, `58sglh`, `g57k85`): that is the exit of M2.
- M3 Day-2 operations is in progress, in parts M3.1 to M3.9 (`docs/roadmap.md`).
  - M3.1 added the rollout decisions (`internal/rollout`, ADR-0035; maintainer decisions 33 to 35): `Next(State,
    Mode)` returns one step of a rolling update (`Roll`) or of a removal of surplus nodes (`Shrink`) from what the
    cloud and Nomad report, and reaches nothing itself. A simulator with golden step sequences, invariants and a
    resume test from every state proves it. It also added `spec.rollingUpdate` of a node group (`maxSurge`,
    `maxUnavailable`, `drainTimeout`), which never changes the spec hash. Nothing calls the decisions yet: M3.3 wires
    them into `tent rolling-update cluster`. Two voters and a single server are refused until the maintainer chooses
    (ADR-0035).

## Read before changing anything

1. `docs/architecture.md`: the design and the source of truth.
2. `docs/adr/`: accepted decisions. Do not diverge silently. If an implementation must deviate, write a superseding
   ADR first (see `docs/adr/README.md`).
3. `docs/platform-notes.md`: verified Nomad, Hetzner and Vultr API facts and quirks as of 2026-09-25, facts about
   Ubuntu on nodes as of 2026-09-29 (restarts of Docker, containerd and Nomad as of 2026-10-05, cloud-init and the
   stub as of 2026-10-06), and about the Nomad agent on a node as of 2026-10-05.
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
    `internal/model` and `internal/app` included, uses the interfaces in `internal/cloud`.
    Tests and the provider packages themselves are exempt.
  - Cloud SDKs (govultr, hcloud-go) are imported only by their provider package, tests included.
  - `internal/rollout` imports only the standard library, `api/v1alpha1`, `internal/english` and
    `golang.org/x/mod/semver`; its tests are exempt (`rollout-pure`, ADR-0035).
  - `internal/nodeup` (the tent-node agent), `internal/nodeconfig` and `cmd/tent-node`, tests included, never import
    `internal/cloud/...`.
  - `internal/nodeup` and `cmd/tent-node` import only the standard library, `internal/nodeup/...`,
    `internal/nodeconfig`, `internal/secret`, `internal/buildinfo` and `api/v1alpha1` (`tent-node-allowed`); their
    tests are exempt (ADR-0028). A test in `internal/buildconfig` checks all that `./cmd/tent-node` links.
  - Only `internal/nomadops` imports `github.com/hashicorp/nomad/api`; `internal/nomadops/nomadfake`, tests
    included, imports no Nomad module.
  - Never import the root module `github.com/hashicorp/nomad`; it is BUSL-licensed.
  - `test/e2e` and `hack/e2e-janitor`, tests included, import nothing under `github.com/ingvarch/tent/internal`: the
    suite drives tent from outside and reads the clouds with its own clients (`e2e-black-box`, ADR-0034).
  - `internal/pki` imports only the standard library, `internal/uuid`, `internal/secret` and `api/v1alpha1`;
    `internal/uuid`, `internal/secret`, `internal/english`, `internal/secrettest` and `internal/assets/assetstest`
    import only the standard library. Their tests are exempt (ADR-0025, ADR-0027, ADR-0031).
  - `internal/nodeconfig` imports only the standard library, `internal/secret` and `api/v1alpha1`; its tests are
    exempt (ADR-0027).
  - Only tests import `internal/secrettest`, `internal/nomadops/nomadfake`, `internal/nodeup/nodeuptest`,
    `internal/s3url/s3urltest`, `internal/assets/assetstest`, `internal/shellenv/shellenvtest`,
    `test/e2e/janitor/janitortest` and `github.com/hashicorp/hcl`.
  - Only `internal/assets` imports `github.com/ProtonMail/go-crypto`, tests included (ADR-0026).
  - `internal/nodeup`, `internal/nodeconfig` and `cmd/tent-node`, tests included, import neither `internal/assets`
    nor `internal/channels`; tent-node gets its assets in NodeConfig (ADR-0026).
  - `internal/channels` imports only the standard library, `sigs.k8s.io/yaml`, `sigs.k8s.io/json` and
    `golang.org/x/mod/semver`; its tests are exempt (ADR-0026).
  - `internal/s3url`, which the s3 state store and `hack/tent-node-upload` share, imports only the standard library,
    aws-sdk-go-v2's `aws`, `config` and `service/s3`, and `github.com/aws/smithy-go/logging`; its tests are exempt
    (ADR-0028).
- **Visibility.** Everything is under `internal/` except the public API types in `api/` and the E2E suite's own packages
  under `test/e2e/` (ADR-0034).
- **Weakest primitives.** Core mechanisms assume the weakest cloud primitives: non-unique names, no fixed IPs, no
  graceful shutdown. Richer primitives are optimizations behind `Capabilities` (ADR-0015 to ADR-0017).
- **Credentials.** Cloud credentials (`VULTR_API_KEY`, `HCLOUD_TOKEN`) never go into specs, the state store, logs or
  nodes.
- **Idempotency.** Every mutating operation must be safe to re-run after an interruption.
  - Deterministic names.
  - Adopt a resource only when its ownership markers match.
  - Operation ids where names are not unique; never let an SDK blindly retry a create.
  - Create the replacement before removing the old node. The one exception is a client that never registered: it is
    deleted before its replacement is created (decision 28, ADR-0032).
- **Tests.**
  - Unit tests sit next to the code, with golden files under `testdata/`.
  - Every engine task needs an "apply → re-plan → no-op" test.
  - E2E tests use the `e2e` build tag and never run by default. `make e2e` runs them from the maintainer's machine
    and needs `VULTR_API_KEY`, the R2 keys and `TENT_DEV_S3_URL` (`test/e2e/README.md`).
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

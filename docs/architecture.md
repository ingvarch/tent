# tent: Architecture

> **Status:** accepted design, 2026-09-25. Revised the same day: Vultr is now implemented first and hosts the E2E
> suite, and Hetzner Cloud is second ([ADR-0014](adr/0014-vultr-first-provider-and-e2e.md)). **Scope:** v1 covers
> Vultr and Hetzner Cloud, with AWS kept in mind.
>
> - Why each decision was made: [ADRs](adr/README.md).
> - Verified platform facts and sources: [platform notes](platform-notes.md).
> - Milestones and current status: [roadmap](roadmap.md).
>
> This document describes the design as it should be. If code and this document disagree, fix one of them. A
> deliberate change of direction needs a new ADR.

## Contents

1. [Overview](#1-overview)
2. [Design principles](#2-design-principles)
3. [Domain model and API](#3-domain-model-and-api)
4. [System architecture](#4-system-architecture)
5. [Repository layout and dependency rules](#5-repository-layout-and-dependency-rules)
6. [Reconciliation engine](#6-reconciliation-engine)
7. [Provider abstraction](#7-provider-abstraction)
8. [Nodes: tent-node and NodeConfig](#8-nodes-tent-node-and-nodeconfig)
9. [Security](#9-security)
10. [State store and locking](#10-state-store-and-locking)
11. [Vultr provider](#11-vultr-provider)
12. [Hetzner provider](#12-hetzner-provider)
13. [Lifecycle flows](#13-lifecycle-flows)
14. [CLI](#14-cli)
15. [Testing](#15-testing)
16. [Technology stack and releases](#16-technology-stack-and-releases)
17. [Risks](#17-risks)
18. [Open questions](#18-open-questions)
- [Appendix A: Nomad agent configuration sketches](#appendix-a-nomad-agent-configuration-sketches)
- [Appendix B: cloud-init user data sketch](#appendix-b-cloud-init-user-data-sketch)

---

## 1. Overview

tent is a CLI that turns a declarative cluster specification into a running, secured HashiCorp Nomad cluster on a
cloud provider. It then keeps operating that cluster: scaling, Nomad-aware rolling updates, upgrades, backups and
teardown. tent is to Nomad what kops is to Kubernetes.

The name is the only metaphor: a nomad pitches a tent anywhere, and tent pitches a Nomad cluster on any cloud. The
API uses plain terms such as `Cluster` and `NodeGroup`.

The order of providers:
1. **Vultr** is implemented first and runs the E2E suite.
2. **Hetzner Cloud** comes second. Its design below is complete and waits for an account that can run E2E again.
3. **AWS** comes later.

See [ADR-0014](adr/0014-vultr-first-provider-and-e2e.md).

### 1.1 Goals (v1)

- **Two providers**, Vultr and Hetzner Cloud, both with production-grade defaults: 3 or 5 Nomad servers and any
  number of client node groups.
- **Secure bootstrap:** mTLS for RPC and HTTP, gossip encryption, ACLs, client introduction and per-node
  certificates.
- **A plan/apply workflow.** Every operation is idempotent and safe to interrupt. The only state lives in the state
  store and the cloud itself.
- **Day-2 operations:** scale, rolling update, upgrade, validate, backup and restore, and a delete that leaves nothing
  behind.
- **Room for more clouds.** AWS and others can be added as providers without changes to the core.

### 1.2 Non-goals (v1)

- **Consul and Vault.** They may come later as optional components ([ADR-0011](adr/0011-nomad-only-scope-and-licensing.md)).
- **Autoscaling and automatic replacement of failed nodes.** Both need an in-cluster controller; later.
- **Private topology**, meaning nodes without public IPs behind NAT. A later milestone; Vultr's managed NAT gateway
  makes it easier there.
- **Multi-region Nomad federation, Windows clients and Nomad Enterprise features.**
- **Managing workloads**, except a small set of optional addons later.

### 1.3 Constraints that shape the design

These were verified on 2026-09-25. Details and sources are in [platform notes](platform-notes.md).

**Nomad**
- **Versions.** Nomad CE 2.0.7 is current.
  - Since 1.11, client introduction lets servers require a short-lived token for a client's first registration.
  - `server.retry_join` will be removed in 2.1. Only the `server_join {}` block remains.
- **Endpoints tent relies on.**
  - `GET /v1/status/peers` needs no ACL token. mTLS still applies.
  - There is no HTTP endpoint that makes an agent leave gracefully.
- **Licensing.** Nomad is licensed under BUSL 1.1 since 1.7. The Go API module `github.com/hashicorp/nomad/api` is
  MPL-2.0.
- **Cloud auto-join** (go-discover) supports neither Vultr nor Hetzner.

**Vultr**
- **Missing primitives.**
  - Resource names are not unique, and create calls have no idempotency key.
  - Tags are plain strings and exist only on instances.
  - No fixed private IP can be requested in a VPC.
  - The API has no graceful shutdown: `halt` is a hard power-off.
  - No availability zones and no placement spread.
  - No signed instance identity.
- **Limits and behaviour.**
  - The `user_data` size limit is undocumented, but user_data can be changed after creation.
  - A firewall group filters public traffic only, and an instance can have just one.
  - API rate limit: 30 requests per second per IP.
  - Billing is hourly with a one-hour minimum.
  - New-account limits are opaque.

**Hetzner Cloud**
- **Missing primitives.** No autoscaling groups, no IAM and no signed instance identity.
- **Firewalls** filter only public interfaces and do not apply to load balancers.
- **API behaviour.**
  - Rate limit: 3600 requests per hour per project.
  - `user_data` is at most 32 KiB and cannot be changed.
  - A fixed private IP can only be requested through `attach_to_network`.
- **Capacity.**
  - New accounts start with a limit of 5 servers.
  - Since June 2026 Hetzner restricts server creation for some accounts.

**Market**
- No existing tool covers the full Nomad cluster lifecycle (a "kops for Nomad").

---

## 2. Design principles

| # | Principle | ADR |
|---|---|---|
| 1 | **Desired state lives in the state store; actual state lives in the cloud.** There is no tfstate. Resources are found by ownership markers. | [0003](adr/0003-cloud-is-source-of-truth.md) |
| 2 | **Plan, then apply.** Mutating commands print a plan by default, and `--yes` applies it. | [0002](adr/0002-direct-cloud-apis-and-own-engine.md) |
| 3 | **Nodes are immutable.** A changed node configuration means the node is replaced. tent never SSHes into live nodes to change them. | [0005](adr/0005-immutable-nodes-and-nomad-aware-rollouts.md) |
| 4 | **The lifecycle is Nomad-aware.** No node is removed without a drain, and no server without a Raft quorum check. Replacements are created before old nodes go, so a crash leaves a surplus node, not a missing one. | [0005](adr/0005-immutable-nodes-and-nomad-aware-rollouts.md), [0017](adr/0017-api-driven-server-removal.md) |
| 5 | **The core knows nothing about specific clouds, and providers know nothing about Nomad.** The core expresses intents, and providers map them to native resources. | [0004](adr/0004-layered-architecture.md) |
| 6 | **Core mechanisms assume the weakest cloud primitives.** Unique names, fixed IPs and graceful shutdown are optimizations switched on by capabilities, never assumptions. | [0015](adr/0015-idempotency-without-unique-names.md), [0016](adr/0016-server-discovery-seed-and-refresh.md), [0017](adr/0017-api-driven-server-removal.md) |
| 7 | **Secure by default.** mTLS everywhere, gossip encryption, ACLs and client introduction. Cloud credentials never reach nodes. | [0007](adr/0007-security-baseline.md), [0008](adr/0008-node-credential-delivery.md) |
| 8 | **Testability is an architectural requirement.** The cloud and Nomad sit behind interfaces with fakes. We use golden tests and real-cloud E2E runs with a janitor. | [0012](adr/0012-testing-strategy.md), [0014](adr/0014-vultr-first-provider-and-e2e.md) |
| 9 | **Every command is idempotent and safe to interrupt.** It can be re-run after Ctrl-C or a crash and will converge. | [0003](adr/0003-cloud-is-source-of-truth.md), [0015](adr/0015-idempotency-without-unique-names.md) |

---

## 3. Domain model and API

### 3.1 Kinds

The API has two kinds, similar to `Cluster` and `InstanceGroup` in kops. Both live in the state store and can be
kept in a single multi-document YAML file for `tent apply -f`.

A Vultr cluster:

```yaml
apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: prod                     # [a-z][a-z0-9-]{0,18}[a-z0-9]; prefix of every resource name
spec:
  channel: stable                # recommended versions and images (see 13.5)
  cloud:
    provider: vultr
    region: ams                  # Vultr: region | Hetzner: network zone | AWS: region
    zones: [ams]                 # Vultr has no zones: left out, it defaults to [region], the only valid value
    vultr: {}                    # provider-specific block, optional; no fields yet
  networking:
    cidr: 10.64.0.0/16           # VPC CIDR, a private IPv4 range; the default
  access:
    ssh: [203.0.113.7/32]        # empty = SSH closed on the cloud firewall
    api: [0.0.0.0/0]             # the default; :4646 is protected by mTLS + ACL, and tent warns while it is open
  sshKeys:
    - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILVMgcq7nf63leSBwZNfB40Oi4XwSKWNKchNmRGNCb9k ops@example
  nomad:
    version: 2.0.7
    region: global
    tls: {verifyHTTPSClient: true}
    clientIntroduction: strict   # strict | warn | none
    extraConfig:                 # escape hatch, rendered into 99-user.hcl
      server: ""
      client: ""
---
apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: servers
  cluster: prod
spec:
  role: server                   # server | client | combined (ADR-0019)
  machineType: vc2-2c-4gb        # provider-native plan id
  image: ubuntu-24.04            # resolved by the provider (Vultr: os_id 2284)
  size: 3
---
apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: workers
  cluster: prod
spec:
  role: client
  machineType: vc2-2c-4gb
  image: ubuntu-24.04
  size: 3
  nomad:
    nodePool: default
    nodeClass: general
    drivers: [docker, exec]
    meta: {team: platform}
```

The same cluster on Hetzner differs only in the provider-specific parts:

```yaml
spec:
  cloud:
    provider: hetzner
    region: eu-central           # network zone
    zones: [fsn1, nbg1, hel1]    # locations, required: one server per location survives the loss of a location
    hetzner: {}                  # provider-specific block, optional; no fields yet
# NodeGroup: machineType: cx23 / cx33
```

Fields for later milestones (private topology, the API load balancer, rolling update settings, Hetzner placement and
IP options) join the types with those milestones. ACLs are always on ([ADR-0007](adr/0007-security-baseline.md)), so
there is no `acl` field.

### 3.2 Concept mapping

This table is also the check that the abstraction survives several providers.

| Concept | tent | Nomad | Vultr | Hetzner | AWS (later) |
|---|---|---|---|---|---|
| Cluster | `Cluster` | region | instances tagged + resources marked for the cluster | resources labelled `tent/cluster=<name>` | VPC and tagged resources |
| Region | `cloud.region` | — | region (`ams`) | network zone (`eu-central`) | region (`eu-central-1`) |
| Failure domain | `cloud.zones[]` | `datacenter` | none (`zones = [region]`) | location (`fsn1`) | availability zone |
| Group of nodes | `NodeGroup` | `node_pool`, `node_class`, `meta` | tagged instances | labelled servers plus a placement group | EC2 instances, later an ASG |
| Perimeter | `access` intents | — | a servers firewall group for the server or combined group, a clients group only with a client group (public only) + host nftables | Cloud Firewalls (public only) + host nftables | Security Groups |
| Server discovery | join strategy | `server_join` | seed list + tent-node refresh | fixed private IP slots | cloud auto-join by tags |
| Node identity | — | mTLS plus intro token | none (secrets via user data, scrubbed after bootstrap) | none (secrets via user data) | instance identity document plus IAM role |
| State store | `--state` | — | Object Storage (S3 API) or any S3 | Object Storage (S3 API) or any S3 | S3 |

### 3.3 API rules

- **API version.** User-facing kinds use `apiVersion: tent/v1alpha1` (group decided on 2026-09-25).
  Every stored object carries its apiVersion. Conversion functions are added when a second version appears.
- **User spec and completed spec.** The user spec is what the operator wrote. The state store keeps it as given,
  without defaults, but written in tent's field order, so comments and formatting are not kept. The completed spec
  has every default filled in and records what was last applied; `update` writes it from M1 on
  ([13.2](#132-tent-update-cluster---yes)). `tent get --full` does not read it yet: it prints the user spec with the
  defaults filled in.
- **Strict decoding.** Keys are case-sensitive, and unknown fields, duplicate keys and null values are errors: an empty
  value such as `vultr:` is almost always a forgotten entry, so write `vultr: {}`. Decoding errors name the document,
  and the line when the file has the key, for example `document 2 (NodeGroup): line 37: unknown field "spec.sizee"`.
  Validation errors carry field paths, for example `NodeGroup servers: spec.size: must be 1, 3 or 5 for role=server`.
- **Roles.** A node group's role is `server`, `client` or `combined`. A cluster has exactly one group whose role is
  `server` or `combined`, and its size is 1, 3 or 5. `combined` runs server and client in one agent, for dev and
  small clusters, and counts as a server everywhere ([ADR-0019](adr/0019-combined-server-client-role.md)).
- **JSON Schema.** The schema of both kinds lives at `api/v1alpha1/tent.schema.json`, and `make generate` rewrites it
  from the Go types ([ADR-0022](adr/0022-json-schema-from-go-types.md)). Editors with the YAML language server check
  and autocomplete a spec file whose first line is:

  ```yaml
  # yaml-language-server: $schema=https://raw.githubusercontent.com/ingvarch/tent/main/api/v1alpha1/tent.schema.json
  ```

  The schema checks field names, types, required fields and allowed values. Name and CIDR patterns, sizes and
  provider rules are checked by tent only.
- **Provider-native values.** Machine types and images are provider-native strings, with no "small/medium"
  abstractions. The provider validates them against the live API: the type exists and is available in the region
  or location, and the image architecture matches. Images are given by name (`ubuntu-24.04`), and the provider
  resolves them (Vultr: numeric `os_id`).
- **Server groups.** v1 allows exactly one server group, of size 1, 3 or 5. Size 1 requires `--allow-single-server`.
- **Fixed cloud.** A cluster's `cloud.provider` and `cloud.region` never change (decided on 2026-09-28,
  [18](#18-open-questions)). tent made the cluster's cloud objects on that provider and in that region, so a change
  would leave them there: `update` would build the cluster again elsewhere, and `delete cluster` could miss them.
  `replace` and `edit` refuse the change with a field error, such as `Cluster prod: spec.cloud.provider: cannot change
  from vultr to hetzner; a cluster moves by creating a new one`. They compare the values with their defaults. A stored
  `cluster.yaml` that does not decode cannot be replaced, since its cloud is unknown. Every command that reads it then
  fails and says how to repair it: fix the file in the state store by hand and keep its `spec.cloud.provider` and
  `spec.cloud.region`, since deleting the file would leave the cluster's cloud objects behind.
- **Zones.** A group's `zones` must be a subset of the cluster zones and defaults to all of them. Nodes are spread so
  that per-zone counts stay balanced. On Vultr, `cloud.zones` left out defaults to `[cloud.region]`, the only value it
  accepts, and `validate` warns that the cluster has a single failure domain. On Hetzner, `cloud.zones` lists
  locations and is required.
- **Escape hatch.** `nomad.extraConfig` is rendered verbatim into `99-user.hcl`. Nomad merges configuration files in
  lexicographic order, so user settings win. This is documented as unsupported.
- **No secrets in specs.** Cloud credentials come from environment variables. SSH keys are public keys only.

### 3.4 Naming and ownership markers

**Names** are deterministic on every provider.
- On Hetzner they are unique and double as an idempotency guard.
- On Vultr they are only readable handles, and idempotency comes from operation ids
  ([ADR-0015](adr/0015-idempotency-without-unique-names.md)).

| Object | Rule / pattern | Notes |
|---|---|---|
| Cluster name | `^[a-z][a-z0-9-]{0,18}[a-z0-9]$` (≤ 20), except `con`, `prn`, `aux`, `nul`, `com1`–`com9` and `lpt1`–`lpt9` | prefix of every resource name. Names become state store keys, and Windows reserves the excluded ones ([10.1](#101-backends)). |
| NodeGroup name | same rules (≤ 20) | a state store key too |
| Machines | `<cluster>-<group>-<index>` (≤ 63) | Hetzner servers: index = slot. Otherwise the lowest free index. Also the hostname and the Nomad node name. |
| Network | `<cluster>` | Vultr: VPC description carries the ownership marker |
| Firewalls | Hetzner: `<cluster>-nodes`, `<cluster>-servers`; Vultr: `<cluster>-servers`, `<cluster>-clients` | Vultr: the clients' group exists only with a client group ([11.5](#115-firewall-and-host-firewall)) |
| Placement groups (Hetzner) | `<cluster>-<group>-<shard>` | shards of ≤ 10 servers |
| Load balancers | `<cluster>-api`, `<cluster>-internal` | optional |
| SSH key | `<cluster>-<fp>` | Vultr: `fp` is the first 8 hex digits of the SHA-256 digest of the key's decoded data: the digest that `ssh-keygen -l` prints in base64 (`SHA256:…`), written in hex. Hetzner: an existing key with the same fingerprint is adopted, never deleted |
| Lock | Hetzner only: `<cluster>-lock` (an empty firewall) | see [10.4](#104-locking) |

**Canonical labels** are the provider-neutral ownership model. The key prefix is `tent/` (decided on 2026-09-25).

| Label | Value | Meaning |
|---|---|---|
| `tent/cluster` | cluster name | the only ownership signal |
| `tent/nodegroup` | group name | machines, placement groups |
| `tent/role` | `server` / `client` / `combined` | machines |
| `tent/spec-hash` | 16 hex chars | semantic hash of the node configuration ([8.4](#84-nomad-configuration-rendering)) |
| `tent/slot` | `0`–`6` | Hetzner Nomad servers only |
| `tent/op` | UUID | operation id of the create call ([ADR-0015](adr/0015-idempotency-without-unique-names.md)) |
| `tent/lock-for` | cluster name | the Hetzner lock firewall only; it deliberately has no `tent/cluster` |
| `tent/e2e`, `tent/e2e-run` | `true`, run id | resources created by E2E tests |

**How labels are encoded per provider:**

- **Hetzner:** native key/value labels on every resource. Label selectors filter on the server side.
- **Vultr:**
  - *Instances.* Each canonical label becomes one string tag. `GET /v2/instances?tag=` filters by a single tag on the
    server side (the cluster tag); everything else is filtered on the client.
  - *Other resources* (VPC, firewall group, load balancer, SSH key) have no tags. A marker such as
    `tent:cluster=prod;kind=vpc` goes into their one free-text field (description, label or name), and tent filters on
    the client.
  - *Tag syntax.* Tags are `key=value`, stored verbatim (for example `tent/cluster=prod`). They are always
    lower-case, because Vultr's tag filter is an exact but case-insensitive match (spike 2026-09-25,
    [ADR-0018](adr/0018-vultr-provider-design.md)).
  - *Where it lives.* The codec is the only code that knows the exact tag syntax: `internal/cloud/vultr/labels.go`.

**Nomad-side identity:**
- the node name is the hostname, which is the cloud machine name;
- `datacenter` is the node's zone (`fsn1` on Hetzner, `ams` on Vultr);
- clients get node meta `tent_cluster`, `tent_nodegroup` and `tent_instance_id`.

Since Nomad 1.5 jobs default to `datacenters = ["*"]`, so mapping zones to datacenters costs nothing, and
`spread { attribute = "${node.datacenter}" }` spreads workloads where there are several zones.

---

## 4. System architecture

```
 operator / CI
      │
┌─────▼──────────────────────────── tent (CLI) ──────────────────────────────┐
│ cli (cobra) ─► app: use cases (create / update / rollout / validate / ...)  │
│                  │                                                          │
│   statestore ◄───┼──► model (spec → intents, NodeConfig) ──► pki           │
│   (spec, PKI,    │         │                                                │
│    lock)         │         ▼                                                │
│                  │   cloud.Provider ──► engine (plan/apply DAG)             │
│                  │   ├─ vultr   (first)                                     │
│                  │   └─ hetzner (second; aws later)                         │
│                  └──► rollout / validate ──► nomadops (ACL, raft, drain)   │
└──────┬───────────────────┬──────────────────────────┬──────────────────────┘
       ▼                   ▼                          ▼
  S3 / file          Cloud API                Nomad API :4646 (mTLS + ACL)
                           │ VM + user_data(NodeConfig)
                           ▼
               cloud-init ─► tent-node ─► nomad (systemd)
```

### 4.1 Three responsibilities

This split is the central decision.

1. **Infrastructure**: network, firewalls, placement groups, load balancers and SSH keys. It is not Nomad-aware and is
   a graph of provider-implemented tasks run by the engine ([§6](#6-reconciliation-engine)).
2. **Node lifecycle**: create, replace and remove machines.
   - It is Nomad-aware: it drains nodes and respects Raft quorum.
   - It lives in the core (`internal/rollout`) and calls only provider primitives: `List`, `Create`, `Stop` and
     `Delete`.
   - Nodes are **not** tasks in the engine graph. In kops the Hetzner "ServerGroup" is a task, so replacing one node
     re-runs the whole plan.
3. **Nomad configuration**: agent config, TLS material, ACL bootstrap, node pools and the rest of day-1 setup. It
   does not depend on the cloud and lives in the core.

### 4.2 Two binaries

- `tent` is the CLI for operators and CI.
- `tent-node` is the node agent that cloud-init runs on each VM, similar to kops' `nodeup`. Its version always matches
  the CLI that created the node exactly.

---

## 5. Repository layout and dependency rules

```
github.com/ingvarch/tent
├── api/v1alpha1/        # public types: Cluster, NodeGroup, defaults, validation (stdlib only); tent.schema.json
├── cmd/
│   ├── tent/            # CLI entrypoint; registers providers
│   └── tent-node/       # node agent entrypoint
├── internal/
│   ├── cli/             # cobra commands, flags, output (table|yaml|json): a thin layer
│   ├── spec/            # multi-document YAML specs: strict decoding with file lines, encoding in field order
│   ├── apischema/       # generates api/v1alpha1/tent.schema.json (make generate); not linked into tent
│   ├── licenses/        # licence check and THIRD_PARTY_NOTICES (make licenses, make notices); not linked into tent
│   ├── app/             # use cases; used by the CLI, e2e tests and a future controller
│   ├── model/           # spec -> cloud-agnostic intents (network, access, groups, join)
│   ├── engine/          # task graph: plan/apply, diff rendering, retries, concurrency
│   │   └── enginetest/  # ApplyReplan for provider task tests: apply, plan again, expect no changes
│   ├── cloud/           # Provider / Nodes interfaces, capabilities, registry, common types
│   │   ├── vultr/       # govultr wrapper, label codec, tasks, nodes, inventory, pricing
│   │   │   └── vultrfake/ # in-memory fake of vultr.API for provider and core tests
│   │   └── hetzner/     # hcloud-go wrapper, tasks, nodes, inventory, pricing, lock
│   ├── nodeconfig/      # versioned tent <-> tent-node contract, Nomad config rendering
│   ├── nodeup/          # tent-node phases: system, host firewall, runtime, CNI, nomad, join refresh
│   │   └── env/         # metadata clients: vultr/, hetzner/ (IMDSv2 for AWS later)
│   ├── nomadops/        # the ONLY importer of github.com/hashicorp/nomad/api
│   ├── rollout/         # scale up/down, rolling update, server quorum safety
│   ├── validate/        # cloud + Nomad health checks
│   ├── pki/             # CA, node and operator certificates, gossip key, ACL bootstrap secret
│   ├── uuid/            # random lower-case UUIDs of version 4: operation ids, the ACL bootstrap secret
│   ├── english/         # lists as English sentences write them, "a, b and c", for messages
│   ├── secrettest/      # tests only: looks for a secret in what tent prints or logs
│   ├── statestore/      # Store interface, file:// and s3://, layout, locking
│   ├── assets/          # Nomad / CNI / tent-node sources, signature and checksum verification
│   ├── channels/        # embedded channel files: recommended versions and images
│   ├── buildinfo/       # version, commit, date (ldflags)
│   └── buildconfig/     # tests only: CI workflows, Makefile and release config stay consistent
├── test/e2e/            # //go:build e2e: black-box tests against real clouds (Vultr first)
├── hack/                # janitor, dev upload of tent-node, vultr-spike/
└── docs/                # this document, ADRs, platform notes, roadmap
```

The node planner of M1, which scales node groups without Nomad ([13.4](#134-scaling)), is in `internal/app`. It moves
to `internal/rollout` with the drain and the quorum checks
([ADR-0005](adr/0005-immutable-nodes-and-nomad-aware-rollouts.md)).

`depguard` in golangci-lint enforces the dependency rules ([ADR-0021](adr/0021-import-rules.md)):

- `api/...` imports only the standard library and other `api/` packages, so third parties can use the types. Its
  tests are exempt.
- Only `cmd/tent` imports provider packages (`internal/cloud/<provider>`), to register them. Every other package,
  the core (`internal/model`, `internal/rollout`, `internal/app`) included, uses only the interfaces in
  `internal/cloud`, so no package reaches a provider through another one. Tests are exempt, and so is code under
  `internal/cloud/<provider>/`, so a provider can have subpackages.
- Cloud SDKs (govultr, hcloud-go) are imported only by their provider's packages, tests included.
- `internal/nodeup` never imports `internal/cloud/...`. No code that can create or delete cloud resources ever runs
  on a node.
- Only `internal/nomadops` imports `github.com/hashicorp/nomad/api`. The root module `github.com/hashicorp/nomad` is
  BUSL-licensed and must never be imported.
- `internal/pki` imports only the standard library, `internal/uuid` and `api/v1alpha1`, so the code that makes the
  CA and the secrets never reaches a cloud or the state store. `internal/uuid`, `internal/english` and
  `internal/secrettest` import only the standard library. The tests of these four packages are exempt
  ([ADR-0025](adr/0025-stdlib-only-helper-packages.md)). Only tests import `internal/secrettest`.
- Everything except `api/` is `internal/`. The project makes no compatibility promises before it has to.

---

## 6. Reconciliation engine

See [ADR-0002](adr/0002-direct-cloud-apis-and-own-engine.md). The main types of `internal/engine`:

```go
// Key identifies one desired cloud object. It prints as kind/name, e.g. vultr.FirewallGroup/prod-servers.
type Key struct {
	Kind string // e.g. vultr.FirewallGroup
	Name string // deterministic, e.g. prod-servers
}

// Change is what a task plans to do to its object.
type Change struct {
	Action Action      // Noop | Create | Update | Replace | Delete
	Reason string      // e.g. networking.cidr changed
	Diff   []FieldDiff // {Field, Old, New}, rendered by the task
}

// Object is one cloud object the cluster owns, as the snapshot saw it.
type Object struct {
	Key       Key
	ID        string
	Duplicate bool // the provider keeps another object with this key; this one goes
}

// Snapshot is every object the cluster owns, listed once per run. Tasks read the provider's own
// snapshot type through a type assertion; the engine reads only Objects.
type Snapshot interface{ Objects() []Object }

type Env struct {
	Snapshot Snapshot
	Outputs  *Outputs // values that tasks produce for their dependents, such as IDs and addresses
}

// Outputs is safe for concurrent use.
func (o *Outputs) Set(k Key, name, value string)
func (o *Outputs) Get(k Key, name string) (value string, known bool)

// Task is one desired cloud object. Providers implement tasks; the engine orders, plans and applies them.
type Task interface {
	Key() Key    // no two tasks of a run have the same key
	Deps() []Key // the tasks whose changes go first, also through tasks without changes; no reflection

	// Plan compares the desired object with env.Snapshot. It must not call the cloud.
	Plan(ctx context.Context, env *Env) (Change, error)
	// Apply carries out a planned change. It must be safe to run again.
	Apply(ctx context.Context, env *Env, ch Change) error
	// Delete removes an owned object of the task's kind. It must be safe to run again.
	Delete(ctx context.Context, env *Env, obj Object) error
}

// Kind is one kind of object that tasks manage.
type Kind struct {
	Name    string  // the Key.Kind of its tasks and objects
	Deleter Deleter // usually a task of the kind
}

type Deleter interface {
	Delete(ctx context.Context, env *Env, obj Object) error
}

// Retryable marks err as one the engine may retry: after `after` when it is positive, otherwise after a backoff.
func Retryable(err error, after time.Duration) error

func NewPlan(ctx context.Context, tasks []Task, kinds []Kind, snap Snapshot) (*Plan, error)

// ApplyOptions: Parallelism, ChangeTimeout, OnEvent. A plan applies once: each part runs once.
func (p *Plan) ApplyTaskChanges(ctx context.Context, opts ApplyOptions) error // creates, updates, replaces
func (p *Plan) ApplyDeletes(ctx context.Context, opts ApplyOptions) error     // prune and duplicates, after the above
func (p *Plan) Apply(ctx context.Context, opts ApplyOptions) error            // both parts in one call
```

Behaviour:

- **Tasks and kinds.** The engine takes the tasks and a list of kinds. Each kind has a deleter, usually a task of the
  kind, so creation and deletion stay in one type. The engine deletes an object with the deleter of its kind, so prune
  and `delete cluster` can delete objects of kinds that no task of the run has. The order of the kinds is the deletion
  order, so a task's kind comes before the kinds of the tasks it depends on; the engine rejects kinds in another
  order.
- **Snapshot instead of a lookup per task.** At the start of a run the provider lists every resource kind once:
  labelled lists on Hetzner, tag and client-side filters on Vultr. `Plan` reads only this snapshot. The number of API
  calls grows with the number of resource kinds, not resources. This is critical under Hetzner's 3600 requests per
  hour.
- **Plan.** Tasks plan one at a time in topological order; ties keep the order in which the provider gives them. A
  task plans noop, create, update or replace; deletes come only from prune. When a task plans create or replace, its
  outputs are unknown to its dependents during the plan, and their diffs show `(known after apply)`, as in Terraform.
- **Duplicates.** The provider's inventory keeps one object per key and marks the extra copies as `Duplicate`
  ([ADR-0015](adr/0015-idempotency-without-unique-names.md)). The plan deletes the duplicates with the reason
  `duplicate`. On Vultr the inventory keeps ([ADR-0023](adr/0023-vultr-inventory-dedupe-and-images.md)):
  - of firewall groups, the one that the most instances with the cluster's tag use, then the oldest, then the one
    with the lowest id. The inventory counts them in its own list of those instances, since Vultr gave no
    `instance_count` for a group in use;
  - of the other kinds, the oldest, then the one with the lowest id.

  A creation date that does not parse counts as newer than any that does.
- **Apply.** Changes run in parallel, at most 4 at once by default. A change starts once every change it depends on,
  directly or through tasks without changes, has succeeded. A failed change skips every change that waits for it that
  way. The others go on, and all errors are reported together.
- **Prune.** Prune deletes the objects that carry the cluster marker but that no task claims, and the duplicates.
  Nodes and volumes are not engine kinds. The provider leaves them out of the snapshot and the kinds, so prune never
  deletes them: `rollout` manages nodes, and volumes are never deleted implicitly.
  - Prune runs after every task change has succeeded: kind by kind in the order of the kinds, the objects of one kind
    in parallel.
  - A plan applies in two parts: `Plan.ApplyTaskChanges` carries out the task changes, and `Plan.ApplyDeletes` the
    deletes; `Plan.Apply` does both. `ApplyDeletes` fails before the task changes have been applied. When a task
    change failed or was cancelled, it skips every delete and returns an error that says so.
  - `update cluster` applies the task changes, creates and deletes nodes, and then applies the deletes
    ([13.2](#132-tent-update-cluster---yes)). `delete cluster` calls `Apply` on a plan without tasks, so every
    object of the inventory is a delete ([13.7](#137-tent-delete-cluster---yes)).
  - When a task change fails, the deletes wait for the next run.
  - When a delete fails, the deletes of the later kinds wait for the next run; the other deletes of its kind still
    finish. A VPC delete after a failed firewall-group delete would only fail on `attached` and retry until its
    deadline.
- **Errors and retries.**
  - The provider decides which errors are retryable and marks them with `engine.Retryable(err, after)`:
    - Retryable: rate limits (honouring `Retry-After` / `RateLimit-Reset`), conflicts, `locked`, 5xx and failed
      connections on idempotent calls, and the Vultr VPC that cannot be deleted for about 20 s after its instances
      are gone.
    - Permanent: `invalid_input`, `resource_unavailable` after fallbacks, `forbidden`.
  - The engine waits `after`, or a jittered backoff from 1 s doubling to 30 s, and tries again until the change's
    deadline, 5 minutes by default. Other errors fail at once.
- **Ctrl-C.** Changes that have not started are skipped; running ones see the cancel through their context.
- **Task contract.**
  - `Plan` sets the outputs of an object that exists. `Apply` is not called for a task without changes, so its
    dependents see what `Plan` set.
  - `Apply` sets the outputs again after a create or replace; the engine clears them after planning such a task.
  - In `Apply`, a value that was unknown at plan time is read from `Outputs`, not from the change's `Diff`.
  - After a retryable error the engine calls `Apply` again with the same `Change`. So an operation id
    ([ADR-0015](adr/0015-idempotency-without-unique-names.md)) must stay the same across attempts: the task keeps it
    and does not make a new one per call.
  - The snapshot is read-only: `Apply` calls run concurrently.
  - Tasks ignore objects marked `Duplicate`.
  - `Delete` is safe to run again: an object that is already gone counts as deleted.
- **Idempotency without a state file.** Deterministic names and ownership markers make every task safe to re-run.
  Detecting a create whose response was lost depends on the provider
  ([ADR-0015](adr/0015-idempotency-without-unique-names.md)):
  - *`UniqueNames` (Hetzner).* A retried create returns `uniqueness_error`. The task looks the resource up by name and
    adopts it, but only if the ownership labels match.
  - *No unique names (Vultr).*
    - Every create carries a client-generated operation id (`tent/op`).
    - SDK-level retries of non-idempotent calls are disabled.
    - A create whose answer was lost (`ErrUnavailable`) lists the objects of its kind right away and adopts the one
      that the cluster owns and whose marker carries its operation id; of several, the one the inventory keeps. When
      none is listed yet, the error is retryable, and every later attempt searches by operation id before it creates
      again. The search is a list call, so its errors are retryable as those of any idempotent call.
    - A create that follows a search that found nothing searches once more after it succeeds, and returns the copy
      the inventory keeps; when that search fails or finds nothing, the created one
      ([ADR-0023](adr/0023-vultr-inventory-dedupe-and-images.md)).
    - The search lists no instances, so of several copies of a firewall group it returns the oldest. Right after a
      create no node uses the copies yet, so the inventory keeps the oldest too.
    - Any other error of a create is retryable only when Vultr did not carry the call out, such as a rate limit.
    - A dedupe pass removes accidental copies.
- **Normalisation.** Every task normalises cloud-side values before diffing, so a plan never shows a diff forever,
  which is a known kops bug class. Each task has an "apply, re-plan, expect no-op" test that uses the helper
  `enginetest.ApplyReplan`.
- **Output.**
  - A plan for people:
    - Each change is a line: `+` create, `~` update, `-/+` replace, `-` delete, then the key. Notes follow the key:
      `(ID <id>)` or `(ID <id>, <reason>)` on a delete, `(<reason>)` on another change that has a reason.
    - Its field diffs follow below it: `+ field: new`, `- field: old`, `~ field: old -> new`, and a bare `~ field`
      for a diff with neither value, which lets a task show that a secret changed without showing it.
    - The last line counts the changes: `Plan: N to create, N to update, N to replace, N to delete.`
    - A plan without changes is the line `No changes.`
    - `update cluster` and `delete cluster` put the lines of the changes (`Plan.WriteChanges`) and the counts
      (`Plan.Summary`) into their own plans ([13.2](#132-tent-update-cluster---yes)).
  - `-o json` for machines: `{"changes": [...], "summary": {...}}`.
  - `--exit-code` returns 2 when the plan has changes, for drift detection in CI.

Deliberately **not** copied from kops' `fi` framework:
- task contracts checked by reflection only at runtime;
- dependency discovery by walking struct fields;
- "nil means don't care", which cannot express removing a field;
- deletion code separate from creation code;
- re-applying the whole graph for every replaced node.

---

## 7. Provider abstraction

See [ADR-0004](adr/0004-layered-architecture.md).

### 7.1 Interfaces

`cloud.Provider` in `internal/cloud` has the methods that checking specs, building or deleting a cluster's
infrastructure and managing its machines need. `vultr.Provider` implements it.

```go
// Provider is a cloud that tent provisions clusters on. The core reaches a cloud only through it.
type Provider interface {
	Name() string // as specs give it, such as vultr

	// Validate checks specs that passed v1alpha1.Validate against the cloud's live API: the region exists,
	// the machine types can be deployed there, the images exist.
	Validate(ctx context.Context, c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) error

	// BuildInfra returns the engine tasks that make the network, firewalls and SSH keys match the model.
	// Nodes are not part of it.
	BuildInfra(ctx context.Context, m *model.Cluster) ([]engine.Task, error)
	// InfraKinds returns every kind that BuildInfra's tasks manage, in deletion order, whether or not the model
	// asks for one, so that prune can delete an object the specs no longer ask for.
	InfraKinds() []engine.Kind
	// Inventory lists every object the cluster owns. Tasks read it through the provider's own snapshot type.
	Inventory(ctx context.Context, cluster string) (engine.Snapshot, error)
	// Nodes returns the machine primitives.
	Nodes() Nodes
}

// Nodes are the machine primitives of a provider. Drain, quorum and the order of replacements live in the core.
type Nodes interface {
	List(ctx context.Context, cluster string) ([]Instance, error)
	Create(ctx context.Context, req CreateRequest) (Instance, error) // idempotent per req.Op; waits until ready
	Stop(ctx context.Context, node Instance) error                  // hard where there is no graceful shutdown
	Delete(ctx context.Context, node Instance) error                // even while the machine runs
	ScrubUserData(ctx context.Context, node Instance) error         // replaces the user data with a stub
}

// Instance is one machine of a cluster as the cloud reports it.
type Instance struct {
	ID, Name, Cluster, Group string        // Name is <cluster>-<group>-<index>, also the hostname
	Role                     v1alpha1.Role // the Nomad role of its node group
	Zone, SpecHash, Op       string        // SpecHash is empty when the machine carries none
	PrivateIP, PublicIP      netip.Addr    // the invalid Addr until the cloud reports one
	Ready                    bool          // the cloud reports it running and booted
	Created                  time.Time
}

// CreateRequest is one machine to create.
type CreateRequest struct {
	Cluster, Group string
	Role           v1alpha1.Role
	Zone, Name     string
	MachineType    string   // the plan or server type
	Image          string   // by name, such as ubuntu-24.04
	SpecHash       string   // empty for none
	Op             string   // the operation id, a lower-case UUID of version 4 from NewOpID
	UserData       UserData // may hold secrets
}

// UserData prints only its size, such as [user data, 1234 bytes], through fmt, slog and encoding/json.
type UserData []byte
```

- `CreateRequest.Validate` checks what every provider needs: a cluster, group, zone, name, machine type, image and
  operation id, the role `server`, `client` or `combined`, and an operation id of the form that `cloud.NewOpID`
  makes.
- `cloud.NewOpID` makes an operation id: a random lower-case UUID of version 4, such as
  `5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70`. `cloud.ValidOpID` checks that a text has that form. The Vultr tasks make
  one id per object and keep it across retries ([ADR-0015](adr/0015-idempotency-without-unique-names.md)).
  `update cluster` makes one per node create, and waits for a listed node with the id that the node carries.
- `Create` waits until the machine is ready and has no deadline of its own: the caller gives every call one through
  its context. `update cluster` gives each call 10 minutes ([13.2](#132-tent-update-cluster---yes)).
- A machine that is gone counts as stopped, deleted or scrubbed.
- `vultr.Provider.Nodes()` returns the provider itself. Vultr's `Nodes`: [11.3](#113-creating-a-node) to
  [11.6](#116-user_data).
- **Providers of a command.** `cmd/tent` gives the CLI a function (`cli.WithProviders`) that returns the provider a
  cluster's spec names. For `vultr` it builds the provider with the API key in `VULTR_API_KEY`, which it reads only
  then, and fails when the key is not set. For another provider that the API lists, such as `hetzner`, it fails with
  `cloud.UnsupportedProvider(name)`: `tent cannot manage clusters on hetzner yet`, an error that matches
  `cloud.ErrUnsupportedProvider`. tent has made no cloud objects on such a provider, so `delete cluster` deletes only
  the state ([13.7](#137-tent-delete-cluster---yes)). Any other name, such as an empty one or a typo in a stored
  spec, fails with `unknown cloud provider "<name>"`, and `delete cluster` then deletes nothing.

**Target, not built yet.** The other methods join `Provider` with the code that first uses them:
- `Join` with server discovery in tent-node;
- `PackUserData` with NodeConfig;
- `Locker` with the Hetzner lock firewall;
- `Capabilities` with the first core code that depends on one;
- `Default` with the first provider default that `v1alpha1.SetDefaults` does not fill in.

`Nodes` changes with them: `Stop` is graceful where `GracefulShutdown` is set, the core calls `ScrubUserData` only
where `MutableUserData` is set, and `CreateRequest` gets a fixed private IP with the Hetzner provider.

The sketch of the target:

```go
type Provider interface {
	// Name, Validate, BuildInfra, InfraKinds, Inventory and Nodes as above, and:
	Capabilities() Capabilities
	Default(c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) error // provider defaults, at spec time
	Join(m *model.Cluster) model.JoinStrategy                       // how agents find servers
	PackUserData(nc *nodeconfig.NodeConfig) ([]byte, error)         // encoding and size limits
	Locker(cluster string) statestore.Locker                        // optional cloud-native mutex; may be nil
}

type Capabilities struct {
	ManagedGroups         bool // native autoscaling groups (AWS: yes; Vultr, Hetzner: no)
	InstanceIdentity      bool // signed instance identity documents (AWS: yes; Vultr, Hetzner: no)
	CloudAutoJoin         bool // go-discover support (AWS: yes; Vultr, Hetzner: no)
	FirewallCoversPrivate bool // AWS SGs filter VPC traffic; Vultr and Hetzner firewalls are public-only
	UniqueNames           bool // Hetzner: yes. Vultr: no -> operation ids (ADR-0015)
	KeyValueLabels        bool // Hetzner: labels everywhere. Vultr: string tags on instances only
	FixedPrivateIPs       bool // Hetzner: attach_to_network ip. Vultr: no -> seed + refresh (ADR-0016)
	GracefulShutdown      bool // Hetzner: ACPI shutdown. Vultr: no, halt is hard (ADR-0017)
	FailureDomains        bool // Hetzner eu-central: 3 locations. Vultr: none
	SpreadPlacement       bool // Hetzner: placement groups. Vultr: none
	MutableUserData       bool // Vultr: PATCH user_data -> scrub secrets after bootstrap
	MaxUserDataBytes      int  // Hetzner: 32 KiB. Vultr: 64 KiB budget (the API accepts at least 4 MiB)
	CostEstimates         bool // Hetzner /pricing, Vultr /plans
}
```

On the node side, `tent-node` uses a small per-provider environment interface:
- **Vultr:** the metadata service at `http://169.254.169.254/v1.json`.
- **Hetzner:** `http://169.254.169.254/hetzner/v1/...`.
- **AWS:** IMDSv2 later.

```go
type Environment interface {
	InstanceID(ctx context.Context) (string, error) // Vultr: instance-v2-id (the API UUID)
	Zone(ctx context.Context) (string, error)       // normalised to lower case (Vultr reports "AMS")
	PrivateIPv4(ctx context.Context) (netip.Addr, error)
	UserData(ctx context.Context) ([]byte, error)
	MetadataEndpoint() netip.Addr // blocked for workloads by the host firewall
}
```

### 7.2 Intents (the provider's input)

`model.New` computes `model.Cluster` from the specs, with every default filled in. The provider sees only this type,
never Nomad specifics, except in `Validate`, which takes the specs. It holds the cluster's name, provider, region,
zones and SSH public keys, and:

- **Network.** The private network: the cluster CIDR from `networking.cidr`.
- **Access rules.** The internet-facing rules only, the ones cloud firewalls enforce, in this order:
  - `ssh`: 22/tcp from `access.ssh` to every node;
  - `icmp`: ICMP from anywhere, IPv4 and IPv6 (`0.0.0.0/0` and `::/0`), to every node;
  - `api`: 4646/tcp from `access.api` to the servers, the nodes of the server or combined group.

  Sources are sorted, IPv4 first, without repeats. A rule without sources opens nothing and is left out, so an empty
  `access.ssh` closes SSH.
- **Node groups**, sorted by name: role, machine type, image by name, zones and size.

**Target, not built yet.**
- **Intra-cluster rules** (4646–4648 and the dynamic port range between nodes) come with tent-node. Vultr and Hetzner
  firewalls cannot filter private traffic, so their providers map only the internet-facing rules, and tent-node's
  host firewall enforces the intra-cluster ones. AWS will map all of them to Security Groups.
- **Join strategy**, with tent-node: fixed slots on Hetzner, seed plus refresh on Vultr, and cloud auto-join tags on
  AWS.
- **Named subnets** where the provider supports fixed IPs: on Hetzner `control` for fixed addresses and `nodes` for
  automatic ones.
- **Per group:** the NodeConfig template and the spec hash.
- **Load balancers** (optional): the API load balancer, and later an internal one and ingress.

### 7.3 Provider comparison (what the core must not assume)

| Area | Vultr (first) | Hetzner (second) | AWS (later) | Mechanism that keeps the core unchanged |
|---|---|---|---|---|
| Region/zones | region, no AZs | network zone / locations | region / AZs | `cloud.region`, `cloud.zones` semantics |
| Ownership | string tags (instances), text fields elsewhere | key/value labels | tags | per-provider label codec |
| Idempotent create | operation ids | unique names | client tokens / tags | `UniqueNames` ([ADR-0015](adr/0015-idempotency-without-unique-names.md)) |
| Perimeter | one public-only firewall group per instance | public-only Cloud Firewalls | Security Groups | access intents + host firewall |
| Node groups | tent creates each VM | tent creates each VM | EC2 directly, ASG later | `Nodes` primitives; `ManagedGroups` |
| Server discovery | seed + refresh | fixed IP slots | cloud auto-join | `JoinStrategy` ([ADR-0016](adr/0016-server-discovery-seed-and-refresh.md)) |
| Server removal | hard stop + Nomad API | ACPI shutdown + Nomad API | terminate + Nomad API | `GracefulShutdown` ([ADR-0017](adr/0017-api-driven-server-removal.md)) |
| Node credentials | user data, scrubbed after bootstrap | user data | IAM role / bootstrap controller | credential delivery strategy ([9.5](#95-target-architecture-bootstrap-controller)) |
| State store | Vultr Object Storage / any S3 | Hetzner Object Storage / any S3 | S3 | the same `s3://` backend |
| Metadata | `/v1.json` | `/hetzner/v1/` | IMDSv2 | `nodeup/env.Environment` |

---

## 8. Nodes: tent-node and NodeConfig

See [ADR-0006](adr/0006-two-binaries-and-nodeconfig.md).

### 8.1 Bootstrap chain

```
cloud-init (user_data: minimal cloud-config; vendor package upgrades disabled)
  ├─ write /etc/tent/node.json            # NodeConfig, gzip+base64 in user data, mode 0600
  ├─ download tent-node (mirrors) + verify sha256
  └─ tent-node install                    # installs tent-node.service (oneshot, every boot)
        │                                 # and tent-node-join.timer (join refresh, every 60 s)
        └─ tent-node up                   # idempotent phases, see below
              └─ systemctl start nomad
```

- **No operator, no SSH.** The node bootstraps itself, without the operator being online and without SSH access. This
  is a precondition for future autoscaling and automatic replacement. SSH is used only for diagnostics
  (`tent toolbox dump`).
- **No vendor package upgrades.** The cloud-config disables package update and upgrade. Vultr's vendor data has set the
  same since at least 2026-09, but it may change, so tent sets it anyway. OS patching happens by replacing nodes.

### 8.2 tent-node phases

`tent-node up` runs as a systemd oneshot on every boot. Every phase checks the current state before it acts, and
Nomad is restarted only when its files actually changed.

| Phase | What it does |
|---|---|
| `preflight` | Checks the OS and architecture. The instance id and hostname from the metadata service must match NodeConfig, which protects against mixed-up user data. |
| `system` | Hostname, sysctls, kernel modules (`br_netfilter`, `overlay`), time sync, journald limits. |
| `hostfirewall` | **Owns the host firewall.** Disables ufw or firewalld if the image enabled them (Vultr images do). Applies tent's nftables ruleset: SSH, Nomad ports 4646–4648 and the dynamic port range only from the cluster CIDR, and a drop rule for `169.254.169.254` from forwarded (container) traffic and non-root processes. |
| `runtime` | Docker from the distribution package by default, plus `daemon.json` (log limits, live-restore). |
| `cni` | CNI reference plugins into `/opt/cni/bin`, verified by sha256. |
| `join` | Writes `05-join.hcl`: the seed or slot list from NodeConfig, refreshed from the live peer set when a server is reachable ([ADR-0016](adr/0016-server-discovery-seed-and-refresh.md)). |
| `nomad` | Downloads the Nomad zip (verified by sha256); creates the user and directories; writes the TLS files (mode 0600), the agent configuration files, the intro token into `<client state_dir>/intro_token.jwt` and the systemd unit; starts Nomad. |
| `verify` | Checks the local `/v1/agent/health`. Writes `/var/lib/tent/status.json` for `tent toolbox dump`. |

`tent-node refresh-join` runs from a systemd timer every 60 seconds:
1. It calls `GET https://<known server>:4646/v1/status/peers`, authenticating with the node's own certificate. The
   endpoint needs no ACL token.
2. It rewrites `05-join.hcl` atomically when the peer set changes.
3. Nomad reads that file only at start, so the refresh never restarts Nomad. It only guarantees that the next start
   finds the current servers.

### 8.3 NodeConfig contract

```go
// NodeConfig is the versioned contract between tent (producer) and tent-node (consumer).
type NodeConfig struct {
	APIVersion string  // "tent/v1alpha1"
	Kind       string  // "NodeConfig"
	Cluster    string
	NodeGroup  string
	Instance   string  // expected hostname / cloud machine name
	Role       Role    // server | client | combined
	Assets     []Asset // {Name, URLs (mirrors), SHA256}: nomad, cni-plugins
	Files      []File  // {Path, Mode, Owner, Content, PerNode}: Nomad config, TLS material, intro token
	Join       Join    // {Strategy: slots|seed-refresh, Addresses []netip.Addr, RefreshInterval}
	System     System  // sysctls, kernel modules, container runtime settings
	Firewall   HostFirewall
	SpecHash   string  // semantic hash of the group-level configuration (see 8.4)
}
```

- **Size.** The encoded NodeConfig must stay well below the provider's user data limit.
  - Hetzner: 32 KiB, so tests fail above 24 KiB.
  - Vultr: the API accepts at least 4 MiB. tent's budget is 64 KiB, verified end to end through the metadata service
    and cloud-init (spike 2026-09-25).
  - Fallback if a provider's limit is too small: user data carries only a short-lived presigned URL and a key for an
    encrypted NodeConfig object in the state bucket.
- **Versioning.** The contract is versioned, and tent-node always has the CLI's version. Version skew therefore
  appears only when a newer CLI operates a cluster whose nodes an older CLI created. Those nodes keep running their
  tent-node until they are replaced.

### 8.4 Nomad configuration rendering

- **The CLI renders the final Nomad agent configuration**, and tent-node only writes files. So:
  - `tent update` can show a diff of the Nomad configuration per group;
  - the hash of the rendered configuration is exactly the "node is outdated" signal;
  - tent-node stays thin.
- **Files in `/etc/nomad.d/`**, merged by Nomad in lexicographic order:

  | File | Contents | Scope |
  |---|---|---|
  | `00-tent.hcl` | group configuration | identical on all nodes of a group |
  | `05-join.hcl` | `server_join { retry_join = [...] }` | per node; maintained by tent-node, never part of the hash |
  | `10-node.hcl` | `name`, `datacenter`, `tent_instance_id` meta | per node |
  | `99-user.hcl` | `extraConfig` | group |
- **Values known only at runtime** (private IP, interface name) are go-sockaddr templates that select the interface by
  cluster CIDR. Nomad supports them in `bind_addr`, `addresses`, `advertise` and `client.network_interface`.
  Interface names vary on both Vultr and Hetzner, so this matters on both.
- **Spec hash:** the first 16 hex characters of sha256 over the canonical group-level content.
  - Included: `00-tent.hcl`, `99-user.hcl`, asset versions and sha256s, system settings, the host firewall and the
    tent-node version.
  - Excluded: per-node files (certificates, keys, intro token, `05-join.hcl`, `10-node.hcl`) and mirror URLs.
  - So a different download mirror never rolls the cluster (a kops pitfall). A new tent version that changes rendering
    does roll nodes, and the plan says why.
- **Format.** The agent configuration is parsed by HCL1. Rendering uses text templates with strict escaping and
  golden-file tests.

Full sketches: [Appendix A](#appendix-a-nomad-agent-configuration-sketches).

### 8.5 Artifacts and verification

| Artifact | Source | Verification |
|---|---|---|
| Nomad | `https://releases.hashicorp.com/nomad/<v>/nomad_<v>_linux_<arch>.zip` | The **CLI** downloads `nomad_<v>_SHA256SUMS` and verifies its detached signature with HashiCorp's release key, which is embedded in tent. The node verifies only the sha256 carried in NodeConfig. |
| CNI plugins | GitHub releases of `containernetworking/plugins` (version pinned in the channel) | sha256 in NodeConfig |
| tent-node | GitHub release of tent (`tent-node_linux_<arch>` + `checksums.txt`) | The CLI reads `checksums.txt` of its own version and puts the sha256 into user data. |
| Docker | the distribution's package repository | distribution package signatures |

- Trust is established once, on the operator's side, so nodes need no PGP.
- The HashiCorp APT repository is deliberately **not** used: its signing key was rotated on 2026-09-09 after a
  security incident.
- Development builds of tent-node are uploaded to object storage and served through a presigned URL (`TENT_NODE_URL`
  plus `TENT_NODE_SHA256`).

### 8.6 Operating systems

- **Ubuntu** (apt-based) is supported in v1. The default image is `ubuntu-24.04` (decided on 2026-09-25). E2E also
  runs on `ubuntu-26.04`.
- **Architectures:** x86-64 everywhere, and arm64 only where the provider offers it. Hetzner has CAX; Vultr has no
  arm64 Cloud Compute.
- **Other distributions** come later behind a small `osfamily` abstraction in `nodeup`.

---

## 9. Security

See [ADR-0007](adr/0007-security-baseline.md) and [ADR-0008](adr/0008-node-credential-delivery.md).

### 9.1 PKI

`internal/pki` makes the CA, the certificates, the gossip key and the ACL bootstrap secret (M2.1). `update` keeps the
CA and the secrets in the state store ([13.2](#132-tent-update-cluster---yes)); no node uses them yet. The storage and
certificate details are in [ADR-0024](adr/0024-cluster-pki-storage-and-certificates.md).

- **One CA per cluster**, ECDSA P-256, the same as `nomad tls`. The CA private key lives only in the state store and
  never reaches a node.
- **The CA certificate** is self-signed, with `CN=tent <cluster> CA, O=tent`, path length 0, and the key usages
  `CertSign` and `CRLSign`. It is valid for 10 years, until CA rotation exists (maintainer decision of 2026-09-28,
  [18](#18-open-questions)); `nomad tls` makes 5-year CAs. It starts 5 minutes before it is made, as the leaf
  certificates do, because a machine whose clock is behind checks the CA's validity too.
- **The CA is stored as a bundle from day one**, so CA rotation can be added later without migrating the storage
  format. `pki/ca-bundle.pem` holds one or more CA certificates, and `pki/private/ca.key` the key of one of them, the
  active signer. The active signer is the first certificate of the bundle whose public key matches the key. Its id is
  the certificate's Subject Key Identifier in hex. No file stores the id, since the key identifies the signer.
- **Keys** are PKCS#8 PEM (`PRIVATE KEY`), which Nomad reads, and tent loads no other form. A bundle or a key with
  anything but white space after its PEM is refused.
- **Leaf certificates**, of nodes and operators, each get a new ECDSA P-256 key, a random serial number of up to 128
  bits, the key usage `DigitalSignature`, and their first DNS name as the common name. Each is valid from 5 minutes
  before it is made, for clock skew, and never ends after the active signer.
- **Per-node certificates:**
  - servers get `server.<region>.nomad` and clients get `client.<region>.nomad`, combined nodes both
    ([ADR-0019](adr/0019-combined-server-client-role.md)), all plus `localhost` and `127.0.0.1`. The region is the
    Nomad region, `spec.nomad.region` (default `global`), not the cloud region;
  - extended key usage is `serverAuth` **and** `clientAuth`, because tent-node's join refresh calls the servers' HTTP
    API with the node certificate;
  - validity is 1 year, and renewal means replacement; `tent validate` warns 30 days before expiry.
- **No node IP addresses in certificates**, only `127.0.0.1`. The CLI connects to a server's public IP with
  `TLSServerName = server.<region>.nomad`, so server IPs can change freely.
- **Operator certificates** use `cli.<region>.nomad`, with `clientAuth` only. They are short-lived and issued on
  demand by `tent export nomad`, whose TTL defaults to 24 hours. `internal/pki` takes any TTL above zero.
- **mTLS** is on for RPC and HTTP: `verify_server_hostname = true`, and `verify_https_client = true` by default.
- **The gossip encryption key** is used on servers only. It is 32 random bytes in standard base64, as
  `nomad operator gossip keyring generate` makes.
- **The ACL bootstrap secret** is a random lower-case UUID of version 4 ([9.2](#92-acl-and-tokens)).
- **Stored values are checked.** Each plan of `update` loads the stored CA. It checks that the stored gossip key is
  standard base64 of 32 bytes, without line breaks, and that the stored ACL bootstrap secret is a UUID of that form.
  tent never replaces them ([13.2](#132-tent-update-cluster---yes)).
- **Keys and secrets never print.** fmt, slog and JSON show only their size, such as `[secret, 44 bytes]`.

### 9.2 ACL and tokens

- **ACLs are always enabled.**
- **Bootstrap.**
  - tent generates the bootstrap secret, a UUID, and stores it in the state store **before** calling
    `POST /v1/acl/bootstrap {"BootstrapSecret": ...}`.
  - Bootstrap is not idempotent: a second call fails with "ACL bootstrap already done". On retry, tent verifies the
    stored secret with `GET /v1/acl/token/self`.
- **The bootstrap token is used only by tent itself.** Scoped tokens with TTLs for tent's own operations come later.
- **For humans**, `tent export nomad` issues a separate ACL token with a TTL.

### 9.3 Client introduction

Servers run with `client_introduction { enforcement = "strict" }` (Nomad 1.11+).

- **Issuing.** Right before each client VM is created, tent requests an introduction token with
  `POST /v1/acl/identity/client-introduction-token`. The token is bound to the node name and node pool, with a TTL
  of at most 30 minutes.
- **Delivery.** tent-node writes the token to `<client state_dir>/intro_token.jwt`, because the agent configuration
  file cannot carry it.
- **Lifetime.** Nomad uses the token only for the first registration. After that the node holds a self-renewing node
  identity.
- **Gotchas.**
  - An expired or mismatched token is rejected even with `enforcement = "warn"`.
  - A VM that fails to register within the TTL is simply replaced, which is idempotent.
  - Client introduction does not replace mTLS.

### 9.4 Secrets on nodes: threat model

Neither Vultr nor Hetzner offers instance identity. `user_data` is readable through the metadata service from inside
the VM, and by default that includes containers.

| In user data | Risk if read | Mitigation |
|---|---|---|
| CA certificate | none (public) | — |
| Node certificate and key | impersonate that node | nftables blocks the metadata endpoint for containers and non-root processes; servers run no workloads (except combined nodes, [ADR-0019](adr/0019-combined-server-client-role.md)); **Vultr: user data is scrubbed after bootstrap** |
| Gossip key (servers only) | join the server gossip pool | servers run no workloads (except combined nodes); Serf and RPC listen only on the private network |
| Intro token (clients only) | register a fake client | TTL ≤ 30 min, bound to one node name and pool, used once at first registration |
| Cloud API token | — | **never on nodes** |
| State store credentials | — | **never on nodes** (unlike kops on Hetzner) |

Scrubbing on Vultr works like this:
- Vultr lets user data be changed after creation.
- Once a node has registered with Nomad, tent replaces its user data with a non-secret stub (`Nodes.ScrubUserData`).
- From then on the metadata service no longer serves the secrets.

The spike confirmed on 2026-09-25 that this works: the metadata service serves the updated value within seconds, and
cloud-init does not re-run after a restart ([ADR-0018](adr/0018-vultr-provider-design.md)). Hetzner user data is
immutable, so there it stays for the node's lifetime.

### 9.5 Target architecture: bootstrap controller

This is v2, and it is mandatory for ASG-style groups where every instance shares the same user data.

A bootstrap controller runs on the servers, modelled on kops-controller:
1. The node generates its key locally and sends a CSR with its claimed instance id.
2. The controller looks the instance up in the cloud API.
   - Vultr and Hetzner: it checks the ownership markers, the private IP, the creation time and that the node is not
     already registered.
   - AWS: it checks the signed instance identity document.
3. On providers without identity documents, the controller also calls back to the private IP that the API reported
   and runs a one-time challenge.
4. It issues the certificate and the intro token.

User data then carries no secrets at all. Credential delivery is therefore a strategy chosen by capabilities:
`userdata` (v1), `controller` (v2), and possibly `iam-s3` on AWS.

### 9.6 Network perimeter

- **Cloud firewalls cover the public interface only**, on both providers.
  - Vultr: a firewall group for the servers and one for the clients, because an instance can have only one. The
    clients' group exists only when the cluster has a client group
    ([ADR-0023](adr/0023-vultr-inventory-dedupe-and-images.md)).
  - Hetzner: `<cluster>-nodes` and `<cluster>-servers`.
- **What they allow:**
  - 22/tcp from `access.ssh`;
  - ICMP from anywhere, IPv4 and IPv6;
  - 4646/tcp to servers from `access.api`. Left out, `access.api` is `[0.0.0.0/0]`, because mTLS and ACL protect the
    API. An explicit empty list is a validation error, because the tent CLI reaches the servers through this port.
    `validate` and every mutating command warn loudly while it is open to the whole internet.

  Everything else is dropped.
- **Hetzner** firewalls are applied both by label selector and explicitly at server creation. **Vultr** sets the
  group at instance creation. Either way the machine is protected from its first packet.
- **Binding.** RPC and Serf bind only to the private address. HTTP on clients binds to localhost plus the private
  address. HTTP on servers binds to all addresses, behind the cloud firewall.
- **Private traffic is not filtered by either cloud.** tent-node's nftables ruleset allows Nomad ports only from the
  cluster CIDR.
- **Load balancers:** Vultr load balancers are always public, and Hetzner firewalls do not apply to load balancers. An
  optional API load balancer is therefore safe only because of mTLS plus ACL. Vultr load balancer firewall rules can
  narrow the sources.

### 9.7 Operator access

- **`tent export nomad`** writes `NOMAD_ADDR`, `NOMAD_CACERT`, `NOMAD_CLIENT_CERT`, `NOMAD_CLIENT_KEY`,
  `NOMAD_TLS_SERVER_NAME` and `NOMAD_TOKEN` into files and prints the matching `export` lines. The certificate and the
  token are short-lived.
- **`tent ui`** is a local reverse proxy from `127.0.0.1:4646` to the cluster that injects mTLS and the token. The
  browser UI works without installing client certificates, and `verify_https_client = true` stays on.

### 9.8 Credentials handling

- **Cloud credentials** come from `VULTR_API_KEY` and `HCLOUD_TOKEN`, later also from token files. They never go into
  specs, the state store, logs or nodes.
- **Vultr.** Use a service user whose IAM policy covers only the actions tent needs: compute instances, VPCs,
  firewalls, SSH keys, and optionally load balancers and object storage. Add an IP allow-list for CI where possible.
  Vultr IAM also supports OIDC role trusts, a route to short-lived CI credentials; this is to be evaluated.
- **Hetzner.** Tokens are project-wide (Read, or Read & Write) and the rate limit is per project, so use one project
  per cluster or environment.
- **State store credentials** come from the standard AWS credential chain.

---

## 10. State store and locking

See [ADR-0010](adr/0010-state-store-and-locking.md).

### 10.1 Backends

`statestore.Open(ctx, url)` opens a store. It wraps every backend in a check that rejects invalid paths before the
backend sees them, and it names the operation and the path in every error.

- **`file:///abs/path`** (`file:///C:/state` on Windows): local development and single-runner CI jobs (E2E uses it).
  - Local disks only. Network file systems such as NFS emulate file locks per process, so two goroutines of one
    process would not exclude each other and conditional puts would not be atomic.
  - Every operation holds a file lock on `.tent-store.lock` in the root: shared to read, exclusive to write or delete.
  - A write goes to a temp file in the object's directory, which is synced and renamed over the object; then the
    directory is synced. Files are `0600` and directories `0700`, because the store holds secrets.
  - It differs from object stores in two ways. On a case-insensitive file system, the default on macOS and Windows,
    `a` and `A` are the same object. And a path cannot be both an object and the prefix of other objects, so `a` and
    `a/b` cannot both exist.
- **`s3://bucket[/prefix]?endpoint=…&region=…&pathStyle=true`:** uses aws-sdk-go-v2 and works with any S3-compatible
  store.
  - `endpoint` is `https://host` or `http://host`, and every store except AWS S3 needs it.
  - `region` is needed unless the AWS configuration names one. Cloudflare R2 takes `auto`.
  - `pathStyle=true` is for servers without bucket host names.
  - A conditional put gets none of the SDK's retries, because a retry after a lost answer would find the first
    write and report a lost race to the process that won it. So a conditional put that gets a 500 or no answer is
    never sent again.
  - A throttled request (429, `TooManyRequests` or `SlowDown`) is sent again after a random wait of 1–2 seconds, up
    to 30 attempts in all. This includes conditional puts, because the server did not carry out a throttled request.
    The SDK does not retry R2's 429 `TooManyRequests`, but it retries a 503 `SlowDown` on a plain request (3
    attempts, backoff up to 20 s) inside each of these attempts.
    Cloudflare R2 takes one write per second to a key and answers faster writes with 429 `TooManyRequests`.
  - **Vultr Object Storage** (`<cluster-id>.vultrobjects.com`):
    - EU endpoints: `ams1`, `ams2`, `lhr1`, `mxp1`.
    - At least $18/month (Standard tier); the Archive tier cannot hold state.
    - Access keys can be created through the Vultr API.
  - **Hetzner Object Storage** (`<fsn1|nbg1|hel1>.your-objectstorage.com`): EU only, keys only from the Console,
    €6.49/month base price. tent does not use its conditional writes ([10.4](#104-locking)).
  - **AWS S3, Cloudflare R2, MinIO.**
- **The state store does not depend on the compute provider.** A Vultr cluster may keep its state in R2, for example.
- **Credentials** come from the standard AWS credential chain: the environment, the shared files, or the machine's
  role. They never go in the URL: `Open` rejects a URL that contains `@`, and its errors never repeat the URL.
- **Bucket versioning** is recommended.
- **Tests.** Every backend runs the conformance suites in `internal/statestore/storetest`. CI runs the s3 backend's
  suites against a Cloudflare R2 bucket ([platform notes §5.1](platform-notes.md#51-conditional-writes-)).

```go
type Store interface {
	Get(ctx context.Context, path string) ([]byte, Version, error) // ErrNotFound
	Put(ctx context.Context, path string, data []byte, opts PutOptions) (Version, error)
	List(ctx context.Context, prefix string) ([]string, error) // full paths, sorted
	Delete(ctx context.Context, path string) error              // deleting a missing object succeeds
	Capabilities(ctx context.Context) (Capabilities, error)     // may ask the backend once, then keeps the answer
	String() string                                             // the URL without credentials
}

type PutOptions struct { // at most one of the two
	IfNoneMatch bool    // create only
	IfMatch     Version // replace only this version
}

type Capabilities struct {
	ConditionalPut bool // PutOptions are enforced atomically, also between processes
}
```

- **Conditional puts.** When the condition does not hold, `Put` writes nothing and returns `ErrPreconditionFailed`. A
  store without `ConditionalPut` writes nothing and returns an error that wraps `errors.ErrUnsupported`: it never
  falls back to reading first and writing in a separate step.
- **Paths** are segments separated by single slashes, such as `prod/cluster.yaml`. A segment matches
  `[A-Za-z0-9_][A-Za-z0-9._-]*`, does not end with `.`, and is not a Windows device name (`con`, `prn`, `aux`, `nul`,
  `com1`–`com9`, `lpt1`–`lpt9`, in any case and with any extension). So every path works in every backend on every
  operating system. Names that start with `.` are left to the backends ([10.2](#102-layout)).

### 10.2 Layout

```
<state>/<cluster>/
  tent-version                           # minimum tent version; older CLIs refuse to touch the cluster
  cluster.yaml                           # user spec
  nodegroups/<name>.yaml                 # user specs
  cluster.completed.yaml                 # last applied, with all defaults
  lock                                   # the lock's lease, with a conditional-put or best-effort lock (10.4)
  pki/ca-bundle.pem                      # public
  pki/private/ca.key                     # secret
  secrets/gossip.key                     # secret
  secrets/acl-bootstrap-token            # secret
  backups/<timestamp>.snap               # Raft snapshots (contain the keyring: secret)
  history/<timestamp>-<operation>.yaml   # audit trail of applies
```

`statestore.Layout` names these objects, and `statestore.Clusters` lists the clusters in a store: the top-level names
that hold a `cluster.yaml`.

Names that start with `.` belong to the backends, and `List` never returns them: `.tent-store.lock` and
`.tent-locks/` in the root of a `file://` store, the temp files of its writes (`.tent-tmp-*`), and the objects that
the s3 backend's probe writes below `.tent-probe/` ([10.4](#104-locking)).

**Version guard.** `CheckVersion` fails when `tent-version` names a newer tent than the running one, for example
`cluster prod needs tent v0.4.0 or newer; this is v0.3.1`. `RaiseVersion` records the running version when it is
newer, under the cluster's lock. A release, a pre-release and `git describe` output are checked, and
`v0.3.0-4-gabc1234` counts as `v0.3.0`. Development builds, such as `dev` and GoReleaser `-SNAPSHOT` builds, skip the
guard and never raise the version.

`tent delete cluster` removes the state last. It refuses to remove files it does not recognise unless `--force` is
given.

### 10.3 Secrets at rest

- v1 relies on the bucket being private and on provider-side encryption.
- Later, an optional `SecretStore` layer over `Store` adds client-side encryption with `age`.

### 10.4 Locking

Mutating commands (`create`, `replace`, `edit`, `update`, `rolling-update`, `upgrade`, `delete`, `backup restore`)
take a cluster lock. `statestore.NewLocker` picks the first mechanism that fits the store:

| Order | Mechanism | Where |
|---|---|---|
| 1 | `flock` on `.tent-locks/<cluster>.lock` in the store's root | `file://` |
| 2 | Conditional put: the lease object `<cluster>/lock` is created with `If-None-Match: *` and replaced with `If-Match` | S3 stores whose probe shows that they enforce conditional puts. **AWS S3** and **Cloudflare R2** document both headers; CI runs the lock tests against R2. **Ceph RGW on RADOS**, which Vultr runs, honours them in a local test (20.2.4, 2026-09-26). **Vultr Object Storage** itself is unverified: the spike's check needs a bucket and has not run. Servers that ignore the headers fail the probe and get row 3 or 4 ([platform notes §5.1](platform-notes.md#51-conditional-writes-)). **Not Hetzner Object Storage**: conditional writes are unsupported on versioned buckets and undocumented otherwise, so the s3 backend treats every endpoint on `your-objectstorage.com` or its subdomains as a store without them and does not probe it, until E2E proves them ([ADR-0010](adr/0010-state-store-and-locking.md)). |
| 3 | Cloud-native mutex from the provider, available only on providers with `UniqueNames` (M4) | **Hetzner**: an empty firewall named `<cluster>-lock` labelled `tent/lock-for=<cluster>`. Creating it acquires the lock, deleting it releases it. It deliberately has no `tent/cluster` label, so inventory and prune ignore it. A Hetzner cluster with Hetzner Object Storage locks this way. |
| 4 | Best-effort lease (write, wait, read back) with a loud warning | anything else, for example Vultr compute with Hetzner Object Storage |

- **flock wins for `file://`.** A `file://` store enforces conditional puts too, but the OS releases a file lock when
  its process ends, so a crashed tent never leaves a lock to wait out. The lock file is never deleted, because a
  waiter could then lock the old file while a newcomer locks a new one.
- **The probe.** The s3 backend creates an object below `.tent-probe/` twice with `If-None-Match: *`, then deletes
  it. Only a server that refuses the second create counts as enforcing conditional puts, and the store keeps the
  answer. On R2 it costs about 2–4 s the first time a process locks, because it writes one key three times (two
  puts and a delete) at one write per second. Hetzner endpoints are never probed. The probe cannot see whether a
  server stays atomic under concurrent writers: S3Proxy and S3Mock pass it and are not. Before trusting a
  self-hosted server, set `TENT_TEST_S3_URL` to a bucket on it and run
  `go test -race -count=1 -run S3 ./internal/statestore/`, which includes the race tests.
- **The lease** records a random holder ID, the owner (the OS user), host, pid, operation, acquired-at and
  expires-at. Rows 2 and 4 keep it in `<cluster>/lock`. Under flock it sits next to the lock file, in
  `.tent-locks/<cluster>.lease`, because Windows keeps other processes from reading a locked file.
- **Waiting.** `Acquire` tries every 2 seconds until it holds the lock or its context ends. In the second case the
  error names the holder, for example
  `cluster prod is locked by igor@laptop (pid 4242) for update since 2026-09-26 10:00:00 UTC`. While another tent
  holds the lock, a waiting tent writes the lock key at most once per try. When many waiters exceed R2's one write
  per second per key, the throttled writes are sent again ([10.1](#101-backends)).
- **Renewal.** A lease lasts 2 minutes (the TTL) and is renewed every TTL/3; a renewal that takes longer than TTL/3
  fails. `Lost()` closes when the lock is gone, or when renewals have failed until the lease would expire before the
  next one. The operation must then stop.
- **Takeover.** A lease past its expiry, by the clock of the one who wants the lock, is taken over. Under flock,
  expiry does not matter: the OS keeps the lock while its holder runs, and a lease under a free lock was left by a
  holder that died. Either way `Previous()` returns the old lease so that the CLI can warn. Clocks of different hosts
  must differ by less than the TTL.
- **Release** stops the renewals and unlocks with its own 5-second timeout, so it also works after Ctrl-C has
  cancelled the operation.
- **`tent state unlock --force`** (`ForceUnlock`) removes the lease whoever holds it and returns it. With a lease in
  the store, the holder's next renewal fails and its `Lost()` closes. Under flock it refuses while the holder's
  process runs ("stop that process first"), because only that process can release its file lock; it removes a lease
  that a dead holder left.
- **Best effort** can fail. Two holders may both get the lock when one of them takes more than 5 seconds (the wait)
  between reading the lock and writing its lease. And a renewal, which reads and then writes, may undo a `ForceUnlock`
  that comes between the two.

---

## 11. Vultr provider

The first provider. See [ADR-0018](adr/0018-vultr-provider-design.md) for the decisions and
[platform notes §3](platform-notes.md#3-vultr) for the facts. Items marked 🔬 are verified with
[`hack/vultr-spike`](../hack/vultr-spike/README.md) before the provider code relies on them.

### 11.1 Resources

`vultr.New(api)` returns the provider over a `vultr.API` ([11.8](#118-api-client-rate-limits-cost)); `WithLogger`
sets where its warnings go.

| Resource | Engine key | Marker | Managed by | Notes |
|---|---|---|---|---|
| SSH key | `vultr.SSHKey/<cluster>-<fp>` ([3.4](#34-naming-and-ownership-markers)) | name `tent:cluster=<c>;kind=ssh-key;fp=<fp>;op=<op>` | engine | one per key of `sshKeys`; keys with the same type and data are one, whatever their comments. Two different keys with the same `fp` fail `BuildInfra`. A Vultr key with the marker but other key material fails the plan: delete it in Vultr, and tent creates the spec's key |
| VPC | `vultr.VPC/<cluster>` | description `tent:cluster=<c>;kind=vpc;op=<op>` | engine | one per cluster, in `cloud.region`, with the CIDR of `networking.cidr` (`/16`, `/20` and `/24` verified); at most 5 VPCs per region. The region and the CIDR never change ([ADR-0023](adr/0023-vultr-inventory-dedupe-and-images.md)): moving the network would need every node replaced first, so the plan fails, for example with `the VPC of cluster prod is 10.64.0.0/16 in ams; the spec asks for 10.65.0.0/16 in ams, and tent cannot move a cluster's network` |
| Firewall groups | `vultr.FirewallGroup/<cluster>-servers`, `vultr.FirewallGroup/<cluster>-clients` | description `tent:cluster=<c>;kind=firewall;role=server;op=<op>`, or `role=client` | engine | the servers' group for the server or combined group; the clients' group only when the cluster has a client group. An instance has exactly one group. Rules: [11.5](#115-firewall-and-host-firewall) |
| Instances | none | label = hostname = `<cluster>-<group>-<index>`; tags = canonical labels via the codec | rollout via `Nodes` | see [11.3](#113-creating-a-node) |
| Load balancer (optional) | later | label `tent:cluster=<c>;kind=lb;name=api` | engine + rollout | always public; targets are instance IDs, so rollout updates membership on replacement; LB firewall rules narrow the sources |
| NAT gateway (private topology, later) | later | tag | engine | one per VPC, $0.03/hour |

- **Markers.** `op` is the operation id of the create, a lower-case UUID
  ([ADR-0015](adr/0015-idempotency-without-unique-names.md), [6](#6-reconciliation-engine)). Each task makes one when
  `BuildInfra` builds it.
- **Text lengths and SSH keys** (spike 2026-09-27,
  [platform notes §3.10](platform-notes.md#310-ownership-fields-on-other-resources)). Vultr stores 255 characters of
  a VPC or firewall group description and 128 of an SSH key name verbatim; tent's markers reach 99. The spike set the
  texts with updates: `PUT` for a VPC or a firewall group, `PATCH` for an SSH key. Vultr accepts a second SSH key with
  the same key material, so two clusters can use the same operator key.
- **Deletion order.** `InfraKinds` returns `vultr.FirewallGroup`, `vultr.VPC`, `vultr.SSHKey`: the engine deletes
  firewall groups first and SSH keys last.
- **Inventory.** `Inventory` makes one list call each for SSH keys, VPCs and firewall groups, over the whole account,
  one for the instances with the tag `tent/cluster=<cluster>`, then one for the rules of each firewall group it keeps
  ([ADR-0023](adr/0023-vultr-inventory-dedupe-and-images.md)).
  - Of several copies of a firewall group, it keeps the one that the most of those instances use. It counts every
    instance that the tag filter lists, as the delete guard does ([11.5](#115-firewall-and-host-firewall)), and does
    not read Vultr's `instance_count`.
  - An object is the cluster's when the marker in its name (an SSH key) or its description (a VPC or a firewall
    group) names the cluster, has the kind of the list it came from, and has the `fp` or `role` its kind needs.
    Which copy of a key stays: [6](#6-reconciliation-engine). The search after a lost create uses the same rule,
    but counts no instances: of several firewall groups it returns the oldest, which the inventory keeps too while
    no node uses the copies.
  - It skips these objects with a warning, and neither adopts nor deletes them:
    - a text that starts with `tent:` and does not parse, whatever cluster it names;
    - a marker of the cluster whose kind does not match the list it came from, such as `kind=vpc` on an SSH key;
    - an SSH key marker of the cluster without an `fp` of 8 lower-case hex digits;
    - a firewall group marker of the cluster without the role `server` or `client`.
- **Plan.** The tasks have no dependencies on each other. The plan of the cluster of [3.1](#31-kinds) on an empty
  account (`internal/cloud/vultr/testdata/infra.plan.golden`):

  ```
  + vultr.SSHKey/prod-8ba890ed
  + vultr.VPC/prod
      + cidr: 10.64.0.0/16
      + region: ams
  + vultr.FirewallGroup/prod-servers
      + rule: v4 icmp 0.0.0.0/0
      + rule: v4 tcp 0.0.0.0/0 4646
      + rule: v4 tcp 203.0.113.7/32 22
      + rule: v6 icmp ::/0
  + vultr.FirewallGroup/prod-clients
      + rule: v4 icmp 0.0.0.0/0
      + rule: v4 tcp 203.0.113.7/32 22
      + rule: v6 icmp ::/0

  Plan: 4 to create, 0 to update, 0 to replace, 0 to delete.
  ```
- **Deletes.** An object that is already gone counts as deleted. Vultr refuses to delete a VPC for up to about 20 s
  after its servers are gone, and the engine retries. Vultr deletes a firewall group that instances use, so tent
  never deletes one that nodes of the cluster use ([11.5](#115-firewall-and-host-firewall)).
  - **VPCs that nodes use: not built yet.** The dedupe keeps the oldest VPC, which may not be the one that holds the
    cluster's nodes, and nothing stops the delete of a VPC that nodes use. `delete cluster` deletes the VPC only
    after every node is gone ([13.7](#137-tent-delete-cluster---yes)), so this matters for a duplicate VPC that
    holds nodes during `update cluster`. Vultr refuses that delete with `ErrInUse`, the engine retries it until the
    change's deadline, and then the apply fails.

### 11.2 Server discovery: seed and refresh

Vultr cannot assign fixed private IPs, go-discover does not support Vultr, and the load balancer is always public.
Vultr's own Nomad guide puts static private IPs into `retry_join`, which goes stale as soon as servers are replaced.
tent therefore uses the generic seed-and-refresh strategy
([ADR-0016](adr/0016-server-discovery-seed-and-refresh.md)):

1. **Seed.** At creation, tent renders the private IPs of the servers that already exist into the node's
   `05-join.hcl`. It takes them from `GET /v2/instances/{id}/vpcs` (`ip_address`, `mac_address`). On first bootstrap,
   `server-0` is created first, and the remaining servers get `[server-0]`. Serf join is transitive, so
   `bootstrap_expect` sees every server.
2. **Refresh.** On boot and every 60 seconds, tent-node asks a known server for `GET /v1/status/peers` and rewrites
   `05-join.hcl` whenever the peer set changes.
3. **Rollout guard.** Before replacing servers, every node must be healthy. Between server replacements tent waits at
   least one refresh interval.

### 11.3 Creating a node

```
GET  /v2/instances?tag=tent/op=<op>          → the cluster's instance of this op, if any: adopt it, skip to the wait
the inventory (11.1)                         → the VPC, the firewall group of the role, the SSH keys
POST /v2/instances {region, plan, os_id, label, hostname, tags: [<canonical labels>],
                    sshkey_id: [...], firewall_group_id, attach_vpc: [vpc],
                    user_data, backups: "disabled"}              → 202 {instance.id}
GET  /v2/instances/{id}, every 5 s           → until status=active, power_status=running, server_status=ok
GET  /v2/instances/{id}/vpcs                 → until it lists the private IP (seed lists, LB membership)
```

`Nodes.Create` follows [ADR-0015](adr/0015-idempotency-without-unique-names.md): Vultr's names are not unique, so a
create that may have been carried out is never sent again.
1. **Search.** It lists the instances with the tag `tent/op=<op>`, adopts the cluster's instance it finds and goes
   on to step 5. Of several, it adopts the oldest, then the one with the lowest id, and logs a warning that names the
   others; a date that does not parse counts as the newest. It skips, with a warning, an instance whose tent tags do
   not decode or name another cluster.
   - It fails when the instance it finds has another name, node group, role or zone than the request: the caller
     gave one `op` to two nodes. For example: `create node prod-servers-0 of cluster prod: instance <id> with the
     operation id <op> is another node: group dev (want servers); each node needs its own operation id`.
2. **Infrastructure.** It reads the inventory and takes the copies that the inventory keeps: the cluster's VPC, the
   firewall group of the node's role (`<cluster>-servers` for a server or combined node, `<cluster>-clients` for a
   client node) and all the cluster's SSH keys. It fails before the create when the cluster has no VPC or no such
   group, or when the zone is not the VPC's region.
3. **Create.** One POST. The region is the zone, the plan the machine type, and `os_id` comes from the image table
   ([11.7](#117-zones-placement-and-availability)). The label and the hostname are the node's name. The tags are the
   canonical labels: cluster, node group, role, `op`, and the spec hash when there is one. The user data goes in
   base64.
4. **Lost answer.** govultr's own retries are off ([11.8](#118-api-client-rate-limits-cost)). After a create without
   an answer (`ErrUnavailable`), or with an answer that holds no instance id, it searches by the `op` tag once more
   and adopts what it finds. When that search fails or lists nothing yet, the error matches `ErrUnavailable`, and a
   `Create` with the same `op` searches before it creates. Vultr listed each new instance by tag on the first request
   after the create answer (spikes 2026-09-25 and 2026-09-27).
5. **Readiness.** It reads the instance at once, then every 5 s. `server_status` goes through `installingbooting`.
   The node is ready when the status fields read `active`, `running` and `ok` and `GET /v2/instances/{id}/vpcs`
   lists its address in the VPC; `0.0.0.0` counts as no address. The wait has no deadline of its own: the caller
   gives every `Create` one through its context. `update cluster` gives 10 minutes to each call, search, inventory
   and wait included ([13.2](#132-tent-update-cluster---yes)); the API reported new instances ready after 46–73 s
   (below). When the context ends first, the error matches the context's error and names the instance.

- **Time before the POST.** Steps 1 and 2 are list calls, one after another: the search, then the inventory's lists
  of SSH keys, VPCs, firewall groups, the cluster's instances and the rules of each firewall group, 7 calls for a
  cluster with both groups. Each Vultr list call took 0.9–2 s on 2026-09-28
  ([platform notes §3.1](platform-notes.md#31-api-basics-and-access-control)), so `Create` took 8–10 s before its
  POST. `update cluster` creates nodes one at a time, and each `Create` reads its own inventory; one inventory for
  several nodes would save that time, and is not built.
- **Errors.** An invalid request fails without a call, such as `create request: no operation id`. Every other error
  reads `create node <name> of cluster <cluster>: …`, such as
  `create node prod-servers-0 of cluster prod: cluster prod has no VPC; apply its infrastructure first`. A refusal at
  an account limit ends with the hint of [11.7](#117-zones-placement-and-availability).
- **Root password.** The create answer holds the instance's root password (`default_password`), and no other answer
  does. tent keeps only the id from it.
- **Listing.** `Nodes.List` lists the instances with the tag `tent/cluster=<cluster>`, sorted by name, then by id,
  and reads each one's address in its VPC with `GET /v2/instances/{id}/vpcs`.
  - A node's name is its hostname, which only a reinstall changes and which Nomad uses as the node name, or its label
    when it has no hostname. The label can be changed in the Vultr console.
  - It skips, with a warning, an instance whose tent tags do not decode, such as one with a tag in upper case, which
    the case-insensitive filter lists too, or whose cluster label names another cluster.
  - When the address read answers 404, it reads the instance. It skips the instance only when that read answers 404
    too: the instance was deleted after the list. Otherwise it keeps the instance without a private address, so a
    caller does not create a second node with its name. A 404 alone does not count as gone: in the spike of
    2026-09-28 the first read of a pending instance's addresses, 31 s after the create, answered with the address,
    and what Vultr answers in the first 30 s is still open ([platform notes §3.5](platform-notes.md#35-vpc)).
    `0.0.0.0` counts as no address.
- **VPC at creation.** The VPC is attached only at creation. Attaching later reboots the VM. cloud-init configures the
  private interface statically from metadata. MTU is 1450, and interface names vary, so interfaces are matched by MAC
  or CIDR, never by name.
- **Boot time.** Spike 2026-09-25, `vc2-1c-1gb` in `ams`, counted from the create call
  ([platform notes §3.4](platform-notes.md#34-user_data-metadata-and-identity)):
  - The API reports `active/running/ok` after 46–73 s, sometimes before the kernel has started. Readiness in the API
    does not mean the OS is up.
  - sshd becomes public about 31 s after kernel start, when a vendor-data script removes its
    `ListenAddress 127.0.0.1`. cloud-init, vendor data included, finishes 32–37 s after kernel start.
  - From outside, cloud-init was done after 129–149 s. E2E and rollout timeouts allow twice that: 5 minutes per new
    node.
  - Vultr's vendor data no longer upgrades packages. So tent-node can run from cloud-init `runcmd`; an earlier start
    would save only seconds.
  - Deploying from snapshots adds 10–15 minutes, so it is avoided.

### 11.4 Removing a node

- **No graceful shutdown.** Vultr's `halt` is a hard power-off (verified 2026-09-25), and the API has no graceful
  shutdown (`GracefulShutdown=false`).
  - `Nodes.Stop` sends `POST /v2/instances/{id}/halt`: the node's processes get no chance to stop.
  - `Nodes.Delete` sends `DELETE /v2/instances/{id}`, which destroys the instance at once, even while it runs.
  - An instance that is gone (404) counts as stopped or deleted. Other errors name the node, such as
    `stop node prod-servers-0 (<id>): …`.
- **Servers** follow the Nomad-API removal path of [ADR-0017](adr/0017-api-driven-server-removal.md):
  1. surge;
  2. transfer leadership if needed;
  3. `DELETE` the old instance;
  4. remove the Raft peer, then `force-leave` with `prune`;
  5. verify.
- **Clients:** drain through the Nomad API → `DELETE` → purge.

### 11.5 Firewall and host firewall

- **Cloud side.** The firewall groups carry the internet-facing access rules of the model
  ([7.2](#72-intents-the-providers-input)). The rules are accept-only, and unmatched inbound traffic is dropped. They
  filter the public interface only: a group without a 4646 rule blocked 4646 from the internet within 12 s, while
  4646 over the VPC stayed open (spike 2026-09-25).
- **Rules per group** ([ADR-0023](adr/0023-vultr-inventory-dedupe-and-images.md)).
  - `<cluster>-servers`, for the server or combined group: the rules to every node and the rules to the servers.
  - `<cluster>-clients`, only when the cluster has a client group: the rules to every node.
  - One Vultr rule per source prefix of an access rule, `v4` or `v6` by the prefix.
- **Rule text.** tent compares rules by a text: the IP type, the protocol and the subnet, then the port when the rule
  has one and `source=<source>` when it has a source, such as `v4 tcp 203.0.113.7/32 22` or `v6 icmp ::/0`. It reads
  a rule as Vultr may write it: the IP type and the protocol in any case, an address in any of its forms, a range of
  one port (`22:22`) as that port, and an ICMP rule with a port.
  - Vultr lists a rule created without a source with the rule's own subnet as its source, such as
    `"source": "203.0.113.7/32"` (checked 2026-09-27, [platform notes §3.6](platform-notes.md#36-firewall-groups)).
    A source equal to the rule's subnet is no source. tent compares the parsed prefixes, so `::/0` and
    `0:0:0:0:0:0:0:0/0` are the same.
  - tent's rules have no source, so a rule with any other source, such as `cloudflare`, a load balancer id or
    another subnet, is always extra.
- **Plan.** A missing group plans a create with a `+ rule:` line per rule ([11.1](#111-resources)). An existing group
  plans an update when its rules differ: a `+ rule:` line for each missing rule and a `- rule:` line for each rule to
  delete, sorted by the rule text. After `access.ssh` changes from `203.0.113.7/32` to `198.51.100.0/24` and
  `2001:db8::/48`:

  ```
  ~ vultr.FirewallGroup/prod-servers
      + rule: v4 tcp 198.51.100.0/24 22
      - rule: v4 tcp 203.0.113.7/32 22
      + rule: v6 tcp 2001:db8::/48 22
  ```
- **Apply.** It creates the group when there is none, then lists the group's rules afresh, so it sees what an earlier
  attempt did.
  - It adds the missing rules, then deletes the extra rules and every copy of a wanted rule but the one with the
    lowest id. So a run that swaps a rule, such as a new `access.ssh`, never leaves a moment with neither rule.
  - When the group lacks room for the additions (its rules and the missing ones pass its limit), or Vultr refuses an
    addition as over the group's limit, it deletes first.
  - A rule that is already gone counts as deleted.
  - A rule create without an answer is retryable with no search first. The retry lists the rules first and does not
    add a listed rule again. When the list lags and the retry sends the create again, Vultr refuses the second copy
    with `400 This rule is already defined`, and tent counts that as done.
- **Rule limit.** A group holds at most its `max_rule_count` rules; tent takes 50 for a new group or one that
  reports none. The plan fails when a group needs more, for example with
  `firewall group prod-servers needs 53 rules; Vultr allows 50`. When Vultr refuses a rule create because the group
  is full, the error names that limit ([11.7](#117-zones-placement-and-availability)).
- **Delete guard.** tent does not delete a firewall group that a node of the cluster uses. Vultr deletes such a group
  (204) and leaves the instance without a firewall group (checked 2026-09-27,
  [platform notes §3.6](platform-notes.md#36-firewall-groups)), and the image allows root login with a password
  (below).
  - Before the delete, tent lists the instances with the tag `tent/cluster=<cluster>` afresh and does not delete
    the group while any of them has it. It names those nodes by hostname.
  - When the cluster no longer wants the group, the delete fails, and the engine does not retry it:
    `firewall group prod-clients (ID <id>) still protects nodes prod-workers-0 (<id>), and tent does not delete a
    firewall group that nodes use; delete those nodes first`.
  - A duplicate is reported instead ([ADR-0015](adr/0015-idempotency-without-unique-names.md)): tent logs a warning
    that names the group and the nodes, and the apply goes on. Every run reports it again until those nodes are
    replaced. The new nodes join the copy that the inventory keeps, and the next run deletes the duplicate.
  - It counts every instance that the tag filter lists. A node whose tent tags do not decode, which `Nodes.List`
    skips, still blocks the delete, so `delete cluster` stops at the group and names the node.
  - It cannot see an instance without the cluster's tag in the group: one made by hand, or one of another cluster.
    Vultr would delete the group and leave such an instance without a firewall group.
  - The engine retries a failed list after a 429, a 5xx or no answer, and an answer to the delete that matches
    `ErrInUse`. A 401 or a 403 on the list fails the delete at once.
  - Prune and the removal of duplicates delete through the same guard. `delete cluster` deletes the nodes first
    ([13.7](#137-tent-delete-cluster---yes)).
- **Order in `update`.** Removing the last client group makes the plan delete `<cluster>-clients` while the surplus
  clients still use it. A prune before the node scale-down would stop at the guard, and the refusal is not retried,
  so the run would never reach the scale-down. So `update` applies the infrastructure without its deletes first,
  creates and removes nodes, then applies the deletes in a second engine pass
  ([13.2](#132-tent-update-cluster---yes)).
- **Host side.** Vultr's Ubuntu 24.04 image enables ufw: deny incoming, allow 22/tcp only. That blocks Nomad ports on
  the VPC as well, so tent-node's `hostfirewall` phase must disable ufw and apply tent's nftables ruleset.
- **SSH.** The image allows root login with a password, and password guessing from the internet starts within minutes.
  The firewall groups allow 22/tcp only from `access.ssh`, and an empty list closes it.
- **Sysctls.** Vultr's vendor data writes `/usr/lib/sysctl.d/90-vultr.conf` and makes it immutable. tent-node puts
  its sysctls into a later file, such as `/etc/sysctl.d/99-tent.conf`.

### 11.6 user_data

- **Size.** The API accepts at least 4 MiB (spike 2026-09-25). tent keeps its own 64 KiB budget for NodeConfig. A
  65,508-byte user_data worked end to end: the metadata service served all of it, and cloud-init wrote its
  `write_files` payload intact. Larger payloads are pointless and were not tested on an instance.
- **Contents.** A cloud-config that disables package update and upgrade (Vultr's vendor data sets the same today; tent
  keeps it explicit), plus the NodeConfig.
- **Scrubbing.** Once the node has joined the cluster, the core calls `Nodes.ScrubUserData`. It PATCHes the user
  data to a stub that holds no secrets and no modules:

  ```
  #cloud-config
  # tent removed this node's user data after the node joined the cluster
  ```

  - The PATCH changes nothing else. govultr sends `"tags": null` with it, and Vultr keeps the tags (spike 2026-09-27,
    [platform notes §3.3](platform-notes.md#33-instances)).
  - An instance that is gone counts as scrubbed. The PATCH is idempotent, so after an error that matches
    `ErrUnavailable` the caller may send it again.
  - Verified 2026-09-25: the metadata service serves the stub 4 s after the PATCH, and after a restart cloud-init
    neither re-runs `runcmd` nor changes the instance-id. Per-boot modules would run from the stub, so the stub
    contains none.

### 11.7 Zones, placement and availability

- **One failure domain.** A Vultr cluster lives in a single data center. There are no availability zones and no
  placement or anti-affinity parameters. Three servers survive the loss of a VM, not of the data center, and there is
  no guarantee that they run on different hosts. `validate` states this.
- **Preflight.** `Validate` checks the specs against the live API before tent changes anything. It fills in the
  defaults on copies and makes three calls:
  1. `GET /v2/regions/{region}/availability`: the plans of every type that the region can deploy now. An unknown
     region answers `400 Invalid region.` (checked 2026-09-27), an `ErrInvalid`. tent reads only a 400 `ErrInvalid`
     answer as an unknown region and skips the availability checks; any other error, such as a 405 or a 501, fails
     the preflight.
  2. `GET /v2/plans`: every plan. Its `locations` field does not mean "in stock", so tent does not use it.
  3. `GET /v2/os`: every image.

  It reports every problem at once as field errors, the cluster's first, then each node group's by name:
  - `Cluster prod: spec.cloud.region: Vultr has no region "xyz"`;
  - `NodeGroup workers: spec.machineType: plan "vc2-9c-9gb" does not exist`;
  - `NodeGroup workers: spec.machineType: plan "vc2-2c-4gb" is not available in ams now`;
  - `NodeGroup servers: spec.image: tent supports ubuntu-24.04 and ubuntu-26.04 on Vultr, not "debian-12"`;
  - `NodeGroup servers: spec.image: Vultr does not offer ubuntu-24.04 (os_id 2284) now`.

  Any other error of the API fails the preflight as it is (`preflight of cluster prod: …`).
- **Images.** tent maps image names to Vultr's `os_id` with its own table, checked against `GET /v2/os`
  ([ADR-0023](adr/0023-vultr-inventory-dedupe-and-images.md)):

  | Image | `os_id` | Vultr's name |
  |---|---|---|
  | `ubuntu-24.04` | 2284 | Ubuntu 24.04 LTS x64 |
  | `ubuntu-26.04` | 2760 | Ubuntu 26.04 LTS x64 |
- **Limits.** Vultr refuses a create that would pass a limit. tent shows Vultr's message and does not retry.
  - Account limits, such as the most instances, are opaque and not exposed by the API. Their errors end with
    `(an account limit can be raised in the Vultr console under Billing, Limits)`.
  - Vultr does not raise the limit of 5 VPCs per region or the most rules a firewall group holds, so their errors
    name the limit instead: `ams may already have 5 VPCs, the most Vultr allows in a region: …` and
    `firewall group prod-servers may already hold the most rules Vultr allows in a group: …`.
- **Deploy incidents recur.** E2E retries in a fallback region.

### 11.8 API client, rate limits, cost

- **API client.** `vultr.API` in `internal/cloud/vultr` lists the calls tent makes. `Client` implements it over
  govultr v3, pinned.
  - Lists are whole: the client follows the cursors, for at most 1,000 pages.
  - Calls take and return govultr's types, and fail with typed errors (below).
  - govultr's own retries are off. govultr sets them for every call at once, and it would send a POST again, which
    may create a second object.
  - The API key may not hold whitespace or control characters. The base URL may use `http` only for `localhost`,
    `127.0.0.0/8` and `::1`, since the key would travel in clear text.
- **Transport.** Every request goes through tent's own `http.RoundTripper`. It:
  - sets the `Authorization` header. The key prints as `[redacted]`;
  - limits the rate with a token bucket that holds one token and gets a new one every 100 ms: at most 10 requests/s
    with no burst, under Vultr's 30 requests/s per IP;
  - after a 429 with `Retry-After`, starts no request until that time has passed, waiting at most 1 minute;
  - sends a GET, HEAD, PUT, PATCH or DELETE up to 4 times in all after a failed attempt, a 429 or a 5xx other than
    501. Between attempts it waits `Retry-After`, at most 1 minute, or else 0.5 s, 1 s and 2 s;
  - never sends a POST again. It hands a POST to net/http without `GetBody`, because Go 1.26's HTTP/2 client sends a
    request again through `GetBody` when the server resets the stream with `PROTOCOL_ERROR`;
  - records the last answer of each call. The client builds its errors from that record, because govultr turns 429,
    5xx and failed connections into plain text ([platform notes §3.2](platform-notes.md#32-govultr-)).
- **Answers.**
  - A success with a body whose `Content-Type` is not exactly `application/json` is an error: govultr decodes no
    other type and would return an empty result. A DELETE reads nothing from its answer, so its type does not matter.
  - A list answer without Vultr's `meta` field is an error, since it would read as an empty list.
  - No error shows the body of a success, since it may hold a secret, such as the root password in the answer to an
    instance create ([11.3](#113-creating-a-node)). To a success status that govultr does not read, such as 207, it
    returns the body as its error; the client names the status instead.
  - The client follows no redirect, since one would send the API key to wherever it points.
  - Each attempt is bounded: 30 s to connect, 30 s for TLS, 1 minute for the answer's header, and 2 minutes for the
    whole attempt including the body. The context bounds the whole call, retries and pauses included.
- **Errors.** A call that gets an error status or no answer fails with an `*APIError`. It holds the method, the path,
  the status, the message (the answer's `error` field or its body, on one line, at most 512 bytes) and `RetryAfter`.
  It matches at most one class with `errors.Is`. 401, 403, 404, 409, 423, 429 and 5xx get their class from the
  status alone; for any other 4xx the message may decide.
  - `ErrNotFound`: 404.
  - `ErrRateLimited`: 429. The call was not carried out, and `RetryAfter` holds the wait the answer asked for.
  - `ErrForbidden`: 401 or 403.
  - `ErrInUse`: 409, 423, or a 4xx to a DELETE whose message says `are attached` or `in use`, such as the 400 that
    a VPC delete gets for 14–20 s after its instances are gone ([platform notes §3.5](platform-notes.md#35-vpc)).
    The object is busy or still in use, and the same request may succeed later. A create told that a name is in use
    gets the same answer every time, so it stays `ErrInvalid`.
  - `ErrLimitReached`: a 4xx whose message has the word `limit` or `limits`, or `reached the maximum`, and does not
    speak of the rate limit. A limit of the account or of an object was reached, for example the account's
    instance limit, 5 VPCs per region or 50 rules per firewall group. tent shows the message verbatim. Vultr raises
    account limits on request ([11.7](#117-zones-placement-and-availability)); the limits of objects stay.
  - `ErrInvalid`: 501, or a 400, 405, 413, 414, 415 or 422 whose message fits no class above. The same request
    gets the same answer.
  - `ErrUnavailable`: a 5xx other than 501, or no answer. For a POST the outcome is unknown, since Vultr may have
    carried it out ([ADR-0015](adr/0015-idempotency-without-unique-names.md)). A POST whose success the client
    cannot read, because of its type, a body that does not decode or a missing object, gets this class as well.
    When the call's context ended, the error matches the context's error too.
  - The provider marks `ErrInUse`, `ErrRateLimited` and, for idempotent calls, `ErrUnavailable` retryable for the
    engine ([6](#6-reconciliation-engine)).
- **Ids.** The client checks each id it puts into a path before the request goes out. An id must be ASCII letters,
  digits and `-`, and not empty; a firewall rule id must be positive. Any other id fails the call: it could change
  the path, and a delete sent to another path may answer 404, which a caller takes for success. A list of instances
  by an empty tag fails the same way, since it would hold every instance of the account.
- **Cost.** `GET /v2/plans` gives `monthly_cost`.
  - Hourly cost: tent computes `monthly_cost / 672` itself, because the API's `hourly_cost` divides by 730 and is
    about 8% low.
  - Minimum 1 hour per instance, and stopped instances are billed.
  - Some locations cost more (the `location_cost` multiplier, for example 1.5× in São Paulo).
  - Extras: a load balancer costs $10/month, a NAT gateway $0.03/hour.

### 11.9 Private topology (later)

- Instances are created with `vpc_only: true`: no public interface, egress through a managed NAT gateway on the VPC.
- The API is reached through a load balancer or a bastion.
- This is simpler than on Hetzner, which has no managed NAT.

---

## 12. Hetzner provider

The second provider. The design is complete and is implemented after Vultr (roadmap M4).

### 12.1 Resources

| Resource | Name | Managed by | Notes |
|---|---|---|---|
| SSH key | `<cluster>-<fp8>` | engine | Unique per fingerprint in a project. An existing key is adopted, but not labelled or deleted. |
| Network | `<cluster>` | engine | `ip_range` = `networking.cidr` |
| Subnets | — | engine | `control` /24 and `nodes` /20, type `cloud`, `network_zone` = `cloud.region` |
| Firewalls | `<cluster>-nodes`, `<cluster>-servers` | engine | `apply_to` label selectors; 50 rules per firewall, 5 firewalls per server |
| Placement groups | `<cluster>-<group>-<shard>` | engine | type `spread`, ≤ 10 servers each, not bound to a location, set at server creation |
| Servers | `<cluster>-<group>-<index>` | rollout via `Nodes` | see [12.4](#124-creating-a-node) |
| Load balancer (optional) | `<cluster>-api` | engine | label-selector targets (`tent/role=server`), TCP passthrough on 4646, private IPs |
| Lock | `<cluster>-lock` | statestore / provider | empty firewall, see [10.4](#104-locking) |

### 12.2 Address plan

```
10.64.0.0/16            network (one network zone per cluster)
├─ 10.64.0.0/24         subnet "control": only explicitly assigned IPs
│   ├─ 10.64.0.1        gateway (first address of the network, reserved by Hetzner)
│   ├─ 10.64.0.2        internal load balancer (optional, later)
│   └─ 10.64.0.10–.16   Nomad server slots 0–6 (up to 5 servers + 2 surge)
└─ 10.64.16.0/20        subnet "nodes": clients, auto-assigned via attach ip_range
```

- The default CIDR avoids the Docker bridge (`172.17.0.0/16`) and the Nomad bridge (`172.26.64.0/20`).
- The private network MTU is 1450.

### 12.3 Server discovery

Hetzner can assign fixed private IPs, so it uses static slots
([ADR-0009](adr/0009-server-discovery-fixed-ip-slots.md)):

- `05-join.hcl` always lists all 7 slot IPs.
- A surge server takes a free slot, and the replaced server frees its own.

The seed-and-refresh timer still runs. It is harmless here, and it would cover slot changes.

| Strategy | Pros | Cons | Use |
|---|---|---|---|
| **Fixed IP slots** | Free, no token on nodes, and the list never goes stale. | Only via `attach_to_network`, so each server is created powered off, attached, then powered on: +2 actions. | **Hetzner default** |
| Seed + refresh | Works without fixed IPs. | A timer on each node. | Vultr default; a Hetzner fallback |
| Internal LB (`public_interface: false`) | One stable address. | About €7 per month. | private topology (later) |
| `exec=` + hcloud CLI | Documented by Nomad. | An API token on every node. | rejected |

### 12.4 Creating a node

```
1. POST /servers {name, server_type, image, location, ssh_keys, labels, user_data,
                  placement_group, firewalls, public_net, start_after_create: false}   # no networks
2. wait for the create action
3. POST /servers/{id}/actions/attach_to_network
       servers: {network, ip: <slot IP>}
       clients: {network, ip_range: <nodes subnet>}
4. wait for the action
5. POST /servers/{id}/actions/poweron, then wait
```

- **Resumable.** Each step checks the current state first. A `uniqueness_error` on step 1 means "adopt it if the
  labels match".
- **Waiting.** Waits use `Action.WaitFor`, which batches up to 25 action ids, with exponential `WithPollOpts`.
- **Servers without any public IP** must get their network at creation, so private topology uses an internal load
  balancer for joining.

### 12.5 Removing a node

1. `POST /servers/{id}/actions/shutdown` sends an ACPI shutdown. tent polls until the server is `off`, falling back
   to `poweroff` after a timeout.
2. `DELETE /servers/{id}`.

With `leave_on_terminate = true`, the ACPI shutdown makes a server leave the Raft peer set gracefully. That is the
`GracefulShutdown` optimization of [ADR-0017](adr/0017-api-driven-server-removal.md). The Nomad-API checks still run
afterwards.

### 12.6 Zones, placement, availability, rate limits, cost

- **Zones.** A cluster lives in exactly one network zone, and only `eu-central` (fsn1, nbg1, hel1) has several
  locations.
- **Placement.** Nodes are spread over the group's zones. A surge node goes to the zone of the node it replaces.
  Placement groups keep each shard of ≤ 10 servers on different physical hosts.
- **Availability.** The preflight checks `server_types[].locations[].available`. tent handles
  `412 resource_unavailable` with zone fallback.
- **Account limit.** The default is 5 servers, and Hetzner does not expose limits in the API, so the check is
  advisory.
- **Rate limit.**
  - The RoundTripper throttles adaptively on `RateLimit-Remaining` and `RateLimit-Reset`.
  - Never use `datacenter`: `/datacenters` returns 410 from 2026-10-01. Use `locations[].deprecation` instead of the
    removed `deprecated`.
- **Cost.** `GET /pricing` returns the prices. A minimal HA cluster of 3×CX23, 2×CX33 and 5 IPv4 addresses costs
  about €36 per month excluding VAT (Sept 2026 prices).

---

## 13. Lifecycle flows

### 13.1 `tent create cluster`

- Generates or loads the specs and writes them to the state store without touching the cloud, as in kops.
- `create cluster --yes` and `create -f FILE --yes` then build the cluster in the cloud, as `update cluster --yes`
  does ([13.2](#132-tent-update-cluster---yes)). `create -f` with node groups only builds the cluster they belong
  to.
  - With `-o table` tent prints the lines of the create, such as `cluster prod created`, then what
    `update cluster --yes` prints.
  - With `-o json` or `-o yaml` it prints one document: `{"changes": [...], "update": <the plan it applied>}`.
    `update` is left out when the update failed.
  - When the create fails, the update does not run. Running the same command again finishes an interrupted build:
    the create reports the stored objects as `unchanged`, and the update goes on.
- `create cluster --dry-run --yes` is refused: `--dry-run writes nothing, so it takes no --yes`. `replace` has no
  `--yes`.
- `create -f file.yaml` loads multi-document YAML.

### 13.2 `tent update cluster [--yes]`

**Built in M1.** `update cluster` brings the cluster's infrastructure and the sizes of its node groups to the specs.
The nodes are empty machines. They boot a placeholder cloud-config without secrets, which turns off the package
updates and upgrades of the first boot.

```
 1. load specs → defaults → validate → provider.Validate (region, plans, images)
 2. plan: inventory → provider.BuildInfra → engine plan; Nodes.List → node changes (13.4); the completed spec;
    the missing secrets (M2.1)
 3. without --yes: print the plan and stop
 4. lock → check the tent version → steps 1 and 2 again; the plan made under the lock is the one applied
 5. raise the tent version → write the missing secrets (M2.1)
 6. the infrastructure's task changes (Plan.ApplyTaskChanges)
 7. node creates, one at a time: server and combined groups first, then client groups, by group and index;
    each with a new operation id (cloud.NewOpID)
 8. waits for listed nodes that are not ready yet, by name: Create again with the node's operation id
 9. node deletes, one at a time, by name: duplicates, surplus nodes, nodes of groups not in the spec
10. the infrastructure's deletes: prune and duplicates (Plan.ApplyDeletes)
11. write cluster.completed.yaml → unlock
```

- **Failures.** The first step that fails stops the run, and the next run finishes the job. A run cut after a
  create's POST leaves an instance with its operation id. The next run counts it, and while it is not ready yet,
  plans `~ node prod-workers-1 (ID <id>, wait until it is ready)` and waits for it. No second instance is created.
- **Completed spec.** `cluster.completed.yaml` holds the specs with every default filled in
  ([3.3](#33-api-rules)). It counts as a change when the stored one is missing or differs, so the first run writes
  it, and so does a run after a spec change that changes nothing in the cloud. It is written only after every other
  step has succeeded.
- **Deletes come last.** An object that the plan deletes may still hold nodes that step 9 removes, such as
  `<cluster>-clients` after the last client group is gone. Vultr's firewall guard refuses to delete a group that
  nodes use and the engine does not retry that ([11.5](#115-firewall-and-host-firewall)), so a delete in step 6
  would stop the run before step 9. The deletes therefore wait for step 10.
- **Deadlines.** `Nodes.Create` has no deadline of its own ([11.3](#113-creating-a-node)), so `update` gives each
  create and each wait 10 minutes. The engine gives each infrastructure change 5 minutes
  ([6](#6-reconciliation-engine)).
- **Single server.** `update` validates the specs, so a cluster with one server needs `--allow-single-server` on
  every run, as every command that validates specs does.
- **Output.**
  - The plan goes to stdout: the infrastructure's lines as the engine writes them ([6](#6-reconciliation-engine)),
    one line per node change, a blank line, and a line of counts per part that changes. The last line names the
    objects that the plan writes to the state store, in write order: the missing secrets, then the completed spec,
    such as `State: secrets/gossip.key and cluster.completed.yaml will be written.` A plan that writes only state is
    that line alone. Operation ids and the secrets' contents do not show. A plan without changes is `No changes.`,
    and a plan with changes adds `run with --yes to apply the changes` on stderr. An example with every kind of node
    change (`internal/app/testdata/update_plan.golden`, its infrastructure lines left out):

    ```
    + node prod-servers-2 (server, vc2-2c-4gb, ams)
    + node prod-workers-1 (client, vc2-4c-8gb, ams)
    ~ node prod-servers-1 (ID instance-2, wait until it is ready)
    - node prod-old-0 (ID instance-7, not in the spec)
    - node prod-workers-0 (ID instance-5, duplicate)
    - node prod-workers-3 (ID instance-8, surplus)

    Plan: 2 to create, 1 to update, 0 to replace, 1 to delete.
    Nodes: 2 to create, 1 to wait for, 3 to delete.
    State: pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token and cluster.completed.yaml will be written.
    ```
  - With `--yes`, tent prints the plan made under the lock (step 4), applies it with each step on stderr as it
    happens ([14](#14-cli)), and then prints a blank line and one line in the past tense, each part only when it
    changed, such as `Applied: 4 created, 0 updated, 0 replaced, 0 deleted. Nodes: 5 created, 0 waited for, 0
    deleted. Wrote pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token and
    cluster.completed.yaml.` The writes to the state store print no progress lines. A cluster without changes prints
    `cluster prod is up to date`.
  - `-o json` and `-o yaml` print the plan as data: `{"infrastructure": <the engine's plan>, "nodes": [...],
    "secrets": ["pki/private/ca.key", ...], "completedSpec": true}`, the node changes in the order they run and the
    secrets in the order they are written. `secrets` is left out when the store holds them all. With `--yes` they
    print only the plan that was applied, with `"applied": true`.
- **`--exit-code`.** Without `--yes`, a plan with changes makes tent exit with 2 and print no error, for drift
  detection in CI. With `--yes` it is refused.

**Built in M2.1.** Step 2 of the target below, ensure secrets, runs after tent raises the tent version and before the
infrastructure (step 5 above). The secrets are the CA's key and bundle, the gossip key and the ACL bootstrap secret
([9.1](#91-pki), [10.2](#102-layout)).
- **Plan.** Each plan, also without `--yes`, reads which of the four objects the store holds and makes the missing
  ones in memory. A cluster built before M2.1 gets its secrets on its next `update --yes`.
- **Writes.** The order is `pki/private/ca.key`, `pki/ca-bundle.pem`, `secrets/gossip.key`,
  `secrets/acl-bootstrap-token`. Where the store has conditional puts, each write uses `IfNoneMatch`: a secret that
  another run wrote meanwhile stays, and the run stops with `<path> was written meanwhile; run the command again`.
  Elsewhere the writes are plain puts under a best-effort lock ([10.4](#104-locking)). Two runs that both hold it
  can interleave their puts and leave a key and a bundle of different CAs. Every later plan then fails with
  `CA key: matches no certificate of the CA bundle`. While no node trusts the CA yet, delete both objects by hand
  and run `update` again ([ADR-0024](adr/0024-cluster-pki-storage-and-certificates.md)).
- **A key without a bundle**, left by a run cut between the two writes, stays: the next plan signs a new CA
  certificate with it, and the run writes that as the bundle.
- **A bundle without a key, a key that matches no certificate of the bundle, or a stored gossip key or ACL bootstrap
  secret that does not check** fails the plan, and the error names the paths. tent never replaces them: a new CA
  would cut off every node that trusts the old one.

**Target, with Nomad.** The whole flow:

```
 1. lock → load specs → defaults → validate (+ live: types, regions/locations, images, availability)
 2. ensure secrets (idempotent): CA, gossip key, ACL bootstrap token
 3. model → provider.BuildInfra → engine plan → print → apply without the deletes
    (SSH keys, network, firewalls, [placement groups], [LB]; tasks that do not depend on each other apply in parallel)
 4. servers first: create missing servers
    (Hetzner: into slots; Vultr: server-0 first, then the rest seeded with existing server IPs)
 5. wait for a leader → ACL bootstrap with the pre-generated secret
 6. day-1 over the API: node pools (descriptions/meta), other cluster settings
 7. clients: for each missing node → intro token → Nodes.Create (seeded with current server IPs) → wait ready
 8. Vultr: scrub user data of nodes that registered (Nodes.ScrubUserData)
 9. scale down surplus nodes: drain → stop/delete → purge
10. apply the plan's deletes, the prune and the duplicates (a second engine pass)
11. validate → write cluster.completed.yaml + history → unlock
12. report: "N nodes are out of date (reason: config diff) → run tent rolling-update cluster"
```

- The secrets (step 2) are built (M2.1, above), but no node uses them yet. The NodeConfig with the seed of server
  addresses and the intro tokens (steps 4 and 7), the ACL bootstrap (step 5), the day-1 configuration (step 6) and
  the scrub (step 8) come with Nomad in M2.
- The drain and the purge (step 9), `validate` and the history (step 11) and the report of outdated nodes (step 12)
  are not built yet.

`update` never replaces existing nodes. With Nomad it reports outdated nodes and why. Replacement is always explicit.

### 13.3 `tent rolling-update cluster [--yes]`

**Order.** All server groups roll before any client group. This follows Nomad's upgrade guide, and the planner refuses
any state in which clients would run a newer Nomad than servers. A node is outdated when its `tent/spec-hash` differs
from the desired hash, or when `--force` is given.

**Servers**, one at a time. Precondition: autopilot healthy, failure tolerance ≥ 1, every node healthy.

```
create the replacement (Hetzner: free slot; Vultr: seeded with the current servers)
→ wait: it is a Raft voter and autopilot reports Healthy
     (GET /v1/operator/autopilot/health answers HTTP 429 while unhealthy: treat as "not yet")
→ if the old server is the leader: PUT /v1/operator/raft/transfer-leadership to an updated server
→ stop the old server: ACPI shutdown where GracefulShutdown (graceful leave via leave_on_terminate),
  otherwise hard stop/DELETE (Vultr)
→ if it is still a peer: DELETE /v1/operator/raft/peer?id=<raft id>; PUT /v1/agent/force-leave?node=<name>&prune=true
→ wait: peer count back to N, autopilot healthy
→ seed-and-refresh providers: wait at least one join refresh interval
→ delete the old VM (if not already) → next
```

See [ADR-0017](adr/0017-api-driven-server-removal.md).

**Clients**, per group, honouring `maxSurge` and `maxUnavailable`:

```
create surge node(s) → wait until registered and ready
     (optionally start ineligible via client.default_ineligible, Nomad ≥ 2.0.3; check the node; mark eligible)
→ old node: mark ineligible → drain (deadline, honours job migrate{} blocks) → wait for completion
→ stop/delete the VM → purge the node in Nomad → validate → next batch
```

- **Targeted.** Replacements call `Nodes` directly and never re-run the whole plan.
- **Crash-safe.** Because the new node is created first, a crash leaves an extra outdated node, and the next run
  finishes the job.

### 13.4 Scaling

**Built in M1.** `update` plans the node changes from `Nodes.List` ([13.2](#132-tent-update-cluster---yes)). Only
instances with the cluster's label count. A node group's nodes are the instances whose group label names it. The
planner is in `internal/app`, and it moves to `internal/rollout` with the drain and the quorum checks
([ADR-0005](adr/0005-immutable-nodes-and-nomad-aware-rollouts.md)).
- **Scale up.** A missing node gets the lowest free index: its name `<cluster>-<group>-<index>` is one that no listed
  instance of the cluster has, whatever its group. It goes into the group's zone with the fewest nodes, the zone
  listed first on a tie.
- **Scale down.** A group with more nodes than its size loses the newest ones, by creation time, then by id. Until
  tent runs Nomad there is no drain: `update` deletes the machines, servers included, without a quorum check.
- **Zones.** A node in a zone that its group no longer lists counts toward the group's size and stays. New nodes go
  only into the listed zones.
- **Duplicates.** Of instances with one name, the oldest stays and the others are deleted as `duplicate`.
- **Groups not in the spec.** An instance whose group label names no group of the spec, or is empty, is deleted as
  `not in the spec`.
- **Nodes that are not ready.** A node that is not ready yet, left by an interrupted run, counts, and `update` waits
  for it by calling `Create` with its operation id. A node without a valid operation id counts, and nothing waits
  for it.

**Target, with Nomad.**
- **Scale up** is part of `update`.
- **Scale down** is also part of `update`. It is always shown in the plan and always drains first. Victims are
  chosen in this order:
  1. outdated nodes;
  2. nodes in the most loaded zone;
  3. the newest nodes.
- **Server group size** can change 1 → 3 → 5 and back, one server at a time, never below quorum.

### 13.5 `tent upgrade cluster [--yes]`

- Reads the cluster's channel. The file is embedded in the binary, and a URL can override it.
- A channel defines recommended and supported Nomad versions, the CNI plugin version and default images per provider.
- The command proposes upgrades, rewrites the spec and prints the plan. Then run `update` and `rolling-update`.
- It never downgrades. Only channel-tested version pairs are allowed, because Nomad's 2.x version skew policy has not
  been restated.
- **Later:** an opt-in in-place upgrade strategy (swap the binary and restart).

### 13.6 `tent validate cluster [--wait 10m]`

- **Cloud checks.** Every group has `size` running machines. There are no unknown machines with the cluster marker
  and no duplicates.
  - Lock state is reported.
  - On Vultr, it warns about the single failure domain.
- **Nomad checks.**
  - A leader exists, and every expected server is an alive voter.
  - Autopilot is healthy.
  - Every expected client is `ready` and eligible.
  - Nomad versions are consistent.
  - Certificate expiry is more than 30 days away.
- **Output.** A table of failures. `--wait` loops until the cluster is valid, and the exit code is non-zero while it
  is not.

### 13.7 `tent delete cluster [--yes]`

**Built in M1.**

```
1. plan: the paths of the state; the cloud that the stored cluster.yaml names;
   Nodes.List → a delete per node; the inventory → an engine plan without tasks, which deletes every object
2. without --yes: print the plan and stop
3. lock → check the tent version → step 1 again
4. delete every node, one at a time, by name (Nodes.Delete)
5. list the nodes every 5 s until the cloud lists none, for up to 5 minutes; while it lists some, say once how many
6. a fresh inventory → the engine deletes the infrastructure in the order of InfraKinds (Plan.Apply)
7. delete the state: the secrets next to last, cluster.yaml and tent-version last → unlock
```

- **The cloud.** tent decodes the stored `cluster.yaml` as it is, without validating the specs or checking them
  against the cloud. So a cluster that is half built or whose specs are invalid can still be deleted, and the command
  takes no `--allow-single-server`.
  - A cluster on Vultr needs `VULTR_API_KEY` even without `--yes`, since the plan lists the nodes and the inventory.
  - A cluster without `cluster.yaml`, such as one left by an interrupted create, has no known cloud. tent deletes
    only its state and warns: `WARNING: cluster prod has no cluster.yaml, so tent cannot tell its cloud; deleting its
    state only`.
  - On a provider that tent cannot manage yet, such as `hetzner`, tent has made no cloud objects
    ([7.1](#71-interfaces)). It deletes only the state and warns with the provider's name.
  - A provider name that tent does not know, such as a typo in the stored `cluster.yaml`, is an error:
    `unknown cloud provider "<name>"`. tent deletes nothing.
  - A stored `cluster.yaml` that does not decode is an error that says to fix the file by hand and keep its provider
    and region ([3.3](#33-api-rules)). Deleting the file instead would make the delete remove the state alone and
    leave the cloud objects running.
- **Vultr.** tent refuses to delete a firewall group that a node of the cluster uses
  ([11.5](#115-firewall-and-host-firewall)), so the nodes go first. A node that `Nodes.List` skips, such as one whose
  tent tags do not decode, still blocks the delete of its firewall group, and `delete cluster` stops there and names
  it. The engine deletes the firewall groups, the VPC, then the SSH keys. The VPC delete fails with
  `400 The following servers are attached…` for 14–20 s after its instances are gone, so it is retried.
- **State.** The state is `tent-version`, the specs, the completed spec, and since M2.1 the four secrets: the CA's
  key and bundle, the gossip key and the ACL bootstrap secret ([10.2](#102-layout)). Other objects under the cluster
  in the store, such as another object under `pki/`, make tent refuse before it calls the cloud, unless `--force` is
  given; then they are deleted with the rest. The lock's lease goes when the lock is released.
- **Failures.** The first step that fails stops the delete. The state stays until the cloud's part has succeeded, so
  the next run still finds the cluster and finishes the job. `cluster.yaml` and `tent-version` go last, so a delete
  that stops while it deletes the state can run again too. The secrets go just before them, in the reverse of their
  write order ([13.2](#132-tent-update-cluster---yes)). So a delete cut among them never leaves a CA bundle without
  its key; at worst it leaves a key without its bundle, which `update` completes.
- **Output.** The plan goes to stdout, in the order of deletion, with a line of counts per part that deletes
  anything (`internal/app/testdata/delete_plan.golden`):

  ```
  - node prod-servers-0 (ID instance-1)
  - node prod-workers-0 (ID instance-4)
  - vultr.FirewallGroup/prod-servers (ID fw-1)
  - vultr.VPC/prod (ID vpc-1)
  - vultr.SSHKey/prod-99aabbcc (ID ssh-9)
  - state prod/cluster.completed.yaml
  - state prod/nodegroups/servers.yaml
  - state prod/secrets/acl-bootstrap-token
  - state prod/secrets/gossip.key
  - state prod/pki/ca-bundle.pem
  - state prod/pki/private/ca.key
  - state prod/cluster.yaml
  - state prod/tent-version

  Nodes: 2 to delete.
  Plan: 0 to create, 0 to update, 0 to replace, 3 to delete.
  State: 8 objects to delete.
  ```

  - Without `--yes`, a hint follows on stderr: `run with --yes to delete them`, or `--yes --force` with `--force`.
  - With `--yes`, tent prints the plan made under the lock (step 3), deletes with each step on stderr as it happens
    ([14](#14-cli)), and then prints a blank line and a line such as `Deleted: 2 nodes, 3 infrastructure objects, 8
    state objects.`
  - `-o json` and `-o yaml` print the plan as data: `{"nodes": [...], "infrastructure": <the engine's plan>,
    "state": [...]}`, with `"cloudUnknown": true` or `"unsupportedProvider": "hetzner"` when tent deletes only the
    state. With `--yes` they print only the plan that was applied, with `"applied": true`.

**Target.**
- Load balancers go before the nodes on Vultr. On Hetzner the order is load balancers → servers → placement groups
  → firewalls → network → owned SSH keys.
- Volumes are deleted only with `--delete-volumes`. Volumes created by CSI drivers carry no tent markers, a known
  kops leak.

### 13.8 Backups

- **`tent backup create`** reads `GET /v1/operator/snapshot` and stores the snapshot under `backups/`.
- **`tent backup restore`** writes it back with `PUT /v1/operator/snapshot`.
- **Scheduling.** The snapshot agent is Enterprise-only, so scheduled backups are the operator's cron or CI calling
  `tent backup create`.

---

## 14. CLI

The last column names the milestone that built the command. The spec commands of M0 work only on the state store;
`update cluster` and `delete cluster` of M1 reach the cloud. The other commands come with later milestones
([roadmap](roadmap.md)).

| Command | kops analogue | Purpose | Built |
|---|---|---|---|
| `tent create cluster [NAME] [flags] [--yes]`, `tent create -f FILE [--yes]` | `create cluster` | generate or load specs into the state store; with `--yes` also build the cluster ([13.1](#131-tent-create-cluster)) | M0; `--yes` in M1 |
| `tent get [NAME]`, `tent get clusters\|nodegroups [NAME...]`, all with `[--full]` | `get` | print a cluster's specs, list clusters or node groups | M0 |
| `tent get nodes` | `get instances` | list the cluster's nodes | — |
| `tent edit cluster [NAME]`, `tent edit nodegroup NAME` | `edit` | an editor, with validation and a diff before saving | M0 |
| `tent replace -f FILE` | `replace` | GitOps: replace stored specs with those of a file | M0 |
| `tent apply -f FILE` | — | `replace` + `update` | — |
| `tent update cluster [NAME] [--yes] [--exit-code]` | `update cluster` | infrastructure, node counts, day-1 configuration ([13.2](#132-tent-update-cluster---yes)) | M1, without Nomad |
| `tent rolling-update cluster [--yes] [--nodegroups a,b] [--force]` | `rolling-update cluster` | Nomad-aware replacement | — |
| `tent upgrade cluster [--yes]` | `upgrade cluster` | version bumps from the channel | — |
| `tent validate cluster [--wait 10m]` | `validate cluster` | cloud and Nomad health | — |
| `tent delete cluster [NAME] [--yes] [--force]` | `delete cluster` | full cleanup by ownership markers ([13.7](#137-tent-delete-cluster---yes)) | M1 |
| `tent export nomad [--ttl 24h]` | `export kubeconfig --admin` | short-lived operator credentials and env | — |
| `tent ui` | — | local mTLS proxy for the UI and CLI | — |
| `tent cost` | — | monthly and hourly cost of the cluster or plan (Vultr `/plans`, Hetzner `/pricing`) | — |
| `tent backup create\|restore` | etcd-manager backups | Raft snapshots | — |
| `tent toolbox dump` | `toolbox dump` | diagnostics bundle (via SSH) | — |
| `tent state unlock [NAME] [--force]` | — | remove a stale lock | M0 |
| `tent version` | `version` | build info | M0 |

**Global flags and configuration**
- `--state` (env `TENT_STATE`), `--name` (env `TENT_CLUSTER`), `-o table|yaml|json`, `-v` or `-vv`,
  `--log-format text|json`, and `--lock-timeout` (default 5m).
- A setting comes from its flag, then its environment variable, then the config file, then the built-in default.
- The config file is `$XDG_CONFIG_HOME/tent/config.yaml`. When `XDG_CONFIG_HOME` is unset or not an absolute path, it
  is `~/.config/tent/config.yaml` on every operating system, and without a home directory there is no config file. It
  holds defaults only, never secrets: the string keys `state`, `cluster`, `output` and `logFormat`. Unknown or
  duplicate keys and values that are not strings are errors that name the file and the line.
- A command for one cluster takes its name from `NAME`, else from `--name`. A `NAME` and a `--name` on the command
  line must agree. `get nodegroups` and `edit nodegroup` take the cluster from `--name`, as does `get` for a cluster
  named `cluster`, `clusters`, `nodegroup` or `nodegroups`.
- Cloud credentials come from the environment. tent reads `VULTR_API_KEY` only when a command reaches a cluster on
  Vultr: `update cluster`, `delete cluster` and `create --yes` ([7.1](#71-interfaces)). `HCLOUD_TOKEN` comes with
  the Hetzner provider.

**Output**
- Results go to stdout. Warnings, notices, progress and logs go to stderr.
- Warnings are plain lines that start with `WARNING:`, and `--log-format` does not change them. Logs use `log/slog`,
  as text or JSON by `--log-format`; `-v` shows info logs and `-vv` debug logs. tent's own logs are debug logs. The
  Vultr provider logs its warnings at the level `WARN`, which shows without `-v`, such as an object that it skips
  because its marker does not parse.
- `-o table`, the default, prints tables and lines of text, such as `node group workers created`. `-o yaml` and
  `-o json` print the same results as data. JSON never escapes HTML characters such as `<` and `&`.
- `tent get [NAME]` prints the Cluster and its node groups as YAML documents, the file that `create -f` and
  `replace -f` take. With `-o json` it prints them as a list, which those commands take too. It takes one `NAME`.
  `tent get clusters` and `tent get nodegroups` take several names and print tables, and with `-o yaml` or `-o json`
  the specs. On an empty store `tent get clusters` also says `no clusters in <store>` on stderr, because a mistyped
  `--state` looks like an empty store. `--full` fills in the defaults ([3.3](#33-api-rules)).
- `update cluster` and `delete cluster` print their plan on stdout ([13.2](#132-tent-update-cluster---yes),
  [13.7](#137-tent-delete-cluster---yes)). A plan with changes, without `--yes`, adds a hint on stderr, such as
  `run with --yes to apply the changes`. With `--yes` and `-o table` they print the plan made under the lock, just
  before the first change.
- With `--yes` they print each step on stderr as it happens: a line of text, or with `-o json` a JSON object on one
  line.
  - Lines: `creating vultr.VPC/prod`, `created node prod-servers-0 (10.64.0.3)`, `waiting for node prod-workers-1`,
    `deleted node prod-workers-3 (ID <id>)`, `retrying vultr.VPC/prod in 1.2s: <error>`,
    `skipped deleting vultr.VPC/prod (ID <id>): an earlier change failed`, `failed to create node prod-servers-0:
    <error>`, and `waiting for 3 nodes to go` once when a delete waits for the cloud to stop listing the nodes it
    deleted.
  - JSON: `{"type":"infrastructure","event":"started","kind":"vultr.VPC","name":"prod","action":"create"}` with
    `id`, `wait`, `cause` and `error` when they apply,
    `{"type":"node","step":"done","action":"create","name":"prod-servers-0","id":"<id>","address":"10.64.0.3"}` with
    `error` for a failed step, and `{"type":"wait","nodes":3}` for the wait of a delete.
  - With `-o json`, stderr mixes the JSON progress lines with plain `WARNING:` lines and the logs. A program reads
    the lines that start with `{`. The logs are text unless `--log-format json` makes them JSON objects too; they
    carry `level` and `msg`, which progress lines never have.
- Then `-o table` prints a blank line and one line in the past tense on stdout: `Applied: …`, `Nodes: …` and
  `Wrote …` (the objects written to the state store) for `update`, and `Deleted: …` for `delete`. `-o yaml` and
  `-o json` print the plan that was applied instead, with `"applied": true`.

**Spec commands**
- `create cluster` generates the Cluster and two node groups, `servers` and `workers`, or with `--combined` one
  group `nodes`. It needs `--provider`, `--region` and `--machine-type`. The other flags are `--zones`,
  `--worker-machine-type`, `--servers` and `--workers` (both default to 3), `--image`, `--ssh-key PATH` (a public key
  file; repeatable), `--ssh-access` and `--api-access` (CIDRs), and `--nomad-version`. The specs hold only what the
  flags set. `--dry-run` checks the specs and prints them without writing: it needs no state store, and with one it
  also checks them against the store.
- `create -f FILE` stores new objects: a Cluster with its node groups, or node groups for an existing cluster.
  `replace -f FILE` replaces objects that exist. `-f -` reads standard input. Both write at once, as in kops.
  `create --yes` then builds the cluster in the cloud ([13.1](#131-tent-create-cluster)).
- `create`, `replace` and `edit` check the whole cluster that results, and `--allow-single-server` lets them accept a
  server group of size 1. They write only the objects that differ from the stored ones and report the others as
  `unchanged`. `replace` and `edit` refuse to change a cluster's provider or region ([3.3](#33-api-rules)).
- A command that changes the state store can run again after an interruption. `create` writes `cluster.yaml` last,
  and running it again with the same specs finishes it.

**`tent edit`**
- It opens the one object, the Cluster or one node group, in an editor: the first of `$TENT_EDITOR`, `$VISUAL` and
  `$EDITOR` that is set, else `vi` (`notepad` on Windows). The value is a command line split at spaces, and double
  quotes keep spaces, as in `"C:\Program Files\Editor\editor.exe" --wait`. tent waits for the editor to exit, so an
  editor that returns at once needs its wait flag, such as `code --wait`.
- When the file does not decode or the spec is invalid, tent opens the editor again with the errors as YAML comments
  at the top. Saving the file unchanged, or empty, cancels.
- tent prints a valid edit as a unified diff and asks `Save? [y/N]`; `--yes` saves without asking. It saves only if
  the stored object has not changed since tent read it, and it refuses to rename the object.
- When the edit fails, is interrupted, or is cancelled after errors, tent keeps the edited file if it holds changes,
  and prints its path.
- While the editor runs, tent ignores Ctrl-C and Ctrl-\ so that the editor handles them, as git does. SIGTERM still
  cancels the edit, which stops when the editor exits.
- `edit` prints text: it refuses `-o json` and `-o yaml` on the command line and ignores `output` in the config file.

**Locks and interrupts**
- A command that changes the state store or the cloud takes the cluster's lock ([10.4](#104-locking)) and checks the
  tent version ([10.2](#102-layout)). A run that finds nothing to change takes no lock.
- While another tent holds the lock, a command waits up to `--lock-timeout` and says once who holds it, for example
  `cluster prod is locked by igor@laptop (pid 4242) for replace since 2026-09-27 10:00:00 UTC; waiting up to 5m0s
  (--lock-timeout)`. With `--lock-timeout 0` it fails at once.
- It warns when the store allows only a best-effort lock, and when it takes over the lock of a holder whose lease
  expired or whose tent ended.
- `state unlock` removes a lock whose holder expired or ended, and with `--force` any lock that a live holder can
  lose ([10.4](#104-locking)). For a cluster that is not locked and has nothing in the store, it fails with
  `not found`.
- The first Ctrl-C or SIGTERM cancels the command, which then releases its lock. A second one ends tent at once.

**Warnings, errors and exit codes**
- tent warns while `access.api` lets the whole internet reach the Nomad API, a `/0` range such as the default
  `0.0.0.0/0`: after `create`, `replace` or a saved `edit`, and before `update cluster --yes` applies changes. A
  command warns once, `create --yes` included.
- An error goes to stderr after `Error: `. An invalid spec prints `Error: invalid spec:` and then one indented line
  per problem, with the field path ([3.3](#33-api-rules)).
- Exit codes: 0 success, 1 error, 2 when `update cluster --exit-code` finds a plan with changes, and 130 when a
  second Ctrl-C or SIGTERM ends tent. Exit code 2 prints no error.

---

## 15. Testing

Details in [ADR-0012](adr/0012-testing-strategy.md). The E2E platform is chosen in
[ADR-0014](adr/0014-vultr-first-provider-and-e2e.md).

1. **Unit tests:**
   - defaults and validation;
   - PKI;
   - Nomad config rendering (golden HCL);
   - the label codecs;
   - the address plan;
   - the engine: golden plans, apply with fake time (`testing/synctest`);
   - rollout decisions as pure functions (cluster state → next step).
2. **Provider tests.**
   - Tasks and `Nodes` run against in-memory fakes of narrow interfaces that inject provider-specific failures.
   - Vultr: `internal/cloud/vultr/vultrfake` is an in-memory fake of `vultr.API`
     ([11.8](#118-api-client-rate-limits-cost)). It uses the client's types, errors and id checks.
     - A test seeds objects, sets the plans, images and region availability, reads the objects back without a call,
       and reads the calls that reached the fake.
     - It fails a call on a missing object with `ErrNotFound`, and a sixth VPC in a region or a rule past a group's
       `max_rule_count` (50 by default) with `ErrLimitReached`. It answers the availability of a region it does not
       know with `400 Invalid region.`, as Vultr does.
     - It lists a rule created without a source with the rule's own subnet as its source, and refuses a second copy
       of a rule with `400 This rule is already defined`, as Vultr does. A seeded rule without a source is the same
       rule as a created one.
     - Instances boot as `GetInstance` reads them: `pending`, then `active` with `installingbooting`, then `ok`, by
       default after 1 and 2 reads (`SetBootReads`). `ListInstances` shows an instance as the next read will and
       does not move its boot on, so lists that run at the same time cannot change when it boots. An instance gets a
       main IP from `198.18.0.1` on and, in each VPC, the lowest free address from the third host on, such as
       `10.64.0.3`. Only the create answer holds a `default_password`. The tag filter is exact but ignores case, and
       a PATCH with `"tags": null` keeps the tags.
     - As Vultr does, it deletes a firewall group that instances use and leaves them without one, and refuses to
       delete a VPC while instances are attached (`400 The following servers are attached to this VPC network: …`,
       `ErrInUse`).
     - It is simpler than Vultr in these ways. `GET /v2/instances/{id}/vpcs` lists nothing until the instance
       shows as `active`; Vultr listed the address 6–7 s after the create. A deleted instance is gone from every call
       at once. It deletes a VPC as soon as its instances are gone.
     - It injects faults into chosen calls: a lost answer after the fake carried the call out, so a lost create
       leaves its object (`LoseResponse`); a 429 with `Retry-After` (`Throttle`); a given error (`Fail`). A hook
       (`SetHook`) wraps every call, so a test can act at a chosen call, such as end the context of the run that
       makes it.
     - The infrastructure tasks, the preflight and `Nodes` run on this fake. Each task has an apply, re-plan, no-op
       test, and a lost create ends with exactly one object. The plan of the cluster of [3.1](#31-kinds) is a golden
       file ([11.1](#111-resources)). The `Nodes` tests wait for readiness with fake time (`testing/synctest`).
     - Core integration tests (item 3) run the real Vultr provider on this fake.
   - Hetzner: a fake of hcloud-go's `I*Client` interfaces that injects `uniqueness_error`, actions and 412.
3. **Integration tests without a cloud**, like kops' `tests/integration`.
   - Full `update`, `rolling-update` and `delete` flows run against the cloud fakes of item 2 and a fake Nomad API.
   - Golden files hold the plan and the sequence of operations.
   - Interruption tests cut a flow at every step and check that the next run converges.
   - Runs on every PR.
   - **Built in M1** for `update cluster` and `delete cluster`, without Nomad (`internal/app/integration_test.go`,
     `internal/app/interrupt_test.go`). They run the use cases with the real Vultr provider on `vultrfake` and a
     `file://` state store, in `testing/synctest` bubbles, on the cluster of [3.1](#31-kinds) with 2 workers.
     - Golden files (`internal/app/testdata/flow_*.golden`) hold the plans and the calls that reach Vultr, for the
       build on an empty cloud, the scale from 2 to 3 workers and the delete. Operation ids show as `<op1>`,
       `<op2>` and so on. The engine runs changes in parallel, so the calls of one engine run come grouped by object
       in the order of the plan.
     - Cuts: the context of a run ends at each call of an uninterrupted build or delete, once just before the call,
       which then reaches nothing, and once just after the fake carried it out, when the call loses its answer, as a
       request in flight does. The cut run leaves the state store as it was and the lock free. After the next run,
       a build leaves the cloud as an uninterrupted build does, with one copy of every object, and a plan after it
       has no changes; a delete leaves no object with the cluster's markers and no state.
     - Lost answers: for each create call of the build, the fake carries the call out and the call gets no answer.
       The same run finds what the call made by its operation id, or lists the rules again, and ends with one copy
       of every object and one instance per node.
     - The fake's hook (`SetHook`) makes the cuts and the lost answers.
4. **tent-node tests.** Phases run with an abstracted filesystem and exec. Occasionally they run in a
   systemd-enabled container or a VM.
5. **E2E on Vultr** (`//go:build e2e`, black box):
   - Region `ams`, falling back to `fra` or `lhr`.
   - Scenarios: `smoke`, `ha`, `upgrade` and `security`.
   - A dedicated account or IAM service user, with limits raised before CI is wired.
   - Runs are serialized, and each stays under 60 minutes because billing has a one-hour minimum.
   - About $0.05–0.09 per 5-VM run.
   - A janitor deletes everything tagged `tent/e2e` that is older than 3 hours.
   - Nightly, and on a PR label.
   - Hetzner E2E, including the `arm` scenario on CAX, is added in M4 once an account can create servers reliably.
6. **Platform spike.** [`hack/vultr-spike`](../hack/vultr-spike/README.md) verifies undocumented Vultr behaviour
   before the provider relies on it. It runs manually and its report goes into the platform notes.
7. **Build config tests.** `internal/buildconfig` reads the CI and release workflows, the Makefile and
   `.goreleaser.yaml`, and fails when they disagree, for example on the golangci-lint version or the test command
   ([ADR-0020](adr/0020-release-channels-and-ci-conventions.md)). They run with the unit tests.

---

## 16. Technology stack and releases

See [ADR-0013](adr/0013-technology-stack.md). Releases and CI follow
[ADR-0020](adr/0020-release-channels-and-ci-conventions.md).

- **Go.** `go 1.26` or newer in `go.mod`: `github.com/hashicorp/nomad/api` requires it. hcloud-go needs 1.25+, and
  govultr 1.23+.
- **Libraries:**
  - `spf13/cobra`;
  - `vultr/govultr/v3`, pinned at v3.33.0, with its retries off: tent's own transport retries idempotent calls and
    limits the rate ([11.8](#118-api-client-rate-limits-cost));
  - `hetznercloud/hcloud-go/v2`;
  - `hashicorp/nomad/api` (pinned by pseudo-version);
  - `aws-sdk-go-v2` (`config`, `service/s3`) and `aws/smithy-go` for the s3 state store;
  - `gofrs/flock` for the file store's locks (`flock` on Unix, `LockFileEx` on Windows);
  - `golang.org/x/mod/semver` for the version guard, pinned at v0.40.0 because v0.41.0 declares `go 1.26.0` and would
    rewrite `go.mod`;
  - `sigs.k8s.io/yaml`;
  - `go.yaml.in/yaml/v3` and `sigs.k8s.io/json` for spec files ([ADR-0022](adr/0022-json-schema-from-go-types.md));
  - `invopop/jsonschema`, in the schema generator only;
  - `google/licenseclassifier/v2`, in the licence check only;
  - `ProtonMail/go-crypto`;
  - `golang.org/x/sync/errgroup`;
  - `log/slog`;
  - tests: `google/go-cmp`, and `santhosh-tekuri/jsonschema/v6` to check examples against the schema.
- **Quality gates:**
  - golangci-lint with the depguard layer rules; `go vet` runs as its govet linter;
  - `go test -race` on Linux, macOS and Windows with the Go from `go.mod`, which builds the release, and on Linux with
    the newest Go too;
  - the s3 state store's tests against a Cloudflare R2 bucket, skipped where its secrets are missing, as in forks.
    The job needs the repository variable `TENT_TEST_S3_URL` (an `s3://tent-ci/ci?…` URL, prefix `ci`), the secrets
    `R2_ACCESS_KEY_ID` and `R2_SECRET_ACCESS_KEY` (an R2 API token limited to the bucket, Object Read & Write), and a
    lifecycle rule on the bucket that expires objects under `ci/` after 1 day, for runs cancelled before cleanup;
  - govulncheck, also weekly;
  - the licences of every module tent links, on each platform the release builds for: each must be Apache-2.0,
    BSD-2-Clause, BSD-3-Clause, ISC, MIT or MPL-2.0, and every licence file other than a NOTICE must name one. A
    licence file the classifier cannot name, such as BUSL-1.1 or a proprietary text, fails even when the module has
    another, allowed licence (`make licenses`, in the lint job);
  - a release snapshot on every pull request;
  - `make check` runs fmt, lint, licenses, test and build locally;
  - Renovate.
- **Releases** use GoReleaser on `v*` tags:
  - `tent` for linux, darwin and windows on amd64 and arm64: archives, a Homebrew cask with signed and notarized
    macOS binaries, and deb and rpm packages;
  - every archive and package carries `LICENSE` and `THIRD_PARTY_NOTICES`. A GoReleaser hook writes the notices
    before the build, as `make notices` does: the licence and notice files of the Go standard library and of every
    module tent links, with each module's version and where to download its source;
  - `tent-node` for linux on amd64 and arm64, as raw binaries, from M2;
  - `checksums.txt` with a keyless cosign signature, and an SBOM per archive;
  - tent-node's version always equals the CLI's;
  - nothing is released while the repository is private.

---

## 17. Risks

| Risk | Impact | Mitigation |
|---|---|---|
| Opaque Vultr new-account limits (possibly tiny; perhaps a per-day creation cap) | E2E cannot create its VMs | request increases before wiring CI; surface limit errors verbatim; the maintainer's account ran 3 instances at once on 2026-09-25 |
| Recurring Vultr deploy/API incidents | flaky E2E and rollouts | idempotent retries, fallback region, tag-based janitor |
| Vultr single failure domain, no anti-affinity | a data-center outage takes the whole cluster down | documented; `validate` warns; multi-region federation later |
| Vultr API churn (VPC 2.0 removed in 2026; the Terraform provider broke) | runtime breakage | pin govultr, Renovate, nightly E2E |
| Undocumented Vultr behaviour (user_data limit, tag syntax, firewall scope, halt semantics) | wrong assumptions in code | `hack/vultr-spike` settled all of these on 2026-09-25 except Object Storage conditional writes; re-run it when Vultr changes something relevant |
| Hetzner capacity and account limits (5 servers, creation restrictions since June 2026) | Hetzner provider cannot be E2E-tested | Vultr first (ADR-0014); Hetzner E2E in M4 |
| Nomad 2.x version skew rules not yet restated | broken upgrades | channels allow only tested version pairs; servers before clients |
| BUSL licence of Nomad | a paid managed offering would need a commercial licence | tent downloads official binaries and never redistributes them; stays free (not legal advice) |
| Secrets in user data | node impersonation if metadata leaks | mitigations in [9.4](#94-secrets-on-nodes-threat-model), including scrubbing on Vultr; bootstrap controller in v2 |
| Hetzner rate limit (3600/h per project) | slow or failing large rollouts | snapshots, batched waits, adaptive throttling, targeted rollouts, one project per cluster |

---

## 18. Open questions

Questions that only the maintainer can decide go here. The six initial questions were decided on 2026-09-25:

1. **Label prefix and API group:** `tent/…` labels and `apiVersion: tent/v1alpha1`
   ([ADR-0003](adr/0003-cloud-is-source-of-truth.md)). This is effectively permanent, because existing clusters are
   found by their ownership markers.
2. **Default API exposure:** `access.api` defaults to `[0.0.0.0/0]` with mTLS + ACL, as in kops, and tent warns loudly
   while it is open ([ADR-0007](adr/0007-security-baseline.md)).
3. **Combined server+client mode:** allowed for dev and small clusters through an explicit `combined` role
   ([ADR-0019](adr/0019-combined-server-client-role.md)).
4. **Default OS image:** `ubuntu-24.04`. E2E also runs on `ubuntu-26.04`.
5. **Consul and Vault:** out of v1 ([ADR-0011](adr/0011-nomad-only-scope-and-licensing.md)).
6. **Licence of tent:** Apache-2.0, like kops, and compatible with MPL-2.0 dependencies.

Decided on 2026-09-28:

7. **A cluster's cloud:** its `cloud.provider` and `cloud.region` never change. A cluster moves by creating a new one
   ([3.3](#33-api-rules)).
8. **CA validity:** 10 years, until CA rotation exists ([9.1](#91-pki)).

---

## Appendix A: Nomad agent configuration sketches

These are illustrative. Once the golden files exist in the repository, they are authoritative.

**Server: `/etc/nomad.d/00-tent.hcl`** (group level, part of the spec hash):

```hcl
# Rendered by tent. Do not edit: changes are overwritten on the next boot.
region             = "global"
data_dir           = "/var/lib/nomad"
leave_on_terminate = true # graceful leave where the provider can shut down via ACPI

addresses {
  http = "0.0.0.0" # reached by the tent CLI through the cloud firewall (access.api)
  rpc  = "{{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"address\" }}"
  serf = "{{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"address\" }}"
}

advertise {
  http = "{{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"address\" }}"
  rpc  = "{{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"address\" }}"
  serf = "{{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"address\" }}"
}

server {
  enabled          = true
  bootstrap_expect = 3
  encrypt          = "<gossip key>"

  client_introduction {
    enforcement = "strict"
  }
}

acl {
  enabled = true
}

tls {
  http = true
  rpc  = true

  ca_file   = "/etc/nomad.d/tls/ca.pem"
  cert_file = "/etc/nomad.d/tls/agent.pem"
  key_file  = "/etc/nomad.d/tls/agent-key.pem"

  verify_server_hostname = true
  verify_https_client    = true
}

autopilot {
  cleanup_dead_servers = true
}

telemetry {
  prometheus_metrics   = true
  publish_node_metrics = true
}
```

**Client: `/etc/nomad.d/00-tent.hcl`** (group level):

```hcl
# Rendered by tent. Do not edit: changes are overwritten on the next boot.
region             = "global"
data_dir           = "/var/lib/nomad"
leave_on_terminate = true # also triggers drain_on_shutdown as a safety net

addresses {
  http = "127.0.0.1 {{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"address\" }}"
  rpc  = "{{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"address\" }}"
}

advertise {
  http = "{{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"address\" }}"
  rpc  = "{{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"address\" }}"
}

client {
  enabled           = true
  node_pool         = "default"
  node_class        = "general"
  network_interface = "{{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"name\" }}"

  drain_on_shutdown {
    deadline           = "10m"
    ignore_system_jobs = true
  }

  options {
    # Cloud environment fingerprinters only slow down startup on Vultr and Hetzner.
    "fingerprint.denylist" = "env_aws,env_gce,env_azure,env_digitalocean"
  }

  meta {
    tent_cluster   = "prod"
    tent_nodegroup = "workers"
    team           = "platform"
  }
}

tls {
  http = true
  rpc  = true

  ca_file   = "/etc/nomad.d/tls/ca.pem"
  cert_file = "/etc/nomad.d/tls/agent.pem"
  key_file  = "/etc/nomad.d/tls/agent-key.pem"

  verify_server_hostname = true
  verify_https_client    = true
}

telemetry {
  prometheus_metrics   = true
  publish_node_metrics = true
}
```

**Join list: `/etc/nomad.d/05-join.hcl`** (per node, maintained by tent-node, excluded from the spec hash):

```hcl
# Maintained by tent-node (join refresh). Do not edit.
# Servers use the serf port (4648), clients the RPC port (4647).
client {
  server_join {
    retry_join = ["10.64.0.5:4647", "10.64.0.9:4647", "10.64.0.12:4647"]
  }
}
```

On servers the same file sets `server { server_join { retry_join = [...] } }`. On Hetzner it lists the seven slot IPs.

**Per node: `/etc/nomad.d/10-node.hcl`** (excluded from the spec hash):

```hcl
# Rendered by tent for this node only.
name       = "prod-workers-3"
datacenter = "ams"

client {
  meta {
    tent_instance_id = "cb676a46-66fd-4dfb-b839-443f2e6c0b60"
  }
}
```

---

## Appendix B: cloud-init user data sketch

```yaml
#cloud-config
package_update: false    # OS patching happens by node replacement
package_upgrade: false   # Vultr's vendor data sets this too (2026-09); kept explicit in case it changes
write_files:
  - path: /etc/tent/node.json
    encoding: gz+b64
    owner: root:root
    permissions: "0600"
    content: H4sIAAAAAAAA...   # gzip+base64 NodeConfig
runcmd:
  - - /bin/sh
    - -c
    - |
      set -eu
      for url in "https://github.com/ingvarch/tent/releases/download/v0.1.0/tent-node_linux_amd64" "<mirror>"; do
        curl -fsSL --retry 5 -o /usr/local/bin/tent-node "$url" && break
      done
      echo "<sha256>  /usr/local/bin/tent-node" | sha256sum -c -
      chmod 0755 /usr/local/bin/tent-node
      /usr/local/bin/tent-node install --config /etc/tent/node.json
```

`tent-node install` writes and starts two units:

- **`/etc/systemd/system/tent-node.service`:** `Type=oneshot`, `RemainAfterExit=yes`, ordered
  `Before=nomad.service`, runs `tent-node up`.
- **`tent-node-join.timer`:** runs `tent-node refresh-join` every 60 seconds.

On Vultr, tent replaces this user data with a non-secret stub once the node has registered (§9.4).

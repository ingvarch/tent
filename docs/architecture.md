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
  channel: stable                # Nomad and CNI versions (see 13.5)
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
    version: 2.0.7               # empty means the version the cluster was first built with (see 13.2)
    region: global
    tls: {verifyHTTPSClient: true}
    clientIntroduction: strict   # strict | warn | none
    extraConfig:                 # escape hatch, rendered into 98-user-server.hcl and 99-user-client.hcl
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
| Node identity | — | mTLS plus intro token | none (secrets via user data, scrubbed once the node has joined) | none (secrets via user data) | instance identity document plus IAM role |
| State store | `--state` | — | Object Storage (S3 API) or any S3 | Object Storage (S3 API) or any S3 | S3 |

### 3.3 API rules

- **API version.** User-facing kinds use `apiVersion: tent/v1alpha1` (group decided on 2026-09-25).
  Every stored object carries its apiVersion. Conversion functions are added when a second version appears.
- **User spec and completed spec.** The user spec is what the operator wrote. The state store keeps it as given,
  without defaults, but written in tent's field order, so comments and formatting are not kept. The completed spec
  has every default filled in and records what was last applied, the Nomad version included; `update` writes it from
  M1 on ([13.2](#132-tent-update-cluster---yes)). `tent get --full` does not read it yet: it prints the user spec
  with the defaults filled in.
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
- **Channel and Nomad version** (M2.2, [ADR-0026](adr/0026-channels-and-release-assets.md)). `spec.channel` names a
  channel embedded in tent, `stable` by default ([13.5](#135-tent-upgrade-cluster---yes)). `spec.nomad.version` is
  optional, and the channel must allow it: a release `X.Y.Z` from the channel's minimum up to, not including, the next
  major version.
  - `api/v1alpha1` checks only that the channel is a name, and no longer checks the version's form: the channel says
    which versions are valid. The app checks the channel and the version wherever it checks specs: `create` (with
    `--dry-run` too, also without a state store), `replace`, `edit` and `update`.
  - The problems are field errors, the channel's first:

    ```
    Cluster prod: spec.channel: unknown channel "x"; known: stable
    Cluster prod: spec.nomad.version: "2.0" is not a Nomad version such as 2.0.7
    Cluster prod: spec.nomad.version: 1.11.0 is older than 2.0.0, the oldest Nomad that channel stable allows
    Cluster prod: spec.nomad.version: 3.0.0 is newer than this tent knows; channel stable allows 2.x from 2.0.0
    ```

    The example in the second line is the channel's recommended version. An unknown channel leaves the version
    unchecked.
  - A version that the channel allows but has not tested passes with a warning ([14](#14-cli)).
  - A spec without a version runs the version pinned in the completed spec, which the first `update` takes from the
    channel ([13.2](#132-tent-update-cluster---yes)).
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
- **Client settings.** tent writes a group's `nomad` settings into the Nomad agent configuration, so it refuses values
  that Nomad would misread ([ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md)):
  - a meta key is one or more words of letters, digits, `_` and `-`, joined by dots
    (`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`), and must not start with `tent_`, which tent keeps for `tent_cluster`,
    `tent_nodegroup` and `tent_instance_id`;
  - a meta value must not hold a control character, U+E123 or `${` ([8.4](#84-nomad-configuration-rendering));
  - `nodePool` must not be `all`, Nomad's built-in pool of every node, which no client joins.
- **Escape hatch.** `nomad.extraConfig` is added as given: `server` as `98-user-server.hcl` on server and combined
  nodes, `client` as `99-user-client.hcl` on client and combined nodes. Nomad merges configuration files in the order
  of their names, so the operator's settings win over tent's ([8.4](#84-nomad-configuration-rendering)). This is
  documented as unsupported.
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
| `tent/joined` | `true` | machines whose node has joined its cluster, set together with the scrub of their user data ([7.1](#71-interfaces), [9.4](#94-secrets-on-nodes-threat-model), [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)) |
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
│   statestore ◄───┼──► model (spec → intents) ──► nodeconfig ◄── pki        │
│   (spec, PKI,    │         │                                                │
│    lock)         │         ▼                                                │
│                  │   cloud.Provider ──► engine (plan/apply DAG)             │
│                  │   ├─ vultr   (first)                                     │
│                  │   └─ hetzner (second; aws later)                         │
│                  └──► rollout ──► nomadops (ACL, raft, drain, proxy)       │
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
3. **Nomad configuration**: agent config, TLS material, ACL bootstrap, node pool membership and the rest of day-1
   setup. It does not depend on the cloud and lives in the core. tent renders the agent configuration into
   NodeConfig (`internal/nodeconfig`), and tent-node writes it on the node ([8.4](#84-nomad-configuration-rendering)).

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
│   └── tent-node/       # node agent: install, up, refresh-join, version (standard flag package)
├── internal/
│   ├── cli/             # cobra commands, flags, output (table|yaml|json): a thin layer
│   ├── spec/            # multi-document YAML specs: strict decoding with file lines, encoding in field order
│   ├── apischema/       # generates api/v1alpha1/tent.schema.json (make generate); not linked into tent
│   ├── licenses/        # licence check and THIRD_PARTY_NOTICES (make licenses, make notices); not linked into tent
│   ├── app/             # use cases, validate and operator access included; used by the CLI, e2e tests and a
│   │                    # future controller
│   ├── model/           # spec -> cloud-agnostic intents (network, access, rules between nodes, join, groups)
│   ├── engine/          # task graph: plan/apply, diff rendering, retries, concurrency
│   │   └── enginetest/  # ApplyReplan for provider task tests: apply, plan again, expect no changes
│   ├── cloud/           # Provider / Nodes interfaces, capabilities, registry, common types
│   │   ├── vultr/       # govultr wrapper, label codec, tasks, nodes, inventory, pricing
│   │   │   └── vultrfake/ # in-memory fake of vultr.API for provider and core tests
│   │   └── hetzner/     # hcloud-go wrapper, tasks, nodes, inventory, pricing, lock
│   ├── nodeconfig/      # tent <-> tent-node contract: NodeConfig, Nomad config rendering, nomad.service, spec hash,
│   │                    # user data
│   ├── nodeup/          # tent-node: phase runner over FS and exec, phases, systemd units, install; status.json;
│   │   │                # the host firewall, Docker, the CNI plugins and the asset cache; Nomad: join, install,
│   │   │                # start, health over mTLS; refresh-join and its lock with up
│   │   ├── env/         # Environment: a cloud's metadata service, read once per run
│   │   │   └── vultr/   # Vultr's /v1.json (hetzner/ in M4, IMDSv2 for AWS later)
│   │   ├── retry/       # what tent-node's retries share: the answers worth another try, a sleep that ctx cancels
│   │   └── nodeuptest/  # tests only: in-memory FS, scripted runner, fake Ubuntu with systemd, nft, apt and ufw;
│   │                    # tars and zips, HTTPS and mTLS servers
│   ├── nomadops/        # the ONLY importer of github.com/hashicorp/nomad/api: mTLS client, ACL bootstrap and tokens,
│   │                    # waits, the proxy of tent ui
│   │   └── nomadfake/   # in-memory Nomad cluster behind nomadops.API, for the app's tests
│   ├── rollout/         # scale up/down, rolling update, server quorum safety
│   ├── pki/             # CA, node and operator certificates, gossip key, ACL bootstrap secret
│   ├── secret/          # the Secret type of keys and tokens, which never prints
│   ├── uuid/            # random lower-case UUIDs of version 4: operation ids, the ACL bootstrap secret
│   ├── english/         # lists as English sentences write them, "a, b and c", for messages
│   ├── secrettest/      # tests only: looks for a secret in what tent prints or logs
│   ├── shellenv/        # env lines for fish and sh, for the tools whose output a shell runs
│   │   └── shellenvtest/ # tests only: runs the lines in a real sh and fish
│   ├── statestore/      # Store interface, file:// and s3://, layout, locking
│   ├── s3url/           # s3:// URLs of a bucket and prefix, and their S3 clients: the state store, dev uploads
│   │   └── s3urltest/   # tests only: keeps the developer's AWS configuration out of a test
│   ├── assets/          # Nomad, CNI and tent-node: URLs and sha256s; checks Nomad's signature; the online test of
│   │   │                # nomad config validate on the goldens of nodeconfig
│   │   └── assetstest/  # tests only: Nomad's signed SHA256SUMS and tent's checksums, no network
│   ├── channels/        # embedded channel files: the Nomad versions allowed and tested, the CNI plugins
│   ├── buildinfo/       # version, commit, date (ldflags); which release a version counts as
│   └── buildconfig/     # tests only: CI, Makefile and release config agree; what tent-node links
├── test/e2e/            # //go:build e2e: black-box tests against real clouds (Vultr first)
├── hack/                # tent-node-upload/ (dev builds), tent-node-userdata/ (VM check), vultr-spike/, janitor
└── docs/                # this document, ADRs, platform notes, roadmap
```

The node planner of M1, which scales node groups without Nomad ([13.4](#134-scaling)), is in `internal/app`. It moves
to `internal/rollout` with the drain and the quorum checks
([ADR-0005](adr/0005-immutable-nodes-and-nomad-aware-rollouts.md)). `validate cluster` is in `internal/app` as well,
since it reuses that planner ([13.6](#136-tent-validate-cluster---wait-duration),
[ADR-0033](adr/0033-operator-commands.md)).

`depguard` in golangci-lint enforces the dependency rules ([ADR-0021](adr/0021-import-rules.md)):

- `api/...` imports only the standard library and other `api/` packages, so third parties can use the types. Its
  tests are exempt.
- Only `cmd/tent` imports provider packages (`internal/cloud/<provider>`), to register them. Every other package,
  the core (`internal/model`, `internal/rollout`, `internal/app`) included, uses only the interfaces in
  `internal/cloud`, so no package reaches a provider through another one. Tests are exempt, and so is code under
  `internal/cloud/<provider>/`, so a provider can have subpackages.
- Cloud SDKs (govultr, hcloud-go) are imported only by their provider's packages, tests included.
- `internal/nodeup`, `internal/nodeconfig` and `cmd/tent-node`, tests included, never import `internal/cloud/...`
  (`nodeup-no-cloud`). No code that can create or delete cloud resources ever runs on a node.
- `internal/nodeup` and `cmd/tent-node` import only the standard library, `internal/nodeup` and its subpackages,
  `internal/nodeconfig`, `internal/secret`, `internal/buildinfo` and `api/v1alpha1` (`tent-node-allowed`). Their tests
  are exempt ([ADR-0028](adr/0028-tent-node-agent-units-and-delivery.md)).
- depguard checks each file's direct imports. A test in `internal/buildconfig` checks everything tent-node links, on
  linux/amd64 and linux/arm64, with `go list -deps ./cmd/tent-node`: no `internal/cloud`, `internal/assets`,
  `internal/channels`, `internal/statestore`, `internal/app` or `internal/nomadops`, no cloud SDK, no go-crypto, no
  Nomad module and no cobra, and no module besides tent's own and `golang.org/x/mod`.
- Only `internal/nomadops` imports `github.com/hashicorp/nomad/api`, tests included (`nomad-only-in-nomadops`). The
  root module `github.com/hashicorp/nomad` is BUSL-licensed and must never be imported: inside `internal/nomadops`
  only the API module is allowed (`nomadops-api-only`). Throw-away files proved both rules on 2026-09-29 (#92): an
  import of the API module in `internal/app` code, in an `internal/app` test and in `cmd/tent`, and an import of the
  root module in `internal/nomadops`, gave four findings; the API module in `internal/nomadops` gave none.
- Since M2.7a `internal/app`, `internal/cli` and `cmd/tent` import `internal/nomadops` in non-test code, so the `tent`
  binary links `nomad/api`. `internal/cli` also imports `internal/assets` and `internal/channels` for its options, and
  `internal/shellenv` for the lines of `export nomad`. `internal/cli` has the options `WithProviders`, `WithAssets`,
  `WithNomad` and, since M2.8, `WithNomadProxy`, which gives `tent ui` the handler of `nomadops.NewProxy`; the channel
  list has no option: the tests of `internal/cli` set the unexported `channels` field of its options, which goes to
  `Service.Channels`.
- `internal/nomadops/nomadfake`, tests included, imports no Nomad module: it stands in for Nomad with the types of
  `internal/nomadops` alone (`nomadfake-no-nomad`). It imports `testing`, so only tests import it.
- `internal/pki` imports only the standard library, `internal/uuid`, `internal/secret` and `api/v1alpha1`, so the
  code that makes the CA and the secrets never reaches a cloud or the state store. `internal/uuid`, `internal/secret`,
  `internal/english`, `internal/secrettest` and `internal/assets/assetstest` import only the standard library
  (`assetstest-stdlib-only`, so that the tests of `internal/assets` can import it). The tests of these six packages are
  exempt ([ADR-0025](adr/0025-stdlib-only-helper-packages.md),
  [ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md),
  [ADR-0031](adr/0031-bootstrap-in-update.md)). Only tests import `internal/secrettest`,
  `internal/nomadops/nomadfake`, `internal/nodeup/nodeuptest`, `internal/s3url/s3urltest`,
  `internal/assets/assetstest` and `internal/shellenv/shellenvtest`.
- `internal/nodeconfig`, the contract that tent-node decodes, imports only the standard library, `internal/secret`
  and `api/v1alpha1`. Its tests are exempt ([ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md)).
- Only tests import `github.com/hashicorp/hcl`: they parse the rendered Nomad configuration back with HCL1. No code
  that tent runs parses HCL.
- Only `internal/assets` imports `github.com/ProtonMail/go-crypto`, tests included. `internal/nodeup`,
  `internal/nodeconfig` and `cmd/tent-node`, tests included, import neither `internal/assets` nor
  `internal/channels`: tent-node gets its versions and sha256s in NodeConfig and carries no PGP code
  ([ADR-0026](adr/0026-channels-and-release-assets.md)).
- `internal/channels` imports only the standard library, the decoder of the specs (`sigs.k8s.io/yaml` and
  `sigs.k8s.io/json`) and `golang.org/x/mod/semver`. Its tests are exempt.
- `internal/s3url` imports only the standard library, aws-sdk-go-v2's `aws`, `config` and `service/s3`, and
  smithy-go's `logging` (`s3url-stdlib-and-aws`). Its tests are exempt
  ([ADR-0028](adr/0028-tent-node-agent-units-and-delivery.md)).
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
	// Arch returns the CPU architecture of the machines of a machine type, cloud.ArchAMD64 or cloud.ArchARM64.
	Arch(ctx context.Context, machineType string) (string, error)
}

// Nodes are the machine primitives of a provider. Drain, quorum and the order of replacements live in the core.
type Nodes interface {
	List(ctx context.Context, cluster string) ([]Instance, error)
	Create(ctx context.Context, req CreateRequest) (Instance, error) // idempotent per req.Op; waits until ready
	Stop(ctx context.Context, node Instance) error                  // hard where there is no graceful shutdown
	Delete(ctx context.Context, node Instance) error                // even while the machine runs
	MarkJoined(ctx context.Context, node Instance) error            // labels the machine, scrubs its user data
}

// Instance is one machine of a cluster as the cloud reports it.
type Instance struct {
	ID, Name, Cluster, Group string        // Name is <cluster>-<group>-<index>, also the hostname
	Role                     v1alpha1.Role // the Nomad role of its node group
	Zone, SpecHash, Op       string        // SpecHash is empty when the machine carries none
	PrivateIP, PublicIP      netip.Addr    // the invalid Addr until the cloud reports one
	Ready, Joined            bool          // Joined: it carries the label tent/joined=true
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
- `MarkJoined` sets the label `tent/joined=true` on the machine and, where the cloud lets user data change, replaces
  the user data with a stub that holds no secrets. The core calls it once the node has joined, on every cloud. Both
  changes go in one request on Vultr ([11.6](#116-user_data)). It is safe to repeat.
  [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md) has the reasons. `Instance.Joined` reports the label.
- A machine that is gone counts as stopped, deleted or marked as joined.
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
- `PackUserData` only with a provider that needs another envelope than the cloud-config that `nodeconfig.UserData`
  makes for every provider ([8.3](#83-nodeconfig-contract));
- `Locker` with the Hetzner lock firewall;
- `Capabilities` with the first core code that depends on one;
- `Default` with the first provider default that `v1alpha1.SetDefaults` does not fill in.

`Nodes` changes with them: `Stop` is graceful where `GracefulShutdown` is set, the provider's `MarkJoined` replaces
the user data only where `MutableUserData` is set (the core calls it on every cloud, and a cloud with immutable user
data sets the label alone), and `CreateRequest` gets a fixed private IP with the Hetzner provider.

The sketch of the target:

```go
type Provider interface {
	// Name, Validate, BuildInfra, InfraKinds, Inventory, Nodes and Arch as above, and:
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
	MutableUserData       bool // Vultr: PATCH user_data -> scrub secrets once the node has joined
	MaxUserDataBytes      int  // only below tent's 24 KiB budget, such as AWS's 16 KB (8.3)
	CostEstimates         bool // Hetzner /pricing, Vultr /plans
}
```

On the node side, `tent-node` reads its cloud's metadata service through `env.Environment` in `internal/nodeup/env`
(M2.5, [ADR-0028](adr/0028-tent-node-agent-units-and-delivery.md)):

```go
// Environment is a cloud's metadata service, as the machine sees it.
type Environment interface {
	// Read asks the metadata service about the machine. Each call reads the service anew, so tent-node calls it once
	// per run.
	Read(ctx context.Context) (Instance, error)
}

// Instance is the machine as its cloud describes it.
type Instance struct {
	ID        string     // the cloud's id of the machine, as its API names it; into 11-instance.hcl
	Zone      string     // in lower case, such as ams (Vultr reports AMS)
	PrivateIP netip.Addr // the IPv4 address on the cluster's private network
}
```

- **One snapshot per run.** `preflight` reads the service once and keeps the result for the other phases and
  `status.json` ([8.2](#82-tent-node-phases)). There is no user data method: cloud-init has written `node.json`
  already. There is no metadata address either: NodeConfig's `firewall.blockMetadata` is the one source of it.
- **Chosen by NodeConfig's `provider`** (decision 20 of [18](#18-open-questions)). A provider without an environment
  fails `up` with `tent-node does not support provider <name> yet`.
- **Vultr** (`env/vultr`): `GET http://169.254.169.254/v1.json`, never through a proxy and without redirects, 5 s per
  try with the body, 1 s between tries, until the context ends; `preflight` gives it 3 minutes. It tries again after
  a failed connection, a try without a whole answer, a 429 or a 5xx other than 501, as the API client does
  ([11.8](#118-api-client-rate-limits-cost)). Any other answer fails at once, 404 included: cloud-init read the
  document earlier in the same boot. It takes `instance-v2-id`, `region.regioncode` in lower case and the IPv4 address
  of exactly one interface with `network-type: private`; none, two, an empty address or `0.0.0.0` fails. No error
  holds the document, which carries the user data.
- **The mark** (decision 21 of [18](#18-open-questions)). On Linux the client sets `SO_MARK` to `env.MetadataMark`
  (`0x747`) on each socket, and the host firewall lets only packets with that mark reach the metadata service
  ([9.4](#94-secrets-on-nodes-threat-model)). Setting it needs CAP_NET_ADMIN or CAP_NET_RAW; a failed mark fails the
  try.
- **Hetzner** (`http://169.254.169.254/hetzner/v1/...`) comes with the Hetzner provider, and IMDSv2 for AWS later.

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
- **Rules between nodes** (`Intra`, M2.3). Vultr and Hetzner firewalls do not filter private traffic, so their
  providers map only the access rules, and the host firewall of each node enforces these
  ([8.3](#83-nodeconfig-contract)). All come from the cluster CIDR, in this order:
  - `nomad-http`: 4646/tcp to every node;
  - `nomad-rpc`: 4647/tcp to the servers. Clients do not listen on it. A combined node's client reaches its own server
    through it, over the private address ([platform notes §1.2](platform-notes.md#12-features-tent-relies-on));
  - `serf`: 4648/tcp and 4648/udp to the servers;
  - `dynamic`: 20000–32000/tcp and /udp to the clients, the nodes of the client and combined groups, which run the
    workloads. This is Nomad's default range of dynamic ports, and tent writes it into the agent configuration too
    (`model.DynamicPorts`).
- **Join strategy** (`Join`, M2.3): `seed-and-refresh` ([ADR-0016](adr/0016-server-discovery-seed-and-refresh.md)).
- **Targets.** A rule opens all nodes, the servers or the clients. `Target.Includes(role)` says which roles a rule
  reaches; the Vultr firewall groups and the host firewall both use it.
- **Node groups**, sorted by name: role, machine type, image by name, zones and size.

The model holds no Nomad settings. `internal/app` builds each group's NodeConfig from the model and the completed specs
([8.3](#83-nodeconfig-contract), [ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md)).

**Target, not built yet.**
- **Join strategies of other providers:** fixed slots on Hetzner and cloud auto-join tags on AWS. AWS will also map the
  rules between nodes to Security Groups.
- **Named subnets** where the provider supports fixed IPs: on Hetzner `control` for fixed addresses and `nodes` for
  automatic ones.
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
| Node credentials | user data, scrubbed once the node has joined | user data | IAM role / bootstrap controller | credential delivery strategy ([9.5](#95-target-architecture-bootstrap-controller)) |
| State store | Vultr Object Storage / any S3 | Hetzner Object Storage / any S3 | S3 | the same `s3://` backend |
| Metadata | `/v1.json` | `/hetzner/v1/` | IMDSv2 | `nodeup/env.Environment` |

---

## 8. Nodes: tent-node and NodeConfig

See [ADR-0006](adr/0006-two-binaries-and-nodeconfig.md).

### 8.1 Bootstrap chain

```
cloud-init (user_data: minimal cloud-config; vendor package upgrades disabled)
  ├─ write /etc/tent/node.json            # NodeConfig, gzip+base64 in user data, mode 0600
  ├─ download tent-node: each URL in turn, until a file has the sha256
  └─ exec tent-node install               # in cloud-final: writes and enables tent-node.service (oneshot, every
        │                                 # boot) and tent-node-join.timer (join refresh, every 60 s), then starts
        │                                 # the service and waits for it
        └─ tent-node.service: tent-node up   # idempotent phases, see below
              └─ systemctl start nomad.service   # after the host firewall; never enabled for boot
```

- **No operator, no SSH.** The node bootstraps itself, without the operator being online and without SSH access. This
  is a precondition for future autoscaling and automatic replacement. SSH is used only for diagnostics
  (`tent toolbox dump`).
- **No vendor package upgrades.** The cloud-config disables package update and upgrade. Vultr's vendor data has set the
  same since at least 2026-09, but it may change, so tent sets it anyway. OS patching happens by replacing nodes.
- **The user data** is built in M2.3 (`nodeconfig.UserData`, [Appendix B](#appendix-b-cloud-init-user-data-sketch)).
  `update` gives it to every node since M2.7a ([13.2](#132-tent-update-cluster---yes)).
- **The handover** (decision 19 of [18](#18-open-questions),
  [ADR-0028](adr/0028-tent-node-agent-units-and-delivery.md)).
  - **First boot.** cloud-final runs `tent-node install` through `exec`. `install` starts `tent-node.service` and
    waits until `up` has run, so cloud-init finishes after `up`, and fails when `up` fails. Then it starts the timer.
  - **Every later boot.** `tent-node.service` starts with `multi-user.target`, which waits until `up` has run.
    cloud-final is ordered after `multi-user.target`, so it waits too. `runcmd` does not run again.
  - **No ordering that could hang.** No unit is ordered on `cloud-final.service`, `cloud-init.target`,
    `cloud-config.service` or `cloud-init-main.service`: the first two would wait for the `install` that waits for
    them, and the other two are left out as a precaution (on Ubuntu 26.04 `cloud-init-main.service` runs every stage of
    cloud-init, [ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md)). None is ordered after
    `multi-user.target`, which `WantedBy=` orders after the service, nor before `nomad.service`, which `up` starts
    from inside the service.
  - **Finite timeouts:** 45 minutes for `up`, 5 for a join refresh.
- **Nomad** ([ADR-0030](adr/0030-nomad-on-nodes.md)). `nomad.service` has no `[Install]` section, so it is never
  enabled: at every boot `up` starts it in the `nomad` phase, after `hostfirewall` has loaded tent's table, so no
  workload runs before tent's rules.
- **Commands.** `tent-node install`, `up` and `refresh-join` take `--config`, `/etc/tent/node.json` by default.
  `tent-node version` prints the version. The exit code is 0 on success, 1 when the command fails and 2 for a wrong
  command line. tent-node logs to stderr, which systemd puts into the journal.
- **The lock.** `up` and `refresh-join` take an exclusive `flock` on `/run/tent-node.lock`, so they never run at once.
  `up` tries again every second while another run holds it, until its context ends; `refresh-join` gives up after
  4 minutes and exits 0 ([8.2](#82-tent-node-phases)). At boot the timer's job waits in systemd's queue behind
  `tent-node.service`, so at boot only runs by hand wait for the lock.

### 8.2 tent-node phases

`tent-node up` runs as a systemd oneshot on every boot, and every boot runs every phase. Each phase compares the machine
with NodeConfig and acts only on a difference, so a second run changes nothing but `status.json`. Nomad is restarted
only when its binary, a NodeConfig file (`nomad.service` among them) or `11-instance.hcl` changed; `05-join.hcl`
never restarts it. `up` runs the phases in the order of the table and stops at the first that fails
([ADR-0028](adr/0028-tent-node-agent-units-and-delivery.md)).

- **Results.** A phase ends `done` (it changed the machine), `unchanged`, `skipped` or `failed`, with a reason where
  one helps. The phases after a failure are skipped with `not run: <phase> failed`. Once the context has ended, as
  when systemd stops the unit, the next phase fails with `not started: …`.
- **`/var/lib/tent/status.json`.** `up` writes it on every run, whatever happened, for the operator and
  `tent toolbox dump`: tent-node's version, the spec hash, the start and end times, the instance (id, zone, private
  IP) and each phase's result. Only root reads it: the directory has mode 0700 and the file 0600.
- **Built in M2.5, M2.6a and M2.6b:** `preflight`, `system` and `verify` in M2.5; `hostfirewall`, `runtime` and `cni`
  in M2.6a ([ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md)); `join`, `nomad` and the Nomad checks of
  `verify` in M2.6b ([ADR-0030](adr/0030-nomad-on-nodes.md)). They run over a filesystem, a program runner, an HTTP
  transport and a dialer that tests replace with fakes ([15](#15-testing)). After a reboot `hostfirewall` and `nomad`
  report `done`: the kernel has forgotten tent's table, which `hostfirewall` loads again, and `nomad` starts Nomad,
  which is never enabled. The other phases are `unchanged`.
- **Stopping a program.** On Unix each program that a phase runs has a process group of its own. When the context
  ends, the runner sends SIGTERM to the group and waits for the program and its output, at most 10 s, after which it
  kills the program. When it stops waiting, it sends SIGKILL to what is left of the group.
- **Writing files.** `FS.WriteFile` and `FS.WriteStream` write a temporary file beside the target and rename it over
  the old one. They first remove the regular files `.<base>.tmp*` beside the target, which a write killed by SIGKILL
  or a power loss leaves; a directory that cannot be listed fails the write. `FS.Open` and `FS.HasContent` read a
  file as a stream, so a large file is never held in memory whole.

| Phase | What it does |
|---|---|
| `preflight` | Checks linux on amd64 or arm64, root, that systemd runs the machine, and Ubuntu, with a warning as the reason outside 24.04 and 26.04. The host name must be NodeConfig's `name`, which protects against mixed-up user data, and tent-node's version that of NodeConfig's `tent-node` asset. Then it reads the metadata service, for at most 3 minutes ([7.1](#71-interfaces)); NodeConfig carries no instance id. It changes nothing. |
| `system` | Writes the kernel modules of NodeConfig's `system` ([8.3](#83-nodeconfig-contract)) to `/etc/modules-load.d/tent.conf` and loads them with `modprobe`; writes its sysctls to `/etc/sysctl.d/99-tent.conf` and applies that file with `sysctl -p`; turns on NTP with `timedatectl set-ntp true` where timedatectl can, and elsewhere requires `chrony.service` or `systemd-timesyncd.service` to be active; limits the journal to 1 GiB with a drop-in and restarts journald. A command runs only when its file changed, and when it fails, the file goes, so that the next run tries again. A node without modules or sysctls, such as a server, gets no file for them. It does not set the host name. |
| `hostfirewall` | **Owns the host firewall.** Loads tent's table `inet tent` from `/etc/tent/firewall.nft` (0600) with `nft -f` when the kernel lacks it or holds another version (the table's comment carries the file's sha256); the file replaces only that table, in one transaction. Stops firewalld before the load and turns ufw off after it ([11.5](#115-firewall-and-host-firewall)). The chains: [9.6](#96-network-perimeter); the metadata block: [9.4](#94-secrets-on-nodes-threat-model). |
| `runtime` | Skipped when NodeConfig's `system.docker` is false. Writes `/etc/docker/daemon.json` (live-restore; json-file logs, 3 files of 10 MB) before any install, so that Docker's first start reads it, and restarts an installed Docker when the file changed, which restarts Nomad's docker tasks: Nomad's docker driver stops the containers that live-restore kept ([ADR-0030](adr/0030-nomad-on-nodes.md)); a failed restart removes the file. Installs Ubuntu's `docker.io` when `dpkg-query` does not report it installed: `dpkg --configure -a`, `apt-get update` and `apt-get install --no-install-recommends docker.io`, without questions and keeping changed configuration files, each tried again every 5 s after any failure, such as a lock that another apt holds, for up to 10 minutes. Then enables and starts `docker.service` where it is not; a job that systemd already has for it, such as the start job of a boot, means Docker starts on its own, and that is no change (`systemctl show -p Job`). Docker keeps its iptables backend. |
| `cni` | Skipped on servers. Fetches the `cni-plugins` asset through tent-node's asset cache ([8.5](#85-artifacts-and-verification)), checks every entry of the archive, then writes its files into `/opt/cni/bin` with their mode masked to 0755, owned by root. Only `./` and regular files at the top level with plain names pass; a nested path, `..`, an absolute path, a link, a device, a file above 256 MiB or a file that comes twice fails the phase before a file is written. Files that the archive lacks stay. |
| `join` | Renders `/etc/nomad.d/05-join.hcl` with `nodeconfig.RenderJoin` ([8.4](#84-nomad-configuration-rendering)). Once the node's TLS files exist, it asks the servers that answered last (`/var/lib/tent/peers.json`, 0600) and then NodeConfig's seed, one at a time, for `GET /v1/status/peers?stale` over mTLS with the TLS name `server.<region>.nomad`, 5 s each ([9.1](#91-pki)). The first answer is rendered and kept in the peers file; without one, the servers it knows are rendered. The first boot renders the seed alone, since `nomad` writes the TLS files later. On server and combined nodes the peers file holds the node's own address, which refuses the call at boot, since Nomad is not running yet ([ADR-0016](adr/0016-server-discovery-seed-and-refresh.md), [ADR-0030](adr/0030-nomad-on-nodes.md)). |
| `nomad` | Fetches the Nomad zip through the asset cache ([8.5](#85-artifacts-and-verification)) and writes the one regular file `nomad` in it to `/usr/local/bin/nomad` (0755; the directory 0755, `root:root`) as a stream, only when the file there differs; `LICENSE.txt` and other entries are ignored. Makes `/etc/nomad.d`, `/etc/nomad.d/tls` and `/var/lib/nomad` (0755), and on client and combined nodes `/var/lib/nomad/client` (0700) before the intro token goes into it. Writes each NodeConfig file ([8.4](#84-nomad-configuration-rendering)), `nomad.service` among them, and on client and combined nodes `11-instance.hcl` with the instance id from the metadata service (`nodeconfig.RenderInstance`), when its content, mode or owner differs (`FS.HasContent`). Runs `systemctl daemon-reload` only when `NeedDaemonReload` asks for it. Starts `nomad.service` when it is not active, and restarts it after a change of its binary, a NodeConfig file or `11-instance.hcl`. Before any write that needs a restart it writes `/var/lib/tent/nomad-restart`, which a successful start or restart removes; a failed mark leaves the file unwritten, so the next run writes it and restarts. Never enables it. The agent runs as root. |
| `verify` | Checks that `tent-node.service` and `tent-node-join.timer` are enabled. Then asks the node's own agent over mTLS (`https://127.0.0.1:4646`, TLS name `localhost`), each check until it answers 200: `GET /v1/status/leader?stale` on server and combined nodes, which answers without a leader (a leader needs other servers, which may boot later); and `GET /v1/agent/health?type=client` on client and combined nodes. A 500 or a lost connection is tried again 2 s later, 60 failed tries in all (about 2 minutes); anything else fails at once. It changes nothing. |

`tent-node refresh-join` runs from `tent-node-join.timer`, every `join.refreshInterval` (60 seconds) from boot on,
and after `tent-node.service` ([ADR-0030](adr/0030-nomad-on-nodes.md)):
1. It reads NodeConfig; a config that cannot be read or decoded exits 1. Then it takes the lock, waiting at most
   4 minutes for it. One deadline of 4m45s, 15 s less than its unit's `TimeoutStartSec`, bounds the whole run.
2. Without the TLS files it changes nothing. On server and combined nodes it asks the node's own agent first,
   `https://127.0.0.1:4646/v1/status/peers?stale` with the TLS name `server.<region>.nomad`: the cluster's first
   server has an empty seed, so only its own agent can name its peers. Then, as `join` does, it asks the servers of
   the peers file and the seed. A client asks those only.
3. The first answer wins; an empty one is no answer, since a server answers `[]` before the bootstrap. It rewrites
   `05-join.hcl` atomically only when the rendering changes, and keeps the answer in the peers file, which never
   holds `127.0.0.1`.
4. It runs no command and never restarts Nomad; a refresh only makes Nomad's next start find the current servers.
5. A refresh exits 0 when it succeeds, and when it changes nothing for want of TLS files, a known server or an
   answer. Of its failures, only the end of its own lock wait or deadline exits 0, with a line that the next refresh
   tries again. SIGTERM, whose cause the error names (`Error: refresh 05-join.hcl: terminated signal received`), and
   any other failure, even one after that deadline, exit 1. A refresh whose first server answers and which changes
   nothing logs nothing.

### 8.3 NodeConfig contract

Built in M2.3 in `internal/nodeconfig` ([ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md)).
`update` uses it since M2.7a ([13.2](#132-tent-update-cluster---yes)).

```go
// NodeConfig is the contract between tent (producer) and tent-node (consumer).
type NodeConfig struct {
	APIVersion string            // "tent/v1alpha1"
	Kind       string            // "NodeConfig"
	Cluster    string
	Provider   v1alpha1.Provider // the cloud, which picks tent-node's metadata service; not in the spec hash
	NodeGroup  string
	Name       string            // the node's name, also its hostname; no cloud instance id
	Role       v1alpha1.Role     // server | client | combined
	Region     string            // the Nomad region, for the TLS name of the servers; not in the spec hash
	Assets     []Asset           // {Name, Version, URLs (mirrors), SHA256}: nomad, tent-node; cni-plugins on clients
	Files      []File            // {Path, Mode, Owner, Content, PerNode, Secret} (8.4)
	Join       Join              // {Strategy: seed-and-refresh, Servers []netip.Addr, RefreshInterval}
	System     System            // {Sysctls, KernelModules, Docker}
	Firewall   HostFirewall      // {Rules []Rule{Name, Protocol, Ports, From}, BlockMetadata netip.Addr}
	SpecHash   string            // the group's spec hash (8.4)
}
```

- **Checks and encoding.** `Validate` checks the header, the names, the provider, the role, the region and the form
  of every asset, file, join setting, system setting and firewall rule, and that a stored spec hash is the config's
  own. The region follows the rule of `spec.nomad.region` (`v1alpha1.RegionOK`). A
  kernel module or a sysctl key must start with a letter or a digit: `modprobe` would read a leading dash as an
  option, and `sysctl.d` ignores the errors of a line whose key starts with one. `Encode` writes indented JSON, the
  same bytes for the same config. `Decode` refuses unknown fields and anything after the object, then validates.
- **Printing.** A file prints only its path and size, and an asset its URLs without their query or password. fmt,
  slog and encoding/json show the same, and `Encode` writes the content and the URLs as they are. Keys and tokens use
  the `Secret` type of `internal/secret`, which prints only its size.
- **Assets.** `nodeconfig.Asset` is NodeConfig's own type. `internal/app` converts the assets of `internal/assets`
  ([8.5](#85-artifacts-and-verification)) to it, so tent-node never imports `internal/assets`. Every node gets `nomad`
  and `tent-node`, and client and combined nodes `cni-plugins` too: servers run no workloads, and a new CNI version
  would otherwise mark every server out of date. The names are constants of `nodeconfig` (`NomadAsset`,
  `CNIPluginsAsset`, `TentNodeAsset`), which `internal/assets` uses too. A name must be a DNS label, as a rule's name
  must (below): tent-node names the asset's file in its cache after it ([8.5](#85-artifacts-and-verification)).
- **Built by `internal/app`.** `groupTemplates` makes a template per node group: the model's provider, the Nomad
  region from `spec.nomad.region`, the agent configuration from the completed specs, the CA bundle, `nomad.service`,
  the assets, the join strategy, the system settings, the host firewall and the spec hash. `nodeConfig` adds what one
  node has: its name, `10-node.hcl` with `bootstrap_expect`, its certificate and key, the seed of servers, and an
  intro token when it is given one: `update` gives one to clients only ([9.3](#93-client-introduction)).
  - One node builder runs these steps. It reads the assets once per architecture, makes a template per group, and
    returns a node's config from its group, name, zone, certificate, seed and intro token. `bootstrap_expect` is the
    size of the server group, or of the combined group, in the specs.
  - `update` uses the builder, and so does `NodeConfigOf` (with `NewNode`), which builds one new node with every group
    on one architecture. `NodeConfigOf` refuses a missing Nomad version, a missing group and an empty architecture
    before any request. `hack/tent-node-userdata` uses it.
- **Join:** `seed-and-refresh`, the private addresses of the servers that exist when the node is created, and a
  refresh every minute ([11.2](#112-server-discovery-seed-and-refresh)).
- **System**, per role. Servers get none.
  - Client and combined nodes get the kernel module `br_netfilter` and the sysctls
    `net.bridge.bridge-nf-call-arptables`, `-ip6tables` and `-iptables` set to 1, as Nomad's bridge networking asks.
  - They get Docker, and with it the kernel module `overlay`, unless the group's drivers leave `docker` out. A group
    without drivers keeps Nomad's own, Docker among them.
  - tent installs nothing else for drivers: `raw_exec` stays disabled unless the operator enables it in `extraConfig`,
    and `java` and `qemu` need packages that tent does not install.
- **Host firewall**, per role (decision 15 of [18](#18-open-questions)):
  - `ssh` (22/tcp), `icmp` and, on server and combined nodes, `api` (4646/tcp), each from `0.0.0.0/0` and `::/0`. The
    cloud firewall filters their sources by `access`, so a change of `access` leaves the nodes as they are. The ports
    come from the model's constants (`model.SSHPort`, `model.APIPort`), not from `access`: an empty `access.ssh`
    closes SSH on the cloud firewall only. The `icmp` rule's IPv6 prefix means ICMPv6; tent does not turn on IPv6 on
    Vultr instances yet;
  - the rules between nodes that reach the role, from the cluster CIDR ([7.2](#72-intents-the-providers-input)): on a
    server `nomad-http`, `nomad-rpc` and `serf`, on a client `nomad-http` and `dynamic`, on a combined node all four;
  - on client and combined nodes, `bridge-http` (4646/tcp) and `bridge-dynamic` (the dynamic ports, tcp and udp) from
    Nomad's default bridge `172.26.64.0/20` and Docker's default bridge `172.17.0.0/16`, for workloads that call the
    host at the node's address; they match source addresses only and do not follow `extraConfig`
    ([ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md));
  - `blockMetadata`: `169.254.169.254` on Vultr and Hetzner, which only tent-node's marked socket reaches
    ([9.4](#94-secrets-on-nodes-threat-model)).

  Rule names are DNS labels, 1 to 63 lower-case letters, digits and dashes, starting and ending with a letter or
  digit, since tent-node puts them into nft comments. `Validate` refuses any other name. `blockMetadata` must be a
  plain IP address, without an IPv6 zone and not IPv4-mapped, and no rule's source an IPv4-mapped prefix, since a
  mapped one would render as `ip6 daddr` or `ip6 saddr`, which no IPv4 packet matches.
- **Size.** One budget for every provider: the whole cloud-config must fit in 24 KiB (`nodeconfig.MaxUserDataBytes`),
  which leaves headroom under Hetzner's 32 KiB (decision 14 of [18](#18-open-questions)). Above it `UserData` fails
  with `user data: node group <group> needs <n> bytes, more than the 24576 that fit`.
  - The largest config of each role leaves at least 8 KiB for `extraConfig`. With `nomad.service`, two CAs, a 2 KiB
    intro token, a 1.5 KiB presigned tent-node URL, a mirror per asset, 5 seeds, 12 meta keys and every firewall rule,
    the user data takes 9.4 KiB on a server, 11.8 KiB on a client and 12.0 KiB on a combined node, which leaves
    12.0 KiB for `extraConfig` (tests, 2026-10-03). A combined node built from real data, with 621 bytes of
    `extraConfig`, takes about 10.7 KiB (10858 to 11042 bytes in five runs, since the size varies with the real
    keys).
  - Vultr's API accepts at least 4 MiB, and 64 KiB worked end to end ([11.6](#116-user_data)). A provider that allows
    less than 24 KiB, such as AWS with 16 KB, brings `Capabilities.MaxUserDataBytes`.
  - Fallback if a provider's limit is too small: user data carries only a short-lived presigned URL and a key for an
    encrypted NodeConfig object in the state bucket.
- **Versioning.** The contract is versioned, and tent-node always has the CLI's version. Version skew therefore
  appears only when a newer CLI operates a cluster whose nodes an older CLI created. Those nodes keep running their
  tent-node until they are replaced.

### 8.4 Nomad configuration rendering

- **The CLI renders the Nomad agent configuration**, and tent-node writes the files. So:
  - `tent update` can show a diff of the Nomad configuration per group;
  - the hash of the rendered configuration is exactly the "node is outdated" signal;
  - tent-node stays thin.

  tent-node renders two small per-node files itself, with the same package: `05-join.hcl`, which the refresh
  rewrites, and `11-instance.hcl`, since only the node knows its instance id.
- **`leave_on_terminate` by role** (decision 26 of [18](#18-open-questions), M2.7a,
  [ADR-0031](adr/0031-bootstrap-in-update.md)). `00-tent.hcl` sets it to `false` on server and combined agents, so a
  stopped server stays a Raft peer, and to `true` on clients. The line is in every role's file, with a comment. Nomad
  merges the key across the files of `/etc/nomad.d` with OR (v2.0.7, `command/agent/config.go`): `extraConfig.server`
  can turn it on again, from false to true only, and `extraConfig.client` cannot turn a client's `true` off. A server
  that stops stays a voter until autopilot's `cleanup_dead_servers` removes it, about 40 s on 2026-10-03
  ([platform notes §1.6](platform-notes.md#16-the-agent-on-a-node)), or until tent removes it through the API
  ([13.3](#133-tent-rolling-update-cluster---yes)).
- **Files in `/etc/nomad.d/`**, merged by Nomad in the order of their names. Root owns every file
  (`nodeconfig.Owner`, `root:root`).

  | File | Contents | Scope | Mode | Roles | Written by |
  |---|---|---|---|---|---|
  | `00-tent.hcl` | tent's settings: `server` and `client` blocks by role, ACL, TLS, Consul auto-join off, no update check, telemetry | group, in the hash | 0644 | all | tent |
  | `01-gossip.hcl` | `server { encrypt }` | group, secret, not in the hash | 0600 | server, combined | tent |
  | `05-join.hcl` | `server_join { retry_join = [...] }` | node, kept current by tent-node | 0644 | all | tent-node |
  | `10-node.hcl` | `name`, `datacenter`; `bootstrap_expect` on servers | node | 0644 | all | tent |
  | `11-instance.hcl` | the meta `tent_instance_id` | node | 0644 | client, combined | tent-node |
  | `98-user-server.hcl` | `extraConfig.server` as given | group, in the hash | 0600 | server, combined | tent |
  | `99-user-client.hcl` | `extraConfig.client` as given | group, in the hash | 0600 | client, combined | tent |
  | `tls/ca.pem` | the CA bundle | group, in the hash | 0644 | all | tent |
  | `tls/agent.pem` | the node's certificate | node | 0644 | all | tent |
  | `tls/agent-key.pem` | the node's key | node, secret | 0600 | all | tent |

  The user files exist only when their part of `extraConfig` is set. The intro token goes to
  `/var/lib/nomad/client/intro_token.jwt` (0600, secret) on the nodes that get one: every client,
  and no combined node ([9.3](#93-client-introduction)).
- **`nomad.service`** (`nodeconfig.RenderNomadService`, [ADR-0030](adr/0030-nomad-on-nodes.md)) is a NodeConfig file
  of every role too: `/etc/systemd/system/nomad.service`, 0644, group-level and in the hash, the same on every node.
  Compared with HashiCorp's stock unit it has:
  - SIGTERM where the stock unit sends SIGINT. A server exits at once with status 1 and stays a Raft peer, so the unit
    ends as failed after a stop of a server or combined node, and the restart works (seen on a real server on
    2026-10-05: [platform notes §3.16](platform-notes.md#316-spike-runs); decision 26 of
    [18](#18-open-questions));
  - systemd's default stop timeout of 90 s, which covers the 5-second graceful wait;
  - `After=docker.service` and no other tie to Docker, so a shutdown stops Nomad while Docker still answers;
  - no `[Install]`, so the unit is never enabled ([8.1](#81-bootstrap-chain)).

  It keeps `Type=notify`, `KillMode=process`, the reload by SIGHUP and the stock limits, and has no order on
  `tent-node.service` and no `Delegate=`. The text: [Appendix A](#appendix-a-nomad-agent-configuration-sketches).
- **No update check, no Consul auto-join.** `00-tent.hcl` sets `disable_update_check = true` and a `consul` block
  with `server_auto_join = false` and `client_auto_join = false` (both default to true), so the agent does not look
  for Nomad servers in Consul, which tent never runs. A client's Consul fingerprinter still asks Consul every 15 s.
- **No drain at shutdown** (decision 25 of [18](#18-open-questions), [ADR-0030](adr/0030-nomad-on-nodes.md)).
  `00-tent.hcl` sets no `drain_on_shutdown`: a client that drained itself when Nomad stops would come back from a
  reboot or a restart of Nomad ineligible, and Nomad has no setting that undoes it
  ([platform notes §1.6](platform-notes.md#16-the-agent-on-a-node)). So a reboot kills a client's tasks without a
  migration; they come back when the client restarts them, if it is back first, or when the scheduler replaces them
  after the missed heartbeats, and the node comes back eligible. A restart of Nomad does not drain, and
  `KillMode=process` is there so that the tasks keep running (seen on the M2.6b VM checks). tent drains a client
  through the API before it removes it ([ADR-0017](adr/0017-api-driven-server-removal.md)). tent does not support a
  `drain_on_shutdown` in `extraConfig`.
- **Values known only at runtime** (private IP, interface name) are go-sockaddr templates that select the interface by
  cluster CIDR. Nomad supports them in `bind_addr`, `addresses`, `advertise` and `client.network_interface`.
  Interface names vary on both Vultr and Hetzner, so this matters on both.
- **Joining.** A server joins the servers' Serf port, 4648, in `server { server_join { retry_join } }`, and a client
  their RPC port, 4647, in `client { server_join { retry_join } }`. A combined node takes the server form: Nomad gives
  its client the RPC address of the server in the same agent, which the client reaches over the private network, and
  the client learns the other servers from it. tent never writes `server.retry_join`, which Nomad 2.1 removes.
- **ACLs on every role.** `acl { enabled = true }` is in the `00-tent.hcl` of clients too: a client with ACLs off
  grants every request to its own endpoints ([platform notes §1.2](platform-notes.md#12-features-tent-relies-on)).
- **`bootstrap_expect`** is per node, in `10-node.hcl`, so a resize of the server group does not mark every server out
  of date.
- **The operator's files come last** and can override tent's settings, such as `data_dir`, `client.state_dir`, the
  `tls` file paths, the dynamic ports and Nomad's bridge subnet (`bridge_network_subnet`); they cannot turn off a
  boolean that Nomad merges with OR, such as a client's `leave_on_terminate`.
  The host firewall does not follow them. After such an override Nomad looks for the TLS files or the intro token
  where tent did not write them, and under strict client introduction the client is refused
  ([9.3](#93-client-introduction)). tent does not check `extraConfig`.
- **Quoting.** Every value is an HCL1 string: in double quotes, with `"` and `\` escaped by a backslash. A value that
  HCL1 cannot read back as itself is refused: invalid UTF-8, a control character, U+E123, or `${`, which starts an
  interpolation and has no escape ([ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md)). Errors name
  the setting, never the value.
- **Spec hash** (`nodeconfig.SpecHash`): the first 16 lower-case hex digits of the sha256 of a canonical JSON of the
  group-level configuration, in format 1.
  - Included: the group files that are not secret (`00-tent.hcl`, `98-user-server.hcl`, `99-user-client.hcl`, the CA
    bundle, `nomad.service`) with their path, mode, owner and content; each asset's name, version and sha256,
    tent-node's included; the system settings; the host firewall.
  - Excluded: the node's name, the per-node files (`05-join.hcl`, `10-node.hcl`, `11-instance.hcl`, the certificate,
    the key, the intro token), the secret files (`01-gossip.hcl`), the join settings, mirror URLs, the provider, which
    a cluster never changes, and the region, which `00-tent.hcl` carries. The hash does not depend on the order of
    rules, files or assets.
  - So a different download mirror never rolls the cluster (a kops pitfall), and a new CA does. A new tent version
    that changes rendering or tent-node rolls nodes, and the plan says why.
  - A test pins the hash of a fixed config. A change of the canonical form raises the format.
- **Format.** Nomad parses the agent configuration with HCL1. Rendering uses text templates with strict quoting. The
  golden files are authoritative, and tests parse each one back with `github.com/hashicorp/hcl` v1. The `online` CI
  job checks them each week with `nomad config validate` ([16](#16-technology-stack-and-releases)).

The golden files and a sketch: [Appendix A](#appendix-a-nomad-agent-configuration-sketches).

### 8.5 Artifacts and verification

| Artifact | Source | Verification |
|---|---|---|
| Nomad | `https://releases.hashicorp.com/nomad/<v>/nomad_<v>_linux_<arch>.zip` | The **CLI** downloads `nomad_<v>_SHA256SUMS` and verifies its detached signature with HashiCorp's release key, which is embedded in tent. Only SHA-256, SHA-384 and SHA-512 signatures count. The node verifies only the sha256 carried in NodeConfig. |
| CNI plugins | `https://github.com/containernetworking/plugins/releases/download/v<v>/cni-plugins-linux-<arch>-v<v>.tgz`, the version from the channel | The channel holds the sha256 per architecture, fixed when tent is released: CNI releases carry no signature. NodeConfig carries it to client and combined nodes. |
| tent-node | GitHub release of tent: the bare binaries `tent-node_linux_amd64` and `tent-node_linux_arm64`, listed in `checksums.txt` (M2.5, [16](#16-technology-stack-and-releases)) | A release build of the CLI reads `checksums.txt` of its own tag over TLS, without checking its cosign signature, and puts the sha256 into user data. A development build takes `TENT_NODE_URL` and `TENT_NODE_SHA256` (below). |
| Docker | Ubuntu's archive: the `docker.io` package ([8.2](#82-tent-node-phases)) | distribution package signatures |

- Trust is established once, on the operator's side, so nodes need no PGP.
- The HashiCorp APT repository is deliberately **not** used: its signing key was rotated on 2026-09-09 after a
  security incident.
- **Built in M2.2** (`internal/assets`, [ADR-0026](adr/0026-channels-and-release-assets.md)). Each artifact resolves
  to a name, a version, its URLs (one for now; NodeConfig adds mirrors) and a sha256. Each file is read with one
  request, which the caller's context bounds, and errors name the URL. The CLI caches nothing. The time at which the
  signature is checked is injectable (`assets.Options.Now`).
- **NodeConfig carries the assets** from M2.3: `internal/app` resolves them (`resolveAssets`) and converts them to
  NodeConfig's own type ([8.3](#83-nodeconfig-contract)). Since M2.7a `update` reads the release files with its own
  clock for the signature check, but only in a plan that creates a node or repeats the create of one. Such a plan needs
  releases.hashicorp.com, and for a release build github.com. A plan without node changes reads no release file and
  needs no development variables ([13.2](#132-tent-update-cluster---yes),
  [ADR-0031](adr/0031-bootstrap-in-update.md)).
- **tent-node's downloads** (M2.6a, [ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md)). tent-node keeps
  one file per asset, `/var/lib/tent/assets/<name>` (0600; `/var/lib/tent` and `assets` 0700). A file with
  NodeConfig's sha256, mode 0600 and owner root is used without a download; tent-node checks it as a stream
  (`FS.HasContent`), so a hit holds no asset in memory. A file with the right content and a wrong mode or owner is
  rewritten from itself as a stream. Any other content, such as an older version, is hashed as a stream and removed
  before the download, so each asset has at most one version on disk. The phases get the cache file's path: `nomad`
  opens its zip with `FS.Open`, and `cni` reads its archive once with `ReadFile` and checks the sha256 of the bytes it
  unpacks, so a cache file changed after the check fails the phase. No path reads a whole cached asset into memory
  but `cni`'s one read.
  - It tries the asset's URLs in turn, through Go's default transport and proxy settings, and follows up to 10
    redirects itself, as GitHub's release downloads need.
  - Up to 3 tries per URL, 2 s apart, after a failed connection, a 429, a 5xx other than 501, or a body that stalls,
    is cut short or runs out of time. Any other status, a wrong sha256 and the size limit move on to the next URL at
    once.
  - Limits: 10 minutes per try, 60 s for the headers, 60 s without a byte of the body, 256 MiB.
  - Errors and logs show URLs without their query or password (`nodeconfig.RedactURL`).
  - **The Nomad zip** (M2.6b, [ADR-0030](adr/0030-nomad-on-nodes.md)), about 55 MB, is read from the cache file at
    any offset, so the `nomad` phase holds neither the zip nor the binary in it, about 140 MB, whole; a download holds
    the zip in memory while it checks its sha256. The phase hashes the binary and compares it with
    `/usr/local/bin/nomad`, and writes it only when they differ, as a stream through a temporary file.
- **The key expires.** HashiCorp's key and its signing subkey expire on 2030-03-01
  ([platform notes §1.4](platform-notes.md#14-downloads-and-verification)). tent checks a signature at the current
  time, so from then on it verifies no Nomad download, older releases included, and fails with `HashiCorp's release
  key embedded in this tent expired on 2030-03-01: a newer tent, with the renewed key, is needed`. The weekly `online`
  CI job fails from 180 days before that ([16](#16-technology-stack-and-releases)). A revocation by HashiCorp reaches
  tent only with a new embedded copy of the key.
- **Development builds of tent-node.** A tent whose version is exactly a release or a pre-release tag, such as
  `v0.3.0` or `v0.3.0-rc.1`, is a release build (`buildinfo.IsRelease`). Every other build is a development build:
  `dev`, snapshots, `git describe` output such as `v0.3.0-4-gabc1234`, and `-dirty` builds. The version guard counts
  `git describe` output as its tag ([10.2](#102-layout)), but the tag's tent-node is not the one built from the later
  commit, and a node runs the CLI's own tent-node.
  - A development build's tent-node is uploaded to object storage and served through a presigned URL:
    `TENT_NODE_URL` plus `TENT_NODE_SHA256`. One URL and one sha256 serve every architecture.
  - Without them a development build fails and names them. A release build ignores them. Its warning when they are
    set is `devVariablesWarning` in `internal/app`. `update --yes` shows it before the first change, such as
    `TENT_NODE_URL is set, but tent v0.3.0 is a release build and ignores it: its nodes download the tent-node of
    release v0.3.0`. A plan without `--yes` does not warn.
  - `cmd/tent` reads `TENT_NODE_URL` and `TENT_NODE_SHA256` once at start and gives them to the service as
    `assets.Options`. A release build ignores them.
  - **Where it lives** (decision 18 of [18](#18-open-questions),
    [ADR-0028](adr/0028-tent-node-agent-units-and-delivery.md)). `make dev-upload` builds tent and tent-node, and
    `hack/tent-node-upload` puts tent-node into the CI R2 bucket at `dev/tent-node/<sha256>/tent-node_linux_amd64`,
    presigns a GET for at most 7 days and prints the two variables for fish or sh
    ([README](../hack/tent-node-upload/README.md)). The maintainer's machine uses its own R2 token, and a lifecycle
    rule deletes `dev/` objects after 8 days.
  - `make build` builds tent-node for linux/amd64 only, so a development build's clusters need amd64 plans. On an
    arm64 plan the node fails at cloud-init with an exec format error.

### 8.6 Operating systems

- **Ubuntu** (apt-based) is supported in v1. The default image is `ubuntu-24.04` (decided on 2026-09-25). E2E also
  runs on `ubuntu-26.04`.
- **Architectures:** x86-64 everywhere, and arm64 only where the provider offers it. Hetzner has CAX; Vultr has no
  arm64 Cloud Compute (151 plans, none arm, on 2026-10-05,
  [platform notes §3.8](platform-notes.md#38-regions-images-plans-and-billing-)).
  - A node group's architecture comes from `cloud.Provider.Arch` for its machine type, and `update` resolves the
    assets for it. Vultr's `Arch` answers amd64 for every plan without a call, so an arm64 plan on Vultr would get
    amd64 files until the provider reads the plan.
- **Other distributions** come later behind a small `osfamily` abstraction in `nodeup`.

---

## 9. Security

See [ADR-0007](adr/0007-security-baseline.md) and [ADR-0008](adr/0008-node-credential-delivery.md).

### 9.1 PKI

`internal/pki` makes the CA, the certificates, the gossip key and the ACL bootstrap secret (M2.1). `update` keeps the
CA and the secrets in the state store ([13.2](#132-tent-update-cluster---yes)). A run holds the CA, the gossip key and
the bootstrap secret in memory, stored or new, and issues each node's certificate and the run's operator certificate
from the CA without storing them (M2.7a). The storage and certificate details are in
[ADR-0024](adr/0024-cluster-pki-storage-and-certificates.md).

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
  - validity is 1 year, and renewal means replacement. `tent validate cluster` warns 30 days before the end and fails
    after it. tent stores no node certificate, so it reads the end as the machine's creation time plus one year
    (`pki.NodeCertificateEnd`, which `IssueNode` uses too): an estimate, since the machine is created after its
    certificate is issued. A machine without a creation time is not checked. The CA's end is checked the same way
    ([13.6](#136-tent-validate-cluster---wait-duration)).
- **No node IP addresses in certificates**, only `127.0.0.1`. The CLI connects to a server's public IP with
  `TLSServerName = server.<region>.nomad`, so server IPs can change freely.
- **Operator certificates** use `cli.<region>.nomad`, with `clientAuth` only. They are short-lived and issued on
  demand by `tent export nomad`, whose `--ttl` defaults to 24 hours, and by `tent ui`, which uses 24 hours
  ([9.7](#97-operator-access)). `internal/pki` takes any TTL above zero.
- **mTLS** is on for RPC and HTTP: `verify_server_hostname = true`, and `verify_https_client = true` by default.
- **tent-node's calls** to Nomad's HTTP API (`join`, `refresh-join` and `verify`, M2.6b,
  [ADR-0030](adr/0030-nomad-on-nodes.md)) use `net/http` and `crypto/tls` alone, since `nomadops` would link
  `nomad/api` into tent-node: TLS 1.2 or newer, the cluster's CA bundle as the only roots, the node's certificate and
  key, no proxy, no redirects, 5 s per call and answers of at most 1 MiB. A call to a server at its private address
  names `server.<region>.nomad`, with the region from NodeConfig. Of the calls to the node's own agent at `127.0.0.1`,
  `verify`'s name `localhost`, and `refresh-join`'s names `server.<region>.nomad`, which a server's certificate
  carries. Errors and logs never show the URL.
- **The gossip encryption key** is used on servers only. It is 32 random bytes in standard base64, as
  `nomad operator gossip keyring generate` makes.
- **The ACL bootstrap secret** is a random lower-case UUID of version 4 ([9.2](#92-acl-and-tokens)).
- **Stored values are checked.** Each plan of `update` loads the stored CA. It checks that the stored gossip key is
  standard base64 of 32 bytes, without line breaks, and that the stored ACL bootstrap secret is a UUID of that form.
  tent never replaces them ([13.2](#132-tent-update-cluster---yes)).
- **Keys and secrets never print.** fmt, slog and JSON show only their size, such as `[secret, 44 bytes]`.

### 9.2 ACL and tokens

- **ACLs are always enabled.**
- **Bootstrap.** Built in M2.4 as `nomadops.Client.Bootstrap`; `update` calls it since M2.7a.
  - Every `nomadops` client carries the bootstrap secret as its token, also for the bootstrap call itself, which
    Nomad accepts: `ACL.Bootstrap` throws away the auth error of that call (`nomad/acl_endpoint.go`, v2.0.7; run on
    2026-10-05, [platform notes §1.6](platform-notes.md#16-the-agent-on-a-node)).
  - tent generates the bootstrap secret, a UUID, and stores it in the state store **before** it calls
    `PUT /v1/acl/bootstrap {"BootstrapSecret": ...}`.
  - Before any request, `Bootstrap` checks that the secret is a lower-case UUID of version 4, as tent makes it. It
    never sends an empty secret, for which Nomad would make one that nobody knows.
  - Nomad's bootstrap is not idempotent: a second call fails with 400 `ACL bootstrap already done (reset index: N)`.
    On that answer, `Bootstrap` calls `GET /v1/acl/token/self` with the stored secret. A management token means an
    earlier call bootstrapped with this secret, and `Bootstrap` succeeds. A 403, or a token of another type, means
    the cluster was bootstrapped with another secret: the call fails with `ErrBootstrapMismatch`, whose message names
    `secrets/acl-bootstrap-token` and never the value.
  - So a bootstrap whose answer was lost is safe to repeat. The lost call fails with `ErrNotReady`, since its outcome
    is unknown, and the next call finds the bootstrap done and verifies the secret.
- **The bootstrap token is used only by tent itself.** Scoped tokens with TTLs for tent's own operations come later.
- **For humans**, `tent export nomad` and `tent ui` each make a separate management token with a TTL
  (`nomadops.CreateToken`, `PUT /v1/acl/token`, [ADR-0033](adr/0033-operator-commands.md)).
  - The token's name is `tent <purpose> <owner>@<host>`, such as `tent export nomad igor@laptop`, with the owner and
    host as the cluster lock names its holder. Nomad makes the accessor and the secret and keeps the token; tent
    keeps neither.
  - Nomad allows a TTL from 1 minute to 24 hours unless its servers are set otherwise. tent does not check that and
    shows Nomad's refusal, which reads like `nomad: PUT /v1/acl/token: 400: token 0 invalid: 1 error occurred: *
    expiration time cannot be more than 24h0m0s in the future (was 24h1m0s)`.
  - It is a management token, since tent owns no ACL policy yet. The TTL is the limit.
  - A call that fails or loses its answer may leave a token that nobody holds. It ends with its TTL, as do the token
    of an earlier export and the token of a `tent ui` that has ended. `nomad acl token delete <accessor>` revokes one
    early.

### 9.3 Client introduction

Servers run with `client_introduction { enforcement = "strict" }` by default (Nomad 1.11+). A cluster with a combined
group defaults to `warn`, and `strict` is refused there: the combined node's client registers before intro tokens
exist ([ADR-0019](adr/0019-combined-server-client-role.md)).

- **Issuing.** Right before each client VM is created, `update` requests an introduction token with
  `PUT /v1/acl/identity/client-introduction-token` (`nomadops.Client.IntroToken`, built in M2.4, used since M2.7a). The
  token is bound to the node name and node pool. Every client gets one, whatever the enforcement.
  - **Combined nodes get none**: `update` asks for no token for them
    ([ADR-0019](adr/0019-combined-server-client-role.md), [ADR-0031](adr/0031-bootstrap-in-update.md)). `nodeConfig`
    refuses a token only on the `server` role.
- **TTL.** 30 minutes at most (`nomadops.MaxIntroTTL`), the default `max_identity_ttl` of the servers. A server cuts
  a longer TTL to its maximum without a word, so nomadops refuses one before it sends the request. It refuses an
  empty node name or pool too. A server accepts a token for one more minute after it expires
  (`nomadops.IntroLeeway`, [platform notes §1.6](platform-notes.md#16-the-agent-on-a-node)).
- **Node pools.** tent creates none. Nomad creates a pool when its first client registers, and an intro token may
  name a pool that does not exist yet (decision 17 of [18](#18-open-questions)).
- **Delivery.** NodeConfig carries the token as a secret file, and tent-node writes it to
  `<client state_dir>/intro_token.jwt`, because the agent configuration file cannot carry it. With tent's `data_dir`
  that is `/var/lib/nomad/client/intro_token.jwt` ([8.4](#84-nomad-configuration-rendering)). An `extraConfig` that
  moves `data_dir` or `client.state_dir` makes Nomad look for the token elsewhere, and under `strict` the client is
  refused.
- **Lifetime.** Nomad uses the token only for the first registration. After that the node holds a self-renewing node
  identity.
- **Gotchas.**
  - An expired or mismatched token is rejected even with `enforcement = "warn"`.
  - A client that has not registered 31 minutes after its machine was created (`nomadops.MaxIntroTTL` plus
    `nomadops.IntroLeeway`) is deleted and created again with a new token ([13.4](#134-scaling),
    [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)).
  - Client introduction does not replace mTLS.

### 9.4 Secrets on nodes: threat model

Neither Vultr nor Hetzner offers instance identity. `user_data` is readable through the metadata service from inside
the VM, and by default that includes containers.

| In user data | Risk if read | Mitigation |
|---|---|---|
| CA certificate | none (public) | — |
| Node certificate and key | impersonate that node | nftables lets only tent-node's marked socket reach the metadata service (below); servers run no workloads (except combined nodes, [ADR-0019](adr/0019-combined-server-client-role.md)); **Vultr: user data is scrubbed once the node has joined** |
| Gossip key (servers only, `01-gossip.hcl`, mode 0600) | join the server gossip pool | servers run no workloads (except combined nodes); Serf and RPC listen only on the private network |
| Intro token (clients only) | register a fake client | TTL ≤ 30 min, bound to one node name and pool, used once at first registration |
| Cloud API token | — | **never on nodes** |
| State store credentials | — | **never on nodes** (unlike kops on Hetzner) |

**The metadata block** (decision 21 of [18](#18-open-questions),
[ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md)):
- tent-node's metadata client marks its socket with `0x747` ([7.1](#71-interfaces)). In tent's nftables table the
  output chain lets a packet leave for the metadata address only with that mark, and the forward chain drops every
  packet to it, so containers on a bridge get no answer either. Both drops count their packets.
- Root containers with host networking, exec and java tasks, and Nomad's artifact fetcher, which runs as root in
  `nomad.service`, get no answer. Neither does root on the host: `curl` to the metadata service on a node stops
  working.
- A workload with CAP_NET_ADMIN or CAP_NET_RAW can set the mark itself; CAP_NET_RAW alone is enough since Linux 5.17.
  Nomad gives workloads neither by default. What can still reach the service:
  - root `raw_exec` tasks and privileged containers, which are root on the node anyway;
  - a task that the operator gives NET_RAW through `allow_caps` or `cap_add`, for ping for example;
  - a container started with a plain `docker run` outside Nomad, which gets NET_RAW by Docker's default. Nomad's docker
    driver drops it.
- After a reboot the table is back once `tent-node.service` has run. Until then no Nomad workload runs: Nomad starts
  only from `up`, after the table ([ADR-0030](adr/0030-nomad-on-nodes.md)).

Scrubbing on Vultr works like this:
- Vultr lets user data be changed after creation.
- Once a node has joined its cluster, tent replaces its user data with a stub that holds no secrets and labels the
  machine `tent/joined=true`, in one call (`Nodes.MarkJoined`, [11.6](#116-user_data)). From then on the metadata
  service no longer serves the secrets.
- A node has joined when
  - a server's machine has a voter at its private address in the Raft configuration, after the servers are healthy
    and vote ([13.2](#132-tent-update-cluster---yes));
  - a client's machine has a node of its name and private address that Nomad lists as ready and eligible;
  - a combined node has both.
- The label tells the next plan that a machine has joined, without a call to Nomad. tent trusts the label and never
  reads user data back.

The spike confirmed on 2026-09-25 that a changed user data reaches the metadata service within seconds and that
cloud-init does not re-run after a restart ([ADR-0018](adr/0018-vultr-provider-design.md)). Hetzner user data is
immutable, so there it stays for the node's lifetime.

Until a node has joined, its secrets stay in its user data. A node that never joins keeps them: a server or combined
node blocks every client change then ([13.2](#132-tent-update-cluster---yes)), and a client that has not registered
after 31 minutes is deleted and created again ([13.4](#134-scaling)). A cluster built by M2.7a's tent has no
labels: its first `update --yes` waits for every node, scrubs it and labels it, and creates nothing.

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
    `validate cluster` and the commands that change a cluster warn loudly while it is open to the whole internet
    ([14](#14-cli)).

  Everything else is dropped.
- **Hetzner** firewalls are applied both by label selector and explicitly at server creation. **Vultr** sets the
  group at instance creation. Either way the machine is protected from its first packet.
- **Binding.** RPC and Serf bind only to the private address. HTTP on clients binds to localhost plus the private
  address. HTTP on servers binds to all addresses, behind the cloud firewall.
- **Private traffic is not filtered by either cloud.** tent-node's nftables table ([8.2](#82-tent-node-phases)) drops
  at input what no rule accepts.
  - It allows Nomad's ports and the dynamic ports only from the cluster CIDR, each on the roles it reaches, and on
    client and combined nodes 4646 and the dynamic ports from Nomad's and Docker's default bridges.
  - It opens SSH, ICMP and, on servers, 4646 to every source and leaves their sources to the cloud firewall, so a
    change of `access` never changes the nodes ([8.3](#83-nodeconfig-contract), decision 15 of
    [18](#18-open-questions)).
  - It also accepts replies, loopback, IPv6 neighbour discovery and DHCP replies.
  - Its forward and output chains drop only traffic to the metadata service ([9.4](#94-secrets-on-nodes-threat-model)),
    so the chains of Docker and the CNI plugins decide the rest of the forwarded traffic.
- **Load balancers:** Vultr load balancers are always public, and Hetzner firewalls do not apply to load balancers. An
  optional API load balancer is therefore safe only because of mTLS plus ACL. Vultr load balancer firewall rules can
  narrow the sources.

### 9.7 Operator access

**Built in M2.8** ([ADR-0033](adr/0033-operator-commands.md)): `tent export nomad` and `tent ui`. Neither hands the
operator the CA key or the ACL bootstrap secret.

- **`Service.OperatorAccess(ctx, cluster, purpose, ttl)`** makes the access for both commands. A TTL that is not above
  zero fails first, with `operator access: TTL 0s is not above zero`.
  - **It reads** the state store (the tent version, the stored specs with their defaults and without validation, so no
    `--allow-single-server`, the four secrets and the mark `nomad/bootstrapped`), then the cloud's list of the
    machines, then it calls Nomad once. It writes nothing and takes no lock.
  - **It makes** a management token ([9.2](#92-acl-and-tokens)) and an operator certificate: `cli.<region>.nomad`,
    client authentication only, a new ECDSA P-256 key, valid from 5 minutes before now for the TTL and never past the
    CA ([9.1](#91-pki)). A certificate that cannot be issued leaves no token. It is made in memory.
  - **It returns** `app.Access`: the cluster, the Nomad region, the servers' API addresses as `host:port`, the CA
    bundle (public), the certificate and its key, the token's secret and accessor, and when the access ends (in UTC,
    the earlier of the token's end and the certificate's end). The key and the secret are `secret.Secret`, so printing
    an `Access` shows their sizes only.
  - **The servers** are the cluster's machines with a server or combined role label and a public address, also a
    stopped one or one that the specs no longer want, ordered by name. The server that answered the token call comes
    first. A call that fails with `ErrNotReady` moves to the next server.
  - **Errors,** each before any Nomad call unless it says otherwise:
    - `cluster prod: the state store lacks pki/private/ca.key and pki/ca-bundle.pem; run tent update cluster --yes
      first`, with the missing paths of the layout; a stored secret that does not load fails as in `update`;
    - `cluster prod: Nomad is not bootstrapped yet (prod/nomad/bootstrapped is missing); run tent update cluster --yes
      first`;
    - `cluster prod has no server machine; run tent update cluster --yes first`;
    - `cluster prod: no server has a public address`, and `no Nomad client is set up` when the service has no way to
      reach Nomad;
    - `create the operator token: <cause>`, and after an error that matches `ErrNotReady`, also a cluster without a
      leader, the hint `; tent reaches the servers on port 4646: check spec.access.api`.
  - **Safe to repeat.** Each call makes a new token and certificate; for the tokens that stay, see
    [9.2](#92-acl-and-tokens).
- **`tent export nomad [NAME] [--ttl 24h] [--dir DIR] [--shell sh|fish]`** writes the access to four files and prints
  the lines that use them.
  - **The files** are `ca.pem` (the CA bundle), `cli.pem` (the certificate), `cli-key.pem` (its key, PKCS#8 PEM) and
    `token` (the token's secret, one line without a line end). The CA key and the bootstrap secret are in none of them.
  - **The directory** is `--dir` (made absolute), else `$XDG_CACHE_HOME/tent/<cluster>`, else
    `~/.cache/tent/<cluster>`, on every operating system; a relative `XDG_CACHE_HOME` is ignored. A directory that does
    not exist is made with mode 0700, and one that exists keeps its mode. When `OperatorAccess` fails, a directory
    that this run made is removed again, that one directory only and only while it is empty; one that existed stays as
    it was. The files have mode 0600 (on Unix). Two state
    stores with a cluster of the same name share the directory; `--dir` separates them.
  - **The files are written in two steps.** Each goes to a temporary file `.<name>-*` in the directory, created with
    mode 0600. When all four are written, they are renamed over their places in the order `ca.pem`, `cli.pem`,
    `cli-key.pem`, `token`. So a `nomad` that runs meanwhile reads a whole file, and a later run replaces the files of
    an earlier one. A write that fails removes every temporary file, reads `write <path>: <cause>` and leaves the files
    of an earlier run as they were. A rename that fails reads the same and leaves the files renamed before it, so old
    and new files can then sit side by side.
  - **The order:** `--shell`, `--ttl` (`invalid --ttl 0s: must be above zero`) and the cluster's name are checked, the
    directory is made, then `OperatorAccess` runs, then the files are written. So a directory that cannot be made
    leaves no token.
  - **stdout** has six lines and nothing else, for sh:

    ```
    export NOMAD_ADDR='https://203.0.113.5:4646'
    export NOMAD_CACERT='/home/igor/.cache/tent/prod/ca.pem'
    export NOMAD_CLIENT_CERT='/home/igor/.cache/tent/prod/cli.pem'
    export NOMAD_CLIENT_KEY='/home/igor/.cache/tent/prod/cli-key.pem'
    export NOMAD_TLS_SERVER_NAME='server.global.nomad'
    export NOMAD_TOKEN="$(cat '/home/igor/.cache/tent/prod/token')"
    ```

    For fish each line reads `set -gx NAME 'value'`, and the last `set -gx NOMAD_TOKEN (cat '<dir>/token')`. `--shell`
    defaults to fish when `$SHELL` names fish, else sh; any other value is `invalid --shell "x": want sh or fish`.
  - **Why the last line reads the file.** The `nomad` CLI reads the token from `NOMAD_TOKEN` or `-token` only and has no
    variable for a file (`api/api.go` at v2.0.7, [platform notes §1.2](platform-notes.md#12-features-tent-relies-on)).
    A line with the secret would put a secret on stdout. In fish `(cat file)` splits a file of several lines into a
    list, and the token file holds one line.
  - **`NOMAD_ADDR`** is the server that answered the token call. A server's certificate holds no address but
    `127.0.0.1` ([9.1](#91-pki)), so `NOMAD_TLS_SERVER_NAME` is needed: without it the CLI cannot verify the
    certificate for the server's public address. By Go's x509 the error reads `x509: certificate is valid for
    127.0.0.1, not <ip>` (not run against a tent server; the local run of 2026-10-06, whose certificate had no IP
    address, read `... because it doesn't contain any IP SANs`).
  - **stderr** says `wrote the Nomad access of cluster prod to /home/igor/.cache/tent/prod; it works until 2026-10-07
    12:00:00 UTC (token accessor <accessor>)`. The accessor is no secret.
  - **`-o json` and `-o yaml`** print `cluster`, `dir`, `address` (the whole `https://host:port`), `caCert`,
    `clientCert`, `clientKey`, `tlsServerName`, `tokenFile`, `tokenAccessor` and `expires` (RFC 3339, UTC, whole
    seconds), and no shell line. Windows and PowerShell users take these until PowerShell lines exist.
  - **The operator** runs `eval "$(tent export nomad prod)"` in sh, bash or zsh, or `tent export nomad prod | source` in
    fish, and then `nomad server members` or `nomad job run`. After the TTL, the same command again.
  - The token's secret and the key are never printed, and `-v` and `-vv` log neither.
- **`tent ui [NAME] [--listen 127.0.0.1:4646]`** serves a reverse proxy to the cluster on a loopback port, so the
  browser UI and the `nomad` CLI work with `verify_https_client = true` on and no certificate or token on the operator's
  side ([ADR-0033](adr/0033-operator-commands.md)). `internal/nomadops` holds the proxy (`NewProxy`, `ProxyConfig`).
  - **What it does to a request.** It passes the request to one of the servers at `https://<public IP>:4646` over mTLS:
    TLS 1.2 or newer, the cluster's CA as the only root, the operator certificate, the name `server.<region>.nomad`. It
    sets `X-Nomad-Token` to the token that `OperatorAccess` made for this run (purpose `ui`, 24 hours), in place of any
    token the request carried, and drops `Origin` and `Authorization`. It streams the answer as it comes
    (`FlushInterval: -1`), passes websocket upgrades through, and never follows a redirect. `HTTPS_PROXY` and `NO_PROXY`
    apply to it, as to `nomadops.New`. It speaks HTTP/1.1 to the servers, so each open request, such as a blocking
    query of the UI, holds one connection, and a Nomad server takes 100 connections from one address by default
    (`limits.http_max_conns_per_client`; not measured through the proxy).
  - **Servers.** It uses the first server until a request reaches no server. That request gets 502 `bad gateway: no
    Nomad server answered`, and the next request goes to the next server. An error status from a server does not move
    it on, and neither does a caller that gave up.
  - **Who may use the port.** While `tent ui` runs, a request to the port acts with a management token. So:
    - `--listen` takes `host:port` where the host is `localhost` (in any case) or an address in `127.0.0.0/8` or `::1`,
      and the port a number from 0 to 65535 (0 lets the system pick). An IPv4-mapped address, an address with a zone,
      `127.1`, another name and an empty host are refused: `--listen 0.0.0.0:4646: tent ui listens on a loopback
      address only`, `--listen x: want host:port, such as 127.0.0.1:4646`, `--listen 127.0.0.1:http: the port must be a
      number from 0 to 65535`. The check comes before the cluster's name and before any call, and the listener's real
      address is checked again;
    - a request whose `Host` is not the listener's own address, or `localhost` with the listener's port, gets 403
      `forbidden: unexpected Host`;
    - a request with an `Origin` gets 403 `forbidden: unexpected Origin` unless it is exactly one `http` origin whose
      host and port are the request's `Host`; one with a `Sec-Fetch-Site` gets 403 `forbidden: cross-site request`
      unless it is the single value `same-origin` or `none`;
    - a request with neither header, such as the Nomad CLI's, passes;
    - the 403 comes before anything goes to a server. Nomad's CORS answers cover 14 endpoints, and a blind `no-cors`
      POST from a page of another port wrote to Nomad even with `Origin` removed (a run of 2026-10-06, [platform notes
      §1.2](platform-notes.md#12-features-tent-relies-on)), so dropping `Origin` is not enough;
    - a link to the UI on a page of another site is a cross-site request and gets 403 too: the operator opens the
      address from the terminal, or types or pastes it;
    - every process of every user on the operator's machine can still use the port. `kubectl proxy` has the same
      property. Such a process can read the token's secret from `GET /v1/acl/token/self` and, with a management
      token, make tokens that do not expire; the end of the session undoes none of that;
    - two kinds of GET from a web page still pass, by the Fetch Metadata rules (not tried in a browser): a navigation
      that the user typed and that another site redirects to the port carries `Sec-Fetch-Site: none`, and a browser
      that sends no fetch metadata (Safari before 16.4) sends neither header with a cross-site GET. The page cannot
      read the answer of either.
  - **What the browser keeps.** The UI calls `/v1/acl/token/self` on every load and stores the secret of the answer in
    `localStorage` for `http://127.0.0.1:<port>`. So the browser holds the session's token until it expires after 24
    hours. It is no use without a client certificate of the cluster's CA.
  - **Order.** `--listen` is checked, the listener is opened (so a busy port leaves no token), `OperatorAccess` runs,
    the proxy is built with the listener's real address, then tent prints and serves. A context that ends before the
    server starts is `interrupted` with exit code 1.
  - **stdout:** `Nomad UI of cluster prod: http://127.0.0.1:4646/ui/`, or with `-o json` and `-o yaml` one object with
    `cluster`, `url` and `expires`. **stderr:** `the Nomad CLI works through it with NOMAD_ADDR=http://127.0.0.1:4646;
    press Ctrl-C to stop; the session ends at 2026-10-07 12:00:00 UTC`. With `-v` the log has one line per request:
    method, path and status, never a header or the query. tent opens no browser.
  - **The end.** Nomad answers an expired token as it answers a missing right on `/v1/jobs` and the autopilot report
    (403 `Permission denied`); only `/v1/acl/token/self` says `ACL token expired`
    ([platform notes §1.2](platform-notes.md#12-features-tent-relies-on)). So the session's end is tent's own clock.
    The session ends at Ctrl-C, or when the operator's wall clock reaches the end of the access, which tent
    reads at least once a minute: a Go timer does not count the time that the machine sleeps. tent then closes the
    listener, waits up to 5 seconds for open requests, closes what stays open and exits with 0. When the clock ends
    the session, stderr gets `the session of tent ui ended: its certificate and token expired; run it again`; Ctrl-C
    prints nothing more. An upgraded connection, such as an exec, is ended by the process's exit. The token
    is not deleted: it expires. The server has a read header timeout of 10 seconds and no other limit.
  - **Limits.** The UI's log view first calls the node's own HTTP address from the browser, which fails, and falls back
    to the server through the proxy after a delay. The port is a loopback port of the machine that runs tent: from
    another machine it is reached through an SSH tunnel with the same local port (`ssh -L 4646:127.0.0.1:4646`), since
    a `Host` with another port gets 403. Not tested: a page left open across a restart of `tent ui` with a new token,
    a real sleep of the machine, and whether the UI falls back to polling when a websocket fails.
- **tent's own calls** go through `nomadops.Client` (built in M2.4, used by `update` since M2.7a):
  - `update` makes one client per server with a public address, at `<public IP>:4646`, and uses them as one API
    (`nomadops.Servers`, [13.2](#132-tent-update-cluster---yes)). Its operator certificate lasts 24 hours, is made in
    memory for the run and never stored.
  - mTLS with an operator certificate (`cli.<region>.nomad`) and the cluster's CA, both PEM in memory; TLS 1.2 or
    newer, over HTTP/1.1. It always uses https and expects the certificate of `server.<region>.nomad` whatever
    address it dials (`TLSServerName`).
  - It is configured by hand, so the `NOMAD_*` variables of the Nomad CLI have no effect.
  - It goes through the proxy that `HTTPS_PROXY` and `NO_PROXY` name, as tent's other HTTPS clients do. TLS stays
    end to end, so a proxy does not see the token.
  - It never follows a redirect, so the token goes to the server alone: Go's HTTP client would send `X-Nomad-Token`
    to any redirect target ([platform notes §1.2](platform-notes.md#12-features-tent-relies-on)).
  - Its errors never show the token, a key or a secret.

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
- **`internal/s3url`** reads the s3 URL and makes its client, for this backend and for `hack/tent-node-upload`
  ([8.5](#85-artifacts-and-verification)); its errors never show the URL or a value of its query.
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
  nomad/bootstrapped                     # the mark that the ACL system is bootstrapped (the time)
  backups/<timestamp>.snap               # Raft snapshots (contain the keyring: secret)
  history/<timestamp>-<operation>.yaml   # audit trail of applies
```

`statestore.Layout` names these objects, and `statestore.Clusters` lists the clusters in a store: the top-level names
that hold a `cluster.yaml`.

`nomad/bootstrapped` is written last in the Nomad step: after the ACL bootstrap succeeded, the servers are healthy and
all vote, and the server and combined machines of the plan are scrubbed and labelled (M2.7a, M2.7b,
[13.2](#132-tent-update-cluster---yes)). `update` deletes it and writes it again when the store holds it and no server
or combined machine of the cluster stays. Only its existence is read; it holds the time in RFC 3339 and a newline.
`tent delete cluster` knows it ([13.7](#137-tent-delete-cluster---yes)).

Names that start with `.` belong to the backends, and `List` never returns them: `.tent-store.lock` and
`.tent-locks/` in the root of a `file://` store, the temp files of its writes (`.tent-tmp-*`), and the objects that
the s3 backend's probe writes below `.tent-probe/` ([10.4](#104-locking)).

**Version guard.** `CheckVersion` fails when `tent-version` names a newer tent than the running one, for example
`cluster prod needs tent v0.4.0 or newer; this is v0.3.1`. `RaiseVersion` records the running version when it is
newer, under the cluster's lock. A release, a pre-release and `git describe` output are checked, and
`v0.3.0-4-gabc1234` counts as `v0.3.0`. Development builds, such as `dev` and GoReleaser `-SNAPSHOT` builds, skip the
guard and never raise the version. The rule is `buildinfo.Release`. tent-node's assets use the stricter
`buildinfo.IsRelease`, under which `git describe` output is a development build
([8.5](#85-artifacts-and-verification)).

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
- **The holder's name** `owner@host`, the OS user and the host name, comes from `statestore.LocalHolder`, which the
  lease of a lock and the name of an operator token ([9.2](#92-acl-and-tokens)) share.
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
| Instances | none | label = hostname = `<cluster>-<group>-<index>`; tags = canonical labels via the codec, with `tent/joined=true` once the node has joined | rollout via `Nodes` | see [11.3](#113-creating-a-node) |
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

1. **Seed.** At creation, tent puts the private IPs of the servers that already exist into the node's NodeConfig
   (`join.servers`), and tent-node renders them into `05-join.hcl` ([8.4](#84-nomad-configuration-rendering)). tent
   takes them from `GET /v2/instances/{id}/vpcs` (`ip_address`, `mac_address`).
   - `update` seeds a server or combined node with the private addresses of every other server that the run knows,
     by name: the servers that exist, and the ones it created before. On first bootstrap `server-0` is created first
     with an empty seed, and each later server gets the servers created so far. Serf join is transitive, so
     `bootstrap_expect` sees every server. A client gets every known server
     ([13.2](#132-tent-update-cluster---yes)).
   - A server that is not ready has no private address yet. So `update` waits for the interrupted servers before it
     creates more, and fails a server's create when servers are listed and none has an address. A client's create
     fails the same way when no server is known.
2. **Refresh.** At every boot, before Nomad starts, the `join` phase of `up` asks the servers that answered last and
   then the seed for `GET /v1/status/peers?stale` and renders the answer into `05-join.hcl`
   ([8.2](#82-tent-node-phases)). `refresh-join` does it every 60 seconds, and on server and combined nodes asks the
   node's own agent first, so the cluster's first server learns its peers. Neither restarts Nomad.
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
   - **The instance limit.** Vultr counted a machine it had just deleted against the account's instance limit in one
     run ([platform notes §3.14](platform-notes.md#314-account-limits-terms-and-operations-)). A POST that
     fails with `ErrLimitReached` made no instance, so it is sent again every poll interval (5 s) while less than 2
     minutes (`limitSettle`) have passed since a delete that Vultr accepted on this provider value. A delete that
     answers 404 opens no window. The first refusal is logged at info level. A limit that holds on returns the error
     with its hint ([ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md) has the basis of the bound). Only the
     node create retries. When the context ends during a wait, the error matches the context's error.
4. **Lost answer.** govultr's own retries are off ([11.8](#118-api-client-rate-limits-cost)). After a create without
   an answer (`ErrUnavailable`), or with an answer that holds no instance id, it searches by the `op` tag once more
   and adopts what it finds. When that search fails or lists nothing yet, the error matches `ErrUnavailable`, and a
   `Create` with the same `op` searches before it creates. Vultr listed each new instance by tag on the first request
   after the create answer (spikes 2026-09-25 and 2026-09-27); on 2026-09-29 one of two needed a second request,
   about 1 s later ([platform notes §3.3](platform-notes.md#33-instances)).
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
  and reads each one's address in its VPC with `GET /v2/instances/{id}/vpcs`. `Instance.Joined` is true when the
  tags hold `tent/joined=true`.
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
- **Until M3, `update` deletes only nodes that never joined.** It refuses to delete a machine that carries
  `tent/joined=true`, and checks with Nomad before each delete ([13.4](#134-scaling),
  [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)). The paths below are the target; `delete cluster`
  destroys every machine as before.
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
  the VPC as well. tent-node's `hostfirewall` phase ([8.2](#82-tent-node-phases),
  [ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md)) loads tent's nftables table first and turns ufw off
  after it, so the node always has a host firewall on its first boot.
  - `ufw disable` runs only while `/etc/ufw/ufw.conf` says `ENABLED=yes`: it also sets the policies of the iptables
    `filter` chains to accept, which would undo Docker's drop in `FORWARD`. Then `systemctl disable ufw.service`.
    A machine without the ufw unit is left alone.
  - firewalld, which Ubuntu's images lack, is stopped and disabled before the load.
  - `nftables.service` is never enabled: its `/etc/nftables.conf` flushes every table, Docker's too.
  - An operator who turns ufw on again gets `ufw disable` at the next boot, which resets Docker's `FORWARD` policy
    until Docker restarts.
- **SSH.** The image allows root login with a password, and password guessing from the internet starts within minutes.
  The firewall groups allow 22/tcp only from `access.ssh`, and an empty list closes it.
- **Sysctls.** Vultr's vendor data writes `/usr/lib/sysctl.d/90-vultr.conf` and makes it immutable. tent-node puts
  its sysctls into a later file, such as `/etc/sysctl.d/99-tent.conf`.

### 11.6 user_data

- **Size.** The API accepts at least 4 MiB (spike 2026-09-25). A 65,508-byte user_data worked end to end: the
  metadata service served all of it, and cloud-init wrote its `write_files` payload intact. Larger payloads were not
  tested on an instance. tent's budget is 24 KiB for the whole cloud-config, as on every provider
  ([8.3](#83-nodeconfig-contract)).
- **Contents.** The cloud-config of [Appendix B](#appendix-b-cloud-init-user-data-sketch): package update and upgrade
  off (Vultr's vendor data sets the same today; tent keeps it explicit), the NodeConfig as a gz+b64 `write_files`
  entry, and the tent-node download. The M2.5 VM check found node.json intact from the gz+b64 payload on Vultr's
  Ubuntu 24.04 and 26.04 images on 2026-09-29
  ([platform notes §3.4](platform-notes.md#34-user_data-metadata-and-identity)). Since M2.7a `update` gives every
  node this user data, and its size is checked at plan time ([13.2](#132-tent-update-cluster---yes)). Once the node
  has joined, the user data is scrubbed (below).
- **Scrubbing.** Once the node has joined the cluster, the core calls `Nodes.MarkJoined`. It reads the instance with
  `GET /v2/instances/{id}`, then sends one `PATCH` with the instance's tags, in their order, with `tent/joined=true`
  added at the end, and the user data set to a stub that holds no secrets and no modules:

  ```
  #cloud-config
  # tent removed this node's user data after the node joined the cluster
  {}
  ```

  The empty mapping on the last line is needed: cloud-init refuses a cloud-config that loads to nothing and then
  reports a degraded status ([platform notes §6.5](platform-notes.md#65-cloud-init-and-the-stub)).

  - Vultr replaces the whole tag set when the PATCH sets `tags`, so the PATCH sends every tag the instance has, the
    operator's own too ([platform notes §3.3](platform-notes.md#33-instances)). A `tent/joined` tag with another
    value is replaced, since two tags of one key make the codec fail and `List` would drop the machine.
  - Vultr has no call that adds one tag. A tag that an operator changes between the read and the PATCH, about a
    second, is lost.
  - The PATCH changes nothing else. The spike of 2026-09-27 saw tags alone, and user data with `"tags": null`, work.
    Vultr applies both fields of one PATCH ([platform notes §3.3](platform-notes.md#33-instances)).
  - An instance that is gone counts as marked. The call is idempotent, so after an error that matches
    `ErrUnavailable` the caller may send it again.
  - Verified 2026-09-25: the metadata service serves the stub 4 s after the PATCH, and after a restart cloud-init
    neither re-runs `runcmd` nor changes the instance-id. Per-boot modules would run from the stub, so the stub
    contains none.

### 11.7 Zones, placement and availability

- **One failure domain.** A Vultr cluster lives in a single data center. There are no availability zones and no
  placement or anti-affinity parameters. Three servers survive the loss of a VM, not of the data center, and there is
  no guarantee that they run on different hosts. `validate cluster` warns about the single failure domain
  ([13.6](#136-tent-validate-cluster---wait-duration)).
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
- **Limits.** Vultr refuses a create that would pass a limit. tent shows Vultr's message and does not retry, except
  for a node create right after a delete ([11.3](#113-creating-a-node)).
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

Servers run with `leave_on_terminate = false` (decision 26 of [18](#18-open-questions)), so the ACPI shutdown stops
Nomad but the server stays a Raft peer. The `GracefulShutdown` optimization of
[ADR-0017](adr/0017-api-driven-server-removal.md) then gives a server no graceful leave: the peer goes through the
Nomad API, or through autopilot's cleanup when that is first. Clients still leave when they stop.

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
  - Since M2.7a that build includes the Nomad cluster: the servers, the ACL bootstrap and the clients. Since M2.7b it
    also scrubs the user data of each node that has joined.
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

**Built in M1, M2.7a and M2.7b.** `update cluster` brings the cluster's infrastructure and the sizes of its node
groups to the specs. Since M2.7a it also builds the Nomad cluster on them: servers with real user data, a leader, the
ACL system bootstrapped with the stored secret, healthy servers that all vote, and clients with intro tokens that
register ([ADR-0031](adr/0031-bootstrap-in-update.md)). Before M2.7a the nodes were empty machines that booted a
placeholder cloud-config without secrets. Since M2.7b it scrubs the user data of each node that has joined and labels
its machine, replaces a client that never registered, and refuses to delete a node that joined
([ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)).

```
 1. load specs → defaults → validate → the channel and the Nomad version (M2.2) → provider.Validate (region, plans,
    images)
 2. plan: inventory → provider.BuildInfra → engine plan; Nodes.List → node changes (13.4), which asks Nomad when
    a client may never have registered; the refusal to delete a node that joined; the completed spec; the missing
    secrets (M2.1); the Nomad step; for a plan that creates a node or repeats the create of one, the assets, a node
    config per group and the size of each node's user data
 3. without --yes: print the plan and stop
 4. lock → check the tent version → steps 1 and 2 again; the plan made under the lock is the one applied
 5. raise the tent version → write the missing secrets → write the completed spec when the plan says so
 6. the infrastructure's task changes (Plan.ApplyTaskChanges); then, when the plan bootstraps and the store holds a
    stale mark, the delete of the mark
 7. server and combined nodes: the waits that repeat a create, by name; then the creates, one at a time, by group
    and index, each seeded with the servers that exist and with a new operation id (cloud.NewOpID)
 8. the Nomad step: wait for a leader → bootstrap the ACL system → wait until the servers are healthy and all vote
    → read the Raft configuration → for each server and combined node of the plan, in order: for a combined node,
    wait until it has registered; check that a voter sits at its address; scrub its user data and label its machine
    → write the mark
 9. client nodes: the waits; then the deletes of clients that never registered; then the creates, one at a time, by
    group and index. A create, and a wait that repeats one, boots the machine with an intro token. Each client is
    waited for until it has registered, then scrubbed and labelled, before the next change
10. node deletes, one at a time, by name: duplicates, surplus nodes, nodes of groups not in the spec; each after
    Nomad's answer that its machine has not joined
11. the infrastructure's deletes: prune and duplicates (Plan.ApplyDeletes) → unlock
```

- **Failures.** The first step that fails stops the run, and the next run finishes the job. A run cut after a
  create's POST leaves an instance with its operation id. The next run counts it, and while it is not ready yet,
  plans `~ node prod-workers-1 (ID <id>, wait until it joins, scrub its user data)` and waits for it: while the cloud
  reports it not ready, the wait repeats the create with its operation id. No second instance is created.
- **Completed spec.** `cluster.completed.yaml` holds the specs with every default filled in
  ([3.3](#33-api-rules)). It counts as a change when the stored one is missing or differs, so the first run writes
  it, and so does a run after a spec change that changes nothing in the cloud. It is written before the first node,
  with the secrets (step 5, decision 12 of [18](#18-open-questions)), so the nodes run the Nomad version that it pins.
  After a run that failed later, it already equals the specs, and the next plan no longer lists it.
- **Deletes come last.** An object that the plan deletes may still hold nodes that step 10 removes, such as
  `<cluster>-clients` after the last client group is gone. Vultr's firewall guard refuses to delete a group that
  nodes use and the engine does not retry that ([11.5](#115-firewall-and-host-firewall)), so a delete in step 6
  would stop the run before step 10. The deletes therefore wait for step 11.
- **Deadlines.** `Nodes.Create` has no deadline of its own ([11.3](#113-creating-a-node)), so `update` gives each
  create and each wait that repeats a create 10 minutes. The leader wait, the health wait and each registration wait
  get 10 minutes each, and each scrub 5. The engine gives each infrastructure change 5 minutes
  ([6](#6-reconciliation-engine)).
- **Single server.** `update` validates the specs, so a cluster with one server needs `--allow-single-server` on
  every run, as every command that validates specs does.
- **Output.**
  - The plan goes to stdout: the infrastructure's lines as the engine writes them ([6](#6-reconciliation-engine)), one
    line per node change, a blank line, a line of counts per part that changes, a `Nomad:` line when the plan has a
    Nomad step, and a last line that names the objects that the plan writes to the state store, in write order: the
    missing secrets, the completed spec, then the bootstrap mark, such as `State: secrets/gossip.key,
    cluster.completed.yaml and nomad/bootstrapped will be written.` The line lists the mark whenever the plan
    bootstraps, also when the store holds a stale one that the apply deletes first; that delete shows nowhere in the
    plan, the progress or the applied line. A plan that writes only state is that line alone. Operation ids and the
    secrets' contents do not show. A plan without changes is `No changes.`, and a plan with changes adds `run with --yes
    to apply the changes` on stderr. An example with every kind of node change
    (`internal/app/testdata/update_plan.golden`, its infrastructure lines left out):

    ```
    ~ node prod-servers-1 (ID instance-2, wait until it joins, scrub its user data)
    + node prod-servers-2 (server, vc2-2c-4gb, ams)
    + node prod-workers-1 (client, vc2-4c-8gb, ams)
    - node prod-old-0 (ID instance-7, not in the spec)
    - node prod-workers-0 (ID instance-5, duplicate)
    - node prod-workers-3 (ID instance-8, surplus)

    Plan: 2 to create, 1 to update, 0 to replace, 1 to delete.
    Nodes: 2 to create, 1 to wait for, 3 to delete.
    Nomad: bootstrap the ACL system and wait for 3 healthy servers.
    State: pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token, cluster.completed.yaml and nomad/bootstrapped will be written.
    ```

    The `Nomad:` line reads `Nomad: wait for 3 healthy servers.` when the mark exists and a server or combined
    machine stays, and says `1 healthy server`
    for one. A plan with only the `Nomad:` and `State:` lines is a plan with changes.
  - With `--yes`, tent prints the plan made under the lock (step 4), applies it with each step on stderr as it
    happens ([14](#14-cli)), and then prints a blank line and one line in the past tense, each part only when it
    changed, such as `Applied: 4 created, 0 updated, 0 replaced, 0 deleted. Nodes: 5 created, 0 waited for, 0
    deleted. Nomad: bootstrapped the ACL system; 3 servers are healthy. Wrote pki/private/ca.key,
    pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token, cluster.completed.yaml and
    nomad/bootstrapped.` Without a bootstrap the Nomad part is `Nomad: 3 servers are healthy.` The writes to the
    state store print no progress lines. A cluster without changes prints `cluster prod is up to date`.
  - `-o json` and `-o yaml` print the plan as data: `{"infrastructure": <the engine's plan>, "nodes": [...],
    "nomad": {"bootstrap": true, "servers": 3}, "secrets": ["pki/private/ca.key", ...], "completedSpec": true}`,
    the node changes in the order they run and the secrets in the order they are written. A create, and a wait
    that repeats a create, carry `"specHash"`, the hash of the group's node configuration; a wait without an
    operation id has neither `op` nor `specHash`. `nomad` is left out when the plan has no Nomad
    step, and `secrets` when the store holds them all. With `--yes` they print only the plan that was applied, with
    `"applied": true`.
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

**Built in M2.2.** Step 1 checks the cluster's channel and Nomad version ([3.3](#33-api-rules)) and pins the version
in the completed spec ([ADR-0026](adr/0026-channels-and-release-assets.md)).
- **The pin.** The completed spec's `spec.nomad.version` is the one that the spec sets, else the one of the stored
  completed spec, else the one that the channel recommends. So the first `update` pins the recommended version, and a
  later tent that recommends another one keeps it: tent does not move a cluster to another Nomad by itself
  ([13.5](#135-tent-upgrade-cluster---yes)). A version that the spec set and then left out stays pinned.
- **A version in the spec wins** over the pin, even a lower one: nothing refuses a downgrade yet. `upgrade cluster`
  and `rolling-update` will (M3).
- **When the pin is written.** The pinned version is part of the completed spec, so a change of the version alone is
  a plan that writes `cluster.completed.yaml` and nothing else. So is the first plan of a cluster whose completed spec
  an older tent wrote without a version. Since M2.7a the first run pins the version before the first node (step 5), so
  a first `update` that is cut and then run again by a newer tent keeps the version that the first run pinned.
- **A pin outside the channel** fails the plan before it writes anything or reaches the cloud, such as `cluster prod
  is pinned to Nomad 2.0.7 (prod/cluster.completed.yaml), which is older than 2.1.0, the oldest Nomad that channel
  stable allows; set spec.nomad.version to a version that the channel allows`.
- **A completed spec that does not decode, or holds no Cluster,** blocks `update` only when the pin is needed, that
  is when the spec leaves `spec.nomad.version` empty. The error then says to set `spec.nomad.version` to the Nomad
  version the cluster was built with, or to fix the file by hand. With a version in the spec, `update` writes a new
  completed spec.
- **Untested versions.** When the version, set in the spec or pinned, is one that the channel has not tested,
  `update --yes` warns before it applies changes ([14](#14-cli)). A plan without `--yes`, or a run without changes,
  does not warn.
- **Downloads.** NodeConfig carries the assets from M2.3, and `update` reads their release files since M2.7a, in a plan
  that creates a node or repeats the create of one ([8.5](#85-artifacts-and-verification)).

**Built in M2.3, used since M2.7a.** NodeConfig, the rendering of the Nomad configuration, the spec hash and the user
data exist ([8.3](#83-nodeconfig-contract), [8.4](#84-nomad-configuration-rendering),
[ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md)). Until M2.7a `update` did not call them: nodes
booted the placeholder, carried no `tent/spec-hash` label and got no secrets. The next blocks say what M2.7a added.

**Built in M2.4, used since M2.7a.** `internal/nomadops` holds the calls that the Nomad step and the clients need, and
`nomadfake` stands in for Nomad in the app's tests ([15](#15-testing)).
- **Calls.** `nomadops.API` has `Leader`, `Bootstrap` ([9.2](#92-acl-and-tokens)), `IntroToken`
  ([9.3](#93-client-introduction)), `CreateToken` (M2.8, [9.2](#92-acl-and-tokens)), `Nodes`, `Peers` (M2.7b) and
  `Health`. A `Client` talks to one server
  ([9.7](#97-operator-access)); the caller moves to the next server when a call fails with `ErrNotReady`. Make one
  client per server and reuse it: its idle connections stay open for 90 seconds, and a server takes at most 100 HTTP
  connections from one address.
- **Deadlines.** Each call has 30 seconds, and none is retried. The caller's context can end it sooner.
- **Errors.** A call that may succeed later, on this server or another one, fails with an error that matches
  `ErrNotReady`: no answer, an answer broken off, no answer within 30 seconds, a 5xx, a 429, or no leader. For a
  write the outcome is then unknown. When the caller's context ends, the error matches the context's error instead.
  TLS failures and every other 4xx are permanent. Messages read like `nomad: PUT /v1/acl/bootstrap: 400: …`.
- **Answers.** `Leader` takes an empty leader as no leader. `Health` takes the 429 of an unhealthy cluster as a
  report, not an error, and returns whether the servers are healthy, how many vote and, since M2.8, each server of the
  report as a `ServerHealth`: its name (`<name>.<region>`), its Raft address, Serf's status of it, whether it is
  healthy, votes and leads, and its Nomad version. Nomad changes the order of the servers between calls, so a caller
  finds a server by address or name. `Nodes` reads `/v1/nodes`
  itself, skips `null` elements and keeps the server's order: the API module's `Nodes().List` panics on a `null`.
  A name can appear twice, for a node that went down and its replacement. Each node carries its address, the host of
  the HTTP address that the client advertises: with tent's configuration, the node's private address
  ([platform notes §1.2](platform-notes.md#12-features-tent-relies-on)), and the Nomad version that it runs.
  `Node.Is(name, addr)` tells a node by both.
  `Peers` (`GET /v1/operator/raft/configuration`) returns each server of the Raft configuration with its name, its
  Raft address and whether it votes. Nomad answers it only to a management token, and names a server `(unknown)`
  when no Serf member has its Raft address ([platform notes §1.2](platform-notes.md#12-features-tent-relies-on)).
  Unlike the autopilot report, which `Health` reads, the configuration has no lag.
- **Waits.** `WaitLeader`, `WaitNode` and `WaitHealthy` wait 2 seconds after each call and try again only after
  `ErrNotReady`. When the context ends, the error says what they waited for and the last cause, and does not match
  `ErrNotReady`. `WaitNode` takes a name and an address and ends when a node of that name that advertises that
  address is ready and eligible; a node of the name at another address, such as a twin or the node of an earlier
  machine, and a `down` node do not end it. An invalid address matches no node. `WaitHealthy` needs healthy servers
  and at least the given number of voters.
- **Servers** (M2.7a, `nomadops.NewServers`). It makes the servers of a cluster one `API`.
  - A call goes to the server that answered last, the first one at the start. After an error that matches
    `ErrNotReady` it goes to the next server, each server once per call, wrapping around the list. Any other error is
    returned as it is: it is permanent, or the caller's context ended during the call, such as `nomad: PUT
    /v1/acl/bootstrap: context canceled`. When the context has ended before a server is tried, the error is the
    context's own, without a `nomad:` prefix.
  - When no server answers, the error matches `ErrNotReady` and names each server by the address of its HTTP API with
    its cause, in the order tried: `nomad: no server is ready: 198.51.100.1:4646: <cause>; 198.51.100.2:4646:
    <cause>`. So the waits keep polling over `Servers`, and the timeout error of a wait shows the last causes: `nomad:
    wait for a leader: context deadline exceeded; last: nomad: no server is ready: …`.
  - `NewServers` fails with `nomad: no servers` or `nomad: server <address> has no API`. It is safe for concurrent
    use, and printing it with any verb shows the addresses alone.
- **`Servers.Last()`** (M2.8) is the address of the server that answered the last call that returned no error, which
  the next call asks first; before any such call it is the first server's. `export nomad` prints it as `NOMAD_ADDR`.
  `Servers.CreateToken` moves on after `ErrNotReady` as the other writes do, so a lost answer leaves a token that
  nobody holds, one per server at most.
- **`CreateToken`** (M2.8) sends one `PUT /v1/acl/token` with the name, `Type: "management"` and the TTL as a Go
  duration (`24h0m0s`); the token is not global. Before any request it fails with `nomad: ACL token: no name` or
  `nomad: ACL token: TTL 0s is not above zero`. An answer without a secret is the permanent error `nomad: PUT
  /v1/acl/token: the answer holds no secret`, and one without an end `... the answer holds no end`. The client's own
  token must be a management token. A `Token` prints without its secret.
- **`nomadops.NewProxy`** (M2.8) is the handler of `tent ui` ([9.7](#97-operator-access)). It shares the checks and the
  TLS settings of `New`.
- **`nomadfake`** logs the server that each call reached (`Call.Server`, the `Address` of the `Config` that made the
  client), so a test can show that the app moved to the next server. It lists nodes with an address and a version,
  serves the peers that a test sets (`SetPeers`) and the servers of the health report, and fails `Peers` before the
  bootstrap as Nomad's 403 does. Since M2.8 `CreateToken` gives a new accessor and secret for each call, ends the token
  at its clock plus the TTL, answers a TTL under a minute or over 24 hours with Nomad's text, fails with `permission
  denied` before the bootstrap, and lists each token it issued, also one whose answer was lost, by name, TTL and
  accessor and never by secret (`Issued()`).

**Built in M2.7a.** The flow of `update` with Nomad ([ADR-0031](adr/0031-bootstrap-in-update.md)). It is in
`internal/app` and reaches Nomad only through `internal/nomadops`.
- **The order of node changes.** The waits for server and combined nodes run first, by name; then their creates, by
  group, then index; then the waits of client nodes, the deletes of clients that never registered (M2.7b), and the
  creates of client nodes, in the same order; the other deletes come last, by name, then ID. A wait is ranked by the
  machine's own role label: every role other than client, an empty one included, counts with the servers. The reason
  for the order is in [11.2](#112-server-discovery-seed-and-refresh).
- **Node configs.** A plan that creates a node, or waits for one with an operation id, asks the provider for the
  architecture of each distinct machine type of the groups (`Provider.Arch`, [8.6](#86-operating-systems)) and makes
  one node builder
  ([8.3](#83-nodeconfig-contract)) with the run's asset cache, so both plans of one `update --yes` read Nomad's
  release files once. Each such change carries the spec hash of its group, which becomes the `tent/spec-hash`
  label. Any other plan reads no release file.
- **The size check.** The plan builds every planned node's config with a certificate issued for the check, a seed as
  long as the server and combined groups (the last addresses of the cluster CIDR, the longest text) and, for a client,
  a stand-in intro token of 2048 bytes that gzip shrinks no more than a real one (base64 text of random bytes; a real
  token is about 750 bytes, [platform notes §1.6](platform-notes.md#16-the-agent-on-a-node)).
  - `nodeconfig.UserData` then fails as it does at the create, so a group that does not fit fails the plan, also
    without `--yes` and before any write, such as `user data: node group workers needs 25012 bytes, more than the 24576
    that fit`.
  - The check's certificate differs by a few bytes from the one the apply issues, so a group within those bytes of the
    limit can pass the plan and fail at its create.
- **Warnings.** `devVariablesWarning` ([8.5](#85-artifacts-and-verification)) joins the other warnings, before the
  first change of `update --yes`. A development build without `TENT_NODE_URL` and `TENT_NODE_SHA256` fails a plan that
  creates nodes: `find tent-node: tent dev is a development build, so no release holds its tent-node: set TENT_NODE_URL
  and TENT_NODE_SHA256 to a tent-node built from the same commit`.
- **Secrets and certificates.** The run holds the CA, the gossip key and the bootstrap secret, stored or new. It issues
  each node's certificate with `ca.IssueNode` and one operator certificate with `ca.IssueOperator`, ([9.1](#91-pki),
  [9.7](#97-operator-access)). A plan made before the
  CA is stored issues a new CA in memory each time, so its spec hashes differ from plan to plan until the first
  `update --yes` has stored the CA.
- **Servers.** For each server or combined change, in plan order: the seed is the private addresses of every other known
  server, by name; the certificate; the config from the builder without an intro token; `nodeconfig.UserData`; then
  `Nodes.Create` with the spec hash, the user data and a new operation id, or, for a wait, the machine's own. The
  machine it returns joins the known servers. The first server of a cluster without servers gets an empty seed. A
  create or a wait that has no user data to boot with fails with `node <name>: no user data` before any cloud call. When
  servers are listed and none has a private address, the change fails, such as `node prod-servers-1: no server of
  cluster prod has a private address yet (prod-servers-0); run the command again`.
- **The Nomad step.** It runs when the plan has one.
  - **When it is planned.** A plan has it with the bootstrap when the store has no mark, or when no server or
    combined machine of the cluster stays (a cluster whose machines are all gone gets new servers and a new
    bootstrap, although the mark exists). It has it also when no node change is left, so a run cut before the servers
    were healthy is finished by the next run. With the mark and servers that stay, a plan that creates or waits for a
    server or combined node has it without the bootstrap, and any other plan has none. Its size is that of the server
    or combined group.
  - **The mark** is read with `Store.Get` of `nomad/bootstrapped` ([10.2](#102-layout)); only its existence counts.
    When the plan bootstraps and the store holds a stale mark, the apply deletes it after the infrastructure's task
    changes and before the first node change, so a run cut anywhere in the rebuild is finished by the next run with
    the bootstrap. Neither the delete nor the put of the mark sends a progress event.
  - **No Nomad client.** The service's Nomad factory must be set when the plan has a Nomad step, a client create or
    wait, or a node delete. Without it the apply fails at its start with `no Nomad client is set up`, before it raises
    the tent version or writes anything. A plan without `--yes` fails only when it must ask Nomad about a client that
    may never have registered ([13.4](#134-scaling)).
  - **The calls.** One `nomadops` client per known server with a public address, at `<public IP>:4646`, with the
    region, the CA bundle, the operator certificate and the bootstrap secret as the token, over `nomadops.Servers`. A
    cluster with no such server fails with `cluster prod: no server has a public address`. Then `WaitLeader`; with the
    bootstrap, `Bootstrap`; `WaitHealthy` for the servers; the scrubs of the servers (the block of M2.7b below); with
    the bootstrap, the put of the mark. Each wait and the bootstrap send progress events ([14](#14-cli)); the put of
    the mark sends none.
  - **Errors and cuts.** The texts are `bootstrap the ACL system: <error>`, for the put of the mark `write <path>:
    <error>`, such as `write prod/nomad/bootstrapped: <error>`, and for the delete of a stale mark `delete <path>:
    <error>`, which stops the run before any node is created; the next run tries again. The mark comes last in the Nomad
    step, so a run cut before it is finished by the next run with the leader wait, the bootstrap (safe to repeat) and
    the health wait; the labels that the cut run set stay, and a server without one is waited for and scrubbed. A
    cut between the last label and the mark leaves a run that repeats the leader wait, the bootstrap and the health
    wait, scrubs nothing and writes the mark. A lost `Bootstrap`
    answer ends with the cluster bootstrapped, and the next server finds
    it done ([9.2](#92-acl-and-tokens)); with one server the call fails with `ErrNotReady` and the next run bootstraps
    again, finds it done and writes the mark. A cut after the put goes straight to the clients.
  - **Deadlines and tries.** `Bootstrap` and `IntroToken` are tried once on each server and not repeated in a loop.
    When the leader wait ends at its deadline, its error adds `; tent reaches the servers on port 4646: check
    spec.access.api`, since an operator outside `access.api` gets no answer.
- **Clients.** For each client change, in plan order: `IntroToken` for the node's name, its group's pool and
  `nomadops.MaxIntroTTL`, a failure being `intro token for node prod-workers-0: <error>`; the config from the builder
  with the token and a seed of every known server; `Nodes.Create` as for a server; then `WaitNode` until Nomad lists a
  node of its name at its private address, ready and eligible, and then the scrub (M2.7b below). A client registered 21
  to 24 s after a fresh server's leadership on the M2.6b VM checks (an inference from one combined node). A lost answer
  of `IntroToken` is harmless: the run finishes, and the node's user data holds a token for its name.
- **What M2.7a leaves.**
  - **A renamed server group.** The machines of the server or combined group that stay are counted ready or not. A
    group renamed in the specs has none that stay, so `update` deletes the mark, builds the new group as a new Nomad
    with its own bootstrap and deletes the old machines last. Since M2.7b that works only for machines that carry no
    joined label; once they do, `update` refuses the delete ([13.4](#134-scaling)).
  - **Clusters built by an older tent** have placeholder nodes and no mark: the plan shows the Nomad step, and `--yes`
    fails after 10 minutes without a leader. Delete such a cluster and create it again.

**Built in M2.7b.** The scrub, the replacement of a client that never registered and the delete guard
([ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)). They are in `internal/app` and `internal/cloud`.
- **The joined label.** `tent/joined=true` ([3.4](#34-naming-and-ownership-markers)) says that a machine's node has
  joined and its user data is scrubbed. `Nodes.MarkJoined` sets it ([7.1](#71-interfaces), [11.6](#116-user_data)).
  `Nodes.List` reports it as `Instance.Joined`, so a plan sees which machines still have to join without a call to
  Nomad.
- **Who is scrubbed, and when.**
  - **Servers**, in the Nomad step, after the servers are healthy and vote. `update` reads the Raft configuration once,
    and then, for each server or combined change of the plan in order, checks that a voter sits at the private
    address of the machine, and scrubs it. A combined node must have registered first. A plan with no server or
    combined change reads no configuration.
  - **Clients**, one by one, after the wait until the node has registered.
  - A machine's node is told by its name and its private address together
    ([ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)).
  - The bootstrap mark is written after the last scrub.
- **The scrub's deadline** is 5 minutes for the read and the update. A failure stops the run with the provider's
  error. The next plan waits for the machine again, since it has no label.
- **Waits.** A plan waits for every machine that stays and carries no joined label, whatever its role. The wait keeps
  its operation id only when the cloud reports the machine not ready and the id is valid; the apply then repeats the
  create, with the node's user data. A wait without an operation id has neither `op` nor `specHash` in the JSON plan,
  calls no cloud before the scrub, asks for no intro token and reads no release file.
  - So a plan whose only node change is the wait of a client needs the Nomad factory, and `update --exit-code` exits
    with 2 until every machine carries the label.
  - A cluster built by M2.7a's tent has no labels. Its first `update --yes` waits for every node, scrubs it and labels
    it, and creates nothing.
  - A client that the cloud reports ready and that never registers is waited for 10 minutes in every run, until the
    plan replaces it ([13.4](#134-scaling)).
- **Errors.**
  - `node <name>: the servers are healthy with <n> voters, but none votes at its address <ip>` (`1 voter` for one);
  - `read the Raft configuration: <cause>`;
  - `node <name>: the cloud reports no private address for it yet; run the command again`, when a registration wait
    or a vote check meets a machine without an address.
- **Calls.** On the fakes, a build of three servers and two clients makes one `Peers` call, then one `GetInstance` and
  one `UpdateInstance` per server; each client's `Nodes` wait is followed by its own pair
  (`internal/app/testdata/flow_build.calls.golden`). A second `update --yes` plans nothing, makes no Nomad call and
  only reads the cloud.

**Target, with Nomad.** The whole flow:

```
 1. lock → load specs → defaults → validate (+ live: types, regions/locations, images, availability)
 2. ensure secrets (idempotent): CA, gossip key, ACL bootstrap token; write cluster.completed.yaml before the first node
 3. model → provider.BuildInfra → engine plan → print → apply without the deletes
    (SSH keys, network, firewalls, [placement groups], [LB]; tasks that do not depend on each other apply in parallel)
 4. servers first: create missing servers
    (Hetzner: into slots; Vultr: server-0 first, then the rest seeded with existing server IPs)
 5. wait for a leader → ACL bootstrap with the pre-generated secret
 6. day-1 over the API: cluster settings; no node pools, which Nomad creates when their first client registers
 7. clients: for each missing node → intro token → Nodes.Create (seeded with current server IPs) → wait ready
 8. scrub the user data of nodes that joined and label them (Nodes.MarkJoined)
 9. scale down surplus nodes: drain → stop/delete → purge
10. apply the plan's deletes, the prune and the duplicates (a second engine pass)
11. validate → write the history → unlock
12. report: "N nodes are out of date (reason: config diff) → run tent rolling-update cluster"
```

- Steps 2, 4, 5, 7 and 8 are built (M2.1, M2.7a and M2.7b, above), as the flow above runs them: the servers are
  scrubbed after they are healthy and before the mark is written, and each client is scrubbed right after it
  registers. The day-1 configuration (step 6, without node pools: decision
  17 of [18](#18-open-questions)) is not built.
- The drain and the purge (step 9), the history (step 11) and the report of outdated nodes (step 12) are not built
  yet. `tent validate cluster` is built ([13.6](#136-tent-validate-cluster---wait-duration)), but `update` does not call
  it at its end: that part of step 11 stays a target.

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
→ stop the old server: ACPI shutdown where GracefulShutdown, otherwise hard stop/DELETE (Vultr); the server
  does not leave Raft either way (leave_on_terminate is false on servers, decision 26)
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
  M3 there is no drain, so `update` deletes only machines that never joined: a delete of a machine that carries the
  joined label fails the plan ([the guard](#the-delete-guard) below).
- **Zones.** A node in a zone that its group no longer lists counts toward the group's size and stays. New nodes go
  only into the listed zones.
- **Duplicates.** Of instances with one name, one that carries the joined label stays before one that does not, then
  one that is not a client that never registered (below) before one that is, then the oldest. The others are deleted
  as `duplicate`. A twin that registered, which carries no label yet, is labelled by the guard and stays at the next
  plan.
- **Groups not in the spec.** An instance whose group label names no group of the spec, or is empty, is deleted as
  `not in the spec`.
- **Nodes that have not joined.** A machine that stays and carries no joined label, left by an interrupted run,
  counts, and `update` waits for it ([13.2](#132-tent-update-cluster---yes)). When the cloud reports it not ready and
  its operation id is valid, the wait calls `Create` with that id. A machine that is ready, or has no valid operation
  id, is waited for until its node joins, and the wait calls no cloud until the scrub.

#### A client that never registered

A client that has not registered before its intro token expires is deleted and created again, with a new token
([ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md), decision 28 of [18](#18-open-questions)). It never ran a
workload, so nothing is drained.
- **Which machines.** The plan judges every machine of the cluster whose role label says client, that carries no
  joined label and is older than 31 minutes by the cloud's creation time: the token's 30 minutes (`MaxIntroTTL`) and
  the minute that a server still accepts it after that (`IntroLeeway`,
  [9.3](#93-client-introduction)). A machine without a creation time is never old.
- **The question.** The plan asks Nomad once, with one `Nodes` call through the servers that stay and carry the joined
  label. It asks only when such a server exists. Without one the cluster is still bootstrapping or was built by M2.7a's
  tent, and the clients are waited for. A machine whose name and private address Nomad lists as a node that is not
  `down` has registered. The others have not: after 31 minutes this holds also for a client that the cloud still
  reports not ready and for one without a private address.
- **The plan.** A machine that never registered and would stay under its name is deleted with the reason
  `not registered`. It does not count toward its group and frees its name, so a create takes the name unless the group
  is then full: with a group scaled from 2 to 1, two such clients are both deleted as `not registered` and one is
  created. The delete comes before the create, which departs from the rule that a replacement is created before the
  old node is removed: the old machine never served, a second machine of its name would be a duplicate that the next
  plan has to sort out, and a cut after the delete leaves a plain create. The waits of the clients come before the
  delete, so a run that fails at a wait does not reach it. On Vultr the create follows the delete within seconds, and
  the account's instance limit may still count the deleted machine, so the provider sends a refused create again
  ([11.3](#113-creating-a-node)).
  ```
  - node prod-workers-1 (ID instance-5, not registered)
  + node prod-workers-1 (client, vc2-2c-4gb, ams)

  Nodes: 1 to create, 0 to wait for, 1 to delete.
  ```
- **A plan that cannot ask fails**, with and without `--yes` and with `--exit-code`, before it changes anything. It
  needs the Nomad factory, the servers' public addresses and a reachable API, so `access.api` must let the operator
  in. The error names each machine with its ID, since a machine of the same name may have joined: `node prod-workers-1
  (ID instance-5) did not join within 31 minutes of its creation, and tent could not ask Nomad whether it
  registered: <cause>`, and for several `nodes prod-workers-0 (ID instance-4) and prod-workers-1 (ID instance-5) did
  not join within 31 minutes of their creation, and tent could not ask Nomad whether they registered: <cause>`.
- **The clock.** The age is the local clock against the cloud's creation time. The delete asks Nomad again.
- **Calls.** The apply of a plan that replaces one client makes these Nomad calls: `Nodes` (the plan), `Nodes` (the
  plan under the lock), `Nodes` and `Peers` (the guard), `IntroToken`, and `Nodes` (the registration wait).

#### The delete guard

Until M3 `update` deletes only nodes that never joined (decision 27 of [18](#18-open-questions)).
- **The plan refuses.** A plan that deletes a machine that carries the joined label fails with one error that names
  each such machine, its ID and the reason of its delete: `not in the spec`, `surplus` or `duplicate of ID <id>`, the
  machine of that name that the plan does not delete as a duplicate or as not in the spec. It fails with and without
  `--yes` and with `--exit-code`, before any change, with exit code 1:
  `update would delete a node that joined Nomad: prod-workers-2 (ID instance-6, surplus); tent cannot drain a node or
  remove a server yet, so update deletes only nodes that never joined; keep this node in the specs, or delete the
  whole cluster with tent delete cluster`. With several, the text reads `nodes that joined Nomad: A, B and C`
  and `keep these nodes in the specs`. For a duplicate it says `remove one of the two machines called <name> from
  Nomad and delete it in the cloud`, and for several duplicates `of the machines that share a name, remove one from
  Nomad and delete it in the cloud`. Advice for different reasons is joined with `, and `.
- **The apply asks Nomad** before each delete, whatever its reason, since a machine may have joined without a label:
  - a client of the machine's name and private address that is not `down` (`Nodes`), or
  - a server of the Raft configuration at that address, voter or not (`Peers`).

  A machine that the cloud lists without a private address counts as not joined, and no call is made. A machine found
  joined is scrubbed and labelled, and the step fails with `delete node <name> (<id>): the node has joined Nomad (a
  registered client at <ip>)` or `(a server at <ip>:4647)`, followed by `; tent cannot drain a node or remove a server
  yet, so update deletes only nodes that never joined`. The next plan then refuses the delete with the plan's error.
  When Nomad cannot be asked, nothing is deleted: `delete node <name> (<id>): ask Nomad whether the node joined:
  <error>`. A delete, then, needs the Nomad factory.

**Target, with Nomad.**
- **Scale up** is part of `update`.
- **Scale down** is also part of `update`. It is always shown in the plan and always drains first. Victims are
  chosen in this order:
  1. outdated nodes;
  2. nodes in the most loaded zone;
  3. the newest nodes.
- **Server group size** can change 1 → 3 → 5 and back, one server at a time, never below quorum.

### 13.5 `tent upgrade cluster [--yes]`

**Channels, built in M2.2** ([ADR-0026](adr/0026-channels-and-release-assets.md)). tent embeds its channels in
`internal/channels`, one YAML file each, decoded strictly. `stable` is the only one. Its file on 2026-09-28:

```yaml
name: stable
nomad:
  minimum: 2.0.0        # the oldest version a cluster may run
  recommended: 2.0.7    # what a new cluster runs
  tested: [2.0.7]       # the versions tested with this tent
cni:
  version: 1.9.1
  sha256:               # of cni-plugins-linux-<arch>-v1.9.1.tgz
    amd64: <sha256>
    arm64: <sha256>
```

- A channel holds Nomad and the CNI plugins only. Images stay in the provider's table
  ([ADR-0023](adr/0023-vultr-inventory-dedupe-and-images.md)) and the API default (decision 4 of
  [18](#18-open-questions)).
- A cluster may run any official Nomad release from the channel's minimum up to, not including, the next major
  version: 2.0.0 up to 3.0.0 for `stable`. So a new Nomad patch needs no tent release. A version that the channel does
  not list as tested gets a warning ([14](#14-cli)). tent does not check that the version was released: its signed
  `SHA256SUMS` proves that when tent fetches the assets ([8.5](#85-artifacts-and-verification)).
- This relaxes the rule planned here, that only channel-tested version pairs are allowed. Nomad's 2.x version skew
  policy has still not been restated ([platform notes §1.3](platform-notes.md#13-upgrades-and-version-skew)).

**`upgrade cluster`, target.**
- Reads the cluster's channel. A URL can override the embedded file.
- The command proposes upgrades, also for a cluster whose pinned Nomad version
  ([13.2](#132-tent-update-cluster---yes)) the channel no longer allows, rewrites the spec and prints the plan. Then
  run `update` and `rolling-update`.
- It never downgrades, and neither does `rolling-update`.
- **Later:** an opt-in in-place upgrade strategy (swap the binary and restart).

### 13.6 `tent validate cluster [--wait DURATION]`

**Built in M2.8** ([ADR-0033](adr/0033-operator-commands.md)): `tent validate cluster [NAME] [--wait DURATION]
[--allow-single-server]`. It compares the machines and Nomad with the specs, prints what differs, and exits with 2
while something does. It is `Service.ValidateCluster` in `internal/app`.

- **Reads only.** It reads the state store, the cloud and Nomad, writes nothing and takes no lock.
  - The state store: the specs, loaded and validated as `update` loads them (`loadCluster`: defaults, `checkCluster`,
    the pinned Nomad version, the model), so a single server needs `--allow-single-server`; the four secrets; the
    mark `nomad/bootstrapped`; and who holds the cluster's lock (`statestore.Holder`,
    which reads the lease without choosing a lock mechanism: no `Capabilities` probe and no write, and on a store that
    is no file store an expired lease reads as a free lock).
  - The cloud: one `Nodes.List`. No inventory, no preflight, no release file.
  - Nomad: one round is `Leader`, `Peers`, `Health` and `Nodes`, once each and in this order, through every server or
    combined machine that stays and has a public address, with tent's own access (an in-memory operator certificate and
    the bootstrap secret, as `update` has).
- **The machines** are judged by the node planner of `update` ([13.4](#134-scaling)) over the cluster's machines, with
  no machine that never registered: each node change that `update` would plan is a failure, and so is a machine that
  stays and that the cloud does not report as running or that has no joined label. So what `update` would change and
  what `validate` reports cannot differ. `validate` runs the node planner, not the whole plan of `update`
  ([ADR-0033](adr/0033-operator-commands.md)).
- **The failures.** A failure has a check, a node (left out for a failure of the whole cluster, shown as `-`), the
  machine's ID and a detail. The check names are in the JSON, so they stay.

  | Check | What it compares | Detail |
  |---|---|---|
  | `machine-missing` | a group's machines with its size | `no machine: node group workers has 1 of its 2` |
  | `machine-not-running` | `Instance.Ready` of a machine that stays | `the cloud reports its machine (ID instance-4) as not running` |
  | `machine-surplus` | a group's machines with its size | `machine ID instance-6 is one more than the size of node group workers, 2` |
  | `machine-duplicate` | the names of the machines | `machine ID instance-9 has the name of machine ID instance-4` |
  | `machine-unknown` | a machine's group label with the specs | `machine ID instance-7 is of node group old, which the specs do not have`, or `machine ID instance-7 has no node group label` |
  | `not-joined` | the `tent/joined` label of a machine that stays | `machine ID instance-3 has not joined Nomad: it carries no tent/joined label` |
  | `nomad-not-set-up` | the secrets, the mark, a server's public address | `the state store lacks <paths>`, `Nomad is not bootstrapped yet: tent update cluster --yes bootstraps it`, or `no server has a public address`, checked in this order; Nomad is not asked |
  | `nomad-no-leader` | the first call of the round that fails | `Nomad has no leader, or tent cannot reach it: <error>; tent reaches the servers on port 4646: check spec.access.api`; the error of `Peers`, `Health` or `Nodes` is also this check, with the step in front of it (`read the Raft configuration: `, `read autopilot's health: `, `list the nodes: `); the rest of the round is skipped |
  | `server-no-vote` | each expected server with the Raft configuration | `no server votes at its address 10.64.0.4`, `its server at 10.64.0.5:4647 has no vote`, or `the cloud reports no private address for it` |
  | `server-unknown` | each Raft peer with the expected servers | `the Raft configuration lists a server at 10.64.0.9:4647 (prod-servers-9.global) that is no server machine of the cluster`, or `a server named <name> with no usable address, which is no server machine of the cluster` |
  | `autopilot-unhealthy` | `Health.Healthy` | `autopilot reports the servers unhealthy` |
  | `server-not-alive` | each expected server's entry in autopilot's report, found by private address | `Serf reports its server as left` (the status Nomad gave), or `autopilot does not list its server` |
  | `server-unhealthy` | the same entry | `autopilot reports its server unhealthy` |
  | `client-not-registered` | each expected client with `Nodes`, told by name and private address | `Nomad lists no client of its name at 10.64.0.7`, or `the cloud reports no private address for it` |
  | `client-not-ready` | that node's status and eligibility | `its Nomad client is down`, `its Nomad client is initializing` or `its Nomad client is ready but not eligible` |
  | `nomad-version` | each server's and each client's version with the pinned one, as text | `its client runs Nomad 2.0.6; the cluster is pinned to 2.0.7` (`its server runs …` for a server); a server that the report lacks and a client that Nomad does not list or lists as `down` are not compared |
  | `certificate-expired` | the CA's end, and each machine's creation time plus a year, with now | `its node certificate ended about 2027-10-05`, or for the CA, with no node, `the cluster CA ended on 2036-10-05` |

  - The expected servers are the machines that stay in the server and combined groups, the expected clients those in
    the client and combined groups. Of several nodes of one name at one address, a ready one passes, as `WaitNode`
    counts ([ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)).
  - A stopped machine that has not joined shows two failures. The failures come in the order of the table, then by node
    name, then by ID, whatever the order of the machines.
  - A node that joined and later died keeps its label. It shows as `client-not-ready` or `client-not-registered`, as
    `server-no-vote` once autopilot has removed the server, and as `machine-not-running` or `machine-missing` when the
    cloud knows.
  - **Nomad that does not answer is a failure** (`nomad-no-leader`), so `--wait` can wait for a cluster that is still
    starting. A cloud that does not answer is an error.
  - **A server that died seconds ago still reads as valid.** On Nomad 2.0.7 the autopilot report shows a killed server
    as `alive` for about 36 s, until Serf marks it failed, and the Raft configuration keeps its vote until autopilot
    removes it (47.8 s in the run of 2026-10-05). A later run, or `--wait`, finds it
    ([platform notes §1.2](platform-notes.md#12-features-tent-relies-on)).
  - **Certificates** are checked for the machines that stay and for the CA when the store holds all four secrets, also
    when Nomad cannot be asked. A certificate that ends within 30 days is a warning (below), one that has ended a
    failure. A machine without a creation time is not checked. The end of a node certificate is an estimate
    ([9.1](#91-pki)).
- **The warnings** go to stderr as `WARNING:` lines ([14](#14-cli)), once per command, and never change the exit code.
  They are those of every change (an open `access.api`, a combined group, a Nomad version that the channel has not
  tested), then:
  - `cluster prod runs in one failure domain, ams: an outage there takes the whole cluster down`, when the model has one
    zone, which is always so on Vultr. The model gives the zones: `cloud.Capabilities` does not exist yet, and the
    warning that a cluster has no host anti-affinity waits for it ([ADR-0018](adr/0018-vultr-provider-design.md));
  - `the certificate of node prod-workers-0 ends about 2026-11-01, in 26 days; node certificates last one year, and a
    node gets a new one when it is replaced`, by node name, and `the cluster CA ends on 2036-10-05, in 29 days; tent
    cannot renew a CA yet` (`in 1 day` and `in less than a day` for the last day);
  - `tent could not read the lock of cluster prod: <error>`, when the lease of the lock cannot be read.
- **Output.**
  - A notice on stderr, in every output format, when the cluster is locked: `cluster prod is locked by igor@laptop (pid
    4242) for update since 2026-10-06 10:00:00 UTC`, or `an unknown holder` when the holder cannot be named. A lock is
    no failure, and with `--wait` the notice is that of the last round.
  - `-o table`, valid: `cluster prod is valid: 3 servers and 2 clients run Nomad 2.0.7`. The counts are the machines
    that stay under the specs, by their group's role, a combined machine as a server and a client.
  - `-o table`, not valid: a table with the columns `NODE` and `FAILURE`, then a blank line and `cluster prod is not
    valid: 4 failures` (`1 failure`).
  - `-o json` and `-o yaml`: `cluster`, `valid`, `servers`, `clients`, `nomadVersion`, `failures` (each with `check`,
    `node`, `id` and `detail`; `node` and `id` are left out for a failure of the whole cluster), `warnings` and `lock`
    (the lease, left out when the lock is free). `failures` and `warnings` are lists even when empty.
- **`--wait DURATION`.** Without it, one round. With it, a round every 10 seconds until the cluster is valid or the
  duration has passed, the last sleep cut to the time left; the result of the last round is printed. Before the first
  sleep, one line on stderr: `cluster prod is not valid yet (3 failures); checking every 10s for up to 10m0s`. A
  negative duration is `invalid --wait -1m0s: must not be negative`. A round that could not check ends the wait with
  exit code 1 and no result. Ctrl-C during a wait gives `Error: interrupted` and exit code 1.
- **Exit codes.** 0 when the cluster is valid. 2 when it is not, with the table and no `Error:` line. 1 when tent could
  not check: an invalid spec, a stored secret that does not load, a cloud call that failed (`VULTR_API_KEY is not
  set`), `no Nomad client is set up`, a result that cannot be written, or Ctrl-C.
- **Needs** the cloud's credentials in the environment (`VULTR_API_KEY` for Vultr) and a way to port 4646 of the
  servers, which `spec.access.api` allows.
- **Not checked.** The infrastructure (firewalls and the network; `update --exit-code` reports them), a node's spec
  hash (`rolling-update` will), and the `drain_on_shutdown` of `extraConfig` ([ADR-0030](adr/0030-nomad-on-nodes.md)).

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
- **Nodes that joined.** `delete cluster` deletes machines that carry the joined label as it deletes the others. The
  guard of `update` ([13.4](#134-scaling)) does not apply, and the command calls no Nomad.
- **State.** The state is `tent-version`, the specs, the completed spec, since M2.1 the four secrets: the CA's key and
  bundle, the gossip key and the ACL bootstrap secret, and since M2.7a the bootstrap mark `nomad/bootstrapped`
  ([10.2](#102-layout)). The mark goes with the first objects, after the node group specs and before the secrets.
  Other objects under the cluster in the store, such as another object under `pki/`, make tent refuse before it calls
  the cloud, unless `--force` is given; then they are deleted with the rest. The lock's lease goes when the lock is
  released.
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

  - The store of a cluster that `update` has bootstrapped also holds the mark. The plan then lists
    `- state prod/nomad/bootstrapped` after the node group specs, and the state count is one higher.
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
`update cluster` and `delete cluster` of M1 reach the cloud, and `validate cluster`, `export nomad` and `ui` of M2.8
reach the cloud and the Nomad API. The other commands come with later milestones ([roadmap](roadmap.md)).

| Command | kops analogue | Purpose | Built |
|---|---|---|---|
| `tent create cluster [NAME] [flags] [--yes]`, `tent create -f FILE [--yes]` | `create cluster` | generate or load specs into the state store; with `--yes` also build the cluster ([13.1](#131-tent-create-cluster)) | M0; `--yes` in M1 |
| `tent get [NAME]`, `tent get clusters\|nodegroups [NAME...]`, all with `[--full]` | `get` | print a cluster's specs, list clusters or node groups | M0 |
| `tent get nodes` | `get instances` | list the cluster's nodes | — |
| `tent edit cluster [NAME]`, `tent edit nodegroup NAME` | `edit` | an editor, with validation and a diff before saving | M0 |
| `tent replace -f FILE` | `replace` | GitOps: replace stored specs with those of a file | M0 |
| `tent apply -f FILE` | — | `replace` + `update` | — |
| `tent update cluster [NAME] [--yes] [--exit-code]` | `update cluster` | infrastructure, node counts, the Nomad cluster: servers, ACL bootstrap, clients, the scrub of user data, the delete guard ([13.2](#132-tent-update-cluster---yes)) | M1; Nomad in M2.7a; scrub and guard in M2.7b |
| `tent rolling-update cluster [--yes] [--nodegroups a,b] [--force]` | `rolling-update cluster` | Nomad-aware replacement | — |
| `tent upgrade cluster [--yes]` | `upgrade cluster` | version bumps from the channel | — |
| `tent validate cluster [NAME] [--wait DURATION] [--allow-single-server]` | `validate cluster` | the machines and Nomad against the specs; exits with 2 while they differ ([13.6](#136-tent-validate-cluster---wait-duration)) | M2.8 |
| `tent delete cluster [NAME] [--yes] [--force]` | `delete cluster` | full cleanup by ownership markers ([13.7](#137-tent-delete-cluster---yes)) | M1 |
| `tent export nomad [NAME] [--ttl 24h] [--dir DIR] [--shell sh\|fish]` | `export kubeconfig --admin` | short-lived operator credentials in files, and the shell lines that use them ([9.7](#97-operator-access)) | M2.8 |
| `tent ui [NAME] [--listen 127.0.0.1:4646]` | — | local mTLS proxy for the UI and CLI ([9.7](#97-operator-access)) | M2.8 |
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
  Vultr: `update cluster`, `delete cluster`, `create --yes`, and since M2.8 `validate cluster`, `export nomad` and `ui`
  ([7.1](#71-interfaces)). The last three need it because tent stores no address of a machine: a server's public
  address, which the calls to port 4646 need, comes from the cloud's list. `HCLOUD_TOKEN` comes with the Hetzner
  provider.
- A development build of tent reads `TENT_NODE_URL` and `TENT_NODE_SHA256` once at start, for the tent-node that its
  nodes download ([8.5](#85-artifacts-and-verification)). A release build ignores them. The long help of
  `update cluster` names them.

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
  `--state` looks like an empty store. `--full` fills in the defaults ([3.3](#33-api-rules)). The `NOMAD` column of
  `tent get clusters` shows the version that the user spec sets, and `-` for a cluster that only has a pinned one
  ([13.2](#132-tent-update-cluster---yes)), since `get` does not read the completed spec yet.
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
  - Scrub lines (`update`), for each node that has joined: `scrubbing the user data of node prod-workers-0`,
    `scrubbed the user data of node prod-workers-0` and `failed to scrub the user data of node prod-workers-0:
    <error>`. They show on every cloud, also on one that cannot change user data. The line names the node and not the
    machine's ID. A delete that the guard stops shows a scrub started and done, then `deleting node prod-workers-2
    (ID instance-6)` and `failed to delete node prod-workers-2 (ID instance-6): <error>`.
  - Nomad lines (`update`), for each step started, done and failed: `waiting for a Nomad leader`, `Nomad has a leader
    (10.64.0.3:4647)`, `failed to wait for a Nomad leader: <error>`; `bootstrapping the ACL system`, `bootstrapped the
    ACL system`, `failed to bootstrap the ACL system: <error>`; `waiting for 3 healthy Nomad servers`, `3 Nomad
    servers are healthy`, `failed to wait for 3 healthy Nomad servers: <error>`; and, for each node that the run waits
    for, `waiting for node prod-workers-0 to register`, `node prod-workers-0 registered`, `failed to wait for node
    prod-workers-0 to register: <error>`. One server reads `1 healthy Nomad server` and `1 Nomad server is healthy`.
  - JSON: `{"type":"infrastructure","event":"started","kind":"vultr.VPC","name":"prod","action":"create"}` with
    `id`, `wait`, `cause` and `error` when they apply,
    `{"type":"node","step":"done","action":"create","name":"prod-servers-0","id":"<id>","address":"10.64.0.3"}` with
    `error` for a failed step, `{"type":"node","step":"done","action":"scrub","name":"prod-workers-0","id":"<id>"}`
    for a scrub, which has no `address`, `{"type":"wait","nodes":3}` for the wait of a delete, and
    `{"type":"nomad","step":"done","action":"leader","leader":"10.64.0.3:4647"}` for the Nomad step. Its `action` is
    `leader`, `bootstrap`, `healthy` or `register`; `name` names the node of a `register`, `voters` the servers of a
    `healthy` (the number waited for when it starts, the number that vote when it is done), and `error` a failed
    step.
  - With `-o json`, stderr mixes the JSON progress lines with plain `WARNING:` lines and the logs. A program reads
    the lines that start with `{`. The logs are text unless `--log-format json` makes them JSON objects too; they
    carry `level` and `msg`, which progress lines never have.
- `validate cluster`, `export nomad` and `ui` print one result on stdout and notices on stderr, and no progress lines
  ([13.6](#136-tent-validate-cluster---wait-duration), [9.7](#97-operator-access)). None of them prints a secret, in
  any output format, in a log or in an error. `export nomad` prints the six shell lines, or with `-o json` and `-o yaml`
  its paths, address and end; `ui` prints the URL, or with `-o json` and `-o yaml` the cluster, the URL and the end of
  the session.
- Then `-o table` prints a blank line and one line in the past tense on stdout: `Applied: …`, `Nodes: …`, `Nomad: …`
  and `Wrote …` (the objects written to the state store) for `update`, and `Deleted: …` for `delete`. `-o yaml` and
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
- tent warns about the cluster that results from a change: after `create`, `replace` or a saved `edit`, and before
  `update cluster --yes` applies changes. `validate cluster` warns on every run. A command prints each warning once,
  `create --yes` included. A plan without `--yes`, `delete cluster`, `get`, `state unlock`, `export nomad` and `ui` do
  not warn: they leave no changed cluster behind. It warns:
  - while `access.api` lets the whole internet reach the Nomad API, a `/0` range such as the default `0.0.0.0/0`;
  - for a combined node group: `WARNING: node group nodes is combined: its nodes run the Nomad servers and the
    workloads together, which is meant for development and small clusters; workloads share them with Raft and the
    gossip key` ([ADR-0019](adr/0019-combined-server-client-role.md));
  - when a release build has `TENT_NODE_URL` or `TENT_NODE_SHA256` set and so ignores it, only before
    `update cluster --yes` applies changes ([8.5](#85-artifacts-and-verification));
  - when the cluster's Nomad version is one that its channel allows but has not tested
    ([13.5](#135-tent-upgrade-cluster---yes)), such as `WARNING: Nomad 2.0.8 is not tested by this tent; channel
    stable tests 2.0.7`. `create`, `replace` and `edit` check the version that the spec sets, and `update` also a
    pinned one ([13.2](#132-tent-update-cluster---yes));
  - and `validate cluster` also warns about a cluster in one failure domain and about certificates that end within 30
    days ([13.6](#136-tent-validate-cluster---wait-duration)).
- An error goes to stderr after `Error: `. An invalid spec prints `Error: invalid spec:` and then one indented line
  per problem, with the field path ([3.3](#33-api-rules)). An `update cluster` that would delete a node that joined
  prints the one error of [13.4](#134-scaling) and exits with 1, with and without `--yes` and with `--exit-code`.
- The long help of `update cluster` says that tent replaces the user data of a node that has joined with a stub, that
  a client that did not register within 31 minutes of its creation is deleted and created again, and that an update
  that would delete a node that joined fails. The long help of `validate cluster`, `export nomad` and `ui` says what
  they check or write, what they print, and that they need the cloud's credentials and a way to port 4646 of the
  servers; the help of `validate cluster` also gives its exit codes.
- Exit codes: 0 success, 1 error, 2 when `update cluster --exit-code` finds a plan with changes or `validate cluster`
  finds the cluster not valid, and 130 when a second Ctrl-C or SIGTERM ends tent. Exit code 2 prints no error. `tent ui`
  ends with 0 at Ctrl-C and at the end of its session.

---

## 15. Testing

Details in [ADR-0012](adr/0012-testing-strategy.md). The E2E platform is chosen in
[ADR-0014](adr/0014-vultr-first-provider-and-e2e.md).

1. **Unit tests:**
   - defaults and validation;
   - PKI;
   - Nomad config rendering (golden HCL, parsed back with HCL1), the spec hash (a pinned canary) and the user data
     size (the largest config of each role);
   - the label codecs;
   - the address plan;
   - the engine: golden plans, apply with fake time (`testing/synctest`);
   - the Nomad API client: `httptest` servers with real mTLS from `internal/pki`, and the waits on `nomadfake` with
     fake time;
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
   - The fake Nomad API is `internal/nomadops/nomadfake` (M2.4). Its clients implement `nomadops.API` with the error
     classes of nomadops, so the app runs on it as on the real client.
     - A test sets the leader, registers nodes and sets the health, and reads the calls that reached the fake and
       the tokens its clients got.
     - Without a leader every call fails with `ErrNotReady`, as Nomad answers `No cluster leader`. With a leader,
       `Nodes`, `Health`, `IntroToken` and `Peers` fail before the first bootstrap with `nomadfake: <method>: permission
       denied`, which does not match `ErrNotReady`, as Nomad with ACLs refuses them. The first
       bootstrap stores its secret; the same secret again succeeds, and another one fails with
       `ErrBootstrapMismatch`. Intro tokens are unsigned JWTs that carry the node's name and pool.
     - Faults: a given error (`Fail`), or a lost answer after the fake carried the call out (`LoseResponse`), so a
       lost bootstrap leaves the ACL system bootstrapped.
     - `NewCluster` makes the cluster a new one, without leader, nodes, bootstrap or health, and keeps the calls, tokens
       and faults; `SetBootstrapped` makes it bootstrapped with a copy of a secret, without a call.
     - It is simpler than Nomad: it checks no ACL token.
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
       request in flight does. A cut build changes nothing in the state store but for the missing secrets, the completed
       spec and the bootstrap mark that it had written or deleted by then; a cut delete leaves the store as it was. The
       lock is free. After the next run, a build leaves the cloud as an uninterrupted build does, with one copy of every
       object, and a plan after it has no changes; a delete leaves no object with the cluster's markers and no state.
     - Lost answers: for each create call of the build, the fake carries the call out and the call gets no answer.
       The same run finds what the call made by its operation id, or lists the rules again, and ends with one copy
       of every object and one instance per node.
     - The fake's hook (`SetHook`) makes the cuts and the lost answers.
   - **Built in M2.7a** ([ADR-0031](adr/0031-bootstrap-in-update.md)): the same kind of tests for the whole flow with
     Nomad, on `vultrfake` and `nomadfake`.
     - **The fakes.**
       - `internal/assets/assetstest` serves HashiCorp's signed `SHA256SUMS` and `.sig` of Nomad 2.0.7 and the
         `checksums.txt` of every release of tent, with a line for `tent-node_linux_amd64` and `_arm64` (made-up
         sums), through an `http.RoundTripper` without a network. Every other URL is 404, so a test that pins another
         Nomad version gets 404. It gives the constants and the clock (2026-09-28 12:00 UTC, when the signature
         verifies with the embedded key) that callers need. The tests of `internal/app`,
         `internal/cli`, `internal/assets` and `hack/tent-node-userdata` share it. It imports only the standard library.
       - `nomadfake` logs the server that each call reached, and the tests of `nomadops.Servers` run on stub APIs
         and on it, with the waits in `testing/synctest`.
       - A test channel of the app copies the CNI plugins of the embedded `stable` channel, and tests run `update`
         as a release.
       - The Nomad of the flow tests is a wrapper of a `nomadfake` client that follows the Vultr fake: before each call
         it sets a leader once as many servers are ready as the server group has, the health from the listed servers,
         and each ready client or combined instance as registered. When none of the server machines it saw at the last
         call is listed any more, it makes the fake a new, unbootstrapped cluster. A hook, as on `vultrfake`, lets a
         test end the run's context before or after a named Nomad call.
     - **Golden files** (`internal/app/testdata/flow_build.*.golden` and the scale goldens) hold the plans, and the
       cloud calls with the Nomad calls in their place, as `nomad <Method> <arg> (<the server's node name>)`. The two
       logs merge by the cloud call count at each Nomad call.
     - **The flows.** A fresh cluster of three servers and two clients is built with the seeds, tokens, labels, spec
       hashes and user data that the tests name; so is a combined cluster of three nodes and one of a single node. A
       second `update --yes` plans nothing, makes no Nomad call and only reads the cloud. A call moves to the next
       server after a not-ready error. No leader for 10 minutes fails the leader wait after 10 minutes and creates no
       client. A client that never registers fails after its 10 minutes with the node named.
     - **Cuts.** The context of a run ends before and after each call of the golden, cloud and Nomad calls together.
       A second test fails each put of a secret, the completed spec and the mark, before it and after it with the
       answer lost. The cuts run on two clusters, three servers with two clients and a combined cluster of three, and
       a third test cuts the rebuild of each on an empty cloud from a store that holds the secrets and the mark. After
       the next run the cluster has one instance per node, a bootstrapped ACL system (one or two `Bootstrap` calls: a
       run stopped after the call, in the health wait, is followed by a call that finds it done) and no changes, the
       mark is written and the lock is free. Named points have their own cases: after the first server; after all
       servers and before the leader; in the health wait, before the mark (the next plan bootstraps and creates the
       clients); after the mark and before the clients; and after a client's create and before its registration. A lost
       `Bootstrap` answer ends with the cluster bootstrapped; a lost `IntroToken` answer ends with a token for the
       node's name in its user data.
     - **Checks of tests.** A test of the plan: an `extraConfig` of 30 KiB fails the plan with the size error without
       `--yes`; a development build without the variables fails a plan that creates nodes and passes one that does
       not; `update --yes` reads Nomad's release files once.
   - **Built in M2.7b** ([ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)): the same flows with the scrub,
     the guard and the replacement of a client, in `testing/synctest` bubbles.
     - **The fakes.** `nomadfake` lists each node with an address and serves the Raft peers (`SetPeers`); its `Peers`
       fails before the bootstrap as Nomad's 403 does. The Nomad of the flow tests sets the peers before each call:
       every ready server and combined machine is a voter named `<hostname>.global` at `<private address>:4647`. It
       registers a client or combined node at the first VPC address of its instance, which it reads with
       `vultrfake.InstanceVPCs`, and only while the cluster has a leader. `vultrfake.SetInstanceTags` and
       `SetInstanceUserData` change an instance without a call.
     - **Golden files.** `flow_build.calls.golden` holds the `Peers` call and the `GetInstance` and
       `UpdateInstance` of each scrub; `flow_scale.calls.golden` holds the pair of the new client's scrub and no
       `Peers` call. `flow_unregistered.plan.golden` and
       `flow_unregistered.calls.golden` hold the replacement of a client.
     - **The flows.** A fresh cluster of three servers and two clients, a combined cluster of three and a cluster of
       one node end with the label and the stub on every instance, as the golden files show; a second `update --yes`
       plans nothing, makes no Nomad call and only reads the cloud. A client that never registers is waited for while
       it is younger than 31 minutes; after that the plan shows its delete as `not registered` and a create of the
       same name, which the apply carries out with a new token. A plan that would delete a node that joined fails with
       the error of [13.4](#134-scaling), with and without `--yes`; a machine without the label that registered is not
       deleted. `delete cluster` deletes a cluster of labelled nodes as before.
     - **Cuts.** The context of a run ends before and after each cloud call and each Nomad call of a build and
       of a rebuild, and each state write fails before and after it. The replacement of a client is cut before and
       after the delete of its machine. The next run ends
       with one instance per node, each labelled and scrubbed, and a plan without changes. Named points have their
       own cases: after the health wait and before the first server's scrub; between two servers' scrubs; a scrub
       whose answer is lost; user data scrubbed by hand without the label; after a client registered and before its
       scrub; after the delete of an unregistered client and before its create. Every test of a step that changes
       the cloud or the store ends with a plan that has no changes (`wantConverged`).
   - **Built in M2.8** ([ADR-0033](adr/0033-operator-commands.md)): the three operator commands, in
     `testing/synctest` bubbles where time matters, on `vultrfake`, `nomadfake` and loopback listeners.
     - **The fakes.** The Nomad of the flow tests reports each server's health entry and each node's version, and a test
       changes a version, the peers, one server or one node. `nomadfake` issues tokens
       ([13.2](#132-tent-update-cluster---yes)). `internal/shellenv/shellenvtest` runs the printed lines in a real sh
       and a real fish.
     - **`validate cluster`.** A built cluster of three servers and two clients, a combined cluster of three and a
       cluster of one node are valid. The golden files `validate.golden` and `validate.json.golden` hold a result with a
       line for each check of [13.6](#136-tent-validate-cluster---wait-duration), 17 failures, and one certificate
       warning. One round asks Nomad `Leader`, `Peers`, `Health` and `Nodes`, once each. A call
       leaves the state store as it was and the lock free. `--wait` ends with 0 once the cluster turns valid and with 2
       at its deadline, a negative `--wait` is refused, and Ctrl-C during a wait ends with 1.
     - **`export nomad`.** The four files have the names, content and (on Unix) modes of [9.7](#97-operator-access); the
       certificate and the key make a pair; the token file has no line end. The six lines set the six variables in a
       real sh and fish. A second run replaces the files and leaves no temporary file, and a failed write keeps the
       earlier files. A directory that cannot be made and a TTL that is not above zero leave no token: Nomad gets no
       call. Neither they nor an access that fails leave a file or a directory that the run made. The bootstrap secret
       is in no file.
     - **Operator access.** The token is not the bootstrap secret; the certificate verifies against the CA bundle,
       names `cli.global.nomad`, allows client authentication only and ends TTL after tent's clock, or with the CA; the
       access ends with the earlier of the token's end and the certificate's end; each error of
       [9.7](#97-operator-access) has its test, except `no Nomad client is set up`, which the tests of `validate` and
       `update` cover. A lost `CreateToken` answer on the first server gives the second
       server's token, and the fake has issued two. No call writes to the state store or takes a lock.
     - **The proxy.** `httptest` servers with certificates from `internal/pki` require a client certificate. A request
       reaches the server with the token header, path, query and body; a token that the request carried is replaced;
       `Origin` is not passed on. A server of another CA gets 502 and nothing of the request reaches it. A foreign
       `Host`, `Origin` and `Sec-Fetch-Site` get 403 before anything goes upstream (`TestRefusal` is a table). A
       response written in two parts arrives in two parts; an upgrade carries bytes both ways; the next request goes to
       the next server after one that reached none; a 302 comes back as a 302.
     - **`tent ui`.** A loopback listener on port 0 and a stub proxy factory: the printed URL has the listener's real
       port and answers through the stub, and the config holds the servers, the region, the CA and a token that is not
       the bootstrap secret. Ctrl-C ends the command with 0 and closes the port. The end of the session is tested with
       an injected clock, not with a real sleep: at the time, at the next check of the clock, and when the clock jumps.
       A busy port fails without a token; the other addresses of [9.7](#97-operator-access) are refused before any call.
       The wiring test of `cmd/tent` shows that the binary has the real proxy.
     - **Secrets.** `internal/secrettest` finds none in the output, the logs, the JSON or an error of any of them.
   - `hack/vultr-spike/spike.sh --only cluster` (spike v12) checks the flow on a real Vultr account: tent builds a
     cluster of three servers and two clients with `create cluster --yes` on a temporary `file://` state store and a
     development tent-node, five instances at once. It records the time to the leader, the bootstrap, healthy servers
     and each registration. It checks `tent export nomad` (the modes, the six lines, that the token differs from the
     bootstrap token, and what Nomad says about it and about the certificate), and with the exported files and curl
     the members, autopilot and the nodes. It runs a docker job on a client, and `nomad server members` when a `nomad`
     binary is on the PATH. It checks `tent validate cluster` on the built cluster (exit 0, the valid line, the open
     `access.api` and failure-domain warnings), `tent ui` in the background (four requests through its port, `Host:
     example.com` refused with 403, SIGINT ending it with exit 0 and the port closed), and a client whose Nomad stops
     (`validate` exits with 2 and names the node, then exits with 0 after `systemctl start nomad.service` and
     `--wait 5m`). It checks that `update` has nothing left to do, each instance's `tent/spec-hash`, the first peers
     call between two machines, a restart of `nomad.service` on a server, and `delete cluster --yes` leaving nothing in
     the cloud or the store ([README](../hack/vultr-spike/README.md)). Since M2.7b it also checks each node's scrub
     line, its `tent/joined=true` tag and its user data against the stub, a reboot of a client, the refusal to delete a
     joined node, and with `--unregistered` the replacement of a client that never registered. It needs `openssl`.
     Spike v12 ran on 2026-10-07 (run `9pxbqn`, [platform notes §3.16](platform-notes.md#316-spike-runs)).
4. **tent-node tests.** Phases run with an abstracted filesystem and exec. Occasionally they run in a
   systemd-enabled container or a VM.
   - **Built in M2.5.** `internal/nodeup/nodeuptest` holds the fakes: an in-memory filesystem that behaves as
     `nodeup.OSFS` does on Linux and records what changed, a runner that answers commands from a script and records
     them, a fake Ubuntu 24.04 whose systemd (unit states, enabling, `NeedDaemonReload`) and `timedatectl` keep state,
     a host and a metadata service. The Vultr metadata client runs against an `httptest` server that stands in for
     `169.254.169.254`, with a document of the form a Vultr instance served on 2026-09-25, its ids and addresses
     replaced by documentation values.
   - Golden files hold the units, the files of `system` and `status.json`. The tests check the exact order of the
     commands, that a second `up` or `install` runs no command that changes anything and writes nothing but
     `status.json`, that no unit is ordered on cloud-init, `multi-user.target` or `nomad.service`, the context ending
     between phases and inside a command, and that no secret reaches a log, an error or `status.json`.
   - **Built in M2.6a.** A host from `nodeuptest.NewHost` has no network: its transport fails every request, so a test
     serves what the machine downloads with `httptest`. `nodeuptest.Tgz` builds gzip tars that are the same bytes for
     the same files, and can cut one short as a broken download does. The fake Ubuntu also keeps the state of:
     - ufw: `ufw.conf` with `ENABLED=yes` and the unit enabled and active, as on Vultr's images; `ufw disable` writes
       `ENABLED=no` and leaves the unit enabled, as ufw does;
     - firewalld, installed, enabled and active only after `InstallFirewalld`;
     - nft: `nft -f` of tent's ruleset keeps the table's comment, `nft -j list tables` prints the tables as nft 1.0.9
       does, and `Reboot` drops the table, as the kernel does;
     - dpkg and apt: `docker.io` is not installed, and `dpkg-query` exits with 1; the install puts `docker.service`
       into `/usr/lib/systemd/system`, enabled and active, as the package's postinst does; `LockDpkg`,
       `LockAptLists` and `LockAptArchives` make the next runs of a command fail with the lock errors of apt 2.8 and
       dpkg 1.22;
     - `docker.service`: enable, start, restart, `show -p Job` and their states. `Reboot` leaves an enabled Docker
       inactive with a queued start job, as the timing of the M2.6a VM check on 24.04 suggests.
   - Golden files hold tent's nftables ruleset for each role and `daemon.json`. The tests check that a second `up`
     changes nothing and downloads nothing, that after a simulated reboot `hostfirewall` loads the table again and
     reports `done`, that `ufw disable` runs once and never while `ENABLED=no`, and that a server gets neither
     Docker nor the CNI plugins. They also cover the install's retries, with fake time (`testing/synctest`), and
     each kind of bad tar entry.
   - **Built in M2.6b** ([ADR-0030](adr/0030-nomad-on-nodes.md)). Its dialer fails every call too, so a test sends the
     machine's calls to a `nodeuptest` server through `Server.Dial`. More fakes in `nodeuptest`:
     - `Zip` builds zips of stored entries, also one whose header gives a wrong size, and `NomadZip` one as HashiCorp
       ships Nomad: `LICENSE.txt`, then `nomad`;
     - `ServeMTLS` is an `httptest` server with a node's certificate from `internal/pki` that requires a client
       certificate of the CA; `Server.Dial` stands in for the host's dialer whatever address it is asked for, and
       records the address;
     - `Agent` (`ServeAgent`) is a fake Nomad agent over `ServeMTLS`: its peers, a leader answer without a leader,
       and a health check that answers 500 as often as a test sets; `Hang` is a dialer that answers nothing until the
       call's context ends;
     - `Runner.Next` queues a one-time answer of a command before the one that `On` gave it;
     - the in-memory filesystem has `Open`, `WriteStream` and `HasContent`, and the fake Ubuntu has
       `systemctl restart` and `/usr/local/bin`; `Reboot` starts the enabled units again and leaves the others
       inactive, so `nomad.service` stays inactive until `up` starts it.
   - A golden file holds `nomad.service`. The tests check that:
     - a second `up` neither writes the binary nor restarts Nomad;
     - a changed file restarts Nomad once, also when the run that wrote it failed before the restart, and a directory
       change or a reload alone restarts nothing;
     - a server gets no `/var/lib/nomad/client` and no `11-instance.hcl`;
     - `verify` tries 60 times, 2 s apart (with fake time and refused dials), and passes on a 500 and then a 200 over
       loopback without a wait, since `testing/synctest` cannot wait on a real socket;
     - `up` holds the lock while the phases run.

     They also cover each kind of bad zip, `join`'s fallbacks to the known servers, the lock with real `flock` calls
     on Unix, and `refresh-join`: a changed and an unchanged answer, no answer, a server asking its own agent first,
     the lock wait and the deadline with their exit codes, and a failed write.
   - `hack/vultr-spike/spike.sh --only tentnode` (spike v8) boots a development build of tent-node on a real Vultr VM
     and reboots it, with the user data of `hack/tent-node-userdata`: the only node of a cluster `tent-node-check`, a
     combined node with `bootstrap_expect = 1`, no seed and no intro token, built by `app.NodeConfigOf`, with
     throwaway secrets (decision 24 of [18](#18-open-questions)). The check records the phases, Nomad's unit, an ACL
     bootstrap, a docker job in bridge mode across a restart of Docker (its container kept or replaced), of
     containerd and of Nomad, `refresh-join`'s first real mTLS peers call, and after the reboot the client eligible
     and Nomad stopped before Docker; it also records needrestart, `20auto-upgrades`, `debconf-show docker.io` and
     `docker.service`'s relations ([README](../hack/vultr-spike/README.md)).
   - `ExecRunner`'s process groups are tested on Unix with real processes, checked by their recorded ids.
   - The tests run on Linux, macOS and Windows. `OSFS` sets and compares modes and owners only on Unix, and the tests
     of modes and owners skip Windows.
   - A test in `internal/buildconfig` lists what `./cmd/tent-node` links with `go list -deps`
     ([5](#5-repository-layout-and-dependency-rules)).
   - `TestNomadAgentFiles`, on every pull request, fails when a golden of `internal/nodeconfig` goes to no role, so
     the weekly `nomad config validate` ([16](#16-technology-stack-and-releases)) covers every golden.
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
  - `spf13/cobra`, in tent only: tent-node parses its flags with the standard `flag` package;
  - `vultr/govultr/v3`, pinned at v3.33.0, with its retries off: tent's own transport retries idempotent calls and
    limits the rate ([11.8](#118-api-client-rate-limits-cost));
  - `hetznercloud/hcloud-go/v2`;
  - `hashicorp/nomad/api`, in `internal/nomadops` only, pinned at `v0.0.0-20260917172403-9dcbdc5e64ec`, the commit
    of Nomad's tag v2.0.7, which the `stable` channel recommends. It moves by hand with the channel, and Renovate
    leaves it alone: a rule in `.github/renovate.json`, checked by a test in `internal/buildconfig` (decision 16 of
    [18](#18-open-questions)). The modules it brings are in
    [platform notes §1.5](platform-notes.md#15-licensing);
  - `hashicorp/go-cleanhttp`, for the pooled transport of the Nomad API client;
  - `aws-sdk-go-v2` (`config`, `service/s3`) and `aws/smithy-go` for the s3 state store and the upload of development
    builds (`internal/s3url`);
  - `gofrs/flock` for the file store's locks (`flock` on Unix, `LockFileEx` on Windows);
  - `golang.org/x/mod/semver` for the version guard and the channels, pinned at v0.40.0 because v0.41.0 declares
    `go 1.26.0` and would rewrite `go.mod`;
  - `sigs.k8s.io/yaml`;
  - `go.yaml.in/yaml/v3` and `sigs.k8s.io/json` for spec files ([ADR-0022](adr/0022-json-schema-from-go-types.md));
  - `invopop/jsonschema`, in the schema generator only;
  - `google/licenseclassifier/v2`, in the licence check only;
  - `ProtonMail/go-crypto`, in `internal/assets` only, to verify Nomad's `SHA256SUMS`
    ([ADR-0026](adr/0026-channels-and-release-assets.md));
  - `golang.org/x/sync/errgroup`;
  - `log/slog`;
  - tests: `google/go-cmp`, `santhosh-tekuri/jsonschema/v6` to check examples against the schema, and
    `hashicorp/hcl` v1 (MPL-2.0) to parse the rendered Nomad configuration back.
- **Quality gates:**
  - golangci-lint with the depguard layer rules; `go vet` runs as its govet linter;
  - `go test -race` on Linux, macOS and Windows with the Go from `go.mod`, which builds the release, and on Linux with
    the newest Go too;
  - the s3 state store's tests against a Cloudflare R2 bucket, skipped where its secrets are missing, as in forks.
    The job needs the repository variable `TENT_TEST_S3_URL` (an `s3://tent-ci/ci?…` URL, prefix `ci`), the secrets
    `R2_ACCESS_KEY_ID` and `R2_SECRET_ACCESS_KEY` (an R2 API token limited to the bucket, Object Read & Write), and a
    lifecycle rule on the bucket that expires objects under `ci/` after 1 day, for runs cancelled before cleanup;
  - govulncheck, also weekly;
  - the `online` job, weekly and never on pull requests: the tests named `…Online` read public release sites with
    `TENT_TEST_ONLINE=1`. They check that the recommended Nomad of `stable` still verifies with the embedded HashiCorp
    key, and fail when the key expires within 180 days ([8.5](#85-artifacts-and-verification)). They also run
    `nomad config validate` of the channel's minimum and recommended Nomad on each role's goldens of
    `internal/nodeconfig` (`TestNomadConfigValidateOnline` in `internal/assets`, decision 23 of
    [18](#18-open-questions)). The job can also be run by hand (`workflow_dispatch`); a manual run starts only this
    job, and every other job of `ci.yml` skips it.
    `internal/buildconfig` checks that every test that reads `TENT_TEST_ONLINE` has such a name;
  - the licences of every module tent or tent-node links, on each platform the release builds for: each must be
    Apache-2.0, BSD-2-Clause, BSD-3-Clause, ISC, MIT or MPL-2.0, and every licence file other than a NOTICE must name
    one. A licence file the classifier cannot name, such as BUSL-1.1 or a proprietary text, fails even when the module
    has another, allowed licence (`make licenses`, in the lint job);
  - a release snapshot on every pull request, which fails when `checksums.txt` lacks either tent-node binary;
  - `make check` runs fmt, lint, licenses, test and build locally. `make build` also builds
    `bin/tent-node_linux_amd64`, which `make dev-upload` uploads for a development build
    ([8.5](#85-artifacts-and-verification));
  - Renovate.
- **Releases** use GoReleaser on `v*` tags:
  - `tent` for linux, darwin and windows on amd64 and arm64: archives, a Homebrew cask with signed and notarized
    macOS binaries, and deb and rpm packages;
  - every archive and package of tent carries `LICENSE` and `THIRD_PARTY_NOTICES`. A GoReleaser hook writes the
    notices before the build, as `make notices` does: the licence and notice files of the Go standard library and of
    every module tent or tent-node links on any platform the release builds for, with each module's version and where
    to download its source;
  - `tent-node` for linux on amd64 and arm64 from M2.5, as the bare binaries `tent-node_linux_amd64` and
    `tent-node_linux_arm64`, the names that `internal/assets` reads. They have no room for the notices, so
    `THIRD_PARTY_NOTICES` is also a file of the release. Only tent's macOS binaries are notarized
    ([ADR-0028](adr/0028-tent-node-agent-units-and-delivery.md));
  - `checksums.txt` with a keyless cosign signature, which also lists the tent-node binaries and the notices; an SBOM
    per archive and per tent-node binary;
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
| Vultr counted a deleted machine against the account's instance limit, so the create of a replacement was refused (seen 2026-10-06, run `rugw2m`) | the replacement of a client that never registered fails in an account at its limit | the Vultr provider sends a refused node create again for up to 2 minutes after a delete that it made ([11.3](#113-creating-a-node)); a limit that still holds then returns the limit error with its hint, and so does a rerun of tent right after a run that deleted, since the window does not outlive the process |
| Hetzner capacity and account limits (5 servers, creation restrictions since June 2026) | Hetzner provider cannot be E2E-tested | Vultr first (ADR-0014); Hetzner E2E in M4 |
| Nomad 2.x version skew rules not yet restated | broken upgrades | channels allow one major version from a minimum, and tent warns about versions they have not tested; servers before clients |
| HashiCorp's embedded release key expires on 2030-03-01, or is rotated or revoked | tent cannot verify Nomad downloads, or trusts a revoked key | a weekly CI job fails 180 days before the expiry; a tent release embeds the new key ([8.5](#85-artifacts-and-verification)) |
| BUSL licence of Nomad | a paid managed offering would need a commercial licence | tent downloads official binaries and never redistributes them; stays free (not legal advice) |
| Nomad reads a rendered value differently from the HCL1 that the tests use: Nomad parses with its fork `v1.0.1-nomad-1`, the tests with upstream v1.0.0 | a node does not start, or runs with another setting | strict quoting refuses what HCL1 cannot read back; tests parse every golden back; the weekly `online` job runs `nomad config validate` of the channel's minimum and recommended Nomad on the goldens ([ADR-0030](adr/0030-nomad-on-nodes.md)) |
| A reboot of a client kills its tasks without a migration, since clients do not drain themselves at shutdown | the tasks are down until the client restarts them, if it is back first, or the scheduler replaces them after the missed heartbeats | a self-drain would leave the node ineligible after every reboot or restart (hashicorp/nomad#17093); tent drains a client through the API before it removes it ([ADR-0017](adr/0017-api-driven-server-removal.md), [ADR-0030](adr/0030-nomad-on-nodes.md)) |
| A client's servers are not up within `verify`'s 2 minutes | `up` fails on the client, and on the first boot `install` and cloud-init, though Nomad joins later | `update` creates clients after the servers are healthy and judges a client by its registration in Nomad (M2.7a) |
| tent-node restarts Docker, after a hand edit of `daemon.json` and an `up` by hand, or dockerd crashes | the node's docker tasks restart after the restart policy's delay (17-20 s seen): Nomad's docker driver stops the containers that live-restore kept ([platform notes §6.4](platform-notes.md#64-restarts-of-docker-containerd-and-nomad)) | accepted on 2026-10-05 ([ADR-0030](adr/0030-nomad-on-nodes.md)); tent-node restarts Docker only for its own `daemon.json` |
| nomadops relies on one Nomad answer read in the v1.11.3 source, not in 2.0.7: 403 from `token/self` for an unknown secret (the report in the 429 of an unhealthy cluster was seen on 2.0.7 on 2026-10-05, [platform notes §1.2](platform-notes.md#12-features-tent-relies-on); the text `ACL bootstrap already done` on 2026-10-05, [§1.6](platform-notes.md#16-the-agent-on-a-node)) | a repeated bootstrap fails | nomadops matches status codes and one message prefix; E2E runs real Nomad from M2.9 ([platform notes §1.2](platform-notes.md#12-features-tent-relies-on)) |
| `extraConfig` overrides tent's settings, such as `data_dir`, the TLS paths, the dynamic ports or Nomad's bridge subnet (`bridge_network_subnet`) | a node cannot find its files, a client is refused, or the host firewall blocks workloads | documented as unsupported ([3.3](#33-api-rules), [8.4](#84-nomad-configuration-rendering)) |
| A unit ordering between cloud-init, tent-node and Nomad hangs the boot, or `up` hangs | the node never comes up; cloud-init never finishes | `install` waits for `up`, and no unit is ordered on cloud-init, `multi-user.target` or `nomad.service`, nor `nomad.service` on `tent-node.service` (a unit test checks); `nomad.service` is never enabled; finite start timeouts (45 minutes for `up`); `status.json` on every run; the M2.5 VM check boots and reboots a node ([8.1](#81-bootstrap-chain)) |
| Secrets in user data | node impersonation if metadata leaks | mitigations in [9.4](#94-secrets-on-nodes-threat-model), including the scrub on Vultr once a node has joined (M2.7b); a node that never joins keeps them; bootstrap controller in v2 |
| A client whose machine is ready never registers, or a run is cut between a client's create and its registration | the client's intro token expires after 30 minutes and is refused a minute later, and the node never joins | the registration wait fails after 10 minutes and names the node; a client without the joined label that is older than 31 minutes and that Nomad does not list is deleted and created again ([13.4](#134-scaling), [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)) |
| A scrub reaches a machine before cloud-init has read its user data (Vultr reported an instance ready 7 s before its kernel started, [platform notes §3.3](platform-notes.md#33-instances)) | the node boots without its config and never joins | tent scrubs a machine only after it saw a node of the machine's own name and private address; a stale node of an earlier machine at that name and address can read `ready` for up to 30 s after the machine died, 300 s after a leader change; a create takes longer than 30 s on Vultr; the node meta `tent_instance_id` would be exact ([ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)) |
| An operator changes a tag of a machine between the read and the update of `MarkJoined` | the change is lost | see [11.6](#116-user_data) |
| While `tent ui` runs, its loopback port acts with a management token: every process of every user on the operator's machine can use it, and the UI keeps the token in the browser's `localStorage` until it expires | such a process, or a web page that passes the proxy's checks, acts as an operator for up to 24 hours | loopback only; a foreign `Host`, `Origin` or `Sec-Fetch-Site` gets 403 before anything goes upstream; the token is of no use without a client certificate of the cluster's CA; a process of the operator's own user could also read the files of `export nomad`, a process of another user could not; accepted on 2026-10-06 ([9.7](#97-operator-access), [ADR-0033](adr/0033-operator-commands.md)) |
| A token whose answer was lost, an earlier export's token and a `tent ui` token stay valid until they expire | an old token works for up to 24 hours | each has a TTL, and `nomad acl token delete <accessor>` revokes one early; `export nomad` prints the accessor ([9.2](#92-acl-and-tokens)) |
| The autopilot report shows a killed server as `alive` for about 36 s on Nomad 2.0.7, and the Raft configuration keeps its vote until autopilot removes it ([platform notes §1.2](platform-notes.md#12-features-tent-relies-on)) | `validate` can read a server that died seconds ago as valid | `validate --wait`, or a second run, checks again ([13.6](#136-tent-validate-cluster---wait-duration)) |
| tent stores no node certificate ([9.1](#91-pki)) | `validate` reads a node certificate's end by estimate | a warning 30 days ahead leaves room for the difference |
| `nomad.service` should end as failed after every stop of a server or combined node, since servers run with `leave_on_terminate = false` (seen on a real server on 2026-10-05, [platform notes §3.16](platform-notes.md#316-spike-runs)) | an operator or a monitor sees a failed unit after a stop or restart that worked | accepted: `SuccessExitStatus=1` would hide real failures (decision 26 of [18](#18-open-questions), [ADR-0031](adr/0031-bootstrap-in-update.md)) |
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
9. **Nomad's sha256s:** checked at run time. The CLI downloads `nomad_<v>_SHA256SUMS` and its detached signature and
   verifies them with HashiCorp's release key, which tent embeds ([8.5](#85-artifacts-and-verification),
   [ADR-0026](adr/0026-channels-and-release-assets.md)). So a plan that creates a node or repeats the create of one
   needs
   releases.hashicorp.com (M2.7a).
10. **The Nomad version of a spec without one:** the first `update` records the channel's recommended version in
    `cluster.completed.yaml`, and later runs keep it. A newer tent does not move nodes to another version by itself;
    `upgrade cluster` does ([13.2](#132-tent-update-cluster---yes), [13.5](#135-tent-upgrade-cluster---yes)).
    Revised the same day: a cluster may run any official Nomad release from the channel's minimum (2.0.0) up to the
    next major version (3.0.0, not included), so a new Nomad patch needs no tent release. The channel's tested
    versions only decide whether tent warns.
11. **What a channel holds:** Nomad and the CNI plugins only. Images stay in the provider's table
    ([ADR-0023](adr/0023-vultr-inventory-dedupe-and-images.md)) and the API default (decision 4).
12. **When NodeConfig reaches `update`:** in M2.7, with tent-node, intro tokens and the bootstrap. M2.7a built it,
    with the asset downloads, the warning about development variables and the Nomad pin written before the first node
    ([13.2](#132-tent-update-cluster---yes),
    [ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md),
    [ADR-0031](adr/0031-bootstrap-in-update.md)). Nodes get real user data, which holds node keys and the gossip key,
    and the scrub runs only after the node has joined. M2.7b built it ([9.4](#94-secrets-on-nodes-threat-model),
    [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)).
13. **The instance id:** NodeConfig carries the node's name, not a cloud instance id. tent-node reads the id from the
    metadata service and writes `tent_instance_id` into its own `11-instance.hcl` on client and combined nodes.
    Preflight compares the hostname with the name ([8.2](#82-tent-node-phases)).
14. **The user data budget:** 24 KiB for the whole cloud-config on every provider, headroom under 32 KiB. A
    per-provider `Capabilities.MaxUserDataBytes` comes when a provider needs less, such as AWS with 16 KB. An
    `extraConfig` that does not fit fails with the group's name and the size ([8.3](#83-nodeconfig-contract)).
15. **The host firewall and `access`:** the host does not copy `access.ssh` or `access.api`. SSH, ICMP and, on server
    and combined nodes, 4646 are open on the host, and the cloud firewall filters their sources. The host opens
    Nomad's ports and the dynamic ports only to the cluster CIDR and blocks the metadata address for workloads. A
    change of `access` never changes the spec hash ([8.3](#83-nodeconfig-contract)). Since M2.6a client and combined
    nodes also open 4646 and the dynamic ports to Nomad's and Docker's default bridges, and the metadata block is
    decision 21 ([ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md)).

Decided on 2026-09-29:

16. **The Nomad API module:** `github.com/hashicorp/nomad/api` is pinned to the commit of the Nomad tag that the
    channel recommends, v2.0.7 now, and moved by hand with the channel. Renovate is off for it
    ([16](#16-technology-stack-and-releases)).
17. **Node pools:** tent does not create them. Nomad creates a pool when its first client registers
    ([9.3](#93-client-introduction), [13.2](#132-tent-update-cluster---yes)). Pool descriptions or meta come when the
    spec has such fields.
18. **Where development builds of tent-node live:** in the CI R2 bucket under `dev/`, served by presigned URLs valid
    for at most 7 days. The maintainer's machine uploads with its own R2 token, not CI's, and a lifecycle rule deletes
    `dev/` objects after 8 days ([8.5](#85-artifacts-and-verification)).
19. **The handover from cloud-init to tent-node:** `tent-node install` starts `tent-node.service` and waits for it.
    The unit is not ordered on `cloud-final.service`, `cloud-init.target`, `cloud-config.service`,
    `multi-user.target` or `nomad.service`, and has a finite `TimeoutStartSec`. `up` starts Nomad itself (M2.6b), and
    `verify` checks Nomad without waiting for a leader ([8.1](#81-bootstrap-chain), [8.2](#82-tent-node-phases)).
    M2.6a added `cloud-init-main.service` to those units as a precaution
    ([ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md)).
20. **The provider on the node:** NodeConfig carries `provider`, validated and outside the spec hash, since a
    cluster's provider never changes (decision 7). tent-node picks its metadata service by it
    ([7.1](#71-interfaces), [8.3](#83-nodeconfig-contract)).
21. **The metadata service on a node:** only tent-node's own socket reaches it. tent-node's metadata client sets the
    socket mark `0x747`; tent's nftables table lets packets with that mark leave for the metadata address and drops
    every other packet to it, from the host (output) and from containers (forward). It fails closed: root `curl` to the
    service on a node gets no answer. A workload with CAP_NET_ADMIN or CAP_NET_RAW can still set the mark, such as a
    task that the operator gives NET_RAW for ping ([9.4](#94-secrets-on-nodes-threat-model)).

Decided on 2026-09-30:

22. **The Nomad region on the node:** NodeConfig carries `region`, validated as `spec.nomad.region` is and outside the
    spec hash, since `00-tent.hcl` brings the region into it already. tent-node names the servers it calls
    `server.<region>.nomad`. Parsing the region from the node's certificate was the alternative
    ([8.3](#83-nodeconfig-contract)).
23. **`nomad config validate` of the goldens:** an `…Online` test in the weekly `online` CI job, not on every pull
    request and not only in a VM check. On 2026-10-02 the maintainer added that it runs with both the channel's minimum
    and its recommended Nomad: a key added after the minimum would break a cluster pinned to an older allowed version
    ([16](#16-technology-stack-and-releases)).
24. **The secrets of the tent-node VM check:** `hack/tent-node-userdata` makes a throwaway CA, the node's certificate
    and a gossip key on each run, for one combined node with `enforcement = "warn"` and no intro token. They sit in the
    user data of a VM that is deleted after the check; tent scrubs nothing there, since no cluster owns the VM
    ([15](#15-testing)).

Decided on 2026-10-05:

25. **No drain at shutdown:** clients do not drain themselves when Nomad stops, since Nomad's self-drain leaves them
    ineligible after their next start; `00-tent.hcl` sets no `drain_on_shutdown`. tent's own removals drain a client
    through the Nomad API first ([8.4](#84-nomad-configuration-rendering),
    [ADR-0017](adr/0017-api-driven-server-removal.md)).
26. **`leave_on_terminate` by role:** server and combined agents set it to `false`, so a stopped server stays a Raft
    peer, and clients keep `true`. tent removes a server from Raft through the Nomad API, on every provider
    ([8.4](#84-nomad-configuration-rendering), [ADR-0017](adr/0017-api-driven-server-removal.md),
    [ADR-0031](adr/0031-bootstrap-in-update.md)).
27. **Deleting nodes that joined** (decided on 2026-10-05, built in M2.7b): until M3, `update` refuses to delete a
    node that joined Nomad: a machine that carries the joined label, a registered client, or a server of the Raft
    configuration. A Nomad node that is `down` does not count as registered (confirmed on 2026-10-06).
    `delete cluster` works as before ([13.4](#134-scaling),
    [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)).

Decided on 2026-10-06:

28. **A client that never registered:** `update` deletes a client that did not register before
    its intro token expired and creates it again, shown as `not registered`. It never ran a workload, so nothing is
    drained. The delete comes before the create, an exception to creating the replacement before removing the old node
    (confirmed on 2026-10-06; [13.4](#134-scaling), [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md)).
29. **`tent ui` and the token:** the proxy adds the token and mTLS, so the UI and the `nomad` CLI need neither a
    certificate nor a token. It listens on a loopback address only, and a request with a foreign `Host` or `Origin`
    gets 403. While it runs, every local process that can reach the port acts with a management token, and the UI keeps
    that token in the browser until it expires ([9.7](#97-operator-access),
    [ADR-0033](adr/0033-operator-commands.md)).
30. **The exported token:** a management token with a TTL, 24 hours by default. tent owns no ACL policy yet, so the TTL
    is the limit ([9.2](#92-acl-and-tokens)).
31. **Where `export nomad` writes:** `$XDG_CACHE_HOME/tent/<cluster>`, else `~/.cache/tent/<cluster>`, the directory
    with mode 0700 and the files with mode 0600. `--dir` changes the place ([9.7](#97-operator-access)).
32. **`validate cluster` exit codes:** 2 when the cluster is not valid, with the table and no `Error:` line, and 1 when
    tent could not check ([13.6](#136-tent-validate-cluster---wait-duration), [14](#14-cli)).

Decisions 18 to 20 are recorded in [ADR-0028](adr/0028-tent-node-agent-units-and-delivery.md), decision 21 in
[ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md), decisions 22 to 25 in
[ADR-0030](adr/0030-nomad-on-nodes.md), decision 26 in [ADR-0031](adr/0031-bootstrap-in-update.md), decisions 27
and 28 in [ADR-0032](adr/0032-joined-label-scrub-and-delete-guard.md), and decisions 29 to 32 in
[ADR-0033](adr/0033-operator-commands.md).

---

## Appendix A: Nomad agent configuration sketches

The golden files in [`internal/nodeconfig/testdata/`](../internal/nodeconfig/testdata/) are authoritative.
`go test ./internal/nodeconfig -update` rewrites them, and the tests parse each one back with HCL1. Their inputs are a
cluster `prod` with the CIDR `10.64.0.0/16`, the groups `servers`, `workers` and `core`, meta values with quotes,
backslashes, `%{`, `{{` and Unicode, and a seed with an IPv6 address.

| File | Server | Client | Combined |
|---|---|---|---|
| `00-tent.hcl` | [`server_00-tent.hcl`](../internal/nodeconfig/testdata/server_00-tent.hcl.golden) | [`client_00-tent.hcl`](../internal/nodeconfig/testdata/client_00-tent.hcl.golden) | [`combined_00-tent.hcl`](../internal/nodeconfig/testdata/combined_00-tent.hcl.golden) |
| `01-gossip.hcl` | [`server_01-gossip.hcl`](../internal/nodeconfig/testdata/server_01-gossip.hcl.golden) | — | [`combined_01-gossip.hcl`](../internal/nodeconfig/testdata/combined_01-gossip.hcl.golden) |
| `05-join.hcl` | [`server_05-join.hcl`](../internal/nodeconfig/testdata/server_05-join.hcl.golden) | [`client_05-join.hcl`](../internal/nodeconfig/testdata/client_05-join.hcl.golden) | [`combined_05-join.hcl`](../internal/nodeconfig/testdata/combined_05-join.hcl.golden) |
| `10-node.hcl` | [`server_10-node.hcl`](../internal/nodeconfig/testdata/server_10-node.hcl.golden) | [`client_10-node.hcl`](../internal/nodeconfig/testdata/client_10-node.hcl.golden) | [`combined_10-node.hcl`](../internal/nodeconfig/testdata/combined_10-node.hcl.golden) |
| `11-instance.hcl` | — | [`11-instance.hcl`](../internal/nodeconfig/testdata/11-instance.hcl.golden) | [`11-instance.hcl`](../internal/nodeconfig/testdata/11-instance.hcl.golden) |
| `98-user-server.hcl` | [`server_98-user-server.hcl`](../internal/nodeconfig/testdata/server_98-user-server.hcl.golden) | — | [`combined_98-user-server.hcl`](../internal/nodeconfig/testdata/combined_98-user-server.hcl.golden) |
| `99-user-client.hcl` | — | [`client_99-user-client.hcl`](../internal/nodeconfig/testdata/client_99-user-client.hcl.golden) | [`combined_99-user-client.hcl`](../internal/nodeconfig/testdata/combined_99-user-client.hcl.golden) |

[`node.json`](../internal/nodeconfig/testdata/node.json.golden) is an encoded NodeConfig, and
[`user-data.yaml`](../internal/nodeconfig/testdata/user-data.yaml.golden) the user data with its payload masked
([Appendix B](#appendix-b-cloud-init-user-data-sketch)).

**Client: `00-tent.hcl`**, from `client_00-tent.hcl.golden` with its meta cut to five keys. `<private>` stands for
`{{ GetPrivateInterfaces | include \"network\" \"10.64.0.0/16\" | attr \"address\" }}`, and `<interface>` for the
same template with `attr \"name\"`.

```hcl
# Rendered by tent. Do not edit: changes are overwritten on the next boot.
region               = "global"
data_dir             = "/var/lib/nomad"
leave_on_terminate   = true # leave the cluster gracefully when Nomad stops
disable_update_check = true

addresses {
  http = "127.0.0.1 <private>"
  rpc  = "<private>"
}

advertise {
  http = "<private>"
  rpc  = "<private>"
}

client {
  enabled           = true
  node_pool         = "batch"
  node_class        = "general"
  network_interface = "<interface>"
  min_dynamic_port  = 20000
  max_dynamic_port  = 32000

  options {
    "driver.allowlist"     = "docker,exec"
    # Cloud fingerprinters only slow down startup on Vultr and Hetzner.
    "fingerprint.denylist" = "env_aws,env_gce,env_azure,env_digitalocean"
  }

  meta {
    "tent_cluster"   = "prod"
    "tent_nodegroup" = "workers"
    "quote"          = "say \"hi\""
    "rack.id"        = "r1"
    "team"           = "platform"
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

# There is no Consul: do not look for Nomad servers in it.
consul {
  server_auto_join = false
  client_auto_join = false
}

telemetry {
  prometheus_metrics   = true
  publish_node_metrics = true
}
```

A server's `00-tent.hcl` differs:
- `addresses` has `http = "0.0.0.0"`, which the tent CLI reaches through the cloud firewall, and a `serf` address, and
  `advertise` a `serf` address too;
- it has `server { enabled = true }` with `client_introduction { enforcement = "strict" }` instead of the `client`
  block, and `autopilot { cleanup_dead_servers = true }`.
- it has `leave_on_terminate   = false # a stopped server stays a Raft peer; tent removes servers through the Nomad API`
  where the client has `true` (decision 26 of [18](#18-open-questions)).

A combined node's has the server's `leave_on_terminate` line and addresses and both blocks, with
`enforcement = "warn"` by default.

**Server and combined: `01-gossip.hcl`** (the only secret file of the group, mode 0600):

```hcl
# Rendered by tent. Do not edit: changes are overwritten on the next boot.
server {
  encrypt = "<gossip key>"
}
```

**Client: `05-join.hcl`** (rendered by tent-node; servers and combined nodes use `server { … }` and port 4648):

```hcl
# Rendered by tent-node. Do not edit: changes are overwritten.
client {
  server_join {
    retry_join = ["10.64.0.5:4647", "10.64.0.9:4647", "[fd00:64::c]:4647"]
  }
}
```

**Combined: `10-node.hcl`** (a client's has no `server` block):

```hcl
# Rendered by tent. Do not edit: changes are overwritten on the next boot.
name       = "prod-core-0"
datacenter = "ams"

server {
  bootstrap_expect = 3
}
```

**Client and combined: `11-instance.hcl`** (rendered by tent-node from the metadata service):

```hcl
# Rendered by tent-node. Do not edit: changes are overwritten.
client {
  meta {
    "tent_instance_id" = "cb676a46-66fd-4dfb-b839-443f2e6c0b60"
  }
}
```

**Every role: `/etc/systemd/system/nomad.service`** ([8.4](#84-nomad-configuration-rendering)), from
[`nomad.service.golden`](../internal/nodeconfig/testdata/nomad.service.golden):

```ini
# Rendered by tent. Do not edit: changes are overwritten on the next boot.
[Unit]
Description=The Nomad agent of this node
Documentation=https://developer.hashicorp.com/nomad
Wants=network-online.target
After=network-online.target docker.service

[Service]
Type=notify
ExecStart=/usr/local/bin/nomad agent -config /etc/nomad.d
ExecReload=/bin/kill -HUP $MAINPID
KillMode=process
KillSignal=SIGTERM
Restart=on-failure
RestartSec=2
LimitNOFILE=65536
LimitNPROC=infinity
TasksMax=infinity
OOMScoreAdjust=-1000
```

---

## Appendix B: cloud-init user data sketch

`nodeconfig.UserData` renders the user data ([`userdata.go`](../internal/nodeconfig/userdata.go)).
[`user-data.yaml.golden`](../internal/nodeconfig/testdata/user-data.yaml.golden) holds a real one, with its payload
masked and the exact curl options. In this sketch `<…>` marks what differs per node or per release:

```yaml
#cloud-config
package_update: false
package_upgrade: false
write_files:
  - path: /etc/tent/node.json
    encoding: gz+b64
    owner: root:root
    permissions: "0600"
    content: <node.json, compressed with gzip, then base64>
runcmd:
  - - /bin/sh
    - -c
    - |
      set -eu
      mkdir -p /usr/local/bin
      for url in 'https://github.com/ingvarch/tent/releases/download/v0.1.0/tent-node_linux_amd64' '<mirror>'; do
        if curl -fsSL <timeouts and retries> -o /usr/local/bin/tent-node.download "$url" &&
          echo '<sha256>  /usr/local/bin/tent-node.download' | sha256sum -c -; then
          chmod 0755 /usr/local/bin/tent-node.download
          mv /usr/local/bin/tent-node.download /usr/local/bin/tent-node
          exec /usr/local/bin/tent-node install --config /etc/tent/node.json
        fi
      done
      echo "tent-node: no URL gave a file with the expected sha256" >&2
      exit 1
```

- **Package updates stay off.** OS patching happens by replacing nodes. Vultr's vendor data sets the same (2026-09),
  and tent keeps it explicit in case that changes.
- **The payload** is NodeConfig's JSON, compressed without a name or a time, so the same config always gives the same
  user data. Only root reads `/etc/tent/node.json`.
- **The download.** The script tries the tent-node URLs in turn and installs the first file whose sha256 matches.
  curl retries every failure of one URL, a refused connection included, as when the network is not up yet at boot,
  and starts no new try after 10 minutes. A try fails when it stalls below 1 KiB/s for 30 s or runs for 10 minutes, so
  one URL takes at most about 20 minutes before the next mirror gets its turn. When no URL gives the right file, the
  script fails. The URLs are printable ASCII in single quotes, one inert shell word each. `mkdir` makes
  `/usr/local/bin` with mode 0755 where an image lacks it.
- **`exec`** hands cloud-final over to `tent-node install` ([8.1](#81-bootstrap-chain)).
- **Size.** The whole cloud-config must fit in 24 KiB ([8.3](#83-nodeconfig-contract)).

`tent-node install` writes three units into `/etc/systemd/system` (mode 0644, `root:root`), has systemd read them
again only when `systemctl show -p NeedDaemonReload` asks for it, enables the service and the timer, starts the
service and waits until `up` has run, then starts the timer ([8.1](#81-bootstrap-chain)). The golden files in
[`internal/nodeup/testdata/`](../internal/nodeup/testdata/) are authoritative; this is `tent-node.service`:

```ini
# Rendered by tent-node. Do not edit: changes are overwritten.
[Unit]
Description=tent-node up: set the machine up as a Nomad agent of its node group
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/tent-node up --config /etc/tent/node.json
TimeoutStartSec=45min
KillMode=mixed

[Install]
WantedBy=multi-user.target
```

- **No other ordering** ([8.1](#81-bootstrap-chain)): `up` starts Nomad itself.
- **`KillMode=mixed`:** systemd sends SIGTERM to tent-node alone, which cancels its context and stops the process
  group of the program it runs ([8.2](#82-tent-node-phases)), and kills whatever is left when the stop times out.
- **`tent-node-join.service`:** `Type=oneshot`, `After=tent-node.service`, runs `tent-node refresh-join` with
  `TimeoutStartSec=5min`. It has no `[Install]` section: the timer starts it.
- **`tent-node-join.timer`:** `OnBootSec` and `OnUnitActiveSec` set to NodeConfig's `join.refreshInterval` (60 s),
  `AccuracySec=1s`, `WantedBy=timers.target`.

On Vultr, tent replaces this user data with a non-secret stub once the node has joined its cluster (§9.4).

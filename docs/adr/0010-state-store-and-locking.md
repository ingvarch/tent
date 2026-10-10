# ADR-0010: State store backends, layout and locking

- **Status:** Accepted; for providers without unique names see [ADR-0015](0015-idempotency-without-unique-names.md).
  The M0 follow-up (the `Store` interface, the file and S3 backends, and the lease locking with shared conformance
  tests) was built on 2026-09-26 in `internal/statestore`;
  [architecture §10](../architecture.md#10-state-store-and-locking) describes it as built. The Hetzner firewall
  mutex comes with the Hetzner provider in M4, not M1 ([ADR-0014](0014-vultr-first-provider-and-e2e.md)). The layout
  gained `names/<nodegroup>` in M3.4 ([ADR-0038](0038-rolling-update-of-server-groups.md), item 22).
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0003](0003-cloud-is-source-of-truth.md), [ADR-0007](0007-security-baseline.md),
  [architecture §10](../architecture.md#10-state-store-and-locking)

## Context

tent needs a durable place for the desired state (specs), the completed spec, PKI material, cluster secrets,
backups and an audit trail. Operators work from laptops and CI, so the store must be remote-capable. Local files are
still needed for development and tests.

Facts:

- **Hetzner Object Storage** speaks the S3 API. It is EU-only, its credentials can be created only in the Console,
  and its base price is €6.49 per month. It supports versioning, object lock and bucket policies. Conditional writes
  are unsupported on versioned buckets, and `If-None-Match` on unversioned buckets is undocumented.
- **AWS S3** supports conditional writes (`If-None-Match: *`).
- **kops has no locking.** Concurrent `update` runs can race, and the issue went stale.
- **Hetzner resource names are unique per project**, so creating a named resource is an atomic operation.

## Decision

- **Interface.** `internal/statestore.Store` has `Get`, `Put` (with optional `IfNoneMatch`/`IfMatch`), `List` and
  `Delete`, plus `Capabilities`.
- **Backends:**
  - `file:///path` for development and tests;
  - `s3://bucket/prefix?endpoint=…&region=…` through aws-sdk-go-v2, for Hetzner Object Storage, AWS S3, R2 and
    MinIO. Credentials come from the standard AWS credential chain.
- **Layout per cluster:** `tent-version`, `cluster.yaml`, `nodegroups/<name>.yaml`, `cluster.completed.yaml`,
  `names/<nodegroup>`, `pki/…`, `secrets/…`, `backups/…` and `history/…`. See architecture §10.2.
- **Version guard.** `tent-version` records the minimum tent version that may operate the cluster. Older CLIs
  refuse to run mutating commands.
- **Safe deletion.** `tent delete cluster` removes the state last and refuses to delete unknown files without
  `--force`.
- **Secrets.** In v1 they live in the same private bucket; enabling bucket versioning is recommended. Client-side
  encryption with `age` is a later addition through a `SecretStore` layer.
- **Locking.** Mutating commands take a cluster lease recording owner, host, operation, acquired-at and expires-at.
  The lease is renewed during long operations.

  | Backend | Mechanism |
  |---|---|
  | `file://` | `flock` |
  | S3 with conditional writes | `If-None-Match: *` |
  | Hetzner Object Storage | cloud-native mutex (see below) |

  - **The Hetzner cloud-native mutex** is an empty firewall named `<cluster>-lock`. Its labels are
    `tent/lock-for=<cluster>` plus owner and expiry. It has no `tent/cluster` label, so inventory and prune ignore
    it. Creating it acquires the lock, and deleting it releases the lock.
  - **Stale locks.** `tent state unlock --force` removes a stale lock. A lock past its expiry can be taken over, with
    a warning.

## Consequences

### Positive

- One code path for all S3-compatible stores.
- Concurrent operators and CI jobs are serialized.
- Old CLIs cannot damage clusters managed by newer ones.

### Negative / trade-offs

- The Hetzner lock depends on the Hetzner API being reachable. That is also true of every mutating operation.
- The firewall mutex counts against the project's firewall limit (50 by default).
- Secrets rely on bucket privacy until `age` encryption lands.

### Follow-ups

- The `Store` interface, the file and S3 backends, and the lease locking with conformance tests shared by all
  backends (M0).
- The Hetzner firewall mutex (M1).
- Probing Hetzner Object Storage for `If-None-Match` support in E2E, and switching to it if it proves reliable.

## Alternatives considered

- **No locking** (kops). Races between operators and CI lead to duplicated or conflicting changes.
- **An external lock service** (DynamoDB, etcd, Consul). Another dependency to provision before the cluster
  exists.
- **Separate secret storage** (Vault, a cloud secrets manager). Circular for a tool that bootstraps clusters. It may
  be offered later as an optional backend.

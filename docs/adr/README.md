# Architecture Decision Records

This directory records the significant decisions behind tent's design, one decision per file. We use Michael
Nygard's format, lightly extended; see [template.md](template.md).

- [`docs/architecture.md`](../architecture.md) describes **what** the design is.
- ADRs explain **why** it is that way and which alternatives lost.
- Verified facts the decisions rely on are in [`docs/platform-notes.md`](../platform-notes.md).

## Rules

- An accepted ADR is not rewritten. To change a decision, add a new ADR that supersedes it, and set the old one's
  status to `Superseded by ADR-NNNN`. Typos and broken links may be fixed in place.
- Number ADRs sequentially (`NNNN-short-title.md`) and add each one to the index below in the same change.
- If an implementation needs to diverge from an accepted ADR, write the superseding ADR first.
- Parts of a decision that still wait for the maintainer, or for a platform spike, are marked **provisional**
  inside the ADR. Maintainer questions are listed in [architecture §18](../architecture.md#18-open-questions).
- Amendments that do not replace a decision (an ADR that extends or narrows an older one) are recorded on the older
  ADR's status line, for example "extended by ADR-0015".

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | Accepted |
| [0002](0002-direct-cloud-apis-and-own-engine.md) | Call cloud APIs directly and reconcile with an in-house engine | Accepted |
| [0003](0003-cloud-is-source-of-truth.md) | The cloud is the source of truth: ownership labels and deterministic names | Accepted; extended by 0015 |
| [0004](0004-layered-architecture.md) | Layered architecture: a cloud-agnostic core and providers that translate intents | Accepted; provider order changed by 0014; narrowed by 0021 |
| [0005](0005-immutable-nodes-and-nomad-aware-rollouts.md) | Immutable nodes and Nomad-aware rolling updates | Accepted; amended by 0017 |
| [0006](0006-two-binaries-and-nodeconfig.md) | Two binaries and a versioned NodeConfig contract | Accepted |
| [0007](0007-security-baseline.md) | Security baseline: PKI, mTLS, ACL, client introduction | Accepted; see 0019 |
| [0008](0008-node-credential-delivery.md) | Node credential delivery: user data in v1, bootstrap controller as the target | Accepted; exception in 0019 |
| [0009](0009-server-discovery-fixed-ip-slots.md) | Nomad server discovery on Hetzner through fixed private IP slots | Accepted (Hetzner-specific; generic strategy in 0016) |
| [0010](0010-state-store-and-locking.md) | State store backends, layout and locking | Accepted; see 0015 |
| [0011](0011-nomad-only-scope-and-licensing.md) | Nomad-only scope for v1 and licensing boundaries | Accepted |
| [0012](0012-testing-strategy.md) | Testing strategy: fakes, golden integration tests, E2E on Hetzner | Accepted; E2E platform amended by 0014 |
| [0013](0013-technology-stack.md) | Technology stack and release engineering | Accepted; govultr added by 0018; extended by 0020; JSON Schema generator and spec decoding libraries added by 0022 |
| [0014](0014-vultr-first-provider-and-e2e.md) | Implement Vultr first and run the E2E suite on Vultr | Accepted |
| [0015](0015-idempotency-without-unique-names.md) | Idempotent creation on clouds without unique names | Accepted; amended by 0023 |
| [0016](0016-server-discovery-seed-and-refresh.md) | Nomad server discovery with a seed list and tent-node refresh | Accepted |
| [0017](0017-api-driven-server-removal.md) | Remove Nomad servers through the Nomad API; ACPI shutdown is an optimization | Accepted |
| [0018](0018-vultr-provider-design.md) | Vultr provider design | Accepted (item 11 still provisional); amended by 0023 |
| [0019](0019-combined-server-client-role.md) | A combined server+client role for dev and small clusters | Accepted |
| [0020](0020-release-channels-and-ci-conventions.md) | Release channels and CI conventions | Accepted |
| [0021](0021-import-rules.md) | Import rules that list the allowed importers | Accepted |
| [0022](0022-json-schema-from-go-types.md) | Generate the JSON Schema from the Go types | Accepted |
| [0023](0023-vultr-inventory-dedupe-and-images.md) | Vultr inventory, dedupe, images and firewall groups | Accepted |

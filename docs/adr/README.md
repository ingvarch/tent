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
| [0004](0004-layered-architecture.md) | Layered architecture: a cloud-agnostic core and providers that translate intents | Accepted; provider order changed by 0014; narrowed by 0021; amended by 0035 |
| [0005](0005-immutable-nodes-and-nomad-aware-rollouts.md) | Immutable nodes and Nomad-aware rolling updates | Accepted; amended by 0017, 0030, 0031, 0032, 0035 and 0037 |
| [0006](0006-two-binaries-and-nodeconfig.md) | Two binaries and a versioned NodeConfig contract | Accepted; extended by 0026; amended by 0027 and 0028 |
| [0007](0007-security-baseline.md) | Security baseline: PKI, mTLS, ACL, client introduction | Accepted; see 0019; amended by 0024, 0029 and 0033 |
| [0008](0008-node-credential-delivery.md) | Node credential delivery: user data in v1, bootstrap controller as the target | Accepted; exception in 0019; amended by 0029 and 0032 |
| [0009](0009-server-discovery-fixed-ip-slots.md) | Nomad server discovery on Hetzner through fixed private IP slots | Accepted (Hetzner-specific; generic strategy in 0016) |
| [0010](0010-state-store-and-locking.md) | State store backends, layout and locking | Accepted; see 0015 |
| [0011](0011-nomad-only-scope-and-licensing.md) | Nomad-only scope for v1 and licensing boundaries | Accepted; extended by 0026 |
| [0012](0012-testing-strategy.md) | Testing strategy: fakes, golden integration tests, E2E on Hetzner | Accepted; E2E platform amended by 0014; E2E running and marking amended by 0034 |
| [0013](0013-technology-stack.md) | Technology stack and release engineering | Accepted; govultr added by 0018; extended by 0020; JSON Schema generator and spec decoding libraries added by 0022 |
| [0014](0014-vultr-first-provider-and-e2e.md) | Implement Vultr first and run the E2E suite on Vultr | Accepted; E2E rules amended by 0034 |
| [0015](0015-idempotency-without-unique-names.md) | Idempotent creation on clouds without unique names | Accepted; amended by 0023 and 0032 |
| [0016](0016-server-discovery-seed-and-refresh.md) | Nomad server discovery with a seed list and tent-node refresh | Accepted; amended by 0027, 0030, 0031 and 0035 |
| [0017](0017-api-driven-server-removal.md) | Remove Nomad servers through the Nomad API; ACPI shutdown is an optimization | Accepted; amended by 0030, 0031, 0035 and 0036 |
| [0018](0018-vultr-provider-design.md) | Vultr provider design | Accepted (item 11 still provisional); amended by 0023, 0027, 0032, 0033 and 0034 |
| [0019](0019-combined-server-client-role.md) | A combined server+client role for dev and small clusters | Accepted; amended by 0031, 0032 and 0033 |
| [0020](0020-release-channels-and-ci-conventions.md) | Release channels and CI conventions | Accepted; amended by 0028 |
| [0021](0021-import-rules.md) | Import rules that list the allowed importers | Accepted; extended by 0025, 0026, 0027, 0028, 0031, 0033, 0034 and 0035 |
| [0022](0022-json-schema-from-go-types.md) | Generate the JSON Schema from the Go types | Accepted |
| [0023](0023-vultr-inventory-dedupe-and-images.md) | Vultr inventory, dedupe, images and firewall groups | Accepted |
| [0024](0024-cluster-pki-storage-and-certificates.md) | Cluster PKI storage and certificate details | Accepted; amended by 0031 and 0033 |
| [0025](0025-stdlib-only-helper-packages.md) | Standard-library-only helper packages | Accepted; amended by 0027 |
| [0026](0026-channels-and-release-assets.md) | Channels and release assets | Accepted; M2.3 follow-ups moved to M2.7 by 0027; amended by 0028 and 0031 |
| [0027](0027-nodeconfig-contract-rendering-and-spec-hash.md) | NodeConfig contract, rendering and spec hash | Accepted; amended by 0028, 0029, 0030, 0031 and 0032 |
| [0028](0028-tent-node-agent-units-and-delivery.md) | tent-node agent, units and delivery | Accepted; amended by 0029 and 0030 |
| [0029](0029-host-firewall-runtime-and-cni-on-nodes.md) | Host firewall, container runtime and CNI plugins on nodes | Accepted; amended by 0030 and 0032 |
| [0030](0030-nomad-on-nodes.md) | Nomad on nodes | Accepted; amended by 0031, 0032 and 0033 |
| [0031](0031-bootstrap-in-update.md) | Bootstrap in `update` | Accepted; amended by 0032, 0033, 0034 and 0036 |
| [0032](0032-joined-label-scrub-and-delete-guard.md) | The joined label, the scrub and the delete guard | Accepted; amended by 0033 and 0037 |
| [0033](0033-operator-commands.md) | Operator commands: validate, export nomad and ui | Accepted |
| [0034](0034-e2e-suite-on-vultr.md) | The E2E suite on Vultr | Accepted |
| [0035](0035-rollout-decisions.md) | Rollout decisions | Accepted; refusal of two voters and single servers provisional (answered, built in M3.4); amended by 0036 and 0037 |
| [0036](0036-nomad-calls-of-a-roll.md) | The Nomad calls of a roll | Accepted |
| [0037](0037-rolling-update-of-client-groups.md) | Rolling update of client groups | Accepted |

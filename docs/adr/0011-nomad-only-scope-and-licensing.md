# ADR-0011: Nomad-only scope for v1 and licensing boundaries

- **Status:** Accepted. The maintainer confirmed on 2026-09-25 that Consul and Vault are out of v1 and that tent is
  licensed under Apache-2.0.
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0006](0006-two-binaries-and-nodeconfig.md), [ADR-0013](0013-technology-stack.md),
  [platform notes: Nomad](../platform-notes.md#1-nomad)

## Context

- **Versions.** Nomad CE 2.0.7 is current as of 2026-09-25. Nomad 2.0 introduced IBM-style versioning, and CE gets
  a two-year base backport policy per release.
- **Features tent relies on:**
  - client introduction (1.11);
  - `server_join {}`, now required because `server.retry_join` is removed in 2.1;
  - native service discovery, Variables and Workload Identity, which make Consul and Vault optional for many
    clusters.
- **Licensing:**
  - Nomad is licensed under BUSL 1.1 since 1.7.
  - The Go module `github.com/hashicorp/nomad/api` is MPL-2.0. It has no semver tags and needs Go 1.26+.
  - Packages in the root module (for example `helper/tlsutil`) are BUSL.
  - BUSL's competitive-offering clause excludes products that are not provided on a paid basis.
- **No fork.** No maintained community fork of Nomad exists; OpenNood has been dormant since 2023.
- **Enterprise-only features:** per-node-pool scheduler configuration and the snapshot agent.

## Decision

- **v1 deploys Nomad CE only**, with native service discovery.
  - Consul and Vault are out of scope for v1. They can be added later as optional node components and cluster
    integrations.
  - Confirmed by the maintainer on 2026-09-25.
- **Supported Nomad versions.**
  - The minimum is 2.0.x: `server_join {}` only, and client introduction available.
  - Channels list exactly which versions are tested and recommended.
- **Only the MPL-2.0 API module is imported.**
  - tent imports `github.com/hashicorp/nomad/api`, pinned by pseudo-version, and only from `internal/nomadops`.
  - The root module `github.com/hashicorp/nomad` must never be imported. depguard enforces this.
- **Official binaries only.** Nodes download official binaries from `releases.hashicorp.com` at bootstrap, verified
  as in [ADR-0006](0006-two-binaries-and-nodeconfig.md). tent never redistributes Nomad binaries.
- **Enterprise features are out of scope.** tent does not configure per-pool scheduler settings. Backups use the CE
  snapshot API through `tent backup`.
- **Documentation.** The README states that Nomad itself is BUSL-licensed and that users accept HashiCorp's terms
  when tent installs it.

## Consequences

### Positive

- A smaller v1 that ships sooner.
- A clean licensing position for a free tool: tent's own licence, Apache-2.0, is compatible with its MPL-2.0
  dependencies.

### Negative / trade-offs

- **No service mesh (Consul Connect) in v1.** Clusters that need it must wait for the Consul component or configure
  it themselves through `extraConfig`.
- **Commercial use.** A paid managed-Nomad offering built on tent would need a commercial Nomad licence. This is not
  legal advice.
- **No fallback.** If HashiCorp or IBM changes the licence or distribution again, there is no community fork to
  fall back to.

### Follow-ups

- Add the Apache-2.0 `LICENSE` file in M0.

## Alternatives considered

- **Consul and Vault in v1.** Triples the bootstrap, PKI and upgrade surface before the core is proven.
- **Importing Nomad root-module helpers** (TLS generation, config structs). A BUSL contamination risk, and heavy
  dependencies.
- **Supporting Nomad 1.x.** It lacks client introduction and would need the deprecated join syntax. Not worth it for
  a new tool.

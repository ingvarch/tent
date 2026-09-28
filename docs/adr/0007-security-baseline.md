# ADR-0007: Security baseline: PKI, mTLS, ACL, client introduction

- **Status:** Accepted; the `access.api` default was decided on 2026-09-25 (see below); combined nodes get both
  certificate names ([ADR-0019](0019-combined-server-client-role.md)); amended by
  [ADR-0024](0024-cluster-pki-storage-and-certificates.md) (the active signer, the CA's validity, certificate details)
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0008](0008-node-credential-delivery.md), [architecture §9](../architecture.md#9-security)

## Context

A cluster created by tent should be secure without extra work from the operator. The relevant Nomad and Hetzner
facts are verified in the platform notes:

- **Nomad TLS.** Nomad supports mTLS for RPC and HTTP and verifies server hostnames. The expected certificate names
  are `server.<region>.nomad`, `client.<region>.nomad` and `cli.<region>.nomad`. `nomad tls` uses ECDSA P-256, with a
  5-year CA and 1-year certificates.
- **ACL bootstrap with an operator-supplied secret.** Supported since 1.3.2. The call is not idempotent.
- **Client introduction (1.11+).** Short-lived tokens gate a client's first registration. Enforcement is
  `none`, `warn` or `strict`. The token cannot be put into the configuration file.
- **The `nomad/api` client** supports `TLSServerName` and in-memory PEM material.
- **Hetzner Cloud Firewalls** filter only public interfaces and do not apply to load balancers.
- **Hetzner API tokens** are project-wide: Read, or Read & Write.

## Decision

- **PKI.**
  - One CA per cluster, using ECDSA P-256. The private key lives only in the state store.
  - The CA is stored as a bundle (`ca-bundle.pem` plus the active signer id) so that CA rotation can be added later
    without a format change.
  - Each node gets its own certificate (`server.<region>.nomad` or `client.<region>.nomad`, plus `localhost` and
    `127.0.0.1`) valid for 1 year. Renewal happens by node replacement, and `tent validate` warns 30 days before
    expiry.
  - Certificates contain no IP addresses. The CLI reaches servers by public IP with
    `TLSServerName = server.<region>.nomad`.
- **Transport.**
  - `tls { http = true, rpc = true, verify_server_hostname = true }`.
  - `verify_https_client = true` by default. `tent ui` keeps browser access practical.
  - RPC and Serf bind to the private network only. HTTP binds to localhost plus the private address on clients, and
    to all addresses on servers, behind the Cloud Firewall.
- **Gossip** encryption is always on (servers).
- **ACL.**
  - ACLs are always enabled.
  - tent generates the bootstrap secret (a UUID) and stores it **before** calling `POST /v1/acl/bootstrap`. On retry
    it verifies the stored token with `GET /v1/acl/token/self`.
  - Humans never receive the bootstrap token. `tent export nomad` issues a separate token with a TTL.
- **Client introduction.**
  - `client_introduction { enforcement = "strict" }` by default.
  - tent issues one intro token per client VM right before creating it. The token is bound to the node name and node
    pool, with a TTL of at most 30 minutes.
- **Operator access.**
  - `tent export nomad` issues a short-lived `cli.<region>.nomad` certificate (24 hours by default) and an ACL token
    with a TTL.
  - `tent ui` runs a local reverse proxy that injects mTLS and the token.
- **Perimeter.**
  - Cloud Firewall rules come from access intents: SSH from `access.ssh`, 4646 to servers from `access.api`, ICMP.
    Everything else is dropped.
  - Firewalls are applied both by label selector and explicitly at server creation.
  - Host nftables blocks the metadata endpoint for workloads.
- **Credentials.**
  - Cloud tokens and state store credentials never go into specs, logs or nodes.
  - One Hetzner project per cluster or environment is recommended.

## Consequences

### Positive

- Secure defaults with no manual certificate or token work, and a small blast radius per credential.
- CA rotation is possible later without migrating the storage format.

### Negative / trade-offs

- Certificate renewal requires rolling the cluster, at least yearly. `validate` warns ahead of time.
- `strict` client introduction makes client creation depend on a reachable, healthy server API. This is true in
  every tent flow anyway.
- `verify_https_client = true` makes direct browser access awkward, which `tent ui` solves.
- The default for `access.api` is `[0.0.0.0/0]`: the API is protected by mTLS and ACL, as in kops. `validate` and
  every mutating command warn loudly while it is open to the whole internet (decided 2026-09-25).

### Follow-ups

- `internal/pki` and `nomadops` ACL/intro token helpers (M2).
- CA rotation, scoped operator policies and nftables restrictions for private traffic come later.

## Alternatives considered

- **Nomad's own `nomad tls` CLI to generate certificates.** It shells out to a BUSL binary, and the certificate
  generation code sits in the root module, which we must not import. The standard library `crypto/x509` is enough.
- **A shared client certificate** for all nodes. No per-node revocation or attribution.
- **`verify_https_client = false`** by default. Nomad documents it as acceptable with ACLs, but mTLS on HTTP is
  cheap defence in depth now that `tent ui` exists.
- **Exporting the bootstrap token** to operators. A permanent god-token on laptops.

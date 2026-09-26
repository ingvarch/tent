# ADR-0008: Node credential delivery: user data in v1, bootstrap controller as the target

- **Status:** Accepted; combined server+client nodes are an exception to "servers run no workloads"
  ([ADR-0019](0019-combined-server-client-role.md))
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0006](0006-two-binaries-and-nodeconfig.md), [ADR-0007](0007-security-baseline.md),
  [architecture §9.4–9.5](../architecture.md#94-secrets-on-nodes-threat-model)

## Context

Every node needs secrets before Nomad can start:

- a TLS key and certificate;
- the gossip key, on servers;
- an intro token, on clients.

What makes delivery hard on Hetzner:

- There is no IAM and no signed instance identity.
- `user_data` stays readable through the metadata service (`169.254.169.254`) from inside the VM for its whole
  lifetime. That includes containers unless blocked, and Hetzner firewalls do not apply to metadata traffic.

How kops handles this on Hetzner, which we do not want to repeat:

- S3 credentials for the state store sit in plaintext in the user data of control-plane nodes.
- A project-wide Read & Write Hetzner token is copied into the cluster.
- For worker nodes, kops-controller verifies identity with a cloud API lookup plus a challenge callback to the
  node's private IP.

Looking ahead to AWS: instances in an autoscaling group share one launch template, so per-instance secrets cannot be
put into user data at all.

## Decision

- **Credential delivery is a strategy** chosen by the provider's capabilities and group management mode. Planned
  strategies:
  - `userdata` (v1);
  - `controller` (v2);
  - possibly `iam-s3` on AWS.
- **v1 (`userdata`)** applies whenever tent creates each VM itself, as on Hetzner:
  - Per-node secrets go into that node's user data: the node's own certificate and key, the gossip key for servers,
    and the intro token for clients.
  - **Metadata is blocked for workloads.** nftables drops traffic to the metadata endpoint from forwarded (container)
    traffic and from non-root local processes.
  - Servers are dedicated and run no workloads.
  - Intro tokens are bound to one node name and pool and expire within 30 minutes.
  - **Never on nodes:** cloud API tokens and state store credentials.
- **Target (v2, `controller`)**, mandatory before ASG-style groups exist. A bootstrap controller runs on the servers:
  1. The node generates its key locally and sends a CSR with its claimed instance id.
  2. The controller checks the claim against the cloud API. On Hetzner it checks the labels, private IP, creation
     time and that the node is not already registered. On AWS it verifies the signed identity document.
  3. On Hetzner it also calls back to the API-reported private IP with a one-time challenge.
  4. It returns the certificate and the intro token.

  After that, user data contains no secrets.

## Consequences

### Positive

- v1 is simple and has no extra moving parts. Its blast radius is limited to one node's own identity, and servers
  run no workloads.
- It is strictly better than the kops approach on Hetzner: no bucket credentials and no project token anywhere on
  nodes.
- The strategy seam makes the v2 controller and the AWS options additive.

### Negative / trade-offs

- In v1, anything running as root on a node can read that node's user data forever. We accept this, since root on a
  node is already game over for that node.
- If the metadata block regresses, workloads could read node secrets. E2E must assert that the block is in place.
- The v2 controller adds a component that must be highly available and must itself be bootstrapped (servers still
  use user data).

### Follow-ups

- nftables rules and an E2E assertion that containers cannot reach `169.254.169.254` (M2).
- Design and implementation of the bootstrap controller come after M3.

## Alternatives considered

- **State store credentials in user data** so that nodes fetch their secrets (kops). Long-lived credentials that can
  read every secret of every cluster in the bucket.
- **A read-only Hetzner token on nodes** (for example for `exec=` discovery). The token sees the whole project, and
  Hetzner has no finer scopes.
- **One shared client certificate** baked into all clients. No per-node attribution or revocation.
- **Delivering secrets over SSH after boot.** Needs the operator online and SSH reachability, and it breaks
  autonomous bootstrap.

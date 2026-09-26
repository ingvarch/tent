# ADR-0018: Vultr provider design

- **Status:** Accepted. The spike runs of 2026-09-25 resolved items 1, 3, 4, 5 and 7:
  - item 1: tags use `key=value`, verbatim and always lower-case, because the tag filter is case-insensitive; markers
    are stored verbatim;
  - item 3: `/16` is accepted;
  - item 4: firewall groups filter only the public interface. The image's ufw blocks Nomad ports on the VPC too, so
    tent-node must own the host firewall, as decided;
  - item 5: the API has no user_data limit up to 4 MiB, and tent keeps a 64 KiB budget, verified end to end.
    Scrubbing works: the metadata service serves the stub within seconds, and cloud-init does not re-run after a
    restart;
  - item 7: `halt` is a hard power-off.

  Still **provisional**: item 11 (Object Storage conditional writes). The context's claim that vendor data upgrades
  packages before SSH is outdated: the vendor data now sets `package_upgrade: false` itself, and tent keeps setting
  it. See [platform notes §3.16](../platform-notes.md#316-spike-runs-2026-09-25).
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR-0014](0014-vultr-first-provider-and-e2e.md), [ADR-0015](0015-idempotency-without-unique-names.md),
  [ADR-0016](0016-server-discovery-seed-and-refresh.md), [ADR-0017](0017-api-driven-server-removal.md),
  [architecture §11](../architecture.md#11-vultr-provider), [platform notes §3](../platform-notes.md#3-vultr)

## Context

Vultr is the first implemented provider ([ADR-0014](0014-vultr-first-provider-and-e2e.md)). The facts that shape its
design are in [platform notes §3](../platform-notes.md#3-vultr).

- **Instances.**
  - Labels are not unique; tags are plain strings on instances only.
  - One firewall group per instance.
  - A VPC can be attached only at creation without a reboot.
  - Readiness is `active` + `running` + `ok`.
- **No private-IP control.** No fixed private IPs, and VPC 2.0 is gone.
- **Power.** No graceful shutdown in the API.
- **user_data.** Its size limit is undocumented, it can be changed by `PATCH`, and it stays readable from inside the
  VM without authentication.
- **Images.** Vultr images enable ufw or firewalld, and vendor data upgrades packages before SSH starts.
- **Topology.** No availability zones and no anti-affinity. x86 only.
- **API.**
  - 30 requests/s per IP, with 429 and `Retry-After`.
  - govultr retries POST requests, and its errors are untyped.
  - IAM (2026) supports action-level policies, OIDC role trusts and an STS-compatible AssumeRole.

## Decision

1. **Ownership and labels.**
   - The canonical labels (`tent/cluster`, `tent/nodegroup`, `tent/role`, `tent/spec-hash`, `tent/op`, `tent/e2e`)
     are encoded as instance tags by a reversible codec in `internal/cloud/vultr/labels.go`.
     **Provisional:** the syntax is `key=value` (for example `tent/cluster=prod`) if the spike shows that `/` and `=`
     are accepted and stored verbatim; otherwise `key:value` with `/` replaced by `.`.
   - Instances are listed by the single cluster tag on the server side. All other filtering happens in tent.
   - Other resources carry a marker `tent:cluster=<c>;kind=<kind>[;role=…][;op=…]` in their single free-text field:
     VPC and firewall group `description`, load balancer `label`, SSH key `name`. tent lists them per region and
     filters on the client. **Provisional:** the spike checks that these fields store the marker verbatim.
2. **Instances.**
   - One `POST /v2/instances` with `attach_vpc`, `firewall_group_id`, `sshkey_id`, the tags, `label` = `hostname` =
     `<cluster>-<group>-<index>`, `os_id`, `backups: "disabled"` and `user_data`.
   - `os_id` is resolved from the image name through `GET /v2/os`; Ubuntu 24.04 is 2284.
   - Poll `GET /v2/instances/{id}` until `status=active`, `power_status=running` and `server_status=ok`.
   - Read the private IP and MAC from `GET /v2/instances/{id}/vpcs`.
3. **Network.**
   - One VPC per cluster (region-scoped; Vultr allows at most 5 VPCs per region), attached only at instance creation.
   - The CIDR comes from `networking.cidr`. **Provisional:** the spike records which masks Vultr accepts. If `/16` is
     rejected, the Vultr default becomes the largest accepted mask.
   - The private interface is found by MAC or CIDR, never by name. MTU is 1450.
4. **Firewall.**
   - Two firewall groups per cluster, `servers` and `clients`, because an instance has exactly one group. They carry
     only the internet-facing access intents.
   - tent-node owns the host firewall. It disables ufw or firewalld and applies tent's nftables ruleset: Nomad ports
     and the dynamic port range from the cluster CIDR only, plus the metadata block for workloads.
   - **Provisional:** the spike checks that firewall groups do not filter VPC traffic, and what the image's default
     host firewall blocks.
5. **user_data.**
   - The cloud-config sets `package_update: false` and `package_upgrade: false`.
   - `MaxUserDataBytes` is the limit the spike measures, minus a 25% margin.
   - After a node registers in Nomad, tent PATCHes its user data to a non-secret stub (`Nodes.ScrubUserData`).
     **Provisional:** this requires that the metadata service then serves the stub and that cloud-init does not re-run
     on reboot. If either fails, scrubbing is dropped and the threat model reverts to the Hetzner one.
6. **Zones and placement.**
   - `cloud.zones` must equal `[cloud.region]`, and the Nomad `datacenter` is the region id.
   - `validate` warns that the cluster has a single failure domain and no host anti-affinity.
7. **Removal.** `Nodes.Stop` is a hard stop (`GracefulShutdown=false`). Removal follows
   [ADR-0017](0017-api-driven-server-removal.md). **Provisional:** the spike confirms that `halt` is a hard power-off.
8. **API client.**
   - govultr v3, pinned.
   - Automatic retries off for non-idempotent calls ([ADR-0015](0015-idempotency-without-unique-names.md)).
   - A tent-side token bucket, 10 requests/s by default, that honours `Retry-After`.
   - A wrapper that turns govultr's untyped errors into typed ones (not found, rate limited, limit reached,
     invalid).
9. **Credentials.**
   - `VULTR_API_KEY`, for a service user whose IAM policy allows only the needed actions: compute instances, VPCs,
     firewalls, SSH keys, and optionally load balancers and object storage.
   - An IP allow-list where CI egress is stable.
   - OIDC role trusts for short-lived CI credentials are to be evaluated.
10. **Availability and cost.**
    - The preflight uses `GET /v2/regions/{id}/availability`.
    - Hourly cost is `monthly_cost / 672`, not the API's `hourly_cost`. Billing has a one-hour minimum, and
      `location_cost` multipliers apply.
    - Errors that mention account limits are shown verbatim.
11. **State store.**
    - E2E uses `file://`.
    - Operators may use Vultr Object Storage (from $18/month; keys can be created via the API) or any S3.
    - **Provisional:** the spike checks `If-None-Match` for locking.
12. **Private topology (later):** `vpc_only` instances behind a managed NAT gateway.

## Consequences

### Positive

- A complete, testable provider design despite Vultr's weaker primitives.
- Scrubbed user data gives a better secret posture than Hetzner's.
- Vultr's API headroom makes the provider fast to drive.

### Negative / trade-offs

- Several items depend on undocumented behaviour and stay provisional until the spike has run.
- One failure domain per cluster.
- Filtering happens on the client for everything except instances.

### Follow-ups

- Run `hack/vultr-spike`, update the platform notes, and resolve each provisional item by editing this ADR's status
  line (or by a superseding ADR if a decision changes).
- M1 implementation: label codec, client wrapper, tasks, `Nodes`, inventory and fakes.

## Alternatives considered

- **Encode ownership only in labels or hostnames.** Instances cannot be filtered by label on the server side reliably:
  exact versus substring matching is undocumented. Tags are the supported filter.
- **Rely on Vultr's default host firewall.** Its rules are image-dependent and unknown, so tent must own the host
  firewall to guarantee Nomad connectivity and the metadata block.
- **Use the new Vultr "Clusters" API** (instance pools from templates) for node groups. It is not in govultr yet and
  it is poorly documented. It may back `ManagedGroups` later.

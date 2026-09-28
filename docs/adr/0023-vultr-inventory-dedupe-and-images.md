# ADR-0023: Vultr inventory, dedupe, images and firewall groups

- **Status:** Accepted. The spike of 2026-09-27 settled decision 6
  ([platform notes §3.16](../platform-notes.md#316-spike-runs)):
  - Vultr accepts a second SSH key with the same key material;
  - it lists a rule as sent, with the rule's own subnet as its `source`, and refuses a second copy of a rule;
  - it stores VPC and firewall group descriptions of up to 255 characters and SSH key names of up to 128 verbatim.
    The spike set them with updates: `PUT` for a VPC or a firewall group, `PATCH` for an SSH key;
  - it deletes a firewall group that instances use (204) and leaves them without a firewall group. So tent never
    deletes a firewall group that nodes of the cluster use: it reports a duplicate and refuses to delete a group
    that the cluster no longer wants. This is the firewall part of the M1.4 follow-up
    ([architecture §11.5](../architecture.md#115-firewall-and-host-firewall));
  - Neither `GET /v2/firewalls/{id}` nor `GET /v2/firewalls` had an `instance_count` (2026-09-27 and 2026-09-28).
    So the inventory counts the attached instances of decision 2 itself: it lists the instances with the cluster's
    tag once, one list call more than decision 1 names, and counts those in each firewall group. It does not read
    `instance_count`. The search by operation id lists no instances, so of several copies of a firewall group it
    returns the oldest. Right after a create no node uses the copies yet, so the inventory keeps the oldest too.

  The VPC part of the M1.4 follow-up waits for the `update cluster` and `delete cluster` work: keeping the VPC that
  holds the cluster's nodes, and refusing to delete a VPC that nodes use. Until then Vultr refuses such a delete
  with `ErrInUse`, the engine retries it until the change's deadline, and then the apply fails.
- **Date:** 2026-09-27
- **Deciders:** ingvarch
- **Related:** amends [ADR-0015](0015-idempotency-without-unique-names.md) (the dedupe pass) and
  [ADR-0018](0018-vultr-provider-design.md) (items 1, 2 and 4); [ADR-0019](0019-combined-server-client-role.md),
  [architecture §6](../architecture.md#6-reconciliation-engine), [§11.1](../architecture.md#111-resources),
  [§11.5](../architecture.md#115-firewall-and-host-firewall),
  [§11.7](../architecture.md#117-zones-placement-and-availability),
  [platform notes §3.6](../platform-notes.md#36-firewall-groups),
  [§3.10](../platform-notes.md#310-ownership-fields-on-other-resources)

## Context

Building the Vultr inventory and the tasks for SSH keys, the VPC and the firewall groups showed where the accepted
ADRs do not fit Vultr or leave a case open:

- **Listing.** [ADR-0018](0018-vultr-provider-design.md) item 1 lists the resources without tags per region. SSH keys
  and firewall groups have no region, and `GET /v2/vpcs` has no documented region filter.
- **Dedupe.** [ADR-0015](0015-idempotency-without-unique-names.md) keeps, of several copies with one key, the one
  whose operation id matches the plan. The inventory runs before the plan, and each task makes a new operation id
  when it is built. No copy in Vultr carries the ids of the current plan.
- **A copy the list does not show yet.** A create whose answer was lost searches by its operation id. When the list
  does not show the lost copy yet, the next attempt creates a second one. The task then returns the id of the newer
  copy, while the inventory of the next run keeps the older one and deletes the newer one.
- **Images.** ADR-0018 item 2 resolves `os_id` from the image name through `GET /v2/os`. Vultr's names are free text,
  such as `Ubuntu 24.04 LTS x64`, and a match by name breaks when Vultr renames an image or adds a variant.
- **Firewall groups.** ADR-0018 item 4 gives each cluster a `servers` and a `clients` group. The combined role
  ([ADR-0019](0019-combined-server-client-role.md)) came later, and a cluster may have no client group. Architecture
  §9.6 allowed ICMP without saying from where.
- **The network.** An instance joins a VPC at creation; a later attach reboots it
  ([platform notes §3.5](../platform-notes.md#35-vpc)). Moving a cluster to another VPC needs every node replaced.
- **Unverified behaviour.** The tasks rely on Vultr behaviour that no spike run has checked yet
  ([platform notes §3.6](../platform-notes.md#36-firewall-groups),
  [§3.10](../platform-notes.md#310-ownership-fields-on-other-resources)).

The maintainer confirmed these decisions on 2026-09-27.

## Decision

1. **Inventory.** tent lists SSH keys, VPCs and firewall groups over the whole account, with one list call for each
   kind, and filters them on the client by their markers. Then it lists the rules of each firewall group it keeps.
   This replaces "per region" in ADR-0018 item 1.
2. **Dedupe on Vultr.** This replaces "the one whose operation id matches the plan, otherwise the oldest" in
   ADR-0015 for Vultr. Of the owned copies with one key, the inventory keeps:
   - of firewall groups, the one with the most attached instances, then the oldest, then the one with the lowest id;
   - of SSH keys and VPCs, the oldest, then the one with the lowest id.

   A creation date that does not parse counts as newer than any that does. The other copies are duplicates, and the
   plan deletes them.
   - The search by operation id uses the inventory's rule for which objects the cluster owns, and of several copies
     it returns the one the inventory keeps.
   - A create that follows a search that found nothing searches once more after it succeeds, and returns the copy
     the inventory keeps. When that search fails or finds nothing, it returns the created id.
   - A VPC or a firewall group that instances still use is handled with the nodes (see Follow-ups).
3. **Images.** tent maps image names to `os_id` with its own table: `ubuntu-24.04` → 2284 and `ubuntu-26.04` → 2760.
   The preflight checks each id against `GET /v2/os` and fails for an image name that is not in the table. This
   replaces "resolved from the image name through `GET /v2/os`" in ADR-0018 item 2.
4. **Firewall groups.** This narrows ADR-0018 item 4:
   - `<cluster>-servers` always exists and serves the server or combined group;
   - `<cluster>-clients` exists only when the cluster has a client group;
   - the groups hold the internet-facing rules of the model: 22/tcp from `access.ssh` to every node, ICMP from
     anywhere in IPv4 and IPv6 (`0.0.0.0/0` and `::/0`) to every node, and 4646/tcp from `access.api` to the
     servers.
5. **The VPC.** A VPC's region and CIDR never change. When the cluster's VPC has another region or CIDR than the
   spec, the plan fails and names both.
6. **Unverified behaviour.** Four facts are marked 🔬 in the platform notes
   ([§3.6](../platform-notes.md#36-firewall-groups),
   [§3.10](../platform-notes.md#310-ownership-fields-on-other-resources)): whether Vultr accepts a second SSH key
   with the same key material, the form in which Vultr lists firewall rules, the longest description and SSH key
   name that Vultr stores verbatim, and Vultr's answer to the delete of a firewall group that instances still use.
   A spike run settles them before the Nodes and `update cluster` work relies on them.

## Consequences

### Positive

- An inventory takes three list calls plus one per kept firewall group, however many regions the account uses.
- The inventory and the search share one ownership rule and one keep rule, so a task adopts the copy that the next
  run keeps.
- Keeping the firewall group with the most instances leaves the running nodes in the group they have.
- A renamed image in Vultr's catalogue does not change what tent deploys.
- A cluster without clients has no empty clients group. ICMP from anywhere keeps path MTU discovery and ping working
  for every peer.

### Negative / trade-offs

- The lists grow with every SSH key, VPC and firewall group in the account, not only the cluster's.
- A new image needs a new tent release.
- The keep rule knows nothing of nodes yet. The oldest VPC may not be the one that holds the cluster's instances,
  and the dedupe would then try to delete the VPC in use.
- When the lost copy shows up only after the second search, the task returns the newer id, and the next run deletes
  that copy as a duplicate.
- ICMP is open to the whole internet.

### Follow-ups

- Run the spike for the four 🔬 facts before M1.4 relies on them, and record the results in the platform notes.
- M1.4: never mark as a duplicate, or refuse to delete, a VPC or a firewall group that instances still use; report
  it instead, as ADR-0015 does for instances registered in Nomad. Once the nodes inventory exists, keep the VPC that
  holds the cluster's instances.

## Alternatives considered

- **List per region** (ADR-0018 item 1). SSH keys and firewall groups have no region, and the VPC list has no
  documented region filter.
- **Keep the copy whose operation id matches the plan** (ADR-0015). No copy in Vultr carries an operation id of the
  current plan.
- **Keep the newest copy.** A copy created later by accident would displace the one that earlier runs set up.
- **Return the created id after a create that followed an empty search.** The next inventory keeps the older lost
  copy, so the task's dependents would get the id of a copy that the next run deletes.
- **Resolve `os_id` by name through `GET /v2/os`** (ADR-0018 item 2). Vultr's names are free text and may change.
- **A clients group in every cluster.** It would hold rules that no node uses.
- **ICMP from `access.ssh` only.** Path MTU discovery needs ICMP from every peer, and IPv6 depends on it.
- **Replace the VPC when its CIDR or region changes.** Every node would have to be replaced first, which tent does
  not do yet; the plan fails instead of cutting the nodes off their network.

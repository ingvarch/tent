# Platform notes

Facts about Nomad, Hetzner Cloud, Vultr, S3-compatible object stores and prior art that tent's design relies on.

> **Verified on 2026-09-25**, and [section 5](#5-s3-compatible-object-stores) on 2026-09-26. Sources: official
> documentation, the Hetzner Cloud OpenAPI spec (`https://docs.hetzner.cloud/cloud.spec.json`), the Vultr API
> reference (OpenAPI spec from a Wayback copy of `https://www.vultr.com/api/`, 2026-09-12) plus live calls to public
> Vultr endpoints, release APIs and upstream source code (Nomad `main`, kops `master`, hcloud-go, govultr,
> cloud-init). The confidence is high unless marked otherwise.
>
> Items marked 🔬 are **unverified** and are checked with `hack/vultr-spike` against a real account before code
> depends on them. Facts marked "spike 2026-09-25" were measured by the spike runs of that day (see
> [3.16](#316-spike-runs-2026-09-25)).
>
> Items marked ⏳ are **volatile**: prices, availability, versions, incidents. Re-check them before relying on them.
> When a fact here turns out wrong, fix it and note the date.

## Contents

1. [Nomad](#1-nomad)
2. [Hetzner Cloud](#2-hetzner-cloud)
3. [Vultr](#3-vultr) (spike results: [3.16](#316-spike-runs-2026-09-25))
4. [Prior art](#4-prior-art)
5. [S3-compatible object stores](#5-s3-compatible-object-stores)
6. [Sources](#6-sources)

---

## 1. Nomad

### 1.1 Releases and support ⏳

- **Current release.** The latest stable CE release is **2.0.7** (changelog 2026-09-17, published 2026-09-18).
  `main` is at 2.0.8-dev.
- **Recent major releases:**
  - **1.8.0 (2024-05)**: first Enterprise LTS; exec2 driver beta.
  - **1.9.0 (2024-10)**: HCL1 jobspecs removed; keyring moved into Raft; clients older than 1.6 rejected.
  - **1.10.0 (2025-04)**: dynamic host volumes; legacy Consul/Vault token workflows removed.
  - **1.11.0 (2025-11)**: **client introduction and node identity**; `secret` block.
  - **2.0.0 (2026-04)**: IBM-style versioning (April starts a support lifecycle, October adds features, fixes are
    monthly); "no breaking changes"; Raft WAL log store.
    - 2.0.3 added `acl token create -token` (custom secret) and `client.default_ineligible`.
    - 2.0.4 deprecated unauthenticated `server join`.
- **Support.** For 2.0.0 and later, CE has a two-year base backport policy; 2.0.x base support ends 2028-04-30.

### 1.2 Features tent relies on

**Client introduction** (1.11.0, CE):
- **Server configuration.** In the `server` block:
  `client_introduction { enforcement = "none|warn|strict" (default warn), default_identity_ttl = "5m",
  max_identity_ttl = "30m" }`.
- **Creating a token.** `nomad node intro create [-node-name] [-node-pool] [-ttl]`, or
  `POST /v1/acl/identity/client-introduction-token {NodeName, NodePool, TTL}`, which returns `{"JWT": …}`. Needs
  `node:write`.
- **Giving it to a client.** Use `-client-intro-token`, `NOMAD_CLIENT_INTRO_TOKEN`, or the file
  `<client state_dir>/intro_token.jwt`. It **cannot** be set in the agent configuration file.
- **Lifecycle.** The token is used only for the first registration. After that the node holds an identity JWT with
  a per-pool `node_identity_ttl` (default 24h), renewed at about 67% of its lifetime.
- **Gotchas.**
  - A present but invalid, expired or mismatched token is rejected **even under `warn`**. `warn` only admits
    clients with no token at all.
  - The token's pool must equal the client's `node_pool`.
  - Client introduction does not replace mTLS.

**ACL bootstrap with an operator-supplied secret** (since 1.3.2):
- `POST /v1/acl/bootstrap {"BootstrapSecret": "<UUID>"}`. A non-UUID secret gets HTTP 400.
- The Go client offers `ACLTokens().BootstrapOpts`.
- **Not idempotent.** A second call fails with "ACL bootstrap already done (reset index: N)", HTTP 400 in the
  source. To make tent idempotent, verify the stored secret with `GET /v1/acl/token/self` after such an error.

**Autopilot and Raft:**
- `GET /v1/operator/autopilot/health` (`operator:read`) returns `Healthy`, `FailureTolerance`, `Leader`, `Voters`
  and `Servers[]` (ID, Name, Address, SerfStatus, Version, Leader, LastContact, LastTerm, LastIndex, Healthy, Voter,
  StableSince).
- **It returns HTTP 429 while unhealthy.** The Go `AutopilotServerHealth` then returns an error instead of the
  body, so handle 429 explicitly.
- `cleanup_dead_servers` defaults to true.
- **Leadership transfer** (since 1.7.0): `PUT /v1/operator/raft/transfer-leadership?id=<peer id>` or
  `nomad operator raft transfer-leadership`.
- **Removing a peer:** `DELETE /v1/operator/raft/peer?id=<raft id>` with a management token. `?address=` returns
  400.

**Node pools** (since 1.6.0):
- Client configuration: `client { node_pool = "x" }`, default `default`. The built-ins `default` and `all` cannot
  be modified, and `all` is for jobs only.
- **Pools are auto-created** when a client registers in the authoritative region. A job that references a missing
  pool fails to register.
- **Per-pool `scheduler_config` is Enterprise-only.**

**Joining servers:**
- Use `server_join { retry_join = [...] }`. `server.retry_join`, `start_join`, `retry_interval` and `retry_max`
  will be removed in 2.1.0.
- **go-discover has no Hetzner provider.** Issue #73 has been open since 2018, and PR #167 is still open.
  - Built-in providers: aliyun, aws, azure, digitalocean, gce, linode, mdns, os, packet, scaleway, softlayer,
    tencentcloud, triton, vsphere, and `srv` (DNS SRV, since 2.0.4).
  - Nomad's docs suggest `exec=` (go-netaddrs, since 1.7.0) with the `hcloud` CLI for Hetzner.
  - go-netaddrs runs the command without a shell, so pipes do not work. You need a wrapper that prints
    space-separated IPs, plus `HCLOUD_TOKEN` on the node.

**Agent configuration:**
- **go-sockaddr templates** work in `bind_addr`, `addresses.*`, `advertise.*` and `client.network_interface`.
  `addresses.http` accepts several space-separated addresses.
- **File merging.** `.hcl` and `.json` files in a configuration directory are merged in lexicographic order.
- **Parser.** Agent configuration is still parsed with **HCL1**, with no HCL2 functions.
- **Graceful shutdown settings.**
  - `leave_on_interrupt` and `leave_on_terminate` default to false.
  - With them enabled, a server leaves the peer set gracefully.
  - Clients with `drain_on_shutdown { deadline, force, ignore_system_jobs }` drain themselves on shutdown.
- **Jobs default to `datacenters = ["*"]`** (since 1.5).

**TLS:**
- **Recommended:** `rpc = true`, `http = true`, `verify_server_hostname = true`, `verify_https_client = true`.
  Nomad's docs call `verify_https_client = false` "safe so long as ACLs are enabled".
- **Certificate names:** `server.<region>.nomad`, `client.<region>.nomad` and `cli.<region>.nomad`, plus `localhost`
  and `127.0.0.1`.
- **`nomad tls`** always uses ECDSA P-256. The CA is valid 5 years, other certificates 1 year.
- **The Go API client's `TLSConfig`** has `CACert`, `CAPath`, `CACertPEM`, `ClientCert`, `ClientCertPEM`, `ClientKey`,
  `ClientKeyPEM`, `TLSServerName` and `Insecure`.

**Status and agent endpoints** (used by seed-and-refresh discovery and API-driven server removal):
- **`GET /v1/status/leader` and `GET /v1/status/peers`** require **no ACL token**; mTLS still applies. `peers` returns
  the Raft peers' RPC addresses (`["10.0.0.5:4647", …]`).
- **`GET /v1/agent/servers`** (`agent:read`) lists the servers a client knows.
- **`PUT /v1/agent/servers?address=…`** (`agent:write`) replaces that list. Whether this survives a restart is not
  documented.
- **There is no `/v1/agent/leave`.** No endpoint makes an agent leave gracefully.
- **`PUT /v1/agent/force-leave?node=<name>&prune=true`** (`agent:write`) removes a failed or left member from the Serf
  member list immediately. A member that is still alive rejoins.

**Fingerprinting:**
- The only cloud environment fingerprinters are `env_aws`, `env_gce`, `env_azure` and `env_digitalocean`. There is
  none for Hetzner or Vultr.
- Disable them with `client.options."fingerprint.denylist"`.

**Backups:**
- The snapshot agent is still **Enterprise-only**.
- In CE, use `nomad operator snapshot save` or `GET /v1/operator/snapshot`. Since 1.11.3 the
  `operator:snapshot-save` capability is sufficient.
- Restore with `PUT /v1/operator/snapshot`.
- Snapshots contain the keyring, so treat them as secrets.

**Volumes:**
- Dynamic host volumes (1.10) are CE; only governance features are Enterprise.
- The **hcloud CSI driver**:
  - Its docs say "Nomad is not officially supported". It does ship a Nomad guide (`docs/nomad/README.md`) and runs
    Nomad 2.0.5 E2E in CI.
  - Requirements: plugin id `csi.hetzner.cloud`, privileged Docker, the token stored as a Nomad Variable,
    single-node-writer volumes, a 10 GB minimum.
  - The latest release is v2.23.0 (2026-09-03).

### 1.3 Upgrades and version skew

- **Order.** Servers first, one at a time with health checks, then clients.
- **Methods.** An in-place binary swap and restart is supported (allocations continue), and so is rolling VM
  replacement.
- **Skew.** The rule is "backward compatible for at least 2 point releases". Servers on 1.9+ reject clients older than
  1.6. There are no downgrades.
- **Unverified:** the skew rule has not been restated for the 2.x versioning scheme.

### 1.4 Downloads and verification

- **Binaries:** `https://releases.hashicorp.com/nomad/{V}/nomad_{V}_linux_{amd64|arm64}.zip`. The zip contains
  `nomad` and `LICENSE.txt`.
- **Checksums and signatures:** `nomad_{V}_SHA256SUMS`, plus detached signatures `nomad_{V}_SHA256SUMS.sig` and
  `….72D7468F.sig`.
- **JSON index:** `https://api.releases.hashicorp.com/v1/releases/nomad/{V|latest}`.
- **Release signing key:** `https://www.hashicorp.com/.well-known/pgp-key.txt`, fingerprint
  `C874 011F 0AB4 0511 0D02 1055 3436 5D94 72D7 468F`. The 2.0.7 signature and the linux_amd64 sha256 were checked
  and are valid.
- **APT.** ⏳ `apt.releases.hashicorp.com` provides arm64. Its signing key was **rotated on 2026-09-09** after a
  security incident (HCSEC-2026-33); the new key is `D55C 0D1A C78A 8D81 26CB 631C FC9C A96A CA02 6560`. tent does not
  use the APT repository.

### 1.5 Licensing

- **Nomad is BUSL 1.1 from 1.7.0.** The 1.6.x tags are MPL-2.0.
- **MPL-2.0 modules:** `github.com/hashicorp/nomad/api` (a separate module with no semver tags, so pin by
  pseudo-version; requires Go 1.26+), `jobspec2`, `go-discover`, `go-netaddrs`.
- **Do not import root-module packages** such as `helper/tlsutil`: they are BUSL.
- **Competitive use.** BUSL "competitive offering" covers products provided on a paid basis. A free tool that
  downloads official binaries is fine; a paid managed-Nomad offering would need a commercial licence. This is not
  legal advice.
- **Forks.** No maintained, foundation-backed fork exists. OpenNood/nood (based on 1.6.5) has had no commits since
  December 2023. (Medium confidence.)

---

## 2. Hetzner Cloud

### 2.1 API basics

- **Rate limit.** The default is **3600 requests per hour per project**, refilling at 1 per second, with bursts
  allowed.
  - Headers: `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset` (a UNIX time).
  - Exceeding the limit gives `429 rate_limit_exceeded`.
- **Labels.**
  - A key is an optional prefix (a DNS subdomain of at most 253 characters, followed by `/`) plus a name of at most
    63 characters. The name starts and ends alphanumeric, with `-`, `_` and `.` allowed inside.
  - A value is at most 63 characters with the same character rules, or empty.
  - The `hetzner.cloud/` prefix is reserved.
- **Label selectors:** `k=v`, `k==v`, `k!=v`, `k`, `!k`, `k in (a,b)`, `k notin (a,b)`, combined with commas (AND).
- **Waiting on actions.** There are no webhooks, so you poll actions.
  - `GET /actions?id=…&id=…` batches up to 25 ids per request. hcloud-go's `Action.WaitFor` / `WaitForFunc` does
    this (since v2.8.0).
  - The per-resource action endpoint `GET /<resource>/{id}/actions/{action_id}` is deprecated (2026-04-30).
- **Tokens.** Tokens are per project: **Read** or **Read & Write**. There are no finer-grained permissions.

### 2.2 Capacity, limits and prices ⏳

- **Capacity.** Since 2026-06-26 a status incident says Hetzner **restricts server creation** for new customers and
  some randomly chosen existing customers. Every CX and CAX plan is marked "not available" on the website. A tool
  must:
  - check `server_types[].locations[].available`;
  - handle `412 resource_unavailable`;
  - handle a create action that fails after the server briefly appears.
- **Default limits:**
  - **5 servers per account.** The docs also mention 8 dedicated-vCPU servers.
  - 20 load balancers, 50 firewalls, 30 snapshots and 10 floating IPs.
  - Primary IPs: up to 2× the server limit.
  - 50 placement groups per project.
  - Up to 3 networks per server.
- **Raising limits.** Console → Limits → Request change. Allowed only after one month and a paid first invoice.
- **Server type names changed on 2025-10-16:**
  - CX22/32/42/52 became **CX23/33/43/53**.
  - CPX11–51 became **CPX12/22/32/42/52/62** in FSN, NBG, HEL and SIN. The old CPX types remain in ASH and HIL.
  - CAX11–41 (Arm) and CCX13–63 (dedicated AMD) are unchanged.
  - CX and CAX are EU-only.
  - The API field `deprecated` is removed on 2026-11-02; use `locations[].deprecation`.
- **Prices.** Prices rose for all products on 2026-04-01, and again on 2026-06-15 for new orders and rescales. EU
  prices per month, excluding VAT and IPv4:

  | Type | Specs | €/month |
  |---|---|---|
  | CX23 | 2 vCPU, 4 GB, 40 GB | 5.49 |
  | CX33 | 4 vCPU, 8 GB | 8.49 |
  | CAX11 | 2 Arm vCPU, 4 GB | 5.99 |
  | CAX21 | 4 Arm vCPU, 8 GB | 10.49 |
  | CPX12 | 1 vCPU, 2 GB | 11.49 |
  | CPX22 | 2 vCPU, 4 GB, 80 GB | 19.49 |
  | CPX32 | 4 vCPU, 8 GB | 35.49 |
  | CCX13 | 2 dedicated vCPU, 8 GB | 42.99 |

  - Primary IPv4 costs €0.50 per month; IPv6 is free.
  - The smallest load balancer, LB11, costs €6.99 plus €0.50 for IPv4.
  - US examples: CPX11 €17.49, CCX13 €43.49.
- **Type picks:**
  - Nomad servers: CX23 or CAX11 when in stock, otherwise CPX22.
  - Workers: CX33, CX43 or CAX21–31, otherwise CPX32 or CPX42.
  - CCX for steady CPU.

### 2.3 Locations and network zones

| Network zone | Locations |
|---|---|
| `eu-central` | `fsn1`, `nbg1`, `hel1` |
| `us-east` | `ash` |
| `us-west` | `hil` |
| `ap-southeast` | `sin` |

- **Networks.** All subnets of a network are in one network zone. Servers in different locations of that zone can
  share it, which is useful in practice only in `eu-central`.
- **Location binding.** Load balancers and floating IPs are bound to a network zone; primary IPs to a location.
- **Datacenters are gone.** ⏳ `/datacenters` returns 410 from 2026-10-01, and the `datacenter` field was removed
  from servers and primary IPs on 2026-07-01 (hcloud-go v2.45 removed it). Use locations.

### 2.4 Servers

- **`POST /servers` parameters:**
  - `networks`: IDs only, and the IP is assigned automatically;
  - `start_after_create` (default true);
  - `placement_group`;
  - `firewalls` (applied to the public interface);
  - `public_net.enable_ipv4` / `enable_ipv6`;
  - `ssh_keys`, `labels`, `volumes`, `automount`;
  - `location` (ID or name).
- **`user_data`:**
  - limited to **32 KiB**;
  - cannot be updated (`PUT /servers` changes only the name and labels);
  - since 2026-01-16, a **rebuild**, which wipes the disk, can replace it.
- **`shutdown`** sends an ACPI request, and the action status only reflects that it was sent. Use `poweroff` if the
  server must be off.
- **`DELETE /servers/{id}`** removes the server immediately and detaches volumes, primary IPs, floating IPs,
  firewalls and placement groups.
- **Names.** Server names must be unique per project and be valid hostnames.
- **SSH keys** are unique per fingerprint within a project.

### 2.5 Networks

- **Subnet types:**
  - `cloud` (use this);
  - `server` (deprecated);
  - `vswitch`, which couples a Robot dedicated-server vSwitch into the cloud network. This makes a future
    bare-metal client option possible.
- **`attach_to_network`** takes `network`, `ip` (request a specific IP), `alias_ips` (up to 5, configured on the host
  by hand) and `ip_range` (auto-assign from a specific subnet). Errors: `ip_not_available`, `no_subnet_available`,
  `server_already_attached`.
- **A fixed private IP is only possible through `attach_to_network`.** Create the server with
  `start_after_create: false`, attach it with `ip`, then power it on.
- **Servers with no public IP** must get their network at creation.
- **MTU** is 1450 on the private interface and 1500 on the public one.
- **Gateway.** The gateway is the first IP of the network's range (for example `10.0.0.1`). `172.31.1.1` is reserved
  as the public default gateway.
- **Interface names:**
  - `ens10` on CX\*1 and CCX\*1;
  - `enp7s0` on CX\*2, CX\*3, CPX, CAX, CCX\*2 and CCX\*3, then `enp8s0`, `enp9s0`;
  - `eth1` on CentOS 7.
- **Private interface configuration.** Since 2026-05-18, Ubuntu 22.04+ and Fedora 44 configure the private interface
  through cloud-init 25.3+. Debian ≤13 and Alma/Rocky still use `hc-utils` (`hc-net-ifup@<if>.service`).

### 2.6 Firewalls

- **Scope.** Firewalls filter the **public interface only**. The FAQ answers "Can Firewalls secure traffic to my
  private Networks?" with "Not yet".
- **Where they do not apply:** load balancers ("Not yet"). Metadata requests also bypass them.
- **`apply_to`** takes type `server` or `label_selector`. Resources added directly take precedence over those added
  through a label selector.
- **Default policy.** Inbound defaults to DROP when the firewall has no inbound rules; outbound defaults to ACCEPT.
- **Limits:** 5 firewalls per server, 50 rules and 500 effective rules per firewall.
- **Names** are unique per project.

### 2.7 Placement groups

- Only the `spread` type exists: every server in a group runs on a different physical host.
- At most **10 servers per group**, 50 groups per project and 1 group per server.
- Placement groups are **not bound to a location**.
- A server must be powered off to be added to a group, or it can be set at creation.

### 2.8 Load balancers

- **Targets:** `server`, `label_selector` or `ip` (ip targets only in eu-central). `use_private_ip` requires the load
  balancer to be in the same network.
- **Private networking.** Attach at creation (`network`) or with `attach_to_network` (`ip`, `ip_range`).
  `public_interface: false` makes the load balancer internal-only.
- **Protocols:** `tcp` (passthrough, optional PROXY protocol), `http`, `https` (TLS terminated at the load balancer).
- **Health checks:** tcp or http, with interval, timeout, retries, path and status codes.
- **No source-IP allow-list and no firewall** for load balancers.
- **LB11:** 5 services, 25 targets, 10 certificates, 10k connections and 20 TB of traffic in the EU.

### 2.9 Metadata service

- **Endpoint:** `http://169.254.169.254/hetzner/v1/metadata` returns a YAML summary with `hostname`, `instance-id`,
  `public-ipv4`, `private-networks`, `availability-zone` (for example `hel1-dc2`) and `region` (the network zone, for
  example `eu-central`). User data is at `/hetzner/v1/userdata`.
- **`private-networks`** lists, for each network: `ip`, `alias_ips`, `interface_num`, `mac_address`, `network_id`,
  `network_name`, `network`, `subnet` and `gateway`.
- **EC2-style routes** (`/latest/...`) were removed on 2026-08-01.
- **Access control.** No token or hop-limit protection is documented. Assume that containers using NAT can read user
  data, and block `169.254.169.254` for workloads. (Medium confidence, inferred.)

### 2.10 Object Storage ⏳

- **Endpoints:** `https://{fsn1,nbg1,hel1}.your-objectstorage.com`. EU only.
- **Pricing** is per account: a base of €6.49 per month, billed hourly while any bucket exists. It includes 1 TB of
  storage and 1 TB of egress. Beyond that, storage is €0.0087 per TB-hour and egress €1 per TB. The minimum billable
  object size is 64 KB.
- **S3 credentials can be created only in the Console.** Neither the Cloud API nor hcloud-go manage buckets.
- **Supported:** versioning, Object Lock (enabled at bucket creation), bucket policies and lifecycle rules.
- **Conditional writes.** Conditional PUT and DELETE are unsupported on versioned buckets. `If-None-Match: *` on
  unversioned buckets is undocumented and unverified, so test it before relying on it. Until E2E does, tent treats
  every endpoint on `your-objectstorage.com` as a store without conditional writes ([5.1](#51-conditional-writes-)).
- **Limits:** 100 buckets, 200 credentials and 750 requests per second per bucket.

### 2.11 DNS

- **DNS is part of the Cloud API** (`/zones`, `/zones/{id}/rrsets`, zone file import and export). It was in beta from
  2025-10-07 and generally available from 2025-11-10.
- **hcloud-go** added `ZoneClient` in v2.27.0; it is GA since v2.30.0, with `exp/zoneutil`.
- **The old `dns.hetzner.com` API** went read-only on 2026-05-20 and was removed on 2026-05-27. Old DNS tokens do not
  work with the new API.

### 2.12 Images and snapshots

- **Images** are named by name and architecture, and passing the name picks the right one.
- **Available:** `ubuntu-22.04`, `ubuntu-24.04` and `ubuntu-26.04` (x86 and Arm), `debian-12` and `debian-13`,
  `fedora-43/44`, and Alma and Rocky 8–10.
- **`debian-11`** is deprecated and unavailable after 2026-11-30.
- **Snapshots** are not bound to a location, but they are bound to an architecture. The default limit is 30.
- **Arm servers** exist only in the EU.

### 2.13 hcloud-go ⏳

- **Latest release:** v2.49.0 (2026-09-22). `go.mod` requires Go 1.25.0.
- **Retries.** hcloud-go retries on 409 conflict, 429, bad_gateway, timeouts, HTTP 502/504 and network timeouts.
  - Backoff: exponential from 1 s, capped at 60 s, with jitter, at most 5 retries (`WithRetryOpts`).
  - It does **not** read `RateLimit-Reset`.
- **Polling.** The default action poll is a **constant 500 ms**. Set `WithPollOpts` with exponential backoff.
- **Mockable interfaces.** `I*Client` interfaces (for example `IServerClient`) make fakes straightforward.
- **Experimental `exp/` packages:** actionutil, deprecationutil, labelutil, **mockutil** (a scripted httptest server),
  zoneutil and kit. They may break in minor releases.
- **Recent additions:**
  - the Storage Box client;
  - user data on rebuild;
  - per-location server type availability;
  - datacenter fields removed (v2.45).

### 2.14 Integrations relevant to Nomad

- **CSI driver:** see [1.2](#12-features-tent-relies-on) (not officially supported for Nomad, but documented and
  tested).
- **Cloud controller manager:** Kubernetes only.
- **Autoscaler plugin.** There is no official Nomad Autoscaler plugin for Hetzner. The community
  `AndrewChubatiuk/nomad-hcloud-autoscaler` looks unmaintained: its last tag was in 2024-03 and its last commit in
  2024-12. The Nomad Autoscaler itself is active (v0.5.0, 2026-05).
- **go-discover:** no Hetzner provider (see [1.2](#12-features-tent-relies-on)).

---

## 3. Vultr

> Vultr blocks automated access to `www.vultr.com` and `status.vultr.com`, so the API spec, the pricing pages and the
> status history were read from Wayback snapshots (August–September 2026). Public endpoints (`/v2/regions`,
> `/v2/regions/{id}/availability`, `/v2/plans`, `/v2/os`) were called live without a key on 2026-09-25.

### 3.1 API basics and access control

- **Base and auth.** Base URL `https://api.vultr.com/v2`, authenticated with `Authorization: Bearer <API key>`.
- **Rate limit.**
  - Counted per source IP: more than 30 requests/s may return `429` with a `Retry-After` header (in seconds).
  - Live responses also carry undocumented `X-RateLimit-*` headers. Treat them as informational.
- **Pagination.** Cursor-based: `per_page` (default 100, maximum 500), `cursor`, `meta.links.next/prev`,
  `meta.total`.
- **Keys and users.**
  - Keys belong to users. A user may have several named keys, each with an optional expiry.
  - User ACLs: abuse, activity_logs, alerts, billing, dns, firewall, loadbalancer, manage_users, objstore,
    provisioning, subscriptions, subscriptions_view, support, upgrade.
  - `service_user` creates an API-only user.
  - The IP allow-list is per user (all of the user's keys), not per key.
- **IAM (2026).**
  - Policies list actions such as `compute.instance.Create|Delete|List|Read|Update`, `network.firewall.*` and
    `network.vpc.*`, with Allow or Deny.
  - Resources are `*` or `type:id`. No tag conditions.
  - Roles, groups, role trusts with OIDC federation, assumed-role sessions, and an STS-compatible AssumeRole.

### 3.2 govultr ⏳

Facts dated 2026-09-27 were read in the v3.33.0 source.

- **Release.** v3.33.0 (2026-09-01), which removed VPC 2.0. `go.mod` requires Go 1.23.
- **Retries.** govultr wraps go-retryablehttp, with 3 retries and 500 ms waits by default. It retries 429, 5xx other
  than 501 and connection errors for **every method, POST included**, and honours `Retry-After`.
  - `SetRetryLimit(n)` sets the retry count for every call. There is no policy per method (2026-09-27).
  - `SetRateLimit(d)` only sets the minimum and maximum wait between retries. It is not a throttle, and the README's
    `SetRateLimit(500)` means 500 ns.
  - An `OnRequestCompleted` hook gets each response, or `nil` where the error handler below took it (2026-09-27).
- **Errors are untyped.** `Delete`, `Halt`, `Start` and `Reboot` return only an error. In detail (2026-09-27):
  - A 429 or a 5xx other than 501 goes through retryablehttp's error handler, even with retries at 0. The call gets
    a `nil` response and, at retries 0, the error `gave up after 1 attempts, last error: "<body>"`.
  - A transport error becomes text as well, so `errors.Is(err, context.Canceled)` fails.
  - Any other non-2xx answer comes back as `errors.New(body)`, with a `{"error","status"}` payload.
- **Requests and answers (2026-09-27).**
  - govultr never sets `Authorization`. The `http.Client` it gets must set it, as the README's oauth2 example does.
  - It decodes an answer only when `Content-Type` is exactly `application/json`. For any other type it returns an
    empty result without an error.
  - `InstanceUpdateReq` sends `tags` and `ddos_protection` even when they are unset, as `null`. Whether Vultr then
    clears the instance's tags is not verified 🔬: check before an instance PATCH relies on it.
- **Mocking.** The service fields (`InstanceService`, `VPCService`, `FirewallGroupService`, `LoadBalancerService`,
  and so on) are interfaces, so fakes are easy. There are no official mocks.

### 3.3 Instances

- **`POST /v2/instances`** requires `region` and `plan`, plus one of `os_id`, `iso_id`, `snapshot_id`, `app_id` or
  `image_id`.
  - Optional parameters include:
    - `label`, `hostname`, `tags[]`, `user_data` (base64), `sshkey_id[]`;
    - `firewall_group_id` (a single group), `attach_vpc[]` or `enable_vpc`, `vpc_only`;
    - `enable_ipv6`, `disable_public_ipv4` (only together with IPv6), `reserved_ipv4`;
    - `backups` (`enabled`/`disabled`), `ddos_protection`, `script_id`, `user_scheme` (`root`/`limited`),
      `activation_email`, `block_devices`.
  - It returns **202** with `instance.id` and **no job id**.
  - The older fields `attach_private_network`, `enable_private_network` and `tag` are deprecated.
- **Status fields:**
  - `status`: `pending`, `active`, `suspended`, `resizing`;
  - `power_status`: `running`, `stopped`;
  - `server_status`: `none`, `locked`, `installingbooting`, `ok`.

  Treat `active` + `running` + `ok` as ready in the API. **Spike 2026-09-25:** that state arrived 46–73 s after the
  create call, in one case 7 s before the kernel started. It does not mean the OS is up; see
  [3.4](#34-user_data-metadata-and-identity) for the boot timeline.
- **Labels are not unique, and create has no idempotency key.** Vultr's own cloud controller manager handles
  "multiple instances found with name".
- **Tags.**
  - Tags are plain strings and exist on instances (and bare metal) only.
  - The format rules are undocumented. **Spike 2026-09-25:** every tag tried was accepted and stored verbatim:
    `/`, `=`, `:`, `.`, spaces, upper case and non-ASCII (`tent-ü`). 512-character tags were accepted (longer ones
    were not tried). 50 tags per instance were accepted; 100 were rejected.
  - `GET /v2/instances` filters by `tag` (one value), `label`, `hostname`, `region`, `main_ip` and
    `firewall_group_id`. **Spike 2026-09-25:** `?tag=` is an **exact but case-insensitive** match (no prefix or
    suffix matches), and `?label=` is exact (no prefix match).
  - A new instance was listed by `?tag=` on the first request after the create response (5 of 5 instances), so a
    search by operation tag right after a lost response finds it.
- **`PATCH /v2/instances/{id}`** returns 202 with `job_ids`. It can change:
  - `tags` (replaces the whole set);
  - `user_data`;
  - `firewall_group_id`;
  - `plan`;
  - `attach_vpc` / `detach_vpc`;
  - `label` (govultr and vultr-cli send it).

  `hostname` changes only through a reinstall.
- **Private IP and MAC:** `GET /v2/instances/{id}/vpcs` returns `vpcs[]` with `id`, `mac_address` and `ip_address`.
  The instance object also has `internal_ip` and `vpcs[]`.
- **Power actions.** `halt`, `start` and `reboot` return 204.
  - There is **no graceful shutdown endpoint**.
  - The v1 documentation described `halt` as a "hard power off (basically, unplugging the machine)", and the current
    docs call a restart a "hard reboot". **Spike 2026-09-25:** `halt` is a hard power-off.
    - A systemd unit's `ExecStop` marker was not written, although the unit was active.
    - The journal of the halted boot ends mid-activity, with no shutdown messages.
    - `power_status` read `stopped` 5–9 s after the call.
    - `start` returned 204, and the instance was `running/ok` 15 s later.
  - `DELETE` destroys a running instance immediately.
  - **Stopped instances are billed** until they are destroyed.

### 3.4 user_data, metadata and identity

- **user_data.**
  - Base64-encoded. The size limit is not documented anywhere. **Spike 2026-09-25:**
    - The API has no limit up to **4 MiB**. Both `PATCH` and create accepted 4 MiB and stored it intact.
    - A 65,508-byte user_data (tent's 64 KiB budget) works end to end: the metadata service served all of it, and a
      45 KB `write_files` payload (`encoding: b64`) landed on disk with the right sha256.
    - Larger payloads were not tested on the instance.
  - It can be updated through `PATCH`, with no rebuild. **Spike 2026-09-25:**
    - The metadata service served the new value 4 s after the `PATCH`, at both `/latest/user-data` and `/v1.json`.
    - After `halt` and `start`, the instance-id was unchanged and `runcmd` did not run again. cloud-init only cached
      the new value in `/var/lib/cloud/instance/user-data.txt`. Per-boot modules would run from the new value, so a
      scrub stub must contain none.
  - It is readable from inside the instance without authentication, at `/v1.json` (`user-data`) and
    `/latest/user-data`.
  - `GET /v2/instances/{id}/user-data` returns `{"user_data": {"data": "<base64>"}}`.
- **cloud-init.**
  - Official Linux images run cloud-init with the Vultr datasource.
  - Vultr's docs: "cloud-init updates all the system packages before it enables SSH. This takes several minutes.
    The directive is in Vultr's vendor-data." **This is outdated (spike 2026-09-25):** the vendor data now sets
    `package_upgrade: false` itself, and the `package-update-upgrade-install` module took 2 ms.
  - **How SSH is enabled.** The image's sshd listens on `127.0.0.1` only. A vendor-data script removes that
    `ListenAddress` and reloads sshd. Port 22 became public 31 s after kernel start.
  - **Boot timeline (spike 2026-09-25, `vc2-1c-1gb`, `ams`, Ubuntu 24.04), counted from the create call:**

    | Step | Time |
    |---|---|
    | API `active/running/ok` | 46–73 s |
    | kernel start | 63–80 s |
    | sshd public | kernel + about 31 s |
    | cloud-init finished (on the instance) | kernel + 32–37 s |
    | SSH login from outside | 127–133 s |
    | `cloud-init status: done` seen from outside | 129–149 s |

    - The timings were the same with no user data and with a 64 KiB tent-like cloud-config.
    - In `cloud-init analyze blame` no module took more than 5 s; the largest was the Vultr datasource search.
    - The first run's figures (SSH after 365 s; over 900 s with the default vendor data) did not reproduce. The
      on-instance timestamps show no step that slow.
  - **Vendor data** (six MIME parts, read on the instance on 2026-09-25). User data does not replace them: they ran on
    instances with and without user data.
    - A cloud-config: a root password, `ssh_pwauth: true`, `disable_root: false`, a `linuxuser` account,
      `package_upgrade: false`, and a reboot if `/tmp/.vultr_reboot_required` exists.
    - A swap file of 100 MiB per GB of root disk (at most 8 GiB) on disks of 10 GB or more: 2.3 GiB on 25 GB.
    - `/usr/lib/sysctl.d/90-vultr.conf`, made immutable with `chattr +i`: `fq`, `bbr`, larger TCP buffers,
      `net.ipv4.conf.all.arp_filter=1` and `accept_ra=2` per interface. It also sets NIC multi-queue. Other sysctls
      must go into a later file, such as `/etc/sysctl.d/99-tent.conf`.
    - The sshd `ListenAddress 127.0.0.1` removal described above.
    - A one-shot reboot hook.
    - It also shows `/var/log/cloud-init.log` in `less` on tty4.
- **Metadata service.** `http://169.254.169.254/v1/` and `/v1.json`, with no token and no hop limit.
  - Fields: `hostname`, `instanceid` (legacy), `instance-v2-id` (the API UUID), `public-keys`, `region`
    (`countrycode`, and `regioncode` in upper case, for example `AMS`), `bgp`, `nvidia-driver` and `interfaces[]`.
  - Each interface has `network-type` (`public` or `private`), `mac`, IPv4 address, netmask and gateway, and
    `network-v2-id` (the VPC id).
  - **Spike 2026-09-25:**
    - The private interface also carries `mtu` (`"1450"`) and has no gateway.
    - The public interface lists a route for `169.254.169.254/32` through the public gateway, so metadata traffic
      leaves through the public NIC.
    - `/latest/user-data` answered HTTP 200 from inside the instance.
  - `/v1.json` also carries `user-data`, `vendor-data` and `startup-script`.
  - **`tags` is present but was empty** even though the instance had tags (both spike runs on 2026-09-25), so do not
    rely on it.
    Label and plan are not exposed.
- **Networking at boot.** The first interface uses DHCP and SLAAC. Private interfaces get a **static** address,
  netmask, MTU and routes from metadata. So a VPC attached at creation is configured automatically, and a later
  attachment probably is not.
  - **Spike 2026-09-25:** cloud-init wrote netplan with `dhcp4: true` on the public NIC. The private NIC got
    `addresses: [10.64.0.3/16]` and `mtu: 1450`. Both were matched by MAC with `set-name`: `enp1s0` public, `enp8s0`
    private on `vc2-1c-1gb` in `ams`.
- **No signed instance identity** document or attestation.

### 3.5 VPC

- **Only one VPC product remains.**
  - "VPC 2.0" was deprecated on 2025-03-10 and has returned 404 since at least 2026-07-29.
  - govultr removed it on 2026-09-01.
  - `/private-networks` is a deprecated alias.
- **Scope.** A VPC is region-scoped, and a region allows at most 5. An instance may join several; the per-instance
  maximum is undocumented.
- **CIDR.** Set with `v4_subnet` and `v4_subnet_mask` (RFC1918), or assigned automatically.
  - **Spike 2026-09-25:** `/16`, `/20` and `/24` were all accepted for `10.64.0.0`.
  - Addresses are assigned in order: the first two instances got `.3` and `.4`.
  - The instance's VPC address was visible through `GET /v2/instances/{id}/vpcs` 6–7 s after the create call.
- **Attaching.** At creation (`attach_vpc`), or later with `POST /v2/instances/{id}/vpcs/attach {vpc_id}` or `PATCH`.
  **Attaching later reboots the VM.**
- **No specific private IP can be requested.** The attach call takes only `vpc_id`; VPC 2.0's `ip_address` is gone.
- **Alias IPs work.** Legacy docs say "Vultr does not enforce specific IP address ranges on private networks".
  **Spike 2026-09-25:** an address added in the OS (`ip addr add 10.64.0.250/16` on the VPC interface) answered ping
  and HTTP from another instance, so the VPC does not bind addresses to instances. Nothing reserves such an address,
  though, and Vultr may later assign it to a new instance.
- **Traffic.** Ping between instances over the VPC works with the image's default host firewall (spike 2026-09-25).
- **Deleting.** After the instances disappeared from `GET /v2/instances`, `DELETE /v2/vpcs/{id}` still failed for
  14–20 s with `400 The following servers are attached to this VPC network: <IPs>` (spike 2026-09-25). Retry it.
- **MTU and interfaces.** MTU is 1450. Private interface names vary (`enp6s0`, `enp7s0`, `enp8s0`, `ens7`), so match
  by MAC.
- **No VPC peering API.** Cross-region connectivity is do-it-yourself, for example with WireGuard.

### 3.6 Firewall groups

- **Rules:**
  - `ip_type`: `v4` or `v6`.
  - `protocol`: ICMP, TCP, UDP, GRE, ESP or AH.
  - `subnet` + `subnet_size`.
  - `port`: a single port or a range `a:b`.
  - `source`: empty, `cloudflare`, or a load balancer id.
  - `notes`.
- **Accept-only.** Inbound traffic that matches no rule is dropped.
- **Limits.** Each group reports its `max_rule_count` (50 in examples). The number of groups per account is
  undocumented.
- **One group per instance** (`firewall_group_id`), set at creation or with `PATCH`. `PATCH` with
  `firewall_group_id: ""` detaches the group (202).
- **Rule syntax that worked (spike 2026-09-25):** `{ip_type: "v4", protocol: "tcp", subnet: "0.0.0.0",
  subnet_size: 0, port: "22"}`, the same with `v6` and `::`, and `icmp` without a port.
- **How Vultr lists rules** is not verified 🔬: whether a single port comes back as `22` or `22:22`, the case of
  `protocol` and `ip_type`, and the form of `subnet` (such as `::` or `0:0:0:0:0:0:0:0`). tent reads all of these
  forms as one rule.
- **Deleting a group that instances use** is not verified 🔬. tent retries an answer that says the group is in use
  (409, 423, or a 4xx whose message says "in use" or "are attached"), as it retries rate limits and 5xx; any other
  answer fails the delete.
- **Scope:** "the main network interface", inbound. **Spike 2026-09-25:** a group that allowed only SSH and ICMP
  blocked public :4646 12 s after the `PATCH`, while :4646 over the VPC stayed reachable. Groups do not filter VPC
  traffic.
- **Host firewall (Ubuntu 24.04 image, spike 2026-09-25).**
  - ufw is active: default deny incoming, allow outgoing, routed disabled. The only rules allow 22/tcp for IPv4 and
    IPv6, without `limit`.
  - firewalld, fail2ban and sshguard are not installed. The rules live in iptables-nft (`table ip filter`,
    `table ip6 filter`).
  - With these defaults :4646 is blocked from the internet **and from the VPC**. ICMP passes. With ufw disabled,
    both answered.
  - Other images (for example with firewalld) were not checked.
- **sshd defaults (same image).** `PermitRootLogin yes`, `PasswordAuthentication yes`, `MaxAuthTries 6`,
  `MaxStartups 10:30:100`. Password guessing from the internet reached a test instance within 12 minutes of boot.

### 3.7 Load balancers

- **Always public** (IPv4 + IPv6), with no internal-only option. The `vpc` setting makes backends reachable over the
  VPC.
- **Targets** are instance IDs only; there is no tag or label selection.
- **Forwarding.** At most 15 rules (TCP, UDP, HTTP, HTTPS), TCP passthrough with optional PROXY protocol, and TCP,
  HTTP or HTTPS health checks.
- **Firewall.** `firewall_rules` take a port, a source CIDR or `cloudflare`, and an `ip_type`.
- **Availability and price.** Not available in `mxp` or `sao`. $10/month ($0.015/hour).

### 3.8 Regions, images, plans and billing ⏳

- **Regions.** The API returns 33:
  - Americas: atl, dfw, ewr, hnl, lax, mex, mia, ord, sao, scl, sea, sjc, yto.
  - Europe: ams, cdg, fra, lhr, mad, man, mxp, sto, waw.
  - Asia-Pacific: blr, bom, del, icn, itm, mel, nrt, sgp, syd.
  - Africa and the Middle East: jnb, tlv.

  There are **no availability zones**.
- **Availability.** `GET /v2/regions/{id}/availability[?type=vc2]` returns `available_plans[]` and
  `available_vpc_only_plans[]`: the plans deployable right now, without quantities. The `locations` list in
  `/v2/plans` does **not** mean "in stock". Checked live on 2026-09-27:
  - An unknown region answers `400 {"error":"Invalid region.","status":400}`.
  - `type` takes `all`, `vbm`, `vdc`, `vhp`, `vhf`, `vc2`, `voc`, `vcg`, `vdg`, `vdm`, `vx1`, `voc-g`, `voc-s`,
    `voc-c` or `voc-m`. Any other value answers 400 `Please provide a valid type: …` with that list.
- **Images.** Referenced by numeric `os_id` from `GET /v2/os`: Ubuntu 24.04 LTS = **2284**, Ubuntu 26.04 LTS = 2760,
  Debian 12 = 2136, Debian 13 = 2625 (checked live).
- **No arm64** Cloud Compute plans.
- **Smallest plans** (checked live for `ams` on 2026-09-25):

  | Plan | vCPU / RAM / disk | $/month | Real $/hour (÷ 672) |
  |---|---|---|---|
  | `vc2-1c-1gb` | 1 / 1 GB / 25 GB | 5 | 0.0074 |
  | `vc2-1c-2gb` | 1 / 2 GB / 55 GB | 10 | 0.0149 |
  | `vc2-2c-4gb` | 2 / 4 GB / 80 GB | 20 | 0.030 |
  | `vhf-1c-2gb` (NVMe) | 1 / 2 GB / 64 GB | 12 | 0.018 |
  | `vhp-1c-2gb-amd` (NVMe) | 1 / 2 GB / 50 GB | 12 | 0.018 |

  The 512 MB plans ($2.50 IPv6-only, $3.50) are too small for Nomad.
- **Billing.**
  - Hourly, with a **minimum of 1 hour per instance**, capped at 672 hours a month. Stopped instances are billed.
  - The API's `hourly_cost` divides by 730, about 8% below the real rate, so compute `monthly_cost / 672`.
  - `location_cost` applies multipliers (São Paulo 1.5×).
  - A public IPv4 is included.

### 3.9 Networking extras

- **Reserved IPs** are public only, region-bound and cost $3/month.
- **Managed NAT gateway**, generally available since 2026-02-05.
  - One per VPC, 8 Gbps, $0.03/hour.
  - Egress, TCP/UDP port forwarding and its own firewall.
  - `vpc_only: true` instances get no public interface and egress through it.
- **IPv6-only.** `disable_public_ipv4` together with `enable_ipv6`.

### 3.10 Ownership fields on other resources

- **Tags** exist only on instances and bare metal. The NAT gateway has a single `tag`.
- **Everything else has one free-text field:**
  - VPC and firewall group: `description`;
  - load balancer, reserved IP, block storage: `label`;
  - SSH key: `name`;
  - snapshot: `description`.
- **None of these is unique.** Only `GET /v2/instances` filters by tag.
- **Spike 2026-09-25:** the VPC `description`, the SSH key `name` and the firewall group `description` stored
  `tent:cluster=…;kind=…` markers, including `:`, `=` and `;`, verbatim.
- **The longest text stored verbatim** is not verified 🔬. The spike's markers had at most 57 characters. With `op`,
  tent's markers for a 20-character cluster name reach 99 characters on a firewall group, 98 on an SSH key and 82 on
  a VPC.
- **A second SSH key with the same key material** is not verified 🔬. Hetzner refuses one
  ([2.4](#24-servers)). If Vultr refuses too, a second cluster with the same operator key fails, and so does a
  cluster whose key an operator uploaded by hand.

### 3.11 Block storage and CSI

- **Block storage.**
  - Types: `high_perf` (NVMe, 10–10,000 GB) and `storage_opt` (HDD, 40–40,000 GB).
  - Up to 16 volumes per instance, one instance per volume, same region.
  - `live: true` attaches without a restart.
  - $1 per 10 GB per month.
- **vultr-csi** v0.19.0 (2026-09-08) ships **official Nomad docs and example jobs** (`docs/nomad`):
  - the controller runs as a service job and the node plugin as a system job;
  - it needs `allow_privileged`;
  - the API key goes to the controller only;
  - one deployment per region.

### 3.12 Object Storage ⏳

- **Endpoints** are `<id>.vultrobjects.com`: ewr1, ewr2, chi3, atl1, atl2, lax1, sjc1, sea1, ams1, ams2, lhr1, mxp1,
  sgp1, sgp2, blr1, blr2, del1, nrt1, syd1. **There is no Frankfurt endpoint.**
- **Pricing.** Relaunched on 2025-03-03.
  - Standard $18/month, Premium $36, Performance $50, Accelerated $100. Each includes 1 TB of storage and 1 TB of
    egress; extra egress is $0.01/GB.
  - The $6 Archive tier archives objects and needs them restored before reading, so it **cannot hold state**.
  - Limits: 100 buckets and 400 ops/s per subscription.
- **Features.** Versioning, lifecycle rules, policies, ACLs, CORS, tagging and presigned URLs. Object lock is set at
  bucket creation through the Vultr API.
- **Keys through the API.** `POST /v2/object-storage` with `cluster_id` and `tier_id` returns `s3_access_key` and
  `s3_secret_key`. Keys cover the whole subscription.
- **Conditional writes** are not documented by Vultr. The endpoints run Ceph Object Gateway "tentacle", whose source
  honours `If-Match` and `If-None-Match` on PutObject. Likely supported, but untested 🔬. The spike's check needs an
  existing bucket and was not run on 2026-09-25. Ceph RGW 20.2.4 on RADOS honoured both in a local test on 2026-09-26
  ([5.1](#51-conditional-writes-)); Vultr's endpoints themselves are still untested.

### 3.13 DNS

- Full API coverage (`/v2/domains`, records, SOA, DNSSEC), free of charge.
- Documented propagation takes 6–12 hours.

### 3.14 Account limits, terms and operations ⏳

- **Account limits.**
  - Set per account and not published: maximum instances, maximum monthly instance cost, and possibly instances
    created per 24 hours (anecdotal).
  - Not exposed by the API. Raise them in the Console under Billing → Limits.
  - Anecdote: a fresh account showed "maximum instances: 1, maximum billing fee: $1".
- **Verification.** A card or PayPal account is required.
- **TOS and AUP.** Nothing about minimum instance lifetimes. Circumventing limits (for example, with extra accounts) is
  forbidden. Crypto mining is banned.
- **Provisioning time** (VPSBenchmarks, order to SSH, small samples):

  | Provider | Average | Notes |
  |---|---|---|
  | Vultr | 80 s | median 60 s, range 30–260 s, 12 samples |
  | Hetzner | 32 s | 2 samples |

  - Deploying from a snapshot takes 10–15 minutes longer.
  - Creating a snapshot takes up to 30 minutes and costs $0.05/GB-month.
- **Incidents.**
  - Deploy delays: 2025-07-14, 2025-10-08, 2025-11-11, 2025-11-23, 2026-05-09.
  - Deploy failures in the API and console: 2026-08-06.
  - API outage: 2026-09-06.
  - Atlanta partial outage, open since 2026-09-18.

### 3.15 Ecosystem

- **go-discover** has no Vultr provider.
- **Nomad Autoscaler** has no Vultr target plugin.
- **Terraform provider** `vultr/vultr` v2.32.0 fails on instance reads after the VPC 2.0 removal. The fix is on master
  but unreleased as of 2026-09-25.
- **Packer plugin:** v2.7.0.
- **Cluster API provider** CAPVULTR is active: v0.5.0, 2026-09-11.
- **"Clusters" API.** A new API for instance pools built from templates. It is not in govultr, and its documentation
  is thin.
- **Vultr's Nomad guide** (updated 2026-04) joins servers through static private IPs in `retry_join`. That breaks
  after servers are replaced; tent uses seed-and-refresh instead.

### 3.16 Spike runs 2026-09-25

All runs: region `ams`, plan `vc2-1c-1gb`, Ubuntu 24.04 (`os_id` 2284). Reports are in `hack/vultr-spike/results/`
(git-ignored).

**Run 1 (`tt3s1g`, spike v1)** verified:
- the tag syntax, limits and filter semantics;
- ownership markers in free-text fields;
- VPC masks, static private addressing and interface naming;
- metadata fields;
- no user_data limit up to 4 MiB.

Its SSH to the instances stopped working after a burst of about seven connections, so every check that needed SSH was
inconclusive. Its boot timings (SSH after 365 s, over 900 s with the default vendor data) did not reproduce later.

**Run 2 (`hh7sgs`, spike v2)** ran every check except `userdata` (settled by run 1) and `objstore` (no bucket).
Spike v2 opens one multiplexed SSH connection per host, paces new connections and collects data on the instance.
It verified:
- the host firewall defaults and sshd settings of the image ([3.6](#36-firewall-groups));
- that firewall groups do not filter VPC traffic;
- that alias IPs in the VPC are reachable ([3.5](#35-vpc));
- ping between instances over the VPC;
- user_data scrubbing, and that cloud-init does not re-run after a restart ([3.4](#34-user_data-metadata-and-identity));
- that a 64 KiB user_data works end to end;
- that `halt` is a hard power-off and `start` works ([3.3](#33-instances));
- the vendor data contents and where boot time goes;
- that a new instance is listable by tag at once;
- that the VPC cannot be deleted for 14–20 s after its instances are gone.

Its outside boot timings were too high: macOS `nc -w` does not bound the connect time, so every probe of a host that
did not answer yet blocked the poll loop for 75 s. The script now uses a bash `/dev/tcp` probe with a 5 s limit.

**Run 3 (`mqlko4`, `--only boot`)** re-measured boot with the fixed probe. Its figures are the ones in
[3.4](#34-user_data-metadata-and-identity).

**Why run 1 lost SSH is still unknown.** The image has no SSH rate limiting (ufw allows 22 without `limit`, no
fail2ban or sshguard, default `MaxStartups`), and run 2, with one connection per host, had no SSH problems. This does
not affect tent: tent never uses SSH in the node lifecycle.

**Still open:**
- Object Storage conditional writes ([3.12](#312-object-storage-)).
- Images other than Ubuntu 24.04 (for example 26.04) were not checked.
- Account limits beyond 3 concurrent instances were not tested.

---

## 4. Prior art

### 4.1 kops

Read at kops `master` @ ab568ea (2026-09-25). Paths are relative to `github.com/kubernetes/kops`.

**Hetzner support:**
- **Where the code lives.**
  - Tasks: `upup/pkg/fi/cloudup/hetznertasks/{servergroup,network,firewall,loadbalancer,sshkey,volume}.go`.
  - Model builders: `pkg/model/hetznermodel/`.
  - Cloud code: `upup/pkg/fi/cloudup/hetzner/cloud.go`.
- **Labels.** Ownership is by labels only: `kops.k8s.io/{cluster,instance-group,instance-role,…}`. Firewalls and the
  API load balancer select servers by label.
- **ServerGroup task.**
  - It lists servers by label. A server needs an update if its user data hash, location, type, image or IP settings
    differ.
  - It labels such servers `needs-update` rather than replacing them.
  - It creates missing servers named `<ig>-<random hex>`. The count is a floor, and surplus servers are left alone.
- **Rolling update.** kops drains the node, then shuts it down and deletes it (polling up to 150 s). Then it re-runs
  **the whole `ApplyClusterCmd`** to create the replacement. `MaxSurge` defaults to 0 outside AWS.
- **Pitfalls:**
  - random names give non-idempotent creation, and concurrent runs over-provision;
  - the whole graph is re-applied per replaced node, under a limit of 3600 requests per hour;
  - a crash between delete and reconcile leaves the group short;
  - the Hetzner docs are outdated.

**Bootstrap credentials on Hetzner:**
- **Control-plane nodes read the state store directly.** `S3_ENDPOINT`, `S3_REGION`, `S3_ACCESS_KEY_ID` and
  `S3_SECRET_ACCESS_KEY` sit **in plaintext in user data**.
- **Worker nodes** get only kops-controller URLs and the CA bundle.
- **`HCLOUD_TOKEN`**, the project-wide Read & Write token, is copied into the kops-controller environment, the
  etcd-manager manifests and a Kubernetes Secret, all stored in plaintext in the state store.
- **How kops-controller verifies a node:**
  1. The node claims `x-hetzner-id <serverID>`.
  2. The controller looks the server up through the hcloud API.
  3. It connects to the API-reported private IP on port 3987 with a pinned certificate, and the node must answer a
     one-time challenge, `sha256(secret‖nonce)`.
  4. The request is rejected if a Ready Node with that name already exists.
  5. The controller issues certificates and the node configuration.

**The `fi` task framework:**
- **Lifecycle:** `Find` → reflection-based `BuildChanges` ("nil means don't care") → `CheckChanges` → `Render<Cloud>`,
  with the method selected by reflection.
- **Dependencies** are discovered by walking struct fields, with deferred placeholder links.
- **Executor:** parallel waves with a 10-minute retry deadline per task.
- **Pain points:**
  - contracts are checked only at runtime;
  - the dependency graph is implicit;
  - removing a field cannot be expressed;
  - imperfect normalisation causes perpetual diffs and needless rolls;
  - deletion lives in separate code (`pkg/resources`) and drifts;
  - every task needs its own Terraform renderer;
  - ServerGroup fakes its "actual" object.

**Delete:** list by cluster label with retries, blocking edges between resources, and parallel passes retried every
10 seconds for up to 10 minutes. Load balancers created by the cloud controller manager and volumes created by CSI
carry no cluster label and leak.

**State store:**
- **Layout:** `config`, `cluster-completed.spec`, `kops-version.txt` (older kops refuses), `instancegroup/`,
  `igconfig/`, `addons/`, `pki/`, `secrets/`, `backups/etcd/`.
- **No locking.** Issue #5086 went stale.

**Channels:**
- Channels are fetched at runtime from GitHub raw. They list images per provider, Kubernetes versions and the
  enforced kops versions.
- `kops upgrade cluster` rewrites the spec only.

**Assets:** nodeup is downloaded from mirrors with sha256 values in user data. A change of `KOPS_BASE_URL` changes the
user data and rolls every node.

### 4.2 Other tools

| Project | Status (2026-09) | Notes |
|---|---|---|
| `vitobotta/hetzner-k3s` | active (v2.6.0, 2026-06) | Crystal CLI; direct Hetzner API; SSH install; deterministic names (re-running `create` repairs); in-place k3s upgrades; sleeps until `RateLimit-Reset` |
| `syself/cluster-api-provider-hetzner` (CAPH) | active (v1.1.8, 2026-08) | labels on everything; deterministic names + adopt on `uniqueness_error`; `invalid_input`/`resource_unavailable` treated as permanent; pauses an object 5 min on rate limit; CSR approver checks node requests against Hetzner-reported IPs/names |
| `jsiebens/hashi-up` | unmaintained (last commit 2023-06) | SSH installer for Nomad/Consul/Vault, no infrastructure |
| Terraform modules for Nomad on Hetzner | dead or dormant (2022–2024) | e.g. `wenzel-felix/terraform-hetzner-nomad-consul-module`; `hetznercloud/nomad-dev-env` is internal, no compatibility promise |
| Hashistack Ansible collections | configuration only | no cluster lifecycle |
| "kops for Nomad" | **does not exist** | the niche is open |

### 4.3 Lessons adopted by tent

**Copied:**
- ownership by labels;
- deletion by label with explicit dependency edges and retries;
- a user-data/config hash label for drift detection;
- label-selector firewalls and load balancers;
- a version guard file in the state store;
- refusing to delete unknown state files;
- deterministic names with adopt-on-conflict (CAPH, hetzner-k3s);
- reading `RateLimit-Reset` (hetzner-k3s);
- an identity check through the cloud API plus a callback to the private IP (kops-controller, for the v2 bootstrap
  controller).

**Avoided:**
- reflection-driven tasks;
- random names;
- re-planning the whole cluster per replaced node;
- having no lock;
- cloud and state store credentials on nodes;
- environment-dependent node hashes.

---

## 5. S3-compatible object stores

### 5.1 Conditional writes ⏳

The conditional-put lock ([architecture §10.4](architecture.md#104-locking)) needs `PutObject` with
`If-None-Match: *` (create only) and `If-Match: <ETag>` (replace only that version), atomic under concurrent writers.
Few servers document how they behave.

**Documented:**
- **AWS S3** supports both headers. A failed condition gets 412. A concurrent request may get 409
  `ConditionalRequestConflict` instead, and `If-Match` on a missing object gets 404.
- **Cloudflare R2** lists `If-Match` and `If-None-Match` for PutObject (S3 API compatibility page, updated
  2026-07-31), but says nothing about concurrent writers. The race subtests in CI check that against a real R2 bucket.
- **R2 throttling.** R2 takes at most one write per second to a key and answers faster writes with 429
  `TooManyRequests` (error 10058). It does not document whether conditional puts that fail their condition count
  toward that limit.
- **AWS S3 and Ceph RGW** throttle with 503 `SlowDown`.

**Verified on 2026-09-26** by running each server in Docker and sending it single and concurrent conditional puts
through aws-sdk-go-v2:

| Server | Version | Conditional puts |
|---|---|---|
| Garage | v2.4.1 | both headers ignored: a second create-only put returns 200 |
| Scality CloudServer | 9.4.3 | both headers ignored |
| Ceph RGW on the DBStore backend | 20.2.4 | both headers ignored |
| S3Proxy | 4.1.1 | honoured one request at a time, not atomic under concurrent writers |
| Adobe S3Mock | 5.2.3 | honoured one request at a time, not atomic under concurrent writers |
| Ceph RGW on RADOS, the backend Vultr runs | 20.2.4 | honoured; the loser of a race gets 412 or 409 `ConcurrentModification` |
| versitygw | 1.8.0 | honoured, atomic in every race |
| RustFS | 1.0.0 | honoured, atomic in every race |
| SeaweedFS | 4.47 | honoured, atomic in every race |
| moto | 5.2.3 | honoured, atomic in every race |
| pgsty/silo, a MinIO fork | — | honoured, atomic in every race |

- `If-Match` on a missing key returns 404 `NoSuchKey` on most servers and 412 on SeaweedFS.
- Every server accepts the SDK's default CRC32 request checksum.
- Ceph RGW and moto send no response checksum.

**What the s3 backend does with this:**
- It never sends a conditional put again after a 500 or a lost answer
  ([architecture §10.1](architecture.md#101-backends)).
- It sends a throttled request (429, `TooManyRequests` or `SlowDown`) again after a random wait of 1–2 seconds, up to
  30 attempts in all, conditional puts included: the server did not carry out a throttled request. The SDK does not
  retry R2's 429 `TooManyRequests`, but it retries a 503 `SlowDown` on a plain request (3 attempts, backoff up to
  20 s) inside each of these attempts.
- It reads 412, any 409, and a 404 on an `If-Match` put as "precondition failed", and 501 as "no conditional puts".
- It validates response checksums only where the API requires them.
- A probe decides at run time whether the server enforces the headers. A server that ignores them gets the
  provider's lock or the best-effort one instead of a broken one. The probe cannot tell S3Proxy and S3Mock from atomic
  servers.
- It does not probe Hetzner Object Storage (`your-objectstorage.com` and its subdomains) and treats it as a store
  without conditional writes until E2E proves them ([ADR-0010](adr/0010-state-store-and-locking.md)). A Hetzner
  cluster then locks with the firewall mutex (M4); any other cluster that keeps its state there gets the best-effort
  lease with a loud warning.

**MinIO.** The community edition's repository has been archived since 2026-04-25, and it ships as source only: there
are no binaries, and its container images can no longer be pulled. So CI runs no local S3 server: the maintainer
decided on 2026-09-26 to test the s3 backend against Cloudflare R2 only (issue #22 first named MinIO). A MinIO server
that someone already runs is still an S3-compatible store for tent. It was not tested here; pgsty/silo, a fork,
passes every check.

---

## 6. Sources

Nomad:
- Releases API: <https://api.releases.hashicorp.com/v1/releases/nomad>
- Changelog: <https://github.com/hashicorp/nomad/blob/main/CHANGELOG.md>
- Configuration: <https://developer.hashicorp.com/nomad/docs/configuration>
- Client configuration: <https://developer.hashicorp.com/nomad/docs/configuration/client>
- `server_join`: <https://developer.hashicorp.com/nomad/docs/configuration/server_join>
- Job specification (default `datacenters`): <https://developer.hashicorp.com/nomad/docs/job-specification/job>
- Connecting nodes and client introduction: <https://developer.hashicorp.com/nomad/docs/deploy/clusters/connect-nodes>
- ACL tokens API: <https://developer.hashicorp.com/nomad/api-docs/acl/tokens>
- Autopilot API: <https://developer.hashicorp.com/nomad/api-docs/operator/autopilot>
- Raft API: <https://developer.hashicorp.com/nomad/api-docs/operator/raft>
- Node pools: <https://developer.hashicorp.com/nomad/docs/architecture/cluster/node-pools>
- Upgrading: <https://developer.hashicorp.com/nomad/docs/upgrade>
- CE licence and support: <https://developer.hashicorp.com/nomad/docs/ce-license-support>
- go-discover: <https://github.com/hashicorp/go-discover>
- Release signing key: <https://www.hashicorp.com/.well-known/pgp-key.txt>

Hetzner:
- Cloud API OpenAPI spec: <https://docs.hetzner.cloud/cloud.spec.json>
- Changelog: <https://docs.hetzner.cloud/changelog>
- Docs: <https://docs.hetzner.com/cloud/> (servers, networks, firewalls, placement groups, load balancers, object
  storage)
- Price adjustment: <https://docs.hetzner.com/general/infrastructure-and-availability/price-adjustment/>
- Capacity incident: <https://status.hetzner.com/incident/0a75c7ae-3377-41dc-aabe-601063724d24>
- hcloud-go: <https://github.com/hetznercloud/hcloud-go>
- CSI driver, Nomad guide: <https://github.com/hetznercloud/csi-driver/blob/main/docs/nomad/README.md>

Vultr:
- API reference (Wayback copy of 2026-09-12): <https://web.archive.org/web/20260912012717/https://www.vultr.com/api/>
- Account limits: <https://docs.vultr.com/platform/billing/manage-account-limits>
- Billing: <https://docs.vultr.com/support/platform/billing/how-am-i-billed-for-my-servers>
- Cloud-init on Vultr: <https://docs.vultr.com/products/compute/instances/cloud-compute/features/cloud-init>
- Firewall groups FAQ: <https://docs.vultr.com/products/network/firewall-groups/faq>
- VPC FAQ: <https://docs.vultr.com/products/network/vpc-networks/faq>
- Object Storage S3 compatibility: <https://docs.vultr.com/products/storage/object-storage/s3-compatibility-matrix>
- Object Storage relaunch: <https://blogs.vultr.com/object-storage-relaunch>
- NAT gateways: <https://blogs.vultr.com/nat-gateways>
- Nomad guide: <https://docs.vultr.com/how-to-deploy-a-hashicorp-nomad-cluster-on-vultr-cloud-compute-instances>
- govultr: <https://github.com/vultr/govultr>
- vultr-csi Nomad docs: <https://github.com/vultr/vultr-csi/tree/master/docs/nomad>
- cloud-init Vultr datasource: <https://github.com/canonical/cloud-init/blob/main/cloudinit/sources/helpers/vultr.py>
- Terraform provider issue (VPC 2.0 removal): <https://github.com/vultr/terraform-provider-vultr/issues/753>
- Provisioning benchmarks: <https://www.vpsbenchmarks.com/hosters/vultr>
- Incident history: <https://github.com/outages/vultr-outages>

Nomad status and agent API:
- <https://developer.hashicorp.com/nomad/api-docs/status>
- <https://developer.hashicorp.com/nomad/api-docs/agent>

S3-compatible object stores:
- AWS S3 conditional writes: <https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html>
- Cloudflare R2 S3 API compatibility: <https://developers.cloudflare.com/r2/api/s3/api/>
- Cloudflare R2 limits (one write per second per key): <https://developers.cloudflare.com/r2/platform/limits/>
- Cloudflare R2 error codes (10058 `TooManyRequests`): <https://developers.cloudflare.com/r2/api/error-codes/>
- MinIO community edition (archived): <https://github.com/minio/minio>

Prior art:
- kops: <https://github.com/kubernetes/kops>
- CAPH: <https://github.com/syself/cluster-api-provider-hetzner>
- hetzner-k3s: <https://github.com/vitobotta/hetzner-k3s>
- hashi-up: <https://github.com/jsiebens/hashi-up>
- Nomad Autoscaler external plugins: <https://developer.hashicorp.com/nomad/tools/autoscaling/plugins/external>

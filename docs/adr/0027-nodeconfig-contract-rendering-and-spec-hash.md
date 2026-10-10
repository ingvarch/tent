# ADR-0027: NodeConfig contract, rendering and spec hash

- **Status:** Accepted; amended by [ADR-0028](0028-tent-node-agent-units-and-delivery.md) (NodeConfig gains
  `provider`, validated and out of the spec hash; a kernel module or a sysctl key starts with a letter or a digit;
  of the M2.5 follow-ups, `nomad config validate` moves to M2.6 and gz+b64 waits for the M2.5 VM check, which
  verified it on 2026-09-29) and by [ADR-0029](0029-host-firewall-runtime-and-cni-on-nodes.md) (the asset names are
  constants of `nodeconfig`; rule and asset names are DNS labels; client and combined nodes get rules from Nomad's
  and Docker's bridges; of the M2.6 follow-ups, the metadata block matches tent-node's socket mark, and the Nomad
  items move to M2.6b) and by [ADR-0030](0030-nomad-on-nodes.md) (NodeConfig gains `region`, validated and out of the
  spec hash; `nomad.service` is a group-level NodeConfig file of every role, in the hash; `00-tent.hcl` turns off the
  update check and Consul auto-join and has no `drain_on_shutdown`, so the unit keeps systemd's stop timeout; the M2.6
  follow-ups on `/var/lib/nomad/client`, the unit's signal and `nomad config validate` of the goldens are done; the
  user data grows by about 0.5 KiB and still leaves 12.0 KiB for `extraConfig`
  ([architecture §8.3](../architecture.md#83-nodeconfig-contract))) and by [ADR-0031](0031-bootstrap-in-update.md)
  (decision 12 is built: `update` gives nodes NodeConfig through one node builder, with the spec hash as the
  `tent/spec-hash` label, and writes the completed spec before the first node; the M2.7 follow-ups are done; the
  hash of server and combined groups moved with `leave_on_terminate = false`, and the format stays 1) and by
  [ADR-0032](0032-joined-label-scrub-and-delete-guard.md) (the scrub of user data is built: it runs once a node has
  joined its cluster) and by [ADR-0038](0038-rolling-update-of-server-groups.md) (`RenderNode` takes a
  `bootstrap_expect` of 0 on a server or combined node and then writes no `server` block; `update` and the roll give
  that to a server that has a seed and joins a cluster of one server, since with 1 Nomad starts a cluster of its own;
  item 19: `00-tent.hcl` gains `heartbeat_grace = "20s"` on server and combined nodes and `rpc { keep_alive_interval =
  "5s" }` on client and combined nodes, and the hash of every group moved)
- **Date:** 2026-09-29
- **Deciders:** ingvarch
- **Related:** amends [ADR-0006](0006-two-binaries-and-nodeconfig.md) (the size budget, the instance id),
  [ADR-0016](0016-server-discovery-seed-and-refresh.md) (who renders `05-join.hcl`),
  [ADR-0018](0018-vultr-provider-design.md) (item 5) and [ADR-0025](0025-stdlib-only-helper-packages.md)
  (`internal/secret`, the allow list of `internal/pki`); extends [ADR-0021](0021-import-rules.md); moves the M2.3
  follow-ups of [ADR-0026](0026-channels-and-release-assets.md) to M2.7; [ADR-0007](0007-security-baseline.md),
  [ADR-0008](0008-node-credential-delivery.md), [ADR-0019](0019-combined-server-client-role.md),
  [architecture §7.2](../architecture.md#72-intents-the-providers-input),
  [§8](../architecture.md#8-nodes-tent-node-and-nodeconfig), [§18](../architecture.md#18-open-questions),
  [Appendix A](../architecture.md#appendix-a-nomad-agent-configuration-sketches),
  [Appendix B](../architecture.md#appendix-b-cloud-init-user-data-sketch),
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)

## Context

M2.3 builds the contract between tent and tent-node ([ADR-0006](0006-two-binaries-and-nodeconfig.md)): the NodeConfig
that tent puts into a node's user data, the Nomad agent configuration in it, the spec hash and the user data itself.
Nothing reads it on a node yet: tent-node comes in M2.5 and M2.6.

Building it showed where the accepted design did not fit:

- **The instance id.** Architecture §8.2 and Appendix A put the cloud instance id into NodeConfig and `10-node.hcl`.
  On Vultr the id exists only after the create call, and the user data is part of that call.
- **The size.** ADR-0006 limits the encoded NodeConfig to 24 KiB. Architecture §8.3 and
  [ADR-0018](0018-vultr-provider-design.md) item 5 gave Vultr a budget of 64 KiB. Hetzner allows 32 KiB of user data,
  and AWS 16 KB.
- **Who writes `05-join.hcl`.** [ADR-0016](0016-server-discovery-seed-and-refresh.md) has tent render the seed into
  the file and tent-node rewrite it on refresh. Two renderers of one file must agree on its form.
- **Secrets on machines.** Real user data holds node keys, the gossip key and intro tokens. The Vultr scrub runs only
  after a node has joined its cluster ([ADR-0032](0032-joined-label-scrub-and-delete-guard.md)), and joining needs the
  bootstrap of M2.7.
- **The host firewall.** Copying `access.ssh` and `access.api` to the host would make a change of `access` change the
  configuration of every node.
- **Values in HCL.** Nomad parses its agent configuration with HCL1
  ([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)). Operator values, such as client meta, must
  read back as themselves or be refused before they reach a node.
- **Printing.** NodeConfig carries keys and presigned URLs. The redacting `Secret` type lived in `internal/pki`, which
  `internal/nodeconfig`, and with it tent-node, should not import.

## Decision

### Maintainer decisions (2026-09-28)

12. **NodeConfig reaches `update` in M2.7**, with tent-node, intro tokens and the bootstrap. Until then nodes boot the
    placeholder cloud-config, carry no spec-hash label and get no secrets: real user data holds node keys and the
    gossip key, and the scrub runs only after registration. The promises that
    [ADR-0026](0026-channels-and-release-assets.md) gave to M2.3 move to M2.7 as well: `update` fetches the assets,
    warns when `TENT_NODE_URL` or `TENT_NODE_SHA256` is set on a release build, and writes the Nomad pin before it
    creates the first node, with the secrets. ADR-0026 itself stays as it is.
13. **NodeConfig carries the node's name, not a cloud instance id.** tent-node reads the instance id from the metadata
    service and writes it as the meta `tent_instance_id` into its own `11-instance.hcl`, on client and combined nodes.
    Server nodes have no `client` block. Preflight compares the host name with the name.
14. **One user data budget for every provider: 24 KiB for the whole cloud-config**, which leaves headroom under
    Hetzner's 32 KiB. A per-provider `Capabilities.MaxUserDataBytes` comes when a provider needs less, such as AWS with
    16 KB. An `extraConfig` that does not fit fails with the node group's name and the size.
15. **The host firewall does not copy `access.ssh` or `access.api`.** The public ports, 22/tcp, ICMP, and 4646/tcp on
    server and combined nodes, are open to every source on the host, and the cloud firewall filters the sources. The
    host opens Nomad's ports and the dynamic ports only to the cluster CIDR, and blocks the metadata address for
    workloads. A change of `access` never changes the spec hash.

### Model intents

`model.Cluster` gains two network intents. It still holds no Nomad settings; `internal/app` builds NodeConfig from the
model and the completed specs.

- **`Join`**: `JoinSeedAndRefresh`, the strategy of ADR-0016.
- **`Intra`**: the rules between nodes, all from the cluster CIDR, in this order:

  | Rule | Protocol | Ports | To |
  |---|---|---|---|
  | `nomad-http` | tcp | 4646 | all nodes |
  | `nomad-rpc` | tcp | 4647 | servers |
  | `serf` | tcp and udp | 4648 | servers |
  | `dynamic` | tcp and udp | 20000–32000 | clients |

  Servers are the nodes of the server and combined groups, and clients the nodes of the client and combined groups.
  Clients do not listen on 4647, so `nomad-rpc` goes to the servers only. A combined node's client reaches its own
  server over RPC on the private address, not in process
  ([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)), and this rule lets it. Only clients run
  workloads, so only they open the dynamic ports.
- **Targets.** A rule opens all nodes, the servers or the clients. `Target.Includes(role)` says which roles a rule
  reaches. The Vultr firewall groups and the host firewall both use it. `model.DynamicPorts()` returns 20000–32000,
  Nomad's default range, which tent also writes into the agent configuration.
- `v1alpha1.Role` gains `RunsServer` (server and combined) and `RunsClient` (client and combined).

### NodeConfig

- **Fields:** `apiVersion` (`tent/v1alpha1`), `kind` (`NodeConfig`), `cluster`, `nodeGroup`, `name`, `role`, `assets`,
  `files`, `join`, `system`, `firewall` and `specHash`.
- **Checks.** `Validate` checks the header, the names and the role; each asset (a version, absolute http or https
  URLs, a sha256 of 64 lower-case hex digits); each file (an absolute, clean path, a mode within 0777, a `user:group`
  owner, UTF-8 content); the join settings, the system settings and the firewall rules; and a stored spec hash. `Encode`
  writes indented JSON, the same bytes for the same config. `Decode` refuses unknown fields and anything after the
  object, then validates.
- **Assets.** `nodeconfig.Asset` is NodeConfig's own type, and the app converts `assets.Asset` to it, so
  `internal/assets` never reaches tent-node. Each asset has a name, a version, its URLs (one for now) and a sha256.
  Every node gets `nomad` and `tent-node`. Only client and combined nodes get `cni-plugins`: servers run no workloads,
  and a new CNI version would otherwise mark every server out of date. The time at which `internal/assets` checks
  Nomad's signature is injectable (`assets.Options.Now`), so `update` can pass its own clock in M2.7.
- **Printing.** A `File` prints only its path and size, and `[secret, N bytes]` for a secret one. An `Asset` prints its
  URLs without their query, which may carry a signature, and without a password. fmt with any verb, slog and
  encoding/json show the same. `Encode` writes the content and the URLs as they are.
- **Join:** `seed-and-refresh`, the servers that exist when the node is created, and a refresh every minute.
- **System.** Server nodes get none.
  - Client and combined nodes get the kernel module `br_netfilter` and the sysctls
    `net.bridge.bridge-nf-call-arptables`, `-ip6tables` and `-iptables` set to 1, the post-install steps of Nomad's
    bridge networking.
  - They get Docker from the distribution's packages, and with it the kernel module `overlay`, unless the group's
    drivers leave `docker` out. A group without drivers keeps Nomad's own, Docker among them.
  - tent installs nothing else for drivers. `raw_exec` stays disabled unless the operator enables it in
    `extraConfig`, and `java` and `qemu` need packages that tent does not install.
- **Host firewall** (decision 15): the public rules `ssh` (22/tcp), `icmp` and, on server and combined nodes, `api`
  (4646/tcp), each from `0.0.0.0/0` and `::/0`; then each rule between nodes that reaches the role; and
  `blockMetadata`, `169.254.169.254` on Vultr and Hetzner. So a server opens `nomad-http`, `nomad-rpc` and `serf` to
  the cluster CIDR, and a client `nomad-http` and the dynamic ports. The public ports come from the model's constants
  (`model.SSHPort`, `model.APIPort`), not from `access`, so an empty `access.ssh` closes SSH on the cloud firewall
  only. The `icmp` rule's IPv6 prefix means ICMPv6; tent does not turn on IPv6 on Vultr instances yet.

### Files

Every file is owned by `root:root` (`nodeconfig.Owner`): the Nomad agent runs as root.

| Path | Mode | Scope | Secret | Roles | Made by |
|---|---|---|---|---|---|
| `/etc/nomad.d/00-tent.hcl` | 0644 | group | no | all | tent |
| `/etc/nomad.d/01-gossip.hcl` | 0600 | group | yes | server, combined | tent |
| `/etc/nomad.d/05-join.hcl` | 0644 | node | no | all | tent-node |
| `/etc/nomad.d/10-node.hcl` | 0644 | node | no | all | tent |
| `/etc/nomad.d/11-instance.hcl` | 0644 | node | no | client, combined | tent-node |
| `/etc/nomad.d/98-user-server.hcl` | 0600 | group | no | server, combined, when `extraConfig.server` is set | tent |
| `/etc/nomad.d/99-user-client.hcl` | 0600 | group | no | client, combined, when `extraConfig.client` is set | tent |
| `/etc/nomad.d/tls/ca.pem` | 0644 | group | no | all | tent |
| `/etc/nomad.d/tls/agent.pem` | 0644 | node | no | all | tent |
| `/etc/nomad.d/tls/agent-key.pem` | 0600 | node | yes | all | tent |
| `/var/lib/nomad/client/intro_token.jwt` | 0600 | node | yes | client, combined, when a token is given | tent |

- **Who renders what.** tent renders the group files once per group (`nodeconfig.RenderAgent`) and `10-node.hcl` per
  node (`nodeconfig.RenderNode`), and adds the CA bundle, the node's certificate and key and the intro token.
  tent-node renders `05-join.hcl` (`nodeconfig.RenderJoin`) from `join.servers` and on each refresh, and
  `11-instance.hcl` (`nodeconfig.RenderInstance`). These two are not among NodeConfig's files, and both binaries use
  the same code for them.
- **`00-tent.hcl`** holds tent's settings: a `server` block on server and combined nodes, a `client` block on client
  and combined nodes, `acl`, `tls` and `telemetry` on all, and the heartbeat settings of
  [ADR-0038](0038-rolling-update-of-server-groups.md), item 19. Appendix A points to the golden files.
- **`01-gossip.hcl`** holds only `server { encrypt = "…" }`, so the key stays out of the hash and of any diff, and
  only root reads it.
- **`05-join.hcl`.** A server joins the servers' Serf port in `server { server_join { retry_join } }`, a client their
  RPC port in `client { server_join { retry_join } }`. A combined node takes the server form: Nomad gives its client
  the RPC address of the server in the same agent. tent never writes `server.retry_join`, which Nomad 2.1 removes.
- **`10-node.hcl`** holds the name, the datacenter (the node's zone) and, on server and combined nodes,
  `server { bootstrap_expect }`. `bootstrap_expect` is per node, so a resize of the server group does not mark every
  server out of date. A server of a cluster of one server that has a seed gets none
  ([ADR-0038](0038-rolling-update-of-server-groups.md), item 10).
- **ACLs on every role.** `acl { enabled = true }` is in `00-tent.hcl` of clients too
  ([ADR-0007](0007-security-baseline.md)). A client with ACLs off grants every request to its own endpoints.
- **The operator's files.** Nomad merges the files of `/etc/nomad.d` in the order of their names, so the operator's
  files come last, the server part before the client part, and the two parts never share a file. They can override
  anything, tent's settings included, such as `data_dir`, `client.state_dir`, the `tls` file paths and the dynamic
  ports. The host firewall does not follow them. After such an override Nomad looks for the TLS files or the intro
  token where tent did not write them, and under strict client introduction the client is refused. tent does not
  check `extraConfig`, which stays unsupported.

### Quoting

Every value tent writes into HCL, the go-sockaddr templates included, is an HCL1 string: in double quotes, with `"`
and `\` escaped by a backslash, and everything else, Unicode included, as it is. A value that HCL1 cannot read back
as itself is refused:

- text that is not valid UTF-8;
- control characters, such as a line end or a tab, and U+E123, which the HCL1 scanner refuses anywhere;
- `${`, which HCL1 reads as the start of an interpolation and has no escape for.

`{{`, `%{` and a `$` alone stay as they are. Beyond quoting, a driver must not be empty or hold a comma or white space
(Nomad splits `driver.allowlist` at commas); a meta key must not be empty or start with `tent_`; the datacenter must
not hold `*`; a node's name must be a host name. `extraConfig` is written as given and must only be valid UTF-8. An
error names the setting and never shows the value, which may be a secret.

`api/v1alpha1` refuses such values earlier, so the operator sees a field error:

- a meta key must match `^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`, since Nomad refuses empty parts such as `a..b`, and must
  not start with `tent_`, which tent keeps for `tent_cluster`, `tent_nodegroup` and `tent_instance_id`;
- a meta value must not hold a control character, U+E123 or `${`;
- `nodePool` must not be `all`, Nomad's built-in pool of every node, which no client joins.

### Spec hash

`nodeconfig.SpecHash` is the first 16 lower-case hex digits of the sha256 of a canonical JSON:

```
{"format": 1,
 "files":    group-level files that are not secret, sorted by path: path, mode, owner, content,
 "assets":   sorted by name: name, version, sha256,
 "system":   the system settings,
 "firewall": the host firewall, its rules sorted by name, protocol, ports and sources, and each rule's
             sources sorted}
```

- **In the hash:** `00-tent.hcl`, `98-user-server.hcl`, `99-user-client.hcl`, the CA bundle, each asset's version and
  sha256 (tent-node's included, so a new tent rolls nodes), the system settings and the host firewall. The CA bundle
  is in it, so a future CA rotation marks every node out of date.
- **Out of the hash:** the node's name, the per-node files (`10-node.hcl`, the certificate, the key, the intro token),
  the secret files (`01-gossip.hcl`), the join settings, and where the assets come from (mirror URLs and their order).
  So neither a new mirror nor a new server marks a node out of date. The hash does not depend on the order of rules,
  files or assets.
- **Form.** The canonical form is built field by field, since the JSON forms of `File` and `Asset` hide the content and
  the URL queries. `format` changes with the form, and the test `TestSpecHashCanary` pins the hash of a fixed config
  (`50cf9d039b765cbd`). The label codec of Vultr takes the hash as `tent/spec-hash`.
- Every node of a group has the same hash. `Validate` checks that a stored hash is the hash of the config.

### User data

`nodeconfig.UserData` returns the cloud-config of
[architecture Appendix B](../architecture.md#appendix-b-cloud-init-user-data-sketch):

- `package_update: false` and `package_upgrade: false`;
- one `write_files` entry: `/etc/tent/node.json`, `encoding: gz+b64`, `root:root`, mode `0600`, the content being
  `Encode(nc)` compressed with gzip, without a name or a time, and encoded with standard base64;
- a `runcmd` shell script that makes `/usr/local/bin` (mode 0755 where the image lacks it), then tries each URL of the
  `tent-node` asset in turn: it downloads the file, checks it with `sha256sum -c`, and on a match makes it executable,
  moves it to `/usr/local/bin/tent-node` and runs `tent-node install --config /etc/tent/node.json` through `exec`.
  curl retries every failure of one URL, a refused connection included, as when the network is not up yet at boot,
  and starts no new try after 10 minutes. A try fails when it stalls below 1 KiB/s for 30 s or runs for 10 minutes,
  so one URL takes at most about 20 minutes before the next gets its turn. When no URL gives the right sha256, the
  script fails.

The URLs are printable ASCII in single quotes, one inert word in the YAML block and in sh. The same config always
gives the same bytes. Above 24 KiB, `UserData` fails: `user data: node group <group> needs <n> bytes, more than the
24576 that fit`.

The size tests, measured on 2026-09-29, build the largest config of each role without `extraConfig`: two CAs in the
bundle, a large certificate, a 2 KiB intro token, tent-node URLs that include a 1.5 KiB presigned one, a mirror
per asset, 5 seeds, 12 meta keys and every firewall rule, all as incompressible as base64. Their user data takes
8.9 KiB (server) to 11.5 KiB (combined), and each must leave at least 8 KiB for `extraConfig`. A combined node built
from real data, with a real CA and node certificate, the embedded `stable` channel, a 1.5 KiB presigned URL, a 2 KiB
intro token, 5 seeds, 8 meta keys and 621 bytes of `extraConfig`, takes about 10.3 KiB, and its payload decodes back
to `node.json`.

### Import rules

- **`internal/secret`** holds the redacting `Secret` type; `pki.Secret` is an alias of it. It is one more
  standard-library-only helper in the sense of [ADR-0025](0025-stdlib-only-helper-packages.md): `secret-stdlib-only`
  allows it only the standard library. `pki-stdlib-only` now allows `internal/secret` too.
- **`nodeconfig-stdlib-only`**: `internal/nodeconfig` imports only the standard library, `internal/secret` and
  `api/v1alpha1`. Its tests are exempt.
- **`nodeup-no-cloud`** covers `internal/nodeconfig` too, tests included. `node-no-assets` already did (ADR-0026).
- **`hcl-only-in-tests`**: only tests import `github.com/hashicorp/hcl` (v1, MPL-2.0). They parse every golden file
  back; no code that tent runs parses HCL.

## Consequences

### Positive

- tent-node can be written against a contract that golden files pin.
- A new mirror, a new server, a change of `access` or a resize of the server group leaves the nodes up to date.
- No secret reaches a machine before the bootstrap that scrubs it exists.
- One envelope and one budget serve every provider, and the combined worst case leaves about 12.5 KiB for
  `extraConfig`.
- A new CNI version marks only the nodes that run workloads out of date.
- A value that Nomad would misread fails validation or rendering on the operator's machine, never on a node.

### Negative / trade-offs

- The host accepts SSH and, on servers, 4646 from any source. Only the cloud firewall narrows them, so a node that has
  lost its firewall group ([architecture §11.5](../architecture.md#115-firewall-and-host-firewall)) is open on those
  ports.
- `05-join.hcl` and `11-instance.hcl` are outside the hash and outside tent's diffs.
- `extraConfig` can override tent's settings and break a node. tent does not check it.
- The code stays unused by `update` until M2.7.
- gz+b64 is unverified on Vultr; the spike wrote a `b64` payload only
  ([platform notes §3.4](../platform-notes.md#34-user_data-metadata-and-identity)).
- Nomad parses with its own fork of HCL1, and the tests parse with upstream v1.0.0. `nomad config validate` has not
  checked the goldens yet.

### Follow-ups

- **M2.5:** `cmd/tent-node`; a test that `go list -deps ./cmd/tent-node` holds neither go-crypto nor `internal/assets`
  nor `internal/cloud`; gz+b64 on a real Vultr VM; `nomad config validate` of the goldens in the online job or E2E.
  `tent-node install` runs inside cloud-final through `exec`, so it must not wait for a unit ordered after
  `cloud-final.service` or `cloud-init.target`, or boot deadlocks.
- **M2.6:** the phases read `system`, `firewall` and `join` and call `RenderJoin` and `RenderInstance`.
  - The nftables ruleset lets bridge-mode workloads reach the host from Nomad's bridge subnet, `172.26.64.0/20`.
  - Bridge-mode port mappings arrive as forwarded, DNAT-ed traffic. A drop in any nftables base chain wins, so tent's
    forward chain must not drop what the chains of Nomad's CNI plugins and Docker accept.
  - A metadata block that exempts root does not stop root containers with host networking or `raw_exec` tasks.
    Matching workloads by cgroup is sturdier.
  - tent-node creates `/var/lib/nomad/client` with mode 0700 before it writes the intro token.
  - tent-node's unit stops Nomad with SIGTERM, not the stock SIGINT, and with a `TimeoutStopSec` above the drain
    deadline, or `leave_on_terminate` and `drain_on_shutdown` never act.
- **M2.7:** `update` builds NodeConfig: the assets once per run, a NodeConfig per node, the spec hash as a label, real
  user data, its size checked at plan time, the Nomad pin written before the first node, the development-variable
  warning, the asset source given by `cmd/tent`, and the run's clock for the signature check.
- **M3:** the report of out-of-date nodes and a Nomad configuration diff per group.
- **A provider below 24 KiB,** such as AWS: `Capabilities.MaxUserDataBytes`.

## Alternatives considered

- **Wire NodeConfig into `update` now.** Nodes would get keys and the gossip key in user data that nothing scrubs
  until the bootstrap of M2.7 exists.
- **Put the instance id in by PATCH after the create.** The node could boot before the PATCH lands and read the old
  user data; it adds a call per node; and Hetzner's user data cannot change.
- **A 64 KiB budget, or one per provider, now.** Hetzner allows 32 KiB and AWS 16 KB, so one budget under both keeps
  one envelope and one test. A per-provider limit waits for the provider that needs it.
- **Copy the `access` sources to the host firewall.** A change of `access.ssh` would change every node's hash and
  roll the cluster, to repeat what the cloud firewall already enforces.
- **Put the gossip key in `00-tent.hcl`.** That file is in the hash, readable by every local user (0644) and shown in
  configuration diffs, so the key would print. A file of its own is 0600 and left out of both.

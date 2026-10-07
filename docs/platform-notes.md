# Platform notes

Facts about Nomad, Hetzner Cloud, Vultr, S3-compatible object stores, prior art and Ubuntu on nodes that tent's
design relies on.

> **Verified on 2026-09-25**, [section 5](#5-s3-compatible-object-stores) on 2026-09-26,
> [section 6](#6-ubuntu-on-nodes) on 2026-09-29 ([6.4](#64-restarts-of-docker-containerd-and-nomad) on 2026-10-05,
> [6.5](#65-cloud-init-and-the-stub) on 2026-10-06) and [1.6](#16-the-agent-on-a-node) from 2026-09-29 to 2026-10-05,
> and the items of [1.2](#12-features-tent-relies-on) that give their own date.
> Sources: official documentation, the Hetzner Cloud OpenAPI spec
> (`https://docs.hetzner.cloud/cloud.spec.json`), the Vultr API reference (OpenAPI spec from a Wayback copy of
> `https://www.vultr.com/api/`, 2026-09-12) plus live calls to public Vultr endpoints, release APIs and upstream
> source code (Nomad `main`, kops `master`, hcloud-go, govultr, cloud-init). The confidence is high unless marked
> otherwise.
>
> Items marked 🔬 are **unverified** and are checked with `hack/vultr-spike` against a real account before code
> depends on them. Facts marked "spike" or "VM check" with a date were measured by the spike runs of that day (see
> [3.16](#316-spike-runs)).
>
> Items marked ⏳ are **volatile**: prices, availability, versions, incidents. Re-check them before relying on them.
> When a fact here turns out wrong, fix it and note the date.

## Contents

1. [Nomad](#1-nomad)
2. [Hetzner Cloud](#2-hetzner-cloud)
3. [Vultr](#3-vultr) (spike results: [3.16](#316-spike-runs))
4. [Prior art](#4-prior-art)
5. [S3-compatible object stores](#5-s3-compatible-object-stores)
6. [Ubuntu on nodes](#6-ubuntu-on-nodes)
7. [Sources](#7-sources)

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

The items that name a source file were checked in the Nomad **v1.11.3** source (the Go module cache) on 2026-09-29.
Files under `api/` are those of the API module at tent's pin ([1.5](#15-licensing)). The source of 2.0.7 was not
checked offline, except the items that say v2.0.7, which were read in that tag on 2026-09-29 or on the date they
give. ⏳ Re-check them against the version tent runs.

**Client introduction** (1.11.0, CE):
- **Server configuration.** In the `server` block:
  `client_introduction { enforcement = "none|warn|strict" (default warn), default_identity_ttl = "5m",
  max_identity_ttl = "30m" }`.
- **Creating a token.** `nomad node intro create [-node-name] [-node-pool] [-ttl]`, or
  `PUT` or `POST /v1/acl/identity/client-introduction-token {NodeName, NodePool, TTL}`, which returns `{"JWT": …}`
  (`ACLCreateClientIntroductionTokenRequest` in `command/agent/acl_endpoint.go`). The Go client's
  `ACLIdentity().CreateClientIntroductionToken` sends PUT (`api/acl.go`). Needs `node:write`.
  - A TTL above `max_identity_ttl` is cut to it. The server logs a warning, and the answer does not say so
    (`IdentityTTL` in `nomad/structs/acl.go`).
  - The server does not check that the pool exists (`ACL.CreateClientIntroductionToken` in
    `nomad/acl_endpoint.go`).
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
- `PUT` or `POST /v1/acl/bootstrap {"BootstrapSecret": "<UUID>"}` (`ACLTokenBootstrap` in
  `command/agent/acl_endpoint.go`). The Go client's `ACLTokens().BootstrapOpts` sends PUT (`api/acl.go`).
- A secret that is not a UUID gets 400 `invalid acl token`. Without a secret, Nomad makes one.
- **Not idempotent.** A second call fails with 400 `ACL bootstrap already done (reset index: N)`, before the secret is
  checked (`ACL.Bootstrap` in `nomad/acl_endpoint.go`). To make tent idempotent, verify the stored secret with
  `GET /v1/acl/token/self` after such an error.
- **`token/self` with a secret that matches no token** answers 403 `Permission denied`: `Authenticate` turns the
  unknown token into that error (`nomad/auth/auth.go`, `TestACLEndpoint_WhoAmI` in `nomad/acl_endpoint_test.go`). A
  secret of an identity without an ACL token, such as a node's secret, gets 404 `ACL token not found`
  (`aclTokenSelf` in `command/agent/acl_endpoint.go`).

**Autopilot and Raft:**
- `GET /v1/operator/autopilot/health` (`operator:read`) returns `Healthy`, `FailureTolerance`, `Leader`, `Voters`
  and `Servers[]` (ID, Name, Address, SerfStatus, Version, Leader, LastContact, LastTerm, LastIndex, Healthy, Voter,
  StableSince).
- **It returns HTTP 429 while unhealthy**, with the same report as the body (`OperatorServerHealth` in
  `command/agent/operator_endpoint.go`). The Go `AutopilotServerHealth` then returns an error instead of the body, so
  handle 429 explicitly.
- `cleanup_dead_servers` defaults to true.
- **Leadership transfer** (since 1.7.0): `PUT /v1/operator/raft/transfer-leadership?id=<peer id>` or
  `nomad operator raft transfer-leadership`.
- **Removing a peer:** `DELETE /v1/operator/raft/peer?id=<raft id>` with a management token. `?address=` returns
  400.
- **The Raft configuration** (`GET /v1/operator/raft/configuration`; v2.0.7, run on 2026-10-05 and read on
  2026-10-06) lists every server
  of the Raft configuration with `Node`, `Address` and `Voter`, and has no lag.
  - `Operator.RaftGetConfiguration` (`nomad/operator_endpoint.go:46-107`) forwards to the leader and answers a
    management token only; any other token gets 403.
  - `Node` is the server's name in Nomad, and `(unknown)` when no Serf member has the Raft address. `Address` is the
    Raft address, on the RPC port (4647 with tent's configuration). `Voter` is `Suffrage == raft.Voter`.
- **The autopilot report is a snapshot** that autopilot rebuilds on a timer from the Raft configuration of that
  moment (raft-autopilot v0.1.6 `nextServers`; run on local Nomad 2.0.7 on 2026-10-05). It lists every Raft server
  with `Name`, `Address` and `Voter`, also in the body of a 429. It lags Raft, so use it for health and the Raft
  configuration for membership.
  - After a SIGKILL of one of three servers, the report answered 200 with the dead server alive, `Voter` and
    `Healthy` for 41 s (Serf marked it failed at 41 s). It turned 429 at 41.7 s, autopilot removed the server from
    Raft at 47.8 s, and the report dropped it at 49.8 s. A 200 report does not mean that its servers are up.
  - A Raft server without a Serf entry stays in the report with `SerfStatus` `left` and the name it had, or an empty
    one (read, not run). The run of 2026-10-06 below saw a `left` entry.
- **A node's address in `GET /v1/nodes`** is the host of the client's `advertise.http` (`Node.Stub`,
  `nomad/structs/structs.go:2296` at v2.0.7, which splits `HTTPAddr`; run on 2026-10-05: a client that advertised
  `10.64.0.9:15102` showed `"Address":"10.64.0.9"`). It is not the bind address and not `advertise.rpc`. With tent's
  rendering, which sets `advertise.http` to the node's private address, it is that address.
  - The run could not use a second host address, since every agent shared 127.0.0.1, and did not run the template
    that renders tent's address (it needs a real interface in the VPC's CIDR). The real-cloud runs of 2026-10-06
    (`rugw2m`, `sv3vwb`, `rgfckj`; [3.16](#316-spike-runs)) used it: in `sv3vwb` Nomad listed the replacement client
    ready and eligible at its private address, 10.64.0.7.

**Report entries and versions** (v2.0.7, run on 2026-10-06 on a local cluster of three servers and a client, and read
in the source at that tag):
- **An entry of `GET /v1/operator/autopilot/health`** has `Name` as `<name>.<region>` (`s1.global`), `Address` as the
  Raft address `<ip>:<rpc port>`, `SerfStatus`, `Version` (`2.0.7`), `Healthy`, `Voter` and `Leader`. The order of
  `Servers` changes between calls, so find a server by address or name. A node in `GET /v1/nodes` has `Version` `2.0.7`.
- **A killed server** (SIGKILL of a follower, the report read every 5 s, 1 s and 0.2 s) read `alive` for about 36 s
  (Serf's log marked it failed at the same moment), then `left` for 2 to 8 s with `Healthy` false and the cluster
  unhealthy, then left the report. `failed` was never seen, also at 0.2 s. Not explained: `nomad/autopilot.go` maps
  `serf.StatusFailed` to `failed` and `serf.StatusLeft` to `left`. The run of 2026-10-05 above measured 41 s for the
  delay. `none` is a fourth status by the source (not run).

**ACL tokens that expire** (v2.0.7, run on 2026-10-06 with curl and the operator certificate; source at the tag):
- **The create.** `PUT /v1/acl/token {"Name", "Type": "management", "ExpirationTTL": "24h"}` with the bootstrap secret
  answers 200 with an `AccessorID`, a `SecretID` of 36 characters (a UUID) and an `ExpirationTime` exactly 24 hours
  after `CreateTime`. `1m` answers 200. `30s` answers 400 `token 0 invalid: 1 error occurred:\n\t* expiration time
  cannot be less than 1m0s in the future (was 30s)`, and `24h1m` 400 `... cannot be more than 24h0m0s in the future (was
  24h1m0s)`.
  - The limits are `ACLTokenMinExpirationTTL` 1 minute and `ACLTokenMaxExpirationTTL` 24 hours (`nomad/config.go`
    lines 697 and 698), checked in `ACLToken.Validate` (`nomad/structs/acl.go` line 756). Nothing there depends on the
    type, so a management token may expire.
  - `GET /v1/acl/token/self` with the new secret answers 200 with the same accessor, name and times.
- **After the end.** A token of 1 minute, read 4 s after its end: `GET /v1/acl/token/self` answers 500 `rpc error: ACL
  token expired`, while `GET /v1/jobs` and `GET /v1/operator/autopilot/health` answer 403 `Permission
  denied`. By the answer of `/v1/jobs` or of the autopilot report a caller cannot tell an expired token from a missing
  right.
- **The bootstrap secret** must be a UUID, else 400 `invalid acl token`.
- ⏳ A one-time token lives 10 minutes, and `nomad ui -authenticate` makes one (v1.11.3 source, not checked on 2.0.7);
  tent uses neither.

**The `nomad` CLI's environment** (v2.0.7, run on 2026-10-06; `api/api.go` lines 342 to 388):
- The client reads `NOMAD_ADDR`, `NOMAD_REGION`, `NOMAD_NAMESPACE`, `NOMAD_HTTP_AUTH`, `NOMAD_CACERT`, `NOMAD_CAPATH`,
  `NOMAD_CLIENT_CERT`, `NOMAD_CLIENT_KEY`, `NOMAD_TLS_SERVER_NAME`, `NOMAD_SKIP_VERIFY` and `NOMAD_TOKEN`. No variable
  names a token file.
- With `NOMAD_ADDR`, `NOMAD_CACERT`, `NOMAD_CLIENT_CERT`, `NOMAD_CLIENT_KEY`, `NOMAD_TLS_SERVER_NAME` and `NOMAD_TOKEN`
  and nothing else (checked with `env`), against servers whose certificate has DNS names only, `nomad server members`,
  `nomad node status` and `nomad job run` of a small job worked, in sh through `eval` and in fish through `source`
  (run as `fish --no-config`). The key was PKCS#8, and the token file held 36 bytes with no line end.
- Without `NOMAD_TLS_SERVER_NAME` each command exits 1 with `x509: cannot validate certificate for 127.0.0.1 because it
  doesn't contain any IP SANs`.
- A raw_exec job needs `plugin "raw_exec"` in the client configuration, since the driver is disabled by default.
- After its table, `nomad server members` prints a hint on stderr: two empty lines and `==> View and manage Nomad
  servers in the Web UI: <NOMAD_ADDR>/ui/servers`. `NOMAD_CLI_SHOW_HINTS=false` turns it off (run on 2026-10-07 against
  a cluster that tent built). A script that reads the table takes stdout alone. Through `tent ui` the hint named the
  proxy's address. With the exported variables it then names a server's own `https` address (not seen: the run kept
  no output of that call), which a browser cannot open without the client certificate.

**The web UI and the CLI behind a proxy** that adds mTLS and a management token (v2.0.7, run on 2026-10-06 in Chromium
through Playwright, with a throwaway `httputil.ReverseProxy`, and read in the source at the tag):
- **No sign-in.** `/ui/` lists jobs, clients and servers with no token entered. The UI calls `/v1/acl/token/self` on
  every load and stores the secret of the answer in `localStorage.nomadTokenSecret`, so the browser then holds the
  proxy's token (the same sha256). A made-up secret stored first is overwritten, with no error shown
  (`ui/app/services/token.js`, `routes/application.js`).
- **Exec** works with `Origin` removed. With `Origin` passed on and `Host` rewritten to the server's, the upgrade fails
  with 500 `websocket: request origin not allowed by Upgrader.CheckOrigin`; with `Origin` passed on and the browser's
  `Host` kept it works. The UI's live updates are websockets too (`ws_handshake=true`,
  `ui/app/services/watcher-fetch.js`), so the same pair breaks them. An agent option
  `http_disable_websocket_origin_check` and dev mode turn the check off (`command/agent/http.go`).
- **The websocket handshake** reads the token from the first frame and, when a header also carries one, requires the two
  to be equal (`command/agent/websockets.go`, `readWsHandshake`). Behind the proxy they are.
- **Live updates.** A job's page changed within 6 s of `nomad job stop`, with no reload, and the log view followed new
  lines.
- **The log view** first calls the node's own HTTP address from the browser, which fails (the node's API is not
  reachable from outside), and falls back to the server through the proxy after a delay
  (`ui/app/components/task-log.js`).
- **CORS.** Nomad answers cross-origin requests for 14 wrapped endpoints (`/v1/client/fs/`, `/v1/client/stats`,
  `/v1/client/allocation/`, `/v1/client/metadata`, `/v1/client/identity`, `/v1/vars`, `/v1/var/` and others), with all
  origins, all headers and credentials. `/v1/jobs`, `/v1/agent/self` and `/v1/namespace*` have none.
- **Another page** (on another port of the same host, whose `Sec-Fetch-Site` is `same-site`) calling a proxy that adds
  the token and removes `Origin`: `fetch('/v1/jobs')` failed in the page, but the request reached Nomad, and a blind
  `fetch(..., {method: 'POST', mode: 'no-cors'})` of a namespace created it. With `Origin` passed on, `/v1/vars` and
  `/v1/client/stats` were readable. A proxy that answers 403 to a foreign `Host`, `Origin` or a `Sec-Fetch-Site` other
  than `same-origin` and `none` stopped all of it, and nothing was created.
- **The CLI** with `NOMAD_ADDR=http://127.0.0.1:<proxy>` and no other variable ran `nomad status`, `job run`, `job
  stop`, `alloc logs` and `alloc exec`, with the checks on. It sends neither `Origin` nor `Sec-Fetch-Site`.
- Not tested: a page left open across a restart of the proxy with a new token, and whether the UI falls back to polling
  when a websocket fails.

**Node pools** (since 1.6.0):
- Client configuration: `client { node_pool = "x" }`, default `default`. The built-ins `default` and `all` cannot
  be modified, and `all` is for jobs only. A change to either gets 400 `modifying node pool "…" is not allowed`
  (`NodePool.UpsertNodePools` in `nomad/node_pool_endpoint.go`).
- **Pools are auto-created** when a client registers in the authoritative region (`Node.Register` in
  `nomad/node_endpoint.go`, `UpsertNode` in `nomad/state/state_store.go`). A job that references a missing pool fails
  to register.
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
- **File merging.** `LoadConfigDir` (`command/agent/config.go`) reads the `.hcl` and `.json` files of a directory,
  skips temporary files, sorts the paths and merges the files in that order. A later file's single values
  win, except plain booleans such as `leave_on_terminate` and `leave_on_interrupt`: they merge with OR, so a
  later `false` does not undo an earlier `true` (`Config.Merge`, v2.0.7).
  `client.meta` and `client.options` merge key by key. A later, non-empty `server_join.retry_join` replaces the
  earlier list (`ServerJoin.Merge`).
- **Unknown keys are errors.** Each file is checked for keys that no field takes (`extraKeys` in
  `command/agent/config_parse.go`, `helper.UnusedKeys` in `helper/funcs.go`).
- **Parser.** Agent configuration is still parsed with **HCL1**, with no HCL2 functions. Nomad's `go.mod` replaces
  `github.com/hashicorp/hcl` with its fork `v1.0.1-nomad-1`. Upstream v1.0.0, which tent's tests use, shows
  (`hcl/scanner/scanner.go`, `hcl/strconv/quote.go`):
  - the scanner refuses U+E123 anywhere in a file, as "reserved for internal use";
  - in a quoted string, `${` opens a span that runs to its matching `}`. Quotes inside it do not end the string, and
    backslash escapes inside it are kept as written. HCL1 has no escape for `${`.

  The fork itself was not read. Nomad 2.0.0 and 2.0.7, which parse with it, accept tent's golden files:
  `nomad config validate` passed each role's goldens on 2026-10-02 ([1.6](#16-the-agent-on-a-node)).
- **Graceful shutdown settings.**
  - `leave_on_interrupt` and `leave_on_terminate` default to false.
  - With them enabled, a server leaves the peer set gracefully. SIGTERM leaves only with `leave_on_terminate`, and
    SIGINT only with `leave_on_interrupt` (`handleSignals` in `command/agent/command.go`).
  - Clients with `drain_on_shutdown { deadline, force, ignore_system_jobs }` drain themselves on shutdown, and stay
    ineligible after their next start ([1.6](#16-the-agent-on-a-node)).
  - How long the agent waits, and what a server's leave does to Raft: [1.6](#16-the-agent-on-a-node).
- **Jobs default to `datacenters = ["*"]`** (since 1.5).

**Client settings tent writes** (v1.11.3 source, 2026-09-29, as above):
- **`client.options."driver.allowlist"`** is a comma-separated list of driver names, each trimmed of white space
  (`splitValue` in `client/config/config.go`). A non-empty list starts only those drivers
  (`client/pluginmanager/drivermanager/manager.go`); an empty one starts all. `driver.whitelist` is the old name.
  `raw_exec` stays disabled until its plugin sets `enabled = true` (`drivers/rawexec/driver.go`).
- **Dynamic ports.** `client.min_dynamic_port` and `max_dynamic_port` default to 20000 and 32000
  (`nomad/structs/network.go`, `DefaultConfig` in `command/agent/config.go`).
- **ACLs on clients.** A client takes `acl.enabled` from the agent configuration (`convertClientConfig` in
  `command/agent/agent.go`). With it off, the client resolves every token to an ACL that allows everything
  (`resolveTokenAndACL` in `client/acl.go`) and accepts any migrate token (`ValidateMigrateToken` in
  `client/client.go`). So `acl { enabled = true }` belongs on clients too.
- **The state directory and the intro token.** `client.state_dir` defaults to `<data_dir>/client`
  (`convertClientConfig`). When no token came by flag or environment, the agent reads `intro_token.jwt` in that
  directory; a missing file is no error (`readIntroTokenFile` in `command/agent/agent.go`).
- **Joining.** `server { server_join { retry_join } }` joins the servers' Serf, whose default port is 4648.
  `client { server_join { retry_join } }` sets the client's servers, and an address without a port gets 4647
  (`resolveServer` in `client/rpc.go`). One agent may have both blocks.
  - `server.retry_join` still works, with a deprecation warning, and cannot be combined with `server_join`
    (`handleRetryJoin` in `command/agent/command.go`, `retryJoiner.Validate` in `command/agent/retry_join.go`).
  - `start_join` is refused for clients.
- **A combined agent's client reaches its own server over the network.** When the agent runs a server, it adds the
  server's RPC bind and advertise addresses to the client's servers (`finalizeClientConfig` in
  `command/agent/agent.go`). No code outside tests sets the client's in-process `RPCHandler`
  (`client/config/config.go`, `client/rpc.go`). So a combined node must be able to reach its own RPC port on its
  private address.
- **Bridge networking.**
  - Nomad's CNI page asks for `net.bridge.bridge-nf-call-arptables`, `-ip6tables` and `-iptables` set to 1 (docs,
    read 2026-09-29). These sysctls exist only while the `br_netfilter` module is loaded.
  - The default bridge subnet is `172.26.64.0/20` (`client/allocrunner/networking_bridge_linux.go`). Bridge-mode
    workloads reach the host from it.
  - tent's choice for client and combined nodes: load `br_netfilter` and set the three sysctls; install Docker from
    the distribution's packages, with the `overlay` module for its storage, unless the group's drivers leave `docker`
    out. Servers get none of it.
  - **Nomad's CNI configuration** (v1.11.3 and v2.0.7, `client/allocrunner/cni/bridge.go`): cniVersion 0.4.0, name
    `nomad`. `loopback`; `bridge` on the bridge `nomad` with `ipMasq`, `isGateway`, `forceAddress`, `hairpinMode` from
    `bridge_network_hairpin_mode` (default false) and host-local IPAM on the bridge subnet; `firewall` with the
    iptables backend and the admin chain `NOMAD-ADMIN`; `portmap` with `snat`.
  - **In iptables** (CNI plugins v1.9.1 and Nomad v2.0.7 source): the firewall plugin puts `-j CNI-FORWARD` first in
    `filter FORWARD`. `CNI-FORWARD` jumps to `NOMAD-ADMIN`, then accepts each allocation's replies
    (`-d <IP> ctstate RELATED,ESTABLISHED`) and its own traffic (`-s <IP>`); Nomad adds
    `-o nomad -d 172.26.64.0/20 -j ACCEPT` to `NOMAD-ADMIN`. So Docker's drop policy on `FORWARD`
    ([6.2](#62-firewalls-on-the-host)) does not break the bridge. portmap DNATs mapped ports in `nat PREROUTING` and
    `OUTPUT` (`CNI-HOSTPORT-DNAT`) and marks what it masquerades with `0x2000`.
  - **A mapped port reached from another host never reaches the host's input chain:** the DNAT before routing makes
    it forwarded traffic to the post-DNAT address (the hook order is verified; the rest is inferred). A workload on a
    bridge that calls the local agent, or a task with host networking, at the node's address does reach input. So
    does a Docker container that calls a published port of its own node: with `userland-proxy` on, the default, the
    DNAT rule leaves out `docker0`, and docker-proxy answers (moby `iptabler/port.go`).
  - **Backends.** The CNI plugins added nftables backends to `ipMasq` and `portmap` in v1.6.0 (2024-10-15); both use
    iptables unless iptables is unusable (v1.9.1). The firewall plugin has only iptables and firewalld, and Nomad
    picks iptables. ⏳ v1.9.1 (2026-03-16) is the latest; v1.9.0 fixed CVE-2025-67499 in the nftables portmap.

**Workloads on a client** (read 2026-09-29), for the metadata block
([architecture §9.4](architecture.md#94-secrets-on-nodes-threat-model)):
- **Where they run** (v1.11.3 and v2.0.7):
  - A client creates `/sys/fs/cgroup/nomad.slice/{share,reserve}.slice` as plain directories when the agent starts;
    a server-only agent does not.
  - exec, raw_exec and java tasks run in `nomad.slice/<share|reserve>.slice/<alloc>.<task>.scope`; raw_exec honours a
    job's `cgroup_v2_override`.
  - The docker driver sets no cgroup parent since 1.7.0 (GH-18371), so containers run in
    `system.slice/docker-<id>.scope`, next to other services.
  - The artifact fetcher runs as root inside `system.slice/nomad.service`.
- **Capabilities.** Nomad's default capabilities give tasks neither CAP_NET_ADMIN nor CAP_NET_RAW (v2.0.7
  `drivers/shared/capabilities/defaults.go`). An operator adds them with a driver's `allow_caps` and a task's
  `cap_add`. Docker's own default set, outside Nomad, includes NET_RAW.
- **Container logs.** Nomad's docker driver sets json-file with 2 files of 2 MB on each container it starts (v2.0.7
  `drivers/docker/config.go`), whatever the daemon's defaults.

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
  the Raft peers' RPC addresses (`["10.0.0.5:4647", …]`). Their answers with `?stale` and on clients:
  [1.6](#16-the-agent-on-a-node).
- **`GET /v1/agent/servers`** (`agent:read`) lists the servers a client knows.
- **`PUT /v1/agent/servers?address=…`** (`agent:write`) replaces that list. Whether this survives a restart is not
  documented.
- **There is no `/v1/agent/leave`.** No endpoint makes an agent leave gracefully.
- **`PUT /v1/agent/force-leave?node=<name>&prune=true`** (`agent:write`) removes a failed or left member from the Serf
  member list immediately. A member that is still alive rejoins.

**Without a leader:**
- A server that knows no leader holds each request that needs one for the RPC hold timeout, 5 s by default
  (`RPCHoldTimeout` in `nomad/config.go`). Then it answers 500 `No cluster leader` (`getLeaderForRPC` in
  `nomad/rpc.go`, the default status of `wrap` in `command/agent/http.go`). Writes need the leader, and so do reads
  without `?stale`.
- `GET /v1/status/leader` goes the same way: without a leader it answers 500 `No cluster leader`, and `""` only with
  `?stale` (`Status.Leader` in `nomad/status_endpoint.go`).

**The Go API client over HTTP:**
- `Nodes().List` sorts the list by `CreateIndex` and panics on a `null` element (`NodeIndexSort.Less` in
  `api/nodes.go`).
- `api.NewClient` calls `DefaultConfig`, which reads the `NOMAD_*` variables, but takes only its address, and only
  when the given one is empty (`api/api.go`).
- The client sends the token as `X-Nomad-Token` (`api/api.go`). Go's `net/http` client copies every header of the
  first request to each redirect. It drops only `Authorization`, `Www-Authenticate`, `Cookie`, `Cookie2`,
  `Proxy-Authorization` and `Proxy-Authenticate`, and those only when the host changes (`makeHeadersCopier` in
  `net/http/client.go`, Go 1.27.1). So the token follows any redirect.
- An agent takes at most 100 HTTP connections from one client IP by default:
  `limits { http_max_conns_per_client = 100 }` (`DefaultLimits` in `nomad/structs/config/limits.go`, `connLimiter`
  in `command/agent/http.go`).

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
  `LICENSE.txt` (mode 0644) and then `nomad` (0755), both regular files at the top, deflated, and nothing else. The
  linux zips of 2.0.0 and 2.0.7 take 53 to 59 MB and their binaries 137 to 147 MB: 2.0.7's `nomad` is 147,230,688
  bytes on amd64 and 137,387,200 on arm64 (their central directories, read on 2026-10-02).
- **Checksums and signatures:** `nomad_{V}_SHA256SUMS`, plus detached signatures `nomad_{V}_SHA256SUMS.sig` and
  `….72D7468F.sig`.
- **JSON index:** `https://api.releases.hashicorp.com/v1/releases/nomad/{V|latest}`.
- **Release signing key:** `https://www.hashicorp.com/.well-known/pgp-key.txt`, fingerprint
  `C874 011F 0AB4 0511 0D02 1055 3436 5D94 72D7 468F`. The 2.0.7 signature and the linux_amd64 sha256 were checked
  and are valid. Checked again on 2026-09-28, for the copy that tent embeds:
  - The primary key is RSA 4096, made on 2021-04-19, and expires on 2030-03-01T23:14Z.
  - Releases are signed with its subkey `374E C75B 4859 1360 4A83 1CC7 C820 C6D5 CD27 AB87` (RSA 4096, made on
    2021-04-21), which expires on 2030-03-01T23:15Z. The 2.0.7 `SHA256SUMS.sig` is made with this subkey and SHA-256.
  - The file also holds an encryption subkey, which expires with the primary key, and a signing subkey that expired
    on 2022-04-20.
  - A verifier that checks the key at the current time, as tent does, rejects every signature after the expiry,
    whenever it was made. HashiCorp can move the expiry with new self-signatures, and a verifier sees that only with a
    fresh copy of the key.
- **APT.** ⏳ `apt.releases.hashicorp.com` provides arm64. Its signing key was **rotated on 2026-09-09** after a
  security incident (HCSEC-2026-33); the new key is `D55C 0D1A C78A 8D81 26CB 631C FC9C A96A CA02 6560`. tent does not
  use the APT repository.
- **CNI plugins** ⏳ (`containernetworking/plugins`, which Nomad's bridge networking needs): the latest release is
  v1.9.1, of 2026-03-16. Each archive,
  `https://github.com/containernetworking/plugins/releases/download/v{V}/cni-plugins-linux-{amd64|arm64}-v{V}.tgz`,
  has a `.sha256` file next to it and no signature. The v1.9.1 sha256s in tent's `stable` channel were checked
  against the digests that GitHub lists for the release's assets on 2026-09-28.

### 1.5 Licensing

- **Nomad is BUSL 1.1 from 1.7.0.** The 1.6.x tags are MPL-2.0.
- **MPL-2.0 modules:** `github.com/hashicorp/nomad/api` (a separate module with no semver tags, so pin by
  pseudo-version; requires Go 1.26+), `jobspec2`, `go-discover`, `go-netaddrs`.
- **Do not import root-module packages** such as `helper/tlsutil`: they are BUSL.
- **tent's pin.** tent pins `nomad/api` at `v0.0.0-20260917172403-9dcbdc5e64ec`, the commit of tag v2.0.7 (decision
  16 of [architecture §18](architecture.md#18-open-questions)). It brings `hashicorp/cronexpr` v1.1.3,
  `hashicorp/go-rootcerts` v1.0.2 (MPL-2.0), `gorilla/websocket` v1.5.3 (BSD-2-Clause), `go-viper/mapstructure/v2`
  v2.5.0 (MIT) and `mitchellh/go-homedir` v1.1.0 (MIT). `hashicorp/go-cleanhttp` v0.5.2 (MPL-2.0), an indirect
  dependency before, is now direct. The licence check passes on `./cmd/tent ./internal/nomadops` (2026-09-29).
- **cronexpr** offers Apache-2.0 or GPLv3 (its README) and ships both texts, as `APLv2` and `GPLv3`. Its `LICENSE` is
  Apache-2.0, and tent uses it under Apache-2.0. The licence check reads only files named like `LICENSE`, `COPYING`
  or `NOTICE`.
- **Size.** Linking `internal/nomadops` adds about 150 KB to the `tent` binary in a release build (`-s -w`), about
  210 KB unstripped. Since M2.7a `update` calls it.
- **Competitive use.** BUSL "competitive offering" covers products provided on a paid basis. A free tool that
  downloads official binaries is fine; a paid managed-Nomad offering would need a commercial licence. This is not
  legal advice.
- **Forks.** No maintained, foundation-backed fork exists. OpenNood/nood (based on 1.6.5) has had no commits since
  December 2023. (Medium confidence.)

### 1.6 The agent on a node

What tent-node's `join`, `nomad`, `verify` and `refresh-join` rely on, and the shutdown facts behind tent's agent
configuration ([ADR-0030](adr/0030-nomad-on-nodes.md)). Read in the v2.0.7 tag (commit `9dcbdc5`, 2026-09-17) and
in the docs of 2.0.x on 2026-09-29, and seen in live runs of the official `nomad_2.0.7_darwin_arm64` binary, with ACLs
on and without TLS, on 2026-09-29, unless an item says otherwise. The M2.6b VM checks of 2026-10-05 (runs `bxkllh`
on Ubuntu 24.04 and `g2rl9d` on 26.04, [3.16](#316-spike-runs)) settled the items that name them; those still
marked 🔬 are open.

**Health and status** (live runs, and the source where an item says so):
- **`GET /v1/agent/health`** needs no ACL token. It answers 200 with `application/json` when the agent is healthy,
  and 500 with the same JSON as `text/plain` when not. `?type=client` or `?type=server` checks one part; a part that
  the agent does not run fails (`client not enabled`).
  - A server runs the `Status.Leader` RPC, which the leader answers. Without a leader it answers 500
    `{"server":{"ok":false,"message":"No cluster leader"}}` after the RPC hold of 5 s. With `?stale` it answers 500 at
    once, with the message `no leader`.
  - A client is healthy when its list of servers is not empty. The list starts empty, and retry_join adds a server
    only after a `Status.Ping` to it succeeds. Unhealthy is `{"client":{"ok":false,"message":"no known servers"}}`. In
    a live run the client turned healthy right after the join although `Node.Register` failed with
    `Permission denied`.
  - On a combined agent the client part is healthy at once, since the local server's RPC address is in its list
    ([1.2](#12-features-tent-relies-on)). The server part needs a leader.
- **`GET /v1/status/peers` and `/v1/status/leader`** need no token: the agent drops their auth errors on purpose.
  - A server without a leader answers 500 `No cluster leader` after 5 s. With `?stale` it answers from its local Raft
    configuration: `[]` before the bootstrap, and `""` for the leader. So `GET /v1/status/leader?stale` answers 200
    on a server without a leader.
  - Peers are RPC addresses, `"10.0.0.5:4647"`, and in IPv6 `"[fd00::1]:4647"` (source only).
  - A client forwards both to a server, but blocks until the node has registered: in a live run a request to a
    client that had not registered hung for 12 s.
- **Server certificates name no node address**: tent's carry `127.0.0.1` alone
  ([architecture §9.1](architecture.md#91-pki)). A caller that dials a server's private IP sets
  `tls.Config.ServerName` to `server.<region>.nomad`. The M2.6b VM checks made one such call on a real node:
  `refresh-join` asked the node's own agent at `127.0.0.1` with `server.global.nomad`, and got the node's address.
  The M2.7a cluster check (run `qypvsk`, 2026-10-05, [3.16](#316-spike-runs)) made the first call between two machines:
  a client's `05-join.hcl` lists the three servers' RPC addresses, and its `tent-node-join.service` ran 3 times with
  0 failed runs. The answer matched the seed, so the file was not rewritten; that the mTLS call with the TLS name
  `server.<region>.nomad` worked is inferred from the runs that did not fail. 🔬 E2E still has to show a call after
  servers have been replaced.

**The stock unit** (`.release/linux/package/usr/lib/systemd/system/nomad.service`): `Wants=` and
`After=network-online.target`; `User=` and `Group=root`; `Type=notify`; `EnvironmentFile=-/etc/nomad.d/nomad.env`;
`ExecReload=/bin/kill -HUP $MAINPID`; `ExecStart=/usr/bin/nomad agent -config /etc/nomad.d`; `KillMode=process`;
`KillSignal=SIGINT`; `LimitNOFILE=65536`; `LimitNPROC=infinity`; `Restart=on-failure`; `RestartSec=2`;
`TasksMax=infinity`; `OOMScoreAdjust=-1000`; `WantedBy=multi-user.target`. HashiCorp's e2e units add
`StartLimitIntervalSec=0` and `StartLimitBurst=3`.
- **sd_notify** (since 1.8.0): the agent sends `READY=1` once it is set up and its signal handlers are in place,
  without waiting for a join or a leader, and again after a SIGHUP reload (`handleSignals` after `setupAgent` in
  `command/agent/command.go`, v1.11.3, read 2026-10-02). On the M2.6b VM checks `systemctl start` of a combined node's
  `Type=notify` unit returned 1.0 to 1.4 s after tent-node asked for it (by the timing of the log lines), far within
  systemd's default start timeout of 90 s.
- **`KillMode=process`:** the executor and logmon processes of tasks stay in the unit's cgroup and must outlive a
  restart of the agent. They do: on the M2.6b VM checks `systemctl restart nomad.service` with a docker job running
  left its container with the same id and start time and the task unrestarted. At a shutdown systemd logs
  `Unit process … (nomad) remains running after unit stopped` for two such processes.
- **`Delegate=yes` is not needed:** Nomad writes the root `cgroup.subtree_control` and makes `nomad.slice` itself
  (GH-18211).
- **cgroups v2:** the client needs root and `cpuset cpu io memory pids` in `/sys/fs/cgroup/cgroup.controllers`, and
  nothing from the unit. On the M2.6b VM checks the client reported the docker and exec drivers healthy on 24.04
  and 26.04 (`Driver Status = docker,exec`), so its cgroup checks passed (inferred). 🔬 `nomad.slice` itself was not
  listed.

**Stopping** (`command/agent/command.go`):
- Without `leave_on_terminate`, SIGTERM makes the agent exit with 1 at once ([1.2](#12-features-tent-relies-on)).
- The graceful wait is `gracefulTimeout`, 5 s, plus the client's `drain_on_shutdown` deadline (`terminateGracefully`,
  v2.0.7). systemd's default stop timeout of 90 s would cut a long drain short; without a drain it covers the 5 s.

**A client's drain at shutdown and its eligibility** (verified on 2026-10-03 in the v2.0.7 tag, commit `9dcbdc5`, and
on a local 2.0.7 cluster on macOS: the official `darwin_arm64` binary, one server and one client, ACLs on; `-dev`
skips the self-drain):
- On SIGTERM with `leave_on_terminate` the client calls `DrainSelf` (`client/drain.go:16-86`). Without a
  `drain_on_shutdown` block it returns at once, since the drain configuration is nil (`DrainConfigFromAgent` in
  `client/config/drain.go`; read in v1.11.3 on 2026-10-03 too), and the agent exits with 0 as soon as the leave
  returns, within the 5-second graceful wait (`command/agent/command.go:1058-1064` and `1083-1089`; `Client.Leave`
  returns at once, `client/drain.go:17-20`). With one it sends
  `Node.UpdateDrain` with `MarkEligible=false` (`drain.go:34`) and the meta message `shutting down`.
- When the drain completes, `NodesDrainComplete` clears the drain strategy and leaves the eligibility as it is
  (`nomad/drainer_shims.go:26`, `nomad/state/state_store.go:1231-1236`). At the client's next registration the
  server keeps the stored eligibility (`state_store.go:1016-1018`), and the client takes it
  (`client/client.go:2299-2303`).
- Live: an eligible client, stopped with SIGTERM, was ineligible, with its last drain's AccessorID `client:<id>` and
  the message `shutting down`; after a start it was `ready` and `ineligible`. A crash, SIGKILL or a power loss leaves
  it eligible (by the source: only `DrainSelf` marks it).
- Nomad has no setting that makes the client eligible again: hashicorp/nomad#17093, open since 2023-05-05 and
  accepted, where a maintainer writes "Currently there's no built-in way to do that". The docs of 2.0.x
  (`configuration/client.mdx`) say nothing about eligibility after `drain_on_shutdown`.
- The node's secret ID can clear it through `PUT /v1/node/<id>/drain` (verified live). Nomad's code says the secret
  ID "is no longer used as the primary authentication method" and still uses it while the servers are not upgraded
  (`client/client.go:935-942`). `node:write` has no per-node scope (`acl/policy.go:239-241`): it covers the drain,
  eligibility, purge and meta of every node, the garbage collection of allocations and intro tokens.

**A server's `leave_on_terminate`** (verified on 2026-10-03 in the v2.0.7 source and live runs of three local
servers with ACLs, retry_join and `bootstrap_expect = 3`):
- With it, SIGTERM makes a leader remove itself from Raft, and a follower waits up to 5 s for the leader to remove it
  (`nomad/server.go:817-908`). That wait (`raftRemoveGracePeriod`, after the Serf leave, `server.go:861-906`) can
  outlast the agent's graceful wait of 5 s: the agent then exits with 1, and systemd would show the unit failed
  after the stop, though the stop, and a restart, complete (inferred; the runs had no systemd). A combined agent in a
  cluster of several servers runs the same server code, so it does the same (by the source, not run).
- At the next start Serf has forgotten its peers (`rejoin_after_leave` is false), retry_join brings the server back,
  the leader adds it as a nonvoter, and autopilot promotes it (`server_stabilization_time`, 10 s). In the runs it was
  a voter again 7 to 20 s after its start; the 7 s case, the former leader, is not explained.
- A whole cluster whose servers left one by one comes back only through the last server to leave: until it starts,
  the others answer `No cluster leader`.
- Without it, a short restart keeps the voter set. A stopped server stays a voter until autopilot removes it (about
  40 s in the run) or `DELETE /v1/operator/raft/peer` does.
- A single server (`bootstrap_expect = 1`, as a combined node of one) skips the removal and comes back as it was.
- A hard power-off never leaves.
- The docs of 2.0.x (`configuration/index.mdx`) say to set it on servers only "if the terminated server will never
  join the cluster again"; hashicorp/nomad#7943 gives the reasons (extra Raft log entries, Raft v2 ids).
- **Run on 2026-10-05** (three local servers with `leave_on_terminate = false`, Nomad 2.0.7 `darwin_arm64`, TLS and
  ACLs on): SIGTERM to a follower made it exit with status 1 within 0.28 s, and 0.44 s in a second run, and the log
  said `nomad: serf: Shutdown without a Leave`. In 25 polls of `GET /v1/operator/raft/configuration`, one a second
  from 2 s before the stop to 12 s after the restart, it stayed `Voter=true` while it was down (8 s) and after it
  restarted with the same `data_dir`. Autopilot was healthy with three voters again. On a real server the unit showed
  failed after that exit status ([3.16](#316-spike-runs), run `qypvsk`, 2026-10-05).

**A new cluster's bootstrap and first clients** (what `update` of M2.7a relies on). Run on 2026-10-05 against local
Nomad 2.0.7 agents: the binary from the official `nomad_2.0.7_darwin_arm64.zip`, whose sha256 matches HashiCorp's signed
`nomad_2.0.7_SHA256SUMS`, ports 14700 to 14732 on 127.0.0.1, region `global`, TLS on HTTP and RPC with
`verify_server_hostname` and `verify_https_client`, ACLs on, certificates made with openssl. Requests came from curl
and python over mTLS; the source is the v2.0.7 tag:
- **`PUT /v1/acl/bootstrap` with an unknown secret in `X-Nomad-Token` is accepted.** With the header set to the
  bootstrap secret S and the body `{"BootstrapSecret": "S"}`, before any bootstrap: HTTP 200 and a management token
  whose `SecretID` was S. A second identical call answered 400 `ACL bootstrap already done (reset index: 8)`, and
  `GET /v1/acl/token/self` with S returned the same token. `GET /v1/status/leader` with an unknown token answered
  200 before the bootstrap.
  - The source agrees. `ACL.Bootstrap` (`nomad/acl_endpoint.go:450-452`) throws away any auth error "so that we can
    measure rate metrics" and only authenticates. A non-empty `BootstrapSecret` must be a UUID, else 400
    `invalid acl token`, and becomes the `SecretID` (lines 497-504). The HTTP handler (`ACLTokenBootstrap`,
    `command/agent/acl_endpoint.go:145-162`) takes PUT or POST, reads the header into the request and checks nothing.
  - `nomadops.Client.Bootstrap` itself was not run: the check used curl and python, not the Go client.
- **A new cluster is healthy at once.** `GET /v1/operator/autopilot/health` with the bootstrap token, for one server
  (`bootstrap_expect = 1`): HTTP 200, `Healthy`, one voter. For three servers (`bootstrap_expect = 3`), started one
  after another within about 1 s: the first leader showed 1.5 s after the poller started, the bootstrap answered 200
  at once, and the first health call after it, 0.02 s after the leader showed, was healthy with three voters, all
  `Voter=true`. The log said `found expected number of peers, attempting to bootstrap cluster`. All three servers are
  in the first Raft configuration, so no promotion is awaited. Localhost timing only: VMs are slower.
- **Intro tokens.** `PUT /v1/acl/identity/client-introduction-token` with `{"NodeName": "n1", "NodePool": "default",
  "TTL": "30m"}` and the management token answered 200 with `{"JWT": …}`. The string `30m` is accepted (`exp - iat =
  1800`); `ACLIdentityClientIntroductionTokenRequest` in `api/acl.go` has `TTL time.Duration`, `NodeName`, `NodePool`.
  - **Size.** 739 bytes for the node name `n1`. The JWT is RS256 with the claims `aud`, `exp`, `iat`, `jti`, `nbf`,
    `nomad_node_name`, `nomad_node_pool` and `sub` (`node-introduction:global:default:n1:default`), so each character
    of a longer name adds about 4/3 bytes twice, in the name claim and in `sub`.
  - **`strict`.** A client with the token in `<data_dir>/client/intro_token.jwt` (0600) registered
    (`client: node registration complete`, `GET /v1/nodes` showed it `ready`). The file stayed after registration. A
    client without a token did not register: the client logged `Permission denied` for `Node.Register`, the server
    `node registration without introduction token: enforcement_level=strict`, and `GET /v1/nodes` was empty.
  - **`warn`.** After the servers restarted with `enforcement = "warn"`, a client without a token registered, and the
    server logged a warning with `enforcement_level=warn`.

**Nodes of one name and expired intro tokens.** Run on 2026-10-05 against local Nomad 2.0.7 (the official
`nomad_2.0.7_darwin_arm64.zip`, sha256 `4ada34db43f8b75c80c4e09ce50f4753e661bcb4a3745f415dd4aa7d01483e58`, which
matches `nomad_2.0.7_SHA256SUMS`): three servers, region `global`, TLS on HTTP and RPC, ACLs on, `strict` client
introduction unless said otherwise, the default heartbeat and garbage-collection settings. The source is the v2.0.7
tag:
- **A killed client reads `down` after 14 to 16 s.** SIGKILL at t=0, polled every 2 s: `ready` at 14.1 s, `down` at
  16.1 s. The defaults allow 20 to 30 s: a heartbeat TTL of 10 s plus a random stagger of up to 10 s, plus a grace of
  10 s (`nomad/config.go`, `nomad/heartbeat.go`). After a leader change every node that is not terminal gets
  `failover_heartbeat_ttl`, 300 s (read, not run), so a dead client can read `ready` for up to 5 minutes then.
- **The node stays listed.** 5 minutes later it was still `down`; `node_gc_threshold` is 24 h (not waited for).
- **A new client of the same name registers at once**, as a second node. With an empty data directory, the same HTTP
  address and a new intro token, `GET /v1/nodes` listed two nodes called `c1`: one `down` and one `ready`, with
  different IDs and the same `Address`.
- **An intro token works for 60 s after its expiry.** `VerifyClaim` (`nomad/encrypter.go`) calls go-jose v3.0.5's
  `Validate`, whose default leeway is one minute. With tokens of TTL 1 s, a new client registered when it started 30,
  50 and 58 s after `exp`, and was refused at 62, 75 and 80 s (`Permission denied` for `Node.Register`; the server
  logged `node registration introduction authentication failure … token is expired (exp)`). The client retries on its
  own and never registers.
- **Under `warn` an expired token is refused too** (the server logged the same failure with `enforcement_level=warn`),
  and a client without a token registers (`node registration without introduction token`). `newRegistrationAllowed`
  (`nomad/node_endpoint.go:312`) returns before it looks at the level when the token fails. The default enforcement is
  `warn` when the `server` block sets none (`nomad/structs/node.go:860`).
- **The server accepts any TTL.** `1s`, `5s`, `10s`, `20s`, `1m`, `0s` (the server default, 5 m), `500ms`, `1ms` and
  `-1s` all answered 200, so a TTL has no lower bound; one above `max_identity_ttl` is cut to it. A TTL that works is
  limited by the 60 s of leeway.

**What the agent reads once:**
- **retry_join** is read only at start; SIGHUP does not reload it (live run). SIGHUP reloads the log level, the TLS
  certificates, the scheduler workers, some Raft settings and the client's fingerprinters.
- **`retry_interval`** defaults to 30 s and **`retry_max`** to 0, which sets no limit (`DefaultConfig` in
  `command/agent/config.go`, `retryJoiner` in `command/agent/retry_join.go`, v1.11.3, read 2026-10-02).

**The data directory:**
- The agent makes missing directories and never changes the mode of one that exists: a server's `data_dir` and
  `server/` 0755 and its keystore 0700; a client's `client/` 0700, `alloc/` 0711, and
  `<parent of data_dir>/alloc_mounts` 0711 (`/var/lib/alloc_mounts`).
- A `client/` made beforehand with mode 0700 and holding `intro_token.jwt` was left as it was. The agent reads the
  token once at start, and never removes or rewrites it.

**Settings tent turns off:**
- `disable_update_check = true`, a top-level setting, stops the agent's update check with HashiCorp (docs).
- **Consul auto-join.** `consul { server_auto_join, client_auto_join }` default to true (`DefaultConsulConfig` in
  `nomad/structs/config/consul.go`, v2.0.7, read 2026-09-30). With these defaults and no Consul, the agent logs
  Consul discovery errors every few seconds (live run). With both false, a server sets up no Consul bootstrap handler
  (`setupConsulSyncer` in `nomad/server.go`) and a client runs no Consul discovery (`consulDiscovery` in
  `client/client.go`). The agent still asks Consul otherwise: a client's Consul fingerprinter runs every 15 s
  (`client/fingerprint/consul.go`; these three in v1.11.3, read 2026-10-02).

**`nomad config validate <dir>`:**
- It merges a directory as the agent does (`*.hcl` and `*.json`, sorted, subdirectories skipped, a later non-empty
  retry_join replacing an earlier one), refuses unknown keys and runs `IsValidConfig`; it exits with 1 on errors. It
  does not resolve go-sockaddr templates, and missing TLS files only warn, with exit 0. A top-level `server_join {}`
  passes and is ignored (source and live runs, 2026-09-29).
- Zips exist for darwin on amd64 and arm64, linux on amd64 and arm64, and windows on amd64.
- tent's online test ran it on darwin/arm64 on 2026-10-02 with Nomad 2.0.0 and 2.0.7, on each role's directory of
  tent's goldens: each printed `WARNING: Error when parsing TLS configuration: open /etc/nomad.d/tls/ca.pem: no such
  file or directory` and `Configuration is valid!`, and exited with 0. A file with `tent_unknown_key = true` exited
  with 1: `Error loading <dir>/50-unknown.hcl: unexpected keys tent_unknown_key`. With several bad files only the
  first in name order is named (2.0.7, 2026-10-02).

**Seen on Ubuntu** (the M2.6b VM checks, 2026-10-05): Nomad bound to the node's private address at its start, so
the VPC interface was up by `network-online.target` and the go-sockaddr templates resolved; at a shutdown with a job
running Nomad stopped in 1.1 s, before Docker, without a drain; the client was ready and eligible after the reboot.
Nomad did not reattach to its container after `systemctl restart docker` ([6.2](#62-firewalls-on-the-host)).

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
- **Latency ⏳.** In the M1 exit run of 2026-09-28 ([3.16](#316-spike-runs)), each list call that tent made took
  0.9–2 s. A node create makes 7 list calls, one after another, before its POST in a cluster with both firewall
  groups, so it spent 8–10 s before the POST ([architecture §11.3](architecture.md#113-creating-a-node)).
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
  - `InstanceUpdateReq` sends `tags` and `ddos_protection` even when they are unset, as `null`, and leaves out every
    other empty field, `firewall_group_id` included (`omitempty`). What Vultr does with such a PATCH:
    [3.3](#33-instances).
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
  - A new instance was listed by `?tag=` on the first request after the create response (5 of 5 instances on
    2026-09-25, 1 of 1 on 2026-09-27). On 2026-09-29 one of two was listed on the first request, and the other only
    on the second, about 1 s after the create response. So a search by operation tag right after a lost response can
    miss the instance for about a second.
- **`PATCH /v2/instances/{id}`** returns 202 with `job_ids`. It can change:
  - `tags` (replaces the whole set);
  - `user_data`;
  - `firewall_group_id`;
  - `plan`;
  - `attach_vpc` / `detach_vpc`;
  - `label` (govultr and vultr-cli send it).

  `hostname` changes only through a reinstall.

  **Spike 2026-09-27:** a PATCH with `"tags": null` and `"ddos_protection": null`, as govultr's `InstanceUpdateReq`
  sends it, keeps the tags. That held with `user_data` and with `firewall_group_id: ""`, and the user data was
  applied. govultr leaves out an empty `firewall_group_id` ([3.2](#32-govultr-)), so its `Instance.Update` cannot
  detach a firewall group.

  **One PATCH with `tags` and `user_data`** is what `Nodes.MarkJoined` sends: every tag the instance has, with
  `tent/joined=true` added, and the scrub stub. The spike saw `tags` alone and `user_data` with `"tags": null`, never
  both fields in one call. The M2.7b cluster runs of 2026-10-06 (`rugw2m`, `sv3vwb`, `rgfckj`; [3.16](#316-spike-runs))
  verified that Vultr applies both: in each run five of five instances carried `tent/joined=true` and a user data
  that equalled the stub byte for byte. In all three runs Vultr listed the tags sorted (`tent/joined=true` second),
  although `MarkJoined` sends that tag last.
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
  - **Spike 2026-09-28:** a second `halt` of a stopped instance also returned 204, so halting twice is not an error.
  - `DELETE` destroys a running instance immediately.
  - **Stopped instances are billed** until they are destroyed.

### 3.4 user_data, metadata and identity

- **user_data.**
  - Base64-encoded. The size limit is not documented anywhere. **Spike 2026-09-25:**
    - The API has no limit up to **4 MiB**. Both `PATCH` and create accepted 4 MiB and stored it intact.
    - A 65,508-byte user_data (tent's budget then, 64 KiB) works end to end: the metadata service served all of it,
      and a 45 KB `write_files` payload (`encoding: b64`) landed on disk with the right sha256. tent's budget is now
      24 KiB on every provider ([ADR-0027](adr/0027-nodeconfig-contract-rendering-and-spec-hash.md)).
    - Larger payloads were not tested on the instance.
    - tent writes `node.json` with `encoding: gz+b64`, which cloud-init's `write_files` accepts. **VM check
      2026-09-29 (M2.5):** on Ubuntu 24.04 and 26.04 the node.json on the instance had the sha256 of the payload,
      mode 0600 and owner `root:root`.
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
  - **Ordering.** cloud-init runs `runcmd` in `cloud-final.service`, so `tent-node install` runs there. On Ubuntu,
    cloud-init's unit orders `cloud-final.service` after `multi-user.target` (upstream
    `systemd/cloud-final.service.tmpl`), so on a reboot cloud-final waits for `tent-node.service`, which
    `multi-user.target` wants. **VM check 2026-09-29 (M2.5), Ubuntu 24.04 and 26.04:**
    - After the reboot, `After=` of `multi-user.target` listed `tent-node.service`. `tent-node.service` became
      active, then `multi-user.target`, then `cloud-final.service` started: at 25.3 s, 25.8 s and 26.0 s on 24.04,
      and 22.2 s, 23.0 s and 23.0 s on 26.04 (monotonic).
    - `cloud-init status --wait --long` was `done`, with no errors, on both boots.
    - The critical chain of `tent-node.service` passed no `cloud-final`, `cloud-config` or `cloud-init.target`.
    - Both images run cloud-init 26.1, under different unit names. The critical chains pass
      `cloud-init-local.service` and `cloud-init.service` on 24.04, and `cloud-init-main.service`,
      `cloud-init-local.service` and `cloud-init-network.service` on 26.04.
    - **VM check 2026-09-30 (M2.6a)**, with Docker and the CNI plugins: the same order after the reboot, with
      `tent-node.service` active, `multi-user.target` active and `cloud-final.service` started all at 28.3 s on 24.04
      and 29.1 s on 26.04; `cloud-init status` `done` with no errors on both boots.
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
    - **VM check 2026-09-29 (M2.5)**, with tent-node's user data, on Ubuntu 24.04 and 26.04: API `active/running/ok`
      after 52–55 s, SSH login after 102 s, and `cloud-init status: done` at 104–105 s. After a reboot from inside,
      SSH answered on the new boot after 52–53 s. By `status.json`, `up` ran for about 0.3 s on the first boot and
      1.1–1.3 s after the reboot. By the critical chain, `tent-node.service` took 0.3 s and 1.6–1.7 s.
    - **VM check 2026-09-30 (M2.6a)**, with Docker and the CNI plugins, on Ubuntu 24.04 and 26.04: API
      `active/running/ok` after 55 s and 67 s, SSH login after 102 s and 118 s, `cloud-init status: done` at 155 s and
      138 s. On the first boot `up` ran for 50 s and 32.5 s: the Docker install took 43 s and 26 s, the CNI download
      and unpack 4 s. `tent-node.service`'s memory peak on the first boot, apt and dpkg included, was 554 MiB and
      473 MiB. After a reboot SSH answered after 53 s and 52 s, and `up` ran for 6.3 s and 6.8 s, of which the `cni`
      phase's cache hit took 2.9 s and 3.2 s.
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
  - **What tent-node reads** ([ADR-0028](adr/0028-tent-node-agent-units-and-delivery.md)): `instance-v2-id`,
    `regioncode` in lower case, and the IPv4 address of the one interface with `network-type: private`, from one GET
    of `/v1.json` per run. Its tests use a document of the form an instance in `ams` served on 2026-09-25, with
    documentation values in place of its ids and public addresses.
    - **VM check 2026-09-29 (M2.5):** on Ubuntu 24.04 and 26.04 the read succeeded at the first try, on the first
      boot and after a reboot. The read and the rest of `preflight` took 145 ms and 163 ms on 24.04, and 123 ms and
      136 ms on 26.04, under the 1 s that a failed try waits before the next. The instance id, zone and private IP in
      `status.json` matched the API.
    - **VM check 2026-09-30 (M2.6a):** again at the first try, in 164 ms and 204 ms on the first boot and 184 ms
      and 383 ms after the reboot (24.04 and 26.04), now through the marked socket.
  - **Who may ask it:** since M2.6a only tent-node's marked socket
    ([ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md) decision 1).
    - **VM check 2026-09-30 (M2.6a)**, Ubuntu 24.04 and 26.04: root `curl` on the host, a root `busybox:1.38`
      container on the host network and one on Docker's bridge got no answer, each confirmed by the drop counter of
      its chain (output 0, 3, then 6; forward 0, then 3). `tent-node up` by hand exited with 0, every phase
      unchanged, and the output drops stayed at 6 during it.
    - After the reboot both drop counters stayed at 0: nothing asked the service without the mark once `up` had
      loaded the table. cloud-init's Vultr datasource keeps its cached data when the DMI id matches
      (`DataSourceVultr.check_instance_id`, cloud-init source, read 2026-09-29; the images run cloud-init 26.1), and
      cloud-final runs after `up`.
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
- **systemd and time sync on the image, as tent-node uses them.**
  - `systemctl show -p Job --value <unit>` prints the id of the unit's queued or running job, or an empty line when it
    has none, on systemd 255 and 259 (systemd source at v255 and v259, `src/systemctl/systemctl-show.c` and
    `src/shared/bus-print-properties.c`, read 2026-09-30: `-p` shows the property even when it is empty, and
    `--value` prints only the value). `systemctl show` output is stable for programs
    (`docs/PORTABILITY_AND_STABILITY.md`). `runtime` reads it for `docker.service`
    ([ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md) decision 15). **VM check reruns 2026-09-30
    (M2.6a):** exercised on systemd 255 and 259, by the timing: `runtime` waited 2.2 s and 1.5 s after the reboot
    and reported `unchanged` on both, which a failed call or a wrong reading would not have allowed. The reports
    show the waits and the statuses, not the job.
  - A `systemctl start` of a service whose automatic restart waits out `RestartSec` (`SERVICE_AUTO_RESTART`): on
    systemd 255 `service_start` returns `-EAGAIN`, so the start job waits for the automatic restart; on 259 it cuts
    the wait short and starts the service at once (`src/core/service.c` at v255 and v259, read 2026-09-30).
  - **VM check 2026-09-29 (M2.5):** `systemctl is-enabled` of a unit without a file prints `not-found` on stdout and
    nothing on stderr, and exits with 4, on systemd 255 (`255.4-1ubuntu8.17`, Ubuntu 24.04) and 259
    (`259.5-0ubuntu3.4`, Ubuntu 26.04). tent-node's `install` and `verify` read the state from stdout and fail when it
    is empty.
  - **VM check 2026-09-29 (M2.5):** Vultr's Ubuntu 24.04 and 26.04 images both run systemd-timesyncd.
    `/usr/lib/systemd/ntp-units.d/` holds only `80-systemd-timesync.list`, chrony is inactive, and after `up`
    `timedatectl show` gave `CanNTP=yes` and `NTP=yes`. `NTPSynchronized` was `no` at the first check on 24.04 and
    `yes` after the reboot; on 26.04 it was `yes` both times.
    - Ubuntu moved its default to chrony in 25.10, but Vultr's 26.04 image does not use it. The earlier guess here
      that 26.04 runs chrony was wrong for Vultr's image.
    - tent-node turns NTP on through `timedatectl` where it can, and otherwise requires `chrony.service` or
      `systemd-timesyncd.service` to be active, so it also accepts an image that runs chrony.
  - **Reloads of PID 1.** `systemctl enable` without `--no-reload` asks PID 1 to reload by itself (systemctl(1),
    `--no-reload`). The M2.5 VM check did not record the reloads.
    - **VM check 2026-09-30 (M2.6a):** on the first boot PID 1 logged reloads requested by `cloud-final.service` ×2,
      `cloud-init-local.service` ×1 and `tent-node.service` ×4 on 24.04, and by `cloud-init-main.service` ×3 and
      `tent-node.service` ×4 on 26.04. systemd 255 logs them as "Reloading requested from client …", 259 as
      "Reload requested from client …". By their times, three of tent-node.service's came while `hostfirewall` ran
      and one during the Docker install, so docker.io's install reloads PID 1 too; whether through its debhelper
      snippets or systemd's dpkg trigger was not checked.
    - `systemctl status tent-node.service` showed no "changed on disk" warning, and `NeedDaemonReload` was `no`.

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
  - **Spike 2026-09-28:** the first read of `/vpcs`, 31 s after the create call, answered 200 with the address while
    the instance was still `pending`. No 404 was seen, but the first 31 s were not sampled.
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
  On 2026-09-28 the delete succeeded 12 s after the instance was gone, on the second request; the `<IPs>` in the
  refusal were the instance's public address, not its VPC address. On 2026-09-29 it succeeded 21 s and 25 s after the
  instance was gone, each time on the second request, after the same refusal. On 2026-09-30 it took 73 s and
  7 requests, refused with the instance's public address until then, and 4 s and 1 request; in the reruns of that
  day 16 s and 2 requests, and 4 s and 1 request.
- **MTU and interfaces.** MTU is 1450. Private interface names vary (`enp6s0`, `enp7s0`, `enp8s0`, `ens7`), so match
  by MAC.
- **No VPC peering API.** Cross-region connectivity is do-it-yourself, for example with WireGuard.

### 3.6 Firewall groups

- **Rules:**
  - `ip_type`: `v4` or `v6`.
  - `protocol`: ICMP, TCP, UDP, GRE, ESP or AH.
  - `subnet` + `subnet_size`.
  - `port`: a single port or a range `a:b`.
  - `source`: empty, `cloudflare`, or a load balancer id. Vultr lists an empty source as the rule's own subnet (see
    below).
  - `notes`.
- **Accept-only.** Inbound traffic that matches no rule is dropped.
- **Limits.** Each group reports its `max_rule_count` (50 in examples). The number of groups per account is
  undocumented.
- **One group per instance** (`firewall_group_id`), set at creation or with `PATCH`. `PATCH` with
  `firewall_group_id: ""` detaches the group (202).
- **Rule syntax that worked (spike 2026-09-25):** `{ip_type: "v4", protocol: "tcp", subnet: "0.0.0.0",
  subnet_size: 0, port: "22"}`, the same with `v6` and `::`, and `icmp` without a port.
- **How Vultr lists rules (checked 2026-09-27, `hack/vultr-spike` run `aqh7ag`).** Rules sent in tent's form come
  back as sent: `ip_type`, `protocol`, `subnet`, `subnet_size` and `port` keep their values and case, a single port
  stays `22`, `::` stays `::`, and an ICMP rule has the port `""`. tent also reads `22:22`, other cases and
  `0:0:0:0:0:0:0:0` as the same rule. The listing also has these fields, of which govultr's `FirewallRule` reads
  only `source`:
  - `source`: the rule's own subnet when the create had no source, such as `"203.0.113.7/32"` or `"::/0"`. tent
    reads a source equal to the rule's subnet as no source.
  - `type`: the same as `ip_type`.
  - `direction`: `"in"`.
  - `loadbalancer_id`: `""`.
- **A second copy of a rule is refused (checked 2026-09-27):** the same rule sent twice gets
  `400 {"error":"This rule is already defined ","status":400}` the second time, with a trailing space in the text.
  tent counts it as done.
- **Deleting a group that an instance uses (spike 2026-09-27):** `DELETE /v2/firewalls/{id}` answers 204 and
  deletes the group. The instance's `firewall_group_id` reads `""` right after and 15 s later, so the instance has
  no firewall group. tent refuses to delete a group that nodes of the cluster use
  ([architecture §11.5](architecture.md#115-firewall-and-host-firewall)). It still retries an answer that says the
  group is in use (409, 423, or a 4xx whose message says "in use" or "are attached"), though Vultr gave none.
- **`instance_count` (spikes 2026-09-27 and 2026-09-28):** while an instance used the group, neither
  `GET /v2/firewalls/{id}` nor `GET /v2/firewalls` had an `instance_count` (the field was missing, for 32 s after the
  attach). tent does not read it: the inventory counts the cluster's instances in each group itself
  ([architecture §11.1](architecture.md#111-resources)).
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
  - What tent-node does with it: [architecture §11.5](architecture.md#115-firewall-and-host-firewall).
  - **VM check 2026-09-30 (M2.6a)**, Ubuntu 24.04.5 (`os_id` 2284) and 26.04.1 (`os_id` 2760):
    - The images: nftables 1.0.9-1ubuntu0.1 and 1.1.6-1, iptables 1.8.10-3ubuntu2 and 1.8.11-2ubuntu3, no firewalld;
      `nftables.service` disabled; the iptables alternative is iptables-nft; the apt sources list
      `main restricted universe multiverse`.
    - The first boot: `unattended-upgrades.service` started at 26.4 s (24.04) and 24.6 s (26.04) (monotonic), but
      only as the "Unattended Upgrades Shutdown" helper; apt-daily and apt-daily-upgrade did not run, and no install
      command waited for a lock.
    - After `up`: docker.io 29.1.3-0ubuntu3~24.04.2 and 29.1.3-0ubuntu4.1 from the updates pockets, and 65 and 62
      files in `/var/lib/apt/lists`; `ufw.conf` says `ENABLED=no` and the unit is disabled; tent's table `inet tent`,
      with its comment in `nft -j list tables`, next to Docker's `ip filter`, `ip6 filter`, `ip nat` and `ip6 nat`;
      the `ip filter FORWARD` policy is Docker's drop and `ip6 filter FORWARD` accepts; Docker 29.1.3 runs with
      overlayfs, the systemd cgroup driver on cgroup v2, live-restore, json-file logs and the iptables firewall
      backend; `/opt/cni/bin` holds 20 files.
    - After a reboot, as soon as SSH answered (uptime 33 s and 37 s, `up` already done): sshd on 22 (IPv4 and IPv6),
      systemd-resolved on 127.0.0.53 and 127.0.0.54, the DHCP client (systemd-networkd) on udp 68 of the public
      address, and containerd on a random 127.0.0.1 port listened, and nothing else.
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
- **No arm64** Cloud Compute plans. Checked live on 2026-10-05: `GET /v2/plans?type=all&per_page=500`, which answered
  without a key at about 12:30 UTC, returned 151 plans. `type` is `voc` (51), `vx1` (36), `vhp` (16), `vcg` (15), `vc2`
  (12), `vdm` (11) or `vhf` (10); `cpu_vendor` is AMD (97), Intel (53) or empty (1, a `vdm` GPU plan); `vcpu_type` is
  `thread`; `gpu_brand` is `none`, NVIDIA or AMD. No field says arm, a plan has no architecture field, and a
  case-insensitive search of the whole answer for `arm`, `ampere` and `aarch` found nothing.
- **An unauthenticated `GET /v2/plans` failed on 2026-10-05.** From about 18:11 UTC Vultr answered it without a key with
  HTTP 500 `Call to a member function request() on null`, while the same call answered 200 at about 12:30 UTC that
  day. The failure was without a key: the preflight of the M2.7a cluster check read the plan and its price with the
  key at about 21:37 UTC. The spike's preflight sends the key when it has one, and goes on without the plan's price when
  the call fails without a key. On 2026-10-06 the call answered 200 without a key again.
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
- **The longest text stored verbatim (spike 2026-09-27):** 255 characters in a VPC or firewall group
  `description`, 128 in an SSH key `name`. Longer text is accepted (204) and cut to that length without an error.
  The spike set the texts with updates as govultr sends them: `PUT` for a VPC or a firewall group, `PATCH` for an
  SSH key. With `op`, tent's markers for a 20-character cluster name reach 99 characters on a firewall group, 98 on
  an SSH key and 82 on a VPC, so they fit.
- **A second SSH key with the same key material is accepted (spike 2026-09-27)** under another name, and the
  account then lists both. So two clusters can use the same operator key, and so can a cluster and a key that an
  operator uploaded by hand.

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
  - **A deleted instance still counts for a short time.** Run `rugw2m` (2026-10-06, 5 instances before the delete):
    the create that tent sent after the delete of an instance, in the same `update --yes`, failed with `400 Bad
    Request: Server add failed: You have reached the maximum number of active instances for this account.` The report
    gives no time between the two calls: the whole run took 11 s, and the plan alone had taken 5 s, so the create came
    less than 10 s after the delete. In run `sv3vwb` the replacement was created 51 s after the delete of the machine
    it replaced, where an ordinary create of that run took 50 s, so the limit held that create back for a few seconds
    at most. Neither run measured how long Vultr counts a deleted instance.
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

### 3.16 Spike runs

All runs: region `ams`, plan `vc2-1c-1gb`, Ubuntu 24.04 (`os_id` 2284), except one of the M2.5 VM checks, one M2.6a
VM check and its rerun, and one M2.6b VM check and its rerun, which ran Ubuntu 26.04 (`os_id` 2760). Runs 1 to 3 ran
on 2026-09-25, run 4 on 2026-09-27, run 5 on 2026-09-28, the M2.5 VM checks on 2026-09-29, the M2.6a VM checks and
their reruns on 2026-09-30, the M2.6b VM checks, their reruns and the M2.7a cluster check on 2026-10-05, and the
three M2.7b cluster runs below, all on 2026-10-06. The M2.8 cluster check is pending.
Reports are in `hack/vultr-spike/results/` (git-ignored). The M1 exit run below was not a spike run.

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

**Run 4 (`aqh7ag`, spike v3, `--only sshdup,lengths,rules,fwinuse,patchtags`)** checked the facts that
[ADR-0023](adr/0023-vultr-inventory-dedupe-and-images.md) and the node primitives rely on, with one instance. It
verified:
- that Vultr accepts a second SSH key with the same key material ([3.10](#310-ownership-fields-on-other-resources));
- the longest description and SSH key name stored verbatim, and that longer text is cut without an error;
- how Vultr lists firewall rules, and that it refuses a second copy of a rule ([3.6](#36-firewall-groups));
- that Vultr deletes a firewall group that an instance uses; `GET /v2/firewalls/{id}` of that group had no
  `instance_count`;
- that the instance PATCH govultr sends keeps the tags and applies the user data ([3.3](#33-instances));
- again, that a new instance is listable by tag at once.

**Run 5 (`ekrnuo`, spike v4, `--only vpcpending,fwinuse,halttwice`)** ran one instance in a firewall group with no
rules, since none of these checks needs SSH. It verified:
- that an instance can be created with a firewall group, which applies from its first boot;
- that `/vpcs` answered 200 with the address at the first read, 31 s after the create call ([3.5](#35-vpc));
- that neither firewall group endpoint reports `instance_count` ([3.6](#36-firewall-groups));
- that a second `halt` of a stopped instance answers 204 ([3.3](#33-instances)).

**M1 exit run (2026-09-28)** was tent itself against the API, in `ams` with `vc2-1c-1gb`: a cluster with one server
and one worker, then two workers.
- `tent create cluster … --allow-single-server --yes` built the VPC, both firewall groups and two nodes, whose VPC
  addresses were `10.64.0.3` and `10.64.0.4`. `tent update cluster --exit-code --allow-single-server` then printed
  `No changes.` and exited with 0, so the firewall rules read back from Vultr without a diff.
- With the workers raised to 2, an `update --yes` was interrupted with SIGINT 20 s after `creating node …-workers-1`:
  after the POST, while tent waited for the node to be ready. The next `update --yes` planned
  `~ node …-workers-1 (ID …, wait until it is ready)` (the line of that tent; since M2.7b it reads `wait until it
  joins, scrub its user data`) and created no instance, and a further `--exit-code` run had no
  changes. Vultr listed exactly three instances.
- `tent delete cluster --yes` deleted the three nodes, both firewall groups, the VPC and the four state objects. The
  API then listed no instance, VPC, firewall group or SSH key with the cluster's marker.
- Each list call took 0.9–2 s ([3.1](#31-api-basics-and-access-control)).

**M2.5 VM checks (`ppssbr` and `yccsoc`, spike v5, `--only tentnode`, 2026-09-29)** each booted one instance from the
user data of `hack/tent-node-userdata`: `ppssbr` on Ubuntu 24.04 (`os_id` 2284), `yccsoc` on Ubuntu 26.04
(`os_id` 2760). Both ran one development build of tent-node, `v0.1.0-rc.2-18-g4fc8be6-dirty`, which `make dev-upload`
put into the CI R2 bucket; the object was deleted afterwards. Both passed. They verified:
- that node.json arrives intact from the gz+b64 user data ([3.4](#34-user_data-metadata-and-identity));
- that `up` ran on the first boot, and after a reboot changed nothing but `status.json`;
- the order of `tent-node.service`, `multi-user.target` and cloud-final after the reboot, and that cloud-init finished
  with no errors on both boots;
- that the metadata read succeeded at the first try on both boots;
- what `systemctl is-enabled` prints for a unit without a file on systemd 255 and 259;
- that both images run systemd-timesyncd, not chrony;
- that a new instance can need a second request, about 1 s after the create response, to be listed by tag
  ([3.3](#33-instances));
- that the VPC delete succeeded 21–25 s after the instances were gone ([3.5](#35-vpc)).

**M2.6a VM checks (`3ornct` and `yqjlcb`, spike v6, `--only tentnode`, 2026-09-30)** each booted one client instance
from the user data of `hack/tent-node-userdata`: `3ornct` on Ubuntu 24.04.5 (`os_id` 2284, systemd 255.4-1ubuntu8.17),
`yqjlcb` on Ubuntu 26.04.1 (`os_id` 2760, systemd 259.5-0ubuntu3.4). Both ran one development build of tent-node,
`v0.1.0-rc.2-30-ga41d6e1`. Both passed but for one finding. They verified:
- the image's packages and firewalls, and that no apt timer held a lock on the first boot ([3.6](#36-firewall-groups));
- ufw off, tent's table next to Docker's tables, Docker with `daemon.json`, and the CNI plugins in `/opt/cni/bin`;
- that root `curl`, a root container on the host network and one on Docker's bridge get no answer from the metadata
  service, while tent-node's marked read does, and that nothing asked it unmarked after the reboot
  ([3.4](#34-user_data-metadata-and-identity));
- PID 1's reloads during `install` and `up`, with no "changed on disk" warning;
- after the reboot: tent's table loaded again with the same comment, only `status.json` changed among tent-node's
  files, cloud-init done with no errors, the boot order as designed, and only sshd, systemd-resolved, the DHCP client
  and containerd on loopback listening when SSH answered, after `up` had run;
- that a new instance was listed by tag at the first request, and that the VPC delete took 73 s and 7 requests, then
  4 s and 1 request, after the instance was gone ([3.5](#35-vpc)).

The finding: after the reboot on 24.04 `runtime` reported `done`. By the timing, `docker.service` was still inactive
with its start job queued, and `systemctl start` waited 2 s for it. On 26.04 `runtime` reported `unchanged`; it took
1.9 s, so Docker was most likely still activating, which the old code counted as no change; with Docker active,
`runtime` takes about 30 ms (`up` by hand). The fix followed: `runtime` now counts a pending job of `docker.service`
as Docker starting on its own ([ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md) decision 15). The
reruns below confirmed it.

**M2.6a VM reruns (`7lhvvv` and `jcirel`, spike v6, `--only tentnode`, 2026-09-30)** ran the check again with the
fix: `7lhvvv` on Ubuntu 24.04 (`os_id` 2284), `jcirel` on Ubuntu 26.04 (`os_id` 2760), both with one development
build of tent-node, `v0.1.0-rc.2-30-ga41d6e1-dirty`, and the spike's parser for systemd 259's reload messages. Both
passed, with no failed, unexpected, missing or unknown row. They verified:
- the fix on a real VM: after the reboot `runtime` waited about 2.2 s for Docker on 24.04, as in the first run, and
  reported `unchanged`; on 26.04 it reported `unchanged` after 1.5 s; only `hostfirewall` was `done` on both;
- the reloads of the first boot, now parsed on both images, with the same counts as the first runs
  ([3.4](#34-user_data-metadata-and-identity));
- everything else as in the first runs: the metadata probes blocked with their counters, `up` by hand unchanged,
  tent's table back after the reboot with the same comment, and only `status.json` changed.

**M2.6b VM checks (`bxkllh` and `g2rl9d`, spike v7, `--only tentnode`, 2026-10-05)** each booted the combined node
of a cluster of one node from the user data of `hack/tent-node-userdata` ([ADR-0030](adr/0030-nomad-on-nodes.md)):
`bxkllh` on Ubuntu 24.04.5 (`os_id` 2284, systemd 255.4, docker.io 29.1.3-0ubuntu3~24.04.2), `g2rl9d` on Ubuntu
26.04.1 (`os_id` 2760, systemd 259.5, docker.io 29.1.3-0ubuntu4.1). Both ran one development build of tent-node,
`v0.1.0-rc.2-30-g3b219a6-dirty`, with Nomad 2.0.7; the user data took 6111 and 5959 bytes. Every row was as expected
on both images but one, the restart of Docker (below). They verified:
- **Boot.** cloud-init finished 248 s and 137 s after the create call. `tent-node.service` was active 92.0 s and
  83.9 s after boot, and `nomad.service` 91.9 s and 83.8 s. `systemctl start nomad.service` returned 1.0 to 1.4 s
  after tent-node logged `start Nomad`, on the first boot and after the reboot (by the timing of the log lines), well
  within systemd's start timeout of 90 s. After the reboot `nomad.service` was active 38.2 s and 35.4 s after boot,
  and started after `hostfirewall` had logged its result (27.5 s, then 37.2 s; 23.5 s, then 34.2 s, monotonic).
- **Cost on the node.** Over the first boot the cgroup of `tent-node.service` peaked at 519.4 MB and 436.7 MB, apt's
  install and the page cache of what `up` wrote included (systemd's memory peak). With the zip in the cache, the
  `nomad` phase took about 3 to 4 s before `start Nomad` (by the timing).
- **Registration.** The first `Node.Register` failed with `failed to sign node identity claims: keyring has not been
  initialized yet` and succeeded 24 s and 21 s later. After `systemctl restart nomad.service`, `Node.Register` and
  `Node.GetClientAllocs` failed with `Permission denied` and succeeded 23 s and 20 s later; on 24.04 the keyring
  replicator first logged `failed to fetch key from any peer`. After the reboot the client registered at once.
  `verify` passed every time: a combined node's client knows its own server before it has registered.
- **Nomad.** `nomad acl bootstrap` exited with 0, and the token stayed in a root-only file. The node led its cluster
  of one in the region `global` with the datacenter `ams`, and `nomad node status` showed it ready and eligible in the
  pool `default`, with the meta `tent_instance_id`, `tent_cluster` and `tent_nodegroup`. With `enforcement = warn` the
  server logged `node registration without introduction token`.
- **A job** (`busybox:1.38`'s httpd in bridge mode with a dynamic port): its port answered on the private address,
  and from its container the metadata service timed out while the drop counter of tent's forward chain grew.
- **A restart of Nomad with the job running** returned after 1.5 s and 1.7 s. The container kept its id and start
  time, the task was not restarted, the node stayed ready and eligible, no line named a drain, and the port answered.
  Nomad logged nothing at INFO about restoring or reattaching.
- **`refresh-join`.** Its first run, right after `tent-node.service` finished, asked the node's own agent over mTLS
  (`server.global.nomad`) and rewrote `05-join.hcl` to `10.64.0.3:4648` (`known=0 servers=1`); later runs changed
  nothing, and `peers.json` held `["10.64.0.3"]`.
- **The reboot.** `hostfirewall` and `nomad` were done, every other phase unchanged, and only `status.json` changed
  among tent-node's files. `join` logged `a server did not answer` for the node's own address (connection refused),
  since Nomad was not running yet. The node came back ready and eligible. The first boot's allocation ran again: the
  client restarted its task after `Exit Code: 255`, with the restart policy's delay of 17.3 s and 15.6 s. When SSH
  answered, `up` had finished on 24.04 and Nomad listened; on 26.04 `up` still ran, and only sshd,
  systemd-resolved, the DHCP client and containerd listened.
- **The shutdown** with the job running: Nomad's stop took 1.1 s on both and ended before Docker's stop began, and no
  line named a drain. systemd logged `Unit process … (nomad) remains running after unit stopped` for two processes,
  which `KillMode=process` leaves to the tasks. Docker's stop took about 5 s.
- **docker.io** came from `noble-updates/universe` and `noble-security/universe` on 24.04, and from
  `resolute-updates/universe` and `resolute-security/universe` on 26.04 (`apt-cache policy`).
- Preflight's metadata read took 256 ms and 148 ms on the first boot, 255 ms and 379 ms after the reboot. The VPC
  delete succeeded 12 s and 3 requests after the instance was gone, on both.

The finding: **a restart of Docker restarted the job's task** on both images. `tent-node up` with a changed
`daemon.json` restarted `docker.service` (`runtime` done, every other phase unchanged, Nomad not restarted). Nomad's
docker driver logged `failed to wait for container; already terminated` and `log streaming ended with error:
unexpected EOF`; the task ended `Terminated` with `Exit Code: 0, Exit Message: "unexpected EOF"`, and the client
started it again in a new container after the restart policy's delay of 17.3 s and 18.7 s. So Nomad 2.0.7 did not
reattach to its container after a restart of docker.io 29.1.3 with live-restore. What tent does about it: see
[ADR-0030](adr/0030-nomad-on-nodes.md)'s follow-ups.

**M2.6b VM reruns (`bqpw7p` and `xskxx4`, spike v8, `--only tentnode`, 2026-10-05)** ran spike v8 on Ubuntu 24.04
(`bqpw7p`) and 26.04 (`xskxx4`) with a rebuild of the same version label (sha256
`ed76e2b8c84d74ffce336a6df185c3d1a27fb8858d3a1b222b97c5e7fb35910e`; the v7 runs had
`eed8a97a23d5f7f4801fab7b347e4dc4f6f61c2614fbe8569dd72d3bca101a40`), and passed every row on both. Besides what the
v7 runs showed, they verified:
- **A restart of containerd with the job running** (`systemctl restart containerd.service`) kept the task: the same
  container id and start time, no task restart, watched 62 s and 63 s after containerd was active again. Neither
  `docker.service` nor `nomad.service` became active again. So an upgrade of containerd does not touch Nomad's
  docker tasks ([6.4](#64-restarts-of-docker-containerd-and-nomad)).
- **A restart of Docker** replaced the task again, as accepted: it ran again 17 s and 20 s after `docker.service` was
  active again.
- **Records.** needrestart is installed on Vultr's images (3.6-7ubuntu4.5 on 24.04, 3.11-1ubuntu2 on 26.04), with no
  `$nrconf{restart}` line, the package's default. `/etc/apt/apt.conf.d/20auto-upgrades` sets
  `APT::Periodic::Update-Package-Lists "1"` and `APT::Periodic::Unattended-Upgrade "1"` on both. `debconf-show
  docker.io` gives `docker.io/restart: false` on both. `docker.service` has `Wants=` and `After=` on
  `containerd.service`, `Requires=docker.socket`, no `BindsTo=` or `PartOf=`, and `Restart=always`.
- Everything else as on the v7 runs: a restart of Nomad kept the task, the node was eligible after the reboot, and
  Nomad stopped in 1.1 s, before Docker.

**M2.7a cluster check (`qypvsk`, spike v9, `--only cluster`, 2026-10-05)** ran tent itself on Ubuntu 24.04, `ams`,
`vc2-1c-1gb`, with tent `v0.1.0-rc.2-43-g58a50ff-dirty` and a development tent-node, and passed every row. It built a
cluster of 3 servers and 2 clients, five instances at once, from an empty account, with `tent create cluster --yes`:
- **The build.** The command exited 0 after 453 s. The progress lines showed a Nomad leader, the bootstrap and healthy
  servers all at 190 s, and the clients registered 126 s and 263 s after the leader line (each client's create
  included). The bootstrap and the health wait with three voters answered at once, as on the local run
  ([1.6](#16-the-agent-on-a-node)). The longest wait of the Nomad step stayed far under its 10 minutes.
- **The Nomad API,** through the certificate and token of `hack/tent-operator`, the stopgap that `tent export nomad` has
  since replaced (TLS name `server.global.nomad`): 3 alive
  servers, autopilot healthy with 3 voters, 2 ready and eligible clients under their instance labels. A docker job in
  bridge mode ran on a client after 5 s and was purged.
- **A second run.** `update cluster --exit-code` exited 0 and `update cluster --yes` printed `cluster … is up to
  date`, each in 4 s. All 5 instances carry a `tent/spec-hash` tag, 2 distinct values (servers and clients).
- **User data.** All 5 instances still held `/etc/tent/node.json`: M2.7a scrubbed nothing.
- **A client.** `status.json` had `preflight` and `verify` unchanged and every other phase done; `05-join.hcl` listed
  the 3 servers on port 4647. The intro token file was 787 bytes, mode 0600, `root:root`, for the node name
  `spk-qypvsk-workers-0` (the plan's size check assumes up to 2048). The size matches the estimate of 4/3 bytes per name
  character, twice, from the 739 bytes for a two-character name ([1.6](#16-the-agent-on-a-node)).
- **Decision 26 on a real server.** `systemctl restart nomad` exited 0 and the unit was active again at once (`Result`
  `success`, `NRestarts` 0). Its journal had one `Failed with result 'exit-code'` line, the stop ending with status 1.
  Autopilot was healthy with 3 voters 7 s after the restart.
- **The delete.** `tent delete cluster --yes` exited 0 after 23 s; the VPC delete was retried four times, about 10 s,
  while Vultr still listed the instances as attached. Afterwards no instance, VPC, firewall group or SSH key of the
  cluster was left, and the state store was empty. None of the CA key, gossip key, ACL bootstrap token and operator
  key appeared in tent's output or the report.

**M2.7b cluster check, first run (`rugw2m`, spike v10, `--only cluster --unregistered`, 2026-10-06)** ran tent
`v0.1.0-rc.2-53-g638ebd9-dirty` with a development tent-node on Ubuntu 24.04, `ams`, `vc2-1c-1gb`. It built the cluster
of the M2.7a check, 3 servers and 2 clients, and the replacement of a client then failed (below). The three rows
after it, the replacement, its node and the final `update --exit-code`, did not run. It verified:
- **The build.** `tent create cluster --yes` exited 0 after 543 s. The leader, the ACL bootstrap and the healthy
  servers showed at 268 s, and the clients registered 142 s and 275 s after the leader line. Five `scrubbed the user
  data of node …` lines followed, a server's after the healthy servers and a client's after its registration.
- **The scrub.** Five of five instances carried `tent/joined=true` beside `tent/spec-hash`, and their user data, read
  from `GET /v2/instances/{id}/user-data`, equalled tent's stub byte for byte and held no `/etc/tent/node.json`. So
  one PATCH set the tags and the user data ([3.3](#33-instances)). `update --exit-code` exited 0 and `update --yes`
  printed `is up to date`, each in 4 s.
- **A reboot of a client** left `/etc/tent/node.json` with its sha256. SSH answered after 53 s and Nomad listed the
  node ready and eligible 54 s after the reboot.
- **The delete guard.** With one client in the specs, `update` and `update --yes` each exited 1 in 5 s and 4 s with
  `update would delete a node that joined Nomad: …-workers-1`, and 5 instances stayed. With the specs restored, the
  plan was empty.
- **A client that never registered.** `systemctl stop nomad.service` left the unit `inactive`, and it was still
  inactive 1711 s later, so nothing starts Nomad again. The purge of its node answered HTTP 200. At an age of 1946 s
  the plan was a delete as `not registered` and a create of the same name, exit 0 in 5 s.
- **The delete.** `tent delete cluster --yes` exited 0 after 19 s. The VPC delete was retried three times (0.9 s,
  1.6 s, 3.3 s) while Vultr still said that servers were attached. Nothing was left in the Vultr API or the state
  store, and no secret was in the output.

It found two things, both fixed:
- **cloud-init read degraded after the reboot.** `cloud-init status --wait --long` printed `status: done`,
  `extended_status: degraded done` and exit 2, with the recoverable error `Failed at merging in cloud config part
  from part-001: empty cloud config`. The stub was a header and a comment ([6.5](#65-cloud-init-and-the-stub)). The
  stub got a line `{}`. The row had called this as expected, since it read only `errors: []`.
- **The replacement failed at the account's limit** ([3.14](#314-account-limits-terms-and-operations-)): `update
  --yes` exited 1 after 11 s. The Vultr provider now sends a node create again that the instance limit refuses
  within 2 minutes of its own delete.

**M2.7b cluster check, second run (`sv3vwb`, spike v11, `--only cluster --unregistered`, 2026-10-06)** ran tent
`v0.1.0-rc.2-54-g7d1c6fd-dirty` the same way, with the two fixes in the code. Spike v11 changed three things against
v10: the stub's bytes, the cloud-init verdict of the reboot row (it needs `status: done`, exit 0 and no recoverable
error), and the row of the replacement, which also gives the seconds between the delete and the create and the time
that a client's create took in the build. It verified:
- **The build.** `tent create cluster --yes` exited 0 after 484 s. The leader, the bootstrap and the healthy servers
  showed at 212 s, the clients registered 140 s and 271 s after the leader line, and five of five instances carried
  `tent/joined=true` and the stub of three lines. `update --exit-code` exited 0 in 6 s and `update --yes` printed `is
  up to date` in 3 s. Three voters were back 1 s after a server's restart.
- **cloud-init after the reboot was healthy:** `status: done`, `extended_status: done`, `errors: []`,
  `recoverable_errors: {}`, exit 0 ([6.5](#65-cloud-init-and-the-stub)). The row itself read `UNEXPECTED: cloud-init
  status ? (errors [])`: `cloud-init status --wait` printed `..status: done`, and the script read the status only at
  the start of a line. The script is fixed, and the third run shows the row as expected; the details of this report
  hold the healthy status.
- **The delete guard** exited 1 in 5 s and 4 s with 5 instances left, and `--exit-code` after the specs were
  restored exited 0 in 4 s.
- **A client that never registered.** Nomad was inactive after the stop and still inactive 1711 s later. At an age of
  1944 s the plan was the delete as `not registered` and the create, exit 0 in 6 s. `tent update cluster --yes` then
  exited 0 in 142 s: it deleted, created, registered and scrubbed the client. The new machine took the node's name
  and Nomad listed it ready and eligible at 10.64.0.7. `update --exit-code` after it exited 0 in 7 s.
- **Vultr gave the replacement the private address of the machine it replaced:** 10.64.0.7 for the new instance
  `222dbfa9-…` as for the old `4b4c83f4-…`.
- **The delete.** `tent delete cluster --yes` exited 0 after 18 s and left nothing; the state store was empty; no
  secret was in the output.

The report does not say whether Vultr refused the replacement's create: the provider logs a refusal at info level, and
the run was made without `-v`. `created node` came 51 s after `deleted node`, against 50 s for a client's create in the
build, so a retry cost a few seconds at most ([3.14](#314-account-limits-terms-and-operations-)). Unit tests cover the
retry.

**M2.7b cluster check, third run (`rgfckj`, spike v11, `--only cluster`, 2026-10-06)** ran tent
`v0.1.0-rc.2-54-g17bc737-dirty` with the fixed script and without `--unregistered`. Every row read as expected, and
the client check was skipped. `tent create cluster --yes` exited 0 after 429 s; the leader and the bootstrap showed at
185 s and the healthy servers at 188 s; the clients registered 124 s and 243 s after the leader line; five of five
instances carried `tent/joined=true` and the stub. After the reboot of a client SSH answered after 53 s, Nomad listed
the node ready and eligible 54 s after the reboot, and the row read `cloud-init status done, exit 0, no recoverable
error` (`status: done` without dots, `extended_status: done`, `recoverable_errors: {}`, exit 0). The delete guard
exited 1 in 7 s and 4 s with 5 instances left. `tent delete cluster --yes` exited 0 after 20 s and left nothing.

**M2.8 cluster check (`9pxbqn`, spike v12, `--only cluster --keep`, 2026-10-07)** ran tent
`v0.1.0-rc.2-67-g57d69b6-dirty`. Every row read as expected but one, where the script was wrong. `tent create cluster
--yes` exited 0 after 490 s; the leader, the bootstrap and the healthy servers showed at 208 s; the clients registered
138 s and 281 s after the leader line. The new rows:

- **`tent export nomad`** exited 0 in 1 s. The directory had mode 700 and the four files 600; stdout was the six lines
  with a server's public address; the token was not the bootstrap token and was in neither stream; Nomad called it a
  management token named `tent export nomad ...` that ended 24 hours after the export; `cli.pem` was for
  `cli.global.nomad`, for client authentication only, and ended with the token. With the exported files curl read
  three alive members, a healthy autopilot report with three voters and two ready clients, and the docker job ran
  after 6 s.
- **`nomad server members`** with the six lines sourced listed the three servers as alive. The row read `UNEXPECTED:
  3 alive of 4`: the script read stdout and stderr together and counted the CLI's hint
  ([1.2](#12-features-tent-relies-on)) as a fourth server. The script now reads the table from stdout alone.
- **`tent validate cluster`** exited 0 in 2 s with `cluster spk-9pxbqn is valid: 3 servers and 2 clients run Nomad
  2.0.7` and the warnings about the open `spec.access.api` and the one failure domain.
- **`tent ui --listen 127.0.0.1:0`** answered `/v1/status/leader` and `/ui/` with 200, used a management token named
  `tent ui ...` that was not the bootstrap token, refused `Host: example.com` with 403, ended with exit 0 one second
  after SIGINT, and its port then refused connections.
- **A client whose Nomad stops.** After `systemctl stop nomad.service` on `spk-9pxbqn-workers-1` Nomad listed the node
  down after 21 s. `tent validate cluster` then exited 2 in 3 s and named the node with `its Nomad client is down`.
  After `systemctl start nomad.service`, `tent validate cluster --wait 5m` exited 0 in 2 s.

The rows of M2.7a and M2.7b read as before: after the reboot of a client SSH answered after 53 s and Nomad listed the
node ready and eligible after 54 s, and the delete guard exited 1 twice with five instances left. None of the six
secrets that the run read (the CA key, the gossip key, the bootstrap token, the operator's key and token, the token of
`tent ui`) was in its output. The delete rows did not run, since `--keep` left the cluster for the steps by hand; the
maintainer deleted it with `tent delete cluster --yes` after them.

**By hand against that cluster, through `tent ui`, the same day:**

- The maintainer opened the UI in a browser and it worked, an exec into a task and the Logs tab included. The first
  try showed nothing: `tent ui` ran on another machine over SSH, and its port is a loopback port of the machine that
  runs tent.
- Headless Chrome 154 loaded `/ui/` through the proxy, and the proxy's log at `-v` showed 200 or 204 for every request
  it made. A request with a `Host` of another port got 403 `forbidden: unexpected Host`, as an SSH tunnel on another
  local port would.
- With `NOMAD_ADDR` set to the proxy's address and no other variable, the `nomad` CLI 2.0.7 ran the job of the
  quickstart (`nomad job run`: the deployment was successful and the allocation ran on a client), `nomad alloc exec`
  into its task (the proxy logged the upgrade with status 101), `nomad alloc logs` and `nomad job stop -purge`.
  `tent ui` exited with 0 on SIGINT each time.

**Still open:**
- Object Storage conditional writes ([3.12](#312-object-storage-)).
- Images other than Ubuntu 24.04 were not checked, except Ubuntu 26.04 by one M2.5 VM check, one M2.6a VM check and
  its rerun, and one M2.6b VM check and its rerun, for tent-node only.
- Account limits beyond 5 concurrent instances were not tested (the M2.7a check and the three M2.7b runs ran 5).
- Whether the Nomad UI works against a cluster that tent built: the by-hand step of the M2.8 check.
- What `/vpcs` answers in the first 30 s after a create ([3.5](#35-vpc)).

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

## 6. Ubuntu on nodes

What tent-node's `hostfirewall` and `runtime` phases rely on in Ubuntu 24.04 and 26.04
([ADR-0029](adr/0029-host-firewall-runtime-and-cni-on-nodes.md)). Read on 2026-09-29 in the packaging source,
Launchpad, packages.ubuntu.com and the cloud image manifests, unless an item says otherwise. What Vultr's images hold
is in [3.6](#36-firewall-groups).

### 6.1 Packages ⏳

- **docker.io** is built from the source package `docker.io-app`.
  - 24.04 (noble-updates and -security): 29.1.3-0ubuntu3~24.04.2 (2026-05-05), with containerd 2.2.1 and runc 1.3.4.
    The release pocket still has 24.0.7.
  - 26.04 (resolute-updates): 29.1.3-0ubuntu4.1, with containerd 2.2.2 and runc 1.4.0.
  - It is in `universe`, which Vultr's 24.04 and 26.04 images enable (VM check 2026-09-30,
    [3.6](#36-firewall-groups)).
  - It depends on `iptables` and `libnftables1`, and recommends git, ubuntu-fan, pigz, xz-utils, apparmor and
    ca-certificates, which `--no-install-recommends` leaves out. Without pigz Docker unpacks layers with Go's gzip,
    which is slower.
  - Its unit (`debian/docker.io.docker.service` in the noble-updates branch of the Launchpad git, read 2026-09-29)
    has `Type=notify`, `Requires=docker.socket`, `After=network-online.target firewalld.service containerd.service`
    and `TimeoutSec=0`, so neither its start nor its stop times out. moby's own
    `contrib/init/systemd/docker.service` (docker-v29.1.3) has `TimeoutStartSec=0`.
  - Its postinst enables and starts Docker on the first install (`invoke-rc.d docker start` when Docker is not
    running). It ships no files in `/etc/docker`, so a `daemon.json` written before the install is read by the first
    start. An upgrade does not restart Docker (the debconf default is false).
- **nftables and iptables** are in both cloud images: nftables 1.0.9 on 24.04 and 1.1.6 on 26.04 (manifests of
  2026-09-26 and 2026-09-29). iptables-nft is the default alternative (the iptables postinst gives nft the priority 20
  and legacy 10).
- **firewalld** is in neither manifest, and Vultr's 24.04 image lacked it (spike 2026-09-25).

### 6.2 Firewalls on the host

- **Docker 29** keeps iptables as its firewall backend. Its nftables backend is experimental and only with
  `"firewall-backend": "nftables"` (Docker 29 release notes; `selectFirewallBackend` in moby docker-v29.1.3).
  - When Docker turns `ip_forward` on itself, it sets the policy of the `ip` and `ip6` `filter FORWARD` chains to drop;
    `"ip-forward-no-drop": true` prevents that (Docker docs, packet filtering and firewalls). It may also set the
    drop when `ip_forward` was already 1 (unverified).
  - `log-driver` and `log-opts` are not among the settings that Docker reloads on SIGHUP, so a change needs a restart.
    `live-restore` keeps the containers of a daemon that started with it running while it restarts (dockerd
    reference). Neither Docker's nor Nomad's docs mention a conflict between live-restore and Nomad (read
    2026-09-29). Nomad 2.0.7 did not reattach after a restart of docker.io 29.1.3 with live-restore on the M2.6b VM
    checks (2026-10-05, runs `bxkllh` and `g2rl9d`): the task ended with `unexpected EOF` and the client started it
    again in a new container ([3.16](#316-spike-runs)); why: [6.4](#64-restarts-of-docker-containerd-and-nomad).
- **nftables** (nftables wiki, "Configuring chains"):
  - A drop in any base chain is final; an accept is not.
  - `nft -f` applies a file as one transaction. `table inet tent`, `delete table inet tent`, `table inet tent { … }`
    replaces one table atomically without `destroy table`, which needs kernel 6.3 or later.
  - `nftables.service` is not enabled (`--no-enable --no-start`), and the default `/etc/nftables.conf` starts with
    `flush ruleset`, which would wipe Docker's and the CNI plugins' tables.
  - JSON: every `nft -j list` puts a `metainfo` object first in the `nftables` array (`src/json.c` of 1.0.9), so
    `nft -j list tables` on an empty ruleset prints only that object and exits with 0. 1.0.9 and 1.1.6 print a table's
    `comment`, on Vultr's images too (VM check 2026-09-30); Debian's 1.0.6 does not (the container runs in the next
    item, and `table_print_json` in Ubuntu's 1.0.9 `src/json.c`).
  - tent's rulesets for the three roles loaded on real nft 1.0.9 and 1.1.6 in containers on 2026-09-29, next to an
    `ip filter` table as Docker leaves it, and loaded again over each other. nft refuses a set whose elements overlap,
    and lists `ip saddr { 0.0.0.0/0 }` as `ip saddr 0.0.0.0/0`. IPv6 neighbour discovery is untracked, not invalid,
    so an input chain that drops by default must accept it by type.
  - **`socket cgroupv2 level N "<path>"`** (nftables 1.0.9 `src/datatype.c`, Linux 6.8 `net/netfilter/nft_socket.c`):
    nft resolves the path to the cgroup's id when it parses the rule. A missing path fails the whole batch
    (`Error: cgroupv2 path fails: No such file or directory`), and a cgroup removed and made again gets a new id, which
    the rule silently stops matching. It needs kernel 5.13 or later, and on 6.8 works only in prerouting, input and
    output. Ancestors match by level, and the first SYN matches.
- **`SO_MARK`** needs CAP_NET_ADMIN, or CAP_NET_RAW since Linux 5.17 (`sk_setsockopt` in v6.8 `net/core/sock.c`;
  checked on a 6.10 kernel on 2026-09-29).
- **ufw** (Ubuntu's ufw packaging source, ufw 0.36.2: 0.36.2-6 in 24.04 and 0.36.2-9build1 in 26.04, read
  2026-09-29):
  - `ufw disable` always runs `ufw-init force-stop`: it writes `ENABLED=no` into `/etc/ufw/ufw.conf`, deletes ufw's
    chains and sets the policies of the `ip` and `ip6` `filter` chains `INPUT`, `OUTPUT` and `FORWARD` to accept.
    After Docker started, that undoes Docker's drop in `FORWARD`.
  - At boot `ufw-init start` does nothing while `ENABLED=no`. `systemctl disable --now ufw` alone removes the rules
    but leaves `ENABLED=yes`. Package upgrades never restart ufw.
  - Removing the package takes the program and the unit and leaves `ufw.conf`, a configuration file.

### 6.3 apt and dpkg

Read in the source of apt 2.8.3 and 3.2.0 and of dpkg 1.22, on 2026-09-29.

- **Locks.**
  - dpkg takes its frontend and database locks without waiting (`lib/dpkg/dbmodify.c`).
  - `apt-get update` never waits for the lock of the package lists (`pkgAcquire::GetLock` has no timeout).
  - `apt-get install` takes `/var/cache/apt/archives/lock` without waiting, even with `-o DPkg::Lock::Timeout=<s>`,
    which makes it wait for dpkg's lock only (`private-install.cc`, `acquire.cc`).
  - So while unattended-upgrades installs, `apt-get install` fails on `/var/lib/dpkg/lock-frontend` with exit status
    100, and while apt-daily downloads, on the lock of the archives.
  - The errors: `apt-get update` prints `E: Could not get lock /var/lib/apt/lists/lock. It is held by process <pid>
    (apt-get)` and exits with 100 (apt 2.8), and dpkg prints `dpkg: error: dpkg frontend lock was locked by another
    process with pid <pid>` and exits with 2 (dpkg 1.22.6). 26.04's dpkg may word it differently (unverified).
- **Package lists.** With `package_update: false` cloud-init does not refresh the package lists, so the image's lists
  may be stale or missing (unverified).
- **`dpkg-query -W -f='${Status}' <package>`** prints three words: the selection, a flag and the state, such as
  `install ok installed`, `hold ok installed` or `deinstall ok installed`. The state is `config-files` after a remove
  and `half-configured` after an install that stopped. It exits with 1 for a package that dpkg never had
  (dpkg-query(1)). dpkg drops the `=` of a short option's value, so `-f=${Status}` works too.
- **Configuration files.** With `--force-confdef --force-confold` dpkg takes its default, where it has one, and
  otherwise keeps the installed file, when both the admin and the package changed it. Without them it asks, and
  without a terminal the question fails (dpkg(1)).

### 6.4 Restarts of Docker, containerd and Nomad

Read on 2026-10-05 in the Nomad v2.0.7 source and in Ubuntu's packages, whose postinst scripts ran in throwaway
containers, unless an item says otherwise.

- **A restart of Docker stops Nomad's containers.** Nomad's docker driver waits on each container with
  `ContainerWait` (`run()` in `drivers/docker/handle.go`, lines 281-353). dockerd cuts that stream when it stops; the
  driver logs `failed to wait for container; already terminated` and calls `ContainerStop` with a timeout of 0, and
  the task runner's `DestroyTask` removes the container (`ContainerRemove`). So Nomad itself stops the container that
  live-restore kept.
  - The stop is a guard against a wait that "returned incorrectly" (`handle.go:330-341`, present in v1.8.0), and no
    option of the driver changes it. hashicorp/nomad#24081 (merged 2024-09-30) made the wait's context endless.
    hashicorp/nomad#24105 asked for live-restore support and was closed in 2024-10 with a maintainer's note that
    Nomad picks the containers up again; 2.0.7 did not on the VM checks. #19962 (bridge-mode containers brought back
    with broken networks) and moby/moby#27987 (`docker wait` across a live-restore restart) are related.
  - The task restarts under the job's `restart` block, by default after 15 s plus up to 25% of jitter (17.3 s and
    18.7 s on the M2.6b VM checks, [3.16](#316-spike-runs)). Each such restart counts toward the block's attempts.
- **An upgrade of docker.io does not restart Docker.** The debconf question `docker.io/restart` defaults to false, and
  the postinst of 29.1.3-0ubuntu3~24.04.2 and 29.1.3-0ubuntu4.1 restarts Docker on an upgrade only when it is true
  (run with `DEBIAN_FRONTEND=noninteractive` on both releases; `debconf-show docker.io` gave `false` on Vultr's
  images, on the M2.6b VM reruns). unattended-upgrades installs docker.io from
  `<codename>-security/universe` (`Allowed-Origins` of `50unattended-upgrades`, with an empty `Package-Blacklist`), so
  it upgrades the package without restarting Docker, and a security fix of Docker takes effect only at the next
  reboot or restart of Docker.
- **An upgrade of containerd restarts it.** containerd 2.2.1-0ubuntu1~24.04.3 and 2.2.2-0ubuntu1.1 restart
  `containerd.service` on every upgrade without asking, and unattended-upgrades installs them from `-security`.
  `apt-daily-upgrade.timer` runs at 06:00 with `RandomizedDelaySec=60m`, so a fleet takes such an upgrade within an
  hour. It does not touch Nomad's docker tasks: on the M2.6b VM reruns a restart of containerd kept the task, and
  neither Docker nor Nomad restarted ([3.16](#316-spike-runs)). `docker.service` has `Wants=` and `After=` on
  `containerd.service`, and no `Requires=`, `BindsTo=` or `PartOf=` on it.
- **needrestart**, which `ubuntu-server` recommends, restarts services after library upgrades under APT. It skips
  `docker.service` (`qr(^docker) => 0`) but not `nomad.service`, since the Nomad binary is dynamically linked: a
  `libc6` security upgrade restarts Nomad. The tasks survive a restart of Nomad ([3.16](#316-spike-runs)). Vultr's
  images have needrestart (3.6-7ubuntu4.5 on 24.04, 3.11-1ubuntu2 on 26.04), with no `$nrconf{restart}` line, the
  package's default, and unattended-upgrades on (`20auto-upgrades`), on the M2.6b VM reruns of 2026-10-05.

### 6.5 cloud-init and the stub

- **cloud-init refuses a cloud-config that loads to nothing.** The handler (`cloudinit/handlers/cloud_config.py`)
  raises `ValueError("empty cloud config")`. The stub of M2.7b first held a header and a comment, which is YAML that
  loads to nothing. Checked on 2026-10-06 with the handler itself, in an ubuntu:24.04 container with cloud-init
  26.1-0ubuntu1~24.04.1: that stub logs `Failed at merging in cloud config part from part-001: empty cloud config`,
  and the same stub with a line `{}` after the comment loads as an empty mapping and merges without a warning.
- **A degraded status keeps `status: done` and `errors: []`.** `cloud-init status --wait --long` shows it in
  `extended_status: degraded done`, in the items under `recoverable_errors:`, and in exit code 2. A healthy node prints
  `extended_status: done`, `recoverable_errors: {}` and exits 0.
- **On a real node.** After the reboot of a scrubbed client, run `rugw2m` (2026-10-06) showed the degraded status, with
  the warning above. With the stub of three lines (`#cloud-config`, the comment, `{}`), run `sv3vwb` showed the healthy
  one.
- **`--wait` prints a dot for each wait,** on the line of the status (`..status: done` in run `sv3vwb`; the line had
  no dots in `rugw2m`), so a reader of the output must not expect the status at the start of a line.

---

## 7. Sources

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
- CNI plugins releases: <https://github.com/containernetworking/plugins/releases>
- CNI and bridge networking: <https://developer.hashicorp.com/nomad/docs/networking/cni>
- Source of v1.11.3: <https://github.com/hashicorp/nomad/tree/v1.11.3>
- Source of v2.0.7: <https://github.com/hashicorp/nomad/tree/v2.0.7>
- `consul` block: <https://developer.hashicorp.com/nomad/docs/configuration/consul>
- Eligibility after `drain_on_shutdown` (issue #17093): <https://github.com/hashicorp/nomad/issues/17093>
- Whether a stopped server should leave (issue #7943): <https://github.com/hashicorp/nomad/issues/7943>
- `nomad config validate`: <https://developer.hashicorp.com/nomad/commands/config/validate>
- The stock unit of v2.0.7:
  <https://github.com/hashicorp/nomad/blob/v2.0.7/.release/linux/package/usr/lib/systemd/system/nomad.service>
- HCL1 v1.0.0, which Nomad forks as `v1.0.1-nomad-1`: <https://github.com/hashicorp/hcl/tree/v1.0.0>

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

Ubuntu on nodes:
- docker.io source package: <https://launchpad.net/ubuntu/+source/docker.io-app>
- docker.io packaging (unit and postinst): <https://git.launchpad.net/ubuntu/+source/docker.io-app>
- moby's systemd unit: <https://github.com/moby/moby/blob/docker-v29.1.3/contrib/init/systemd/docker.service>
- systemctl(1): <https://www.freedesktop.org/software/systemd/man/latest/systemctl.html>
- Ubuntu packages: <https://packages.ubuntu.com/>
- Ubuntu cloud images and their manifests: <https://cloud-images.ubuntu.com/>
- ufw source package: <https://launchpad.net/ubuntu/+source/ufw>
- Docker packet filtering and firewalls: <https://docs.docker.com/engine/network/packet-filtering-firewalls/>
- Docker Engine 29 release notes: <https://docs.docker.com/engine/release-notes/29/>
- dockerd reference (configuration reload): <https://docs.docker.com/reference/cli/dockerd/>
- Nomad's docker driver waits on a container with a context that never ends (PR #24081):
  <https://github.com/hashicorp/nomad/pull/24081>
- Live-restore support in Nomad's docker driver (issue #24105): <https://github.com/hashicorp/nomad/issues/24105>
- Bridge-mode containers brought back with broken networks (issue #19962):
  <https://github.com/hashicorp/nomad/issues/19962>
- `docker wait`, live-restore and restarting dockerd (moby issue #27987): <https://github.com/moby/moby/issues/27987>
- CNI plugins: <https://github.com/containernetworking/plugins>
- nftables wiki, configuring chains: <https://wiki.nftables.org/wiki-nftables/index.php/Configuring_chains>
- nftables source: <https://git.netfilter.org/nftables/>
- apt source: <https://salsa.debian.org/apt-team/apt>
- dpkg source: <https://git.dpkg.org/cgit/dpkg/dpkg.git/>
- Linux v6.8 source: <https://github.com/torvalds/linux/tree/v6.8>

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

# tent-node-userdata

`tent-node-userdata` prints the user data that boots a machine into a node of a test cluster, to check a development
build of tent-node on a real machine without tent. The `tentnode` check of
[the Vultr spike](../vultr-spike/README.md) runs it for its instance.

The output is what `nodeconfig.UserData` makes of a NodeConfig. cloud-init writes the config to `/etc/tent/node.json`
(gz+b64), downloads tent-node, checks its sha256 and runs `tent-node install`.

## What the config holds

- Cluster `tent-node-check`, node group `nodes`, provider `vultr`, the role and the name you give.
- The tent-node asset, with the URL, sha256 and version you give. A node that runs a client (`client` or `combined`)
  also gets the `cni-plugins` asset for amd64 that the embedded `stable` channel pins. No Nomad: the phases that use
  it are not built yet, and the config needs none to validate.
- The system settings that tent gives the role, from `internal/app`. A node that runs a client gets them as a node
  group that keeps the docker driver: Docker, `br_netfilter` and `overlay`, and the three
  `net.bridge.bridge-nf-call-*` sysctls. A server gets none.
- The host firewall that tent gives the role in a cluster whose private network is `-cidr`, from `internal/app`:
  - `ssh` (22/tcp) and `icmp` from anywhere; `api` (4646/tcp) from anywhere on a server or combined node;
  - from the CIDR: `nomad-http` (4646/tcp) on every node, `nomad-rpc` (4647/tcp) and `serf` (4648/tcp and udp) on a
    server or combined node, `dynamic` (20000-32000/tcp and udp) on a client or combined node;
  - on a client or combined node, `bridge-http` and `bridge-dynamic` from Nomad's and Docker's bridges
    (172.26.64.0/20 and 172.17.0.0/16);
  - the metadata service 169.254.169.254 blocked for all but tent-node.

  tent-node's input chain drops everything else, so SSH works only because the `ssh` rule is there.
- Join by seed and refresh every minute, no servers.
- No files, so no certificate, key or token.

## Flags and environment

| Flag | Default | Value |
|---|---|---|
| `-name` | required | the node's name, which must be the machine's host name: tent-node's preflight refuses another |
| `-version` | required | the version of the tent-node binary, as `bin/tent version -o json` prints it after the same `make build`: tent-node refuses to run under another |
| `-url` | `$TENT_NODE_URL` | where the node downloads tent-node |
| `-sha256` | `$TENT_NODE_SHA256` | its sha256 |
| `-role` | `client` | the node's role: `server`, `client` or `combined` |
| `-cidr` | `10.64.0.0/16` | the private network of the node's cluster, the CIDR of the machine's VPC: the source of the rules between nodes |

`make dev-upload` sets `TENT_NODE_URL` and `TENT_NODE_SHA256` ([hack/tent-node-upload](../tent-node-upload/README.md)).
The usage and the errors never show the URL, which carries a signature. A flag the tool cannot parse, such as a CIDR
without its length, exits with 2; a value that the config rejects, such as an unknown role or a CIDR with bits set
after its length, exits with 1.

## Example (fish)

```fish
make dev-upload | source
go run ./hack/tent-node-userdata -name my-node-1 -version (./bin/tent version -o json | jq -r .version) \
    -role client -cidr 10.64.0.0/16 >user-data.yaml
```

## amd64 only

`make build` builds tent-node for linux/amd64 alone, so boot the machine on an amd64 plan: on arm64 the download
passes its sha256 check and then fails to run with an exec format error. The CNI plugins are the amd64 build too.

# tent-node-userdata

`tent-node-userdata` prints the user data that boots a machine into the only node of a test cluster, to check a
development build of tent-node on a real machine without tent. The `tentnode` check of
[the Vultr spike](../vultr-spike/README.md) runs it for its instance.

The output is what `nodeconfig.UserData` makes of the NodeConfig that tent gives the node: the tool builds the
cluster's specs and calls `internal/app` (`NodeConfigOf`), the code that renders a node's config for tent. cloud-init
writes the config to `/etc/tent/node.json` (gz+b64, mode 0600), downloads tent-node, checks its sha256 and runs
`tent-node install`.

## Secrets

The output carries the secrets of a throwaway CA. Each run makes a new CA, the node's certificate and a gossip key.
The CA's key stays in the tool's memory and is lost when it exits. The user data carries the CA bundle (certificates
only), the node's certificate and key, and the gossip key, as a real node's does, so anyone who can read the user data
can join the test cluster:

- delete the machine after the check (the spike deletes it on exit unless `--keep` is given);
- do not reuse the user data for another machine.

The tool prints the user data to stdout and nothing else. Its errors and usage never show a key or the tent-node URL,
which carries a signature. The user data must fit in 24 KiB (`nodeconfig.MaxUserDataBytes`); with a presigned URL of
about 400 characters it takes about 6 KB.

## What the config holds

The specs are those of a cluster `tent-node-check` on Vultr with one node group `nodes` of one `combined` node, with
tent's defaults: Nomad region `global`, the `stable` channel and its recommended Nomad, `verifyHTTPSClient` true. A
combined group makes the client introduction `warn`, so the node needs no intro token.

- The name you give, the zone (`-zone`, Nomad's datacenter) and the private network (`-cidr`).
- The assets: `nomad` for linux/amd64, whose sha256 comes from HashiCorp's SHA256SUMS after its signature is checked
  with the release key that tent embeds (the tool reads two files from releases.hashicorp.com); `cni-plugins` for
  amd64 as `stable` pins them; and the tent-node at the URL with the sha256 and version you give.
- The files: `00-tent.hcl`, `01-gossip.hcl`, the CA bundle `tls/ca.pem`, `nomad.service`, `10-node.hcl` with
  `bootstrap_expect = 1`, and the node's `tls/agent.pem` and `tls/agent-key.pem`. No join seed: the node is the
  cluster's only server.
- The system settings of a node that runs a client with the docker driver: Docker, `br_netfilter` and `overlay`, and
  the three `net.bridge.bridge-nf-call-*` sysctls.
- The host firewall of a combined node in a cluster whose private network is `-cidr`:
  - `ssh` (22/tcp), `icmp` and `api` (4646/tcp) from anywhere;
  - from the CIDR: `nomad-http` (4646/tcp), `nomad-rpc` (4647/tcp), `serf` (4648/tcp and udp) and `dynamic`
    (20000-32000/tcp and udp);
  - `bridge-http` and `bridge-dynamic` from Nomad's and Docker's bridges (172.26.64.0/20 and 172.17.0.0/16);
  - the metadata service 169.254.169.254 blocked for all but tent-node.

  tent-node's input chain drops everything else, so SSH works only because the `ssh` rule is there.
- Join by seed and refresh every minute, with the spec hash of the group's configuration.

## Flags and environment

| Flag | Default | Value |
|---|---|---|
| `-name` | required | the node's name, which must be the machine's host name: tent-node's preflight refuses another |
| `-version` | required | the version of the tent-node binary, as `bin/tent version -o json` prints it after the same `make build`: tent-node refuses to run under another. A release or pre-release version is refused: tent gives the nodes of a release the release's tent-node |
| `-url` | `$TENT_NODE_URL` | where the node downloads tent-node |
| `-sha256` | `$TENT_NODE_SHA256` | its sha256 |
| `-zone` | `ams` | the machine's zone, which on Vultr is its region: the cluster's `spec.cloud.region` and Nomad's datacenter |
| `-cidr` | `10.64.0.0/16` | the private network of the node's cluster, the CIDR of the machine's VPC: the source of the rules between nodes |

`make dev-upload` sets `TENT_NODE_URL` and `TENT_NODE_SHA256` ([hack/tent-node-upload](../tent-node-upload/README.md)).
A flag the tool cannot parse, such as a CIDR without its length, and a release version exit with 2. A value that the
specs or the config reject, such as a CIDR with bits set after its length, a public CIDR or a name that is no host
name, and a Nomad release that cannot be read or checked, exit with 1.

## Example (fish)

```fish
make dev-upload | source
go run ./hack/tent-node-userdata -name my-node-1 -version (./bin/tent version -o json | jq -r .version) \
    -zone ams -cidr 10.64.0.0/16 >user-data.yaml
```

`user-data.yaml` holds the node's key and the gossip key: remove it after use.

## amd64 only

`make build` builds tent-node for linux/amd64 alone, so boot the machine on an amd64 plan: on arm64 the download
passes its sha256 check and then fails to run with an exec format error. Nomad and the CNI plugins are the amd64
builds too.

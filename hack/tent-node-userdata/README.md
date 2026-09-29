# tent-node-userdata

`tent-node-userdata` prints the user data that boots a machine into a node of a test cluster, to check a development
build of tent-node on a real machine without tent. The `tentnode` check of
[the Vultr spike](../vultr-spike/README.md) runs it for its instance.

The output is what `nodeconfig.UserData` makes of a NodeConfig. cloud-init writes the config to `/etc/tent/node.json`
(gz+b64), downloads tent-node, checks its sha256 and runs `tent-node install`.

## What the config holds

- Cluster `tent-node-check`, node group `clients`, provider `vultr`, role `client`, and the name you give.
- One asset, tent-node, with the URL, sha256 and version you give. No Nomad and no CNI plugins: the phases that
  would use them are not built yet, and the config needs none of them to validate.
- The system settings that tent gives a client that runs Docker: `br_netfilter` and `overlay`, and the three
  `net.bridge.bridge-nf-call-*` sysctls.
- Join by seed and refresh every minute, no servers; the host firewall blocks the metadata service and has no rules.
- No files, so no certificate, key or token.

## Flags and environment

| Flag | Default | Value |
|---|---|---|
| `-name` | required | the node's name, which must be the machine's host name: tent-node's preflight refuses another |
| `-version` | required | the version of the tent-node binary, as `bin/tent version -o json` prints it after the same `make build`: tent-node refuses to run under another |
| `-url` | `$TENT_NODE_URL` | where the node downloads tent-node |
| `-sha256` | `$TENT_NODE_SHA256` | its sha256 |

`make dev-upload` sets `TENT_NODE_URL` and `TENT_NODE_SHA256` ([hack/tent-node-upload](../tent-node-upload/README.md)).
The usage and the errors never show the URL, which carries a signature.

## Example (fish)

```fish
make dev-upload | source
go run ./hack/tent-node-userdata -name my-node-1 -version (./bin/tent version -o json | jq -r .version) >user-data.yaml
```

## amd64 only

`make build` builds tent-node for linux/amd64 alone, so boot the machine on an amd64 plan: on arm64 the download
passes its sha256 check and then fails to run with an exec format error.

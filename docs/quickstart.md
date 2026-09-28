# Quick start

This guide builds a small cluster on Vultr and deletes it again. It takes about ten minutes and costs a few cents.

> tent is at milestone M1. It builds a cluster's network, firewalls, SSH keys and machines on Vultr, but the
> machines are empty: Nomad comes with M2 ([roadmap](roadmap.md)). So the guide shows how tent plans, builds, checks
> and deletes a cluster; there is nothing to run on it yet.

## What you need

- A Vultr account and an API key (Account → API). Vultr accepts the key only from the addresses in the access
  control list next to it, so add the address you run tent from.
- Go 1.26 or newer, to build tent. The only release so far, the pre-release `v0.1.0-rc.1`, predates the commands
  that reach the cloud.
- An SSH key pair, if you want to log in to the machines.

## 1. Install tent

```sh
go install github.com/ingvarch/tent/cmd/tent@main
tent version
```

`go install` puts `tent` into `$(go env GOPATH)/bin`, which must be on your `PATH`.

## 2. Set the state store and the API key

tent keeps a cluster's specs in a state store and, from the first build, its CA key and secrets, so keep the store
private ([architecture §10.3](architecture.md#103-secrets-at-rest)). For this guide a local directory is enough; a
team shares an S3 bucket, `s3://bucket/prefix` ([architecture §10](architecture.md#10-state-store-and-locking)).

```sh
export TENT_STATE="file://$HOME/.tent"
export VULTR_API_KEY="<your API key>"
```

In fish: `set -x TENT_STATE file://$HOME/.tent` and `set -x VULTR_API_KEY <your API key>`.

tent reads the API key only from the environment. It never writes the key into the specs, the state store, its logs
or the machines.

## 3. Write the specs

```sh
tent create cluster demo \
  --provider vultr --region ams --machine-type vc2-1c-1gb \
  --combined \
  --ssh-key ~/.ssh/id_ed25519.pub \
  --ssh-access 203.0.113.7/32 \
  --api-access 203.0.113.7/32
```

```
cluster demo created
node group nodes created
```

- `--combined` makes one node group, `nodes`, of three machines that are both Nomad servers and clients. Without it
  tent makes three servers and three workers.
- `--ssh-access` and `--api-access` take the addresses that may reach SSH (port 22) and the Nomad API (port 4646).
  Put your own address in place of `203.0.113.7`. Without `--ssh-access` no one can reach SSH. Without
  `--api-access` the whole internet can reach the Nomad API, and tent warns about it.
- `vc2-1c-1gb` is Vultr's smallest plan: $5 a month, about $0.0074 an hour, for each machine (September 2026).

Nothing exists in the cloud yet. `tent get demo` prints the specs as YAML, and `tent edit cluster demo` or
`tent edit nodegroup nodes --name demo` changes them.

## 4. See the plan

```sh
tent update cluster demo
```

```
+ vultr.SSHKey/demo-7855a371
+ vultr.VPC/demo
    + cidr: 10.64.0.0/16
    + region: ams
+ vultr.FirewallGroup/demo-servers
    + rule: v4 icmp 0.0.0.0/0
    + rule: v4 tcp 203.0.113.7/32 22
    + rule: v4 tcp 203.0.113.7/32 4646
    + rule: v6 icmp ::/0
+ node demo-nodes-0 (combined, vc2-1c-1gb, ams)
+ node demo-nodes-1 (combined, vc2-1c-1gb, ams)
+ node demo-nodes-2 (combined, vc2-1c-1gb, ams)

Plan: 3 to create, 0 to update, 0 to replace, 0 to delete.
Nodes: 3 to create, 0 to wait for, 0 to delete.
State: pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token and cluster.completed.yaml will be written.
run with --yes to apply the changes
```

`+` creates, `~` changes or waits for an object, and `-` deletes one. The name of the SSH key ends with a hash of
the key. Without `--yes` tent changes nothing. With `--exit-code` it exits with 2 when the plan has changes, which
suits scripts and CI.

## 5. Build the cluster

```sh
tent update cluster demo --yes
```

tent prints the plan again, then each step as it happens, then what it did. It creates the SSH key, the VPC and the
firewall group at the same time, so their lines may come in another order:

```
creating vultr.FirewallGroup/demo-servers
creating vultr.SSHKey/demo-7855a371
creating vultr.VPC/demo
created vultr.VPC/demo
created vultr.FirewallGroup/demo-servers
created vultr.SSHKey/demo-7855a371
creating node demo-nodes-0
created node demo-nodes-0 (10.64.0.3)
creating node demo-nodes-1
created node demo-nodes-1 (10.64.0.4)
creating node demo-nodes-2
created node demo-nodes-2 (10.64.0.5)

Applied: 3 created, 0 updated, 0 replaced, 0 deleted. Nodes: 3 created, 0 waited for, 0 deleted. Wrote pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token and cluster.completed.yaml.
```

- tent creates the machines one at a time and waits until Vultr reports each one running with its address in the
  VPC. That takes about two minutes per machine.
- `cluster.completed.yaml` in the state store holds the specs with every default that tent applied. The first build
  also writes the cluster's CA, gossip key and ACL bootstrap secret there.
- If the run stops halfway, because of Ctrl-C or a lost connection, run the same command again. tent finds what the
  earlier run created by the markers it put on each object, and each machine by the operation id of its create call.
  It never creates a second machine for one node.

## 6. Check it

```sh
tent update cluster demo --exit-code
```

```
No changes.
```

The exit code is 0: the cloud matches the specs.

tent does not list the machines yet; `tent get nodes` comes later. The Vultr console shows them as `demo-nodes-0` to
`demo-nodes-2`, with their public addresses. From an address in `--ssh-access` you can log in as `root` with your
SSH key.

To change the number of machines, edit the group's `size` with `tent edit nodegroup nodes --name demo` and run
`tent update cluster demo --yes` again. tent creates the missing machines, or deletes the surplus ones, newest first.
A combined group, like a server group, has 1, 3 or 5 machines, and 1 needs `--allow-single-server` on each command.

## 7. Delete the cluster

```sh
tent delete cluster demo
```

```
- node demo-nodes-0 (ID <id>)
- node demo-nodes-1 (ID <id>)
- node demo-nodes-2 (ID <id>)
- vultr.FirewallGroup/demo-servers (ID <id>)
- vultr.VPC/demo (ID <id>)
- vultr.SSHKey/demo-7855a371 (ID <id>)
- state demo/cluster.completed.yaml
- state demo/nodegroups/nodes.yaml
- state demo/secrets/acl-bootstrap-token
- state demo/secrets/gossip.key
- state demo/pki/ca-bundle.pem
- state demo/pki/private/ca.key
- state demo/cluster.yaml

Nodes: 3 to delete.
Plan: 0 to create, 0 to update, 0 to replace, 3 to delete.
State: 7 objects to delete.
run with --yes to delete them
```

Without `--yes` this is only the plan. To delete:

```sh
tent delete cluster demo --yes
```

tent prints the plan again, then each step, then what it deleted:

```
deleting node demo-nodes-0 (ID <id>)
deleted node demo-nodes-0 (ID <id>)
deleting node demo-nodes-1 (ID <id>)
deleted node demo-nodes-1 (ID <id>)
deleting node demo-nodes-2 (ID <id>)
deleted node demo-nodes-2 (ID <id>)
deleting vultr.FirewallGroup/demo-servers (ID <id>)
deleted vultr.FirewallGroup/demo-servers (ID <id>)
deleting vultr.VPC/demo (ID <id>)
deleted vultr.VPC/demo (ID <id>)
deleting vultr.SSHKey/demo-7855a371 (ID <id>)
deleted vultr.SSHKey/demo-7855a371 (ID <id>)

Deleted: 3 nodes, 3 infrastructure objects, 7 state objects.
```

tent deletes the machines first and waits until Vultr no longer lists them. Then it deletes the firewall group, the
VPC and the SSH key, and last the specs and the secrets in the state store. It finds the cloud objects by the markers
it put on them, so it deletes only this cluster's objects.

## Next

- [Architecture §13](architecture.md#13-lifecycle-flows): what `update cluster` and `delete cluster` do, step by
  step.
- [Architecture §14](architecture.md#14-cli): every command, flag, output format and exit code.
- [Roadmap](roadmap.md): what comes next.

# Quick start

This guide builds a small Nomad cluster on Vultr and deletes it again. It takes about fifteen minutes and costs a
few cents.

> tent is at milestone M2 ([roadmap](roadmap.md)). `update cluster --yes` builds the network, the firewalls, the
> machines and a running, secured Nomad cluster: a leader, ACLs bootstrapped with the secret in the state store, and
> registered nodes. Once a node has joined, tent replaces its user data, which holds its keys, with a stub.
> `tent export nomad` gives you a short-lived certificate and token for the Nomad API, `tent validate cluster` checks
> the cluster, and `tent ui` serves the Nomad web UI on a port of your machine. Not done yet:
> - tent cannot scale a running cluster down before M3: `update` refuses to delete a node that joined Nomad, since it
>   cannot drain a node or check the Raft quorum yet. `delete cluster` still deletes everything.

## What you need

- A Vultr account and an API key (Account → API). Vultr accepts the key only from the addresses in the access
  control list next to it, so add the address you run tent from.
- Go 1.26 or newer, to build tent. The releases so far predate the Nomad bootstrap, so the guide builds tent from
  `main`.
- The tent-node of that build. A development build needs `TENT_NODE_URL` and `TENT_NODE_SHA256`, which
  `make dev-upload` prints the lines that set, in a clone of the repository, which `go install` does not give you
  (it needs an R2 bucket and token; [details](../hack/tent-node-upload/README.md)). Without them
  `tent update cluster` fails with `find tent-node: tent dev is a development build, so no release holds its
  tent-node: set TENT_NODE_URL and TENT_NODE_SHA256 to a tent-node built from the same commit`. A release build finds
  its tent-node in its own release and ignores the variables. The tent-node is for linux/amd64, which is all that
  Vultr offers.
- An SSH key pair, if you want to log in to the machines.
- The `nomad` CLI, for steps 7 and 8 ([install Nomad](https://developer.hashicorp.com/nomad/install)). A browser, for
  step 10.

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

On stderr tent also warns, because the group is combined: `WARNING: node group nodes is combined: its nodes run the
Nomad servers and the workloads together, which is meant for development and small clusters; workloads share them
with Raft and the gossip key`. `tent update cluster demo --yes` prints the same line after the plan, before its first
step.

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
Nomad: bootstrap the ACL system and wait for 3 healthy servers.
State: pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token, cluster.completed.yaml and nomad/bootstrapped will be written.
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
firewall group at the same time, so their lines may come in another order. After the machines it waits for Nomad:

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
waiting for a Nomad leader
Nomad has a leader (10.64.0.3:4647)
bootstrapping the ACL system
bootstrapped the ACL system
waiting for 3 healthy Nomad servers
3 Nomad servers are healthy
waiting for Nomad's keyring
Nomad's keyring is ready
waiting for node demo-nodes-0 to register
node demo-nodes-0 registered
scrubbing the user data of node demo-nodes-0
scrubbed the user data of node demo-nodes-0
waiting for node demo-nodes-1 to register
node demo-nodes-1 registered
scrubbing the user data of node demo-nodes-1
scrubbed the user data of node demo-nodes-1
waiting for node demo-nodes-2 to register
node demo-nodes-2 registered
scrubbing the user data of node demo-nodes-2
scrubbed the user data of node demo-nodes-2

Applied: 3 created, 0 updated, 0 replaced, 0 deleted. Nodes: 3 created, 0 waited for, 0 deleted. Nomad: bootstrapped the ACL system; 3 servers are healthy. Wrote pki/private/ca.key, pki/ca-bundle.pem, secrets/gossip.key, secrets/acl-bootstrap-token, cluster.completed.yaml and nomad/bootstrapped.
```

- tent creates the machines one at a time and waits until Vultr reports each one running with its address in the
  VPC. That takes about two minutes per machine. Each machine boots with its own configuration, which tent-node turns
  into a running Nomad agent. The first server has no peers to join; every later server and every client is given the
  servers that exist.
- Once the servers have a leader, tent bootstraps the ACL system with the secret in the state store, then waits until
  the servers are healthy and Nomad's keyring has an active key, which signs intro tokens. Then, for each combined
  node, it waits until the node has registered, replaces the node's user data with a stub and labels the machine
  `tent/joined=true`. With separate clients it creates them after that, each with an intro token, waits for each to
  register and scrubs it. Each of these waits takes at most 10 minutes.
- `cluster.completed.yaml` in the state store holds the specs with every default that tent applied, among them the
  Nomad version. The first build also writes the cluster's CA, gossip key and ACL bootstrap secret there, and
  `nomad/bootstrapped`, the mark that the ACL system is bootstrapped.
- If the run stops halfway, because of Ctrl-C or a lost connection, run the same command again. tent finds what the
  earlier run created by the markers it put on each object, and each machine by the operation id of its create call. It
  never creates a second machine for one node. It repeats the leader wait, the bootstrap and the health wait until the
  mark is written, and it waits for every machine whose node has not joined yet, which is a machine without the label.
  A client that has not registered 31 minutes after its machine was created is deleted and created again. The release
  files of Nomad (releases.hashicorp.com) are read only by a run that creates a machine or repeats the create of one.

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

To add machines, edit the group's `size` with `tent edit nodegroup nodes --name demo` and run
`tent update cluster demo --yes` again. tent creates the missing machines and they join the cluster. A running
group cannot be made smaller yet: before M3 `update` refuses to delete a node that joined Nomad, and fails with an
error that names the node. A combined group, like a server group, has 1, 3 or 5 machines, and 1
needs `--allow-single-server` on each command.

## 7. Reach the Nomad API

The Nomad API needs a client certificate of the cluster's CA and an ACL token. tent keeps the CA key and the ACL
bootstrap secret in the state store and never gives them to you. `tent export nomad` makes a certificate and a token
that both end after 24 hours instead:

```sh
tent export nomad demo
```

```
export NOMAD_ADDR='https://198.51.100.10:4646'
export NOMAD_CACERT='/home/you/.cache/tent/demo/ca.pem'
export NOMAD_CLIENT_CERT='/home/you/.cache/tent/demo/cli.pem'
export NOMAD_CLIENT_KEY='/home/you/.cache/tent/demo/cli-key.pem'
export NOMAD_TLS_SERVER_NAME='server.global.nomad'
export NOMAD_TOKEN="$(cat '/home/you/.cache/tent/demo/token')"
```

On stderr tent says where it wrote the files, when the access ends and the token's accessor:

```
wrote the Nomad access of cluster demo to /home/you/.cache/tent/demo; it works until 2026-10-08 12:00:00 UTC (token accessor 3f6c2a9e-...)
```

- The files are `ca.pem`, `cli.pem`, `cli-key.pem` and `token`, in `$XDG_CACHE_HOME/tent/demo`, else
  `~/.cache/tent/demo`. `--dir` picks another place. The directory has mode 0700 and the files 0600.
- The last line reads the token from its file when your shell runs it, so the token never appears on the screen. The
  address is one server's public address.
- `--ttl` sets how long the access lasts. Nomad refuses less than 1 minute and more than 24 hours.
- The lines are for sh, bash and zsh. In fish, `--shell fish` prints `set -gx` lines, and tent picks them by itself
  when `$SHELL` names fish. For another shell, `-o json` prints the paths and the address.
- Run the command again for a new token. The earlier token works until it ends. To revoke it sooner, run `nomad acl
  token delete <accessor>` with the new access.

Run the lines:

```sh
eval "$(tent export nomad demo)"
```

```fish
tent export nomad demo | source
```

```sh
nomad server members
nomad node status
```

`nomad server members` lists the three servers as alive, with one leader, and `nomad node status` the three nodes as
ready. The CLI may end its output with a hint to a Web UI address on the server. A browser cannot open that address,
since the server asks for the client certificate: use `tent ui` (step 10).

## 8. Run a job

The nodes run Docker, and Nomad's bridge network is set up. Save this as `hello.nomad.hcl`:

```hcl
job "hello" {
  group "web" {
    network {
      mode = "bridge"
      port "http" {
        to = 8080
      }
    }
    task "web" {
      driver = "docker"
      config {
        image   = "busybox:1.38"
        command = "sh"
        args    = ["-c", "mkdir -p /www && echo hello >/www/index.html && exec httpd -f -p 8080 -h /www"]
        ports   = ["http"]
      }
      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
```

```sh
nomad job run hello.nomad.hcl
nomad job status hello
```

The status shows an allocation that runs on one of the nodes. Stop the job with `nomad job stop -purge hello`.

## 9. Validate the cluster

```sh
tent validate cluster demo
```

```
cluster demo is valid: 3 servers and 3 clients run Nomad 2.0.7
```

tent compares the specs with the machines in the cloud and with Nomad, and changes nothing. It checks that every node
group has its machines and that the cloud reports them running, that each node has joined, that Nomad has a leader,
that every server votes and is healthy, that every client is registered, ready and eligible, that each node runs the
pinned Nomad version, and that no certificate has ended. It needs `VULTR_API_KEY` and a way to port 4646 of the servers,
which `--api-access` allows.

On stderr tent warns about what is not a failure. This cluster is combined and runs in one data center, so it prints:

```
WARNING: node group nodes is combined: its nodes run the Nomad servers and the workloads together, which is meant for development and small clusters; workloads share them with Raft and the gossip key
WARNING: cluster demo runs in one failure domain, ams: an outage there takes the whole cluster down
```

A cluster that differs from its specs prints a table instead, such as:

```
NODE          FAILURE
demo-nodes-1  its Nomad client is down

cluster demo is not valid: 1 failure
```

and the exit code is 2. The exit code is 1 when tent could not check at all. `--wait 5m` checks every 10 seconds until
the cluster is valid or the time has passed, which suits a script that builds a cluster and then waits for it.

## 10. Open the Nomad UI

```sh
tent ui demo
```

```
Nomad UI of cluster demo: http://127.0.0.1:4646/ui/
```

Open the address in a browser. tent keeps running and passes each request to a server of the cluster with a certificate
and a token, so the browser needs neither. On stderr tent says when the session ends:

```
the Nomad CLI works through it with NOMAD_ADDR=http://127.0.0.1:4646; press Ctrl-C to stop; the session ends at 2026-10-08 12:00:00 UTC
```

- Stop it with Ctrl-C. The session also ends after 24 hours.
- Type or paste the address, or open it from the terminal: tent refuses a link to it on a web page of another site.
- While it runs, every program on your machine that can reach the port acts as an administrator of the cluster. So tent
  listens on a loopback address only, and refuses a request with another `Host` or `Origin`. Do not run it on a
  machine that others use.
- The `nomad` CLI works through the same port: set `NOMAD_ADDR` to the address in the notice and nothing else.
- `--listen 127.0.0.1:0` picks a free port, for a machine where a local Nomad uses 4646.
- The port is on the machine that runs tent. When that is another machine, forward the same port to it, for example
  `ssh -L 4646:127.0.0.1:4646 that-machine`, and open the address on your own machine; tent refuses a request that
  names another port.

## 11. Delete the cluster

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
- state demo/nomad/bootstrapped
- state demo/secrets/acl-bootstrap-token
- state demo/secrets/gossip.key
- state demo/pki/ca-bundle.pem
- state demo/pki/private/ca.key
- state demo/cluster.yaml

Nodes: 3 to delete.
Plan: 0 to create, 0 to update, 0 to replace, 3 to delete.
State: 8 objects to delete.
run with --yes to delete them
```

A release build, and the `bin/tent` that `make build` makes, also lists `- state demo/tent-version`, so the count is
9.

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

Deleted: 3 nodes, 3 infrastructure objects, 8 state objects.
```

tent deletes the machines first and waits until Vultr no longer lists them. Then it deletes the firewall group, the
VPC and the SSH key, and last the specs and the secrets in the state store. It finds the cloud objects by the markers
it put on them, so it deletes only this cluster's objects.

`delete cluster` does not touch the files of `tent export nomad`. Remove the directory yourself, such as
`~/.cache/tent/demo`. The certificate and the token in it are no use once the cluster is gone.

## Next

- [Architecture §13](architecture.md#13-lifecycle-flows): what `update cluster` and `delete cluster` do, step by
  step.
- [Architecture §14](architecture.md#14-cli): every command, flag, output format and exit code.
- [Architecture §9.7](architecture.md#97-operator-access) and
  [§13.6](architecture.md#136-tent-validate-cluster---wait-duration): what `export nomad`, `ui` and `validate cluster`
  do, in detail.
- [Roadmap](roadmap.md): what comes next.

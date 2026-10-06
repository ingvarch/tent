# ADR-0031: Bootstrap in `update`

- **Status:** Accepted; amended by [ADR-0032](0032-joined-label-scrub-and-delete-guard.md) (the limits "Secrets stay in
  user data", "A ready machine that has not registered is not seen" and "A health wait that stops" are closed, the M2.7b
  follow-up is built, and the bootstrap mark is written after the servers' scrubs; a renamed server group is refused
  once its machines carry the joined label; of item 4, only a create and a wait that repeats a create read the assets)
- **Date:** 2026-10-05
- **Deciders:** ingvarch
- **Related:** amends [ADR-0005](0005-immutable-nodes-and-nomad-aware-rollouts.md),
  [ADR-0016](0016-server-discovery-seed-and-refresh.md), [ADR-0017](0017-api-driven-server-removal.md),
  [ADR-0019](0019-combined-server-client-role.md), [ADR-0024](0024-cluster-pki-storage-and-certificates.md),
  [ADR-0026](0026-channels-and-release-assets.md), [ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md)
  and [ADR-0030](0030-nomad-on-nodes.md) (see their Status lines); extends [ADR-0021](0021-import-rules.md)
  (`assetstest`); builds on [ADR-0015](0015-idempotency-without-unique-names.md) and
  [ADR-0028](0028-tent-node-agent-units-and-delivery.md);
  [architecture §9.2](../architecture.md#92-acl-and-tokens), [§9.3](../architecture.md#93-client-introduction),
  [§13.2](../architecture.md#132-tent-update-cluster---yes), [§18](../architecture.md#18-open-questions),
  [platform notes §1.6](../platform-notes.md#16-the-agent-on-a-node)

## Context

Until M2.7 `update` made empty machines. M2.7a makes `tent update cluster --yes` (and `create --yes`) build a running,
secured Nomad cluster: servers with real user data, a leader, the ACL system bootstrapped with the stored secret,
healthy servers that all vote, and clients with intro tokens that register. The scrub of user data, nodes that never
registered and the guard against deleting a registered node or a Raft peer were left to M2.7b
([ADR-0032](0032-joined-label-scrub-and-delete-guard.md)).

The facts that shaped it. The ones about Nomad were verified on 2026-10-05 on a local Nomad 2.0.7 and in its source
([platform notes §1.6](../platform-notes.md#16-the-agent-on-a-node)):

- **The bootstrap call carries the secret as its token.** `nomadops` refuses an empty token, so every client holds the
  bootstrap secret, also for `PUT /v1/acl/bootstrap` itself. Nomad throws away the auth error of that call, and
  accepts it.
- **A new cluster is healthy at once.** After the bootstrap, autopilot reported `Healthy` with all voters at the first
  call, for one server and for three, since every server is in the first Raft configuration.
- **A server without `leave_on_terminate` exits with 1 on SIGTERM** and stays a voter.
- **ADR-0019 item 4** says tent gives an intro token to every node it creates once the cluster runs, combined nodes
  too. The first combined nodes cannot get one: their clients register before the cluster has a token.

## Decision

### The flow

1. **The order.** `update` applies the infrastructure's task changes; the delete of a stale mark (decision 8); the
   servers; a Nomad step; the clients; the node deletes; the infrastructure's deletes ([architecture
   §13.2](../architecture.md#132-tent-update-cluster---yes)).
   - A server that is not ready has no private address, so its wait runs before the creates of its role (server and
     combined nodes first, then clients). Two servers that do not know each other never complete `bootstrap_expect`.
   - The completed spec is written before the first node, with the secrets, so the nodes run the Nomad version it pins
     (decision 12 of [architecture §18](../architecture.md#18-open-questions)).
2. **The seed.** A server or combined node is seeded with the private addresses of every other server that the run
   knows, by name. The first server of a cluster without servers has an empty seed. When servers are listed and none has
   a private address, the change fails and says to run the command again. A client is seeded with every known server,
   and fails the same way when it knows none.
   `bootstrap_expect` is the size of the server group on every server.

### Node configuration

3. **One node builder** (`internal/app`) makes the NodeConfig of every node: the template of each node group, then one
   config per node from its name, zone, certificate, seed and intro token. `update` and `app.NodeConfigOf` use it, so
   the VM check tool and `update` cannot diverge.
4. **The assets are read only by a plan that creates a node or repeats the create of one.** Such a plan needs
   releases.hashicorp.com, and for a release build github.com. A plan that does neither needs neither,
   nor `TENT_NODE_URL`: a run that changes no node, or a drift check (`--exit-code`) of a cluster whose nodes are all
   there. A drift check that finds a node to create, or a machine whose create it must repeat, reads the assets like any
   other plan; one that finds only machines to wait for reads none.
   - They are read once per architecture and once per run: the builder keeps them in a cache that the plan and the
     apply share. The cache key is the Nomad version, the tent version and the architecture, without the channel; a
     second embedded channel needs its name in the key.
   - The architecture of each group's machines comes from `cloud.Provider.Arch`. Vultr answers amd64 for every plan,
     with no call, because it lists no arm64 Cloud Compute plan (151 plans on 2026-10-05, [platform notes
     §3.8](../platform-notes.md#38-regions-images-plans-and-billing-)).
   - A development build without `TENT_NODE_URL` and `TENT_NODE_SHA256` fails a plan that creates nodes
     ([ADR-0026](0026-channels-and-release-assets.md)). `cmd/tent` reads the two variables once at start.
5. **The size of the user data is checked at plan time**, so a group that does not fit fails the plan, before any
   write. The check builds each planned node's config with a certificate issued for it, and for a client a stand-in
   intro token of 2048 bytes that gzip shrinks no more than a real one (base64 text of random bytes). A real token is
   about 750 bytes for a short name ([platform notes §1.6](../platform-notes.md#16-the-agent-on-a-node)).
6. **The run's secrets and certificates.** One run holds the CA, the gossip key and the bootstrap secret in memory,
   stored or new. It issues node certificates and one operator certificate from the CA and stores none. The operator
   certificate lasts 24 hours and lives in memory only ([architecture §9.7](../architecture.md#97-operator-access)).

### Nomad

7. **`nomadops.Servers`** makes the servers of a cluster one `nomadops.API`, so the waits keep polling when one server
   is down: a call moves to the next server after an error that matches `ErrNotReady`, and any other error ends it.
   `Bootstrap` is safe to repeat, and a second `IntroToken` makes a token that nothing uses, so writes move on too.
   ([Architecture §13.2](../architecture.md#132-tent-update-cluster---yes) has the error texts.)
8. **The Nomad step** waits for a leader, bootstraps the ACL system, waits until the servers are healthy and all vote,
   and then writes the mark (M2.7b writes it after the scrubs of the servers). tent reaches one `nomadops` client per
   server with a public address, on port 4646, with the operator certificate and the bootstrap secret as the token.
   - **The mark** is `<cluster>/nomad/bootstrapped` in the state store. Without it, or when no server or combined
     machine of the cluster stays, the Nomad step is planned with the bootstrap, so a run cut before the servers were
     healthy is finished by the next run. In the second case `update` deletes the stored mark before the first node
     change, which makes that true for a rebuild. With the mark and a server or combined machine that stays, the step
     is planned only when the plan creates or waits for a server or combined node, and without the bootstrap.
9. **Clients.** After the servers are healthy, each client change asks for an intro token right before the create: 30
   minutes, bound to the node's name and its group's pool. The create seeds the node with every server, and `update`
   waits until Nomad lists the node ready and eligible. A client registered 21 to 24 s
   after a fresh server's leadership on the M2.6b VM checks (an inference from one combined node, ADR-0030).
   - Every client gets a token, whatever the cluster's enforcement: a token that expired is refused even under `warn`,
     and one path is simpler.
   - **Combined nodes get no token**, which amends ADR-0019 item 4. Their clusters never run `strict` (`warn` by
     default), so a token adds nothing, and `update` asks for none, also for a combined node made after the bootstrap.
     (`nodeConfig` would take one on a combined node; it refuses one only on the `server` role.) After the health
     wait `update` waits until each combined node that the plan created or waited for has registered.
10. **Deadlines and single tries.** The leader wait, the health wait and each registration wait have 10 minutes. A
    create and a wait for a machine keep their 10 minutes. `Bootstrap` and `IntroToken` are tried once on each server
    and not repeated in a loop: a failure stops the run.
    - The leader wait's timeout error names `spec.access.api`, since an operator outside it gets no answer.

### Output

11. **Output.** The plan, the summary, the JSON plan and the progress events gain a Nomad part
    ([architecture §13.2](../architecture.md#132-tent-update-cluster---yes), [§14](../architecture.md#14-cli)).

### Servers and `leave_on_terminate` (maintainer decision 26)

12. **`leave_on_terminate = false` on server and combined agents**; clients keep `true`. The line stays in every
    role's `00-tent.hcl`, so an operator reads the value. The server groups' spec hash moves with the file.
    - A stopped server stays a Raft peer, so a short restart keeps the voter set and no cluster comes back only through
      the last server that left. Nomad's own docs say to set it on servers only for a server that never joins again.
    - tent removes a server from Raft through the API, on every provider
      ([ADR-0017](0017-api-driven-server-removal.md), M3). The graceful leave of an ACPI shutdown is gone for servers.
    - Nomad exits with 1 on SIGTERM without it (run on 2026-10-05), so `nomad.service` on server and combined nodes
      ends as failed after a stop. On a real server (2026-10-05) `systemctl restart nomad` exited 0, the unit was
      active again at once, and its journal had one `Failed with result 'exit-code'` line. tent accepts that:
      `SuccessExitStatus=1` would hide real failures. The unit's text does not change.
    - Nomad merges the key across the files of its configuration directory with OR (`command/agent/config.go`,
      v2.0.7): `extraConfig.server` can turn it on again, from false to true only, and `extraConfig.client` cannot
      turn a client's `true` off.

### Tests

13. **`internal/assets/assetstest`** serves Nomad's signed `SHA256SUMS` and tent's checksums to tests without a
    network. It imports only the standard library, so the tests of `internal/assets` can use it, and only tests import
    it (`assetstest-stdlib-only`, `secrettest-only-in-tests`; [ADR-0021](0021-import-rules.md)).

## Consequences

### Positive

- `update --yes` on an empty cloud builds a cluster with a leader, ACLs, healthy servers and registered clients, and a
  second `update --yes` plans nothing, makes no Nomad call and only reads the cloud.
- A run that stops anywhere is finished by the next one: one instance per node, the ACL system bootstrapped (once per
  Nomad cluster, with one or two `Bootstrap` calls), the mark written. The tests cut a run at every cloud call, every
  Nomad call and every state write.

### Negative / trade-offs

- **Closed by M2.7b** ([ADR-0032](0032-joined-label-scrub-and-delete-guard.md)):
  - Secrets stay in user data until the scrub. The scrub runs once a node has joined.
  - A ready machine that has not registered is not seen. The plan waits for every machine without the joined label.
  - A health wait that stops is not repeated. A server without the label is waited for, so the next plan has a
    Nomad step.
- **A renamed server group** has no machine that stays, so `update` deletes the mark, builds the new group as a new
  Nomad with its own bootstrap and deletes the old machines last.
- **A plan made before the CA is stored** issues a new CA in memory each time, so its spec hashes differ from plan to
  plan until the first `update --yes` has stored the CA.
- **A group within a few bytes of 24 KiB** can pass the plan and fail at its create with the same message, since
  the plan's certificate differs by a few bytes from the one the apply issues.
- **Clusters built by an older tent** have placeholder nodes and no mark. `--yes` fails after 10 minutes without a
  leader. Only the pre-releases v0.1.0-rc.1 and v0.1.0-rc.2 have shipped, so only test clusters are affected: delete
  such a cluster and create it again.

### Follow-ups

- **M2.7b** (built, [ADR-0032](0032-joined-label-scrub-and-delete-guard.md)): the scrub of user data once a node has
  joined; nodes that never registered, such as a client whose intro token expired; and decision 27 of [architecture
  §18](../architecture.md#18-open-questions): until M3, `update` refuses to delete a node that joined.
- **M2.8:** `tent export nomad` replaces `hack/tent-operator`, the tool that gives the real-cloud check its access to
  the Nomad API.
- **M3:** server removal through the API; the E2E check of ADR-0016 that a client rejoins after every server has been
  replaced.
- **The real-cloud check** passed on Vultr on 2026-10-05 (run `qypvsk`, [platform notes
  §3.16](../platform-notes.md#316-spike-runs)): three servers and two clients built in 453 s, a docker job, a second
  run with nothing to do, and a `delete cluster` that left nothing.

## Alternatives considered

- **Creates before waits, as in M1.** Rejected for the reason in decision 1.
- **Reading the assets in every plan.** Every drift check would need releases.hashicorp.com, also when no node is
  missing, and tests that only change the pinned version would need signed files.
- **A health wait before every client change**, also on a cluster that has the mark. A run that only adds clients asks
  for intro tokens directly, and a client that cannot register fails its own wait.
- **A retry loop around `Bootstrap` and `IntroToken`.** The next run repeats them.
- **A short operator certificate with renewal during the run.** It adds code for a run that outlasts a certificate
  that already lasts 24 hours.
- **A bootstrap call without the token header.** `nomadops` would need a second kind of client for one call, and Nomad
  accepts the header.
- **`SuccessExitStatus=1` on `nomad.service`**, to keep the unit green after a stop of a server. It would hide real
  failures too.
- **Intro tokens for combined nodes**, as ADR-0019 says. M3 can revisit it with server scaling.

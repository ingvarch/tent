# ADR-0033: Operator commands: validate, export nomad and ui

- **Status:** Accepted
- **Date:** 2026-10-07
- **Deciders:** ingvarch
- **Related:** amends [ADR-0007](0007-security-baseline.md), [ADR-0018](0018-vultr-provider-design.md),
  [ADR-0019](0019-combined-server-client-role.md), [ADR-0021](0021-import-rules.md),
  [ADR-0024](0024-cluster-pki-storage-and-certificates.md), [ADR-0030](0030-nomad-on-nodes.md),
  [ADR-0031](0031-bootstrap-in-update.md) and [ADR-0032](0032-joined-label-scrub-and-delete-guard.md) (see their
  Status lines); builds on [ADR-0023](0023-vultr-inventory-dedupe-and-images.md);
  [architecture §5](../architecture.md#5-repository-layout-and-dependency-rules),
  [§9.1](../architecture.md#91-pki), [§9.2](../architecture.md#92-acl-and-tokens),
  [§9.7](../architecture.md#97-operator-access),
  [§13.6](../architecture.md#136-tent-validate-cluster---wait-duration), [§14](../architecture.md#14-cli),
  [§17](../architecture.md#17-risks), [§18](../architecture.md#18-open-questions),
  [platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)

## Context

After M2.7 a cluster runs, and an operator has no way in. The stopgap `hack/tent-operator` wrote the ACL bootstrap
secret as the token ([ADR-0007](0007-security-baseline.md): humans never receive it) and took the server's address by
hand. Nothing checks a cluster against its specs, and no command warns about a combined cluster. M2.8 closes issues #94
and #98 with three commands, `tent export nomad`, `tent ui` and `tent validate cluster`, and a warning. It replaces
`hack/tent-operator`.

The facts that shaped it were measured on a local Nomad 2.0.7 on 2026-10-06, in its source at that tag and in a browser
([platform notes §1.2](../platform-notes.md#12-features-tent-relies-on)):

- **A token's TTL** lies between 1 minute and 24 hours, unless the servers' configuration changes the limits. Nomad
  answers an expired token differently by endpoint (500 `rpc error: ACL token expired` from `/v1/acl/token/self`, 403
  `Permission denied` from `/v1/jobs` and the autopilot report), so tent cannot tell an expired token from a missing
  right by Nomad's answer.
- **The `nomad` CLI** reads `NOMAD_ADDR`, `NOMAD_CACERT`, `NOMAD_CLIENT_CERT`, `NOMAD_CLIENT_KEY`,
  `NOMAD_TLS_SERVER_NAME` and `NOMAD_TOKEN`, and no file for the token. Against a server certificate that does not hold
  the server's address it fails without `NOMAD_TLS_SERVER_NAME`: the certificate of the run held DNS names only, and
  tent's holds no address but `127.0.0.1`.
- **The autopilot report** shows a killed server as `alive` for about 36 s, and `failed` was never seen.
- **Behind a proxy that adds mTLS and a management token,** the web UI works with no sign-in and stores the proxy's
  token in `localStorage`. Its websockets fail when `Origin` and `Host` differ. The `nomad` CLI works with
  `NOMAD_ADDR` alone.
- **Another web page can reach the proxy's port.** Nomad's CORS headers cover 14 endpoints, and a blind `no-cors` POST
  from a page of another port created a namespace even with `Origin` removed. A check of `Host`, `Origin` and
  `Sec-Fetch-Site` in the proxy stopped it.

## Decision

### Operator access (decisions 30 and 31)

1. **One use case makes the access for `export nomad` and `ui`.** It reads the state store (the specs, the four
   secrets, the bootstrap mark `nomad/bootstrapped`) and the cloud's list of the machines, and it makes one call to
   Nomad, `PUT /v1/acl/token`. It writes nothing and takes no lock.
2. **The token is a management token with a TTL** (maintainer decision 30). It is named `tent <purpose>
   <owner>@<host>`, such as `tent export nomad igor@laptop`, so an operator who lists tokens sees who owns each.
   Nomad keeps it; tent keeps neither its secret nor its accessor. The bootstrap secret and the CA key never leave the
   state store. tent owns no ACL policy yet, so a token bound to a policy comes when tent has one.
3. **The certificate** is `cli.<region>.nomad` with client authentication only and a new key, valid for the same TTL
   and never past the CA ([ADR-0024](0024-cluster-pki-storage-and-certificates.md)). It is made in memory and stored
   nowhere but the operator's files.
4. **tent does not check the TTL against Nomad's limits.** A server whose configuration raises them should work, and
   Nomad's refusal is clear. Only a TTL of zero or less fails first.
5. **`export nomad` writes four files** into one directory: `ca.pem`, `cli.pem`, `cli-key.pem` and `token`
   (maintainer decision 31). The default is `$XDG_CACHE_HOME/tent/<cluster>`, else `~/.cache/tent/<cluster>`, on every
   operating system: short-lived files, in a place that backups and dotfile repositories usually leave out. `--dir`
   changes it. A directory that tent makes has mode 0700, the files 0600. The four files are written to temporary
   names first and then renamed, so a running `nomad` reads a whole file, and a write that fails leaves the files of an
   earlier run as they were. The directory is made before the token is asked for, so a directory that cannot be made
   leaves no token. A directory that tent made is removed again when the access cannot be made.
6. **The printed lines set six variables, and the last reads the token file.** `NOMAD_TOKEN` is
   `"$(cat '<dir>/token')"` in sh and `(cat '<dir>/token')` in fish. A line with the secret itself would put a secret
   on stdout, where tent never prints one, and the CLI has no variable for a file. `NOMAD_ADDR` is the server that
   answered the token call, and `NOMAD_TLS_SERVER_NAME` is `server.<region>.nomad`. `--shell` picks `sh` or `fish`;
   without it, fish when `$SHELL` names fish (a design choice; PowerShell is a follow-up). With `-o json` or `-o yaml`
   the command prints the paths, the address and the end instead.
7. **`ui` makes its own token** for each run (purpose `ui`, 24 hours), so the bootstrap secret never reaches a browser.
8. **Safe to repeat.** Each run makes a new token and certificate and replaces the files. An earlier token stays valid
   until its end, and so does one whose answer was lost. `nomad acl token delete <accessor>` revokes one early; `export
   nomad` prints the accessor.
9. **The commands need the cloud's key** (`VULTR_API_KEY` on Vultr). tent stores no address of a machine, and a server's
   public address, which the calls to port 4646 need, comes from the cloud.

### The proxy (decision 29)

10. **`tent ui` serves a proxy on a loopback port,** `127.0.0.1:4646` unless `--listen` says otherwise. It passes each
    request to one of the servers over mTLS (TLS 1.2 or newer, the cluster's CA as the only root, the operator
    certificate, the name `server.<region>.nomad`), with `X-Nomad-Token` set to its token in place of any token the
    request carried. It drops `Origin` and `Authorization`, streams the answer as it comes, passes websocket upgrades
    through and never follows a redirect. A request that reaches no server gets 502, and the next request goes to the
    next server. So `verify_https_client` stays on, and the browser and the `nomad` CLI need neither certificate nor
    token.
11. **While `tent ui` runs, a request to the port acts with a management token.** So the proxy refuses what a web
    page can send to it:
    - `--listen` takes `localhost` or an address in `127.0.0.0/8` or `::1`, and nothing else;
    - a request whose `Host` is not the listener's own address, or `localhost` with its port, gets 403;
    - a request whose `Origin` is not that `Host` gets 403, and so does one whose `Sec-Fetch-Site` is not `same-origin`
      or `none`;
    - a request with neither header, such as the CLI's, passes.

    Dropping `Origin` is not enough: the blind POST above worked without it. The `Origin` is compared with the
    request's `Host`, so that `localhost` works. A link to the UI on a page of another site is refused as well, so the
    operator types or pastes the address.
12. **What stays open:** every process of every user on the operator's machine can use the port. `kubectl proxy` has
    the same property. A process of the operator's own user could also read the files of `export nomad`; a process of
    another user cannot (0700 and 0600), so on a shared machine the port gives other users what the files do not. The
    UI keeps the token in `localStorage` for `http://127.0.0.1:<port>` until it expires after 24 hours, and the token
    is of no use without a client certificate of the cluster's CA. Whoever reaches the port can read the token's secret
    and, with a management token, make tokens that do not expire; the end of the session undoes none of that. Two
    kinds of GET from a web page pass the checks, by the Fetch Metadata rules and not tried in a browser (architecture
    §9.7); the page cannot read their answers.
13. **The session** ends at Ctrl-C or when the operator's wall clock reaches the end of the access, read at least once
    a minute, since a timer does not count the time that the machine sleeps. tent then closes the listener, waits up to
    5 seconds for open requests and exits with 0. It does not delete the token, which expires (a delete would run
    after the context has ended). tent opens no browser and `ui` has no `--ttl`: opening a browser is code per
    operating system that CI cannot test.

### `validate cluster` (decision 32)

14. **The checks are in `internal/app`,** not in a package `internal/validate`, since they reuse the node planner of
    `update` (`planNodes`). Every node change that `update` would plan is a failure, so the two cannot disagree. A
    second count of machines per group could. `validate` does not run the whole plan of `update`, which reads release
    files, makes missing secrets in memory and fails on a joined node it would delete.
15. **Reads only.** The specs, validated as `update` validates them (a single server needs `--allow-single-server`),
    the stored secrets, the mark, the cloud's list and Nomad. No write, no lock; a held lock is told in a notice.
16. **The failure names are stable,** because `-o json` prints them; the list is in [architecture
    §13.6](../architecture.md#136-tent-validate-cluster---wait-duration). A client is told by its name and private
    address together ([ADR-0032](0032-joined-label-scrub-and-delete-guard.md)), a server by its private address
    in the Raft configuration and in autopilot's report.
17. **Nomad that does not answer is a failure, and a cloud that does not answer is an error.** So `--wait` can wait
    for a cluster that is still starting, and a run without credentials stops at once. A version is consistent when it
    equals the version that the cluster is pinned to, as text.
18. **A server whose Serf status is not `alive` is a failure,** whatever the status. Nomad 2.0.7 reported `left` for a
    killed server and never `failed`. The vote comes from the Raft configuration and health from the autopilot report.
    For about 36 s after a kill the report still shows the server as `alive`, and the Raft configuration keeps its vote
    until autopilot removes it, so a server that died seconds ago can read as valid.
19. **Certificates.** tent stores no node certificate, so the end is the machine's creation time plus one year
    (`pki.NodeCertificateEnd`). A certificate that ends within 30 days is a warning and one that has ended is a failure
    (a design choice). The same holds for the CA. Until M3 an operator cannot replace a node with tent, so a
    failure at 30 days would keep a CI job red for a month with no remedy.
20. **Warnings never change the exit code.** `validate` prints the warnings of every change (an open `access.api`, a
    combined group, an untested Nomad version), one for a cluster in one failure domain, and the certificate warnings.
    The failure domain comes from the model's zones, and Vultr always has one. The warning that a cluster has no host
    anti-affinity waits for `cloud.Capabilities` and Hetzner.
21. **Exit codes** (maintainer decision 32): 0 when the cluster is valid, 2 when it is not, with the table and no
    `Error:` line, and 1 when tent could not check. A script tells a broken cluster from a broken run. `--wait
    DURATION` checks every 10 seconds until the cluster is valid or the time has passed, and prints the last result.
22. **Not in `validate`:** a check of the infrastructure (firewalls and the network; `update --exit-code` reports them),
    the warning about `drain_on_shutdown` in `extraConfig` ([ADR-0030](0030-nomad-on-nodes.md)), which needs an HCL
    parser in code that tent runs, a report of nodes with an outdated spec hash (M3) and `tent get nodes`.

### Which commands warn

23. **The commands that print the open-`access.api` warning print the combined warning too:** `create`, `replace` and a
    saved `edit` after the change, `update cluster --yes` before its first change, `create --yes` once, and
    `validate cluster` on every run. A plan without `--yes`, `delete cluster`, `get`, `state unlock`, `export nomad` and
    `ui` do not warn: they leave no changed cluster behind. This reads "every mutating command" of
    [ADR-0007](0007-security-baseline.md) and [ADR-0019](0019-combined-server-client-role.md) as architecture §14 does:
    tent warns about the cluster that results from a change.

## Consequences

### Positive

- An operator reaches the Nomad API and the UI without the bootstrap secret or the CA key, and `eval "$(tent export
  nomad prod)"` is all that the `nomad` CLI needs.
- `validate` and `update` agree on the machines (item 14).
- `hack/tent-operator` and its copy of the shell lines are gone. `internal/shellenv` replaces `hack/internal/shellenv`,
  since tent now prints shell lines and `cmd/tent` cannot import a package under `hack/internal`
  ([ADR-0021](0021-import-rules.md)).

### Negative / trade-offs

- Every local process can use the proxy's port while `tent ui` runs.
- Tokens of earlier exports and of lost answers live until they expire, and a clock set back on the operator's machine
  delays the end of a `tent ui` session.
- The end of a node certificate and a lagging autopilot report make `validate` approximate.
- On Windows the modes of the directory and the files mean nothing: they get the access rules that the directory
  inherits, so a `--dir` outside the user's profile may be readable by others (not tested on Windows).
- **`--wait` makes new Nomad clients each round.** A round leaves one idle connection to the server it asked, closed
  after 90 seconds: about nine at once, under Nomad's limit of 100 from one address.
- **The UI's log view first calls the node's own address from the browser,** which fails, and falls back to the server
  through the proxy after a delay. Not tested: whether the UI falls back to polling when a websocket fails. A page left
  open across a restart of `tent ui` keeps the old token until it reloads; not tested.

### The real-cloud check

The check of this part ran on Vultr on 2026-10-07 (spike version 12, run `9pxbqn`, [platform notes
§3.16](../platform-notes.md#316-spike-runs)). The three commands worked against a cluster of three servers and two
clients that tent built: the exported certificate and token, `validate` on the cluster as built and with Nomad stopped
on a client, and `tent ui` from curl, a browser and the `nomad` CLI, an exec included. One row read as unexpected, and
the script was wrong there: it counted a hint that the `nomad` CLI prints on stderr as a fourth server.

### Follow-ups

- **M2.9:** the E2E `smoke` reaches Nomad through `tent export nomad` and ends with `tent validate cluster --wait`.
- **M3:** drain and server removal lift the guard of
  [ADR-0032](0032-joined-label-scrub-and-delete-guard.md); `rolling-update` reports nodes with an outdated spec hash.
- **Later:** PowerShell lines (until then Windows users take `-o json`); revoking an exported token; scoped
  operator policies; `tent get nodes`; the warning about `drain_on_shutdown`.

## Alternatives considered

- **A one-time token for `tent ui`** (secret on the terminal, good for one use within 10 minutes), with mTLS only in the
  proxy. The `nomad` CLI through the proxy would then need `NOMAD_TOKEN`, and the UI still ends up holding a token.
- **Leaving `tent ui` out.** The browser UI needs a certificate and a token otherwise.
- **The bootstrap secret as the exported token,** as `hack/tent-operator` did. ADR-0007 forbids it.
- **A token bound to a policy that tent keeps in step.** tent owns no policy yet.
- **`~/.config/tent/clusters/<cluster>`** as the default directory. `config.yaml` there says it never holds secrets.
  **No default** (`--dir` required): every run would need the path.
- **A line `export NOMAD_TOKEN=<secret>`.** It breaks the rule that tent never prints a secret.
- **Exit code 1 for both a broken cluster and a broken run.** A script could not tell them apart.
- **A certificate that ends within 30 days as a failure.** See item 19.
- **A second count of machines per group in `validate`.** It could disagree with `update`.
- **An exact certificate end from a TLS handshake.** It works for servers only, since a client's API is not reachable
  from outside.
- **Checking `--ttl` in tent.** See item 4.
- **Deleting the token when `tent ui` ends.** One more call that runs after the context has ended; the token expires.
- **Opening the browser from `tent ui`.** See item 13.

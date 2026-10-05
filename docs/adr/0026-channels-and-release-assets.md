# ADR-0026: Channels and release assets

- **Status:** Accepted; its M2.3 follow-ups moved to M2.7 by
  [ADR-0027](0027-nodeconfig-contract-rendering-and-spec-hash.md); amended by
  [ADR-0028](0028-tent-node-agent-units-and-delivery.md) (the M2.5 follow-up is built: the release lists the
  tent-node binaries in `checksums.txt`, and development builds upload to the CI R2 bucket; the one URL of a
  development build serves an amd64 binary, so its clusters need amd64 plans) and by
  [ADR-0031](0031-bootstrap-in-update.md) (the follow-ups moved to M2.7 are built:
  `update` reads the assets only in a plan that creates or waits for a node, once per architecture and once per run,
  with its own clock for the signature; the Nomad pin is written with the completed spec before the first node,
  so a first `update` that is cut and run again by a newer tent keeps the first run's pin; the architecture comes
  from `cloud.Provider.Arch`; a development build needs
  `TENT_NODE_URL` and `TENT_NODE_SHA256` for such a plan, which `cmd/tent` reads; the warning for a release build
  shows before the first change)
- **Date:** 2026-09-28
- **Deciders:** ingvarch
- **Related:** extends [ADR-0006](0006-two-binaries-and-nodeconfig.md),
  [ADR-0011](0011-nomad-only-scope-and-licensing.md) and [ADR-0021](0021-import-rules.md);
  [ADR-0013](0013-technology-stack.md), [ADR-0020](0020-release-channels-and-ci-conventions.md),
  [ADR-0023](0023-vultr-inventory-dedupe-and-images.md), [ADR-0025](0025-stdlib-only-helper-packages.md),
  [architecture §3.3](../architecture.md#33-api-rules), [§8.5](../architecture.md#85-artifacts-and-verification),
  [§13.2](../architecture.md#132-tent-update-cluster---yes),
  [§13.5](../architecture.md#135-tent-upgrade-cluster---yes), [§18](../architecture.md#18-open-questions)

## Context

M2.2 decides which Nomad and CNI versions a cluster runs and finds the files that nodes download, with the sha256
each must have. Nothing downloads on nodes yet: NodeConfig takes the files in M2.3.

- Nodes check only the sha256s that NodeConfig carries. The CLI establishes trust, so nodes need no PGP
  ([ADR-0006](0006-two-binaries-and-nodeconfig.md)).
- Architecture §13.5 planned channels as files embedded in tent. Each would hold the recommended and supported Nomad
  versions, the CNI version and images per provider, and only channel-tested version pairs would be allowed.
- Nomad ships fixes monthly ([platform notes §1.1](../platform-notes.md#11-releases-and-support-)). If tent allowed
  only the versions it tested, each Nomad patch would wait for a tent release.
- Nomad's `SHA256SUMS` is signed with HashiCorp's release key, which expires on 2030-03-01
  ([platform notes §1.4](../platform-notes.md#14-downloads-and-verification)). A CNI plugins release has a `.sha256`
  file next to each archive and no signature. tent's own release has `checksums.txt`, signed keyless with cosign
  ([ADR-0020](0020-release-channels-and-ci-conventions.md)).
- A development build of tent has no release that holds its tent-node. ADR-0006 names `TENT_NODE_URL` for it.
- Images already have a home: the provider's table ([ADR-0023](0023-vultr-inventory-dedupe-and-images.md)) and the
  API default (maintainer decision 4).

## Decision

### Channels

1. **The channel file.** tent embeds its channels in `internal/channels`, one YAML file each. `stable` is the only
   one. The file is decoded strictly: unknown fields, keys in the wrong case and duplicate keys fail. It holds Nomad
   and the CNI plugins only, no images (maintainer decision 11):

   ```yaml
   name: stable
   nomad:
     minimum: 2.0.0        # the oldest version a cluster may run
     recommended: 2.0.7    # what a new cluster runs
     tested: [2.0.7]       # the versions tested with this tent
   cni:
     version: 1.9.1
     sha256:               # of cni-plugins-linux-<arch>-v1.9.1.tgz
       amd64: b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303
       arm64: 56171987d3947707c3563db2f4001bccaf50fd63468611b9f3cbecb1375ee7ec
   ```

2. **The version rule** (maintainer decision 10, as revised on 2026-09-28). A cluster may run any official Nomad
   release `X.Y.Z` from the channel's minimum up to, not including, the next major version: from 2.0.0 up to 3.0.0
   for `stable`.
   - A version outside that range, or not of that form (a `v`, a pre-release or a build suffix), is a field error on
     `spec.nomad.version`. So is an unknown `spec.channel`, on that field. `create`, `replace`, `edit` and `update`
     check both.
   - A version that the channel allows but does not list as tested passes with a warning:
     `WARNING: Nomad 2.0.8 is not tested by this tent; channel stable tests 2.0.7`.
   - tent does not check that the version was released. Its signed `SHA256SUMS` proves that when tent fetches the
     assets (decision 5).
   - This relaxes architecture §13.5, which allowed only channel-tested version pairs.
3. **The pin.** When the spec leaves `spec.nomad.version` empty, `update` takes the version of the stored
   `cluster.completed.yaml`, else the channel's recommended one, and writes it into the completed spec. A newer tent
   that recommends another version does not move the cluster; `upgrade cluster` does. A pinned version that the
   channel does not allow fails the plan, and the error says to set `spec.nomad.version` to one it allows. A version
   in the spec wins over the pin.

### Assets

4. **An asset** is a name, a version, the URLs to download it from (one for now; NodeConfig adds mirrors) and a
   sha256. `internal/assets` reads each file with one request, which the caller's context bounds. Errors name the URL.
   Nothing is cached.
5. **Nomad is checked at run time** (maintainer decision 9). The CLI downloads `nomad_<v>_SHA256SUMS` and
   `nomad_<v>_SHA256SUMS.sig` from releases.hashicorp.com and checks the signature with HashiCorp's release key, which
   tent embeds (`internal/assets/hashicorp.asc`). Then it takes the line for `nomad_<v>_linux_<arch>.zip`.
   - Only signatures made with SHA-256, SHA-384 or SHA-512 count.
   - The key is checked at the current time. The embedded key and its signing subkey expire on 2030-03-01. From then
     on tent cannot verify any Nomad download, older releases included, and says that a newer tent, with the renewed
     key, is needed.
   - A CI job, `online`, runs weekly and can also be run by hand; a manual run of `ci.yml` starts only this job. It
     checks that the recommended version of `stable` still verifies, and fails from 180 days before the key expires
     (2029-09-02), so the maintainer embeds the renewed key in time.
   - A revocation by HashiCorp reaches tent only with a new embedded copy of the key.
6. **The CNI plugins' sha256s are in the channel.** The archive is
   `https://github.com/containernetworking/plugins/releases/download/v<v>/cni-plugins-linux-<arch>-v<v>.tgz`, and
   its sha256 is the one the channel holds for the architecture.
7. **tent-node comes from tent's own release.**
   - A release or a pre-release of tent, such as `v0.3.0` or `v0.3.0-rc.1`, reads `checksums.txt` of its own tag
     over TLS and takes the line for `tent-node_linux_<arch>`. Its cosign signature is not checked.
   - Any other version is a development build: `dev`, snapshots, and also `git describe` output such as
     `v0.3.0-4-gabc1234` and `-dirty` builds. Their tent-node is not the one that the tag released, and a node must
     run the CLI's own (ADR-0006). The version guard counts `git describe` output as its tag
     ([architecture §10.2](../architecture.md#102-layout)); both rules live in `internal/buildinfo` (`Release`,
     `IsRelease`).
   - A development build needs `TENT_NODE_URL` and `TENT_NODE_SHA256`, and without them it fails and names them. One
     URL and one sha256 serve every architecture. A release build ignores them.

### Import rules

8. Three depguard rules extend [ADR-0021](0021-import-rules.md), as [ADR-0025](0025-stdlib-only-helper-packages.md)
   did:
   - `gocrypto-only-in-assets`: only `internal/assets` imports `github.com/ProtonMail/go-crypto`, tests included.
   - `node-no-assets`: `internal/nodeup`, `internal/nodeconfig` and `cmd/tent-node`, tests included, import neither
     `internal/assets` nor `internal/channels`. tent-node gets its versions and sha256s in NodeConfig and carries no
     PGP code (ADR-0006).
   - `channels-stdlib-and-yaml`: `internal/channels` imports only the standard library, the decoder of the specs
     (`sigs.k8s.io/yaml` and `sigs.k8s.io/json`) and `golang.org/x/mod/semver`. Its tests are exempt.

## Consequences

### Positive

- A new Nomad patch runs without a new tent. A tent release only moves the recommended and the tested versions.
- Upgrading tent never changes the Nomad version of a running cluster.
- Nomad's sha256s are as trustworthy as HashiCorp's signature, and nodes carry no PGP code.
- A development build's nodes run the tent-node built from its own commit.

### Negative / trade-offs

- A cluster may run a Nomad version that tent has not tested. The warning says so on every change.
- The pin is written with the completed spec. Before M2.7a that came only after the first `update` had succeeded, so a
  first `update` that was cut and then run again by a newer tent pinned that tent's recommendation.
- A version in the spec below the pin is accepted: nothing refuses a downgrade yet.
- Once M2.3 uses the assets, a plan needs releases.hashicorp.com, and for a release build github.com.
- From 2030-03-01, every tent that embeds the current key fails to verify Nomad, and operators must upgrade tent. A
  key that HashiCorp rotates earlier needs a tent release too, and a revoked key still verifies in each tent that
  embeds it.
- A new CNI release needs a tent release.
- `checksums.txt` is trusted over TLS. Whoever can change tent's GitHub release can give nodes another tent-node.
- A developer uploads tent-node and sets two variables before building a cluster.

### Follow-ups

- M2.3: NodeConfig carries the assets and `update` fetches them. The caller warns when `TENT_NODE_URL` or
  `TENT_NODE_SHA256` is set on a release build. Once nodes run Nomad, `update` writes the pin before it creates the
  first node, with the secrets.
- M2.5: the release adds `tent-node_linux_amd64` and `tent-node_linux_arm64` to `checksums.txt`, and development
  builds get a place to upload tent-node.
- M3: `upgrade cluster` moves a pinned version that the channel no longer allows and brings the URL that overrides an
  embedded channel. It and `rolling-update` refuse downgrades
  ([architecture §13.5](../architecture.md#135-tent-upgrade-cluster---yes)).
- By 2029-09-02: embed HashiCorp's renewed key.

## Alternatives considered

- **Only tested versions, as §13.5 planned.** Every Nomad patch, security fixes included, would wait for a tent
  release.
- **Nomad's sha256s in the channel.** Each Nomad release would need a tent release to add its sums, which the version
  rule avoids. HashiCorp's signature proves the sums at run time.
- **CNI's `.sha256` files read at run time.** The file sits in the same release as the archive, so whoever can replace
  the archive can replace the sum. A sha256 fixed and reviewed when tent is released is stronger.
- **Checking the cosign signature of `checksums.txt`.** It would add sigstore's client and trust roots to tent to
  check a file of tent's own release, which tent reads over TLS.
- **The release rule in `internal/statestore`,** where the version guard kept it. `internal/assets` would import the
  state store for a string rule. `internal/buildinfo` owns the version, so the rule moved there, and the guard and the
  assets share it.

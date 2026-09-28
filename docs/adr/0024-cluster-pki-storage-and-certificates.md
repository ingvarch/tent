# ADR-0024: Cluster PKI storage and certificate details

- **Status:** Accepted
- **Date:** 2026-09-28
- **Deciders:** ingvarch
- **Related:** amends [ADR-0007](0007-security-baseline.md); [ADR-0010](0010-state-store-and-locking.md),
  [ADR-0019](0019-combined-server-client-role.md), [architecture §9.1](../architecture.md#91-pki),
  [§13.2](../architecture.md#132-tent-update-cluster---yes),
  [§13.7](../architecture.md#137-tent-delete-cluster---yes), [§18](../architecture.md#18-open-questions)

## Context

M2.1 builds the cluster's CA, gossip key and ACL bootstrap secret (`internal/pki`), and `update` keeps them in the
state store. [ADR-0007](0007-security-baseline.md) fixes the PKI but leaves details open or states them loosely:

- It stores the CA "as a bundle (`ca-bundle.pem` plus the active signer id)", but says neither where the id lives nor
  what keeps it in step with the key.
- It sets no CA validity. Its context cites the 5-year CA of `nomad tls`.
- It says that certificates contain no IP addresses, and also that node certificates carry `127.0.0.1`.
- It does not say what tent does when a stored secret is missing or malformed.
- A run can stop between any two writes, and a delete between any two deletes. The CA's key and bundle must never be
  left in a state that tent cannot repair.

## Decision

1. **Active signer.** `pki/ca-bundle.pem` holds one or more CA certificates, and `pki/private/ca.key` the key of one
   of them. The active signer is the first certificate of the bundle whose public key matches the key. Its id is that
   certificate's Subject Key Identifier in hex. No id is stored. Rotation still needs no format change: add the new
   certificate to the bundle, then switch the key.
2. **CA validity.** 10 years, until CA rotation exists (maintainer decision 8 of 2026-09-28).
3. **Backdate.** Every certificate, the CA's included, starts 5 minutes before it is made, for clock skew. A leaf
   certificate never ends after the active signer.
4. **Encoding.** Keys are PKCS#8 PEM (`PRIVATE KEY`), and tent loads no other form. A bundle or a key with anything
   but white space after its PEM is refused.
5. **Leaf common name.** A leaf certificate's CN is its first DNS name: `server.<region>.nomad` for a server or a
   combined node, `client.<region>.nomad` for a client, `cli.<region>.nomad` for an operator.
6. **Addresses.** "No IP addresses" in ADR-0007 means no node addresses. Node certificates carry `127.0.0.1` for local
   calls, next to `localhost`. Operator certificates carry no IP address.
7. **Checked, never replaced.** Each plan of `update` loads the stored CA, and checks that the stored gossip key is
   standard base64 of 32 bytes, without line breaks, and the ACL bootstrap secret a lower-case UUID of version 4.
   - A CA key without its bundle gets a new CA certificate for that key.
   - A bundle without its key, a key that matches no certificate of the bundle, or a malformed secret fails the plan,
     and the error names the paths. tent never replaces them.
8. **Order.** `update` writes `pki/private/ca.key`, `pki/ca-bundle.pem`, `secrets/gossip.key` and
   `secrets/acl-bootstrap-token` in that order, with `IfNoneMatch` where the store has conditional puts.
   `delete cluster` removes them in the reverse order, before `cluster.yaml` and `tent-version`. A cut update or a cut
   delete can leave a key without its bundle, which the next `update` completes, but never a bundle without its key.

## Consequences

### Positive

- The key and the bundle cannot disagree about which certificate signs: the key picks it.
- A cut update or delete never leaves the CA in a state that tent cannot repair.
- Nodes whose clocks are a few minutes behind accept new certificates.
- A lost or broken secret stops tent with an error, instead of a new CA that cuts every node off.

### Negative / trade-offs

- tent cannot recover a lost CA key. The operator restores it from a backup of the store, such as an older version
  in a versioned bucket ([ADR-0010](0010-state-store-and-locking.md)).
- A CA key that leaks stays valid for up to 10 years, until rotation exists.
- On a store without conditional puts, the lock is best effort
  ([architecture §10.4](../architecture.md#104-locking)). Two `update --yes` runs that both hold it can interleave
  their plain puts and leave a key and a bundle of different CAs. Every later plan then fails with `CA key: matches
  no certificate of the CA bundle`, and tent never repairs it. While no node trusts the CA yet, delete both objects
  by hand and run `update` again.

### Follow-ups

- CA rotation: add the new certificate to the bundle, roll the nodes so that they trust both, switch the key, then
  remove the old certificate.

## Alternatives considered

- **An id file next to the bundle** (ADR-0007's wording). It is a third object that can disagree with the key, and
  one more write that a run can be cut before. The key already names its certificate.
- **A 5-year CA, as `nomad tls` makes.** Without rotation, a cluster would have to be rebuilt after 5 years.
- **Make a new CA when the stored one is broken or missing.** Every node that trusts the old CA would lose the
  cluster, so a mistake in the store would become an outage.

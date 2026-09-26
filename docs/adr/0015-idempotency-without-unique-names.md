# ADR-0015: Idempotent creation on clouds without unique names

- **Status:** Accepted
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** extends [ADR-0003](0003-cloud-is-source-of-truth.md); [ADR-0010](0010-state-store-and-locking.md),
  [ADR-0018](0018-vultr-provider-design.md), [architecture §6](../architecture.md#6-reconciliation-engine)

## Context

[ADR-0003](0003-cloud-is-source-of-truth.md) makes creation idempotent on Hetzner through deterministic, unique names.
A create retried after a timeout returns `uniqueness_error`, and tent then adopts the existing resource if its labels
match.

Vultr has no such guard:

- **No uniqueness.** Labels and names are not unique, and create calls have no idempotency key.
- **SDK retries.** govultr, built on go-retryablehttp, retries **every** method, POST included, on 429, 5xx and
  connection errors. A retried create can therefore produce a second VM.
- **Limited markers.** Tags exist only on instances. Other resources have one free-text field (description, label or
  name), and only `GET /v2/instances` can filter by tag on the server side.
- **No cloud-native lock.** The Hetzner lock (a firewall with a unique name, [ADR-0010](0010-state-store-and-locking.md))
  does not work where names are not unique.

## Decision

- **Capability `UniqueNames`.** Providers declare it.
  - Where it is true (Hetzner), [ADR-0003](0003-cloud-is-source-of-truth.md) applies as written.
  - Where it is false (Vultr), the rules below apply.
- **No blind SDK retries of non-idempotent calls.** The provider client disables automatic retries for create (POST)
  calls. tent's own retry policy knows which calls are idempotent. GET, DELETE and PATCH may be retried.
- **Operation ids.** Before a create call, tent generates an operation id (a UUID) and attaches it to the resource.
  - Instances carry it as the canonical label `tent/op` in their tags.
  - Other resources carry it inside their ownership marker (`…;op=<uuid>`).

  After an ambiguous failure (timeout, 5xx, connection reset, a response lost in transit), tent searches for the
  operation id:
  - **found:** adopt the resource and continue;
  - **not found:** create again with the **same** operation id.
- **Names stay deterministic** (`<cluster>-<group>-<index>`), but only as readable handles. They are never relied
  upon for uniqueness.
- **Dedupe pass.** During inventory, several owned resources with the same deterministic name and kind are
  duplicates.
  - tent keeps the one whose operation id matches the plan, otherwise the oldest.
  - It deletes the others.
  - An instance that has already registered in Nomad is never deleted by dedupe. It is reported instead.
- **Locking.** The cluster lock lives in the state store: a conditional put or `flock`
  ([ADR-0010](0010-state-store-and-locking.md), architecture §10.4). Clouds without unique names offer no
  cloud-native mutex.
- **Deletes are idempotent.** A 404 on delete counts as success.

## Consequences

### Positive

- Safe retries on any cloud, including under the recurring Vultr API incidents.
- The same code path serves future providers without unique names.

### Negative / trade-offs

- An ambiguous failure costs extra list calls. That is cheap at Vultr's 30 requests/s.
- Operation ids take up tag slots on instances. They may be removed after success with a tags PATCH, which is
  optional.
- Duplicates are still possible if the cloud's list endpoint is eventually consistent right after a lost create.
  The dedupe pass is the safety net.

### Follow-ups

- The label codec and operation-id helpers in `internal/cloud/vultr` (M1).
- Fake-provider tests that drop create responses and assert that exactly one resource exists afterwards (M1).

## Alternatives considered

- **Rely on the cluster lock alone.** It prevents concurrent operators but not a single operator's retried create.
- **Check by name before every create.** The retry's lookup can still race a create whose response was lost. Names
  are not unique, so a match proves nothing anyway.
- **No retries at all.** Too fragile given recurring deploy and API incidents.

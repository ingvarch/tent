# ADR-0025: Standard-library-only helper packages

- **Status:** Accepted
- **Date:** 2026-09-28
- **Deciders:** ingvarch
- **Related:** extends [ADR-0021](0021-import-rules.md); [ADR-0006](0006-two-binaries-and-nodeconfig.md),
  [ADR-0024](0024-cluster-pki-storage-and-certificates.md),
  [architecture §5](../architecture.md#5-repository-layout-and-dependency-rules)

## Context

- [ADR-0021](0021-import-rules.md) names the packages that may import providers, cloud SDKs and Nomad. depguard checks
  direct imports only, so a helper package that imports one of them carries it into every package that uses the
  helper.
- M2.1 adds small packages that many layers share:
  - `internal/pki` makes the CA, the certificates, the gossip key and the ACL bootstrap secret;
  - `internal/uuid` makes the UUIDs of operation ids and of the ACL bootstrap secret;
  - `internal/english` writes lists in messages, such as "a, b and c";
  - `internal/secrettest` lets tests look for a secret in what tent prints or logs.

## Decision

One depguard rule per package allows only these imports:

- `internal/pki`: the standard library, `internal/uuid` and `api/v1alpha1`;
- `internal/uuid`, `internal/english` and `internal/secrettest`: the standard library.

The allow entries for tent packages match the import path exactly (`…/internal/uuid$`, `…/api/v1alpha1$`), so a
subpackage needs an entry of its own. Tests are exempt. The rules sit next to those of ADR-0021.

`internal/secrettest` is for tests only: a further rule denies it to every file that is not a test, so `testing`
never reaches a binary.

## Consequences

### Positive

- Both binaries, `tent` and `tent-node` ([ADR-0006](0006-two-binaries-and-nodeconfig.md)), and every layer can use
  these packages without pulling in providers, cloud SDKs or Nomad.
- The code that makes the CA and the secrets cannot reach a cloud or the state store. Only `internal/app` stores what
  it makes.

### Negative / trade-offs

- Each new helper package needs a rule of its own.
- A helper that needs another tent package needs a change to its rule.

## Alternatives considered

- **No rule for helpers.** A helper could import a provider, and the core would reach the provider through it, the
  gap that ADR-0021 closes.
- **One rule for all helpers.** `internal/pki` needs `internal/uuid` and `api/v1alpha1` and the others need nothing,
  so one list would allow each package more than it needs.
- **Prefix matches in the allow list.** A new subpackage of an allowed package would be allowed without review.

# ADR-0001: Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-09-25
- **Deciders:** ingvarch
- **Related:** [ADR index](README.md), [architecture](../architecture.md)

## Context

tent is design-heavy. It has several long-lived decisions (state model, provider abstraction, security model,
bootstrap protocol) that are expensive to reverse once clusters exist in the wild. Development will alternate
between the maintainer and AI coding assistants that start every session without the history of the discussion.
A decision that lives only in a chat transcript will be forgotten or re-litigated.

## Decision

- We record every significant architectural decision as an ADR in `docs/adr/`, using the template in
  [template.md](template.md).
- `docs/architecture.md` is the current design. ADRs are the reasoning behind it. `docs/platform-notes.md` holds the
  verified external facts, with dates and sources.
- Accepted ADRs are immutable. Changes happen through superseding ADRs.
- A change to the code that contradicts an accepted ADR must come with a new ADR in the same pull request.

## Consequences

### Positive

- New contributors, human or AI, can recover the "why" without the original conversation.
- Reversals become explicit and reviewable.

### Negative / trade-offs

- A little overhead per decision. We accept it for decisions that are hard to reverse. Small, local choices do not
  need an ADR.

## Alternatives considered

- **Design doc only.** It captures the "what" but loses the rejected alternatives and the reasoning, which is
  exactly what gets re-litigated.
- **Wiki or issue tracker.** It lives outside the repository, drifts from the code and is not reviewed together with
  it.

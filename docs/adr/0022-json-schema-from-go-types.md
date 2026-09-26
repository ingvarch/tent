# ADR-0022: Generate the JSON Schema from the Go types

- **Status:** Accepted
- **Date:** 2026-09-26
- **Deciders:** ingvarch
- **Related:** [ADR-0013](0013-technology-stack.md), [ADR-0021](0021-import-rules.md),
  [architecture §3.3](../architecture.md#33-api-rules)

## Context

- [Architecture §3.3](../architecture.md#33-api-rules) asks for a JSON Schema generated from the Go types in
  `api/v1alpha1`, so editors can check and autocomplete spec files.
- [ADR-0013](0013-technology-stack.md) names no schema generator.
- The maintainer asked for the option with the least code and upkeep.
- The types already hold what a schema needs: JSON tags, a doc comment on every type and field, and the allowed values
  in `Roles()`, `Providers()` and `ClientIntroductions()`.
- Spec decoding must be strict: `machinetype` is not `machineType`. `encoding/json` and
  `sigs.k8s.io/yaml.UnmarshalStrict` match keys case-insensitively.

## Decision

1. **Generator.** `internal/apischema` builds one draft 2020-12 JSON Schema for both kinds with
   `github.com/invopop/jsonschema`.
   - Descriptions are the full doc comments of the types and fields in `api/v1alpha1`.
   - Enums come from `Roles()`, `Providers()` and `ClientIntroductions()`.
   - Each kind fixes `kind` and `apiVersion` with `const`.
   - `$id` is `https://raw.githubusercontent.com/ingvarch/tent/main/api/v1alpha1/tent.schema.json`, the URL editors
     load it from.
   - `api/` keeps importing only the standard library ([ADR-0021](0021-import-rules.md)). Its only reference to the
     generator is the `//go:generate` comment in `api/v1alpha1/doc.go`.
2. **One committed file.** `make generate` rewrites `api/v1alpha1/tent.schema.json`. A test fails when the committed
   file differs from what the generator writes. Tests check the spec examples against the schema with
   `github.com/santhosh-tekuri/jsonschema/v6`, a test-only dependency.
3. **Spec files.** `internal/spec` reads and writes multi-document YAML:
   - `go.yaml.in/yaml/v3` splits the documents, gives the line of each key and writes output in struct field order;
   - `sigs.k8s.io/yaml.YAMLToJSONStrict` converts each document to JSON;
   - `sigs.k8s.io/json.UnmarshalStrict` decodes the JSON case-sensitively and rejects unknown and duplicate fields.

   Errors name the document and the line in the file.

## Consequences

### Positive

- The generator and its command are about 140 lines of Go. A new field, or a new value in one of the enum lists,
  reaches the schema with `make generate`.
- Editors report unknown fields, wrong types and unknown enum values while the operator types.

### Negative / trade-offs

- invopop/jsonschema brings a few transitive modules (`pb33f/ordered-map/v2`, `bahlo/generic-list-go`,
  `buger/jsonparser`, `go.yaml.in/yaml/v4`), and the validator brings `golang.org/x/text`. Only the generator and
  the tests use them; none is linked into `tent`.
- Schema descriptions are the doc comments, so comments in `api/` are user-facing text, and changing one means
  running `make generate`.
- The schema is looser than `Validate`. It checks types, required fields, enums, the `kind` and `apiVersion`
  constants and unknown fields, but not name or CIDR patterns, sizes or provider rules. A spec the editor accepts
  can still fail tent's validation.
- In two places the schema is stricter than tent. It requires `size` in every node group, while tent reads a missing
  `size` as 0, which is valid for a client group; writing the size out keeps specs readable. It also rejects
  `clientIntroduction: ""`, which tent treats as left out.
- The `$id` points at `main`, so editors check a spec against the newest types, which can be ahead of the operator's
  tent release.

### Follow-ups

- M0's spec commands read and write spec files through `internal/spec`.

## Alternatives considered

- **`github.com/google/jsonschema-go`.** It has fewer dependencies, but descriptions come from `jsonschema` struct
  tags, which would repeat the doc comments, and enums are set by hand.
- **`github.com/swaggest/jsonschema-go`.** Descriptions come from `description` struct tags and enums from an
  `Enum()` method on each type, so both would be written into `api/` a second time.
- **A generator of our own.** Reading doc comments and walking the types with `reflect` is code we would write and
  maintain, which is what the maintainer wanted to avoid.

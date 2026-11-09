# AGENTS.md

tent is a Go CLI that builds and operates HashiCorp Nomad clusters on cloud providers: kops for Nomad. The module path
is `github.com/ingvarch/tent`. `cmd/tent` is the CLI over the use cases in `internal/app`; `cmd/tent-node` is the
agent on each node; `api/v1alpha1` holds the public spec types. Vultr is the first provider and runs the E2E suite,
Hetzner Cloud is second, and AWS comes later and must stay possible without changes to the core. `docs/architecture.md`
is the source of truth: a change that diverges from it or from an accepted ADR needs a superseding ADR first.

- [docs/architecture.md](docs/architecture.md): read before any change to code. It has the domain model and API, the
  reconciliation engine, the provider abstraction, tent-node and NodeConfig, security, the state store and locks, the
  lifecycle flows, the CLI, testing, and the open questions (§18).
- [docs/adr/](docs/adr/README.md): read before a design decision or a change in an area an ADR covers. The README has
  the index and the rules for adding or superseding an ADR.
- [docs/platform-notes.md](docs/platform-notes.md): read before relying on a behaviour of Nomad, Vultr, Hetzner Cloud,
  an S3-compatible store or Ubuntu on a node. Its opening note explains the markers of volatile and unverified items.
- [docs/roadmap.md](docs/roadmap.md): read before starting a milestone or a part of one. It has the goals and exit
  criteria, the status, the table of maintainer decisions and the log.
- [docs/development.md](docs/development.md): read before building, testing, updating golden files, making a
  development build, running the E2E suite or a release. It has the tools and their versions and the project structure.
- [docs/features.md](docs/features.md) and [docs/quickstart.md](docs/quickstart.md): read before changing what a
  command does or prints, and update them with it.
- [CONTRIBUTING.md](CONTRIBUTING.md): read before a commit or a pull request. It has the branch names, the commit
  types and scopes, and the steps of a change.
- [test/e2e/README.md](test/e2e/README.md): read before changing or running the E2E suite.
- [hack/vultr-spike/README.md](hack/vultr-spike/README.md): read before running the spike or recording its findings.

## Rules

- Everything committed is in English: code, comments, log messages, CLI output, docs and commit messages.
- Go 1.26, because `github.com/hashicorp/nomad/api` requires it. Standard library first, `log/slog` for logging,
  errors wrapped with context (`fmt.Errorf("...: %w", err)`), no panics in library code.
- Imports follow the depguard rules in `.golangci.yml`, which give the reason for each rule
  ([ADR-0021](docs/adr/0021-import-rules.md)). Never import the root module `github.com/hashicorp/nomad`: it is
  BUSL-licensed.
- Everything is under `internal/` except the public API types in `api/` and the E2E suite's packages under
  `test/e2e/`.
- Core mechanisms assume the weakest cloud primitives: non-unique names, no fixed IPs, no graceful shutdown. Richer
  primitives are optimizations behind `Capabilities` (ADR-0015 to ADR-0017).
- Cloud credentials (`VULTR_API_KEY`, `HCLOUD_TOKEN`) never go into specs, the state store, logs or nodes.
- Every mutating operation is safe to re-run after an interruption
  ([ADR-0015](docs/adr/0015-idempotency-without-unique-names.md)):
  - deterministic names;
  - adopt a resource only when its ownership markers match;
  - operation ids where names are not unique; never let an SDK blindly retry a create;
  - create the replacement before removing the old node, except a client that never registered: it is deleted before
    its replacement is created ([ADR-0032](docs/adr/0032-joined-label-scrub-and-delete-guard.md)).
- Every engine task needs an "apply, re-plan, no-op" test.
- The tests of `internal/app` open a store in memory (`memstore.New`, through `newService`). A test that checks the
  store's files, their modes, syncs or `flock` opens a file store (`newFileService`), with
  `statestore.OpenWith(..., statestore.WithoutSync())`, as the tests of `internal/cli` do. Production code calls
  `statestore.Open`.
- The tests of `internal/cli` read the environment and the home directory through `globalOptions` (`setenv`,
  `testOptions`), never through `t.Setenv`.
- Tests also run on Windows in CI: never hard-code OS-dependent standard library error text, and quote paths in
  expected output as the code does.
- Every commit passes `make check` on its own, so a pull request can be merged with rebase and merge.
- A question that only the maintainer can decide goes into `docs/architecture.md` §18. Ask before implementing it.
- A new ADR goes into the index of `docs/adr/README.md` and updates `docs/architecture.md` in the same change.
- Work items are GitHub issues in the milestones M0 to M6 (project "tent roadmap"); close them as they land.

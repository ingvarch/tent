# Contributing to tent

Thanks for helping. This file explains how to report problems and how to send a change.

## Report a bug or ask for a feature

Open an issue with one of the [issue templates](https://github.com/ingvarch/tent/issues/new/choose).
For a bug, include the commands you ran and their specs, what happened, what you expected, the output of
`tent version`, and the provider, region and Nomad version of the cluster. Remove API keys, tokens and secrets.

Do not report security problems in public issues. See [SECURITY.md](SECURITY.md).

## Set up

Local setup and the checks are described in [docs/development.md](https://github.com/ingvarch/tent/blob/main/docs/development.md).

## Make a change

1. Create a branch from `main` named `<type>/<short-name>`, for example `fix/vultr-retry-create`.
2. Write a failing test that describes the behavior you want, then the code that makes it pass.
3. Run the [checks](https://github.com/ingvarch/tent/blob/main/docs/development.md#checks). The output must have no warnings and no failures.
4. Update the files in `docs/` if the change alters what users or operators see. A change to the design needs an
   [ADR](https://github.com/ingvarch/tent/blob/main/docs/adr/README.md).
5. Commit with a [conventional commit](https://www.conventionalcommits.org/) message:
   `type(scope): description`, imperative, lower case.
   - Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`.
   - Scopes: the package or area, such as `app`, `cli`, `rollout`, `nomadops`, `statestore`, `vultr`, `tent-node`,
     `e2e`, `ci`.
6. Open a pull request and fill in the template. Link the issue it closes with `Closes #N`.

Pull requests are merged with rebase or squash, so `main` keeps a linear history.

The rules for agents working in this repository are in [AGENTS.md](https://github.com/ingvarch/tent/blob/main/AGENTS.md).

## Code of conduct

Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

By contributing, you agree that your contributions are licensed under the [Apache License 2.0](https://github.com/ingvarch/tent/blob/main/LICENSE).

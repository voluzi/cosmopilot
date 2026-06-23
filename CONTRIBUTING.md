# Contributing to Cosmopilot

Thanks for your interest in improving Cosmopilot! This guide covers how to set up a
development environment, run the tests, and submit changes.

By participating in this project you agree to abide by our standards of respectful,
constructive collaboration.

## Prerequisites

- [Go](https://go.dev/) (see the version pinned in [`go.mod`](go.mod))
- [Docker](https://www.docker.com/) — for building images and running e2e tests
- [make](https://www.gnu.org/software/make/)

Most other tools (controller-gen, helm, kind, kubectl, setup-envtest, ginkgo,
crd-to-markdown) are downloaded automatically into `./bin` by the Makefile targets
that need them, so you don't have to install them globally.

## Getting started

```bash
git clone https://github.com/voluzi/cosmopilot.git
cd cosmopilot
make help        # list all available targets
```

## Building

```bash
make build               # build the manager binary
make docker-build        # build the operator image
make docker-build-nodeutils
```

To run the controller locally against your current kube-context:

```bash
make install   # install CRDs into the cluster
make run       # run the manager on your host
```

## Code generation

If you change API types (`api/v1/*_types.go`) or RBAC/webhook markers, regenerate the
generated code and manifests:

```bash
make generate    # DeepCopy methods
make manifests   # CRDs, RBAC, webhook configuration
make docs        # regenerate the CRD and example documentation
```

Commit the regenerated files together with your changes.

## Tests

```bash
make test.unit          # unit tests
make test.integration   # envtest-based integration tests (no cluster needed)
make test.e2e           # end-to-end tests against a kind cluster
```

`make fmt` and `make vet` run formatting and static checks; please make sure both are
clean before opening a pull request.

## Documentation

Documentation lives under [`docs/`](docs/) and is built with
[Docusaurus](https://docusaurus.io/). The CRD reference and per-chain example pages are
generated from source by `make docs` — do not edit the generated files
(`docs/docs/reference/crds/crds.md` and `docs/docs/examples/**`) by hand. To preview
the site locally:

```bash
cd docs
bun install
bun run start
```

New documentation pages are added to the **Next** (unreleased) version and roll into
the next release snapshot automatically.

## Commit messages & pull requests

- Follow [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`,
  `chore:`, `refactor:`, `docs:`, `ci:`, etc. Keep the subject concise.
- Keep pull requests focused and include a clear description of the change and its
  motivation.
- Make sure code generation, formatting, vetting and the relevant tests pass.
- Reference any related issues.

## Reporting bugs & requesting features

Open an issue at
[github.com/voluzi/cosmopilot/issues](https://github.com/voluzi/cosmopilot/issues) with
as much detail as possible: what you expected, what happened, and how to reproduce it
(resource definitions, events and operator logs are very helpful).

## License

By contributing, you agree that your contributions will be licensed under the
[MIT License](LICENSE.md).

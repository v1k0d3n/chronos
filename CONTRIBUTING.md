# Contributing

Thanks for looking. Chronos is a proof of concept with one maintainer, so this
is short.

## Before you start

Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how the pieces fit and
[docs/SECURITY-MODEL.md](docs/SECURITY-MODEL.md) for the properties every
change must preserve. A change that weakens one of those — a reader seeing
more than their RBAC allows, a credential reaching storage, a record becoming
editable, a revert running with the operator's authority rather than the
requester's — will not be merged however useful it is otherwise.

## Working on it

```sh
make build test          # operator: build + unit and envtest suites
make manifests generate  # after changing API types or RBAC markers
cd console-plugin && node .yarn/releases/yarn-4.14.1.cjs install && \
  node .yarn/releases/yarn-4.14.1.cjs lint && node_modules/.bin/tsc --noEmit -p .
```

Go 1.25 and a container engine are needed; `operator-sdk` only for OLM bundles.
Deploying to a cluster you own is described in the README; keep your cluster's
details in a `config/overlays/<yours>/` directory, which git ignores.

## Changes

- Permissions come from `+kubebuilder:rbac` markers in the package that needs
  them, never hand-edited into `config/rbac/`. Each of the three processes gets
  its ClusterRole generated from its own packages.
- Anything the security model claims must have a test that would fail if the
  claim stopped being true. The existing suites run the actual attack paths
  against a real API server; extend them rather than mock around them.
- Commit messages explain why, in prose. The history is meant to be readable.
- If a change alters what an operator sees — a flag, a metric, a field — the
  operations guide changes in the same commit.

## Reporting problems

Bugs and ideas: open an issue. Security problems: see [SECURITY.md](SECURITY.md).

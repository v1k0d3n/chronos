# Deployment overlays

The manifests under `config/` and `console-plugin/deploy/` describe Chronos as
published: public images, no registry credentials, nothing about any particular
cluster. Anything specific to *your* environment belongs in an overlay here.

```
config/overlays/
├── README.md     committed
├── example/      committed — copy this
└── <yours>/      ignored by git
```

Everything in this directory except this README and `example/` is ignored by
git. That is the point: a private registry hostname, the name of a pull secret,
or a cluster-specific patch can live next to the code without ever being
committed.

## Why not `make deploy IMG=...`?

`make deploy` and `make deploy-mcp-server` run `kustomize edit set image`, which
rewrites tracked files. It works, but every deploy leaves your registry in the
working tree, one `git add -A` away from a commit. An overlay changes nothing
that git tracks.

## Use it

```sh
cp -r config/overlays/example config/overlays/lab
$EDITOR config/overlays/lab/kustomization.yaml   # your registry, your pull secret

make render-overlay OVERLAY=lab     # print what would be applied
make deploy-overlay OVERLAY=lab     # CRDs, operator, MCP server, console plugin
make undeploy-overlay OVERLAY=lab
```

`OVERLAY` defaults to `lab`.

Create the pull secret itself out of band (`oc create secret docker-registry …`).
The overlay only *names* it; do not put credentials in a file, ignored or not.

## Local build targets

If an overlay contains a `local.mk`, the Makefile includes it. Use it for build
and CI commands that only make sense against your own cluster — OpenShift
BuildConfigs, a private registry login, and so on — so they do not have to be
added to the shared Makefile.

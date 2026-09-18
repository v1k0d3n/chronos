# Chronos console plugin

An OpenShift console [dynamic plugin](https://github.com/openshift/console-plugin-template)
that surfaces the Chronos change timeline **inside** the web console — no
separate app to look like OpenShift, because it *is* console surface area.

Scaffolded from Red Hat's `console-plugin-template` (SDK 4.22, PatternFly 6,
React 18).

## What it adds

- **Observe → Timeline** — a cluster-wide view of `ChangeEvent`s with a
  top-right view toggle:
  - **Timeline** — reverse-chronological feed grouped by day (default).
  - **Calendar** — month heat-map colored by the day's highest risk, with a
    day drill-down.
  - **List** — dense sortable table.
- A right slide-out **detail drawer** with the change summary, actor +
  attribution confidence, changed fields, snapshot references, redaction
  notice, and the (Phase 4) revert action.

## Data & security seam

All reads go through `src/useChangeEvents.ts`. Today it reads `ChangeEvent`s
directly via the console k8s API (works for cluster admins). The Chronos
security model requires multi-tenant access to go through a SAR-filtering
backend — when that lands, **only this hook changes**; the views don't.

## Develop

```sh
yarn install
yarn build-dev          # or: yarn build (production)
yarn start              # plugin dev server on :9001
./start-console.sh      # local console bridge against your cluster
```

The `consolePlugin` block in `package.json` and `console-extensions.json`
declare the plugin name (`chronos`), exposed component, page route, and nav
placement.

## Deploy to a cluster

Build the image from this directory's `Containerfile` (UBI Node build → UBI
NGINX runtime — no local `yarn` needed) and apply the manifests in `deploy/`:

```sh
podman build -f Containerfile -t $REGISTRY/chronos-console-plugin:latest .
podman push $REGISTRY/chronos-console-plugin:latest

# point deploy/ at your image, apply, then enable it in the console
( cd deploy && kustomize edit set image \
    quay.io/bjozsa-redhat/chronos-console-plugin=$REGISTRY/chronos-console-plugin:latest )
oc apply -k deploy
oc patch consoles.operator.openshift.io cluster --type=json \
  -p '[{"op":"add","path":"/spec/plugins/-","value":"chronos"}]'
```

The plugin deploys into `chronos-system` (create it first, or deploy the
operator with `make deploy`, which creates it). TLS is issued automatically by
OpenShift's service-serving-cert. See the repo root `README.md` →
**Deploy to a cluster** for the full operator + plugin walkthrough.

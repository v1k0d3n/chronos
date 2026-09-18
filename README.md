# Chronos — a Time Machine for OpenShift

> Chronos gives Red Hat Solution Architects a correlated, forensic timeline of
> **who changed what, when** in an OpenShift cluster — with per-resource diffs
> and surgical revert — so a broken cluster can be diagnosed and rolled back
> quickly, from inside the OpenShift console.

**Status:** proof of concept (use with caution)

**Codename**: **Chronos**.

## The problem

When a user changes something that either breaks or degrades OpenShift, a field
SA has to reconstruct *what changed* from scattered signals. Chronos turns that
into a single answer: a browseable timeline where every change carries its actor,
timestamp, a before/after diff, and a **[Revert]** button.

## How it works

Chronos fuses two signals that are each insufficient alone:

- **Audit logs** know *who / when / from where*, but aren't a state store.
- **Watch/informers** hold full *before/after object state*, but no human identity.

A **change correlator** joins them (by object UID + verb + timestamp window) into
immutable `ChangeEvent` records backed by redacted `ResourceSnapshot`s. A
`RevertOperation` reconstructs a prior snapshot and re-applies it. Everything is
surfaced through an OpenShift **console dynamic plugin**, and rides the platform's
built-in monitoring/alerting instead of shipping its own. The operator runs as
three processes — watcher, correlator, reverter — each under a ServiceAccount
holding only what that role needs (see
[docs/OPERATIONS.md](docs/OPERATIONS.md#the-three-processes)).

```
apiserver audit ─┐
                 ├─► correlator ─► ChangeEvents + ResourceSnapshots ─► console plugin (Observe ▸ Timeline)
watch/informers ─┘                        │                                 ├─► resource-page "History" tab
                                          │                                 └─► MCP server (ask an AI agent)
                                          └─► RevertOperation (surgical, per-resource)
```

How the pieces fit, and why, is in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md);
the threat model and its limits in [docs/SECURITY-MODEL.md](docs/SECURITY-MODEL.md);
running it day to day in [docs/OPERATIONS.md](docs/OPERATIONS.md).

## API (`chronos.ocp.run/v1alpha1`)

| Kind | Role |
|------|------|
| `ChangeEvent` (`chce`) | One immutable point on the timeline: verb, target, actor + attribution confidence, risk, changed fields, before/after snapshot refs. |
| `ResourceSnapshot` (`rsnap`) | Redacted point-in-time object state. Substrate for diffs and revert. Sensitive values are stripped at capture and recorded as hashes. |
| `RevertOperation` (`revop`) | Requests a surgical, per-resource revert by server-side apply, carrying who asked for it. (`DeleteAndRecreate`, for immutable fields, is declared in the API but not implemented.) Its result is recorded as a ChangeEvent credited to the requester. |

`oc get chronos` lists all three (shared `chronos` category).

## Ask Chronos from an AI agent (MCP)

An optional **MCP server** exposes the timeline to AI assistants and agents, so
that "what changed here just before this broke?" can be asked in the middle of an
investigation rather than by hand.

It offers exactly two tools — deliberately, because anything that can already
read a custom resource can already fetch a `ChangeEvent` by name, and
duplicating that would be maintenance without benefit:

| Tool | Answers |
|------|---------|
| `changes_in_window` | What changed between two times, filtered by namespace, kind, name, verb, or risk. Chronos's labels index verb, risk, confidence and target — but **not time**, which is why a generic resource query cannot answer this. |
| `diff_snapshots` | What differs between two captured states of one object, field by field, with status and metadata churn ignored. |

Two properties worth knowing:

- **Redactions are reported, never resolved.** Values stripped at capture stay
  stripped. A diff lists redacted paths so a change to a withheld field is
  visible as *having occurred* without exposing what it became. This is why the
  server lives in Chronos rather than in a consumer: the rule belongs with the
  code that applies it.
- **An empty window is a finding, not an error.** "Nothing in scope changed
  during that period" is frequently the answer that clears the platform, so it is
  stated explicitly rather than returned as an empty table.

**Queries run as the caller, not as the server.** Every request must carry a
bearer token; the server authenticates it with a `TokenReview` and then answers
*as that caller*, so the API server applies their own RBAC. This is the same
guarantee the console gives — defense 1 in the security model below. The
server's own ServiceAccount holds **no read access to Chronos data**: a query
answered under its own identity gets 403.

**Callers use tokens that are good for nothing else.** The server is deployed
with `--token-audience=chronos-mcp`, so a caller mints a token for that
audience (`oc create token <serviceaccount> --audience=chronos-mcp`) and the
server acts as them by impersonation. Such a token is refused by the API server
itself — leaked from an agent's config file or sniffed off the wire, it reads a
timeline and no more — and the server never caches it. The cost is that the
server's ServiceAccount holds `impersonate`; it acts only as an identity the
API server has just authenticated, and that bound is enforced by code and
tests, so treat the ServiceAccount as sensitive. General tokens such as
`oc whoami -t` are refused unless `--allow-unscoped-tokens` is set, because
handing one to any service is handing it a cluster credential.

The listener is TLS (certificate from OpenShift's service-ca), and a
NetworkPolicy admits only the router and namespaces labelled
`chronos.ocp.run/mcp-client=true`. A request without a valid token is refused
with `401` before reaching a tool. Grant the `chronos-mcp-timeline-reader`
ClusterRole to whoever should be able to query — bound to a namespace to scope
a caller to it, or cluster-wide for a fleet view. That binding, not the server,
decides what any given caller can see.

Over `--stdio` there is no second identity to reconcile: the process runs as the
person who launched it, under their own kubeconfig.

```sh
# Build and push
make docker-build-mcp-server docker-push-mcp-server MCP_IMG=$REGISTRY/chronos-mcp-server:latest

# Deploy (optional; not part of `make deploy`)
make deploy-mcp-server MCP_IMG=$REGISTRY/chronos-mcp-server:latest
```

Serves streamable HTTPS on `:8443/mcp`, with `/healthz` for probes. For a local
client such as an IDE, `--stdio` serves over stdin/stdout instead:

```sh
make run-mcp-server          # HTTP, against your current cluster
go run ./cmd/mcp-server --stdio
```

## Security model (non-negotiable)

Two independent defenses against leakage:

1. **Authorization filtering** — every read is performed as the **requesting
   user**, so the API server enforces their RBAC. The console plugin queries
   the CRDs directly from the browser under the user's session; the MCP server
   authenticates the caller's token and impersonates them. No component
   answers a timeline query under a shared, privileged identity. Today this is
   **namespace-level**: a user granted the viewer role in a namespace sees every
   change recorded there. Object-level filtering (only changes to objects the
   user could `get`) is the planned read backend — see
   [docs/SECURITY-MODEL.md](docs/SECURITY-MODEL.md#known-limitations).
2. **Capture-time redaction** — anything recognisable as a credential is
   **never persisted**, on any kind: every `Secret` value; the `last-applied-configuration` annotation;
   ConfigMap entries, environment variables and annotations whose key names a
   credential (`password`, `token`, `secret`, `key`, …); and any string shaped
   like one (a private key, a JWT, a URL with a password, a recognisable
   token). Snapshots keep a keyed hash (HMAC under a per-installation key,
   so a snapshot cannot be used to test guesses) so diffs show "value changed"
   without the value, and a revert leaves redacted fields alone rather than
   blanking them. `ChronosConfig` can add key patterns and field paths per namespace.

Reading is one half. **Revert is the other**, and the same principle governs it:
the operator performs the change, but only on behalf of someone who could have
made it themselves. An admission webhook records the authenticated requester on
every `RevertOperation`; the controller then asks the API server, as that
identity, whether they may `patch` the target (and `escalate` / `bind` for RBAC
objects), and checks that the snapshot being applied really is the object named.
If the webhook is absent or bypassable the operator refuses every revert rather
than act for an identity it cannot vouch for. Details in
[docs/OPERATIONS.md](docs/OPERATIONS.md#who-may-revert-what).

**The ledger is tamper-resistant.** `ChangeEvent`s and `ResourceSnapshot`s are
written by Chronos alone: no shipped role grants anyone else write access, and a
`ValidatingAdmissionPolicy` refuses writes from any other identity even if such
a role is bound later. Once written, a record cannot be changed — not even by
Chronos — apart from learning *who* made the change (write-once, by the audit
correlator) and whether it was later reverted. Tamper-*resistant* rather than
tamper-proof: a cluster-admin can remove the policy binding, and that removal is
itself an audited API call. Details in
[docs/OPERATIONS.md](docs/OPERATIONS.md#the-ledger-is-append-only).

## Phasing

- **Phase 0 (scaffold)** — operator project, the four CRDs, RBAC. ✅
- **Phase 1** — watch controller → `ChangeEvent`s with before/after diffs and best-effort (managedFields) attribution; `ChronosConfig` for per-namespace noise tuning. ✅
- **Phase 2** — audit correlator: joins the API-server audit log to the change timeline to add the verified *who / from where / with which client*. ✅
- **Phase 3** — console dynamic plugin (Chronos ▸ Timeline) with version compare, download, and per-resource history. ✅
- **Phase 4** — surgical per-resource revert (server-side apply, records its own `ChangeEvent`). ✅
- **Phase 5** — optional MCP server exposing the timeline to AI agents (`changes_in_window`, `diff_snapshots`), read-only and redaction-aware. ✅

### How attribution works (Phase 2)

The watch controller can only guess *who* from an object's `managedFields`
(the client/component name), so those events are marked `partial` — or
`unattributed` for deletes, which leave no managedFields. The guess is made
from **who owns the fields that changed**, not from who wrote to the object
most recently: a person scales a Deployment, the deployment controller
annotates it a moment later, and it is still the person's change. Only a
change whose every field belongs to a controller is treated as bookkeeping and
dropped (`chronos_controller_writes_suppressed_total` counts those, by manager
name, since that name is chosen by the client). The **audit correlator**
upgrades the rest to `verified`:

- It reads the kube-apiserver audit log through the **node log API** (the same
  endpoint `oc adm node-logs` uses), so it needs **no privileged DaemonSet** and
  **no audit-profile change** — OpenShift's *default* profile already records the
  `Metadata` level (user, verb, objectRef, sourceIPs, userAgent, timestamp).
- It is **lazy**: it only reads audit logs when there are recent, not-yet-verified
  change events to attribute, and joins them by target + verb + time window,
  preferring a human identity over controller bookkeeping on the same object.
- Enriched events get the real username, groups, source IP, and user agent, are
  re-sourced as `correlated`, and flag **shared** accounts (`kube:admin`,
  `system:admin`) as an attribution blind spot.

This adds two permissions to the operator's ClusterRole: `get`/`list` on `nodes`
and `get` on `nodes/proxy` (to read the node-local audit log via the API).

## Getting started (dev)

```sh
make manifests generate   # regenerate CRDs + deepcopy after editing api/
make build                # compile
make install              # install CRDs into the current-context cluster
ENABLE_WEBHOOKS=false make run   # run the controller locally against that cluster
oc apply -k config/samples # illustrative records — only against `make run`; a deployed Chronos refuses records it did not write
oc get chronos -A
```

`make run` serves no admission webhook, so a locally-run operator records changes
but refuses reverts; deploy it to exercise revert.

If `make test` fails with `go: no such tool "covdata"`, your `go` is older than
`go.mod` asks for and Go downloaded a toolchain that lacks the coverage tool;
install Go 1.25 natively instead.

Prerequisites: Go 1.25+, `operator-sdk` 1.42+, a container engine, and cluster
access (`oc`/`kubectl`). Requires a Kubernetes/OpenShift cluster.

### Supported versions

| Component | Minimum | Why |
|---|---|---|
| Operator and MCP server | OpenShift 4.17 (Kubernetes 1.30) | Oldest release where `ValidatingAdmissionPolicy` is GA; the ledger's write protection is built on it. |
| Console plugin | OpenShift 4.22 | Built against console dynamic plugin SDK 4.22; the console will not load it on older releases. |

On 4.17 through 4.21 the operator, the CRDs, and the MCP server work and
everything can be driven from `oc`; only the console UI is unavailable.

## Deploy to a cluster

Runs Chronos for real (not just `make run` locally): the operator + the console
plugin, in your own environment. Requires cluster-admin on OpenShift 4.22+ (4.17+
without the console plugin — see [Supported versions](#supported-versions)), `oc`, a
container engine (`podman`/`docker`), and a registry you can push to.

Point everything at your registry:

```sh
export REGISTRY=quay.io/your-namespace   # a namespace you can push to
```

**1 — Build and push both images.** Both build from a `Containerfile` (the plugin
image builds itself from UBI Node → UBI NGINX, so it needs no local `yarn`):

```sh
# Operator (uses the Makefile)
make docker-build docker-push IMG=$REGISTRY/chronos-operator:latest

# Console plugin
podman build -f console-plugin/Containerfile -t $REGISTRY/chronos-console-plugin:latest console-plugin
podman push $REGISTRY/chronos-console-plugin:latest
```

**2 — Deploy the operator** (CRDs, then the manager). `make deploy` creates the
`chronos-system` namespace, the operator ServiceAccount + ClusterRole (including
the audit correlator's `nodes`/`nodes/proxy` read), and the manager Deployment.
The watch controller and audit correlator start automatically — no audit-profile
change needed.

```sh
make install                                            # CRDs
make deploy IMG=$REGISTRY/chronos-operator:latest       # RBAC + the three processes (must set IMG)
```

**3 — Deploy the console plugin** into `chronos-system`. Its TLS is issued
automatically by OpenShift's service-serving-cert — no manual certificates.

```sh
# point the manifest at the image you pushed (skip to use the default quay image)
( cd console-plugin/deploy && kustomize edit set image \
    quay.io/bjozsa-redhat/chronos-console-plugin=$REGISTRY/chronos-console-plugin:latest )

oc apply -k console-plugin/deploy
```

**4 — Enable the plugin** in the console (one-time):

```sh
oc patch consoles.operator.openshift.io cluster --type=json \
  -p '[{"op":"add","path":"/spec/plugins/-","value":"chronos"}]'
```

The console rolls out and **Chronos** appears in the navigation (Chronos ▸
Timeline). Opt namespaces into the timeline and tune noise via `ChronosConfig`
(see `config/samples/`).

**Uninstall:** remove `chronos` from `spec.plugins` on
`consoles.operator.openshift.io/cluster` (e.g. `oc edit consoles.operator.openshift.io cluster`),
then:

```sh
oc delete -k console-plugin/deploy
make undeploy && make uninstall
```

> The manifests default to the public `quay.io/bjozsa-redhat` images, so a quick
> evaluation can skip step 1 and the `kustomize edit`/`IMG=` overrides entirely.

### Deploying from your own registry, repeatably

The `IMG=` / `kustomize edit set image` route above rewrites tracked manifests,
which leaves your registry in the working tree. For anything you deploy more
than once, use an overlay instead — it holds your image locations and pull-secret
name in a directory git ignores:

```sh
cp -r config/overlays/example config/overlays/lab   # then edit it
make deploy-overlay OVERLAY=lab
```

See [config/overlays/README.md](config/overlays/README.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE). The console plugin is derived from
the OpenShift console plugin template and keeps its upstream notice in
[console-plugin/LICENSE](console-plugin/LICENSE).

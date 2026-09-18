# Architecture

Chronos answers one question about an OpenShift cluster — *who changed what,
when* — and lets the person asking put it back. This document explains how the
pieces fit and why they are shaped the way they are. Operational detail is in
[OPERATIONS.md](OPERATIONS.md); the security reasoning in
[SECURITY-MODEL.md](SECURITY-MODEL.md).

## Two signals, fused

Neither source of truth is sufficient alone:

| Source | Knows | Does not know |
|---|---|---|
| **API server audit log** | who made a request, from where, with which client, when | what the object looked like before or after |
| **Watch (informers)** | the full object state before and after every change | who made it — `managedFields` names a client, not a person |

Chronos runs both and joins them. The **watcher** records every meaningful
change to a watched kind as a `ChangeEvent` with before/after
`ResourceSnapshot`s, attributing it as well as `managedFields` allows
(`partial` confidence, or `unattributed` for deletes). The **correlator** reads
the audit log and, matching by object, verb and time, upgrades the actor to the
authenticated user (`verified`). A change made through the platform's own
revert is attributed by the **reverter** to the person who requested it.

The join is the core of the product. Everything else — the console plugin, the
MCP server, the metrics — is a way of reading what the join produced.

## The records

Everything Chronos knows is an ordinary Kubernetes object, in the API group
`chronos.ocp.run/v1alpha1`, so `oc`, label selectors, RBAC and GitOps all work
on it without anything bespoke:

- **`ChangeEvent`** — one point on the timeline: verb, target, actor and how
  confidently they were identified, risk level, the changed fields, and the
  names of the before/after snapshots.
- **`ResourceSnapshot`** — the object as it was at one moment, with
  credentials redacted and each redaction listed by path.
- **`RevertOperation`** — a request to restore an object to a recorded
  snapshot, carrying who asked for it.
- **`ChronosConfig`** — a cluster-scoped singleton for tuning what is
  recorded and what is redacted, cluster-wide and per namespace.

Records are stored in the namespace of the object they describe; records for
cluster-scoped objects live in the operator's namespace. They are append-only:
see the security model.

## Three processes

The operator is one binary deployed three times, each under a ServiceAccount
holding only what that role needs:

| Process | Role |
|---|---|
| `watcher` | informers over the watched kinds; writes records; holds the redaction key |
| `correlator` | reads the audit log via the node log API; enriches records with verified actors |
| `reverter` | carries out `RevertOperation`s; serves the admission webhook that stamps the requester |

The split exists so that the broad permissions each role needs — cluster-wide
read of Secrets, `nodes/proxy`, write on RBAC objects — are never held by one
identity. Each role's ClusterRole is generated from the code of that role.

## What is watched

A fixed set of high-signal kinds: Deployments, DaemonSets, StatefulSets,
ConfigMaps, Secrets, Services, ServiceAccounts, Roles, RoleBindings,
ClusterRoles, ClusterRoleBindings, NetworkPolicies. Pods, ReplicaSets and Events
are deliberately excluded as high-churn noise that a person almost never
changed by hand.

Informers can be scoped to a list of namespaces, which is strongly recommended
on large clusters: a cluster-wide cache of every Secret and ConfigMap is
memory-heavy.

## Noise

Most writes to a cluster are controllers reconciling. Chronos's default is
*show human-driven changes, hide controller bookkeeping*. A change counts as
bookkeeping only when every field that changed is owned, per `managedFields`,
by a manager whose name looks like a controller; a person's edit is kept even
when a controller touches the object a moment later. Service-account token and
registry-credential Secret churn is dropped. Namespaces matching
`openshift-*`/`kube-*` are ignored. All of this is tunable in `ChronosConfig`,
per namespace.

## Revert

A revert reconstructs the recorded snapshot, strips server-managed fields, and
applies it with server-side apply under the field manager `chronos-revert`,
taking ownership of the fields it sets. Fields that were redacted at capture
are left out of the apply entirely, so the live value survives; the result
names them. Objects owned by a controller can be reverted, but the controller
will usually reconcile them back — the console warns when a target has an
owner.

Who may revert what is decided by the API server, as the person who asked; see
the security model.

## Console plugin

An OpenShift console dynamic plugin (`console-plugin/`) adds **Observe ▸
Timeline**: a feed, a calendar, and a list of changes, each opening a detail
drawer with the object's whole version history, a red/green diff between any
two versions, downloads, and revert. It reads the CRDs directly under the
user's console session, so the console's own RBAC applies. The design
principle is to extend the console rather than build a UI that resembles it:
PatternFly, the console's Project selector, its theming and its navigation.

## MCP server

An optional server exposes two tools to AI agents — `changes_in_window` and
`diff_snapshots` — over streamable HTTPS. It answers every query as the caller,
by impersonating the identity the API server reports for the caller's
audience-scoped token, so the caller's own RBAC governs what they see.

## Riding the platform

Chronos ships no datastore, no login system, no notification service and no
dashboards of its own. Records are CRDs; metrics (`chronos_*`) are scraped by
the platform monitoring stack; alerts are `PrometheusRule`s; authentication and
authorization are the cluster's. That is roughly four subsystems not built, and
it is what keeps the operator small.

## Where it is going

- A thin read backend that filters the timeline by `SelfSubjectAccessReview`
  under the requesting user's token, so a user sees only changes to objects
  they could `get` — finer than namespace-level RBAC on the CRDs allows today.
- A "History" tab on resource detail pages, so the timeline meets people
  where they already are.
- Retention: records can be deleted by nobody today; Chronos will own that.
- An attribution-health dashboard: change volume, top actors, share
  unattributed.

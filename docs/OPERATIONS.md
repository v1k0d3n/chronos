# Chronos operations guide

For operations teams using Chronos to troubleshoot problematic updates and
upgrades: how to **target the changes you care about**, **filter out noise**, and
drive everything from the CLI.

Everything Chronos records is a normal Kubernetes object (`ChangeEvent`,
`ResourceSnapshot`, `RevertOperation`, `ChronosConfig`), so it all works with
`oc`/`kubectl`, label selectors, RBAC, and GitOps — no bespoke tooling.

**Supported versions:** OpenShift 4.17+ (Kubernetes 1.30+) for the operator and
MCP server; OpenShift 4.22+ for the console plugin. Everything in this guide is
CLI-driven and works across that whole range.

---

## The three processes

Chronos is one binary deployed three times, each under its own ServiceAccount
holding only what that role needs. Compromising one gives none of the others.

| Deployment | Does | Holds | Does not hold |
|---|---|---|---|
| `chronos-watcher` | Observes the watched kinds; writes `ChangeEvent`s and `ResourceSnapshot`s; keeps the redaction key. | read on every watched kind (Secrets included, redacted before storage); **create** on records; the key Secret | any write to cluster objects; audit-log access |
| `chronos-correlator` | Reads the kube-apiserver audit log through the node log proxy; upgrades records' attribution to verified identities. | `nodes/proxy get` — a broad permission, which is why it lives alone; **update** on records | Secrets; any write to cluster objects |
| `chronos-reverter` | Carries out `RevertOperation`s; serves the admission webhook that records the requester. | write on the revertable kinds, `escalate`/`bind` on roles (so any recorded RBAC object can be restored — guarded by the requester needing them too); `SubjectAccessReview`; **update** on records | `list`/`watch` on Secrets; audit-log access |

The roles are generated from the code of each role (`make manifests`), so a
permission cannot be granted without the code that needs it living in that
role's package. `--role=all` runs everything in one process, for `make run`.

## What Chronos watches

A fixed set of high-signal kinds (pods, events, and replicasets are deliberately
excluded as high-churn noise):

- **apps** — Deployments, DaemonSets, StatefulSets
- **core** — ConfigMaps, Secrets, Services, ServiceAccounts
- **rbac** — Roles, RoleBindings, ClusterRoles, ClusterRoleBindings
- **networking** — NetworkPolicies

> Adding a kind is currently a code change (`watcher.DefaultResources`), not
> config — see [TODO.md](../TODO.md).

---

## Inclusion & exclusion: targeting signal, ignoring noise

Filtering happens at two levels, coarse to fine.

### Level 1 — deploy-time env vars (cluster-wide, set on the operator)

| Env var | Default | Effect |
|---|---|---|
| `CHRONOS_WATCH_NAMESPACES` | *(empty = all)* | Comma-separated namespaces to **watch**. Scopes the informers so only these namespaces are cached. See "Scoping the watch" below. |
| `CHRONOS_EXCLUDE_CONTROLLER_ACTORS` | `true` | Drop controller/operator bookkeeping; keep human-driven changes. The primary noise reducer. A change is bookkeeping only when *every changed field* is owned (per `managedFields`) by a manager whose name looks like a controller or operator; a person's edit is kept even if a controller touched the object afterwards. That name is client-chosen, so `chronos_controller_writes_suppressed_total{manager}` shows what is being dropped. |
| `CHRONOS_EXCLUDE_NOISE_SECRETS` | `true` | Drop service-account-token / dockercfg secret churn. |
| `CHRONOS_IGNORE_NAMESPACES` | `openshift-*,kube-*,openshift,default,chronos-system,chronos-demo` | Comma-separated globs; a trailing `*` is a prefix match. Record-time filter. |
| `CHRONOS_SYSTEM_NAMESPACE` | `chronos-system` | Where records for cluster-scoped objects are stored. |
| `CHRONOS_RECORD_SOURCE_IP` | `true` | Keep a verified actor's source IP on the ChangeEvent. Set `false` where holding it is not permitted; identity, groups and user agent are still recorded. |
| `CHRONOS_AUDIT_TAIL_BYTES` | `33554432` (32 MiB) | How much of each control-plane node's `kube-apiserver/audit.log` the attribution pass reads, from the end. It must cover at least the last two minutes of audit; on a busy API server, raise it. A change whose audit record has already scrolled out of this tail is labelled `chronos.ocp.run/attribution=expired` and keeps its best-effort attribution — watch `chronos_audit_expired_total`. |

#### Scoping the watch (`CHRONOS_WATCH_NAMESPACES`) — recommended on large clusters

By default Chronos watches **all** namespaces. On a large cluster, cluster-wide
informers over every Secret and ConfigMap are memory-heavy and can OOM the
operator — and an OOM restart can silently miss change events created while it
was down. **Scope the watch to the namespaces you care about:**

```sh
# flag
--watch-namespaces=team-a,team-b,payments
# or env (equivalent)
CHRONOS_WATCH_NAMESPACES=team-a,team-b,payments
```

This runs one informer per listed namespace for namespaced kinds, keeping the
caches small. Cluster-scoped kinds (`ClusterRole`, `ClusterRoleBinding`) and the
`ChronosConfig` CRD are always watched cluster-wide (they're low-volume). Empty =
watch everything (fine for small clusters). This is the informer-level *include*;
the record-time filters above still apply on top.

> This is separate from `CHRONOS_IGNORE_NAMESPACES`, which is a record-time
> *exclude* filter and does not reduce what the informers cache. For memory, use
> `CHRONOS_WATCH_NAMESPACES`.

### Level 2 — the `ChronosConfig` CR (declarative, live, per-namespace)

`ChronosConfig` (`chcfg`) is a **cluster-scoped singleton** (conventionally named
`cluster`). It tunes filtering **without redeploying** the operator, and is the
only way to set **per-namespace** policy. Settings resolve most-specific-wins:

```
built-in defaults  →  spec.defaults  →  spec.namespaceOverrides[<namespace>]
```

The policy knobs (any layer may set any of them):

| Field | Purpose |
|---|---|
| `excludeControllerActors` | Human-only (`true`) vs. capture everything incl. controllers (`false`). |
| `excludeNoiseSecrets` | Drop SA-token / dockercfg secret churn. |
| `excludeNamespaces` | Glob list (opt-out). |
| `excludeSecretTypes` | e.g. `kubernetes.io/dockerconfigjson`. |
| `excludeFields` | **Ignore** noisy field paths (prefix match) — e.g. a status-like annotation. |
| `includeFields` | **Target** — only record changes to these field paths. |
| `redactKeys` | Extra regular expressions naming credentials by key (ConfigMap keys, env var names, annotation keys). Adds to the built-ins; see "What is redacted". |
| `redactFields` | JSON Pointers to fields always redacted when present, e.g. `/data/config.yaml`. Adds to the built-ins. |

Example (`config/samples/chronos_v1alpha1_chronosconfig.yaml`):

```yaml
apiVersion: chronos.ocp.run/v1alpha1
kind: ChronosConfig
metadata:
  name: cluster
spec:
  defaults:
    excludeControllerActors: true      # human-driven changes only
  namespaceOverrides:
    - namespace: team-a
      excludeFields: ["spec.replicas"]  # ignore autoscaler churn on replicas
    - namespace: team-b
      excludeControllerActors: false    # audit-heavy: capture controllers too
    - namespace: secure-team
      excludeFields: ["metadata.annotations"]  # watch metadata, ignore annotations
```

Apply and inspect:

```sh
oc apply -f config/samples/chronos_v1alpha1_chronosconfig.yaml
oc get chcfg cluster -o yaml
```

Changes take effect live — the operator reloads the config; no restart.

---

## What is redacted

A snapshot is stored only after anything that looks like a credential has been
stripped from it. The value is replaced by an empty string and a short hash is
kept, so a diff can still say "this changed" without saying what it was. This
applies to **every watched kind**, not only Secrets:

| Rule | Applies to |
|---|---|
| All `data` / `stringData` values | Secrets |
| The `kubectl.kubernetes.io/last-applied-configuration` annotation (it embeds the whole object as applied) | every kind — removed, not blanked |
| A key that names a credential: `password`, `passwd`, `passphrase`, `secret`, `token`, `api-key`, `access-key`, `private-key`, `credential`, `authorization`, `bearer`, `*.key`, `*_key`, `dsn`, `connection-string` (case-insensitive) | ConfigMap keys, environment variable names (in any Pod template), annotation keys |
| A value shaped like a credential: a PEM private key, a JWT, a URL with `user:password@`, an AWS / GitHub / GitLab / Slack / Vault / OpenShift token | any string, anywhere in the object |
| `redactKeys` / `redactFields` from `ChronosConfig` | as configured, per namespace |

Public material is deliberately *not* redacted: `ca.crt`, `tls.crt` and other
certificates are worth seeing change. A `valueFrom` reference to a Secret is a
reference, not a value, and is kept.

The hash kept for each redacted value is an **HMAC under a key private to the
installation**, not a plain hash: a snapshot on its own cannot be used to test
guesses against a withheld value. The key is generated on first start into the
Secret `chronos-redaction-key` in `chronos-system`, and each redaction records
the key's ID (`keyId`). Hashes are comparable only under the same key. To
rotate: delete the Secret and restart the operator — snapshots recorded under
the old key keep their hashes but can no longer be compared with new ones.
Include the Secret in any backup of `chronos-system` if that comparability
matters to you.

Each redaction is listed on the snapshot as a JSON Pointer
(`spec.redactions[].fieldPath`, e.g. `/spec/template/spec/containers/0/env/2/value`).
A revert leaves exactly those fields out of the apply, so the live value
survives and everything else is restored; `status.manualStepsRequired` names
what was left alone.

## Driving Chronos from the CLI

### See the timeline

```sh
oc get chronos -A                          # all four kinds (shared category)
oc get chce -n <namespace>                 # ChangeEvents as a table
```

`oc get chce` columns: **Verb · Kind · Target · Actor · Confidence · Risk · When**.

### Filter / target with label selectors

Every `ChangeEvent` is labeled for fast slicing:

```sh
oc get chce -A -l chronos.ocp.run/risk=high
oc get chce -A -l chronos.ocp.run/confidence=partial     # attribution gaps
oc get chce -n <ns> -l chronos.ocp.run/verb=delete
oc get chce -n <ns> -l chronos.ocp.run/target-kind=deployment
```

Available labels: `target-kind`, `target-namespace`, `verb`, `confidence`, `risk`
(all prefixed `chronos.ocp.run/`).

### Inspect a change and its snapshots

```sh
oc get chce <name> -o yaml       # full record: changed fields + before/after snapshot refs
oc get rsnap -n <ns>             # snapshots — columns: Kind · Target · Redacted · Revertable · Captured
```

`Redacted=true` means sensitive values were stripped at capture (never stored);
`Revertable=false` means the snapshot alone can't fully restore the object.

### Revert from the CLI

The console button isn't the only path — a `RevertOperation` is a plain object,
so reverts are scriptable, GitOps-able, and support a **dry-run preview**.

```yaml
apiVersion: chronos.ocp.run/v1alpha1
kind: RevertOperation
metadata:
  generateName: revert-checkout-
  namespace: <namespace>
spec:
  target:
    apiVersion: apps/v1
    kind: Deployment
    namespace: <namespace>
    name: <object>
  # Revert a specific recorded change (uses its before-state)...
  changeEventRef: <changeevent-name>
  # ...or pin an exact snapshot instead:
  # toSnapshot: <resourcesnapshot-name>
  strategy: ServerSideApply
  dryRun: true          # preview only — sets status.message, applies nothing
```

```sh
oc create -f revert.yaml
oc get revop -n <namespace> -w        # watch Phase: → Succeeded / Failed / Skipped
```

`oc get revop` columns: **Kind · Target · Strategy · Phase · Age**. When Chronos
applies a revert it records it as a **new `ChangeEvent`**, so the undo is itself
on the timeline.

The revert uses server-side apply under the field manager `chronos-revert`.

#### Who may revert what

**A revert can only do what the person requesting it could do directly.** The
operator carries out the change, and the operator can write Secrets and RBAC
objects across the cluster — so before it acts, it establishes who is asking and
asks the API server whether *they* could have made this change.

- **Who.** An admission webhook records the authenticated requester in
  `spec.requestedBy` as the RevertOperation is created. You do not set this
  field, and anything a client puts there is overwritten. `spec` is immutable
  after creation.
- **What they may do.** The operator runs a `SubjectAccessReview` as that
  identity: `patch` on the target, plus `create` when the object no longer exists
  and the revert would bring it back. For Roles and ClusterRoles it also requires
  `escalate`, and for bindings `bind` on the referenced role — the same rule the
  API server applies to a direct edit, applied to the requester rather than to
  the operator (which would pass it on anyone's behalf).
- **Which object.** The snapshot must record the same object the request names,
  its stored content must *be* that object, a namespaced target must live in the
  RevertOperation's own namespace, and cluster-scoped targets can only be
  reverted from `chronos-system`. Only the kinds Chronos watches can be reverted.

A refusal is not an error in the operator: the RevertOperation goes to
`Phase: Failed` and `status.message` says why, for example:

```
alice is not allowed to patch deployments "api" in namespace "payments".
A revert can only do what the person requesting it could do directly.
```

So to let someone revert in a namespace, grant them `create` on
`revertoperations` there **and** the ordinary permissions on the objects
themselves. The first alone does nothing.

The webhook is a hard requirement, not an option. If its
`MutatingWebhookConfiguration` is missing, or is configured so that some requests
can skip it (a `failurePolicy` other than `Fail`, a namespace or object selector,
match conditions), the operator refuses **every** revert and says so in
`status.message` — it will not act on an identity it cannot vouch for. This is
also why an operator started with `make run` records changes but cannot revert
them: it serves no webhook.

---

## The ledger is append-only

A timeline you can edit is not evidence. Three controls, from the outside in:

| Control | What it stops |
|---|---|
| **RBAC.** Chronos ships viewer roles for `ChangeEvent` and `ResourceSnapshot` and nothing else. | Nobody is granted write access by default. |
| **Admission policy.** `chronos-ledger-writers` (a `ValidatingAdmissionPolicy`, OpenShift 4.17+) refuses any create, update or delete of those two kinds (status included) unless it comes from one of Chronos's own ServiceAccounts — and each may do only its part: the watcher creates records, the correlator and the reverter update them, nobody deletes. | A write from a role somebody bound later, or from a cluster-admin. The refusal names who tried: `Chronos records are written only by Chronos (created by system:serviceaccount:chronos-system:chronos-watcher, updated by …); alice may not create them`. |
| **CRD validation rules.** A `ResourceSnapshot` is fully immutable. A `ChangeEvent`'s record of *what* changed is immutable; its `actor` and `source` may be filled in once by the audit correlator (a `verified` actor is never revised), and `status.reverted` / `status.revertOperation` may be set when a revert lands (status is covered by the policy as well). | Chronos itself rewriting history — through a bug, or a compromised operator. |

The identities that may write are named in the `chronos-ledger-writers`
ConfigMap in `chronos-system`, filled in from the ServiceAccounts at deploy
time. Namespace deletion and garbage collection are exempt for `DELETE`
only, so removing a namespace still cleans up its records.

What this is not: a cluster-admin can delete the policy binding and then edit
records. That deletion is an audited API call, which is as far as an in-cluster
control can go. Retention — deleting old records on purpose — is Chronos's job,
not a user's, and is not yet implemented.

Consequences worth knowing:

- `oc apply -f` of a hand-written `ChangeEvent` is refused on a deployed
  cluster. The samples under `config/samples` are for `make run`.
- The `revertoperation-editor` role grants `create` only: a request's `spec` is
  immutable and deleting one would hide that it was made.

## What Chronos records about people

A `ChangeEvent` is a record of who did what, so it holds personal data by
design: the actor's username, UID and groups, the client (user agent) they used,
and — once the audit correlator has matched the change — the source IP address
the request came from. Where a shared account was used (`kube:admin`,
`system:admin`) the record says so, and where a request was impersonated it
holds both the caller and the asserted identity.

Reading a record needs `get` on `ChangeEvent`s, which no shipped role grants
beyond the viewer roles and the MCP timeline reader; bind those deliberately.
Records cannot be edited or deleted by anyone (see "The ledger is append-only"),
so there is no way to remove a person's data short of removing the policy
binding — plan retention before deploying where that matters. The source IP is
the one field that is about location rather than identity; set
`CHRONOS_RECORD_SOURCE_IP=false` to stop recording it.

## Notes for upgrade / maintenance windows

Because Chronos watches continuously, changes made during an upgrade are captured
automatically — with verified *who / what / when* and a before-state to revert
to. A practical loop:

1. Before the window, confirm the namespace isn't in `CHRONOS_IGNORE_NAMESPACES`.
2. Perform the upgrade / deploy.
3. `oc get chce -n <ns> --sort-by=.spec.observedAt` to review what actually
   moved (and who/what moved it).
4. Revert any problematic change per resource (CLI above, or the console).

First-class support for this workflow (scoped watch sessions, a window
checkpoint + full delta, and bulk/transactional revert) is proposed in
[TODO.md](../TODO.md).

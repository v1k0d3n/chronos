# Chronos — deferred work & future features

Chronos is a **proof of concept**. It is deliberately kept lightweight and
*unopinionated* about the hard architectural decisions (storage backend,
multi-tenancy model, packaging) so it stays easy to fold into a larger project
— e.g. an upstream OpenShift/Origin effort — without having to unwind choices
made for demo convenience.

This file tracks work we've **consciously deferred**. Where a decision would be
hard to reverse, we've left an API *seam* instead of an implementation, so the
POC keeps working today and the real decision can be made later, in the right
project, by the right owners.

Nothing here is required for the POC to demonstrate the "Time Machine" story
(forensic timeline → compare → download → verified attribution → surgical
revert), which works end-to-end today.

---

## Flagship use case: upgrade / maintenance-window safety net

Chronos as a **change flight-recorder + undo buffer** for maintenance windows:
turn it on for a namespace, perform the risky upgrade, see exactly what moved and
who moved it, and roll back the bad parts. The `do upgrade → inspect timeline →
revert per resource` loop **works today** (see [docs/OPERATIONS.md](docs/OPERATIONS.md)).
To make it first-class:

- [ ] **Scoped, ephemeral watch sessions.** Watching is opt-*out* today
  (everything minus excludes). Add opt-*in*, bounded "sessions" — e.g. watch
  `namespace X` from window start to end. Builds on namespace-scoped informers
  (below).
- [ ] **Window checkpoint + full delta.** Mark a baseline at window start; show
  the *entire* set of changes since, as one reviewable diff. The snapshot
  substrate already exists — this is a marker + a view.
- [ ] **Bulk / transactional revert.** Revert is per-resource today. Add
  "roll back everything that changed since T" as a single operation over the
  window's `ChangeEvent`s.
- [ ] **Operator-owned reality check (already partially there).** Upgrades touch
  operator-managed resources that get reconciled back; Chronos already *warns*
  when an object is controller-owned. Extend this into upgrade-aware guidance.

---

## Storage & scale

The POC stores everything — `ChangeEvent`s and full (redacted) `ResourceSnapshot`
manifests — inline in **etcd** via CRDs. Simple and dependency-free, but it grows
unbounded and adds etcd load. These items make it safe for large clusters, and
each is **opt-in** so default behavior never changes.

- [ ] **Retention policy.** Add a `retention` block to `ChronosConfigSpec`
  (`maxAge`, `maxPerObject`), enforced by a small leader-elected pruning
  Runnable. Delete oldest-first beyond the policy; never delete a snapshot still
  referenced by a retained `ChangeEvent` or a pending `RevertOperation`. Default
  empty = unlimited (today's behavior). *Highest-value, lowest-risk next step.*
- [ ] **Store snapshot bodies outside etcd (opt-in).** The API seam already
  exists: `ResourceSnapshot.spec.contentRef` (`StorageReference{backend,key,
  sizeBytes}`) is defined and mutually exclusive with inline `spec.content` —
  the watcher just always inlines today. Add a `storage` block to
  `ChronosConfigSpec` (`backend: etcd|pvc|s3`, `inlineMaxBytes` threshold for a
  hybrid split, PVC claim / S3 bucket+secretRef). When `backend != etcd`, write
  the body to the backend and set `contentRef`; the console `useSnapshot` hook
  and the revert controller read via `contentRef` when present. Keeps etcd
  holding only lightweight events + snapshot metadata. **Default stays `etcd`**,
  so a customer only offloads when they choose to.
- [x] **Namespace-scope the informers.** *(Done.)* `--watch-namespaces` /
  `CHRONOS_WATCH_NAMESPACES` scopes the watch to specific namespaces (one
  informer factory per namespace for namespaced kinds; cluster-scoped kinds and
  the config CRD stay cluster-wide). Empty = all namespaces. This bounds the
  heavy Secret/ConfigMap caches and fixes the OOM crash-loop that was silently
  dropping change events on large clusters. See [docs/OPERATIONS.md](docs/OPERATIONS.md).
- [ ] **Backfill missed creates after a resync.** The watcher deliberately
  suppresses "create" events for objects already present at informer sync (so a
  restart doesn't spam the timeline). But if the operator is *down* when an
  object is created (e.g. an OOM restart), that create is silently absorbed into
  the next initial list and never recorded — and for controller-reconciled kinds
  (Deployments) nothing re-surfaces it later. Add a reconcile-on-sync pass that
  detects watched objects with no ResourceSnapshot and backfills them (or at
  least emits a metric/log when it happens).
- [ ] **Fix the inert `includeFields` footgun.** `includeFields` currently only
  acts as an exception-list to `excludeFields`; set on its own it silently does
  nothing. Either make it a standalone whitelist ("record only changes to these
  fields") or reject/warn on a config that sets `includeFields` without
  `excludeFields`, so an operator doesn't think a namespace is scoped when it
  isn't.
- [ ] **(Optional) delta storage.** Store diffs between successive versions
  instead of full manifests per version. Meaningful storage win; more complex —
  only if profiling justifies it.

## Multi-tenancy & security

- [ ] **SAR-filtering read backend.** Today reads go straight to the CRDs via the
  console k8s API — fine for cluster-admins only. The security model calls for a
  thin backend that gates every result with `SelfSubjectAccessReview` /
  `SelfSubjectRulesReview` using the **requesting user's own token**, so users
  only see changes to objects they could already `get`, and end users are never
  granted `get` on the Chronos CRDs directly. The seam is isolated: only
  `console-plugin/src/useChangeEvents.ts` changes; the views don't.
## Packaging & delivery

- [ ] **OLM bundle.** Produce an operator bundle (`make bundle` scaffolding
  exists) so Chronos installs via OperatorHub/OLM.
- [ ] **CI/CD image build + push.** `.github/workflows/` has the operator-sdk
  starters (lint, test, test-e2e). Add a workflow that builds both images and
  pushes to `quay.io/bjozsa-redhat`, gated on PRs (with CodeRabbit for review).
  Needs quay robot-account secrets configured.
- [ ] **UBI base images for the operator on graduation.** The console plugin
  already builds on UBI; the operator `Containerfile` still uses upstream
  `golang:1.24` → `gcr.io/distroless/static`. Switch to Red Hat UBI (e.g.
  `ubi9/go-toolset` builder → `ubi9-micro`/`ubi9-minimal` runtime) to meet
  product guidelines.

## UX polish

- [ ] **Richer facet filters.** The timeline toolbar has free-text search;
  add dropdown facets (namespace / kind / risk).
- [ ] **Precise TOC → line anchoring** in the diff panel (jump to the exact
  changed line, not just the field).
- [ ] **Calendar view layout polish** (minor).

---

*Kept intentionally out of scope for the POC: choosing a single storage backend,
a multi-tenant auth model, or a packaging story. Those belong to whatever
larger project Chronos graduates into — the seams above keep that door open.*

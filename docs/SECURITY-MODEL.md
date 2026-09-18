# Security model

Chronos records who changed what in a cluster, and can change it back. Both
halves are dangerous if done carelessly: the first can turn Chronos into a
store of every credential that ever passed through a cluster, and a way for
people to read changes to things they are not allowed to see; the second can
turn a low-privilege user into a cluster-admin. This document states the
threats and the controls, and is honest about the gaps.

The principle throughout: **work with Kubernetes RBAC, never around it. The API
server is the only authority on what a person may see or do.**

## Reading: two independent defenses

**1. A reader sees only what they could already read.** Every timeline query
runs as the person asking. The console plugin reads the CRDs under the user's
own session; the MCP server impersonates the identity the API server reports
for the caller's token. No component answers a query under a shared,
privileged identity, so no bug in a query path can widen what a reader sees
beyond their own RBAC.

**2. Credentials are never stored.** Before a snapshot is written, every
Secret value, the `last-applied-configuration` annotation, any ConfigMap entry,
environment variable or annotation whose key names a credential, and any string
shaped like one (private key, JWT, URL with a password, recognisable token) is
replaced by an empty string. A keyed hash (HMAC under a per-installation key)
is kept so a diff can say the value changed. A snapshot on its own cannot be
used to test guesses against a value. Details: OPERATIONS.md, "What is
redacted".

Each defense holds if the other fails: a reader who somehow gained access to
every record would still find no credential in them; a record that somehow
escaped redaction would still be readable only by those entitled to the object.

## The ledger is append-only

A timeline that can be edited is not evidence. Three controls, from the outside
in: no shipped role grants write access to records; a
`ValidatingAdmissionPolicy` refuses writes from any identity but Chronos's own
(the watcher may create, the correlator and reverter may update, nobody may
delete); and CRD validation rules freeze what a record says once written, so
that even Chronos cannot rewrite history — only learn who did it (once) and
that it was reverted. Tamper-*resistant*, not tamper-proof: a cluster-admin can
remove the policy binding, and that removal is an audited API call.

## Reverting: with the requester's authority, never the operator's

The reverter can write Secrets and RBAC objects across the cluster; a
`RevertOperation` is a request that it do so. Three things must hold before it
acts:

1. **Who asked** is known: an admission webhook writes the authenticated
   requester onto the request at creation, overwriting anything a client
   supplied, and the request is immutable afterwards.
2. **That can be believed**: the reverter checks that the webhook is installed
   and cannot be bypassed (fail-closed policy, no selectors), and refuses every
   revert otherwise.
3. **They could have done it themselves**: the reverter asks the API server,
   as the requester, whether they may `patch` (and `create`) the target — and,
   for RBAC objects, whether they hold `escalate`/`bind`, the API server's own
   rule for writing permissions, applied to the requester rather than to the
   operator. The snapshot must record the requested object and its content must
   be that object; a namespaced target must be in the request's namespace.

A refusal is a `Failed` request with a readable reason; nothing is applied.

## Least privilege

The operator runs as three processes under three ServiceAccounts, so that
cluster-wide read of Secrets (watcher), `nodes/proxy` (correlator) and write on
RBAC objects (reverter) are never held together. Each role's permissions are
generated from that role's own code.

The MCP server's ServiceAccount holds `impersonate`, which is powerful. It
impersonates only an identity the API server has just authenticated by
`TokenReview`, never one a caller names; that bound is enforced by code and
tests, not by RBAC, so the ServiceAccount is to be treated as sensitive.

## Known limitations

Stated so that nobody relies on a property Chronos does not yet have:

- **Namespace-level, not object-level, read authorization.** A user granted
  `get` on `ChangeEvent`s in a namespace sees every change in that namespace,
  including changes to objects they could not themselves read. Object-level
  filtering by `SelfSubjectAccessReview` is the planned thin read backend; until
  it exists, bind the viewer roles as you would bind read access to the
  namespace's Secrets.
- **The field-manager name is client-chosen.** A person writing with
  `--field-manager=my-operator` is dropped as controller bookkeeping. The
  `chronos_controller_writes_suppressed_total{manager}` metric makes such a name
  visible; the audit correlator, where enabled, records the true identity for
  changes that are kept.
- **Redaction is pattern-based.** A credential under a key that names nothing
  (`data.x`) with a value that looks like nothing is stored. `ChronosConfig`
  lets an installation name such keys and fields.
- **No retention.** Records cannot be deleted by anyone while the ledger
  policy is in place. Plan for that before deploying where personal data must
  be removable.
- **Metadata-level audit only.** The correlator relies on the default
  OpenShift audit profile, which records who made a request but not its body.
  That is enough to attribute a change; it cannot reconstruct one.

## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md) at the repository root.

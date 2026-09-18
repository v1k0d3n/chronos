/*
Copyright 2026 Chronos project and its authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

// This file holds types shared across the Chronos CRDs (ChangeEvent,
// ResourceSnapshot, RevertOperation). Keeping them in one place keeps the
// "what/who/how" vocabulary consistent across the API.

// ChangeVerb is the Kubernetes API operation that produced a change.
// +kubebuilder:validation:Enum=create;update;patch;delete
type ChangeVerb string

const (
	VerbCreate ChangeVerb = "create"
	VerbUpdate ChangeVerb = "update"
	VerbPatch  ChangeVerb = "patch"
	VerbDelete ChangeVerb = "delete"
)

// ChangeSource records how Chronos observed a change. "correlated" is the
// highest-fidelity path (audit event joined with a watch-derived state diff).
// +kubebuilder:validation:Enum=watch;audit;correlated;revert
type ChangeSource string

const (
	// SourceWatch: observed via informer state diff only; actor is unknown or
	// inferred from managedFields (partial attribution).
	SourceWatch ChangeSource = "watch"
	// SourceAudit: seen in the audit log only, without a paired state snapshot.
	SourceAudit ChangeSource = "audit"
	// SourceCorrelated: audit event joined to a watch diff — the ideal case,
	// giving both a verified actor and full before/after state.
	SourceCorrelated ChangeSource = "correlated"
	// SourceRevert: the change was made by Chronos carrying out a
	// RevertOperation, and the actor is the person who requested it — an
	// identity the API server authenticated at admission.
	SourceRevert ChangeSource = "revert"
)

// AttributionConfidence expresses how reliably the actor of a change is known.
// The distribution of these values across a cluster is the "attribution
// health" signal Chronos surfaces (kubeadmin/shared identities collapse it).
// +kubebuilder:validation:Enum=verified;partial;unattributed
type AttributionConfidence string

const (
	// AttributionVerified: actor came from an audit event tied to an individual
	// identity (a real user or a specific ServiceAccount).
	AttributionVerified AttributionConfidence = "verified"
	// AttributionPartial: actor inferred from managedFields / user-agent — a
	// controller or a client, not necessarily an individual human.
	AttributionPartial AttributionConfidence = "partial"
	// AttributionUnattributed: no actor could be determined, or the change was
	// made via a shared/privileged identity (e.g. kubeadmin) — a blind spot.
	AttributionUnattributed AttributionConfidence = "unattributed"
)

// RiskLevel is a heuristic severity used for filtering and alerting. It is
// derived from the target kind (e.g. RBAC, cluster config, Secrets rank
// higher) and the verb (delete ranks higher than update).
// +kubebuilder:validation:Enum=low;medium;high;critical
type RiskLevel string

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

// TargetObjectReference identifies the cluster object a ChangeEvent,
// ResourceSnapshot, or RevertOperation concerns.
type TargetObjectReference struct {
	// APIVersion of the target object, e.g. "apps/v1" or "v1".
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Kind of the target object, e.g. "Deployment".
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Namespace of the target object. Empty for cluster-scoped objects.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name of the target object.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// UID pins the target's identity across delete/recreate cycles, so a
	// timeline can distinguish "the same object edited" from "deleted and a new
	// one created with the same name".
	// +optional
	UID string `json:"uid,omitempty"`

	// ResourceVersion of the target at the moment referenced.
	// +optional
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// Actor identifies who performed a change, to the extent Chronos can
// determine it. Even when Username is a shared account, SourceIP and UserAgent
// remain useful forensic signal.
type Actor struct {
	// Username as reported by the API server audit log, e.g. "alice" or
	// "system:serviceaccount:foo:builder". Empty when unattributed.
	// +optional
	Username string `json:"username,omitempty"`

	// UID of the acting identity, when known.
	// +optional
	UID string `json:"uid,omitempty"`

	// Groups the actor belonged to at request time.
	// +optional
	Groups []string `json:"groups,omitempty"`

	// SourceIP the request originated from (from the audit event).
	// +optional
	SourceIP string `json:"sourceIP,omitempty"`

	// UserAgent of the client that made the change, e.g. "oc/4.22.0".
	// +optional
	UserAgent string `json:"userAgent,omitempty"`

	// Confidence expresses how reliably this actor is attributed.
	// +kubebuilder:validation:Required
	Confidence AttributionConfidence `json:"confidence"`

	// Shared is true when the identity is a shared/privileged account
	// (e.g. kubeadmin, system:admin) — flagged as an attribution blind spot.
	// +optional
	Shared bool `json:"shared,omitempty"`

	// ImpersonatedUser is the identity the change was made *as*, when the
	// request used impersonation (kubectl --as). Username stays the
	// authenticated caller who authorized it.
	//
	// Both are recorded because neither alone is the truth. "kube:admin changed
	// it" hides which person was acting; "v1k0d3n changed it" hides that they
	// went through a shared admin account to do so. An auditor needs the pair.
	// +optional
	ImpersonatedUser string `json:"impersonatedUser,omitempty"`

	// ImpersonatedGroups are the groups asserted for the impersonated identity.
	// +optional
	ImpersonatedGroups []string `json:"impersonatedGroups,omitempty"`
}

// RedactionReason categorizes why a field's value was stripped before storage.
// See the Chronos security model: sensitive values are never persisted.
// +kubebuilder:validation:Enum=secret-data;dockerconfig;serviceaccount-token;sensitive-pattern;last-applied;policy
type RedactionReason string

const (
	// RedactionSecretData: a value under a Secret's data or stringData.
	RedactionSecretData RedactionReason = "secret-data"
	// RedactionDockerConfig: registry credentials.
	RedactionDockerConfig RedactionReason = "dockerconfig"
	// RedactionServiceAccountToken: a service account token.
	RedactionServiceAccountToken RedactionReason = "serviceaccount-token"
	// RedactionSensitivePattern: on any kind, a key that names a credential
	// (password, token, …) or a value shaped like one (a private key, a JWT, a
	// URL with a password, a recognisable token prefix).
	RedactionSensitivePattern RedactionReason = "sensitive-pattern"
	// RedactionLastApplied: the kubectl last-applied-configuration annotation,
	// which embeds a full copy of the object as it was applied. Removed rather
	// than blanked.
	RedactionLastApplied RedactionReason = "last-applied"
	// RedactionPolicy: a field named by a ChronosConfig redaction rule.
	RedactionPolicy RedactionReason = "policy"
)

// RedactionEntry records a single field whose value was redacted at capture
// time. The Hash lets a diff truthfully show "value changed (a1b2 -> c3d4)"
// without ever storing or revealing the value itself.
type RedactionEntry struct {
	// FieldPath is an RFC 6901 JSON Pointer to the redacted field, e.g.
	// "/data/tls.key" or "/spec/template/spec/containers/0/env/2/value". A
	// pointer rather than a dotted path because ConfigMap keys and annotation
	// keys routinely contain dots and slashes.
	// +kubebuilder:validation:Required
	FieldPath string `json:"fieldPath"`

	// Reason categorizes why the field was redacted.
	// +kubebuilder:validation:Required
	Reason RedactionReason `json:"reason"`

	// Hash is a keyed digest (HMAC) of the redacted value, so two snapshots can
	// show that a withheld value changed without either revealing it. It is
	// not a plain hash of the value: that would let anyone who can read a
	// snapshot test guesses against it. Empty if the field was absent, or was
	// removed rather than blanked.
	// +optional
	Hash string `json:"hash,omitempty"`

	// KeyID identifies the key the hash was made under. Hashes are only
	// comparable when their KeyIDs match; after a key rotation, older
	// snapshots' hashes cannot be compared with newer ones.
	// +optional
	KeyID string `json:"keyId,omitempty"`
}

// StorageReference points at snapshot content held outside the CRD (e.g. on a
// PVC or object store) when inlining it would bloat etcd. Nil for the POC,
// where small snapshots are stored inline.
type StorageReference struct {
	// Backend identifies the store, e.g. "pvc" or "s3".
	// +kubebuilder:validation:Required
	Backend string `json:"backend"`

	// Key is the path/object key within the backend.
	// +kubebuilder:validation:Required
	Key string `json:"key"`

	// SizeBytes of the stored content.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

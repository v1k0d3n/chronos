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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RevertStrategy selects how a revert is applied to the live object.
// +kubebuilder:validation:Enum=ServerSideApply;DeleteAndRecreate;Auto
type RevertStrategy string

const (
	// StrategyServerSideApply reconstructs the prior spec and applies it via
	// server-side apply. The default and safest strategy for mutable fields.
	StrategyServerSideApply RevertStrategy = "ServerSideApply"
	// StrategyDeleteAndRecreate deletes the object and recreates it from the
	// snapshot, for when immutable fields changed. Declared, not implemented:
	// a request asking for it is refused rather than silently applied.
	StrategyDeleteAndRecreate RevertStrategy = "DeleteAndRecreate"
	// StrategyAuto lets the controller pick: ServerSideApply unless an
	// immutable field differs, in which case it escalates (subject to Force).
	StrategyAuto RevertStrategy = "Auto"
)

// RevertPhase is the lifecycle phase of a RevertOperation.
// +kubebuilder:validation:Enum=Pending;Validating;InProgress;Succeeded;Failed;Skipped
type RevertPhase string

const (
	RevertPending    RevertPhase = "Pending"
	RevertValidating RevertPhase = "Validating"
	RevertInProgress RevertPhase = "InProgress"
	RevertSucceeded  RevertPhase = "Succeeded"
	RevertFailed     RevertPhase = "Failed"
	// RevertSkipped: the live object already matches the target state, so there
	// was nothing to do.
	RevertSkipped RevertPhase = "Skipped"
)

// RevertOperationSpec requests that a single object be restored to a prior
// state. Exactly one of ChangeEventRef or ToSnapshot identifies the target
// state. The revert is itself recorded as a new ChangeEvent, so undoing a
// revert is just another point on the timeline.
type RevertOperationSpec struct {
	// Target is the object to revert. Required.
	// +kubebuilder:validation:Required
	Target TargetObjectReference `json:"target"`

	// ChangeEventRef names a ChangeEvent to undo; the object is restored to that
	// event's BeforeSnapshot. Mutually exclusive with ToSnapshot.
	// +optional
	ChangeEventRef string `json:"changeEventRef,omitempty"`

	// ToSnapshot names a ResourceSnapshot to restore the object to directly.
	// Mutually exclusive with ChangeEventRef.
	// +optional
	ToSnapshot string `json:"toSnapshot,omitempty"`

	// Strategy selects how the revert is applied. Defaults to Auto.
	// +optional
	// +kubebuilder:default=Auto
	Strategy RevertStrategy `json:"strategy,omitempty"`

	// DryRun computes and reports the revert diff without mutating the cluster.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`

	// Force permits an escalation to DeleteAndRecreate when immutable fields
	// differ. Without it, such reverts fail rather than delete the object.
	// +optional
	Force bool `json:"force,omitempty"`

	// RequestedBy is who asked for this revert. Clients do not set it: the
	// admission webhook writes it from the identity the API server
	// authenticated, replacing anything supplied in the request. The controller
	// performs the revert only if this identity could have made the same change
	// to the target directly.
	// +optional
	RequestedBy *Requester `json:"requestedBy,omitempty"`
}

// Requester is the authenticated identity behind a RevertOperation, as the API
// server reported it at admission. It carries everything an authorization
// decision depends on, so that the decision made for the revert is the one the
// API server would have made for the person.
type Requester struct {
	// Username is the authenticated user name.
	Username string `json:"username"`

	// UID is the user's unique identifier, where the authenticator provides one.
	// +optional
	UID string `json:"uid,omitempty"`

	// Groups are the groups the user belonged to when the request was made.
	// +optional
	// +listType=atomic
	Groups []string `json:"groups,omitempty"`

	// Extra holds authenticator-specific attributes (for example OAuth scopes),
	// which authorizers may take into account.
	// +optional
	Extra map[string][]string `json:"extra,omitempty"`
}

// RevertOperationStatus reports the outcome of a revert.
type RevertOperationStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase RevertPhase `json:"phase,omitempty"`

	// AppliedStrategy is the strategy actually used (relevant when Spec.Strategy
	// was Auto).
	// +optional
	AppliedStrategy RevertStrategy `json:"appliedStrategy,omitempty"`

	// Message is a human-readable status detail.
	// +optional
	Message string `json:"message,omitempty"`

	// Diff is a redaction-safe summary of what the revert changed (or would
	// change, for a dry run).
	// +optional
	Diff string `json:"diff,omitempty"`

	// ManualStepsRequired lists actions a human must take to complete the
	// revert — chiefly re-supplying redacted Secret values, which Chronos
	// deliberately never stores.
	// +optional
	ManualStepsRequired []string `json:"manualStepsRequired,omitempty"`

	// ResultingChangeEvent is the name of the ChangeEvent recorded for this
	// revert itself.
	// +optional
	ResultingChangeEvent string `json:"resultingChangeEvent,omitempty"`

	// StartedAt is when actuation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the revert reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ObservedGeneration is the .metadata.generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest observations of the revert's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=chronos,shortName=revop
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.target.kind`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.status.appliedStrategy`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RevertOperation is the Schema for the revertoperations API.
type RevertOperation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// A RevertOperation is a request made once, by one person, for one change.
	// If its spec could be edited after admission, the identity recorded on it
	// would no longer describe who asked for what is now being requested.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; create a new RevertOperation instead"
	Spec   RevertOperationSpec   `json:"spec,omitempty"`
	Status RevertOperationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RevertOperationList contains a list of RevertOperation.
type RevertOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RevertOperation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RevertOperation{}, &RevertOperationList{})
}

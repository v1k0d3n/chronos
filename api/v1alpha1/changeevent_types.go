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

// ChangeEventSpec is the immutable record of a single observed change to a
// cluster object: what changed, when, who did it, and how to find the
// before/after state. It is written by the Chronos watch controller and audit
// correlator, and is never mutated after creation.
type ChangeEventSpec struct {
	// ObservedAt is when the change occurred — the audit requestReceivedTimestamp
	// when correlated, otherwise the time the watch observed it.
	// +kubebuilder:validation:Required
	ObservedAt metav1.Time `json:"observedAt"`

	// Verb is the API operation that produced the change.
	// +kubebuilder:validation:Required
	Verb ChangeVerb `json:"verb"`

	// Target is the object that changed.
	// +kubebuilder:validation:Required
	Target TargetObjectReference `json:"target"`

	// Actor is who performed the change, to the extent it can be attributed.
	// +kubebuilder:validation:Required
	Actor Actor `json:"actor"`

	// Source records how the change was observed (watch, audit, or correlated).
	// +kubebuilder:validation:Required
	Source ChangeSource `json:"source"`

	// RiskLevel is a heuristic severity for filtering and alerting.
	// +optional
	RiskLevel RiskLevel `json:"riskLevel,omitempty"`

	// Summary is a one-line, redaction-safe description of the change,
	// e.g. "spec.replicas 3 -> 0".
	// +optional
	Summary string `json:"summary,omitempty"`

	// ChangedFields lists the field paths that changed, e.g.
	// ["spec.replicas", "spec.template.spec.containers[0].image"]. Safe to show
	// even to users who cannot see the values.
	// +optional
	ChangedFields []string `json:"changedFields,omitempty"`

	// BeforeSnapshot is the name of the ResourceSnapshot holding the object's
	// state prior to this change, in the same namespace as this ChangeEvent.
	// Empty for create events.
	// +optional
	BeforeSnapshot string `json:"beforeSnapshot,omitempty"`

	// AfterSnapshot is the name of the ResourceSnapshot holding the object's
	// state after this change. Empty for delete events.
	// +optional
	AfterSnapshot string `json:"afterSnapshot,omitempty"`

	// Redacted is true when sensitive values were stripped from the referenced
	// snapshots (e.g. the target is a Secret). Reverts of redacted changes may
	// require manual steps.
	// +optional
	Redacted bool `json:"redacted,omitempty"`
}

// ChangeEventStatus captures whether this change has since been reverted.
type ChangeEventStatus struct {
	// Reverted is true once a RevertOperation has undone this change.
	// +optional
	Reverted bool `json:"reverted,omitempty"`

	// RevertOperation is the name of the RevertOperation that reverted this
	// change, in the same namespace.
	// +optional
	RevertOperation string `json:"revertOperation,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=chronos,shortName=chce
// +kubebuilder:printcolumn:name="Verb",type=string,JSONPath=`.spec.verb`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.target.kind`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Actor",type=string,JSONPath=`.spec.actor.username`
// +kubebuilder:printcolumn:name="Confidence",type=string,JSONPath=`.spec.actor.confidence`
// +kubebuilder:printcolumn:name="Risk",type=string,JSONPath=`.spec.riskLevel`
// +kubebuilder:printcolumn:name="When",type=date,JSONPath=`.spec.observedAt`

// ChangeEvent is the Schema for the changeevents API. Each ChangeEvent is one
// point on the Chronos "Time Machine" timeline.
type ChangeEvent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// A ChangeEvent records that something happened. What happened is fixed at
	// the moment it is written; only three things about it may be learned later:
	// who did it (actor and source, filled in by the audit correlator, and
	// write-once — a verified actor is never revised); whether it was
	// subsequently reverted is recorded in status.
	// +kubebuilder:validation:XValidation:rule="self.observedAt == oldSelf.observedAt && self.verb == oldSelf.verb && self.target == oldSelf.target && (has(self.riskLevel) == has(oldSelf.riskLevel) && (!has(self.riskLevel) || self.riskLevel == oldSelf.riskLevel)) && (has(self.summary) == has(oldSelf.summary) && (!has(self.summary) || self.summary == oldSelf.summary)) && (has(self.changedFields) == has(oldSelf.changedFields) && (!has(self.changedFields) || self.changedFields == oldSelf.changedFields)) && (has(self.beforeSnapshot) == has(oldSelf.beforeSnapshot) && (!has(self.beforeSnapshot) || self.beforeSnapshot == oldSelf.beforeSnapshot)) && (has(self.afterSnapshot) == has(oldSelf.afterSnapshot) && (!has(self.afterSnapshot) || self.afterSnapshot == oldSelf.afterSnapshot)) && (has(self.redacted) == has(oldSelf.redacted) && (!has(self.redacted) || self.redacted == oldSelf.redacted))",message="a ChangeEvent's record of what changed is immutable"
	// +kubebuilder:validation:XValidation:rule="oldSelf.actor.confidence != 'verified' || (self.actor == oldSelf.actor && self.source == oldSelf.source)",message="a verified actor is write-once"
	Spec   ChangeEventSpec   `json:"spec,omitempty"`
	Status ChangeEventStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ChangeEventList contains a list of ChangeEvent.
type ChangeEventList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ChangeEvent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ChangeEvent{}, &ChangeEventList{})
}

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
	"k8s.io/apimachinery/pkg/runtime"
)

// ResourceSnapshotSpec stores a point-in-time, redacted copy of a cluster
// object. Snapshots are the substrate for both diffs (before vs after) and
// surgical revert (re-apply a prior snapshot). Sensitive values are stripped
// at capture time — see Redactions — so the store never becomes a honeypot of
// Secrets, dockerconfigs, or tokens.
type ResourceSnapshotSpec struct {
	// Target is the object this snapshot captures.
	// +kubebuilder:validation:Required
	Target TargetObjectReference `json:"target"`

	// CapturedAt is when the snapshot was taken.
	// +kubebuilder:validation:Required
	CapturedAt metav1.Time `json:"capturedAt"`

	// Content is the redacted object manifest, inlined for the POC. Sensitive
	// fields have been removed and recorded in Redactions. Nil if the content
	// was offloaded to ContentRef instead.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Content *runtime.RawExtension `json:"content,omitempty"`

	// ContentRef points at snapshot content stored outside the CRD when it is
	// too large to inline. Mutually exclusive with Content.
	// +optional
	ContentRef *StorageReference `json:"contentRef,omitempty"`

	// Redactions lists every field whose value was stripped before storage,
	// with a hash so diffs can show "value changed" without the value.
	// +optional
	Redactions []RedactionEntry `json:"redactions,omitempty"`

	// Redacted is true when any sensitive value was stripped from Content.
	// Always serialized (no omitempty) so "false" is explicit, not absent.
	// +optional
	Redacted bool `json:"redacted"`

	// Revertable indicates whether this snapshot alone is sufficient to restore
	// the object. False when redaction removed values required to fully
	// reconstruct it (e.g. a Secret's data) — such reverts need manual steps.
	// Always serialized (no omitempty) so "false" is explicit, not absent.
	// +optional
	Revertable bool `json:"revertable"`
}

// ResourceSnapshotStatus defines the observed state of ResourceSnapshot.
type ResourceSnapshotStatus struct {
	// SizeBytes is the serialized size of the stored content, for retention
	// accounting.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=chronos,shortName=rsnap
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.target.kind`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Redacted",type=boolean,JSONPath=`.spec.redacted`
// +kubebuilder:printcolumn:name="Revertable",type=boolean,JSONPath=`.spec.revertable`
// +kubebuilder:printcolumn:name="Captured",type=date,JSONPath=`.spec.capturedAt`

// ResourceSnapshot is the Schema for the resourcesnapshots API.
type ResourceSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// A snapshot is evidence: what an object looked like at one moment. Evidence
	// that can be edited is not evidence, so nothing in spec may change once
	// written — not even by Chronos.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a ResourceSnapshot is immutable"
	Spec   ResourceSnapshotSpec   `json:"spec,omitempty"`
	Status ResourceSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ResourceSnapshotList contains a list of ResourceSnapshot.
type ResourceSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceSnapshot{}, &ResourceSnapshotList{})
}

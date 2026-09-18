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

// FilterPolicy is the set of tunable noise filters. Every field is optional:
// a nil pointer / omitted slice means "inherit" (fall back to the built-in
// defaults for global policy, or to the global policy for a namespace
// override). This keeps the config declarative and layered without a DSL.
type FilterPolicy struct {
	// ExcludeControllerActors drops changes whose actor looks like a controller
	// or operator (the "human vs controller" default). Nil inherits.
	// +optional
	ExcludeControllerActors *bool `json:"excludeControllerActors,omitempty"`

	// ExcludeNoiseSecrets drops changes to auto-generated Secret types. Nil
	// inherits.
	// +optional
	ExcludeNoiseSecrets *bool `json:"excludeNoiseSecrets,omitempty"`

	// ExcludeNamespaces are glob patterns (a trailing "*" is a prefix, e.g.
	// "openshift-*") of namespaces to drop. A non-nil list replaces the
	// inherited one. Cluster-scoped objects are never filtered by namespace.
	// +optional
	ExcludeNamespaces []string `json:"excludeNamespaces,omitempty"`

	// ExcludeSecretTypes are Secret .type values to drop (e.g.
	// "kubernetes.io/dockercfg"). A non-nil list replaces the inherited one.
	// +optional
	ExcludeSecretTypes []string `json:"excludeSecretTypes,omitempty"`

	// ExcludeFields are field-path prefixes whose changes are ignored, e.g.
	// "metadata" or "spec.template.metadata". A change with no remaining
	// meaningful fields is dropped.
	// +optional
	ExcludeFields []string `json:"excludeFields,omitempty"`

	// IncludeFields are field-path prefixes that re-include an otherwise
	// excluded field — e.g. ExcludeFields ["metadata"] plus IncludeFields
	// ["metadata.annotations"] means "all metadata except annotations".
	// +optional
	IncludeFields []string `json:"includeFields,omitempty"`

	// RedactKeys are regular expressions naming credentials by key, in
	// addition to the built-in ones (password, token, secret, key, …). They are
	// matched against ConfigMap keys, environment variable names and annotation
	// keys; a match redacts the value before it is stored. Unlike the Exclude
	// lists, these add to what is inherited rather than replacing it.
	// +optional
	RedactKeys []string `json:"redactKeys,omitempty"`

	// RedactFields are RFC 6901 JSON Pointers to fields that are always
	// redacted when present, on any watched kind — e.g. "/data/config.yaml"
	// for a ConfigMap whose whole config file embeds credentials. They add to
	// what is inherited.
	// +optional
	RedactFields []string `json:"redactFields,omitempty"`
}

// NamespacePolicy overrides the global FilterPolicy for one namespace.
type NamespacePolicy struct {
	// Namespace this override applies to.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	FilterPolicy `json:",inline"`
}

// ChronosConfigSpec tunes what Chronos records. Defaults apply cluster-wide;
// NamespaceOverrides refine them per namespace (override wins over default,
// default wins over built-in). The operator reads the singleton named
// "cluster".
type ChronosConfigSpec struct {
	// Defaults applied cluster-wide, layered over the built-in defaults.
	// +optional
	Defaults FilterPolicy `json:"defaults,omitempty"`

	// NamespaceOverrides tune the policy for specific namespaces.
	// +optional
	// +listType=map
	// +listMapKey=namespace
	NamespaceOverrides []NamespacePolicy `json:"namespaceOverrides,omitempty"`
}

// ChronosConfigStatus defines the observed state of ChronosConfig.
type ChronosConfigStatus struct {
	// ObservedGeneration is the generation the operator last applied.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Message is a human-readable status detail.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=chronos,shortName=chcfg
// +kubebuilder:printcolumn:name="Overrides",type=integer,JSONPath=`.spec.namespaceOverrides[*].namespace`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ChronosConfig tunes Chronos noise filtering. It is a cluster-scoped
// singleton; the operator reads the one named "cluster".
type ChronosConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ChronosConfigSpec   `json:"spec,omitempty"`
	Status ChronosConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ChronosConfigList contains a list of ChronosConfig.
type ChronosConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ChronosConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ChronosConfig{}, &ChronosConfigList{})
}

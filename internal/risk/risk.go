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

// Package risk assigns a heuristic RiskLevel to a change, used for timeline
// filtering and (later) alerting. It is intentionally simple: risk rises with
// the blast radius of the kind (RBAC, cluster config, Secrets rank high) and
// with destructive verbs (delete escalates one level).
package risk

import (
	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
)

// highBaseline kinds change security or cluster-wide behavior; a change to one
// is high risk before any verb escalation.
var highBaseline = map[string]bool{
	"Secret":                         true,
	"ClusterRole":                    true,
	"ClusterRoleBinding":             true,
	"Role":                           true,
	"RoleBinding":                    true,
	"ValidatingWebhookConfiguration": true,
	"MutatingWebhookConfiguration":   true,
	"CustomResourceDefinition":       true,
	"SecurityContextConstraints":     true,
	"APIServer":                      true,
	"OAuth":                          true,
	"Authentication":                 true,
	"Network":                        true,
	"MachineConfig":                  true,
	"Node":                           true,
	"NetworkPolicy":                  true,
}

// mediumBaseline kinds affect workloads or app-level networking.
var mediumBaseline = map[string]bool{
	"Deployment":            true,
	"DaemonSet":             true,
	"StatefulSet":           true,
	"ReplicaSet":            true,
	"Service":               true,
	"ConfigMap":             true,
	"ServiceAccount":        true,
	"Ingress":               true,
	"Route":                 true,
	"PersistentVolumeClaim": true,
}

// Classify returns the risk level for a change to the given kind under verb.
func Classify(group, kind, verb string) chronosv1alpha1.RiskLevel {
	base := baseline(kind)
	if verb == string(chronosv1alpha1.VerbDelete) {
		base = escalate(base)
	}
	return base
}

func baseline(kind string) chronosv1alpha1.RiskLevel {
	switch {
	case highBaseline[kind]:
		return chronosv1alpha1.RiskHigh
	case mediumBaseline[kind]:
		return chronosv1alpha1.RiskMedium
	default:
		return chronosv1alpha1.RiskLow
	}
}

func escalate(r chronosv1alpha1.RiskLevel) chronosv1alpha1.RiskLevel {
	switch r {
	case chronosv1alpha1.RiskLow:
		return chronosv1alpha1.RiskMedium
	case chronosv1alpha1.RiskMedium:
		return chronosv1alpha1.RiskHigh
	case chronosv1alpha1.RiskHigh, chronosv1alpha1.RiskCritical:
		return chronosv1alpha1.RiskCritical
	default:
		return r
	}
}

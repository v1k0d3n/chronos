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

package watcher

import (
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DefaultResources is the curated set of resource types Chronos watches out of
// the box. It targets the objects customers most often change to break or
// degrade a cluster, while deliberately excluding high-churn, low-signal kinds
// (Pods, Events, EndpointSlices, Leases, ReplicaSets) that would flood the
// timeline. This list is the seed for a future ChronosConfig watched-resource
// setting.
func DefaultResources() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{
		// Workloads.
		{Group: "apps", Version: "v1", Resource: "deployments"},
		{Group: "apps", Version: "v1", Resource: "daemonsets"},
		{Group: "apps", Version: "v1", Resource: "statefulsets"},

		// Core config & networking.
		{Group: "", Version: "v1", Resource: "configmaps"},
		{Group: "", Version: "v1", Resource: "secrets"},
		{Group: "", Version: "v1", Resource: "services"},
		{Group: "", Version: "v1", Resource: "serviceaccounts"},

		// RBAC — high-value for "who granted themselves what".
		{Group: rbacv1.GroupName, Version: "v1", Resource: "roles"},
		{Group: rbacv1.GroupName, Version: "v1", Resource: "rolebindings"},
		{Group: rbacv1.GroupName, Version: "v1", Resource: "clusterroles"},
		{Group: rbacv1.GroupName, Version: "v1", Resource: "clusterrolebindings"},

		// Network policy.
		{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
	}
}

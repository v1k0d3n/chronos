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

import "testing"

func TestIsClusterScoped(t *testing.T) {
	clusterScoped := 0
	for _, gvr := range DefaultResources() {
		switch gvr.Resource {
		case "clusterroles", "clusterrolebindings":
			if !isClusterScoped(gvr) {
				t.Errorf("%s should be cluster-scoped", gvr.Resource)
			}
			clusterScoped++
		default:
			if isClusterScoped(gvr) {
				t.Errorf("%s should be namespaced", gvr.Resource)
			}
		}
	}
	// Exactly the two RBAC cluster-scoped kinds; the rest are namespaced. This
	// guards the split that keeps namespace-scoped watching correct — a
	// namespaced factory would never see a cluster-scoped resource.
	if clusterScoped != 2 {
		t.Fatalf("expected 2 cluster-scoped watched kinds, got %d", clusterScoped)
	}
}

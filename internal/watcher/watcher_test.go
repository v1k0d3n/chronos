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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
)

func TestIsControllerActor(t *testing.T) {
	controllers := []string{
		"kube-controller-manager",
		"backplane-operator",
		"deployment-controller",
		"openshift.io/image-registry-pull-secrets_image-pull-secret-controller",
		"cluster-version-operator",
	}
	humans := []string{"kubectl-create", "kubectl-client-side-apply", "oc", "console", "alice", ""}

	for _, c := range controllers {
		if !isControllerActor(c) {
			t.Errorf("expected %q to be classified as a controller", c)
		}
	}
	for _, h := range humans {
		if isControllerActor(h) {
			t.Errorf("expected %q to be classified as human", h)
		}
	}
}

func secretOfType(secretType string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       secretType,
	}}
}

func TestSecretHelpers(t *testing.T) {
	s := secretOfType("kubernetes.io/dockercfg")
	if !isSecret(s) {
		t.Error("expected a Secret")
	}
	if got := secretTypeOf(s); got != "kubernetes.io/dockercfg" {
		t.Errorf("secretTypeOf = %q, want kubernetes.io/dockercfg", got)
	}
	cm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
	}}
	if isSecret(cm) {
		t.Error("ConfigMap should not be a Secret")
	}
}

// observedAt must not be taken from managedFields.
//
// Regression test for the defect that corrupted attribution. A managedFields
// entry records when a field manager's ownership set last changed, not when the
// object changed. Scaling a Deployment touches a field the manager already owns,
// so the entry keeps a stale timestamp — measured 15 minutes stale on one change
// and 20 seconds on another.
//
// Twenty seconds was enough. The audit correlator matches by time, so the
// agent's change landed on the previous actor's audit record and was attributed
// to them with "verified" confidence.
func TestObservedAtIgnoresStaleManagedFields(t *testing.T) {
	stale := metav1.NewTime(time.Now().Add(-15 * time.Minute))
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "web",
			"namespace": "demo",
		},
	}}
	u.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: "kubectl-scale", Time: &stale},
	})

	got := observedAt(string(chronosv1alpha1.VerbUpdate), u)

	if got.Time.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("observedAt = %s; a stale managedFields entry must not be used", got)
	}
}

// A create has an exact timestamp on the object, and it beats observation time
// because it is the moment the change actually happened.
func TestObservedAtUsesCreationTimestampForCreates(t *testing.T) {
	created := metav1.NewTime(time.Now().Add(-90 * time.Second).Truncate(time.Second))
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "cm",
			"namespace": "demo",
		},
	}}
	u.SetCreationTimestamp(created)

	got := observedAt(string(chronosv1alpha1.VerbCreate), u)

	if !got.Time.Equal(created.Time) {
		t.Errorf("observedAt = %s, want the creationTimestamp %s", got, created)
	}
}

// An object with no creationTimestamp must still produce a usable time rather
// than the zero value, which would place the change at year 1 and make it
// unattributable forever.
func TestObservedAtNeverReturnsZero(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "cm", "namespace": "demo"},
	}}

	for _, verb := range []string{
		string(chronosv1alpha1.VerbCreate),
		string(chronosv1alpha1.VerbUpdate),
		string(chronosv1alpha1.VerbDelete),
	} {
		if got := observedAt(verb, u); got.IsZero() {
			t.Errorf("verb %s produced a zero observedAt", verb)
		}
	}
}

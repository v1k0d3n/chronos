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
	"github.com/v1k0d3n/chronos/internal/diff"
)

// A Deployment as the API server holds it after a person applied it and the
// deployment controller then wrote its revision annotation. The controller's
// entry is the most recent one — the situation that used to hide the person.
func deploymentWithManagers() *unstructured.Unstructured {
	t0 := metav1.NewTime(time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC))
	t1 := metav1.NewTime(t0.Add(2 * time.Second))
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name": "api", "namespace": "team-a",
			"annotations": map[string]interface{}{
				"deployment.kubernetes.io/revision": "3",
				"team":                              "payments",
			},
		},
		"spec": map[string]interface{}{
			"replicas": int64(3),
			"template": map[string]interface{}{"spec": map[string]interface{}{
				"containers": []interface{}{map[string]interface{}{
					"name":  "api",
					"image": "example/api:2",
					"env": []interface{}{
						map[string]interface{}{"name": "LOG_LEVEL", "value": "debug"},
					},
					"ports": []interface{}{
						map[string]interface{}{"containerPort": int64(8080), "protocol": "TCP"},
					},
				}},
			}},
		},
	}}
	u.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate, Time: &t0,
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{
				"f:metadata":{"f:annotations":{".":{},"f:team":{}}},
				"f:spec":{
					"f:replicas":{},
					"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"api\"}":{
						".":{},"f:name":{},"f:image":{},
						"f:env":{".":{},"k:{\"name\":\"LOG_LEVEL\"}":{".":{},"f:name":{},"f:value":{}}},
						"f:ports":{".":{},"k:{\"containerPort\":8080,\"protocol\":\"TCP\"}":{".":{},"f:containerPort":{},"f:protocol":{}}}
					}}}}
				}
			}`)},
		},
		{
			Manager: "kube-controller-manager", Operation: metav1.ManagedFieldsOperationUpdate, Time: &t1,
			FieldsType: "FieldsV1",
			FieldsV1:   &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:annotations":{"f:deployment.kubernetes.io/revision":{}}}}`)},
		},
		{
			Manager: "kube-controller-manager", Operation: metav1.ManagedFieldsOperationUpdate, Time: &t1,
			Subresource: "status", FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:replicas":{}}}`)},
		},
	})
	return u
}

func changed(paths ...[]string) []diff.Change {
	out := make([]diff.Change, 0, len(paths))
	for _, p := range paths {
		out = append(out, diff.Change{Path: p})
	}
	return out
}

func TestUpdateIsCreditedToWhoeverOwnsTheChangedFields(t *testing.T) {
	u := deploymentWithManagers()

	actor, bookkeeping := attribution(u, "update", changed([]string{"spec", "replicas"}))

	if actor.Username != "kubectl-client-side-apply" {
		t.Errorf("a person's scale was credited to %q", actor.Username)
	}
	if bookkeeping {
		t.Error("a person's scale was dropped as controller bookkeeping")
	}
	if actor.Confidence != chronosv1alpha1.AttributionPartial {
		t.Errorf("confidence = %q; managedFields is never more than partial", actor.Confidence)
	}
}

func TestControllerBookkeepingIsStillRecognised(t *testing.T) {
	u := deploymentWithManagers()

	actor, bookkeeping := attribution(u, "update",
		changed([]string{"metadata", "annotations", "deployment.kubernetes.io/revision"}))

	if actor.Username != "kube-controller-manager" || !bookkeeping {
		t.Errorf("revision bump: actor=%q bookkeeping=%v; want the controller, bookkeeping", actor.Username, bookkeeping)
	}
}

func TestCreateIsCreditedToTheCreatorNotTheLatestWriter(t *testing.T) {
	u := deploymentWithManagers()

	actor, bookkeeping := attribution(u, "create", nil)

	if actor.Username != "kubectl-client-side-apply" || bookkeeping {
		t.Errorf("create: actor=%q bookkeeping=%v; the controller wrote last but did not create it", actor.Username, bookkeeping)
	}
}

func TestAPersonAmongControllersWins(t *testing.T) {
	u := deploymentWithManagers()

	actor, bookkeeping := attribution(u, "update", changed(
		[]string{"metadata", "annotations", "deployment.kubernetes.io/revision"},
		[]string{"spec", "replicas"},
	))

	if actor.Username != "kubectl-client-side-apply" || bookkeeping {
		t.Errorf("mixed change: actor=%q bookkeeping=%v", actor.Username, bookkeeping)
	}
}

// List items are addressed in managedFields by key, value or index, and the
// key fields are whatever the schema says — which the watcher does not have.
// They are resolved against the item itself.
func TestOwnershipResolvesKeyedListItems(t *testing.T) {
	u := deploymentWithManagers()
	for _, path := range [][]string{
		{"spec", "template", "spec", "containers", "0", "image"},
		{"spec", "template", "spec", "containers", "0", "env", "0", "value"},
		{"spec", "template", "spec", "containers", "0", "ports", "0", "containerPort"},
	} {
		got := managersOfChange(u, changed(path))
		if got["kubectl-client-side-apply"] != 1 || len(got) != 1 {
			t.Errorf("%v: owners = %v, want kubectl alone", path, got)
		}
	}
}

// A field nobody claims must not be dropped: unknown is a reason to record.
func TestUnknownOwnershipIsKeptAndCreditedToTheLatestWriter(t *testing.T) {
	u := deploymentWithManagers()

	actor, bookkeeping := attribution(u, "update", changed([]string{"spec", "paused"}))

	if bookkeeping {
		t.Error("a change nobody owns was dropped")
	}
	if actor.Username != "kube-controller-manager" {
		t.Errorf("fallback actor = %q, want the most recent writer", actor.Username)
	}
}

func TestNoManagedFieldsIsUnattributedNotDropped(t *testing.T) {
	u := deploymentWithManagers()
	u.SetManagedFields(nil)

	actor, bookkeeping := attribution(u, "update", changed([]string{"spec", "replicas"}))

	if bookkeeping || actor.Confidence != chronosv1alpha1.AttributionUnattributed {
		t.Errorf("without managedFields: actor=%+v bookkeeping=%v", actor, bookkeeping)
	}
}

func TestDeleteIsUnattributed(t *testing.T) {
	actor, bookkeeping := attribution(deploymentWithManagers(), "delete", nil)
	if bookkeeping || actor.Confidence != chronosv1alpha1.AttributionUnattributed {
		t.Errorf("delete: actor=%+v bookkeeping=%v", actor, bookkeeping)
	}
}

// The field-manager name is chosen by the client. Someone who writes with
// --field-manager=my-operator is hidden by design of the filter; this test
// pins that limitation so a change to it is deliberate. The suppression
// metric, labelled by manager, is what makes such a name visible.
func TestAClientChosenControllerNameStillHides(t *testing.T) {
	u := deploymentWithManagers()
	mf := u.GetManagedFields()
	mf[0].Manager = "my-operator"
	u.SetManagedFields(mf)

	_, bookkeeping := attribution(u, "update", changed([]string{"spec", "replicas"}))
	if !bookkeeping {
		t.Error("expected the name-based rule to apply; if this is now smarter, update the docs too")
	}
}

// After `oc create` then `oc patch`, the creator still owns the map's
// existence (`"f:data":{".":{}}`) while the patcher owns the key. Those two
// claims look identical in managedFields; the deeper one is the real owner.
func TestTheMostSpecificClaimWins(t *testing.T) {
	t0 := metav1.NewTime(time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC))
	t1 := metav1.NewTime(t0.Add(time.Minute))
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "app", "namespace": "team-a"},
		"data":     map[string]interface{}{"setting": "broken"},
	}}
	u.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: "kubectl-create", Operation: metav1.ManagedFieldsOperationUpdate, Time: &t0, FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{".":{}}}`)}},
		{Manager: "kubectl-patch", Operation: metav1.ManagedFieldsOperationUpdate, Time: &t1, FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{"f:setting":{}}}`)}},
	})

	got := managersOfChange(u, changed([]string{"data", "setting"}))
	if len(got) != 1 || got["kubectl-patch"] != 1 {
		t.Errorf("owners = %v, want kubectl-patch alone", got)
	}
}

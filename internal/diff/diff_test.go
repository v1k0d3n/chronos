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

package diff

import (
	"reflect"
	"testing"
)

func obj(m map[string]interface{}) map[string]interface{} { return m }

func TestCompute_ScalarUpdate(t *testing.T) {
	old := obj(map[string]interface{}{
		"spec": map[string]interface{}{"replicas": int64(3)},
	})
	newObj := obj(map[string]interface{}{
		"spec": map[string]interface{}{"replicas": int64(0)},
	})

	got := Compute(old, newObj)
	if want := []string{"spec.replicas"}; !reflect.DeepEqual(got.ChangedFields, want) {
		t.Errorf("ChangedFields = %v, want %v", got.ChangedFields, want)
	}
	if want := "spec.replicas: 3 -> 0"; got.Summary != want {
		t.Errorf("Summary = %q, want %q", got.Summary, want)
	}
}

func TestCompute_StatusOnlyIsNoOp(t *testing.T) {
	old := obj(map[string]interface{}{
		"spec":   map[string]interface{}{"replicas": int64(3)},
		"status": map[string]interface{}{"readyReplicas": int64(3)},
	})
	newObj := obj(map[string]interface{}{
		"spec":   map[string]interface{}{"replicas": int64(3)},
		"status": map[string]interface{}{"readyReplicas": int64(0)},
	})

	got := Compute(old, newObj)
	if len(got.ChangedFields) != 0 || got.Summary != "no-op" {
		t.Errorf("status-only change should be no-op, got %+v", got)
	}
}

func TestCompute_MetadataChurnIsNoOp(t *testing.T) {
	old := obj(map[string]interface{}{
		"metadata": map[string]interface{}{"name": "x", "resourceVersion": "100", "generation": int64(1)},
		"spec":     map[string]interface{}{"replicas": int64(3)},
	})
	newObj := obj(map[string]interface{}{
		"metadata": map[string]interface{}{"name": "x", "resourceVersion": "101", "generation": int64(2)},
		"spec":     map[string]interface{}{"replicas": int64(3)},
	})

	got := Compute(old, newObj)
	if len(got.ChangedFields) != 0 {
		t.Errorf("resourceVersion/generation churn should be ignored, got %v", got.ChangedFields)
	}
}

func TestCompute_LastAppliedAnnotationIgnored(t *testing.T) {
	mk := func(rv string) map[string]interface{} {
		return map[string]interface{}{
			"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{
					"kubectl.kubernetes.io/last-applied-configuration": rv,
				},
			},
			"spec": map[string]interface{}{"replicas": int64(3)},
		}
	}
	got := Compute(mk("a"), mk("b"))
	if len(got.ChangedFields) != 0 {
		t.Errorf("last-applied-configuration churn should be ignored, got %v", got.ChangedFields)
	}
}

func TestCompute_NestedContainerImage(t *testing.T) {
	mk := func(image string) map[string]interface{} {
		return map[string]interface{}{
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{"name": "app", "image": image},
						},
					},
				},
			},
		}
	}
	got := Compute(mk("app:1.0"), mk("app:2.0"))
	want := []string{"spec.template.spec.containers[0].image"}
	if !reflect.DeepEqual(got.ChangedFields, want) {
		t.Errorf("ChangedFields = %v, want %v", got.ChangedFields, want)
	}
}

func TestCompute_CreateAndDelete(t *testing.T) {
	newObj := obj(map[string]interface{}{"spec": map[string]interface{}{"replicas": int64(1)}})

	if got := Compute(nil, newObj); got.Summary != "created" {
		t.Errorf("create summary = %q, want %q", got.Summary, "created")
	}
	if got := Compute(newObj, nil); got.Summary != "deleted" {
		t.Errorf("delete summary = %q, want %q", got.Summary, "deleted")
	}
}

func TestCompute_AddedFieldSummary(t *testing.T) {
	old := obj(map[string]interface{}{"spec": map[string]interface{}{}})
	newObj := obj(map[string]interface{}{"spec": map[string]interface{}{"paused": true}})

	got := Compute(old, newObj)
	if want := []string{"spec.paused"}; !reflect.DeepEqual(got.ChangedFields, want) {
		t.Errorf("ChangedFields = %v, want %v", got.ChangedFields, want)
	}
	if want := "spec.paused: <none> -> true"; got.Summary != want {
		t.Errorf("Summary = %q, want %q", got.Summary, want)
	}
}

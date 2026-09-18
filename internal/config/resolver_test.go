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

package config

import (
	"testing"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
)

func TestDefaults(t *testing.T) {
	r := NewResolver(nil)

	if !r.EffectiveFor("openshift-cnv").NamespaceIgnored("openshift-cnv") {
		t.Error("openshift-* should be ignored by default")
	}
	if r.EffectiveFor("team-a").NamespaceIgnored("team-a") {
		t.Error("team-a should be kept by default")
	}
	if r.EffectiveFor("").NamespaceIgnored("") {
		t.Error("cluster-scoped (empty namespace) is never ignored")
	}
	if !r.EffectiveFor("team-a").SecretTypeExcluded("kubernetes.io/dockercfg") {
		t.Error("dockercfg secret type should be excluded by default")
	}
	if r.EffectiveFor("team-a").SecretTypeExcluded("Opaque") {
		t.Error("Opaque secret type should not be excluded")
	}
	if !r.EffectiveFor("team-a").ExcludeControllerActors {
		t.Error("excludeControllerActors should default true")
	}
}

func TestNamespaceOverride(t *testing.T) {
	off := false
	cfg := &chronosv1alpha1.ChronosConfig{
		Spec: chronosv1alpha1.ChronosConfigSpec{
			NamespaceOverrides: []chronosv1alpha1.NamespacePolicy{{
				Namespace:    "team-a",
				FilterPolicy: chronosv1alpha1.FilterPolicy{ExcludeControllerActors: &off},
			}},
		},
	}
	r := NewResolver(cfg)

	if !r.EffectiveFor("team-b").ExcludeControllerActors {
		t.Error("team-b should keep the default (true)")
	}
	if r.EffectiveFor("team-a").ExcludeControllerActors {
		t.Error("team-a override should turn excludeControllerActors off")
	}
}

func TestMeaningfulFields_ExcludeMetadataExceptAnnotations(t *testing.T) {
	cfg := &chronosv1alpha1.ChronosConfig{
		Spec: chronosv1alpha1.ChronosConfigSpec{
			Defaults: chronosv1alpha1.FilterPolicy{
				ExcludeFields: []string{"metadata"},
				IncludeFields: []string{"metadata.annotations"},
			},
		},
	}
	fs := NewResolver(cfg).EffectiveFor("team-a")

	in := []string{"metadata.labels.tier", "metadata.annotations.note", "spec.replicas"}
	got := fs.MeaningfulFields(in)

	want := map[string]bool{"metadata.annotations.note": true, "spec.replicas": true}
	if len(got) != len(want) {
		t.Fatalf("MeaningfulFields = %v, want keys %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected surviving field %q", g)
		}
	}
}

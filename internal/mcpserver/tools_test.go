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

package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(chronosv1alpha1.AddToScheme(s))
	return s
}

// eventVerb is the verb every fixture in these tests uses. It was a parameter
// that all five callers passed the same value for, which reads like a knob and
// is not one.
const eventVerb = "update"

func event(name, ns, kind, target string, at time.Time, risk chronosv1alpha1.RiskLevel) *chronosv1alpha1.ChangeEvent {
	verb := eventVerb
	return &chronosv1alpha1.ChangeEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"chronos.ocp.run/target-namespace": strings.ToLower(ns),
				"chronos.ocp.run/target-kind":      strings.ToLower(kind),
				"chronos.ocp.run/verb":             verb,
			},
		},
		Spec: chronosv1alpha1.ChangeEventSpec{
			Verb:       chronosv1alpha1.ChangeVerb(verb),
			ObservedAt: metav1.NewTime(at),
			Source:     "correlated",
			RiskLevel:  risk,
			Summary:    "changed",
			Actor: chronosv1alpha1.Actor{
				Username:   "kube:admin",
				Confidence: "verified",
				Shared:     true,
			},
			Target: chronosv1alpha1.TargetObjectReference{
				APIVersion: "v1", Kind: kind, Name: target, Namespace: ns,
			},
		},
	}
}

// snapshotNamespace is the fixture namespace every snapshot in these tests uses.
// It was a parameter that every caller passed the same value for, which reads
// like a knob and is not one.
const snapshotNamespace = "app"

func snapshot(t *testing.T, name string, content map[string]interface{}, uid string, redactions []chronosv1alpha1.RedactionEntry) *chronosv1alpha1.ResourceSnapshot {
	t.Helper()
	ns := snapshotNamespace
	s := &chronosv1alpha1.ResourceSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: chronosv1alpha1.ResourceSnapshotSpec{
			CapturedAt: metav1.NewTime(time.Now()),
			Target: chronosv1alpha1.TargetObjectReference{
				APIVersion: "v1", Kind: "ConfigMap", Name: "cfg", Namespace: ns, UID: uid,
			},
			Redactions: redactions,
		},
	}
	if content != nil {
		raw, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		s.Spec.Content = &runtime.RawExtension{Raw: raw}
	}
	return s
}

func TestChangesInWindowFiltersByTime(t *testing.T) {
	now := time.Now().UTC()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		event("recent", "app", "ConfigMap", "cfg", now.Add(-10*time.Minute), "medium"),
		event("old", "app", "ConfigMap", "cfg", now.Add(-5*time.Hour), "medium"),
	).Build()
	s := &Server{Provider: &StaticClientProvider{Client: c}}

	out, err := s.ChangesInWindow(context.Background(), ChangesInWindowArgs{Since: "30m", Namespace: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ConfigMap/cfg") {
		t.Errorf("the recent change should appear:\n%s", out)
	}
	if strings.Contains(out, ": 2") {
		t.Errorf("the 5-hour-old change should be outside a 30m window:\n%s", out)
	}
}

// An empty window is a real finding, not a failed query. If this reads like an
// error an agent may retry or, worse, treat the platform as unexamined.
func TestEmptyWindowStatesTheNegativeExplicitly(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := &Server{Provider: &StaticClientProvider{Client: c}}

	out, err := s.ChangesInWindow(context.Background(), ChangesInWindowArgs{Since: "30m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No recorded changes") || !strings.Contains(out, "Nothing in scope was modified") {
		t.Errorf("an empty window must read as a finding:\n%s", out)
	}
}

func TestSinceAcceptsDurationAndTimestamp(t *testing.T) {
	until := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	got, err := resolveSince("30m", until)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(until.Add(-30 * time.Minute)) {
		t.Errorf("duration form: got %s", got)
	}

	got, err = resolveSince("2026-07-25T11:00:00Z", until)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(time.Date(2026, 7, 25, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("timestamp form: got %s", got)
	}

	if _, err := resolveSince("yesterday", until); err == nil {
		t.Error("an unparseable since should be rejected")
	}
	if _, err := resolveSince("", until); err == nil {
		t.Error("an empty since should be rejected")
	}
}

func TestSharedAccountIsFlagged(t *testing.T) {
	now := time.Now().UTC()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		event("e", "app", "ConfigMap", "cfg", now.Add(-1*time.Minute), "high"),
	).Build()
	s := &Server{Provider: &StaticClientProvider{Client: c}}

	out, err := s.ChangesInWindow(context.Background(), ChangesInWindowArgs{Since: "30m"})
	if err != nil {
		t.Fatal(err)
	}
	// kube:admin is shared, so the change cannot be pinned to a person. An agent
	// naming an individual here would be stating something untrue.
	if !strings.Contains(out, "shared account") {
		t.Errorf("a shared account must be flagged as not attributable:\n%s", out)
	}
}

func TestDiffSnapshotsReportsChangedFields(t *testing.T) {
	before := snapshot(t, "before", map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "cfg", "namespace": "app"},
		"data":     map[string]interface{}{"LOG_LEVEL": "info"},
	}, "uid-1", nil)
	after := snapshot(t, "after", map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{"name": "cfg", "namespace": "app"},
		"data":     map[string]interface{}{"LOG_LEVEL": "debug"},
	}, "uid-1", nil)

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(before, after).Build()
	s := &Server{Provider: &StaticClientProvider{Client: c}}

	out, err := s.DiffSnapshots(context.Background(), DiffSnapshotsArgs{
		Namespace: "app", Before: "before", After: "after",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "data.LOG_LEVEL") {
		t.Errorf("the changed field should be named:\n%s", out)
	}
}

// The security-relevant case: a redacted field must be reported as withheld,
// never reconstructed, and never silently omitted.
func TestDiffReportsRedactionsWithoutRevealingThem(t *testing.T) {
	redactions := []chronosv1alpha1.RedactionEntry{{
		FieldPath: "data.password",
		Reason:    "secret-data",
		Hash:      "sha256:abc123",
	}}
	before := snapshot(t, "before", map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]interface{}{"name": "cfg"},
	}, "uid-1", redactions)
	after := snapshot(t, "after", map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]interface{}{"name": "cfg"},
	}, "uid-1", redactions)

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(before, after).Build()
	s := &Server{Provider: &StaticClientProvider{Client: c}}

	out, err := s.DiffSnapshots(context.Background(), DiffSnapshotsArgs{
		Namespace: "app", Before: "before", After: "after",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "data.password") || !strings.Contains(out, "secret-data") {
		t.Errorf("redacted paths must be disclosed as withheld:\n%s", out)
	}
	if strings.Contains(out, "sha256:abc123") {
		t.Errorf("the redaction hash must not be emitted; it is not useful to an agent "+
			"and it is derived from the withheld value:\n%s", out)
	}
}

// Offloaded content would otherwise diff as empty and read as "nothing changed",
// which is a false negative in the one place a false negative matters most.
func TestOffloadedContentIsAnErrorNotAnEmptyDiff(t *testing.T) {
	before := snapshot(t, "before", nil, "uid-1", nil)
	before.Spec.ContentRef = &chronosv1alpha1.StorageReference{Backend: "s3", Key: "snap/before"}
	after := snapshot(t, "after", map[string]interface{}{"kind": "ConfigMap"}, "uid-1", nil)

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(before, after).Build()
	s := &Server{Provider: &StaticClientProvider{Client: c}}

	_, err := s.DiffSnapshots(context.Background(), DiffSnapshotsArgs{
		Namespace: "app", Before: "before", After: "after",
	})
	if err == nil {
		t.Fatal("offloaded content must be an error, not a silent empty diff")
	}
	if !strings.Contains(err.Error(), "offloaded") {
		t.Errorf("the error should explain why: %v", err)
	}
}

func TestDiffRefusesDifferentObjects(t *testing.T) {
	before := snapshot(t, "before", map[string]interface{}{"kind": "ConfigMap"}, "uid-1", nil)
	after := snapshot(t, "after", map[string]interface{}{"kind": "ConfigMap"}, "uid-2", nil)
	after.Spec.Target.Name = "other"

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(before, after).Build()
	s := &Server{Provider: &StaticClientProvider{Client: c}}

	_, err := s.DiffSnapshots(context.Background(), DiffSnapshotsArgs{
		Namespace: "app", Before: "before", After: "after",
	})
	if err == nil {
		t.Fatal("diffing two different objects would be meaningless and must be refused")
	}
}

func TestMissingSnapshotIsAClearError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := &Server{Provider: &StaticClientProvider{Client: c}}

	_, err := s.DiffSnapshots(context.Background(), DiffSnapshotsArgs{
		Namespace: "app", Before: "nope", After: "also-nope",
	})
	if err == nil || !strings.Contains(err.Error(), "no ResourceSnapshot") {
		t.Fatalf("expected a clear not-found error, got %v", err)
	}
}

// newFakeClient builds an empty client for tests that only need the tools to
// run, not to return data.
func newFakeClient(s *runtime.Scheme) client.Client {
	return fake.NewClientBuilder().WithScheme(s).Build()
}

// A change window must read chronologically, and say so.
//
// Regression test for a measured failure. The window was rendered newest first
// with nothing indicating the order, and a language model reading it top to
// bottom reported that a workload had been "scaled down to 0, then scaled back
// up to 3" — the exact reverse of what happened. It concluded the problem had
// already resolved itself and proposed no remediation, while the deployment sat
// at zero replicas with the alarm still firing.
//
// Inverting a timeline inverts causality, which is the one thing a change ledger
// exists to get right.
func TestWindowRendersOldestFirstAndLabelsTheOrder(t *testing.T) {
	base := time.Date(2026, 7, 27, 10, 30, 0, 0, time.UTC)

	// Arrives newest first, as the selection step produces it.
	newer := event("e2", "demo", "Deployment", "web", base.Add(8*time.Minute), "medium")
	newer.Spec.Summary = "spec.replicas: 3 -> 0"
	older := event("e1", "demo", "Deployment", "web", base.Add(6*time.Minute), "medium")
	older.Spec.Summary = "spec.replicas: 0 -> 3"
	events := []chronosv1alpha1.ChangeEvent{*newer, *older}

	out := renderWindow(events, base, base.Add(30*time.Minute), false)

	if !strings.Contains(out, "chronological order (oldest first)") {
		t.Error("the header must state the ordering; a bare list reads as a narrative")
	}

	up := strings.Index(out, "0 -> 3")
	down := strings.Index(out, "3 -> 0")
	if up < 0 || down < 0 {
		t.Fatalf("both changes should be rendered:\n%s", out)
	}
	if up > down {
		t.Errorf("the older change must appear first; got the newest at the top:\n%s", out)
	}
}

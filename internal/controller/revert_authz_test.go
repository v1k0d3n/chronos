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

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
)

const guardConfigName = "chronos-mutating-webhook-configuration"

// The guard decides whether spec.requestedBy is believed. Every case below
// except the first is a cluster in which a RevertOperation could exist without
// the webhook having stamped it, and so a cluster in which the requester could
// have written their own identity.
func TestWebhookConfigGuard(t *testing.T) {
	installed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	fail, ignore := admissionregv1.Fail, admissionregv1.Ignore

	sound := func() admissionregv1.MutatingWebhook {
		return admissionregv1.MutatingWebhook{
			Name:          "mrevertoperation.chronos.ocp.run",
			FailurePolicy: &fail,
			Rules: []admissionregv1.RuleWithOperations{{
				Operations: []admissionregv1.OperationType{admissionregv1.Create},
				Rule: admissionregv1.Rule{
					APIGroups: []string{"chronos.ocp.run"}, APIVersions: []string{"v1alpha1"}, Resources: []string{"revertoperations"},
				},
			}},
		}
	}
	with := func(mutate func(*admissionregv1.MutatingWebhook)) []admissionregv1.MutatingWebhook {
		wh := sound()
		mutate(&wh)
		return []admissionregv1.MutatingWebhook{wh}
	}

	tests := []struct {
		name      string
		webhooks  []admissionregv1.MutatingWebhook
		absent    bool
		createdAt time.Time
		wantErr   string
	}{
		{name: "installed and unavoidable", webhooks: []admissionregv1.MutatingWebhook{sound()}, createdAt: installed.Add(time.Hour)},
		{name: "not installed", absent: true, createdAt: installed.Add(time.Hour), wantErr: "is not installed"},
		{
			name: "covers some other resource", createdAt: installed.Add(time.Hour), wantErr: "no webhook covering RevertOperation creation",
			webhooks: with(func(w *admissionregv1.MutatingWebhook) { w.Rules[0].Resources = []string{"changeevents"} }),
		},
		{
			name: "covers update but not create", createdAt: installed.Add(time.Hour), wantErr: "no webhook covering RevertOperation creation",
			webhooks: with(func(w *admissionregv1.MutatingWebhook) {
				w.Rules[0].Operations = []admissionregv1.OperationType{admissionregv1.Update}
			}),
		},
		{
			name: "fails open", createdAt: installed.Add(time.Hour), wantErr: "failurePolicy is not Fail",
			webhooks: with(func(w *admissionregv1.MutatingWebhook) { w.FailurePolicy = &ignore }),
		},
		{
			name: "skips unlabelled namespaces", createdAt: installed.Add(time.Hour), wantErr: "namespaceSelector",
			webhooks: with(func(w *admissionregv1.MutatingWebhook) {
				w.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"webhooks": "enabled"}}
			}),
		},
		{
			name: "skips objects by label", createdAt: installed.Add(time.Hour), wantErr: "objectSelector",
			webhooks: with(func(w *admissionregv1.MutatingWebhook) {
				w.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"stamp": "me"}}
			}),
		},
		{
			name: "skips requests by condition", createdAt: installed.Add(time.Hour), wantErr: "matchConditions",
			webhooks: with(func(w *admissionregv1.MutatingWebhook) {
				w.MatchConditions = []admissionregv1.MatchCondition{{Name: "not-me", Expression: "false"}}
			}),
		},
		{
			name: "request is older than the webhook", createdAt: installed.Add(-time.Minute), wantErr: "predates",
			webhooks: []admissionregv1.MutatingWebhook{sound()},
		},
		{
			name: "empty selectors are not a bypass", createdAt: installed.Add(time.Hour),
			webhooks: with(func(w *admissionregv1.MutatingWebhook) {
				w.NamespaceSelector, w.ObjectSelector = &metav1.LabelSelector{}, &metav1.LabelSelector{}
			}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if !tc.absent {
				builder = builder.WithObjects(&admissionregv1.MutatingWebhookConfiguration{
					ObjectMeta: metav1.ObjectMeta{Name: guardConfigName, CreationTimestamp: metav1.NewTime(installed)},
					Webhooks:   tc.webhooks,
				})
			}
			guard := &WebhookConfigGuard{Reader: builder.Build(), Name: guardConfigName}
			ro := &chronosv1alpha1.RevertOperation{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(tc.createdAt)},
			}

			err := guard.Verify(context.Background(), ro)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected the requester to be trusted, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected a refusal mentioning %q, but the requester was trusted", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("refusal %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

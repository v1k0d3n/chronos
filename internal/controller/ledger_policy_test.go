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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
)

// The ledger is protected three ways, and these tests hold each layer to its
// promise against a real API server:
//
//   - the ValidatingAdmissionPolicy in config/ledger-policy refuses writes from
//     anyone but the operator, even a user whose RBAC allows them;
//   - the CRD validation rules refuse to alter a record once written, even when
//     the operator itself tries;
//   - the one thing that may change — who did it — may change once.
var _ = Describe("The ledger", Ordered, func() {
	ctx := context.Background()

	const (
		policyNS = "ledger-test"
		// The identities the policy names. Impersonated below.
		writer     = "system:serviceaccount:ledger-test:chronos-watcher"    // may create
		correlator = "system:serviceaccount:ledger-test:chronos-correlator" // may update
		reverter   = "system:serviceaccount:ledger-test:chronos-reverter"   // may update
		janitor    = "system:serviceaccount:kube-system:namespace-controller"
		// A user who has been *granted* write access by RBAC. The policy is what
		// stands between them and the ledger.
		grantedUser = "mallory"
	)

	// as returns a client acting as the given user. Impersonation is evaluated
	// by the API server exactly as a real request from that user would be —
	// authorization and admission included — which is the point.
	as := func(user string) client.Client {
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: user}
		cl, err := client.New(c, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		return cl
	}

	event := func(name string) *chronosv1alpha1.ChangeEvent {
		return &chronosv1alpha1.ChangeEvent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: policyNS, Labels: map[string]string{
				"chronos.ocp.run/confidence": "partial",
			}},
			Spec: chronosv1alpha1.ChangeEventSpec{
				ObservedAt: metav1.Now(),
				Verb:       chronosv1alpha1.VerbUpdate,
				Target:     chronosv1alpha1.TargetObjectReference{APIVersion: "v1", Kind: "ConfigMap", Namespace: policyNS, Name: "app"},
				Actor:      chronosv1alpha1.Actor{Username: "oc", Confidence: chronosv1alpha1.AttributionPartial},
				Source:     chronosv1alpha1.SourceWatch,
				Summary:    "setting changed",
			},
		}
	}
	snapshot := func(name string) *chronosv1alpha1.ResourceSnapshot {
		return &chronosv1alpha1.ResourceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: policyNS},
			Spec: chronosv1alpha1.ResourceSnapshotSpec{
				Target:     chronosv1alpha1.TargetObjectReference{APIVersion: "v1", Kind: "ConfigMap", Namespace: policyNS, Name: "app"},
				CapturedAt: metav1.Now(),
				Content:    &runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"app"}}`)},
				Revertable: true,
			},
		}
	}

	BeforeAll(func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: policyNS, Labels: map[string]string{"ledger-policy-test": "true"}},
		}))).To(Succeed())

		// Everyone in these tests is *allowed* by RBAC to write records. What
		// they can actually do is then up to the policy alone.
		Expect(k8sClient.Create(ctx, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "ledger-test-writer"},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"chronos.ocp.run"}, Resources: []string{"changeevents", "changeevents/status", "resourcesnapshots", "resourcesnapshots/status"}, Verbs: []string{"*"},
			}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "ledger-test-writer"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "ledger-test-writer"},
			Subjects: []rbacv1.Subject{
				{APIGroup: rbacv1.GroupName, Kind: "User", Name: grantedUser},
				{APIGroup: rbacv1.GroupName, Kind: "User", Name: writer},
				{APIGroup: rbacv1.GroupName, Kind: "User", Name: correlator},
				{APIGroup: rbacv1.GroupName, Kind: "User", Name: reverter},
				{APIGroup: rbacv1.GroupName, Kind: "User", Name: janitor},
			},
		})).To(Succeed())

		// Install the shipped policy — the file itself, not a copy — with a
		// parameters ConfigMap naming this test's writer.
		policy := loadShippedPolicy()
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "ledger-writers", Namespace: policyNS},
			Data: map[string]string{
				"namespace": policyNS, "watcher": "chronos-watcher", "correlator": "chronos-correlator", "reverter": "chronos-reverter",
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &admissionregv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "ledger-writers"},
			Spec: admissionregv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName:        policy.Name,
				ValidationActions: []admissionregv1.ValidationAction{admissionregv1.Deny},
				ParamRef: &admissionregv1.ParamRef{
					Name: "ledger-writers", Namespace: policyNS,
					ParameterNotFoundAction: ptr(admissionregv1.DenyAction),
				},
				// The shipped binding is cluster-wide. Here it is confined to this
				// namespace so the other suites in this process, which write
				// records as the test's admin identity, are not caught by it.
				MatchResources: &admissionregv1.MatchResources{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"ledger-policy-test": "true"}},
				},
			},
		})).To(Succeed())

		// The policy takes a moment to become effective. Wait until it refuses
		// something, so no test below races it.
		Eventually(func() bool {
			err := as(grantedUser).Create(ctx, event("probe"))
			return apierrors.IsForbidden(err)
		}, 30*time.Second, 250*time.Millisecond).Should(BeTrue(), "the ledger policy never became active")
	})

	Describe("refuses writes from anyone but Chronos, whatever RBAC says", func() {
		It("refuses a create", func() {
			err := as(grantedUser).Create(ctx, event("forged"))
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)
			Expect(err.Error()).To(ContainSubstring("Chronos records are written only by Chronos"))
			Expect(err.Error()).To(ContainSubstring("mallory may not create them"))
		})

		It("refuses a forged snapshot — the ingredient of the revert attack", func() {
			err := as(grantedUser).Create(ctx, snapshot("forged"))
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)
		})

		It("refuses an edit and a delete of a real record", func() {
			Expect(as(writer).Create(ctx, event("real"))).To(Succeed())

			got := &chronosv1alpha1.ChangeEvent{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "real"}, got)).To(Succeed())
			patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"chronos.ocp.run/confidence":"verified"}}}`))
			err := as(grantedUser).Patch(ctx, got, patch)
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)

			err = as(grantedUser).Delete(ctx, got)
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)
		})

		It("lets the watcher create, and only create", func() {
			Expect(as(writer).Create(ctx, snapshot("genuine"))).To(Succeed())
			Expect(as(writer).Create(ctx, event("genuine"))).To(Succeed())

			got := &chronosv1alpha1.ChangeEvent{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "genuine"}, got)).To(Succeed())
			enrich := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"actor":{"username":"alice","confidence":"verified"},"source":"correlated"}}`))
			err := as(writer).Patch(ctx, got.DeepCopy(), enrich)
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "the watcher has no business rewriting a record: %v", err)
		})

		It("lets the correlator and the reverter update, and only update", func() {
			Expect(as(writer).Create(ctx, event("enrichable"))).To(Succeed())
			got := &chronosv1alpha1.ChangeEvent{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "enrichable"}, got)).To(Succeed())

			for _, who := range []string{correlator, reverter} {
				err := as(who).Create(ctx, event("planted-by-"+who[len(who)-8:]))
				Expect(apierrors.IsForbidden(err)).To(BeTrue(), "%s must not create records: %v", who, err)
			}
			enrich := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"actor":{"username":"alice","confidence":"verified"},"source":"correlated"}}`))
			Expect(as(correlator).Patch(ctx, got.DeepCopy(), enrich)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "enrichable"}, got)).To(Succeed())
			mark := client.RawPatch(types.MergePatchType, []byte(`{"status":{"reverted":true,"revertOperation":"r1"}}`))
			Expect(as(reverter).Status().Patch(ctx, got.DeepCopy(), mark)).To(Succeed())

			// Nobody Chronos-side may delete: retention is not implemented, and
			// until it is, deletion is not a thing any process needs.
			for _, who := range []string{writer, correlator, reverter} {
				err := as(who).Delete(ctx, got.DeepCopy())
				Expect(apierrors.IsForbidden(err)).To(BeTrue(), "%s must not delete records: %v", who, err)
			}
		})

		It("still lets the cluster clean up a deleted namespace", func() {
			Expect(as(writer).Create(ctx, event("to-be-collected"))).To(Succeed())
			got := &chronosv1alpha1.ChangeEvent{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "to-be-collected"}, got)).To(Succeed())

			Expect(as(janitor).Delete(ctx, got)).To(Succeed())
			// ...but cleaning up is all it may do.
			err := as(janitor).Create(ctx, event("planted"))
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)
		})
	})

	Describe("does not let even Chronos rewrite history", func() {
		It("freezes what a ChangeEvent records", func() {
			Expect(as(writer).Create(ctx, event("frozen"))).To(Succeed())
			got := &chronosv1alpha1.ChangeEvent{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "frozen"}, got)).To(Succeed())

			for name, patch := range map[string]string{
				"target":           `{"spec":{"target":{"name":"something-else"}}}`,
				"verb":             `{"spec":{"verb":"delete"}}`,
				"summary":          `{"spec":{"summary":"nothing happened"}}`,
				"beforeSnapshot":   `{"spec":{"beforeSnapshot":"some-other-state"}}`,
				"removing a field": `{"spec":{"summary":null}}`,
			} {
				err := as(correlator).Patch(ctx, got.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(patch)))
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: got %v", name, err)
				Expect(err.Error()).To(ContainSubstring("immutable"), name)
			}
		})

		It("lets the actor be established once, and never revised", func() {
			Expect(as(writer).Create(ctx, event("attributed"))).To(Succeed())
			got := &chronosv1alpha1.ChangeEvent{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "attributed"}, got)).To(Succeed())

			// What the audit correlator does.
			verify := `{"spec":{"actor":{"username":"alice","confidence":"verified"},"source":"correlated"}}`
			Expect(as(correlator).Patch(ctx, got.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(verify)))).To(Succeed())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "attributed"}, got)).To(Succeed())
			revise := `{"spec":{"actor":{"username":"bob","confidence":"verified"}}}`
			err := as(correlator).Patch(ctx, got.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(revise)))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got: %v", err)
			Expect(err.Error()).To(ContainSubstring("write-once"))
		})

		It("lets Chronos, and only Chronos, note that a record was reverted", func() {
			Expect(as(writer).Create(ctx, event("undone"))).To(Succeed())
			got := &chronosv1alpha1.ChangeEvent{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "undone"}, got)).To(Succeed())
			mark := client.RawPatch(types.MergePatchType, []byte(`{"status":{"reverted":true,"revertOperation":"revert-app-x1"}}`))

			// The status subresource is a separate write path; the policy must
			// cover it too, or a user could mark anything reverted.
			err := as(grantedUser).Status().Patch(ctx, got.DeepCopy(), mark)
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)

			Expect(as(reverter).Status().Patch(ctx, got.DeepCopy(), mark)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "undone"}, got)).To(Succeed())
			Expect(got.Status.Reverted).To(BeTrue(), "the mark must actually land, not be pruned")
			Expect(got.Status.RevertOperation).To(Equal("revert-app-x1"))
		})

		It("freezes a snapshot entirely", func() {
			Expect(as(writer).Create(ctx, snapshot("frozen"))).To(Succeed())
			got := &chronosv1alpha1.ResourceSnapshot{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: policyNS, Name: "frozen"}, got)).To(Succeed())

			for name, patch := range map[string]string{
				"content":    `{"spec":{"content":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"app"},"data":{"planted":"yes"}}}}`,
				"revertable": `{"spec":{"revertable":false}}`,
				"target":     `{"spec":{"target":{"name":"other"}}}`,
			} {
				err := as(correlator).Patch(ctx, got.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(patch)))
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: got %v", name, err)
				Expect(err.Error()).To(ContainSubstring("immutable"), name)
			}
		})
	})
})

// loadShippedPolicy decodes the ValidatingAdmissionPolicy from
// config/ledger-policy/policy.yaml, so the expression under test is the one
// that ships.
func loadShippedPolicy() *admissionregv1.ValidatingAdmissionPolicy {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "ledger-policy", "policy.yaml"))
	Expect(err).NotTo(HaveOccurred())
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var u map[string]any
		if err := dec.Decode(&u); err != nil {
			Fail(fmt.Sprintf("no ValidatingAdmissionPolicy in policy.yaml: %v", err))
		}
		if u["kind"] != "ValidatingAdmissionPolicy" {
			continue
		}
		policy := &admissionregv1.ValidatingAdmissionPolicy{}
		Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(u, policy)).To(Succeed())
		return policy
	}
}

func ptr[T any](v T) *T { return &v }

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
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
	"github.com/v1k0d3n/chronos/internal/redact"
	"github.com/v1k0d3n/chronos/internal/watcher"
)

const testSystemNamespace = "chronos-system"

// guardFunc adapts a function to AdmissionGuard.
type guardFunc func(context.Context, *chronosv1alpha1.RevertOperation) error

func (f guardFunc) Verify(ctx context.Context, ro *chronosv1alpha1.RevertOperation) error {
	return f(ctx, ro)
}

// stamped stands in for a cluster where the admission webhook is installed and
// cannot be bypassed, so spec.requestedBy is believed. The guard itself is
// tested separately; these tests are about what happens once identity is known.
var stamped = guardFunc(func(context.Context, *chronosv1alpha1.RevertOperation) error { return nil })

var nsCounter atomic.Int64

var _ = Describe("RevertOperation controller", func() {
	ctx := context.Background()
	var ns string

	alice := &chronosv1alpha1.Requester{Username: "alice", Groups: []string{"system:authenticated"}}

	reconciler := func(guard AdmissionGuard) *RevertOperationReconciler {
		return &RevertOperationReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			APIReader:       k8sClient,
			Admission:       guard,
			SystemNamespace: testSystemNamespace,
			// No watcher runs here, so nothing records applies; do not wait
			// long for a ChangeEvent that will not come.
			AttributionWait: 300 * time.Millisecond,
		}
	}

	ensureNamespace := func(name string) {
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}

	// grant gives a user verbs on a resource within the test namespace.
	grant := func(user, apiGroup, resource string, verbs ...string) {
		name := fmt.Sprintf("%s-%s", user, resource)
		Expect(k8sClient.Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{apiGroup}, Resources: []string{resource}, Verbs: verbs}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: user}},
		})).To(Succeed())
	}

	// snapshot stores content under a snapshot that claims to record `claims`.
	// The two are separate arguments because a forged snapshot is precisely one
	// where they disagree.
	snapshot := func(inNamespace, name string, claims chronosv1alpha1.TargetObjectReference, content runtime.Object) {
		raw, err := json.Marshal(content)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Create(ctx, &chronosv1alpha1.ResourceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: inNamespace},
			Spec: chronosv1alpha1.ResourceSnapshotSpec{
				Target:     claims,
				CapturedAt: metav1.Now(),
				Content:    &runtime.RawExtension{Raw: raw},
				Revertable: true,
			},
		})).To(Succeed())
	}

	// revert creates a RevertOperation, reconciles it once, and returns it.
	revert := func(guard AdmissionGuard, inNamespace string, spec chronosv1alpha1.RevertOperationSpec) *chronosv1alpha1.RevertOperation {
		ro := &chronosv1alpha1.RevertOperation{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "revert-", Namespace: inNamespace},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, ro)).To(Succeed())
		key := types.NamespacedName{Namespace: ro.Namespace, Name: ro.Name}
		_, err := reconciler(guard).Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, ro)).To(Succeed())
		return ro
	}

	configMapRef := func(name string) chronosv1alpha1.TargetObjectReference {
		return chronosv1alpha1.TargetObjectReference{APIVersion: "v1", Kind: "ConfigMap", Namespace: ns, Name: name}
	}
	configMap := func(name, value string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string]string{"setting": value},
		}
	}
	currentValue := func(name string) string {
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, cm)).To(Succeed())
		return cm.Data["setting"]
	}

	BeforeEach(func() {
		ns = fmt.Sprintf("team-%d", nsCounter.Add(1))
		ensureNamespace(ns)
		ensureNamespace(testSystemNamespace)
	})

	Describe("a requester who could make the change themselves", func() {
		It("reverts the object", func() {
			grant("alice", "", "configmaps", "patch")
			live := configMap("app-config", "broken")
			Expect(k8sClient.Create(ctx, live)).To(Succeed())
			snapshot(ns, "app-config-good", configMapRef("app-config"), configMap("app-config", "good"))

			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{
				Target: configMapRef("app-config"), ToSnapshot: "app-config-good", RequestedBy: alice,
			})

			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertSucceeded), ro.Status.Message)
			Expect(currentValue("app-config")).To(Equal("good"))
		})

		It("authorizes a dry run the same way, and changes nothing", func() {
			grant("alice", "", "configmaps", "patch")
			Expect(k8sClient.Create(ctx, configMap("app-config", "broken"))).To(Succeed())
			snapshot(ns, "app-config-good", configMapRef("app-config"), configMap("app-config", "good"))

			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{
				Target: configMapRef("app-config"), ToSnapshot: "app-config-good", RequestedBy: alice, DryRun: true,
			})

			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertSucceeded), ro.Status.Message)
			Expect(currentValue("app-config")).To(Equal("broken"))
		})
	})

	Describe("the timeline record of a revert", func() {
		It("names the person who asked, not the operator, and links the undone change", func() {
			grant("alice", "", "configmaps", "patch")
			Expect(k8sClient.Create(ctx, configMap("app-config", "broken"))).To(Succeed())
			snapshot(ns, "app-config-good", configMapRef("app-config"), configMap("app-config", "good"))
			// The change being undone, as the watcher would have recorded it.
			undone := &chronosv1alpha1.ChangeEvent{
				ObjectMeta: metav1.ObjectMeta{Name: "app-config-broken-by-bob", Namespace: ns,
					Labels: map[string]string{"chronos.ocp.run/confidence": "verified"}},
				Spec: chronosv1alpha1.ChangeEventSpec{
					ObservedAt: metav1.Now(), Verb: chronosv1alpha1.VerbUpdate, Target: configMapRef("app-config"),
					Actor:  chronosv1alpha1.Actor{Username: "bob", Confidence: chronosv1alpha1.AttributionVerified},
					Source: chronosv1alpha1.SourceCorrelated, BeforeSnapshot: "app-config-good",
				},
			}
			Expect(k8sClient.Create(ctx, undone)).To(Succeed())

			// Stand in for the watcher: once the apply lands, record it under
			// the name the watcher would use, credited to the field manager.
			recorded := make(chan string, 1)
			go func() {
				defer GinkgoRecover()
				Eventually(func() string { return currentValue("app-config") }, 5*time.Second, 50*time.Millisecond).Should(Equal("good"))
				cm := &corev1.ConfigMap{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "app-config"}, cm)).To(Succeed())
				name := watcher.EventName("ConfigMap", "app-config", "update", cm.ResourceVersion)
				Expect(k8sClient.Create(ctx, &chronosv1alpha1.ChangeEvent{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"chronos.ocp.run/confidence": "partial"}},
					Spec: chronosv1alpha1.ChangeEventSpec{
						ObservedAt: metav1.Now(), Verb: chronosv1alpha1.VerbUpdate, Target: configMapRef("app-config"),
						Actor:  chronosv1alpha1.Actor{Username: "chronos-revert", Confidence: chronosv1alpha1.AttributionPartial},
						Source: chronosv1alpha1.SourceWatch,
					},
				})).To(Succeed())
				recorded <- name
			}()

			r := reconciler(stamped)
			r.AttributionWait = 5 * time.Second
			ro := &chronosv1alpha1.RevertOperation{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "revert-", Namespace: ns},
				Spec: chronosv1alpha1.RevertOperationSpec{
					Target: configMapRef("app-config"), ChangeEventRef: undone.Name, RequestedBy: alice,
				},
			}
			Expect(k8sClient.Create(ctx, ro)).To(Succeed())
			key := types.NamespacedName{Namespace: ro.Namespace, Name: ro.Name}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, ro)).To(Succeed())
			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertSucceeded), ro.Status.Message)

			var name string
			Eventually(recorded, 5*time.Second).Should(Receive(&name))
			Expect(ro.Status.ResultingChangeEvent).To(Equal(name))

			got := &chronosv1alpha1.ChangeEvent{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
			Expect(got.Spec.Actor.Username).To(Equal("alice"), "the timeline must name the requester")
			Expect(got.Spec.Actor.Confidence).To(Equal(chronosv1alpha1.AttributionVerified))
			Expect(got.Spec.Actor.UserAgent).To(Equal("RevertOperation/" + ro.Name))
			Expect(got.Spec.Source).To(Equal(chronosv1alpha1.SourceRevert))
			Expect(got.Labels["chronos.ocp.run/confidence"]).To(Equal("verified"))

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(undone), undone)).To(Succeed())
			Expect(undone.Status.Reverted).To(BeTrue(), "the undone change must say so")
			Expect(undone.Status.RevertOperation).To(Equal(ro.Name))
		})
	})

	Describe("a snapshot with redacted fields", func() {
		It("reverts everything else and leaves the live secret value alone", func() {
			grant("alice", "apps", "deployments", "patch")
			dep := func(replicas int32, password string) *appsv1.Deployment {
				return &appsv1.Deployment{
					TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
					ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: ns},
					Spec: appsv1.DeploymentSpec{
						Replicas: &replicas,
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
							Spec: corev1.PodSpec{Containers: []corev1.Container{{
								Name: "api", Image: "example/api:1",
								Env: []corev1.EnvVar{
									{Name: "LOG_LEVEL", Value: "info"},
									{Name: "DB_PASSWORD", Value: password},
								},
							}}},
						},
					},
				}
			}
			// Live: someone scaled it to 3 and rotated the password.
			Expect(k8sClient.Create(ctx, dep(3, "rotated-live-value"))).To(Succeed())

			// The snapshot was taken by the watcher, so it went through
			// redaction: the password is gone from it, and its path is listed.
			raw, err := json.Marshal(dep(1, "old-value-never-stored"))
			Expect(err).NotTo(HaveOccurred())
			redacted, entries, wasRedacted := redact.Redact(&unstructured.Unstructured{Object: mustUnmarshal(raw)})
			Expect(wasRedacted).To(BeTrue())
			content, err := json.Marshal(redacted.Object)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).NotTo(ContainSubstring("old-value-never-stored"))
			Expect(k8sClient.Create(ctx, &chronosv1alpha1.ResourceSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "api-before", Namespace: ns},
				Spec: chronosv1alpha1.ResourceSnapshotSpec{
					Target:     chronosv1alpha1.TargetObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: "api"},
					CapturedAt: metav1.Now(),
					Content:    &runtime.RawExtension{Raw: content},
					Redactions: entries,
					Redacted:   true,
				},
			})).To(Succeed())

			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{
				Target:      chronosv1alpha1.TargetObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: "api"},
				ToSnapshot:  "api-before",
				RequestedBy: alice,
			})

			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertSucceeded), ro.Status.Message)
			Expect(ro.Status.ManualStepsRequired).To(HaveLen(1))
			Expect(ro.Status.ManualStepsRequired[0]).To(ContainSubstring("/spec/template/spec/containers/0/env/1/value"))

			got := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "api"}, got)).To(Succeed())
			Expect(*got.Spec.Replicas).To(Equal(int32(1)), "the non-secret field must be reverted")
			env := got.Spec.Template.Spec.Containers[0].Env
			Expect(env).To(HaveLen(2))
			Expect(env[1].Name).To(Equal("DB_PASSWORD"))
			Expect(env[1].Value).To(Equal("rotated-live-value"), "the live secret must be neither blanked nor restored")
		})
	})

	Describe("a requester who could not", func() {
		It("is refused, and the object is left alone", func() {
			Expect(k8sClient.Create(ctx, configMap("app-config", "broken"))).To(Succeed())
			snapshot(ns, "app-config-good", configMapRef("app-config"), configMap("app-config", "good"))

			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{
				Target: configMapRef("app-config"), ToSnapshot: "app-config-good", RequestedBy: alice,
			})

			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring("alice is not allowed to patch configmaps"))
			Expect(currentValue("app-config")).To(Equal("broken"))
		})

		It("needs create as well as patch to bring back a deleted object", func() {
			grant("alice", "", "configmaps", "patch")
			snapshot(ns, "gone-good", configMapRef("gone"), configMap("gone", "good"))

			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{
				Target: configMapRef("gone"), ToSnapshot: "gone-good", RequestedBy: alice,
			})

			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring("alice is not allowed to create configmaps"))
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gone"}, &corev1.ConfigMap{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("cannot use patch on roles to write permissions it does not hold", func() {
			// The API server would refuse alice this directly: she lacks
			// `escalate`. The operator would pass that check on her behalf,
			// because it can read Secrets everywhere. So Chronos asks for her.
			grant("alice", rbacv1.GroupName, "roles", "patch")
			ref := chronosv1alpha1.TargetObjectReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role", Namespace: ns, Name: "helper"}
			role := func(rules ...rbacv1.PolicyRule) *rbacv1.Role {
				return &rbacv1.Role{
					TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
					ObjectMeta: metav1.ObjectMeta{Name: "helper", Namespace: ns},
					Rules:      rules,
				}
			}
			Expect(k8sClient.Create(ctx, role())).To(Succeed())
			snapshot(ns, "helper-forged", ref,
				role(rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list"}}))

			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{Target: ref, ToSnapshot: "helper-forged", RequestedBy: alice})

			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring("alice is not allowed to escalate roles"))
			got := &rbacv1.Role{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "helper"}, got)).To(Succeed())
			Expect(got.Rules).To(BeEmpty())
		})
	})

	// The attack this controller used to permit: someone with ordinary rights in
	// one namespace writes a snapshot whose content is a ClusterRoleBinding to
	// cluster-admin, then asks the operator to "revert" to it. Each test closes
	// one route to that outcome, and each ends by checking the binding was never
	// created — the failure message matters less than the cluster being intact.
	Describe("a forged snapshot granting cluster-admin", func() {
		const bindingName = "alice-is-admin"

		forged := func() *rbacv1.ClusterRoleBinding {
			return &rbacv1.ClusterRoleBinding{
				TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
				ObjectMeta: metav1.ObjectMeta{Name: bindingName},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "alice"}},
			}
		}
		bindingRef := chronosv1alpha1.TargetObjectReference{
			APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding", Name: bindingName,
		}

		BeforeEach(func() {
			// Everything the attacker legitimately holds.
			grant("alice", "", "configmaps", "patch", "create")
		})
		AfterEach(func() {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: bindingName}, &rbacv1.ClusterRoleBinding{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the forged ClusterRoleBinding must never be created")
		})

		It("is refused when disguised as a revert of an object alice may patch", func() {
			Expect(k8sClient.Create(ctx, configMap("app-config", "fine"))).To(Succeed())
			snapshot(ns, "disguised", configMapRef("app-config"), forged())

			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{
				Target: configMapRef("app-config"), ToSnapshot: "disguised", RequestedBy: alice,
			})

			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring("which is not the requested target"))
		})

		It("is refused when requested openly from alice's own namespace", func() {
			snapshot(ns, "open", bindingRef, forged())

			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{Target: bindingRef, ToSnapshot: "open", RequestedBy: alice})

			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring("cluster-scoped objects can only be reverted from namespace"))
		})

		It("is refused even from the system namespace, because alice cannot create it herself", func() {
			snapshot(testSystemNamespace, "from-system", bindingRef, forged())

			ro := revert(stamped, testSystemNamespace, chronosv1alpha1.RevertOperationSpec{
				Target: bindingRef, ToSnapshot: "from-system", RequestedBy: alice,
			})

			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring("alice is not allowed to patch clusterrolebindings"))
		})

		It("is refused when alice names someone more powerful as the requester", func() {
			// Without the webhook installed, requestedBy is whatever the client
			// wrote. The guard is what notices; here it reports exactly that.
			unstamped := guardFunc(func(context.Context, *chronosv1alpha1.RevertOperation) error {
				return errors.New("the Chronos admission webhook is not installed")
			})
			snapshot(testSystemNamespace, "impersonated", bindingRef, forged())

			ro := revert(unstamped, testSystemNamespace, chronosv1alpha1.RevertOperationSpec{
				Target: bindingRef, ToSnapshot: "impersonated",
				RequestedBy: &chronosv1alpha1.Requester{Username: "system:admin", Groups: []string{"system:masters"}},
			})

			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring("not installed"))
		})
	})

	Describe("requests that do not hold together", func() {
		It("refuses when nobody is recorded as the requester", func() {
			snapshot(ns, "s", configMapRef("app-config"), configMap("app-config", "good"))
			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{Target: configMapRef("app-config"), ToSnapshot: "s"})
			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring("does not record who requested it"))
		})

		It("refuses everything when it has no admission guard", func() {
			snapshot(ns, "s", configMapRef("app-config"), configMap("app-config", "good"))
			ro := revert(nil, ns, chronosv1alpha1.RevertOperationSpec{
				Target: configMapRef("app-config"), ToSnapshot: "s", RequestedBy: alice,
			})
			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
		})

		It("refuses a snapshot that records a different object", func() {
			grant("alice", "", "configmaps", "patch")
			snapshot(ns, "other", configMapRef("someone-elses"), configMap("someone-elses", "x"))
			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{
				Target: configMapRef("app-config"), ToSnapshot: "other", RequestedBy: alice,
			})
			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring("not the requested target"))
		})

		It("refuses to reach into another namespace", func() {
			other := chronosv1alpha1.TargetObjectReference{APIVersion: "v1", Kind: "ConfigMap", Namespace: "kube-system", Name: "x"}
			cm := configMap("x", "y")
			cm.Namespace = "kube-system"
			snapshot(ns, "reach", other, cm)
			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{Target: other, ToSnapshot: "reach", RequestedBy: alice})
			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring(`cannot revert an object in namespace "kube-system"`))
		})

		It("refuses kinds Chronos does not record", func() {
			ref := chronosv1alpha1.TargetObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: ns, Name: "p"}
			pod := &corev1.Pod{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns},
			}
			snapshot(ns, "pod", ref, pod)
			ro := revert(stamped, ns, chronosv1alpha1.RevertOperationSpec{Target: ref, ToSnapshot: "pod", RequestedBy: alice})
			Expect(ro.Status.Phase).To(Equal(chronosv1alpha1.RevertFailed))
			Expect(ro.Status.Message).To(ContainSubstring("not a kind Chronos reverts"))
		})
	})

	It("does not allow a request to be rewritten after it is admitted", func() {
		ro := &chronosv1alpha1.RevertOperation{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "revert-", Namespace: ns},
			Spec:       chronosv1alpha1.RevertOperationSpec{Target: configMapRef("app-config"), ToSnapshot: "a", RequestedBy: alice},
		}
		Expect(k8sClient.Create(ctx, ro)).To(Succeed())

		ro.Spec.RequestedBy = &chronosv1alpha1.Requester{Username: "system:admin"}
		err := k8sClient.Update(ctx, ro)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec is immutable"))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ro), ro)).To(Succeed())
		ro.Spec.ToSnapshot = "b"
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, ro))).To(BeTrue())
	})
})

func mustUnmarshal(raw []byte) map[string]interface{} {
	var m map[string]interface{}
	Expect(json.Unmarshal(raw, &m)).To(Succeed())
	return m
}
